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

    let pipeline_str = cfg.pipeline()?;
    info!("GStreamer pipeline:\n  gst-launch-1.0 {}", pipeline_str);

    // ── NOT YET IMPLEMENTED ──────────────────────────────────────────────────
    //
    // To launch the GStreamer pipeline from Rust, add the `gstreamer` crate:
    //
    //   [dependencies]
    //   gstreamer = "0.23"
    //   gstreamer-app = "0.23"
    //
    // Then:
    //   gstreamer::init()?;
    //   let pipeline = gstreamer::parse::launch(&pipeline_str)?;
    //   let bus = pipeline.bus().unwrap();
    //   pipeline.set_state(gstreamer::State::Playing)?;
    //   for msg in bus.iter_timed(gstreamer::ClockTime::NONE) {
    //       use gstreamer::MessageView::*;
    //       match msg.view() {
    //           Eos(..)   => { info!("EOS"); break; }
    //           Error(e)  => { error!("{}", e.error()); break; }
    //           _         => {}
    //       }
    //   }
    //   pipeline.set_state(gstreamer::State::Null)?;
    //
    // The GStreamer crate requires GStreamer system libraries installed on the
    // vehicle (see module-level doc comment for apt install commands).
    // For Jetson: the nvv4l2h265enc plugin ships with JetPack; no extra install.
    //
    // ─────────────────────────────────────────────────────────────────────────
    //
    // In the meantime, you can run the pipeline directly with gst-launch-1.0:
    //
    //   gst-launch-1.0 <pipeline>
    //
    // Or spawn it from a shell script / systemd ExecStart. The process exits on
    // pipeline error; systemd Restart=always will relaunch it automatically.

    error!(
        "GStreamer integration not yet compiled in. \
         Run the pipeline manually with gst-launch-1.0, or add the \
         `gstreamer` crate and implement the pipeline loop above."
    );

    // Print the ready-to-run shell command so operators can start streaming
    // immediately without waiting for the Rust GStreamer binding.
    println!("\nTo start video streaming now, run:");
    println!("  gst-launch-1.0 {pipeline_str}");
    println!("\nTo run as a service, use the systemd unit from docs/hardware-integration.md.");

    Ok(())
}
