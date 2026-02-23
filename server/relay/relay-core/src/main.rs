//! SystemScale Relay Node
//!
//! The relay node is the most performance-critical server in the system.
//! It sits geographically near vehicles, terminates their QUIC connections,
//! and forwards data to the cloud NATS cluster via private backbone.
//!
//! Processes running on each relay VM:
//!   systemscale-relay  — this binary (QUIC server + NATS publisher)
//!   nats-server        — co-located NATS leaf node (localhost:4222)
//!
//! Kernel tuning requirements (applied by Ansible before this runs):
//!   net.core.rmem_max=134217728          # 128MB UDP receive buffer
//!   net.core.wmem_max=134217728          # 128MB UDP send buffer
//!   net.core.netdev_max_backlog=65536    # per-CPU packet queue depth
//!   SO_REUSEPORT sockets — 1 per CPU core, distributes QUIC connections across cores

mod session;

use std::net::SocketAddr;
use std::path::PathBuf;
use std::sync::Arc;

use anyhow::{Context, Result};
use async_nats::ConnectOptions;
use quinn::{Endpoint, ServerConfig};
use session::{handle_connection, new_registry};
use tracing::{error, info};

// ──────────────────────────────────────────────────────────────────────────────
// Configuration
// ──────────────────────────────────────────────────────────────────────────────

#[derive(Debug, serde::Deserialize)]
struct RelayConfig {
    /// Address and port to listen on for vehicle QUIC connections.
    /// Bind to 0.0.0.0:443 in production (vehicles use port 443 to pass firewalls).
    #[serde(default = "default_listen_addr")]
    listen_addr: SocketAddr,

    /// TLS certificate for the relay's server identity (vehicles verify this against CA cert).
    cert_path: PathBuf,
    key_path:  PathBuf,

    /// Path to vehicle CA certificate — used to verify client certs during mTLS.
    vehicle_ca_path: PathBuf,

    /// NATS leaf node URL — co-located on same host, always localhost.
    #[serde(default = "default_nats_url")]
    nats_url: String,

    /// Human-readable region identifier used in logs and NATS subject routing.
    region_id: String,

    /// Prometheus metrics scrape endpoint.
    #[serde(default = "default_metrics_addr")]
    metrics_addr: SocketAddr,

    /// Maximum concurrent vehicle connections per relay instance.
    /// Alert if approaching this limit — scale out relay nodes.
    #[serde(default = "default_max_connections")]
    max_connections: usize,
}

fn default_listen_addr() -> SocketAddr  { "0.0.0.0:443".parse().unwrap() }
fn default_nats_url()   -> String      { "nats://127.0.0.1:4222".to_string() }
fn default_metrics_addr() -> SocketAddr { "0.0.0.0:9090".parse().unwrap() }
fn default_max_connections() -> usize  { 500 }

fn load_config() -> Result<RelayConfig> {
    let path = std::env::var("SYSTEMSCALE_CONFIG")
        .unwrap_or_else(|_| "/etc/systemscale/relay.yaml".to_string());
    let content = std::fs::read_to_string(&path)
        .with_context(|| format!("reading config: {}", path))?;
    serde_yaml::from_str(&content)
        .with_context(|| format!("parsing config: {}", path))
}

// ──────────────────────────────────────────────────────────────────────────────
// Entry point
// ──────────────────────────────────────────────────────────────────────────────

#[tokio::main]
async fn main() -> Result<()> {
    let cfg = load_config()?;

    // Structured JSON logging — every log line is machine-parseable
    tracing_subscriber::fmt()
        .json()
        .with_env_filter(
            tracing_subscriber::EnvFilter::try_from_default_env()
                .unwrap_or_else(|_| "info".parse().unwrap())
        )
        .init();

    info!(
        region    = %cfg.region_id,
        listen    = %cfg.listen_addr,
        nats      = %cfg.nats_url,
        version   = env!("CARGO_PKG_VERSION"),
        "SystemScale relay starting"
    );

    // Start Prometheus metrics exporter
    metrics_exporter_prometheus::PrometheusBuilder::new()
        .with_http_listener(cfg.metrics_addr)
        .install_recorder()
        .map_err(|e| anyhow::anyhow!("metrics exporter: {}", e))?;

    // Connect to co-located NATS leaf node
    let nats = async_nats::connect_with_options(
        &cfg.nats_url,
        ConnectOptions::new()
            .name(format!("relay-{}", cfg.region_id))
            .ping_interval(std::time::Duration::from_secs(5))
            .max_reconnects(None) // reconnect forever
    )
    .await
    .with_context(|| format!("connecting to NATS at {}", cfg.nats_url))?;

    info!("Connected to NATS leaf node");

    // Build QUIC server endpoint with mTLS
    let quic_config = build_server_config(&cfg)?;
    let endpoint = Endpoint::server(quic_config, cfg.listen_addr)
        .with_context(|| format!("binding QUIC endpoint on {}", cfg.listen_addr))?;

    info!(addr = %cfg.listen_addr, "QUIC server listening for vehicle connections");

    // Shared session registry — all tasks share this via Arc<DashMap>
    let registry = new_registry();

    // Start health monitor task (removes stale sessions, logs connection stats)
    let registry_for_monitor = Arc::clone(&registry);
    let region_for_monitor = cfg.region_id.clone();
    tokio::spawn(async move {
        health_monitor(registry_for_monitor, region_for_monitor).await;
    });

    // Accept vehicle QUIC connections
    while let Some(incoming) = endpoint.accept().await {
        let nats      = nats.clone();
        let registry  = Arc::clone(&registry);
        let region    = cfg.region_id.clone();
        let max_conns = cfg.max_connections;

        tokio::spawn(async move {
            // Reject connections if at capacity
            if registry.len() >= max_conns {
                error!(
                    current = registry.len(),
                    max = max_conns,
                    "Connection rejected — at capacity. Scale out relay nodes."
                );
                incoming.refuse();
                metrics::counter!("relay.connections.rejected", "reason" => "capacity").increment(1);
                return;
            }

            // Complete QUIC handshake (includes mTLS certificate verification)
            match incoming.await {
                Ok(connection) => {
                    handle_connection(connection, nats, registry, region).await;
                }
                Err(e) => {
                    error!(error = %e, "QUIC handshake failed");
                    metrics::counter!("relay.connections.handshake_failed").increment(1);
                }
            }
        });
    }

    Ok(())
}

// ──────────────────────────────────────────────────────────────────────────────
// QUIC server config with mTLS
// ──────────────────────────────────────────────────────────────────────────────

fn build_server_config(cfg: &RelayConfig) -> Result<ServerConfig> {
    // Load relay server certificate
    let cert_pem = std::fs::read(&cfg.cert_path)
        .with_context(|| format!("reading cert: {}", cfg.cert_path.display()))?;
    let key_pem = std::fs::read(&cfg.key_path)
        .with_context(|| format!("reading key: {}", cfg.key_path.display()))?;
    let ca_pem = std::fs::read(&cfg.vehicle_ca_path)
        .with_context(|| format!("reading vehicle CA: {}", cfg.vehicle_ca_path.display()))?;

    let certs = rustls_pemfile::certs(&mut cert_pem.as_slice())
        .collect::<Result<Vec<_>, _>>()
        .context("parsing relay certificate")?;
    let key = rustls_pemfile::private_key(&mut key_pem.as_slice())
        .context("parsing relay private key")?
        .context("no private key found")?;

    // Vehicle CA — relay only accepts connections from vehicles signed by this CA
    let mut vehicle_roots = rustls::RootCertStore::empty();
    for ca_cert in rustls_pemfile::certs(&mut ca_pem.as_slice()) {
        vehicle_roots.add(ca_cert.context("parsing vehicle CA cert")?)?;
    }

    let client_cert_verifier = rustls::server::WebPkiClientVerifier::builder(
        Arc::new(vehicle_roots)
    )
    .build()
    .context("building client cert verifier")?;

    let tls_config = rustls::ServerConfig::builder()
        .with_client_cert_verifier(client_cert_verifier)
        .with_single_cert(certs, key)
        .context("building TLS server config")?;

    let mut transport = quinn::TransportConfig::default();
    // Max idle timeout: 30s. Vehicles send heartbeats every 10s, so this catches dead connections.
    transport.max_idle_timeout(Some(std::time::Duration::from_secs(30).try_into().unwrap()));
    // Allow 0-RTT on session resumption (edge agent reconnects without full handshake)
    // Note: quinn enables 0-RTT by default when TLS session tickets are enabled.

    let mut server_config = ServerConfig::with_crypto(Arc::new(
        quinn::crypto::rustls::QuicServerConfig::try_from(tls_config)
            .context("building QUIC server config")?
    ));
    server_config.transport_config(Arc::new(transport));

    Ok(server_config)
}

// ──────────────────────────────────────────────────────────────────────────────
// Health monitor
// ──────────────────────────────────────────────────────────────────────────────

async fn health_monitor(
    registry: session::SessionRegistry,
    region:   String,
) {
    let mut interval = tokio::time::interval(std::time::Duration::from_secs(30));
    loop {
        interval.tick().await;

        let now = std::time::Instant::now();
        let stale_threshold = std::time::Duration::from_secs(45); // 3 missed heartbeats
        let mut stale_vehicles: Vec<String> = Vec::new();

        for entry in registry.iter() {
            if now.duration_since(entry.last_heartbeat) > stale_threshold {
                stale_vehicles.push(entry.vehicle_id.clone());
            }
        }

        for vid in &stale_vehicles {
            tracing::warn!(vehicle_id = %vid, region = %region, "Removing stale vehicle session (missed heartbeats)");
            registry.remove(vid);
        }

        let active = registry.len();
        metrics::gauge!("relay.sessions.active", "region" => region.clone()).set(active as f64);
        info!(active_sessions = active, region = %region, "Health check: {} active vehicle sessions", active);
    }
}
