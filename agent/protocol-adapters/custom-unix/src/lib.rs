//! Custom Unix socket adapter — reads DataEnvelope frames from a Unix domain socket.
//!
//! This adapter is the integration point for any hardware code that isn't MAVLink or ROS2.
//! It accepts two wire formats on the same socket path:
//!
//! ## Wire format 1: Raw proto (for high-frequency, low-overhead integrations)
//! Used by code that generates DataEnvelope proto bytes directly (C, C++, Rust).
//! Frame: [4 bytes LE length][DataEnvelope proto bytes]
//!
//! ## Wire format 2: JSON (for the Python/Go SDK)
//! Used by the SDK's `log()` call. The local HTTP API (localhost:7777) translates
//! SDK calls → JSON frames on the Unix socket internally. External callers can also
//! write JSON directly if preferred.
//! Frame: [4 bytes LE length][UTF-8 JSON bytes]
//!
//! Both formats are distinguished by the first byte of the payload:
//! - JSON always starts with `{` (0x7B)
//! - Proto envelopes start with field tag 0x0A (field 1, wire type 2 = string, vehicle_id)
//!   or 0x10 (field 2, wire type 0 = varint, timestamp). Never 0x7B.
//!
//! Socket path: `/var/run/systemscale/ingest.sock` (configurable)
//!
//! Commands come back on the same socket connection as JSON frames:
//! Frame: [4 bytes LE length][{"type":"command","id":"...","payload":{...},"priority":"normal"}]
//!
//! Multiple concurrent connections are supported (one per SDK client process).

use std::path::PathBuf;
use std::pin::Pin;
use std::sync::Arc;

use adapter_trait::{
    Ack, AckStatus, CommandEnvelope, DataEnvelope, Priority, StreamType, VehicleDataSource, now_ns,
};
use anyhow::{Context, Result};
use bytes::{Bytes, BytesMut};
use futures::Stream;
use serde::{Deserialize, Serialize};
use tokio::io::{AsyncReadExt, AsyncWriteExt};
#[cfg(unix)]
use tokio::net::{UnixListener, UnixStream};
use tokio::sync::{broadcast, mpsc};
use tracing::{debug, error, info, warn};

// ──────────────────────────────────────────────────────────────────────────────
// Configuration
// ──────────────────────────────────────────────────────────────────────────────

#[derive(Debug, Clone, serde::Deserialize)]
pub struct CustomUnixConfig {
    /// Unix socket path where hardware/SDK code writes DataEnvelopes.
    #[serde(default = "default_socket_path")]
    pub socket_path: PathBuf,
    /// vehicle_id to stamp on envelopes that don't set it (fallback).
    pub vehicle_id: String,
}

fn default_socket_path() -> PathBuf {
    PathBuf::from("/var/run/systemscale/ingest.sock")
}

// ──────────────────────────────────────────────────────────────────────────────
// JSON frame format (used by Python/Go SDK log() calls)
// ──────────────────────────────────────────────────────────────────────────────

/// Inbound JSON frame from the SDK's log() call.
#[derive(Debug, Deserialize)]
struct LogFrame {
    /// Stream type: "telemetry" | "sensor" | "event" | "log" | "video_meta"
    #[serde(default = "default_stream")]
    stream: String,
    /// Arbitrary JSON data — stored as JSON bytes in payload.
    data: serde_json::Value,
    #[serde(default)]
    lat: f64,
    #[serde(default)]
    lon: f64,
    #[serde(default)]
    alt: f32,
    /// Override vehicle_id (usually set from adapter config).
    vehicle_id: Option<String>,
    /// Nanosecond timestamp. Uses current time if omitted.
    timestamp_ns: Option<u64>,
}

fn default_stream() -> String { "telemetry".into() }

/// Outbound JSON frame sent back to the SDK for a command.
#[derive(Debug, Serialize)]
struct CommandFrame {
    #[serde(rename = "type")]
    frame_type: &'static str, // always "command"
    id: String,
    command_type: String,
    data: serde_json::Value,
    priority: &'static str,
}

/// Inbound ACK JSON frame from the SDK's cmd.ack() / cmd.reject().
#[derive(Debug, Deserialize)]
struct AckFrame {
    command_id: String,
    status: String,  // "ok" | "rejected" | "failed"
    #[serde(default)]
    message: String,
}

// ──────────────────────────────────────────────────────────────────────────────
// Adapter
// ──────────────────────────────────────────────────────────────────────────────

#[cfg(unix)]
pub struct CustomUnixAdapter {
    cfg: CustomUnixConfig,
    /// Envelope sender: connection handlers → adapter's data_stream()
    envelope_tx: mpsc::Sender<DataEnvelope>,
    envelope_rx: Arc<tokio::sync::Mutex<mpsc::Receiver<DataEnvelope>>>,
    /// Command broadcast: adapter's send_command() → all active connections
    cmd_tx: broadcast::Sender<Arc<CommandEnvelope>>,
    /// ACK channel: connection handlers → send_command() waiters
    ack_tx: mpsc::Sender<AckFrame>,
    ack_rx: Arc<tokio::sync::Mutex<mpsc::Receiver<AckFrame>>>,
}

#[cfg(unix)]
impl CustomUnixAdapter {
    pub async fn new(cfg: CustomUnixConfig) -> Result<Self> {
        // Create socket directory if needed
        if let Some(parent) = cfg.socket_path.parent() {
            tokio::fs::create_dir_all(parent)
                .await
                .with_context(|| format!("create socket dir {}", parent.display()))?;
        }

        // Remove stale socket from previous run
        if cfg.socket_path.exists() {
            tokio::fs::remove_file(&cfg.socket_path).await.ok();
        }

        let (envelope_tx, envelope_rx) = mpsc::channel(4096);
        let (cmd_tx, _)                = broadcast::channel(64);
        let (ack_tx, ack_rx)           = mpsc::channel(256);

        let adapter = Self {
            cfg,
            envelope_tx,
            envelope_rx: Arc::new(tokio::sync::Mutex::new(envelope_rx)),
            cmd_tx,
            ack_tx,
            ack_rx: Arc::new(tokio::sync::Mutex::new(ack_rx)),
        };

        // Spawn the Unix socket accept loop
        adapter.start_accept_loop().await?;
        Ok(adapter)
    }

    async fn start_accept_loop(&self) -> Result<()> {
        let listener = UnixListener::bind(&self.cfg.socket_path)
            .with_context(|| format!("bind {}", self.cfg.socket_path.display()))?;

        info!(
            socket = %self.cfg.socket_path.display(),
            "Custom Unix adapter listening"
        );

        let envelope_tx = self.envelope_tx.clone();
        let cmd_tx      = self.cmd_tx.clone();
        let ack_tx      = self.ack_tx.clone();
        let vehicle_id  = self.cfg.vehicle_id.clone();

        tokio::spawn(async move {
            loop {
                match listener.accept().await {
                    Ok((stream, _)) => {
                        debug!("New Unix socket connection");
                        let etx  = envelope_tx.clone();
                        let crx  = cmd_tx.subscribe();
                        let atx  = ack_tx.clone();
                        let vid  = vehicle_id.clone();
                        tokio::spawn(handle_connection(stream, etx, crx, atx, vid));
                    }
                    Err(e) => {
                        error!(error = %e, "Unix socket accept error");
                        tokio::time::sleep(std::time::Duration::from_millis(100)).await;
                    }
                }
            }
        });

        Ok(())
    }
}

#[cfg(unix)]
impl VehicleDataSource for CustomUnixAdapter {
    fn data_stream(&self) -> Pin<Box<dyn Stream<Item = DataEnvelope> + Send>> {
        let rx = Arc::clone(&self.envelope_rx);
        Box::pin(async_stream::stream! {
            let mut guard = rx.lock().await;
            while let Some(env) = guard.recv().await {
                yield env;
            }
        })
    }

    fn send_command(
        &self,
        cmd: CommandEnvelope,
    ) -> Pin<Box<dyn std::future::Future<Output = Result<Ack>> + Send>> {
        let cmd_tx = self.cmd_tx.clone();
        let ack_rx = Arc::clone(&self.ack_rx);
        let cmd_id = cmd.command_id.clone();

        Box::pin(async move {
            // Broadcast command to all active socket connections
            let cmd_arc = Arc::new(cmd);
            if cmd_tx.receiver_count() == 0 {
                // No SDK clients connected
                return Ok(Ack {
                    command_id:  cmd_id,
                    status:      AckStatus::Rejected,
                    message:     "No SDK client connected to receive command".into(),
                    ack_time_ns: now_ns(),
                });
            }
            cmd_tx.send(cmd_arc).ok();

            // Wait for ACK from the SDK client (5s timeout)
            let deadline = tokio::time::Instant::now() + std::time::Duration::from_secs(5);
            let mut guard = ack_rx.lock().await;
            loop {
                match tokio::time::timeout_at(deadline, guard.recv()).await {
                    Ok(Some(ack_frame)) if ack_frame.command_id == cmd_id => {
                        let status = match ack_frame.status.as_str() {
                            "ok"       => AckStatus::Completed,
                            "rejected" => AckStatus::Rejected,
                            _          => AckStatus::Failed,
                        };
                        return Ok(Ack {
                            command_id:  cmd_id,
                            status,
                            message:     ack_frame.message,
                            ack_time_ns: now_ns(),
                        });
                    }
                    Ok(Some(_)) => continue, // ACK for different command
                    Ok(None) | Err(_) => {
                        return Ok(Ack {
                            command_id:  cmd_id,
                            status:      AckStatus::Timeout,
                            message:     "SDK client did not ACK within 5s".into(),
                            ack_time_ns: now_ns(),
                        });
                    }
                }
            }
        })
    }

    fn adapter_id(&self) -> &str { "custom-unix" }
}

// ──────────────────────────────────────────────────────────────────────────────
// Per-connection handler
// ──────────────────────────────────────────────────────────────────────────────

#[cfg(unix)]
async fn handle_connection(
    mut stream: UnixStream,
    envelope_tx: mpsc::Sender<DataEnvelope>,
    mut cmd_rx: broadcast::Receiver<Arc<CommandEnvelope>>,
    ack_tx: mpsc::Sender<AckFrame>,
    vehicle_id: String,
) {
    let (mut reader, mut writer) = stream.split();
    let mut len_buf = [0u8; 4];

    loop {
        tokio::select! {
            // Read inbound frame (telemetry data OR ack from SDK)
            read_result = reader.read_exact(&mut len_buf) => {
                if read_result.is_err() {
                    debug!("Unix socket connection closed");
                    return;
                }
                let len = u32::from_le_bytes(len_buf) as usize;
                if len == 0 || len > 4_194_304 {
                    warn!(len, "Invalid frame length on Unix socket — closing");
                    return;
                }

                let mut payload = vec![0u8; len];
                if reader.read_exact(&mut payload).await.is_err() {
                    return;
                }

                // Dispatch: JSON frame (SDK) or raw proto?
                if payload.first() == Some(&b'{') {
                    handle_json_frame(&payload, &envelope_tx, &ack_tx, &vehicle_id).await;
                } else {
                    handle_proto_frame(payload, &envelope_tx, &vehicle_id).await;
                }
            }

            // Send outbound command to SDK client
            cmd_result = cmd_rx.recv() => {
                match cmd_result {
                    Ok(cmd) => {
                        if let Err(e) = send_command_frame(&mut writer, &cmd).await {
                            warn!(error = %e, "Failed to send command to SDK client");
                            return;
                        }
                    }
                    Err(broadcast::error::RecvError::Lagged(n)) => {
                        warn!(dropped = n, "Command broadcast lag — SDK client too slow");
                    }
                    Err(broadcast::error::RecvError::Closed) => return,
                }
            }
        }
    }
}

/// Handles a JSON frame from the SDK. Two possible formats:
/// 1. Log frame: {"stream":"telemetry","data":{...},...}
/// 2. ACK frame: {"command_id":"...","status":"ok","message":"..."}
async fn handle_json_frame(
    payload: &[u8],
    envelope_tx: &mpsc::Sender<DataEnvelope>,
    ack_tx: &mpsc::Sender<AckFrame>,
    default_vehicle_id: &str,
) {
    // Try to parse as ACK first (has command_id field)
    if let Ok(ack) = serde_json::from_slice::<AckFrame>(payload) {
        ack_tx.try_send(ack).ok();
        return;
    }

    // Otherwise parse as log frame
    let frame: LogFrame = match serde_json::from_slice(payload) {
        Ok(f) => f,
        Err(e) => {
            warn!(error = %e, "Invalid JSON frame — dropping");
            return;
        }
    };

    let stream_type = match frame.stream.as_str() {
        "telemetry"  => StreamType::Telemetry,
        "sensor"     => StreamType::Sensor,
        "event"      => StreamType::Event,
        "log"        => StreamType::Log,
        "video_meta" => StreamType::VideoMeta,
        other => {
            warn!(stream = other, "Unknown stream type, defaulting to Telemetry");
            StreamType::Telemetry
        }
    };

    // Serialize the data dict as JSON bytes into the payload
    let payload_bytes = match serde_json::to_vec(&frame.data) {
        Ok(b) => Bytes::from(b),
        Err(e) => {
            warn!(error = %e, "Could not re-serialize data field");
            return;
        }
    };

    let env = DataEnvelope {
        vehicle_id:   frame.vehicle_id.unwrap_or_else(|| default_vehicle_id.to_string()),
        timestamp_ns: frame.timestamp_ns.unwrap_or_else(now_ns),
        stream_type,
        payload:      payload_bytes,
        lat:          frame.lat,
        lon:          frame.lon,
        alt:          frame.alt,
        seq:          0, // assigned by sequencer in agent-core
        // fleet_id, org_id, stream_name left empty — filled by relay / local API
        ..Default::default()
    };

    envelope_tx.try_send(env).ok();
}

/// Handles a raw proto DataEnvelope frame (4-byte LE length prefix + proto bytes).
/// Parses only the routing fields (vehicle_id, timestamp, stream_type, lat/lon/alt).
async fn handle_proto_frame(
    payload: Vec<u8>,
    envelope_tx: &mpsc::Sender<DataEnvelope>,
    default_vehicle_id: &str,
) {
    // Partial proto decode: extract routing fields without full deserialize.
    // This mirrors the fast_read_stream_type() approach in the relay.
    let env = partial_decode_envelope(payload, default_vehicle_id);
    envelope_tx.try_send(env).ok();
}

/// Sends a CommandEnvelope to the SDK client as a JSON frame.
async fn send_command_frame(
    writer: &mut (impl AsyncWriteExt + Unpin),
    cmd: &CommandEnvelope,
) -> Result<()> {
    // Deserialize command payload as JSON (SDK passes it as JSON bytes)
    let data: serde_json::Value = serde_json::from_slice(&cmd.payload)
        .unwrap_or(serde_json::Value::Null);

    let frame = CommandFrame {
        frame_type:   "command",
        id:           cmd.command_id.clone(),
        command_type: "command".to_string(), // operator sets type in data field
        data,
        priority: match cmd.priority {
            Priority::Emergency => "emergency",
            Priority::High      => "high",
            Priority::Normal    => "normal",
        },
    };

    let json = serde_json::to_vec(&frame)?;
    let len  = json.len() as u32;
    writer.write_all(&len.to_le_bytes()).await?;
    writer.write_all(&json).await?;
    writer.flush().await?;
    Ok(())
}

// ──────────────────────────────────────────────────────────────────────────────
// Partial proto decode for raw frames
// ──────────────────────────────────────────────────────────────────────────────

fn partial_decode_envelope(data: Vec<u8>, default_vehicle_id: &str) -> DataEnvelope {
    let mut env = DataEnvelope {
        vehicle_id:   default_vehicle_id.to_string(),
        timestamp_ns: now_ns(),
        stream_type:  StreamType::Telemetry,
        payload:      Bytes::from(data.clone()),
        lat: 0.0, lon: 0.0, alt: 0.0,
        seq: 0,
        ..Default::default()
    };

    let mut pos = 0;
    while pos < data.len() {
        let (tag, n) = read_varint(&data[pos..]);
        if n == 0 { break; }
        pos += n;
        let field  = (tag >> 3) as u32;
        let wire   = tag & 0x7;

        match (field, wire) {
            (1, 2) => { // vehicle_id (string)
                let (len, n2) = read_varint(&data[pos..]);
                pos += n2;
                if pos + len as usize <= data.len() {
                    env.vehicle_id = String::from_utf8_lossy(&data[pos..pos+len as usize]).into();
                    pos += len as usize;
                }
            }
            (2, 0) => { // timestamp_ns (varint)
                let (v, n2) = read_varint(&data[pos..]);
                pos += n2;
                env.timestamp_ns = v;
            }
            (3, 0) => { // stream_type (varint enum)
                let (v, n2) = read_varint(&data[pos..]);
                pos += n2;
                env.stream_type = match v {
                    1 => StreamType::Telemetry,
                    2 => StreamType::Event,
                    3 => StreamType::Sensor,
                    4 => StreamType::VideoMeta,
                    5 => StreamType::Log,
                    _ => StreamType::Telemetry,
                };
            }
            (5, 5) => { // lat (float32, wire type 5)
                if pos + 4 <= data.len() {
                    let bits = u32::from_le_bytes(data[pos..pos+4].try_into().unwrap_or([0;4]));
                    env.lat = f32::from_bits(bits) as f64;
                    pos += 4;
                }
            }
            (6, 5) => { // lon (float32)
                if pos + 4 <= data.len() {
                    let bits = u32::from_le_bytes(data[pos..pos+4].try_into().unwrap_or([0;4]));
                    env.lon = f32::from_bits(bits) as f64;
                    pos += 4;
                }
            }
            (7, 5) => { // alt (float32)
                if pos + 4 <= data.len() {
                    let bits = u32::from_le_bytes(data[pos..pos+4].try_into().unwrap_or([0;4]));
                    env.alt = f32::from_bits(bits);
                    pos += 4;
                }
            }
            (_, 0) => { let (_, n2) = read_varint(&data[pos..]); pos += n2; }
            (_, 1) => { pos += 8; }
            (_, 2) => {
                let (len, n2) = read_varint(&data[pos..]);
                pos += n2 + len as usize;
            }
            (_, 5) => { pos += 4; }
            _      => break,
        }
    }
    env
}

fn read_varint(data: &[u8]) -> (u64, usize) {
    let mut result = 0u64;
    for (i, &b) in data.iter().enumerate() {
        result |= ((b & 0x7F) as u64) << (7 * i);
        if b & 0x80 == 0 { return (result, i + 1); }
        if i >= 9 { return (0, 0); }
    }
    (0, 0)
}
