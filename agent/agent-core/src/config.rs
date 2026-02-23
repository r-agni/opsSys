//! Agent configuration loaded from YAML file + environment variable overrides.
//!
//! Configuration file location: /etc/systemscale/agent.yaml (default)
//! Override via environment: SYSTEMSCALE_CONFIG=/path/to/config.yaml
//!
//! All fields that contain secrets (cert paths, keys) reference filesystem paths —
//! the secrets themselves are never embedded in the config file.

use adapter_trait::AdapterType;
use custom_unix_adapter::CustomUnixConfig;
use mavlink_adapter::MavlinkAdapterConfig;
use quic_transport::QuicTransportConfig;
use ring_buffer::RingBufferConfig;
use serde::Deserialize;

#[derive(Debug, Deserialize)]
pub struct AgentConfig {
    /// Globally unique identifier for this vehicle. Embedded in every DataEnvelope.
    /// Must match the Subject CN of the vehicle's X.509 certificate.
    pub vehicle_id: String,

    /// Which protocol adapter to use for communicating with vehicle hardware.
    pub adapter: AdapterType,

    /// MAVLink adapter settings (required when adapter = "mavlink_udp" or "mavlink_serial")
    pub mavlink: Option<MavlinkAdapterConfig>,

    /// Custom Unix socket adapter settings (required when adapter = "custom_unix")
    pub custom_unix: Option<CustomUnixConfig>,

    /// QUIC transport settings (relay connection)
    pub quic: QuicTransportConfig,

    /// Ring buffer settings
    #[serde(default)]
    pub ring_buffer: RingBufferConfig,

    /// Metrics export (Prometheus scrape endpoint)
    #[serde(default)]
    pub metrics: MetricsConfig,

    /// Log level: "trace", "debug", "info", "warn", "error"
    #[serde(default = "default_log_level")]
    pub log_level: String,
}

#[derive(Debug, Deserialize)]
pub struct MetricsConfig {
    /// Bind address for Prometheus scrape endpoint.
    #[serde(default = "default_metrics_addr")]
    pub bind_addr: String,
}

impl Default for MetricsConfig {
    fn default() -> Self {
        Self { bind_addr: default_metrics_addr() }
    }
}

fn default_log_level() -> String { "info".to_string() }
fn default_metrics_addr() -> String { "0.0.0.0:9090".to_string() }

impl AgentConfig {
    pub fn load() -> anyhow::Result<Self> {
        let config_path = std::env::var("SYSTEMSCALE_CONFIG")
            .unwrap_or_else(|_| "/etc/systemscale/agent.yaml".to_string());

        let content = std::fs::read_to_string(&config_path)
            .map_err(|e| anyhow::anyhow!("Cannot read config file {}: {}", config_path, e))?;

        let config: AgentConfig = serde_yaml::from_str(&content)
            .map_err(|e| anyhow::anyhow!("Invalid config file {}: {}", config_path, e))?;

        Ok(config)
    }
}
