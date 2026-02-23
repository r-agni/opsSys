//! SRT video sender — runs as a **separate process** alongside `systemscale-agent`.
//!
//! This binary streams H.265-encoded video from a camera to the relay's SRT
//! listener. It is deliberately separate from the main edge agent so that video
//! congestion never affects the telemetry/command pipeline.
//!
//! # Intended GStreamer pipeline
//!
//! ## Jetson (hardware NVENC encode, ~50 ms glass-to-first-packet)
//! ```text
//! v4l2src device=/dev/video0 ! video/x-raw,width=1920,height=1080,framerate=30/1
//!   ! nvv4l2h265enc bitrate=4000000 iframeinterval=30 preset-level=1
//!   ! h265parse
//!   ! srtsink uri="srt://<relay_srt_addr>?mode=caller&latency=500&pbkeylen=32&passphrase=<key>"
//! ```
//!
//! ## x86 / Raspberry Pi (software encode, ~200 ms, CPU-intensive)
//! ```text
//! v4l2src device=/dev/video0 ! video/x-raw,width=1280,height=720,framerate=30/1
//!   ! x265enc tune=zerolatency bitrate=2000 speed-preset=superfast
//!   ! h265parse
//!   ! srtsink uri="srt://<relay_srt_addr>?mode=caller&latency=500&pbkeylen=32&passphrase=<key>"
//! ```
//!
//! ## USB camera with built-in H.264 output (lower latency alternative)
//! ```text
//! v4l2src device=/dev/video0 ! image/jpeg,width=1920,height=1080,framerate=30/1
//!   ! jpegdec ! x264enc tune=zerolatency bitrate=4000 speed-preset=superfast
//!   ! h264parse ! rtph264pay config-interval=1
//!   ! srtsink uri="srt://<relay_srt_addr>?mode=caller&latency=500"
//! ```
//!
//! # Dependencies (system packages, not Cargo)
//!
//! ```bash
//! # Jetson (JetPack 6.x)
//! sudo apt install gstreamer1.0-tools gstreamer1.0-plugins-bad \
//!                  gstreamer1.0-plugins-good libgstreamer1.0-dev
//! # The nvv4l2h265enc element is provided by the Jetson multimedia API.
//!
//! # Raspberry Pi / x86
//! sudo apt install gstreamer1.0-tools gstreamer1.0-plugins-bad \
//!                  gstreamer1.0-plugins-ugly gstreamer1.0-libav \
//!                  libgstreamer1.0-dev libgstreamer-plugins-base1.0-dev
//! ```
//!
//! # Configuration file
//!
//! `/etc/systemscale/video.yaml`
//! ```yaml
//! vehicle_id:     drone-001
//! camera_device:  /dev/video0
//! width:          1920
//! height:         1080
//! framerate:      30
//! bitrate_kbps:   4000
//!
//! # Relay SRT listener (UDP, same host:port for all vehicles — relay multiplexes by stream ID)
//! relay_srt_addr: 203.0.113.1:9000
//! srt_passphrase: ""   # AES-256 key, must match relay config; empty = no encryption (dev only)
//! srt_latency_ms: 500  # SRT end-to-end latency budget; increase for lossy links
//!
//! encoder: jetson    # "jetson" | "x265" | "x264"
//! ```
//!
//! # Why video is a separate process
//!
//! Video is megabytes/second. Mixing it with telemetry on the same QUIC connection
//! would starve telemetry during link congestion. SRT has its own congestion
//! control tuned for video (MPEG-TS over UDP with selective ARQ). This binary
//! holds a separate SRT connection; the main edge agent holds a QUIC connection.
//! Neither interferes with the other.

use anyhow::{bail, Context, Result};
use serde::Deserialize;
use tracing::{error, info};

#[cfg(feature = "gstreamer-runtime")]
use {std::time::Duration, tracing::warn};

// ─────────────────────────────────────────────────────────────────────────────
// Configuration
// ─────────────────────────────────────────────────────────────────────────────

#[derive(Debug, Deserialize)]
struct VideoConfig {
    vehicle_id:      String,
    #[serde(default = "default_camera")]
    camera_device:   String,
    #[serde(default = "default_width")]
    width:           u32,
    #[serde(default = "default_height")]
    height:          u32,
    #[serde(default = "default_framerate")]
    framerate:       u32,
    #[serde(default = "default_bitrate")]
    bitrate_kbps:    u32,

    relay_srt_addr:  String,
    #[serde(default)]
    srt_passphrase:  String,
    #[serde(default = "default_latency")]
    srt_latency_ms:  u32,

    #[serde(default = "default_encoder")]
    encoder:         String,
}

fn default_camera()    -> String { "/dev/video0".into() }
fn default_width()     -> u32    { 1920 }
fn default_height()    -> u32    { 1080 }
fn default_framerate() -> u32    { 30 }
fn default_bitrate()   -> u32    { 4000 }
fn default_latency()   -> u32    { 500 }
fn default_encoder()   -> String { "x265".into() }

impl VideoConfig {
    fn load() -> Result<Self> {
        let path = std::env::var("SYSTEMSCALE_VIDEO_CONFIG")
            .unwrap_or_else(|_| "/etc/systemscale/video.yaml".into());
        let text = std::fs::read_to_string(&path)
            .with_context(|| format!("Cannot read video config: {path}"))?;
        serde_yaml::from_str(&text)
            .with_context(|| format!("Invalid video config: {path}"))
    }

    /// Build the `gst-launch-1.0`-compatible pipeline string.
    fn pipeline(&self) -> Result<String> {
        let src = format!(
            "v4l2src device={dev} ! video/x-raw,width={w},height={h},framerate={fps}/1",
            dev = self.camera_device,
            w   = self.width,
            h   = self.height,
            fps = self.framerate,
        );

        let encode = match self.encoder.as_str() {
            "jetson" => format!(
                "nvv4l2h265enc bitrate={bps} iframeinterval={gop} preset-level=1 ! h265parse",
                bps = self.bitrate_kbps * 1000,
                gop = self.framerate,
            ),
            "x265" => format!(
                "x265enc tune=zerolatency bitrate={bps} speed-preset=superfast ! h265parse",
                bps = self.bitrate_kbps,
            ),
            "x264" => format!(
                "x264enc tune=zerolatency bitrate={bps} speed-preset=superfast ! h264parse",
                bps = self.bitrate_kbps,
            ),
            other => bail!("Unknown encoder: {other}. Use 'jetson', 'x265', or 'x264'."),
        };

        // Build SRT URI query string
        let mut uri = format!(
            "srt://{}?mode=caller&latency={}&streamid={}",
            self.relay_srt_addr, self.srt_latency_ms, self.vehicle_id,
        );
        if !self.srt_passphrase.is_empty() {
            uri.push_str("&pbkeylen=32&passphrase=");
            uri.push_str(&self.srt_passphrase);
        }

        Ok(format!("{src} ! {encode} ! srtsink uri=\"{uri}\""))
    }
}

// ─────────────────────────────────────────────────────────────────────────────
// Entry point
// ─────────────────────────────────────────────────────────────────────────────

#[tokio::main]
async fn main() -> Result<()> {
    tracing_subscriber::fmt()
        .with_env_filter(
            std::env::var("RUST_LOG").unwrap_or_else(|_| "info".into()),
        )
        .init();

    let cfg = VideoConfig::load()?;
    info!(vehicle_id = %cfg.vehicle_id, "SystemScale video sender starting");

    #[cfg(feature = "gstreamer-runtime")]
    {
        gstreamer::init().context("GStreamer initialization failed")?;
        info!("GStreamer runtime initialized");
        run_pipeline_loop(&cfg).await?;
    }

    #[cfg(not(feature = "gstreamer-runtime"))]
    {
        let pipeline_str = cfg.pipeline()?;
        error!(
            "Built without gstreamer-runtime feature. \
             Rebuild with: cargo build --features gstreamer-runtime"
        );
        println!("\nTo start video streaming manually, run:");
        println!("  gst-launch-1.0 {pipeline_str}");
    }

    Ok(())
}

// ─────────────────────────────────────────────────────────────────────────────
// GStreamer pipeline runner (compiled only with the gstreamer-runtime feature)
// ─────────────────────────────────────────────────────────────────────────────

#[cfg(feature = "gstreamer-runtime")]
enum PipelineOutcome {
    Eos,
    Error(String),
    Shutdown,
}

/// Runs the GStreamer pipeline in a loop with exponential backoff on errors.
/// Gracefully shuts down on SIGINT/SIGTERM (ctrl-c).
#[cfg(feature = "gstreamer-runtime")]
async fn run_pipeline_loop(cfg: &VideoConfig) -> Result<()> {
    let mut backoff = Duration::from_secs(1);
    const MAX_BACKOFF: Duration = Duration::from_secs(30);

    loop {
        let pipeline_str = cfg.pipeline()?;
        info!("Pipeline: gst-launch-1.0 {pipeline_str}");

        let pipeline = gstreamer::parse::launch(&pipeline_str)
            .context("Failed to parse GStreamer pipeline")?;

        let pipeline = pipeline
            .downcast::<gstreamer::Pipeline>()
            .map_err(|_| anyhow::anyhow!("Parsed element is not a Pipeline"))?;

        pipeline
            .set_state(gstreamer::State::Playing)
            .context("Failed to set pipeline to Playing")?;
        info!("Pipeline PLAYING");

        let bus = pipeline.bus().context("Pipeline has no bus")?;

        let outcome = {
            let bus_clone = bus.clone();
            let bus_handle =
                tokio::task::spawn_blocking(move || watch_bus(&bus_clone));

            tokio::select! {
                result = bus_handle => {
                    result.context("Bus watcher task panicked")?
                }
                _ = tokio::signal::ctrl_c() => {
                    info!("Received shutdown signal");
                    PipelineOutcome::Shutdown
                }
            }
        };

        let _ = pipeline.set_state(gstreamer::State::Null);

        match outcome {
            PipelineOutcome::Shutdown => {
                info!("Video sender stopped");
                return Ok(());
            }
            PipelineOutcome::Eos => {
                info!("End-of-stream — restarting pipeline");
                backoff = Duration::from_secs(1);
            }
            PipelineOutcome::Error(msg) => {
                error!("Pipeline error: {msg}");
                warn!("Restarting in {backoff:?}");
                tokio::time::sleep(backoff).await;
                backoff = (backoff * 2).min(MAX_BACKOFF);
            }
        }
    }
}

/// Blocks on the GStreamer bus, returning when EOS or Error is received.
/// Called inside `spawn_blocking` so it doesn't block the tokio runtime.
#[cfg(feature = "gstreamer-runtime")]
fn watch_bus(bus: &gstreamer::Bus) -> PipelineOutcome {
    use gstreamer::MessageView;

    loop {
        let msg = match bus.timed_pop(gstreamer::ClockTime::from_seconds(2)) {
            Some(msg) => msg,
            None => continue,
        };

        match msg.view() {
            MessageView::Eos(..) => {
                return PipelineOutcome::Eos;
            }
            MessageView::Error(e) => {
                let err = e.error().to_string();
                let debug = e
                    .debug()
                    .map(|d| d.to_string())
                    .unwrap_or_default();
                return if debug.is_empty() {
                    PipelineOutcome::Error(err)
                } else {
                    PipelineOutcome::Error(format!("{err} ({debug})"))
                };
            }
            MessageView::Warning(w) => {
                warn!(msg = %w.error(), "GStreamer warning");
            }
            _ => {}
        }
    }
}
