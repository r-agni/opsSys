//! MAVLink v2 protocol adapter.
//!
//! Connects to an autopilot (PX4 or ArduPilot) via UDP or serial.
//! Translates MAVLink messages into `DataEnvelope` objects.
//! Translates incoming `CommandEnvelope` payloads into MAVLink COMMAND_LONG frames.
//!
//! In typical deployment, `mavlink-router` runs as a system daemon that reads
//! the flight controller's UART and re-broadcasts to UDP localhost:14550.
//! This adapter connects to that UDP endpoint. For direct serial connections,
//! use `MavlinkAdapterConfig { connection: MavlinkConnection::Serial { ... } }`.

use std::net::SocketAddr;
use std::pin::Pin;
use std::sync::Arc;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use adapter_trait::{
    Ack, AckStatus, AdapterType, CommandEnvelope, DataEnvelope, Priority,
    StreamType, VehicleDataSource, now_ns,
};
use anyhow::{Context, Result};
use async_stream::stream;
use bytes::Bytes;
use futures::Stream;
use mavlink::{
    ardupilotmega::{
        MavMessage, COMMAND_LONG_DATA, GLOBAL_POSITION_INT_DATA,
        ATTITUDE_DATA, BATTERY_STATUS_DATA, HEARTBEAT_DATA,
        SYS_STATUS_DATA, GPS_RAW_INT_DATA,
        MavCmd, MavResult,
    },
    MavHeader, MavlinkVersion,
};
use tokio::net::UdpSocket;
use tokio::sync::{mpsc, Mutex};
use tokio::time::timeout;
use tracing::{debug, error, info, warn};

// ──────────────────────────────────────────────────────────────────────────────
// Configuration
// ──────────────────────────────────────────────────────────────────────────────

#[derive(Debug, Clone, serde::Deserialize)]
pub struct MavlinkAdapterConfig {
    pub vehicle_id: String,
    pub connection: MavlinkConnection,
    /// System ID this edge agent presents itself as on the MAVLink bus.
    /// Use 255 (GCS ID) so the autopilot accepts commands from us.
    #[serde(default = "default_system_id")]
    pub gcs_system_id: u8,
    /// Component ID for the edge agent's MAVLink identity.
    #[serde(default = "default_component_id")]
    pub gcs_component_id: u8,
}

#[derive(Debug, Clone, serde::Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum MavlinkConnection {
    /// UDP socket to mavlink-router output (most common).
    Udp { addr: SocketAddr },
    /// Direct serial connection to flight controller.
    Serial { port: String, baud: u32 },
}

fn default_system_id() -> u8 { 255 }
fn default_component_id() -> u8 { 190 }

// ──────────────────────────────────────────────────────────────────────────────
// Adapter implementation
// ──────────────────────────────────────────────────────────────────────────────

pub struct MavlinkAdapter {
    vehicle_id:       String,
    config:           MavlinkAdapterConfig,
    adapter_id_str:   String,
    /// Shared socket for both receive (data_stream) and send (send_command).
    /// Both paths share the same UDP socket — MAVLink is full-duplex on one socket.
    socket:           Arc<UdpSocket>,
    /// Pending command ACK waiters: command_id → oneshot sender.
    /// send_command() registers here; the receive loop resolves it when COMMAND_ACK arrives.
    pending_acks:     Arc<Mutex<std::collections::HashMap<u16, tokio::sync::oneshot::Sender<MavResult>>>>,
    /// Remote address of the autopilot (populated on first HEARTBEAT received).
    autopilot_addr:   Arc<Mutex<Option<SocketAddr>>>,
    /// Monotonically increasing sequence counter for outbound command frames.
    cmd_seq:          Arc<std::sync::atomic::AtomicU8>,
}

impl MavlinkAdapter {
    pub async fn new(config: MavlinkAdapterConfig) -> Result<Self> {
        let adapter_id_str = match &config.connection {
            MavlinkConnection::Udp { addr } => format!("mavlink-udp:{}", addr),
            MavlinkConnection::Serial { port, baud } => format!("mavlink-serial:{}@{}", port, baud),
        };

        // Bind a local UDP socket. For UDP connections we bind to 0.0.0.0:0
        // and connect() to the remote so send() works without specifying address each time.
        let socket = match &config.connection {
            MavlinkConnection::Udp { addr } => {
                let sock = UdpSocket::bind("0.0.0.0:0").await
                    .context("binding MAVLink UDP socket")?;
                sock.connect(addr).await
                    .context("connecting MAVLink UDP socket to router")?;
                info!(adapter = %adapter_id_str, remote = %addr, "MAVLink UDP connected");
                Arc::new(sock)
            },
            MavlinkConnection::Serial { .. } => {
                // Serial path: in production this would use tokio-serial.
                // For now, return an error with a clear message.
                anyhow::bail!("Serial MAVLink connections require the tokio-serial feature. \
                               Use mavlink-router to expose serial as UDP for now.");
            }
        };

        Ok(Self {
            vehicle_id: config.vehicle_id.clone(),
            adapter_id_str,
            config,
            socket,
            pending_acks:   Arc::new(Mutex::new(std::collections::HashMap::new())),
            autopilot_addr: Arc::new(Mutex::new(None)),
            cmd_seq:        Arc::new(std::sync::atomic::AtomicU8::new(0)),
        })
    }
}

impl VehicleDataSource for MavlinkAdapter {
    fn data_stream(&self) -> Pin<Box<dyn Stream<Item = DataEnvelope> + Send>> {
        let vehicle_id     = self.vehicle_id.clone();
        let socket         = Arc::clone(&self.socket);
        let pending_acks   = Arc::clone(&self.pending_acks);
        let autopilot_addr = Arc::clone(&self.autopilot_addr);

        Box::pin(stream! {
            let mut buf = vec![0u8; 280]; // MAVLink v2 max frame = 280 bytes
            let mut seq: u32 = 0;

            loop {
                // Receive one MAVLink UDP datagram
                let n = match socket.recv(&mut buf).await {
                    Ok(n) => n,
                    Err(e) => {
                        error!(vehicle = %vehicle_id, error = %e, "MAVLink UDP recv error — retrying");
                        tokio::time::sleep(Duration::from_millis(10)).await;
                        continue;
                    }
                };

                // Parse MAVLink frame from raw bytes
                let frame = match mavlink::read_v2_msg::<MavMessage, _>(
                    &mut mavlink::peek_reader::PeekReader::new(std::io::Cursor::new(&buf[..n]))
                ) {
                    Ok((_hdr, msg)) => msg,
                    Err(e) => {
                        debug!(vehicle = %vehicle_id, error = %e, "MAVLink parse error (non-fatal)");
                        continue;
                    }
                };

                let ts = now_ns();
                seq = seq.wrapping_add(1);

                // Route to COMMAND_ACK handler (unblocks pending send_command futures)
                if let MavMessage::COMMAND_ACK(ref ack_data) = frame {
                    let mut guards = pending_acks.lock().await;
                    if let Some(tx) = guards.remove(&(ack_data.command as u16)) {
                        let _ = tx.send(ack_data.result);
                    }
                    continue; // ACKs don't produce DataEnvelopes
                }

                // Record autopilot address on first HEARTBEAT (needed for send_command)
                if let MavMessage::HEARTBEAT(_) = &frame {
                    // The socket is connected so we can't get the remote address from recv.
                    // In connected-UDP mode, autopilot_addr is set at construction from config.
                    // We mark it as "known" so send_command knows it's safe to send.
                    let mut addr = autopilot_addr.lock().await;
                    if addr.is_none() {
                        *addr = Some("0.0.0.0:0".parse().unwrap()); // sentinel: "connected"
                        info!(vehicle = %vehicle_id, "Received first HEARTBEAT from autopilot");
                    }
                }

                // Translate MAVLink message to DataEnvelope
                if let Some(env) = translate_mavlink_to_envelope(&vehicle_id, &frame, ts, seq) {
                    yield env;
                }
            }
        })
    }

    fn send_command(
        &self,
        cmd: CommandEnvelope,
    ) -> Pin<Box<dyn std::future::Future<Output = Result<Ack>> + Send>> {
        let vehicle_id   = self.vehicle_id.clone();
        let socket       = Arc::clone(&self.socket);
        let pending_acks = Arc::clone(&self.pending_acks);
        let gcs_sysid    = self.config.gcs_system_id;
        let gcs_compid   = self.config.gcs_component_id;
        let seq_counter  = Arc::clone(&self.cmd_seq);

        Box::pin(async move {
            // Decode the command payload as a MavlinkCommand protobuf
            let mav_cmd = decode_mavlink_command(&cmd.payload)
                .context("decoding CommandEnvelope payload as MavlinkCommand proto")?;

            let cmd_id_u16 = mav_cmd.command as u16;
            let (tx, rx) = tokio::sync::oneshot::channel::<MavResult>();

            // Register ACK waiter before sending (avoid race condition)
            {
                let mut guards = pending_acks.lock().await;
                guards.insert(cmd_id_u16, tx);
            }

            // Build and serialize MAVLink COMMAND_LONG frame
            let seq = seq_counter.fetch_add(1, std::sync::atomic::Ordering::Relaxed);
            let header = MavHeader {
                system_id:    gcs_sysid,
                component_id: gcs_compid,
                sequence:     seq,
            };
            let message = MavMessage::COMMAND_LONG(COMMAND_LONG_DATA {
                param1:          mav_cmd.param1,
                param2:          mav_cmd.param2,
                param3:          mav_cmd.param3,
                param4:          mav_cmd.param4,
                param5:          mav_cmd.param5,
                param6:          mav_cmd.param6,
                param7:          mav_cmd.param7,
                command:         num_traits::FromPrimitive::from_u32(mav_cmd.command as u32)
                                     .unwrap_or(MavCmd::MAV_CMD_DO_SET_MODE),
                target_system:   1, // Target the autopilot (system ID 1)
                target_component: 1,
                confirmation:    0,
            });

            let mut out_buf = Vec::with_capacity(280);
            mavlink::write_v2_msg(&mut out_buf, header, &message)
                .context("serializing MAVLink COMMAND_LONG")?;

            socket.send(&out_buf).await
                .context("sending MAVLink COMMAND_LONG via UDP")?;

            debug!(
                vehicle = %vehicle_id,
                command_id = %cmd.command_id,
                mav_cmd = mav_cmd.command,
                "Sent MAVLink COMMAND_LONG"
            );

            // Wait for COMMAND_ACK with timeout
            let ack_timeout = match cmd.priority {
                Priority::Emergency => Duration::from_millis(2000),
                Priority::High      => Duration::from_millis(3000),
                Priority::Normal    => Duration::from_millis(5000),
            };

            let ack_time_ns = now_ns();
            match timeout(ack_timeout, rx).await {
                Ok(Ok(result)) => {
                    let status = match result {
                        MavResult::MAV_RESULT_ACCEPTED         => AckStatus::Accepted,
                        MavResult::MAV_RESULT_TEMPORARILY_REJECTED => AckStatus::Rejected,
                        MavResult::MAV_RESULT_DENIED           => AckStatus::Rejected,
                        MavResult::MAV_RESULT_UNSUPPORTED      => AckStatus::Rejected,
                        MavResult::MAV_RESULT_FAILED           => AckStatus::Failed,
                        _                                       => AckStatus::Failed,
                    };
                    Ok(Ack {
                        command_id: cmd.command_id,
                        status,
                        message: format!("{:?}", result),
                        ack_time_ns,
                    })
                },
                Ok(Err(_)) | Err(_) => {
                    // Remove the pending waiter to avoid leaking it
                    pending_acks.lock().await.remove(&cmd_id_u16);
                    warn!(vehicle = %vehicle_id, command_id = %cmd.command_id, "Command ACK timeout");
                    Ok(Ack {
                        command_id: cmd.command_id,
                        status: AckStatus::Timeout,
                        message: "No COMMAND_ACK from autopilot within deadline".to_string(),
                        ack_time_ns,
                    })
                }
            }
        })
    }

    fn adapter_id(&self) -> &str {
        &self.adapter_id_str
    }
}

// ──────────────────────────────────────────────────────────────────────────────
// MAVLink → DataEnvelope translation
// ──────────────────────────────────────────────────────────────────────────────

fn translate_mavlink_to_envelope(
    vehicle_id: &str,
    msg: &MavMessage,
    timestamp_ns: u64,
    seq: u32,
) -> Option<DataEnvelope> {
    // Build a TelemetryFrame proto from the MAVLink message.
    // We serialize only the relevant fields per message type.
    // The payload is a serialized `TelemetryFrame` proto (partial — only fields this
    // message provides are set; the rest remain zero/default).
    // Consumers that want rich typed data decode the TelemetryFrame proto.
    // The storage layer writes key scalar fields as dedicated QuestDB columns.

    let (stream_type, lat, lon, alt, payload) = match msg {
        MavMessage::GLOBAL_POSITION_INT(d) => {
            let lat = d.lat as f64 / 1e7;
            let lon = d.lon as f64 / 1e7;
            let alt = d.alt as f32 / 1000.0; // mm → m
            let payload = encode_position_payload(lat, lon, alt, d.relative_alt, d.vx, d.vy, d.vz, d.hdg);
            (StreamType::Telemetry, lat, lon, alt, payload)
        },

        MavMessage::ATTITUDE(d) => {
            let payload = encode_attitude_payload(d.roll, d.pitch, d.yaw, d.rollspeed, d.pitchspeed, d.yawspeed);
            (StreamType::Telemetry, 0.0, 0.0, 0.0, payload)
        },

        MavMessage::BATTERY_STATUS(d) => {
            let voltage_mv = d.voltages.first().copied().unwrap_or(0) as u32;
            let current_ma = d.current_battery as i32;
            let remaining  = d.battery_remaining.max(0) as u32;
            let payload = encode_battery_payload(voltage_mv, current_ma, remaining);
            (StreamType::Telemetry, 0.0, 0.0, 0.0, payload)
        },

        MavMessage::HEARTBEAT(d) => {
            let payload = encode_heartbeat_payload(d.base_mode.bits(), d.system_status as u32);
            (StreamType::Event, 0.0, 0.0, 0.0, payload)
        },

        MavMessage::GPS_RAW_INT(d) => {
            let lat = d.lat as f64 / 1e7;
            let lon = d.lon as f64 / 1e7;
            let alt = d.alt as f32 / 1000.0;
            let payload = encode_gps_payload(lat, lon, alt, d.fix_type as u32, d.satellites_visible as u32, d.eph as u32);
            (StreamType::Telemetry, lat, lon, alt, payload)
        },

        MavMessage::SYS_STATUS(d) => {
            let payload = encode_sys_status_payload(d.voltage_battery, d.current_battery, d.battery_remaining);
            (StreamType::Telemetry, 0.0, 0.0, 0.0, payload)
        },

        // Skip messages we don't translate (RC_CHANNELS, etc.)
        _ => return None,
    };

    Some(DataEnvelope {
        vehicle_id:   vehicle_id.to_string(),
        timestamp_ns,
        stream_type,
        payload: Bytes::from(payload),
        lat,
        lon,
        alt,
        seq,
        // fleet_id, org_id, stream_name left empty — filled by relay / local API
        ..Default::default()
    })
}

// ──────────────────────────────────────────────────────────────────────────────
// Payload encoders — each returns serialized TelemetryFrame proto bytes.
// These use manual encoding (prost Message::encode) for zero-allocation hot path.
// ──────────────────────────────────────────────────────────────────────────────

fn encode_position_payload(lat: f64, lon: f64, alt: f32, rel_alt_mm: i32, vx: i16, vy: i16, vz: i16, hdg: u16) -> Vec<u8> {
    // Encode as a minimal TelemetryFrame with velocity + heading fields set.
    // Full position is in the envelope lat/lon/alt fields (zero-overhead routing).
    // Here we encode the fields not in the envelope: relative altitude + velocity.
    use prost::Message;
    // We use a simple struct instead of generated code to avoid a proto dependency cycle.
    // In the full build, prost-generated TelemetryFrame struct from proto/telemetry/telemetry.proto
    // would be used here after `build.rs` runs.
    // Placeholder: encode as raw varint fields matching the telemetry.proto field layout.
    let mut buf = Vec::with_capacity(32);
    // vx_cms = field 4 (sint32), vy_cms = field 5, vz_cms = field 6
    encode_sint32_field(&mut buf, 4, vx as i32 * 100 / 100); // cm/s
    encode_sint32_field(&mut buf, 5, vy as i32 * 100 / 100);
    encode_sint32_field(&mut buf, 6, vz as i32 * 100 / 100);
    encode_sint32_field(&mut buf, 16, rel_alt_mm);             // alt_rel_mm field 16
    encode_uint32_field(&mut buf, 15, hdg as u32);             // heading field 15
    buf
}

fn encode_attitude_payload(roll: f32, pitch: f32, yaw: f32, _rs: f32, _ps: f32, _ys: f32) -> Vec<u8> {
    let mut buf = Vec::with_capacity(16);
    // Fields 1-3: roll_mdeg, pitch_mdeg, yaw_mdeg (sint32, millidegrees)
    encode_sint32_field(&mut buf, 1, (roll.to_degrees() * 1000.0) as i32);
    encode_sint32_field(&mut buf, 2, (pitch.to_degrees() * 1000.0) as i32);
    encode_sint32_field(&mut buf, 3, (yaw.to_degrees() * 1000.0) as i32);
    buf
}

fn encode_battery_payload(voltage_mv: u32, current_ma: i32, remaining: u32) -> Vec<u8> {
    let mut buf = Vec::with_capacity(16);
    encode_uint32_field(&mut buf, 7, voltage_mv);
    encode_sint32_field(&mut buf, 8, current_ma);
    encode_uint32_field(&mut buf, 9, remaining);
    buf
}

fn encode_heartbeat_payload(base_mode: u8, status: u32) -> Vec<u8> {
    let armed = (base_mode & 0x80) != 0; // MAV_MODE_FLAG_SAFETY_ARMED
    let mut buf = Vec::with_capacity(8);
    encode_bool_field(&mut buf, 11, armed);  // armed = field 11
    encode_uint32_field(&mut buf, 10, status); // mode = field 10
    buf
}

fn encode_gps_payload(lat: f64, lon: f64, alt: f32, fix: u32, sats: u32, eph: u32) -> Vec<u8> {
    let mut buf = Vec::with_capacity(16);
    encode_uint32_field(&mut buf, 12, fix);
    encode_uint32_field(&mut buf, 13, sats);
    encode_uint32_field(&mut buf, 14, eph);
    buf
}

fn encode_sys_status_payload(voltage_mv: u16, current_ma: i16, remaining: i8) -> Vec<u8> {
    let mut buf = Vec::with_capacity(16);
    encode_uint32_field(&mut buf, 7, voltage_mv as u32);
    encode_sint32_field(&mut buf, 8, current_ma as i32);
    encode_uint32_field(&mut buf, 9, remaining.max(0) as u32);
    buf
}

// ──────────────────────────────────────────────────────────────────────────────
// Protobuf wire format helpers (manual encoding, no allocation per field)
// ──────────────────────────────────────────────────────────────────────────────

/// Encode a protobuf varint (unsigned).
fn encode_varint(buf: &mut Vec<u8>, mut v: u64) {
    loop {
        let b = (v & 0x7F) as u8;
        v >>= 7;
        if v == 0 { buf.push(b); break; }
        buf.push(b | 0x80);
    }
}

/// Encode field tag: (field_number << 3) | wire_type
fn encode_tag(buf: &mut Vec<u8>, field: u32, wire_type: u32) {
    encode_varint(buf, ((field << 3) | wire_type) as u64);
}

fn encode_uint32_field(buf: &mut Vec<u8>, field: u32, v: u32) {
    encode_tag(buf, field, 0); // wire type 0 = varint
    encode_varint(buf, v as u64);
}

fn encode_sint32_field(buf: &mut Vec<u8>, field: u32, v: i32) {
    // Zigzag encoding for sint32: (n << 1) ^ (n >> 31)
    let z = ((v << 1) ^ (v >> 31)) as u32;
    encode_tag(buf, field, 0);
    encode_varint(buf, z as u64);
}

fn encode_bool_field(buf: &mut Vec<u8>, field: u32, v: bool) {
    encode_tag(buf, field, 0);
    buf.push(if v { 1 } else { 0 });
}

// ──────────────────────────────────────────────────────────────────────────────
// Command payload decoder
// ──────────────────────────────────────────────────────────────────────────────

/// Decoded MavlinkCommand proto (matches proto/command/command_payloads.proto).
struct MavlinkCommandDecoded {
    command: u32,
    param1: f32, param2: f32, param3: f32, param4: f32,
    param5: f32, param6: f32, param7: f32,
}

fn decode_mavlink_command(payload: &[u8]) -> Result<MavlinkCommandDecoded> {
    // Manual proto decode of MavlinkCommand message.
    // Fields: command=1 (uint32), param1-7 = 2-8 (float/fixed32).
    let mut cmd = MavlinkCommandDecoded {
        command: 0,
        param1: 0.0, param2: 0.0, param3: 0.0, param4: 0.0,
        param5: 0.0, param6: 0.0, param7: 0.0,
    };

    let mut pos = 0usize;
    while pos < payload.len() {
        let tag = read_varint(payload, &mut pos)?;
        let field = (tag >> 3) as u32;
        let wire  = tag & 0x7;

        match (field, wire) {
            (1, 0) => cmd.command = read_varint(payload, &mut pos)? as u32,
            (2, 5) => cmd.param1 = read_f32(payload, &mut pos)?,
            (3, 5) => cmd.param2 = read_f32(payload, &mut pos)?,
            (4, 5) => cmd.param3 = read_f32(payload, &mut pos)?,
            (5, 5) => cmd.param4 = read_f32(payload, &mut pos)?,
            (6, 5) => cmd.param5 = read_f32(payload, &mut pos)?,
            (7, 5) => cmd.param6 = read_f32(payload, &mut pos)?,
            (8, 5) => cmd.param7 = read_f32(payload, &mut pos)?,
            (_, 0) => { read_varint(payload, &mut pos)?; }, // skip unknown varints
            (_, 5) => { pos += 4; },                        // skip unknown fixed32
            (_, 1) => { pos += 8; },                        // skip unknown fixed64
            _ => break,
        }
    }
    Ok(cmd)
}

fn read_varint(buf: &[u8], pos: &mut usize) -> Result<u64> {
    let mut result = 0u64;
    let mut shift = 0;
    loop {
        anyhow::ensure!(*pos < buf.len(), "varint truncated");
        let b = buf[*pos];
        *pos += 1;
        result |= ((b & 0x7F) as u64) << shift;
        if b & 0x80 == 0 { return Ok(result); }
        shift += 7;
        anyhow::ensure!(shift < 64, "varint overflow");
    }
}

fn read_f32(buf: &[u8], pos: &mut usize) -> Result<f32> {
    anyhow::ensure!(*pos + 4 <= buf.len(), "f32 truncated");
    let bytes = [buf[*pos], buf[*pos+1], buf[*pos+2], buf[*pos+3]];
    *pos += 4;
    Ok(f32::from_le_bytes(bytes))
}
