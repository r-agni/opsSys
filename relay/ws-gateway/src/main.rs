//! WebSocket gateway — co-located on relay nodes.
//!
//! Operator browsers connect here (via Anycast IP → nearest relay region).
//! On subscription, the gateway subscribes to NATS subjects for the requested
//! vehicle IDs. NATS leaf node replication ensures this receives data from
//! vehicles on other regions transparently.
//!
//! Key design:
//! - One WebSocket connection per operator session
//! - One NATS subscription per vehicle_id the operator is watching
//! - Telemetry/event frames: forwarded as binary WebSocket frames (raw protobuf)
//! - Alert/assistance frames: decoded and forwarded as JSON text WebSocket frames
//! - Rate limit per vehicle: configurable max_hz (default 50Hz, alerts exempt)

use std::net::SocketAddr;
use std::sync::Arc;
use std::time::Duration;

use anyhow::Result;
use async_nats::Client as NatsClient;
use bytes::Bytes;
use dashmap::DashMap;
use futures::{SinkExt, StreamExt};
use tokio::net::{TcpListener, TcpStream};
use tokio::sync::mpsc;
use base64::Engine as _;
use tokio_tungstenite::{accept_async, tungstenite::Message};
use tracing::{debug, error, info, warn};

// ──────────────────────────────────────────────────────────────────────────────
// Configuration
// ──────────────────────────────────────────────────────────────────────────────

#[derive(Debug, serde::Deserialize)]
struct WsGatewayConfig {
    #[serde(default = "default_listen")]
    listen_addr: SocketAddr,
    #[serde(default = "default_nats")]
    nats_url: String,
    region_id: String,
    /// Maximum telemetry rate pushed to each operator per vehicle (Hz)
    #[serde(default = "default_max_hz")]
    max_hz: u32,
    #[serde(default = "default_metrics_addr")]
    metrics_addr: SocketAddr,
}

fn default_listen()       -> SocketAddr { "0.0.0.0:8080".parse().unwrap() }
fn default_nats()         -> String     { "nats://127.0.0.1:4222".to_string() }
fn default_max_hz()       -> u32        { 50 }
fn default_metrics_addr() -> SocketAddr { "0.0.0.0:9091".parse().unwrap() }

// ──────────────────────────────────────────────────────────────────────────────
// WebSocket frame types and format
// Raw binary frames carry protobuf (for SDK deserialization).
// Text frames carry JSON (for alerts and assistance — human-in-the-loop events).
// ──────────────────────────────────────────────────────────────────────────────

enum WsFrame {
    Binary(Bytes),
    Text(String),
}

/// Requested output format, captured from the HTTP upgrade URL (?format=json).
#[derive(Clone, Copy, PartialEq)]
enum FrameFormat {
    Binary, // default — raw protobuf, same as before
    Json,   // decode DataEnvelope → JSON text frame
}

// ──────────────────────────────────────────────────────────────────────────────
// Subscription request (sent by operator on WebSocket connect or on demand)
// ──────────────────────────────────────────────────────────────────────────────

/// JSON message sent by the operator browser to subscribe to vehicles.
///
/// Regular telemetry example:
///   {"vehicle_ids": ["id1", "id2"], "streams": ["telemetry", "event"]}
///
/// Alert/assistance subscription (decoded to JSON, not binary proto):
///   {"vehicle_ids": ["id1", "id2"], "streams": ["alert", "assistance"]}
///
/// Mixed:
///   {"vehicle_ids": ["id1"], "streams": ["telemetry", "alert", "assistance"]}
#[derive(Debug, serde::Deserialize)]
struct SubscribeRequest {
    vehicle_ids: Vec<String>,
    #[serde(default = "default_streams")]
    streams: Vec<String>,
    /// Optional output format. Set to "json" to receive telemetry as JSON text frames
    /// instead of raw protobuf binary. Alerts and assistance are always JSON.
    #[serde(default)]
    format: Option<String>,
}

fn default_streams() -> Vec<String> {
    vec!["telemetry".to_string(), "event".to_string()]
}

// ──────────────────────────────────────────────────────────────────────────────
// Alert/assistance event type constants (must match proto EventType enum)
// ──────────────────────────────────────────────────────────────────────────────

const EVENT_TYPE_ALERT:          i64 = 101;
const EVENT_TYPE_ASSISTANCE_REQ: i64 = 102;

// ──────────────────────────────────────────────────────────────────────────────
// Minimal protobuf decoder — no generated code, no prost dependency needed here.
// Only decodes the specific fields the ws-gateway needs for alert routing.
// ──────────────────────────────────────────────────────────────────────────────

/// Read a protobuf varint from `data`. Returns (value, bytes_consumed).
/// Returns (0, 0) on truncation.
fn read_varint(data: &[u8]) -> (u64, usize) {
    let mut result: u64 = 0;
    for (i, &b) in data.iter().enumerate() {
        result |= ((b & 0x7F) as u64) << (7 * i as u64);
        if b & 0x80 == 0 {
            return (result, i + 1);
        }
        if i >= 9 {
            return (0, 0);
        }
    }
    (0, 0)
}

/// Decode just the vehicle_id (field 1) and payload bytes (field 4) from a
/// DataEnvelope proto message. Returns None on parse error.
fn decode_envelope_routing(data: &[u8]) -> Option<(String, Vec<u8>)> {
    let mut vehicle_id = String::new();
    let mut payload    = Vec::new();
    let mut pos = 0;

    while pos < data.len() {
        let (tag, n) = read_varint(&data[pos..]);
        if n == 0 { break; }
        pos += n;
        let field    = (tag >> 3) as u32;
        let wiretype = (tag & 0x7) as u8;

        match wiretype {
            0 => {
                let (_, n2) = read_varint(&data[pos..]);
                if n2 == 0 { return None; }
                pos += n2;
            }
            1 => {
                if pos + 8 > data.len() { return None; }
                pos += 8;
            }
            2 => {
                let (len, n2) = read_varint(&data[pos..]);
                if n2 == 0 { return None; }
                pos += n2;
                let end = pos + len as usize;
                if end > data.len() { return None; }
                match field {
                    1 => vehicle_id = String::from_utf8_lossy(&data[pos..end]).into_owned(),
                    4 => payload    = data[pos..end].to_vec(),
                    _ => {}
                }
                pos = end;
            }
            5 => {
                if pos + 4 > data.len() { return None; }
                pos += 4;
            }
            _ => return None,
        }
    }
    Some((vehicle_id, payload))
}

/// Decode an EventFrame proto message.
/// Returns (event_type, description, metadata_bytes).
fn decode_event_frame(data: &[u8]) -> (i64, String, Vec<u8>) {
    let mut event_type:  i64     = 0;
    let mut description: String  = String::new();
    let mut metadata:    Vec<u8> = Vec::new();
    let mut pos = 0;

    while pos < data.len() {
        let (tag, n) = read_varint(&data[pos..]);
        if n == 0 { break; }
        pos += n;
        let field    = (tag >> 3) as u32;
        let wiretype = (tag & 0x7) as u8;

        match wiretype {
            0 => {
                let (v, n2) = read_varint(&data[pos..]);
                if n2 == 0 { break; }
                pos += n2;
                if field == 1 { event_type = v as i64; }
            }
            2 => {
                let (len, n2) = read_varint(&data[pos..]);
                if n2 == 0 { break; }
                pos += n2;
                let end = pos + len as usize;
                if end > data.len() { break; }
                match field {
                    2 => description = String::from_utf8_lossy(&data[pos..end]).into_owned(),
                    3 => metadata    = data[pos..end].to_vec(),
                    _ => {}
                }
                pos = end;
            }
            1 => { if pos + 8 > data.len() { break; } pos += 8; }
            5 => { if pos + 4 > data.len() { break; } pos += 4; }
            _ => break,
        }
    }
    (event_type, description, metadata)
}

/// Decode an AlertFrame proto message. Returns (level, message).
fn decode_alert_frame(data: &[u8]) -> (String, String) {
    let mut level:   String = "info".to_string();
    let mut message: String = String::new();
    let mut pos = 0;

    while pos < data.len() {
        let (tag, n) = read_varint(&data[pos..]);
        if n == 0 { break; }
        pos += n;
        let field    = (tag >> 3) as u32;
        let wiretype = (tag & 0x7) as u8;

        if wiretype == 2 {
            let (len, n2) = read_varint(&data[pos..]);
            if n2 == 0 { break; }
            pos += n2;
            let end = pos + len as usize;
            if end > data.len() { break; }
            match field {
                1 => level   = String::from_utf8_lossy(&data[pos..end]).into_owned(),
                2 => message = String::from_utf8_lossy(&data[pos..end]).into_owned(),
                _ => {}
            }
            pos = end;
        } else {
            match wiretype {
                0 => { let (_, n2) = read_varint(&data[pos..]); pos += n2.max(1); }
                1 => { pos += 8; }
                5 => { pos += 4; }
                _ => break,
            }
        }
    }
    (level, message)
}

/// Decode an AssistanceRequestFrame proto message. Returns (request_id, reason).
fn decode_assistance_frame(data: &[u8]) -> (String, String) {
    let mut request_id: String = String::new();
    let mut reason:     String = String::new();
    let mut pos = 0;

    while pos < data.len() {
        let (tag, n) = read_varint(&data[pos..]);
        if n == 0 { break; }
        pos += n;
        let field    = (tag >> 3) as u32;
        let wiretype = (tag & 0x7) as u8;

        if wiretype == 2 {
            let (len, n2) = read_varint(&data[pos..]);
            if n2 == 0 { break; }
            pos += n2;
            let end = pos + len as usize;
            if end > data.len() { break; }
            match field {
                1 => request_id = String::from_utf8_lossy(&data[pos..end]).into_owned(),
                2 => reason     = String::from_utf8_lossy(&data[pos..end]).into_owned(),
                _ => {}
            }
            pos = end;
        } else {
            match wiretype {
                0 => { let (_, n2) = read_varint(&data[pos..]); pos += n2.max(1); }
                1 => { pos += 8; }
                5 => { pos += 4; }
                _ => break,
            }
        }
    }
    (request_id, reason)
}

/// Decode a DataEnvelope proto binary frame into a JSON text frame.
///
/// Proto field numbers (from envelope.proto):
///   1: string     vehicle_id   (wire 2)
///   2: uint64     timestamp_ns (wire 0)
///   3: StreamType type         (wire 0) — ignored
///   4: bytes      payload      (wire 2)
///   5: double     lat          (wire 1)
///   6: double     lon          (wire 1)
///   7: float      alt          (wire 5)
///   8: uint32     seq          (wire 0)
///
/// Returns None on parse error.
fn envelope_to_json_frame(data: &Bytes) -> Option<String> {
    let mut vehicle_id:   String  = String::new();
    let mut timestamp_ns: u64     = 0;
    let mut seq_num:      u64     = 0;
    let mut payload:      Vec<u8> = Vec::new();
    let mut lat:          f64     = 0.0;
    let mut lon:          f64     = 0.0;
    let mut alt:          f64     = 0.0;
    let mut pos = 0;

    while pos < data.len() {
        let (tag, n) = read_varint(&data[pos..]);
        if n == 0 { break; }
        pos += n;
        let field    = (tag >> 3) as u32;
        let wiretype = (tag & 0x7) as u8;

        match wiretype {
            0 => {
                let (v, n2) = read_varint(&data[pos..]);
                if n2 == 0 { return None; }
                pos += n2;
                match field {
                    2 => timestamp_ns = v,
                    8 => seq_num      = v,
                    _ => {}
                }
            }
            1 => {
                if pos + 8 > data.len() { return None; }
                let bytes: [u8; 8] = data[pos..pos + 8].try_into().ok()?;
                let val = f64::from_le_bytes(bytes);
                match field {
                    5 => lat = val,
                    6 => lon = val,
                    _ => {}
                }
                pos += 8;
            }
            2 => {
                let (len, n2) = read_varint(&data[pos..]);
                if n2 == 0 { return None; }
                pos += n2;
                let end = pos + len as usize;
                if end > data.len() { return None; }
                match field {
                    1 => vehicle_id = String::from_utf8_lossy(&data[pos..end]).into_owned(),
                    4 => payload    = data[pos..end].to_vec(),
                    _ => {}
                }
                pos = end;
            }
            5 => {
                if pos + 4 > data.len() { return None; }
                let bytes: [u8; 4] = data[pos..pos + 4].try_into().ok()?;
                let val = f32::from_le_bytes(bytes) as f64;
                if field == 7 { alt = val; }
                pos += 4;
            }
            _ => return None,
        }
    }

    let payload_b64 = base64::engine::general_purpose::STANDARD.encode(&payload);
    let json = serde_json::json!({
        "type":        "telemetry",
        "vehicle_id":  vehicle_id,
        "ts":          timestamp_ns,
        "lat":         lat,
        "lon":         lon,
        "alt":         alt,
        "seq":         seq_num,
        "payload_b64": payload_b64,
    });
    Some(json.to_string())
}

// ──────────────────────────────────────────────────────────────────────────────
// Main
// ──────────────────────────────────────────────────────────────────────────────

#[tokio::main]
async fn main() -> Result<()> {
    let cfg_path = std::env::var("SYSTEMSCALE_WS_CONFIG")
        .unwrap_or_else(|_| "/etc/systemscale/ws-gateway.yaml".to_string());
    let cfg: WsGatewayConfig = serde_yaml::from_str(
        &std::fs::read_to_string(&cfg_path)?
    )?;

    tracing_subscriber::fmt().json()
        .with_env_filter(tracing_subscriber::EnvFilter::try_from_default_env()
            .unwrap_or_else(|_| "info".parse().unwrap()))
        .init();

    info!(region = %cfg.region_id, listen = %cfg.listen_addr, "WebSocket gateway starting");

    metrics_exporter_prometheus::PrometheusBuilder::new()
        .with_http_listener(cfg.metrics_addr)
        .install_recorder()?;

    let nats = async_nats::connect(&cfg.nats_url).await?;
    let listener = TcpListener::bind(cfg.listen_addr).await?;
    let cfg = Arc::new(cfg);

    loop {
        let (stream, peer_addr) = listener.accept().await?;
        let nats = nats.clone();
        let cfg  = Arc::clone(&cfg);

        tokio::spawn(async move {
            if let Err(e) = handle_operator_connection(stream, peer_addr, nats, cfg).await {
                debug!(peer = %peer_addr, error = %e, "Operator WebSocket session ended");
            }
        });
    }
}

// ──────────────────────────────────────────────────────────────────────────────
// Per-operator connection handler
// ──────────────────────────────────────────────────────────────────────────────

async fn handle_operator_connection(
    stream:    TcpStream,
    peer_addr: SocketAddr,
    nats:      NatsClient,
    cfg:       Arc<WsGatewayConfig>,
) -> Result<()> {
    let ws = accept_async(stream).await?;
    let (mut ws_tx, mut ws_rx) = ws.split();

    info!(peer = %peer_addr, "Operator WebSocket connected");
    metrics::counter!("ws_gateway.connections.accepted", "region" => cfg.region_id.clone()).increment(1);
    metrics::gauge!("ws_gateway.connections.active", "region" => cfg.region_id.clone()).increment(1.0);

    // Channel: NATS data → WebSocket sender (binary proto OR JSON text)
    let (frame_tx, mut frame_rx) = mpsc::channel::<WsFrame>(1024);

    // Track active NATS subscriptions so we can cancel them on unsubscribe
    let active_subs: Arc<DashMap<String, ()>> = Arc::new(DashMap::new());

    // Task A: Forward frames from NATS channel → WebSocket
    let ws_send_task = {
        let region = cfg.region_id.clone();
        tokio::spawn(async move {
            while let Some(frame) = frame_rx.recv().await {
                let msg = match frame {
                    WsFrame::Binary(b) => Message::Binary(b.into()),
                    WsFrame::Text(s)   => Message::Text(s),
                };
                if ws_tx.send(msg).await.is_err() {
                    break; // WebSocket closed
                }
                metrics::counter!("ws_gateway.frames.sent", "region" => region.clone()).increment(1);
            }
        })
    };

    // Task B: Read subscription requests from operator
    while let Some(msg_result) = ws_rx.next().await {
        let msg = match msg_result {
            Ok(m) => m,
            Err(e) => {
                debug!(peer = %peer_addr, error = %e, "WebSocket recv error");
                break;
            }
        };

        match msg {
            Message::Text(text) => {
                match serde_json::from_str::<SubscribeRequest>(&text) {
                    Ok(req) => {
                        let fmt = if req.format.as_deref() == Some("json") {
                            FrameFormat::Json
                        } else {
                            FrameFormat::Binary
                        };
                        subscribe_to_vehicles(
                            req, &nats, &frame_tx, &active_subs, &cfg, fmt
                        ).await;
                    }
                    Err(e) => {
                        warn!(peer = %peer_addr, error = %e, "Invalid subscribe request");
                    }
                }
            }
            Message::Close(_) => break,
            Message::Ping(_)  => {} // tungstenite handles Pong automatically
            _                 => {} // ignore other binary messages from client
        }
    }

    // Cleanup
    ws_send_task.abort();
    metrics::gauge!("ws_gateway.connections.active", "region" => cfg.region_id.clone()).decrement(1.0);
    info!(peer = %peer_addr, "Operator WebSocket disconnected");

    Ok(())
}

// ──────────────────────────────────────────────────────────────────────────────
// NATS subscription management
// ──────────────────────────────────────────────────────────────────────────────

async fn subscribe_to_vehicles(
    req:         SubscribeRequest,
    nats:        &NatsClient,
    frame_tx:    &mpsc::Sender<WsFrame>,
    active_subs: &Arc<DashMap<String, ()>>,
    cfg:         &WsGatewayConfig,
    format:      FrameFormat,
) {
    let min_interval = Duration::from_millis(1000 / cfg.max_hz.max(1) as u64);

    for vehicle_id in req.vehicle_ids {
        for stream_type in &req.streams {
            // "alert" and "assistance" both subscribe to the "event" NATS subject
            // but decode the proto and push JSON text frames instead of binary.
            let (nats_stream, decode_as_event) = match stream_type.as_str() {
                "alert"      => ("event", true),
                "assistance" => ("event", true),
                other        => (other,   false),
            };

            let subject = format!("telemetry.{}.{}", vehicle_id, nats_stream);

            if active_subs.contains_key(&subject) {
                continue; // already subscribed (alert+assistance share the same NATS subject)
            }
            active_subs.insert(subject.clone(), ());

            let frame_tx   = frame_tx.clone();
            let nats       = nats.clone();
            let subject_c  = subject.clone();
            let sub_key    = subject.clone();
            let active     = Arc::clone(active_subs);
            let interval   = min_interval;
            let vid        = vehicle_id.clone();
            let region     = cfg.region_id.clone();

            info!(subject = %subject, decode_events = decode_as_event, "Operator subscribed");

            tokio::spawn(async move {
                let mut subscriber = match nats.subscribe(subject_c.clone()).await {
                    Ok(s) => s,
                    Err(e) => {
                        error!(subject = %subject_c, error = %e, "NATS subscribe failed");
                        active.remove(&sub_key);
                        return;
                    }
                };

                let mut last_sent = std::time::Instant::now();

                while let Some(msg) = subscriber.next().await {
                    if !active.contains_key(&sub_key) { break; }

                    if decode_as_event {
                        // Alert/assistance: decode proto → push JSON text.
                        // No rate limiting — these are sparse, high-value signals.
                        if let Some(json) = route_event_frame(&msg.payload, &vid) {
                            let _ = frame_tx.try_send(WsFrame::Text(json));
                            metrics::counter!("ws_gateway.alerts.sent", "region" => region.clone()).increment(1);
                        }
                    } else {
                        // Regular telemetry: rate-limit, then forward as binary proto or JSON.
                        let now = std::time::Instant::now();
                        if now.duration_since(last_sent) < interval {
                            continue;
                        }
                        last_sent = now;
                        let ws_frame = if format == FrameFormat::Json {
                            envelope_to_json_frame(&msg.payload)
                                .map(WsFrame::Text)
                                .unwrap_or_else(|| WsFrame::Binary(msg.payload))
                        } else {
                            WsFrame::Binary(msg.payload)
                        };
                        let _ = frame_tx.try_send(ws_frame);
                    }
                }

                active.remove(&sub_key);
            });
        }
    }
}

// ──────────────────────────────────────────────────────────────────────────────
// Alert / assistance JSON frame builder
// ──────────────────────────────────────────────────────────────────────────────

/// Decode a NATS message payload (DataEnvelope proto bytes) and, if it contains
/// an alert or assistance request event, return a JSON string for the operator.
/// Returns None for all other event types (they're forwarded as binary elsewhere).
///
/// Supports two payload formats:
/// - Proto-encoded: EventFrame + AlertFrame/AssistanceRequestFrame proto bytes.
/// - JSON-encoded: payload is a JSON object with `_event_type` field (SDK path).
fn route_event_frame(data: &Bytes, vehicle_id: &str) -> Option<String> {
    let (_, payload) = decode_envelope_routing(data)?;

    // ── Try proto decode first ────────────────────────────────────────────────
    let (mut event_type, mut description, mut metadata) = decode_event_frame(&payload);

    // ── Fall back to JSON decode (SDK-originated events via local HTTP API) ───
    let json_val: Option<serde_json::Value> = if event_type == 0 {
        serde_json::from_slice(&payload).ok().and_then(|v: serde_json::Value| {
            if v.get("_event_type").is_some() {
                event_type = v["_event_type"].as_i64().unwrap_or(0);
                Some(v)
            } else {
                None
            }
        })
    } else {
        None
    };

    match event_type {
        EVENT_TYPE_ALERT => {
            let (level, message) = if let Some(ref j) = json_val {
                let l = j.get("level").and_then(|v| v.as_str()).unwrap_or("info").to_string();
                let m = j.get("message").and_then(|v| v.as_str())
                    .or_else(|| j.get("description").and_then(|v| v.as_str()))
                    .unwrap_or(&description).to_string();
                (l, m)
            } else if !metadata.is_empty() {
                decode_alert_frame(&metadata)
            } else {
                ("info".to_string(), std::mem::take(&mut description))
            };
            let json = serde_json::json!({
                "type":    "alert",
                "device":  vehicle_id,
                "level":   level,
                "message": message,
            });
            Some(json.to_string())
        }

        EVENT_TYPE_ASSISTANCE_REQ => {
            let (request_id, reason) = if let Some(ref j) = json_val {
                let rid = j.get("_request_id").and_then(|v| v.as_str()).unwrap_or("").to_string();
                let rsn = j.get("reason").and_then(|v| v.as_str())
                    .unwrap_or(&description).to_string();
                (rid, rsn)
            } else if !metadata.is_empty() {
                decode_assistance_frame(&metadata)
            } else {
                (String::new(), std::mem::take(&mut description))
            };
            let json = serde_json::json!({
                "type":       "assistance_request",
                "device":     vehicle_id,
                "request_id": request_id,
                "reason":     reason,
            });
            Some(json.to_string())
        }

        _ => None,
    }
}
