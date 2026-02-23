//! SRT Relay — accepts SRT video streams from vehicles, buffers to NVMe,
//! and forwards to LiveKit Ingress over the private backbone.
//!
//! Architecture per stream:
//!   Vehicle camera (H.265 NVENC, 50ms)
//!     → SRT sender (UDP, AES-256, ARQ retransmission)
//!     → SRT relay accept (this process)
//!     → [NVMe ring buffer, 60s capacity]
//!     → SRT forward to LiveKit Ingress (private backbone: ~10ms)
//!
//! Each vehicle gets one SRT stream. Streams are identified by the SRT stream ID
//! field (set to vehicle_id by the edge agent). Separate SRT connections per
//! vehicle ensure independent congestion windows — one vehicle's bad link
//! doesn't affect other streams.
//!
//! The NVMe ring buffer enables:
//! - Instant replay: operator requests "last 30 seconds" → served from local NVMe
//! - Reconnection resilience: if LiveKit forward fails, buffer accumulates and
//!   replays when the forward connection is restored
//!
//! SRT parameters tuned for high-latency satellite / LTE links:
//! - latency: 500ms SRT latency buffer (absorbs jitter, enables ARQ)
//! - maxbw: -1 (unlimited bandwidth, let congestion control decide)
//! - pbkeylen: 32 (AES-256 encryption)
//! - tlpktdrop: true (drop old packets on overflow rather than blocking)

use std::collections::HashMap;
use std::net::SocketAddr;
use std::path::PathBuf;
use std::sync::Arc;
use std::time::Duration;

use anyhow::Context;
use bytes::Bytes;
use serde::Deserialize;
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tracing::{error, info, warn};

// ──────────────────────────────────────────────────────────────────────────────
// Configuration
// ──────────────────────────────────────────────────────────────────────────────

#[derive(Debug, Deserialize)]
struct Config {
    region_id:    String,
    /// Address to listen for incoming SRT streams from vehicles
    srt_listen:   String,
    /// LiveKit Ingress SRT URL, e.g. "srt://livekit-ingress.cloud:4000"
    livekit_srt:  String,
    /// LiveKit Ingress API token (JWT signed with LiveKit API secret)
    livekit_token: String,
    /// NVMe path for the ring buffer (file per vehicle)
    ring_buffer_dir: PathBuf,
    /// Ring buffer size per vehicle stream in bytes (default 2 GB for ~60s of H.265)
    #[serde(default = "default_ring_bytes")]
    ring_buffer_bytes: u64,
    /// SRT latency in milliseconds (must be > 4× one-way RTT to vehicle)
    #[serde(default = "default_srt_latency")]
    srt_latency_ms: i32,
    /// Prometheus metrics endpoint
    metrics_addr:  String,
}

fn default_ring_bytes()   -> u64 { 2 * 1024 * 1024 * 1024 } // 2 GB
fn default_srt_latency() -> i32 { 500 } // 500ms, covers LTE + Starlink jitter

fn load_config() -> anyhow::Result<Config> {
    let path = std::env::var("SYSTEMSCALE_CONFIG")
        .unwrap_or_else(|_| "/etc/systemscale/srt-relay.yaml".to_string());
    let text = std::fs::read_to_string(&path)
        .with_context(|| format!("read config {path}"))?;
    serde_yaml::from_str(&text).context("parse config yaml")
}

// ──────────────────────────────────────────────────────────────────────────────
// NVMe ring buffer for video (file-backed, per-vehicle)
// ──────────────────────────────────────────────────────────────────────────────

/// A simple append ring buffer on NVMe for a single video stream.
/// Stores raw SRT data chunks with a sequence counter.
///
/// Format (each record):
///   [seq: u64 LE][len: u32 LE][timestamp_ms: u64 LE][data: len bytes]
///
/// The file is pre-allocated to `capacity_bytes`. The write pointer wraps.
/// A separate header page stores head/tail sequence numbers.
struct VideoRingBuffer {
    file:     std::fs::File,
    capacity: u64,
    write_pos: u64,
    seq:       u64,
}

impl VideoRingBuffer {
    fn open(path: PathBuf, capacity: u64) -> anyhow::Result<Self> {
        use std::fs::OpenOptions;
        let file = OpenOptions::new()
            .read(true)
            .write(true)
            .create(true)
            .open(&path)
            .with_context(|| format!("open ring buffer {}", path.display()))?;
        file.set_len(capacity)
            .context("pre-allocate ring buffer")?;
        Ok(Self { file, capacity, write_pos: 0, seq: 0 })
    }

    /// Append a chunk of SRT data to the ring buffer.
    /// Silently wraps on overflow (oldest data is overwritten).
    fn append(&mut self, data: &[u8]) -> anyhow::Result<()> {
        use std::io::{Seek, SeekFrom, Write};

        let ts = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap_or_default()
            .as_millis() as u64;

        let header_len = 8 + 4 + 8; // seq + len + timestamp
        let total = header_len + data.len() as u64;

        // Wrap if needed
        if self.write_pos + total > self.capacity {
            self.write_pos = 0;
        }

        self.file.seek(SeekFrom::Start(self.write_pos))?;
        self.file.write_all(&self.seq.to_le_bytes())?;
        self.file.write_all(&(data.len() as u32).to_le_bytes())?;
        self.file.write_all(&ts.to_le_bytes())?;
        self.file.write_all(data)?;

        self.write_pos += total;
        self.seq += 1;
        Ok(())
    }
}

// ──────────────────────────────────────────────────────────────────────────────
// SRT stream handler — one per vehicle
// ──────────────────────────────────────────────────────────────────────────────

/// Handles a single vehicle's SRT stream:
/// 1. Reads SRT data from the incoming connection
/// 2. Appends to NVMe ring buffer
/// 3. Forwards to LiveKit Ingress via SRT
///
/// Uses srt-rs for the SRT socket layer. Both the incoming and outgoing
/// connections use the SRT protocol with appropriate parameters.
async fn handle_srt_stream(
    vehicle_id:  String,
    ring_dir:    PathBuf,
    ring_bytes:  u64,
    livekit_url: String,
    livekit_tok: String,
    region_id:   String,
) -> anyhow::Result<()> {
    info!(vehicle_id = %vehicle_id, "SRT stream handler started");

    let ring_path = ring_dir.join(format!("{vehicle_id}.ring"));
    let mut ring = VideoRingBuffer::open(ring_path, ring_bytes)
        .context("open video ring buffer")?;

    // In production: use srt-rs to open the outgoing SRT connection to LiveKit.
    // The srt-rs API mirrors libsrt's socket API.
    // Here we simulate the forward loop with a placeholder that logs throughput.
    //
    // Real implementation:
    //   let mut lk_conn = srt::Builder::new()
    //       .latency(Duration::from_millis(120))
    //       .passphrase(&livekit_tok)
    //       .connect(&livekit_url)
    //       .await?;
    //
    // Then pipe: incoming_srt_data → ring.append() + lk_conn.write_all()

    // Placeholder: report that the handler is active
    metrics::gauge!(
        "srt_relay.active_streams",
        "region" => region_id.clone()
    ).increment(1.0);

    // The actual SRT accept + forward loop would be:
    //   loop {
    //       let chunk = incoming.read_chunk(65536).await?;
    //       ring.append(&chunk)?;
    //       lk_conn.write_all(&chunk).await?;
    //       metrics::counter!("srt_relay.bytes_forwarded", ...).increment(chunk.len() as u64);
    //   }

    // Simulate stream activity for metrics
    tokio::time::sleep(Duration::from_secs(u64::MAX)).await;

    metrics::gauge!(
        "srt_relay.active_streams",
        "region" => region_id.clone()
    ).decrement(1.0);

    Ok(())
}

// ──────────────────────────────────────────────────────────────────────────────
// SRT accept loop — listens for incoming vehicle connections
// ──────────────────────────────────────────────────────────────────────────────

/// Listens for incoming SRT connections from vehicles.
/// Each connection's Stream ID header identifies the vehicle (set by edge agent).
/// Spawns a handler task per vehicle stream.
async fn run_srt_accept_loop(cfg: Arc<Config>) -> anyhow::Result<()> {
    info!(
        addr = %cfg.srt_listen,
        "SRT relay listening for vehicle streams"
    );

    // In production, use srt-rs SrtListener:
    //   let listener = srt::Builder::new()
    //       .latency(Duration::from_millis(cfg.srt_latency_ms as u64))
    //       .listen(&cfg.srt_listen, 128)
    //       .await
    //       .context("SRT listen")?;
    //
    //   loop {
    //       let (conn, peer_addr) = listener.accept().await?;
    //       let vehicle_id = conn.stream_id()
    //           .ok_or_else(|| anyhow::anyhow!("SRT stream ID missing — edge agent must set it"))?
    //           .to_string();
    //       info!(vehicle_id = %vehicle_id, peer = ?peer_addr, "SRT stream accepted");
    //
    //       let cfg = cfg.clone();
    //       tokio::spawn(async move {
    //           if let Err(e) = handle_srt_stream(
    //               vehicle_id, cfg.ring_buffer_dir.clone(),
    //               cfg.ring_buffer_bytes, cfg.livekit_srt.clone(),
    //               cfg.livekit_token.clone(), cfg.region_id.clone()
    //           ).await {
    //               error!(error = %e, "SRT stream handler error");
    //           }
    //       });
    //   }

    // Placeholder until srt-rs integration is wired
    info!("SRT accept loop running (srt-rs integration pending libsrt system dependency)");
    tokio::signal::ctrl_c().await?;
    Ok(())
}

// ──────────────────────────────────────────────────────────────────────────────
// Main
// ──────────────────────────────────────────────────────────────────────────────

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    tracing_subscriber::fmt()
        .with_env_filter(
            tracing_subscriber::EnvFilter::from_env("RUST_LOG")
                .add_directive("srt_relay=info".parse().unwrap())
        )
        .json()
        .init();

    let cfg = Arc::new(load_config()?);
    info!(region = %cfg.region_id, "Starting SRT relay");

    // Prometheus metrics
    let builder = metrics_exporter_prometheus::PrometheusBuilder::new();
    builder
        .with_http_listener(cfg.metrics_addr.parse::<std::net::SocketAddr>()?)
        .install()
        .context("install metrics exporter")?;

    // Ensure ring buffer directory exists
    std::fs::create_dir_all(&cfg.ring_buffer_dir)
        .context("create ring buffer directory")?;

    run_srt_accept_loop(cfg).await
}
