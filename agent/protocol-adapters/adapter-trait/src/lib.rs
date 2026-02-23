//! The hardware abstraction layer for the edge agent.
//!
//! `VehicleDataSource` is the single trait that ALL protocol adapters implement.
//! The rest of the edge agent (ring buffer, QUIC transport) only depends on this
//! trait — it has zero knowledge of MAVLink, ROS2, or any specific protocol.
//!
//! To add support for a new vehicle protocol:
//!   1. Create a new crate under `protocol-adapters/`
//!   2. Implement `VehicleDataSource` for your type
//!   3. Register it in `agent-core/src/config.rs` under `AdapterType`
//!   4. Add a match arm in `agent-core/src/main.rs` to construct it from config

use std::pin::Pin;
use std::future::Future;

use anyhow::Result;
use bytes::Bytes;
use futures::Stream;

// ──────────────────────────────────────────────────────────────────────────────
// Core data types
// ──────────────────────────────────────────────────────────────────────────────

/// The universal data envelope — every message produced by any adapter is this type.
/// Corresponds 1:1 with proto/core/envelope.proto `DataEnvelope`.
///
/// This is the Rust-native representation; the QUIC transport serializes it to
/// protobuf bytes before sending. Adapters produce `DataEnvelope`s; the
/// transport layer serializes them. No adapter ever touches the wire format.
#[derive(Debug, Clone, Default)]
pub struct DataEnvelope {
    /// Globally unique vehicle identifier (UUID v4).
    pub vehicle_id: String,
    /// Nanosecond UTC epoch timestamp from the vehicle's clock.
    pub timestamp_ns: u64,
    /// Type of data in `payload`.
    pub stream_type: StreamType,
    /// Opaque payload bytes. Format is determined by `stream_type`.
    /// Adapters serialize their native data structures here.
    pub payload: Bytes,
    /// WGS84 latitude (0.0 if not applicable).
    pub lat: f64,
    /// WGS84 longitude (0.0 if not applicable).
    pub lon: f64,
    /// Altitude in meters (0.0 if not applicable).
    pub alt: f32,
    /// Monotonically increasing per-vehicle sequence number.
    /// The edge agent assigns this; adapters do not set it.
    pub seq: u32,
    /// Project / fleet identifier (proto field 16 `fleet_id`).
    /// Set by the local API from SDK `project_id`; relay validates against JWT claims.
    pub fleet_id: String,
    /// Organization identifier (proto field 17 `org_id`).
    /// Filled in by the relay from the vehicle's mTLS certificate claims; empty on the device.
    pub org_id: String,
    /// Optional custom stream label within a `StreamType` (proto field 18 `stream_name`).
    /// Example: "lidar_front" within StreamType::Sensor. Empty = no sub-label.
    pub stream_name: String,
    /// Sending actor ID (proto field 19 `sender_id`). Set by the SDK; empty for standard telemetry.
    pub sender_id: String,
    /// Sending actor type (proto field 20 `sender_type`): "device" | "operator" | "service" | "human".
    pub sender_type: String,
    /// Target actor ID (proto field 21 `receiver_id`). Empty = broadcast within project.
    pub receiver_id: String,
    /// Target actor type (proto field 22 `receiver_type`): "device" | "operator" | "service" | "human".
    pub receiver_type: String,
}

/// Identifies the kind of data carried in `DataEnvelope.payload`.
/// Must match proto/core/common.proto `StreamType` enum values exactly.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash, Default)]
#[repr(i32)]
pub enum StreamType {
    #[default]
    Unspecified = 0,
    Telemetry   = 1,
    Event       = 2,
    Sensor      = 3,
    VideoMeta   = 4,
    Log         = 5,
    Audio       = 6,
}

/// A command sent from the cloud platform to this vehicle.
/// Corresponds to proto/core/envelope.proto `CommandEnvelope`.
#[derive(Debug, Clone)]
pub struct CommandEnvelope {
    /// Matches the vehicle this edge agent manages.
    pub vehicle_id: String,
    /// UUIDv7 — used for exactly-once deduplication in JetStream.
    pub command_id: String,
    /// Opaque command bytes. Adapter decodes and forwards to autopilot.
    pub payload: Bytes,
    /// Routing priority.
    pub priority: Priority,
    /// Milliseconds until this command expires. 0 = no expiry.
    pub ttl_ms: u32,
}

/// Command priority. Higher priority commands pre-empt queued normal commands.
#[derive(Debug, Clone, Copy, PartialEq, Eq, PartialOrd, Ord)]
pub enum Priority {
    Normal    = 1,
    High      = 2,
    Emergency = 3,
}

/// Acknowledgment returned to the platform after a command is handled.
#[derive(Debug, Clone)]
pub struct Ack {
    pub command_id: String,
    pub status: AckStatus,
    /// Human-readable detail on failure.
    pub message: String,
    /// Vehicle clock at moment of ACK (nanosecond UTC epoch).
    pub ack_time_ns: u64,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum AckStatus {
    Accepted,   // Command received and executing
    Completed,  // Execution finished successfully
    Rejected,   // Autopilot refused (invalid state, unsupported command, etc.)
    Failed,     // Execution failed
    Timeout,    // No response from autopilot within deadline
}

// ──────────────────────────────────────────────────────────────────────────────
// The trait — implement this to add a new vehicle protocol
// ──────────────────────────────────────────────────────────────────────────────

/// Hardware abstraction layer for a single vehicle connection.
///
/// Implementations are responsible for:
/// - Connecting to the vehicle hardware (serial port, UDP socket, Unix socket, etc.)
/// - Translating native protocol messages → `DataEnvelope`
/// - Translating `CommandEnvelope` → native protocol commands and sending them
/// - Reconnecting if the underlying transport drops
///
/// The edge agent calls `data_stream()` once at startup and drives it continuously.
/// `send_command()` is called whenever a command arrives from the cloud.
pub trait VehicleDataSource: Send + Sync + 'static {
    /// Returns a continuous stream of `DataEnvelope` from the vehicle.
    ///
    /// This stream MUST:
    /// - Never return `None` (it's infinite; reconnect internally on hardware loss)
    /// - Be cancel-safe (dropping the stream is always safe)
    /// - Set `DataEnvelope.stream_type` correctly for each message
    /// - Leave `DataEnvelope.seq` at 0 — the edge agent assigns sequence numbers
    ///
    /// The stream runs for the entire lifetime of the edge agent process.
    fn data_stream(&self) -> Pin<Box<dyn Stream<Item = DataEnvelope> + Send>>;

    /// Send a command to the vehicle hardware.
    ///
    /// Implementations MUST:
    /// - Translate `cmd.payload` from the canonical format to the native protocol
    /// - Forward to the autopilot and wait for the autopilot's native acknowledgment
    /// - Return `Ack` reflecting the autopilot's actual response (not a synthetic ACK)
    /// - Time out and return `AckStatus::Timeout` if autopilot doesn't respond in ~5s
    ///
    /// This method is called from a dedicated command task; it MUST NOT block the
    /// `data_stream()` pipeline. Use separate channel/connection for commands if needed.
    fn send_command(
        &self,
        cmd: CommandEnvelope,
    ) -> Pin<Box<dyn Future<Output = Result<Ack>> + Send>>;

    /// Human-readable adapter name, used in logs and metrics labels.
    /// Example: "mavlink-serial:/dev/ttyACM0@57600"
    fn adapter_id(&self) -> &str;
}

// ──────────────────────────────────────────────────────────────────────────────
// Adapter type enum (used in configuration)
// ──────────────────────────────────────────────────────────────────────────────

/// Which adapter implementation to instantiate, driven by the config file.
/// Add a new variant here when adding a new protocol adapter crate.
#[derive(Debug, Clone, serde::Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum AdapterType {
    /// MAVLink v2 via UDP (mavlink-router output, typically localhost:14550)
    MavlinkUdp,
    /// MAVLink v2 directly on a serial port
    MavlinkSerial,
    /// ROS2 DDS topic subscription (uses ros2-client crate)
    Ros2,
    /// Custom hardware writing DataEnvelope protos to a Unix domain socket
    CustomUnix,
}

// ──────────────────────────────────────────────────────────────────────────────
// Helper: current time as nanosecond UTC epoch
// ──────────────────────────────────────────────────────────────────────────────

/// Returns the current wall-clock time as nanoseconds since UNIX epoch.
/// Used by adapters when the native protocol doesn't provide a timestamp.
pub fn now_ns() -> u64 {
    use std::time::{SystemTime, UNIX_EPOCH};
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .expect("system clock before UNIX epoch")
        .as_nanos() as u64
}
