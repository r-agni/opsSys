//! Local HTTP API — the bridge between the W&B-style SDK and the edge agent pipeline.
//!
//! Listens on `localhost:7777`. No authentication — localhost OS permissions handle access.
//! This follows the same pattern as Datadog's DogStatsD and W&B's local mode: a thin SDK
//! calls a local daemon, the daemon handles buffering, batching, and network resilience.
//!
//! Endpoints:
//!   POST /v1/log                   — ingest a telemetry/sensor/event frame from the SDK
//!   GET  /v1/commands              — Server-Sent Events stream of incoming commands
//!   POST /v1/commands/{id}/ack     — acknowledge a command; routes ACK back to relay
//!   GET  /healthz                  — 200 if relay is connected, 503 otherwise

use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};

use adapter_trait::{Ack, AckStatus, CommandEnvelope, DataEnvelope, Priority, StreamType};
use axum::{
    Router,
    extract::{Path, State},
    http::StatusCode,
    response::{
        sse::{Event, KeepAlive, Sse},
        IntoResponse,
    },
    routing::{get, post},
    Json,
};
use bytes::Bytes;
use serde::{Deserialize, Serialize};
use tokio::sync::{broadcast, mpsc};
use tracing::{info, warn};

// ──────────────────────────────────────────────────────────────────────────────
// Shared state
// ──────────────────────────────────────────────────────────────────────────────

/// State shared across all HTTP handlers for the lifetime of the edge agent.
pub struct LocalApiState {
    /// Vehicle ID assigned to this edge agent (from config).
    pub vehicle_id: String,
    /// Relay sends commands here; each SSE subscriber gets a broadcast receiver.
    pub cmd_broadcast: broadcast::Sender<CommandEnvelope>,
    /// HTTP SDK ACKs land here; the QUIC transport reads from the matching Receiver
    /// and writes them back to the relay on QUIC stream 1.
    pub ack_tx: mpsc::Sender<Ack>,
    /// SDK log() frames land here; the sequencer reads from the matching Receiver
    /// and writes them into the ring buffer (same path as hardware adapter data).
    pub log_tx: mpsc::Sender<DataEnvelope>,
    /// True while the QUIC connection to the relay is established.
    pub relay_connected: Arc<AtomicBool>,
}

// ──────────────────────────────────────────────────────────────────────────────
// Request / response types
// ──────────────────────────────────────────────────────────────────────────────

/// Body for `POST /v1/log`.
#[derive(Deserialize)]
pub struct LogRequest {
    /// Arbitrary JSON object — the SDK data dict.
    pub data: serde_json::Value,
    /// One of "telemetry", "sensor", "event", "log". Defaults to "telemetry".
    #[serde(default = "default_stream")]
    pub stream: String,
    /// WGS84 latitude (0.0 if not location-aware).
    #[serde(default)]
    pub lat: f64,
    /// WGS84 longitude (0.0 if not location-aware).
    #[serde(default)]
    pub lon: f64,
    /// Altitude in metres (0.0 if not applicable).
    #[serde(default)]
    pub alt: f32,
    /// Project / fleet ID (maps to DataEnvelope.fleet_id).
    /// Set by SDK init(); relay re-validates against the JWT claims.
    #[serde(default)]
    pub project_id: Option<String>,
    /// Custom stream label within a StreamType (maps to DataEnvelope.stream_name).
    /// Example: "lidar_front" to distinguish multiple sensor streams.
    #[serde(default)]
    pub stream_name: Option<String>,
    /// Arbitrary string tags stored alongside the data (e.g. {"mission": "survey-1"}).
    /// Serialized to JSON and appended to the payload as a `_tags` key.
    #[serde(default)]
    pub tags: Option<serde_json::Map<String, serde_json::Value>>,
    /// Target actor for peer-to-peer routing (maps to DataEnvelope.receiver_id/receiver_type).
    /// Format: "actor_type:actor_id" (e.g. "operator:ground-control", "device:drone-002").
    /// Empty / absent = broadcast within project.
    #[serde(default)]
    pub to: Option<String>,
    /// Override receiver_type if not embedded in `to`.
    #[serde(default)]
    pub to_type: Option<String>,
    /// Override sender_type (defaults to "device").
    #[serde(default)]
    pub sender_type: Option<String>,
}

fn default_stream() -> String { "telemetry".to_string() }

/// Body for `POST /v1/commands/{id}/ack`.
#[derive(Deserialize)]
pub struct AckRequest {
    /// "ok" | "completed" | "accepted" | "rejected" | "failed"
    pub status: String,
    #[serde(default)]
    pub message: String,
}

/// SSE event payload for `GET /v1/commands`.
#[derive(Serialize)]
struct CommandEvent {
    id:           String,
    command_type: String,
    data:         serde_json::Value,
    priority:     String,
}

// ──────────────────────────────────────────────────────────────────────────────
// Server entry point
// ──────────────────────────────────────────────────────────────────────────────

/// Build the axum Router (useful for testing without binding a port).
pub fn router(state: Arc<LocalApiState>) -> Router {
    Router::new()
        .route("/v1/log",                  post(handle_log))
        .route("/v1/commands",             get(handle_commands_sse))
        .route("/v1/commands/:id/ack",     post(handle_ack))
        .route("/healthz",                 get(handle_healthz))
        .with_state(state)
}

/// Bind to `127.0.0.1:7777` and serve forever.
/// Returns an error only if the bind itself fails (port in use, permissions, etc.).
pub async fn run(state: Arc<LocalApiState>) -> anyhow::Result<()> {
    let app      = router(state);
    let listener = tokio::net::TcpListener::bind("127.0.0.1:7777").await?;
    info!("Local SDK API listening on http://127.0.0.1:7777");
    axum::serve(listener, app).await?;
    Ok(())
}

// ──────────────────────────────────────────────────────────────────────────────
// Handlers
// ──────────────────────────────────────────────────────────────────────────────

/// POST /v1/log
///
/// Convert JSON dict to a DataEnvelope and inject it into the ring buffer pipeline.
/// Returns 200 immediately — the SDK must never block waiting for relay delivery.
async fn handle_log(
    State(state): State<Arc<LocalApiState>>,
    Json(req):    Json<LogRequest>,
) -> StatusCode {
    let stream_type = match req.stream.as_str() {
        "telemetry" => StreamType::Telemetry,
        "sensor"    => StreamType::Sensor,
        "event"     => StreamType::Event,
        "log"       => StreamType::Log,
        other => {
            warn!(stream = other, "Unknown stream type in POST /v1/log — defaulting to telemetry");
            StreamType::Telemetry
        }
    };

    // If the caller supplied tags, merge them into the data payload under the "_tags" key.
    // This keeps tags queryable via QuestDB's payload_json column without a separate field.
    let data = if let Some(tags) = req.tags {
        let mut obj = match req.data {
            serde_json::Value::Object(m) => m,
            other => {
                let mut m = serde_json::Map::new();
                m.insert("_value".to_string(), other);
                m
            }
        };
        obj.insert("_tags".to_string(), serde_json::Value::Object(tags));
        serde_json::Value::Object(obj)
    } else {
        req.data
    };

    let payload = match serde_json::to_vec(&data) {
        Ok(v)  => Bytes::from(v),
        Err(e) => {
            warn!(error = %e, "Failed to serialize log data payload");
            return StatusCode::BAD_REQUEST;
        }
    };

    // Parse peer-to-peer routing from the `to` field ("type:id" format)
    let (receiver_type, receiver_id) = if let Some(ref to) = req.to {
        if let Some(colon) = to.find(':') {
            let rtype = req.to_type.clone().unwrap_or_else(|| to[..colon].to_string());
            let rid   = to[colon + 1..].to_string();
            (rtype, rid)
        } else {
            // No colon: treat entire string as receiver_id, use to_type or empty
            (req.to_type.clone().unwrap_or_default(), to.clone())
        }
    } else {
        (String::new(), String::new())
    };

    let envelope = DataEnvelope {
        vehicle_id:    state.vehicle_id.clone(),
        timestamp_ns:  adapter_trait::now_ns(),
        stream_type,
        payload,
        lat:           req.lat,
        lon:           req.lon,
        alt:           req.alt,
        seq:           0, // assigned by the sequencer task in main.rs
        fleet_id:      req.project_id.unwrap_or_default(),
        stream_name:   req.stream_name.unwrap_or_default(),
        org_id:        String::new(),   // filled by relay from mTLS cert
        sender_id:     state.vehicle_id.clone(), // sender = this device
        sender_type:   req.sender_type.unwrap_or_else(|| "device".to_string()),
        receiver_id,
        receiver_type,
    };

    match state.log_tx.try_send(envelope) {
        Ok(())  => StatusCode::OK,
        Err(_)  => {
            // Channel full means the ring buffer writer is behind — apply backpressure to SDK
            warn!("Local API log channel full — returning 503 to SDK for backpressure");
            StatusCode::SERVICE_UNAVAILABLE
        }
    }
}

/// GET /v1/commands
///
/// Server-Sent Events stream. Each event is a JSON-encoded CommandEvent.
/// SDK clients keep this connection open and react to events in a background thread.
/// axum's built-in SSE keep-alive sends a `: keep-alive` comment every 15 seconds.
async fn handle_commands_sse(
    State(state): State<Arc<LocalApiState>>,
) -> Sse<impl futures::Stream<Item = Result<Event, std::convert::Infallible>>> {
    let mut cmd_rx = state.cmd_broadcast.subscribe();

    let stream = async_stream::stream! {
        loop {
            match cmd_rx.recv().await {
                Ok(cmd) => {
                    // Try to parse payload as JSON (true for HTTP SDK log() payloads).
                    // Fall back to a hex string for raw binary (hardware protocol payloads).
                    let data = serde_json::from_slice::<serde_json::Value>(&cmd.payload)
                        .unwrap_or_else(|_| {
                            serde_json::Value::String(hex::encode(&cmd.payload))
                        });

                    let priority_str = match cmd.priority {
                        Priority::Normal    => "normal",
                        Priority::High      => "high",
                        Priority::Emergency => "emergency",
                    };

                    let event_data = CommandEvent {
                        id:           cmd.command_id.clone(),
                        command_type: "command".to_string(),
                        data,
                        priority:     priority_str.to_string(),
                    };

                    match serde_json::to_string(&event_data) {
                        Ok(json) => {
                            yield Ok(
                                Event::default()
                                    .id(cmd.command_id)
                                    .data(json)
                            );
                        }
                        Err(e) => {
                            warn!(error = %e, "Failed to serialize command SSE event");
                        }
                    }
                }

                Err(broadcast::error::RecvError::Lagged(n)) => {
                    // Subscriber couldn't keep up — missed n commands. Log and continue.
                    warn!(dropped = n, "SSE subscriber lagged — some commands were skipped");
                }

                Err(broadcast::error::RecvError::Closed) => {
                    // broadcast sender dropped (agent shutting down) — end stream
                    break;
                }
            }
        }
    };

    Sse::new(stream).keep_alive(KeepAlive::default())
}

/// POST /v1/commands/{id}/ack
///
/// Routes an ACK from the SDK back to the relay via QUIC stream 1.
/// The relay publishes it to NATS; the cloud command-api resolves the pending gRPC response.
async fn handle_ack(
    State(state):    State<Arc<LocalApiState>>,
    Path(command_id): Path<String>,
    Json(req):       Json<AckRequest>,
) -> StatusCode {
    let status = match req.status.as_str() {
        "ok" | "completed" => AckStatus::Completed,
        "accepted"         => AckStatus::Accepted,
        "rejected"         => AckStatus::Rejected,
        "failed"           => AckStatus::Failed,
        other => {
            warn!(status = other, command_id = %command_id, "Unknown ACK status — treating as failed");
            AckStatus::Failed
        }
    };

    let ack = Ack {
        command_id,
        status,
        message:    req.message,
        ack_time_ns: adapter_trait::now_ns(),
    };

    match state.ack_tx.try_send(ack) {
        Ok(())  => StatusCode::OK,
        Err(_)  => {
            warn!("ACK channel full — dropping SDK ACK");
            StatusCode::SERVICE_UNAVAILABLE
        }
    }
}

/// GET /healthz
///
/// Returns 200 if the QUIC connection to the relay is up, 503 if reconnecting.
/// The SDK polls this after startup to know when the pipeline is ready.
async fn handle_healthz(
    State(state): State<Arc<LocalApiState>>,
) -> impl IntoResponse {
    if state.relay_connected.load(Ordering::Relaxed) {
        (StatusCode::OK, "ok")
    } else {
        (StatusCode::SERVICE_UNAVAILABLE, "reconnecting")
    }
}
