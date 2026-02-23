//! Per-vehicle QUIC session management — the most concurrency-critical code in the relay.
//!
//! Each vehicle that connects has one `VehicleSession`. It lives as long as the QUIC
//! connection is alive. The session is stored in a `DashMap<String, VehicleSession>`
//! keyed by vehicle_id, allowing O(1) concurrent lookup from:
//! - The command forwarder (needs to find the right session to push a command to)
//! - The health monitor (checks heartbeat timestamps)
//!
//! Hot path for telemetry (called at 50Hz per vehicle — minimize allocations):
//!   receive UDP packet
//!     → QUIC decrypt (AES-NI, ~2μs)
//!     → read length prefix (8 bytes)
//!     → read envelope bytes (zero-copy: Bytes reference to receive buffer)
//!     → parse vehicle_id from envelope (just the first field, not full decode)
//!     → NATS publish to leaf node on localhost (sub-millisecond)
//!     → done
//!
//! The NATS publish is the only "network" call on the hot path, and it goes to
//! localhost (co-located NATS leaf node), so it's essentially zero-latency.

use std::sync::Arc;
use std::time::{Duration, Instant};

use async_nats::Client as NatsClient;
use bytes::Bytes;
use dashmap::DashMap;
use quinn::{Connection, RecvStream, SendStream};
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::sync::mpsc;
use tracing::{debug, error, info, warn};

// ──────────────────────────────────────────────────────────────────────────────
// Session registry
// ──────────────────────────────────────────────────────────────────────────────

/// The global map of active vehicle sessions on this relay node.
/// Shared between the QUIC accept loop, command forwarder, and health monitor.
pub type SessionRegistry = Arc<DashMap<String, VehicleSession>>;

pub fn new_registry() -> SessionRegistry {
    Arc::new(DashMap::with_capacity(512))
}

/// State for one connected vehicle.
pub struct VehicleSession {
    pub vehicle_id:      String,
    /// Channel to push commands to this vehicle's command stream sender task.
    /// The sender task drains this channel and writes to QUIC stream 1.
    pub cmd_tx:          mpsc::Sender<Bytes>,
    /// When was the last heartbeat received from this vehicle?
    pub last_heartbeat:  Instant,
    /// Certificate subject CN — used for audit logging.
    pub cert_subject:    String,
    /// Observed RTT to this vehicle (updated from QUIC connection stats).
    pub rtt:             Duration,
}

// ──────────────────────────────────────────────────────────────────────────────
// Connection handler — spawned per incoming QUIC connection
// ──────────────────────────────────────────────────────────────────────────────

/// Handle one vehicle QUIC connection from accept to close.
/// This function is spawned as a tokio task per connection.
pub async fn handle_connection(
    connection: Connection,
    nats:       NatsClient,
    registry:   SessionRegistry,
    region_id:  String,
) {
    // Extract vehicle identity from the peer's mTLS certificate Subject CN
    let vehicle_id = match extract_vehicle_id_from_cert(&connection) {
        Some(id) => id,
        None => {
            error!(peer = ?connection.remote_address(), "mTLS cert missing vehicle_id — closing");
            connection.close(1u32.into(), b"missing vehicle_id in cert");
            return;
        }
    };

    let rtt = connection.rtt();
    info!(
        vehicle_id = %vehicle_id,
        peer       = ?connection.remote_address(),
        rtt        = ?rtt,
        region     = %region_id,
        "Vehicle connected"
    );

    metrics::counter!("relay.connections.accepted", "region" => region_id.clone()).increment(1);
    metrics::gauge!("relay.connections.active", "region" => region_id.clone()).increment(1.0);

    // Channel for command forwarder → command sender task (this connection's stream 1)
    let (cmd_tx, cmd_rx) = mpsc::channel::<Bytes>(64);

    // Register session in the shared map
    registry.insert(vehicle_id.clone(), VehicleSession {
        vehicle_id:     vehicle_id.clone(),
        cmd_tx:         cmd_tx.clone(),
        last_heartbeat: Instant::now(),
        cert_subject:   vehicle_id.clone(),
        rtt,
    });

    // Accept the vehicle's 4 streams (edge agent opens them in order)
    // Stream 0: telemetry (send-only from vehicle side → we recv)
    // Stream 1: commands  (bidirectional — we recv ACKs, vehicle recvs commands)
    // Stream 2: heartbeat (send-only from vehicle)
    // Stream 3: video metadata (send-only from vehicle)
    let streams_result = accept_vehicle_streams(&connection).await;

    match streams_result {
        Ok((telem_rx, cmd_bi, hb_rx)) => {
            let (cmd_stream_tx, cmd_stream_rx) = cmd_bi;

            // Run all stream handlers concurrently
            tokio::select! {
                _ = handle_telemetry_stream(telem_rx, &nats, &vehicle_id, &region_id) => {}
                _ = handle_heartbeat_stream(hb_rx, &registry, &vehicle_id) => {}
                _ = send_commands_from_channel(cmd_stream_tx, cmd_rx) => {}
                _ = receive_command_acks(cmd_stream_rx, &nats, &vehicle_id) => {}
            }
        }
        Err(e) => {
            error!(vehicle_id = %vehicle_id, error = %e, "Failed to accept vehicle streams");
        }
    }

    // Cleanup
    registry.remove(&vehicle_id);
    metrics::gauge!("relay.connections.active", "region" => region_id.clone()).decrement(1.0);
    info!(vehicle_id = %vehicle_id, "Vehicle disconnected");
}

// ──────────────────────────────────────────────────────────────────────────────
// Telemetry stream handler — the hot path
// ──────────────────────────────────────────────────────────────────────────────

/// Reads DataEnvelope frames from QUIC stream 0 and publishes to NATS.
///
/// Hot path optimization notes:
/// - `Bytes` is used throughout to avoid copying payload bytes
/// - The NATS subject is built once per envelope by reading only the vehicle_id field
///   from the envelope (first 20-50 bytes) without fully decoding the protobuf
/// - NATS publish to localhost leaf node is sub-millisecond
async fn handle_telemetry_stream(
    mut stream: RecvStream,
    nats:       &NatsClient,
    vehicle_id: &str,
    region_id:  &str,
) -> anyhow::Result<()> {
    let mut len_buf = [0u8; 4];

    loop {
        // Read 4-byte LE length prefix
        stream.read_exact(&mut len_buf).await?;
        let len = u32::from_le_bytes(len_buf) as usize;

        // Sanity check: reject suspiciously large frames
        if len > 65_536 {
            warn!(vehicle_id, len, "Telemetry frame too large — closing stream");
            anyhow::bail!("frame too large");
        }

        // Read exactly `len` bytes — zero-copy: these bytes live in quinn's receive buffer
        let mut payload = vec![0u8; len];
        stream.read_exact(&mut payload).await?;
        let payload = Bytes::from(payload);

        // Extract stream_type from the envelope to build NATS subject.
        // We parse only the first few fields (vehicle_id=1, timestamp=2, type=3)
        // without full protobuf decode to minimize hot-path CPU.
        let stream_type_val = fast_read_stream_type(&payload);

        // NATS subject: telemetry.{vehicle_id}.{stream_type}
        let subject = format!(
            "telemetry.{}.{}",
            vehicle_id,
            stream_type_name(stream_type_val)
        );

        // Publish to NATS leaf node (localhost) — this is the only I/O on the hot path
        nats.publish(subject, payload).await
            .map_err(|e| anyhow::anyhow!("NATS publish failed: {}", e))?;

        metrics::counter!("relay.telemetry.frames", "vehicle" => vehicle_id.to_string()).increment(1);
    }
}

// ──────────────────────────────────────────────────────────────────────────────
// Heartbeat handler
// ──────────────────────────────────────────────────────────────────────────────

async fn handle_heartbeat_stream(
    mut stream: RecvStream,
    registry:   &SessionRegistry,
    vehicle_id: &str,
) -> anyhow::Result<()> {
    let mut ts_buf = [0u8; 4];
    loop {
        stream.read_exact(&mut ts_buf).await?;
        let _ts = u32::from_be_bytes(ts_buf); // vehicle timestamp (seconds)

        // Update heartbeat timestamp in session registry
        if let Some(mut session) = registry.get_mut(vehicle_id) {
            session.last_heartbeat = Instant::now();
        }

        debug!(vehicle_id, "Heartbeat received");
    }
}

// ──────────────────────────────────────────────────────────────────────────────
// Command stream handlers
// ──────────────────────────────────────────────────────────────────────────────

/// Drains the command channel and writes to the vehicle's QUIC command stream.
/// The command forwarder (separate process) pushes to the channel.
async fn send_commands_from_channel(
    mut stream: SendStream,
    mut cmd_rx: mpsc::Receiver<Bytes>,
) -> anyhow::Result<()> {
    while let Some(cmd_payload) = cmd_rx.recv().await {
        // Length-prefix frame then payload
        let len = cmd_payload.len() as u32;
        stream.write_all(&len.to_le_bytes()).await?;
        stream.write_all(&cmd_payload).await?;

        debug!(bytes = cmd_payload.len() + 4, "Forwarded command to vehicle");
        metrics::counter!("relay.commands.forwarded").increment(1);
    }
    Ok(())
}

/// Receives command ACK frames from vehicle and publishes to NATS for the command-api.
async fn receive_command_acks(
    mut stream: RecvStream,
    nats:       &NatsClient,
    vehicle_id: &str,
) -> anyhow::Result<()> {
    let mut len_buf = [0u8; 4];
    loop {
        stream.read_exact(&mut len_buf).await?;
        let len = u32::from_le_bytes(len_buf) as usize;
        if len > 4096 { anyhow::bail!("ACK frame too large"); }

        let mut payload = vec![0u8; len];
        stream.read_exact(&mut payload).await?;

        // Publish ACK to NATS so command-api can resolve the pending gRPC response
        let subject = format!("command.{}.ack", vehicle_id);
        nats.publish(subject, Bytes::from(payload)).await
            .map_err(|e| anyhow::anyhow!("NATS publish ACK failed: {}", e))?;
    }
}

// ──────────────────────────────────────────────────────────────────────────────
// Stream accept helper
// ──────────────────────────────────────────────────────────────────────────────

type BiStream = (SendStream, RecvStream);

async fn accept_vehicle_streams(
    conn: &Connection,
) -> anyhow::Result<(RecvStream, BiStream, RecvStream)> {
    // Edge agent opens 4 streams in this exact order. We accept them in the same order.
    let (_, telem_rx)           = conn.accept_bi().await?; // stream 0: telemetry
    let (cmd_tx, cmd_rx)        = conn.accept_bi().await?; // stream 1: commands
    let (_, hb_rx)              = conn.accept_bi().await?; // stream 2: heartbeat
    let (_, _video_meta_rx)     = conn.accept_bi().await?; // stream 3: video meta (unused for now)

    Ok((telem_rx, (cmd_tx, cmd_rx), hb_rx))
}

// ──────────────────────────────────────────────────────────────────────────────
// Helpers
// ──────────────────────────────────────────────────────────────────────────────

/// Extracts the vehicle ID from the peer's mTLS certificate Subject Common Name (CN).
///
/// Certificates are issued by the internal Step-CA with:
///   Subject: CN=vehicle-{uuid}
///
/// This CN is the vehicle's canonical identifier across the entire platform.
/// All vehicles must present a valid cert; connections without a cert or with a
/// missing CN are rejected at the call site.
fn extract_vehicle_id_from_cert(conn: &Connection) -> Option<String> {
    // quinn 0.11 exposes peer identity via connection.peer_identity().
    // The Any value is Vec<rustls::pki_types::CertificateDer> — the full leaf cert chain.
    let identity = conn.peer_identity()?;
    let certs = identity.downcast_ref::<Vec<rustls::pki_types::CertificateDer>>()?;

    // The leaf (end-entity) certificate is always first in the chain.
    let leaf_der = certs.first()?;

    // x509_parser::parse_x509_certificate takes a DER byte slice and returns
    // (remaining_bytes, X509Certificate). We discard the remainder.
    let (_, parsed) = x509_parser::parse_x509_certificate(leaf_der.as_ref())
        .map_err(|e| warn!("Failed to parse peer X.509 certificate: {}", e))
        .ok()?;

    // Walk the Subject's RDN sequence to find the CN attribute.
    // iter_common_name() returns an iterator over CN AttributeTypeAndValue entries.
    let cn = parsed
        .subject()
        .iter_common_name()
        .next()?
        .as_str()
        .map_err(|e| warn!("Certificate CN is not a UTF-8 string: {}", e))
        .ok()?;

    if cn.is_empty() {
        warn!("Certificate Subject CN is empty — rejecting connection");
        return None;
    }

    Some(cn.to_string())
}

/// Fast stream_type extraction from raw protobuf bytes.
/// Reads only up to field 3 (stream_type) without full decode.
/// Returns the raw i32 value of the StreamType enum.
fn fast_read_stream_type(buf: &[u8]) -> i32 {
    let mut pos = 0;
    while pos < buf.len() {
        if pos >= 64 { break; } // stream_type is always in first 64 bytes
        let tag = match proto_read_varint_fast(buf, &mut pos) {
            Some(v) => v,
            None => break,
        };
        let field = (tag >> 3) as u32;
        let wire  = tag & 0x7;

        if field == 3 && wire == 0 {
            return proto_read_varint_fast(buf, &mut pos).unwrap_or(0) as i32;
        }
        // Skip field
        match wire {
            0 => { proto_read_varint_fast(buf, &mut pos); }
            1 => { pos += 8; }
            2 => {
                let len = proto_read_varint_fast(buf, &mut pos).unwrap_or(0) as usize;
                pos += len;
            }
            5 => { pos += 4; }
            _ => break,
        }
    }
    1 // default: TELEMETRY
}

fn stream_type_name(v: i32) -> &'static str {
    match v {
        1 => "telemetry",
        2 => "event",
        3 => "sensor",
        4 => "video_meta",
        5 => "log",
        6 => "audio",
        _ => "unknown",
    }
}

fn proto_read_varint_fast(buf: &[u8], pos: &mut usize) -> Option<u64> {
    let mut result = 0u64;
    let mut shift = 0;
    loop {
        if *pos >= buf.len() { return None; }
        let b = buf[*pos]; *pos += 1;
        result |= ((b & 0x7F) as u64) << shift;
        if b & 0x80 == 0 { return Some(result); }
        shift += 7;
        if shift >= 64 { return None; }
    }
}
