//! Command Forwarder — consumes NATS `command.{vehicle_id}` subjects and pushes
//! payloads to the correct vehicle QUIC session via an in-process channel.
//!
//! The command forwarder runs as a sibling process to relay-core on each relay VM.
//! It communicates with relay-core via a shared DashMap<vehicle_id, mpsc::Sender<Bytes>>
//! that is exposed over a local Unix socket (IPC).
//!
//! Architecture:
//!   NATS cluster (command.vehicle_id) → leaf node (local)
//!     → command-forwarder subscribes to `command.>`
//!     → looks up vehicle session in relay-core shared registry (IPC or embedded)
//!     → writes payload to session's cmd_tx channel
//!     → relay-core sends it on QUIC stream 1 to the vehicle
//!
//! Because the command forwarder is co-located with relay-core on the same VM,
//! the NATS→forwarder path is localhost (sub-ms). The forwarder→relay-core path
//! is also in-process (channel send). The only network is NATS→VM, which is
//! the private backbone (10-20ms from cloud).
//!
//! In this implementation, command-forwarder is embedded into relay-core as a
//! separate tokio task (sharing the SessionRegistry directly), keeping it as a
//! standalone binary for independent deployment if desired.

use std::collections::HashMap;
use std::sync::Arc;
use std::time::Duration;

use anyhow::Context;
use async_nats::jetstream;
use bytes::Bytes;
use dashmap::DashMap;
use serde::Deserialize;
use tokio::sync::mpsc;
use tracing::{error, info, warn};

// ──────────────────────────────────────────────────────────────────────────────
// Configuration
// ──────────────────────────────────────────────────────────────────────────────

#[derive(Debug, Deserialize)]
struct Config {
    /// NATS server URL for the local leaf node
    nats_url: String,
    /// Region identifier (for metrics labels)
    region_id: String,
    /// Prometheus metrics endpoint
    metrics_addr: String,
    /// Max in-flight commands per vehicle before backpressure
    #[serde(default = "default_cmd_buffer")]
    cmd_buffer_size: usize,
}

fn default_cmd_buffer() -> usize { 64 }

fn load_config() -> anyhow::Result<Config> {
    let path = std::env::var("SYSTEMSCALE_CONFIG")
        .unwrap_or_else(|_| "/etc/systemscale/command-forwarder.yaml".to_string());
    let text = std::fs::read_to_string(&path)
        .with_context(|| format!("read config {path}"))?;
    serde_yaml::from_str(&text).context("parse config yaml")
}

// ──────────────────────────────────────────────────────────────────────────────
// Session sender registry
//
// In production deployment this is shared with relay-core via Tokio in-process
// channels. The registry maps vehicle_id → mpsc::Sender<Bytes> for the QUIC
// command stream of that vehicle's session.
//
// When command-forwarder is embedded in relay-core (recommended), this type
// alias is shared between both modules. When deployed standalone, the registry
// is populated via the IPC listener below.
// ──────────────────────────────────────────────────────────────────────────────

pub type CommandRegistry = Arc<DashMap<String, mpsc::Sender<Bytes>>>;

pub fn new_command_registry() -> CommandRegistry {
    Arc::new(DashMap::with_capacity(512))
}

// ──────────────────────────────────────────────────────────────────────────────
// Core forwarder logic — can be called from relay-core directly
// ──────────────────────────────────────────────────────────────────────────────

/// Subscribe to `command.>` on NATS and forward each message to the appropriate
/// vehicle session channel. This is the hot path for command delivery.
///
/// Subject format: `command.{vehicle_id}`
/// Payload: raw CommandEnvelope protobuf bytes (relay passes them through opaquely)
///
/// Delivery semantics:
/// - JetStream durable consumer → at-least-once delivery from NATS
/// - Deduplication happens at the edge agent (CommandEnvelope.command_id)
/// - If the vehicle is not connected to this relay, the message is NACKed
///   and redelivered to another relay's command-forwarder via NATS
pub async fn run_forwarder(
    nats: async_nats::Client,
    registry: CommandRegistry,
    region_id: String,
) -> anyhow::Result<()> {
    let js = jetstream::new(nats);

    // Create or bind to the durable consumer on the "command" stream
    // The stream captures all command.> subjects with JetStream persistence
    let stream = js.get_stream("command").await
        .context("get command JetStream stream — ensure it is created at startup")?;

    let consumer = stream.get_or_create_consumer(
        "relay-forwarder",
        jetstream::consumer::pull::Config {
            durable_name: Some(format!("relay-forwarder-{region_id}")),
            filter_subject: "command.>".to_string(),
            ack_policy: jetstream::consumer::AckPolicy::Explicit,
            ack_wait: Duration::from_secs(30),
            max_deliver: 5, // retry 5x then DLQ
            ..Default::default()
        },
    ).await.context("create/get forwarder consumer")?;

    info!(region = %region_id, "Command forwarder ready, consuming command.>");

    // Pull messages in batches for throughput
    let mut messages = consumer.messages().await
        .context("open consumer message stream")?;

    use futures::StreamExt;
    while let Some(msg_result) = messages.next().await {
        let msg = match msg_result {
            Ok(m) => m,
            Err(e) => {
                error!(error = %e, "Error receiving command message");
                continue;
            }
        };

        let subject = msg.subject.clone();
        let payload = Bytes::copy_from_slice(&msg.payload);

        // Parse vehicle_id from subject: "command.{vehicle_id}"
        let vehicle_id = match subject.as_str().strip_prefix("command.") {
            Some(id) if !id.is_empty() && !id.contains('.') => id.to_string(),
            _ => {
                warn!(subject = %subject, "Unexpected command subject format");
                msg.ack().await.ok();
                continue;
            }
        };

        // Look up the vehicle session on this relay
        match registry.get(&vehicle_id) {
            Some(sender) => {
                match sender.try_send(payload) {
                    Ok(_) => {
                        msg.ack().await.ok();
                        metrics::counter!(
                            "forwarder.commands.forwarded",
                            "region"     => region_id.clone(),
                            "vehicle_id" => vehicle_id.clone()
                        ).increment(1);
                        info!(vehicle_id = %vehicle_id, "Command forwarded to vehicle session");
                    }
                    Err(mpsc::error::TrySendError::Full(_)) => {
                        // Vehicle's command channel is full — backpressure
                        // NACK the message to retry after ack_wait
                        warn!(vehicle_id = %vehicle_id, "Command channel full, NACKing for retry");
                        msg.ack_with(async_nats::jetstream::AckKind::Nak(None)).await.ok();
                        metrics::counter!(
                            "forwarder.commands.backpressure",
                            "region" => region_id.clone()
                        ).increment(1);
                    }
                    Err(mpsc::error::TrySendError::Closed(_)) => {
                        // Session disconnected between registry lookup and send
                        warn!(vehicle_id = %vehicle_id, "Vehicle session closed, NACKing");
                        msg.ack_with(async_nats::jetstream::AckKind::Nak(None)).await.ok();
                        metrics::counter!(
                            "forwarder.commands.no_session",
                            "region" => region_id.clone()
                        ).increment(1);
                    }
                }
            }
            None => {
                // Vehicle not connected to this relay
                // NACK with delay so NATS delivers to another relay's forwarder
                warn!(
                    vehicle_id = %vehicle_id,
                    "Vehicle not on this relay, NACKing for redelivery"
                );
                msg.ack_with(async_nats::jetstream::AckKind::Nak(Some(Duration::from_secs(2)))).await.ok();
                metrics::counter!(
                    "forwarder.commands.not_on_relay",
                    "region" => region_id.clone()
                ).increment(1);
            }
        }
    }

    Ok(())
}

// ──────────────────────────────────────────────────────────────────────────────
// Standalone binary entry point
// ──────────────────────────────────────────────────────────────────────────────

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    // Logging
    tracing_subscriber::fmt()
        .with_env_filter(
            tracing_subscriber::EnvFilter::from_env("RUST_LOG")
                .add_directive("command_forwarder=info".parse().unwrap())
        )
        .json()
        .init();

    let cfg = load_config()?;
    info!(region = %cfg.region_id, "Starting command-forwarder (standalone mode)");

    // Prometheus metrics
    let builder = metrics_exporter_prometheus::PrometheusBuilder::new();
    builder
        .with_http_listener(cfg.metrics_addr.parse::<std::net::SocketAddr>()?)
        .install()
        .context("install metrics exporter")?;

    // NATS connection
    let nats = async_nats::connect(&cfg.nats_url)
        .await
        .with_context(|| format!("NATS connect {}", cfg.nats_url))?;
    info!(url = %cfg.nats_url, "Connected to NATS");

    // In standalone mode, the registry is populated via a local gRPC or Unix socket
    // from relay-core. For simplicity here, we use an empty registry — in production
    // relay-core embeds this module and passes its SessionRegistry directly.
    let registry: CommandRegistry = new_command_registry();

    // Run the forwarder (blocks until NATS disconnects or error)
    run_forwarder(nats, registry, cfg.region_id).await
}
