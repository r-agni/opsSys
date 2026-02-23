//! SystemScale Edge Agent
//!
//! Entry point. Responsibilities:
//! 1. Load configuration
//! 2. Instantiate the correct protocol adapter based on config
//! 3. Start the ring buffer (producer for adapter data, consumer for QUIC transport)
//! 4. Start the QUIC transport loop (outbound telemetry; inbound commands + ACKs)
//! 5. Start the command dispatch task (relay → adapter + SSE broadcast → ACK back)
//! 6. Start the local HTTP API (localhost:7777 — W&B-style SDK ingest + command SSE)
//! 7. Start the metrics exporter
//!
//! Data topology inside the agent:
//!
//!   [Adapter data_stream()] ──┐
//!                             ├─→ [Sequencer] → [Ring buffer] → [QUIC stream 0 → relay]
//!   [Local HTTP /v1/log]    ──┘
//!
//!   [relay QUIC stream 1] → [cmd_rx] → [Command dispatch] ─→ [adapter.send_command()]
//!                                                          ─→ [cmd_broadcast → SSE]
//!                                                          ─→ [ack_tx → QUIC stream 1]
//!   [Local HTTP /v1/commands/{id}/ack] ──────────────────────→ [ack_tx (clone)]

mod config;
mod local_api;

use std::sync::Arc;
use std::sync::atomic::{AtomicBool, AtomicU32, Ordering};

use adapter_trait::{AdapterType, DataEnvelope, VehicleDataSource};
use anyhow::Result;
use futures::StreamExt;
use tokio::sync::{broadcast, mpsc};
use tracing::{error, info};

#[tokio::main]
async fn main() -> Result<()> {
    // ── Configuration ──────────────────────────────────────────────────────────
    let cfg = config::AgentConfig::load()
        .map_err(|e| { eprintln!("FATAL: {}", e); e })?;

    tracing_subscriber::fmt()
        .with_env_filter(
            tracing_subscriber::EnvFilter::try_from_default_env()
                .unwrap_or_else(|_| cfg.log_level.parse()
                    .unwrap_or_else(|_| "info".parse().unwrap()))
        )
        .json()
        .init();

    info!(
        vehicle_id = %cfg.vehicle_id,
        version    = env!("CARGO_PKG_VERSION"),
        "SystemScale edge agent starting"
    );

    // ── Metrics exporter ────────────────────────────────────────────────────────
    metrics_exporter_prometheus::PrometheusBuilder::new()
        .with_http_listener(cfg.metrics.bind_addr.parse::<std::net::SocketAddr>()?)
        .install_recorder()
        .map_err(|e| anyhow::anyhow!("metrics exporter: {}", e))?;

    // ── Protocol adapter ────────────────────────────────────────────────────────
    let adapter: Arc<dyn VehicleDataSource> = match cfg.adapter {
        AdapterType::MavlinkUdp | AdapterType::MavlinkSerial => {
            let mcfg = cfg.mavlink
                .ok_or_else(|| anyhow::anyhow!(
                    "adapter = mavlink_* requires a [mavlink] section in the config file"
                ))?;
            Arc::new(mavlink_adapter::MavlinkAdapter::new(mcfg).await?)
        }
        #[cfg(unix)]
        AdapterType::CustomUnix => {
            let ucfg = cfg.custom_unix
                .ok_or_else(|| anyhow::anyhow!(
                    "adapter = custom_unix requires a [custom_unix] section in the config file"
                ))?;
            Arc::new(custom_unix_adapter::CustomUnixAdapter::new(ucfg).await?)
        }
        #[cfg(not(unix))]
        AdapterType::CustomUnix => {
            anyhow::bail!(
                "custom_unix adapter is only available on Unix systems."
            );
        }
        AdapterType::Ros2 => {
            anyhow::bail!(
                "ROS2 adapter not compiled in this build. \
                 Use adapter = mavlink_udp or adapter = custom_unix."
            );
        }
    };

    info!(adapter = %adapter.adapter_id(), "Protocol adapter initialized");

    // ── Ring buffer ─────────────────────────────────────────────────────────────
    let (mut ring_producer, ring_consumer) = ring_buffer::open(&cfg.ring_buffer)
        .map_err(|e| anyhow::anyhow!("ring buffer: {}", e))?;

    // ── Channels ────────────────────────────────────────────────────────────────

    // relay → command dispatch  (inbound commands received from QUIC stream 1)
    let (cmd_tx, mut cmd_rx) = mpsc::channel::<adapter_trait::CommandEnvelope>(128);

    // command dispatch → relay  (ACKs sent back to relay on QUIC stream 1 send direction)
    // Both the hardware adapter ACK and the HTTP SDK /ack endpoint share this sender.
    let (ack_tx, ack_rx) = mpsc::channel::<adapter_trait::Ack>(64);
    let ack_tx_for_local_api = ack_tx.clone(); // HTTP SDK ACKs use this clone

    // command dispatch → SSE clients  (broadcast to all /v1/commands subscribers)
    let (cmd_broadcast_tx, _) = broadcast::channel::<adapter_trait::CommandEnvelope>(32);
    let cmd_broadcast_for_dispatch = cmd_broadcast_tx.clone();

    // HTTP SDK log() → sequencer  (injected into ring buffer alongside adapter data)
    let (log_tx, mut log_rx) = mpsc::channel::<DataEnvelope>(1024);

    // Relay connectivity flag  (healthz reads this; transport writes it)
    let relay_connected = Arc::new(AtomicBool::new(false));
    let relay_connected_transport = Arc::clone(&relay_connected);

    // ── Local HTTP API state ────────────────────────────────────────────────────
    let api_state = Arc::new(local_api::LocalApiState {
        vehicle_id:      cfg.vehicle_id.clone(),
        cmd_broadcast:   cmd_broadcast_tx.clone(),
        ack_tx:          ack_tx_for_local_api,
        log_tx,
        relay_connected: Arc::clone(&relay_connected),
    });

    // ── Task 1: Sequencer ──────────────────────────────────────────────────────
    // Merges the hardware adapter stream with HTTP SDK log() frames.
    // Assigns monotonic seq numbers and writes DataEnvelopes into the ring buffer.
    let adapter_for_seq = Arc::clone(&adapter);
    let vehicle_id_seq  = cfg.vehicle_id.clone();
    let seq_counter     = Arc::new(AtomicU32::new(0));
    let seq_for_task    = Arc::clone(&seq_counter);

    let sequencer_task = tokio::spawn(async move {
        let mut hw_stream = adapter_for_seq.data_stream();
        loop {
            let mut envelope: DataEnvelope = tokio::select! {
                // Hardware adapter stream (MAVLink, ROS2, custom-unix socket)
                Some(env) = hw_stream.next() => env,
                // HTTP SDK log() frames (POST /v1/log)
                Some(env) = log_rx.recv()     => env,
                else => {
                    error!(vehicle = %vehicle_id_seq, "All data sources closed — sequencer exiting");
                    break;
                }
            };

            // Sequencer is the sole writer of seq; adapters leave it at 0
            envelope.seq = seq_for_task.fetch_add(1, Ordering::Relaxed);

            metrics::counter!(
                "agent.envelopes.total",
                "vehicle" => vehicle_id_seq.clone()
            ).increment(1);

            if let Err(e) = ring_producer.push(&envelope) {
                error!(vehicle = %vehicle_id_seq, error = %e, "Ring buffer write error");
            }
        }
    });

    // ── Task 2: QUIC transport ─────────────────────────────────────────────────
    // Reads ring buffer → relay (stream 0).
    // Receives commands from relay (stream 1 recv) → cmd_tx.
    // Sends ACKs from ack_rx → relay (stream 1 send).
    // Updates relay_connected flag.
    let quic_cfg = cfg.quic.clone();

    let transport_task = tokio::spawn(async move {
        if let Err(e) = quic_transport::run_transport_loop(
            quic_cfg,
            ring_consumer,
            cmd_tx,
            ack_rx,
            relay_connected_transport,
        ).await {
            error!(error = %e, "QUIC transport loop fatal error");
        }
    });

    // ── Task 3: Command dispatch ───────────────────────────────────────────────
    // Receives commands from the relay and fans out to:
    //   1. adapter.send_command()  — sends command to vehicle hardware, awaits hardware ACK
    //   2. cmd_broadcast           — delivers command to HTTP SDK SSE subscribers
    //
    // The hardware ACK from adapter.send_command() is sent on ack_tx → QUIC → relay.
    // If the user is using the HTTP SDK instead of hardware, they POST to /v1/commands/{id}/ack
    // which sends on the ack_tx clone → same QUIC stream → relay. Whichever arrives first wins.
    let adapter_for_cmd = Arc::clone(&adapter);
    let vehicle_id_cmd  = cfg.vehicle_id.clone();

    let command_task = tokio::spawn(async move {
        while let Some(cmd) = cmd_rx.recv().await {
            // Broadcast to SSE subscribers immediately so the SDK sees the command with minimal delay
            let _ = cmd_broadcast_for_dispatch.send(cmd.clone());

            // Dispatch to hardware adapter in a dedicated task (one task per command)
            let adapter  = Arc::clone(&adapter_for_cmd);
            let vid      = vehicle_id_cmd.clone();
            let cmd_id   = cmd.command_id.clone();
            let ack_tx_c = ack_tx.clone();

            tokio::spawn(async move {
                info!(vehicle = %vid, command_id = %cmd_id, "Dispatching command to adapter");
                match adapter.send_command(cmd).await {
                    Ok(ack) => {
                        info!(
                            vehicle    = %vid,
                            command_id = %cmd_id,
                            status     = ?ack.status,
                            "Command ACK from adapter"
                        );
                        // Route hardware ACK back to relay via QUIC stream 1
                        if ack_tx_c.send(ack).await.is_err() {
                            error!(vehicle = %vid, command_id = %cmd_id, "ACK channel closed");
                        }
                    }
                    Err(e) => {
                        error!(vehicle = %vid, command_id = %cmd_id, error = %e, "Command dispatch error");
                        // Send a Failed ACK so the relay/cloud doesn't hang waiting
                        let failed_ack = adapter_trait::Ack {
                            command_id:  cmd_id,
                            status:      adapter_trait::AckStatus::Failed,
                            message:     e.to_string(),
                            ack_time_ns: adapter_trait::now_ns(),
                        };
                        let _ = ack_tx_c.send(failed_ack).await;
                    }
                }
            });
        }
    });

    // ── Task 4: Local HTTP API ─────────────────────────────────────────────────
    let local_api_task = tokio::spawn(async move {
        if let Err(e) = local_api::run(api_state).await {
            error!(error = %e, "Local HTTP API exited");
        }
    });

    // ── Run until any task exits (none should under normal operation) ──────────
    tokio::select! {
        result = sequencer_task => {
            error!("Sequencer task exited unexpectedly: {:?}", result);
        }
        result = transport_task => {
            error!("Transport task exited unexpectedly: {:?}", result);
        }
        result = command_task => {
            error!("Command task exited unexpectedly: {:?}", result);
        }
        result = local_api_task => {
            error!("Local HTTP API task exited unexpectedly: {:?}", result);
        }
        _ = tokio::signal::ctrl_c() => {
            info!("Received SIGINT — shutting down gracefully");
        }
    }

    Ok(())
}
