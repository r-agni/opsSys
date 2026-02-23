//! QUIC transport layer — connects the edge agent to its nearest relay node.
//!
//! Architecture:
//! - Single QUIC connection to relay (outbound-only, no inbound ports needed)
//! - 4 multiplexed streams on the connection:
//!     Stream 0 (send-only): telemetry — high-frequency, fire-and-forget
//!     Stream 1 (bidirectional): commands — relay→vehicle with ACK back
//!     Stream 2 (send-only): heartbeat — 10s keepalive
//!     Stream 3 (send-only): video metadata — SRT session info
//! - On disconnect: exponential backoff with jitter, then 0-RTT resume
//! - On reconnect: signals ring buffer consumer to enter catch-up replay mode
//! - mTLS: vehicle X.509 certificate presented to relay for identity verification
//!
//! Wire framing: each message on the QUIC stream is length-prefixed:
//!   [4 bytes LE u32 length][length bytes proto payload]
//! This is the same framing used by gRPC and most Protobuf-over-stream protocols.

use std::net::SocketAddr;
use std::path::PathBuf;
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::time::Duration;

use adapter_trait::{Ack, AckStatus, CommandEnvelope, DataEnvelope};
use anyhow::{Context, Result};
use bytes::{BufMut, BytesMut};
use quinn::{ClientConfig, Connection, Endpoint, SendStream, RecvStream};
use ring_buffer::RingBufferConsumer;
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::sync::mpsc;
use tracing::{debug, error, info, warn};

// ──────────────────────────────────────────────────────────────────────────────
// Configuration
// ──────────────────────────────────────────────────────────────────────────────

#[derive(Debug, Clone, serde::Deserialize)]
pub struct QuicTransportConfig {
    /// Relay endpoint to connect to. Vehicles connect to the Anycast IP —
    /// BGP routing delivers to the geographically nearest relay node.
    pub relay_addr: SocketAddr,
    /// SNI hostname for TLS verification (matches relay TLS certificate).
    pub relay_hostname: String,
    /// Path to vehicle's X.509 client certificate (PEM format).
    pub cert_path: PathBuf,
    /// Path to vehicle's private key (PEM format).
    pub key_path: PathBuf,
    /// Path to the relay CA certificate for server verification.
    pub ca_cert_path: PathBuf,
    /// Initial reconnect delay. Doubled on each failure (capped at max_backoff_ms).
    #[serde(default = "default_initial_backoff")]
    pub initial_backoff_ms: u64,
    /// Maximum reconnect backoff in milliseconds.
    #[serde(default = "default_max_backoff")]
    pub max_backoff_ms: u64,
    /// Heartbeat interval in seconds.
    #[serde(default = "default_heartbeat")]
    pub heartbeat_interval_s: u64,
}

fn default_initial_backoff() -> u64 { 250 }
fn default_max_backoff() -> u64 { 30_000 }
fn default_heartbeat() -> u64 { 10 }

// ──────────────────────────────────────────────────────────────────────────────
// QUIC transport — manages one connection, all streams
// ──────────────────────────────────────────────────────────────────────────────

/// Runs the QUIC transport loop: connect → stream telemetry + receive commands → reconnect.
///
/// This function never returns under normal operation. It reconnects indefinitely
/// on any connection failure.
///
/// Arguments:
/// - `config`:          QUIC connection parameters (relay address, certs, etc.)
/// - `ring_consumer`:   Ring buffer consumer — feeds telemetry to relay on stream 0
/// - `cmd_tx`:          Receives commands from relay on stream 1; forwarded to command dispatch
/// - `ack_rx`:          Receives Acks from command dispatch; sent back to relay on stream 1
/// - `relay_connected`: Shared flag written true on connect, false on disconnect
///
/// The adapter is intentionally NOT a parameter here. The transport is purely QUIC I/O:
/// - Telemetry comes from the ring buffer (decoupled from the adapter by design)
/// - Commands go to the dispatch task via mpsc channel
/// - ACKs come back from the dispatch task via mpsc channel
pub async fn run_transport_loop(
    config:          QuicTransportConfig,
    mut ring_consumer: RingBufferConsumer,
    cmd_tx:          mpsc::Sender<CommandEnvelope>,
    mut ack_rx:      mpsc::Receiver<Ack>,
    relay_connected: Arc<AtomicBool>,
) -> Result<()> {
    let mut backoff_ms = config.initial_backoff_ms;
    let endpoint = build_quic_endpoint(&config)
        .context("building QUIC endpoint")?;

    loop {
        info!(relay = %config.relay_addr, "Connecting to relay");

        match connect_and_run(&config, &endpoint, &mut ring_consumer, &cmd_tx, &mut ack_rx).await {
            Ok(()) => {
                info!("QUIC transport loop exited cleanly");
                relay_connected.store(false, Ordering::Relaxed);
                break;
            }
            Err(e) => {
                relay_connected.store(false, Ordering::Relaxed);
                error!(error = %e, backoff_ms, "QUIC connection failed — reconnecting");
            }
        }

        // Jittered exponential backoff before next attempt
        let jitter = rand_jitter(backoff_ms / 4);
        tokio::time::sleep(Duration::from_millis(backoff_ms + jitter)).await;
        backoff_ms = (backoff_ms * 2).min(config.max_backoff_ms);
    }

    Ok(())
}

async fn connect_and_run(
    config:        &QuicTransportConfig,
    endpoint:      &Endpoint,
    ring_consumer: &mut RingBufferConsumer,
    cmd_tx:        &mpsc::Sender<CommandEnvelope>,
    ack_rx:        &mut mpsc::Receiver<Ack>,
) -> Result<()> {
    // QUIC connect — attempts 0-RTT session resumption if a prior session exists.
    // On first connect this is a full 1-RTT handshake with mTLS.
    // On reconnect after recent disconnect: 0-RTT means data can flow before handshake completes.
    let connection = endpoint
        .connect(config.relay_addr, &config.relay_hostname)
        .context("QUIC connect")?
        .await
        .context("QUIC handshake")?;

    info!(
        relay = %config.relay_addr,
        rtt   = ?connection.rtt(),
        "QUIC connected to relay"
    );

    // Signal ring buffer to enter catch-up replay mode
    ring_consumer.on_reconnect();

    // Open the four streams
    let (telemetry_tx, _)          = connection.open_bi().await.context("opening telemetry stream")?;
    let (command_ack_tx, command_rx) = connection.open_bi().await.context("opening command stream")?;
    let (heartbeat_tx, _)          = connection.open_bi().await.context("opening heartbeat stream")?;

    // Run all stream tasks concurrently. Any error causes the whole connection to drop + reconnect.
    tokio::try_join!(
        send_telemetry_loop(telemetry_tx, ring_consumer),
        receive_commands_loop(command_rx, cmd_tx.clone()),
        send_acks_loop(command_ack_tx, ack_rx),
        send_heartbeat_loop(heartbeat_tx, config.heartbeat_interval_s),
        wait_for_connection_close(connection),
    )?;

    Ok(())
}

/// Reads from the ring buffer and writes serialized DataEnvelopes to stream 0.
async fn send_telemetry_loop(
    mut stream: SendStream,
    ring: &mut RingBufferConsumer,
) -> Result<()> {
    loop {
        let Some(payload) = ring.pop().await else { break };

        // Length-prefix framing: [4-byte LE length][payload bytes]
        let len = payload.len() as u32;
        stream.write_all(&len.to_le_bytes()).await
            .context("writing telemetry length prefix")?;
        stream.write_all(&payload).await
            .context("writing telemetry payload")?;

        debug!(bytes = payload.len() + 4, "Sent telemetry frame to relay");
    }
    Ok(())
}

/// Reads CommandEnvelope frames from stream 1 and forwards them to the adapter via channel.
async fn receive_commands_loop(
    mut stream: RecvStream,
    cmd_tx: mpsc::Sender<CommandEnvelope>,
) -> Result<()> {
    let mut len_buf = [0u8; 4];
    loop {
        // Read length prefix
        stream.read_exact(&mut len_buf).await
            .context("reading command length prefix")?;
        let len = u32::from_le_bytes(len_buf) as usize;

        anyhow::ensure!(len <= 65_536, "command frame too large: {} bytes", len);

        let mut payload = vec![0u8; len];
        stream.read_exact(&mut payload).await
            .context("reading command payload")?;

        // Decode CommandEnvelope from payload bytes
        match decode_command_envelope(&payload) {
            Ok(cmd) => {
                debug!(command_id = %cmd.command_id, "Received command from relay");
                if cmd_tx.send(cmd).await.is_err() {
                    anyhow::bail!("command channel closed — adapter shutting down");
                }
            }
            Err(e) => {
                warn!(error = %e, "Failed to decode command envelope — skipping");
            }
        }
    }
    Ok(())
}

/// Sends periodic heartbeat frames on stream 2.
/// The relay side uses these to detect stale connections and trigger failover.
async fn send_heartbeat_loop(mut stream: SendStream, interval_s: u64) -> Result<()> {
    let interval = Duration::from_secs(interval_s);
    loop {
        tokio::time::sleep(interval).await;
        // Heartbeat payload: 4 bytes of current timestamp (seconds since epoch, u32 BE)
        let ts = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap_or_default()
            .as_secs() as u32;
        stream.write_all(&ts.to_be_bytes()).await
            .context("sending heartbeat")?;
        debug!("Sent heartbeat to relay");
    }
}

/// Reads Acks from the command dispatch task and writes them to QUIC stream 1 (send direction).
/// The relay's `receive_command_acks` then publishes them to NATS so command-api can resolve.
///
/// Wire format: same length-prefix framing as telemetry — [4-byte LE len][proto bytes].
/// Proto fields mirror the `Ack` type defined in adapter-trait.
async fn send_acks_loop(
    mut stream: SendStream,
    ack_rx: &mut mpsc::Receiver<Ack>,
) -> Result<()> {
    while let Some(ack) = ack_rx.recv().await {
        let payload = encode_ack(&ack);
        let len = payload.len() as u32;
        stream.write_all(&len.to_le_bytes()).await
            .context("writing ACK length prefix")?;
        stream.write_all(&payload).await
            .context("writing ACK payload")?;
        debug!(command_id = %ack.command_id, status = ?ack.status, "Sent ACK to relay");
    }
    // ack_rx closed means agent is shutting down — this is a clean exit
    Ok(())
}

/// Encodes an `Ack` as a minimal protobuf binary message.
///
/// Field layout (must match proto/core/envelope.proto `CommandAck`):
///   1: command_id  (string)
///   2: status      (enum/varint: 1=Accepted, 2=Completed, 3=Rejected, 4=Failed, 5=Timeout)
///   3: message     (string)
///   4: ack_time_ns (uint64/varint)
fn encode_ack(ack: &Ack) -> bytes::Bytes {
    let mut buf = BytesMut::new();

    // Field 1: command_id (wire type 2 = length-delimited)
    buf.put_u8(0x0A); // tag: field 1, wire 2
    let id_bytes = ack.command_id.as_bytes();
    append_varint(&mut buf, id_bytes.len() as u64);
    buf.put_slice(id_bytes);

    // Field 2: status (wire type 0 = varint)
    let status_val: u64 = match ack.status {
        AckStatus::Accepted  => 1,
        AckStatus::Completed => 2,
        AckStatus::Rejected  => 3,
        AckStatus::Failed    => 4,
        AckStatus::Timeout   => 5,
    };
    buf.put_u8(0x10); // tag: field 2, wire 0
    append_varint(&mut buf, status_val);

    // Field 3: message (wire type 2)
    if !ack.message.is_empty() {
        buf.put_u8(0x1A); // tag: field 3, wire 2
        let msg_bytes = ack.message.as_bytes();
        append_varint(&mut buf, msg_bytes.len() as u64);
        buf.put_slice(msg_bytes);
    }

    // Field 4: ack_time_ns (wire type 0)
    buf.put_u8(0x20); // tag: field 4, wire 0
    append_varint(&mut buf, ack.ack_time_ns);

    buf.freeze()
}

fn append_varint(buf: &mut BytesMut, mut v: u64) {
    loop {
        let mut b = (v & 0x7F) as u8;
        v >>= 7;
        if v != 0 { b |= 0x80; }
        buf.put_u8(b);
        if v == 0 { break; }
    }
}

/// Resolves when the QUIC connection closes (relay restart, network partition, etc.).
/// Triggers reconnect loop.
async fn wait_for_connection_close(conn: Connection) -> Result<()> {
    let reason = conn.closed().await;
    Err(anyhow::anyhow!("QUIC connection closed: {:?}", reason))
}

// ──────────────────────────────────────────────────────────────────────────────
// Command decoder (minimal proto decode of CommandEnvelope)
// ──────────────────────────────────────────────────────────────────────────────

fn decode_command_envelope(buf: &[u8]) -> Result<CommandEnvelope> {
    let mut vehicle_id  = String::new();
    let mut command_id  = String::new();
    let mut payload     = bytes::Bytes::new();
    let mut priority    = adapter_trait::Priority::Normal;
    let mut ttl_ms      = 0u32;

    let mut pos = 0usize;
    while pos < buf.len() {
        let tag  = proto_read_varint(buf, &mut pos)?;
        let field = (tag >> 3) as u32;
        let wire  = tag & 0x7;

        match (field, wire) {
            (1, 2) => vehicle_id  = proto_read_string(buf, &mut pos)?,
            (2, 2) => command_id  = proto_read_string(buf, &mut pos)?,
            (3, 2) => payload     = proto_read_bytes(buf, &mut pos)?,
            (4, 0) => {
                priority = match proto_read_varint(buf, &mut pos)? {
                    2 => adapter_trait::Priority::High,
                    3 => adapter_trait::Priority::Emergency,
                    _ => adapter_trait::Priority::Normal,
                };
            }
            (7, 0) => ttl_ms = proto_read_varint(buf, &mut pos)? as u32,
            (_, 0) => { proto_read_varint(buf, &mut pos)?; },
            (_, 2) => { proto_read_bytes(buf, &mut pos)?; },
            (_, 5) => { pos += 4; },
            (_, 1) => { pos += 8; },
            _ => break,
        }
    }

    anyhow::ensure!(!vehicle_id.is_empty(), "CommandEnvelope missing vehicle_id");
    anyhow::ensure!(!command_id.is_empty(), "CommandEnvelope missing command_id");

    Ok(CommandEnvelope { vehicle_id, command_id, payload, priority, ttl_ms })
}

// ──────────────────────────────────────────────────────────────────────────────
// QUIC endpoint builder (mTLS)
// ──────────────────────────────────────────────────────────────────────────────

fn build_quic_endpoint(config: &QuicTransportConfig) -> Result<Endpoint> {
    // Load vehicle client certificate (for mTLS — vehicle proves its identity to relay)
    let cert_pem = std::fs::read(&config.cert_path)
        .with_context(|| format!("reading cert: {}", config.cert_path.display()))?;
    let key_pem = std::fs::read(&config.key_path)
        .with_context(|| format!("reading key: {}", config.key_path.display()))?;
    let ca_pem = std::fs::read(&config.ca_cert_path)
        .with_context(|| format!("reading CA cert: {}", config.ca_cert_path.display()))?;

    let certs = rustls_pemfile::certs(&mut cert_pem.as_slice())
        .collect::<Result<Vec<_>, _>>()
        .context("parsing vehicle certificate")?;
    let key = rustls_pemfile::private_key(&mut key_pem.as_slice())
        .context("parsing vehicle private key")?
        .context("no private key found")?;

    let mut roots = rustls::RootCertStore::empty();
    for ca_cert in rustls_pemfile::certs(&mut ca_pem.as_slice()) {
        roots.add(ca_cert.context("parsing CA cert")?).context("adding CA cert to store")?;
    }

    let tls_config = rustls::ClientConfig::builder()
        .with_root_certificates(roots)
        .with_client_auth_cert(certs, key)
        .context("building TLS client config")?;

    let mut client_config = ClientConfig::new(Arc::new(
        quinn::crypto::rustls::QuicClientConfig::try_from(tls_config)
            .context("building QUIC client config from TLS")?
    ));

    // Enable BBR v2 congestion control for better performance on 150ms+ RTT links
    let mut transport = quinn::TransportConfig::default();
    transport.congestion_controller_factory(Arc::new(quinn::congestion::BbrConfig::default()));
    // Keep-alive: send QUIC PINGs every 15s so NAT mappings don't expire
    transport.keep_alive_interval(Some(Duration::from_secs(15)));
    // Max idle timeout: 30s (relay drops connection if no data AND no keepalive for 30s)
    transport.max_idle_timeout(Some(Duration::from_secs(30).try_into().unwrap()));
    client_config.transport_config(Arc::new(transport));

    // Bind to any local port — outbound only, no inbound traffic needed
    let mut endpoint = Endpoint::client("0.0.0.0:0".parse().unwrap())
        .context("creating QUIC endpoint")?;
    endpoint.set_default_client_config(client_config);

    Ok(endpoint)
}

// ──────────────────────────────────────────────────────────────────────────────
// Proto read helpers
// ──────────────────────────────────────────────────────────────────────────────

fn proto_read_varint(buf: &[u8], pos: &mut usize) -> Result<u64> {
    let mut result = 0u64;
    let mut shift = 0;
    loop {
        anyhow::ensure!(*pos < buf.len(), "varint truncated at pos {}", *pos);
        let b = buf[*pos]; *pos += 1;
        result |= ((b & 0x7F) as u64) << shift;
        if b & 0x80 == 0 { return Ok(result); }
        shift += 7;
        anyhow::ensure!(shift < 64, "varint overflow");
    }
}

fn proto_read_len_delim<'a>(buf: &'a [u8], pos: &mut usize) -> Result<&'a [u8]> {
    let len = proto_read_varint(buf, pos)? as usize;
    anyhow::ensure!(*pos + len <= buf.len(), "len-delim field truncated");
    let data = &buf[*pos..*pos + len];
    *pos += len;
    Ok(data)
}

fn proto_read_string(buf: &[u8], pos: &mut usize) -> Result<String> {
    let data = proto_read_len_delim(buf, pos)?;
    Ok(String::from_utf8_lossy(data).into_owned())
}

fn proto_read_bytes(buf: &[u8], pos: &mut usize) -> Result<bytes::Bytes> {
    let data = proto_read_len_delim(buf, pos)?;
    Ok(bytes::Bytes::copy_from_slice(data))
}

// ──────────────────────────────────────────────────────────────────────────────
// Helpers
// ──────────────────────────────────────────────────────────────────────────────

/// Simple non-cryptographic jitter in range [0, max_ms]
fn rand_jitter(max_ms: u64) -> u64 {
    if max_ms == 0 { return 0; }
    // Use lower bits of current time as entropy source
    let entropy = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap_or_default()
        .subsec_nanos() as u64;
    entropy % (max_ms + 1)
}
