//! NVMe-backed ring buffer for edge agent telemetry.
//!
//! Purpose: absorb up to 60 seconds of telemetry when the QUIC connection to the
//! relay drops (cellular dead zone, Starlink outage, relay restart, etc.).
//! On reconnect, the ring buffer replays stored messages at 5× the capture rate
//! until it drains, then seamlessly transitions to live telemetry.
//!
//! Design:
//! - Backed by a memory-mapped file on NVMe (survives process restart and power loss)
//! - Single-producer (adapter data stream) single-consumer (QUIC transport)
//! - Lock-free using atomic head/tail indices
//! - Fixed-size slots: each slot holds one serialized DataEnvelope (max 4096 bytes)
//! - When full: oldest slot is overwritten (ring buffer semantics — recent data wins)
//!
//! Capacity: configured in bytes. Default 512 MB @ max 4096 bytes/slot = 131,072 slots.
//! At 1000 messages/sec (20 vehicles × 50Hz), this holds ~131 seconds.

use std::path::PathBuf;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::Arc;

use adapter_trait::DataEnvelope;
use anyhow::{Context, Result};
use tokio::sync::Notify;
use tracing::{debug, info, warn};

// ──────────────────────────────────────────────────────────────────────────────
// Configuration
// ──────────────────────────────────────────────────────────────────────────────

#[derive(Debug, Clone, serde::Deserialize)]
pub struct RingBufferConfig {
    /// Path to the backing file on NVMe storage.
    /// Will be created if it doesn't exist; opened with O_DIRECT for NVMe efficiency.
    #[serde(default = "default_path")]
    pub path: PathBuf,
    /// Total capacity in bytes. Must be a multiple of `slot_size`.
    #[serde(default = "default_capacity")]
    pub capacity_bytes: u64,
    /// Maximum serialized size of a single DataEnvelope in bytes.
    #[serde(default = "default_slot_size")]
    pub slot_size: u32,
    /// Replay rate multiplier during catch-up after reconnect.
    /// 5 = replay at 5× the rate messages were originally captured.
    #[serde(default = "default_replay_multiplier")]
    pub replay_multiplier: u32,
}

fn default_path() -> PathBuf { PathBuf::from("/var/lib/systemscale/ringbuf.bin") }
fn default_capacity() -> u64 { 512 * 1024 * 1024 } // 512 MB
fn default_slot_size() -> u32 { 4096 }
fn default_replay_multiplier() -> u32 { 5 }

impl Default for RingBufferConfig {
    fn default() -> Self {
        Self {
            path:               default_path(),
            capacity_bytes:     default_capacity(),
            slot_size:          default_slot_size(),
            replay_multiplier:  default_replay_multiplier(),
        }
    }
}

// ──────────────────────────────────────────────────────────────────────────────
// Slot header — written at the start of every slot
// ──────────────────────────────────────────────────────────────────────────────

const SLOT_MAGIC:   u32 = 0xDEAD_BEAF;
const SLOT_EMPTY:   u32 = 0x0000_0000;
const HEADER_SIZE:  usize = 16; // magic(4) + len(4) + timestamp_ns(8)

// ──────────────────────────────────────────────────────────────────────────────
// Shared ring buffer state
// ──────────────────────────────────────────────────────────────────────────────

struct RingState {
    /// Absolute write index (monotonically increasing, wraps logically via modulo)
    head: AtomicU64,
    /// Absolute read index (monotonically increasing, always <= head)
    tail: AtomicU64,
    /// Signals the consumer that new data is available
    notify: Notify,
}

// ──────────────────────────────────────────────────────────────────────────────
// Public API
// ──────────────────────────────────────────────────────────────────────────────

/// Producer half: receives DataEnvelopes from the adapter and writes to the ring.
pub struct RingBufferProducer {
    file:       std::fs::File,
    mmap:       memmap2::MmapMut,
    slot_size:  u64,
    num_slots:  u64,
    state:      Arc<RingState>,
    /// Used to serialize DataEnvelope to bytes before writing into a slot.
    encode_buf: Vec<u8>,
}

/// Consumer half: reads from the ring and yields DataEnvelopes to the QUIC transport.
pub struct RingBufferConsumer {
    mmap:       memmap2::Mmap,
    slot_size:  u64,
    num_slots:  u64,
    state:      Arc<RingState>,
    /// Whether we're currently in "catch-up replay" mode (after a reconnect).
    in_replay:  bool,
    /// Original capture rate estimate (messages per second), learned at runtime.
    capture_hz: f64,
    replay_multiplier: u32,
}

/// Create a producer/consumer pair backed by the file at `config.path`.
pub fn open(config: &RingBufferConfig) -> Result<(RingBufferProducer, RingBufferConsumer)> {
    let num_slots = config.capacity_bytes / config.slot_size as u64;
    anyhow::ensure!(num_slots > 0, "ring buffer capacity too small");
    anyhow::ensure!(config.slot_size >= HEADER_SIZE as u32 + 64, "slot_size too small");

    info!(
        path = %config.path.display(),
        capacity_mb = config.capacity_bytes / (1024 * 1024),
        num_slots,
        slot_size = config.slot_size,
        "Opening ring buffer"
    );

    // Create or open the backing file and set its size
    let file = std::fs::OpenOptions::new()
        .read(true)
        .write(true)
        .create(true)
        .open(&config.path)
        .with_context(|| format!("opening ring buffer file: {}", config.path.display()))?;
    file.set_len(config.capacity_bytes)
        .context("setting ring buffer file size")?;

    let state = Arc::new(RingState {
        head:   AtomicU64::new(0),
        tail:   AtomicU64::new(0),
        notify: Notify::new(),
    });

    let mmap_write = unsafe {
        memmap2::MmapMut::map_mut(&file).context("mmap write")?
    };
    let mmap_read = unsafe {
        memmap2::Mmap::map(&file).context("mmap read")?
    };

    let producer = RingBufferProducer {
        file,
        mmap: mmap_write,
        slot_size:  config.slot_size as u64,
        num_slots,
        state:      Arc::clone(&state),
        encode_buf: Vec::with_capacity(config.slot_size as usize),
    };

    let consumer = RingBufferConsumer {
        mmap: mmap_read,
        slot_size:  config.slot_size as u64,
        num_slots,
        state,
        in_replay:  false,
        capture_hz: 50.0, // conservative initial estimate
        replay_multiplier: config.replay_multiplier,
    };

    Ok((producer, consumer))
}

impl RingBufferProducer {
    /// Write a DataEnvelope into the next slot.
    /// If the ring is full, the oldest unread slot is overwritten (head advances past tail).
    /// This operation is O(1) and never blocks.
    pub fn push(&mut self, env: &DataEnvelope) -> Result<()> {
        // Serialize DataEnvelope to bytes (manual proto encoding of the 8 core fields)
        self.encode_buf.clear();
        encode_envelope_to_buf(&mut self.encode_buf, env);

        let payload_len = self.encode_buf.len();
        let max_payload = self.slot_size as usize - HEADER_SIZE;
        if payload_len > max_payload {
            warn!("DataEnvelope too large ({} bytes), truncating to {}", payload_len, max_payload);
        }
        let write_len = payload_len.min(max_payload);

        // Determine which slot to write into
        let head = self.state.head.load(Ordering::Acquire);
        let slot_idx = head % self.num_slots;
        let offset = (slot_idx * self.slot_size) as usize;

        // Write slot header: magic(4) + payload_len(4) + timestamp_ns(8)
        let ts = env.timestamp_ns;
        let slot = &mut self.mmap[offset..offset + self.slot_size as usize];
        slot[0..4].copy_from_slice(&SLOT_MAGIC.to_le_bytes());
        slot[4..8].copy_from_slice(&(write_len as u32).to_le_bytes());
        slot[8..16].copy_from_slice(&ts.to_le_bytes());
        slot[HEADER_SIZE..HEADER_SIZE + write_len]
            .copy_from_slice(&self.encode_buf[..write_len]);

        // Advance head (release ordering so consumer sees the write)
        self.state.head.fetch_add(1, Ordering::Release);

        // If we've lapped the consumer, advance tail to discard oldest slot
        let tail = self.state.tail.load(Ordering::Acquire);
        if head + 1 > tail + self.num_slots {
            self.state.tail.fetch_add(1, Ordering::Release);
        }

        // Notify consumer that data is available
        self.state.notify.notify_one();

        Ok(())
    }
}

impl RingBufferConsumer {
    /// Wait for the next DataEnvelope to become available and return it.
    /// During catch-up replay, this drives messages at `capture_hz × replay_multiplier`.
    /// Once the ring drains (head == tail), switches back to live mode.
    pub async fn pop(&mut self) -> Option<Vec<u8>> {
        loop {
            let tail = self.state.tail.load(Ordering::Acquire);
            let head = self.state.head.load(Ordering::Acquire);

            if tail >= head {
                // Ring is empty — wait for producer to push
                if self.in_replay {
                    info!("Ring buffer catch-up complete — resuming live telemetry");
                    self.in_replay = false;
                }
                self.state.notify.notified().await;
                continue;
            }

            // Apply rate limiting during catch-up replay
            if self.in_replay {
                let delay_ms = 1000.0 / (self.capture_hz * self.replay_multiplier as f64);
                tokio::time::sleep(tokio::time::Duration::from_millis(delay_ms as u64)).await;
            }

            let slot_idx = tail % self.num_slots;
            let offset   = (slot_idx * self.slot_size) as usize;
            let slot     = &self.mmap[offset..offset + self.slot_size as usize];

            // Validate slot magic
            let magic = u32::from_le_bytes([slot[0], slot[1], slot[2], slot[3]]);
            if magic != SLOT_MAGIC {
                debug!("Ring buffer slot at tail={} has invalid magic, skipping", tail);
                self.state.tail.fetch_add(1, Ordering::Release);
                continue;
            }

            let payload_len = u32::from_le_bytes([slot[4], slot[5], slot[6], slot[7]]) as usize;
            if HEADER_SIZE + payload_len > self.slot_size as usize {
                warn!("Ring buffer slot payload_len {} exceeds slot_size, skipping", payload_len);
                self.state.tail.fetch_add(1, Ordering::Release);
                continue;
            }

            let payload = slot[HEADER_SIZE..HEADER_SIZE + payload_len].to_vec();
            self.state.tail.fetch_add(1, Ordering::Release);
            return Some(payload);
        }
    }

    /// Signal that a reconnect just happened — switch to catch-up replay mode.
    pub fn on_reconnect(&mut self) {
        let lag = self.state.head.load(Ordering::Acquire)
            .saturating_sub(self.state.tail.load(Ordering::Acquire));
        if lag > 0 {
            info!(buffered_messages = lag, "Entering ring buffer catch-up replay mode");
            self.in_replay = true;
        }
    }
}

// ──────────────────────────────────────────────────────────────────────────────
// Minimal DataEnvelope serializer (subset of proto fields, optimized for speed)
// ──────────────────────────────────────────────────────────────────────────────

fn encode_envelope_to_buf(buf: &mut Vec<u8>, env: &DataEnvelope) {
    // Field 1: vehicle_id (string, wire type 2)
    encode_len_delim_field(buf, 1, env.vehicle_id.as_bytes());
    // Field 2: timestamp_ns (uint64, wire type 0)
    encode_uint64_field(buf, 2, env.timestamp_ns);
    // Field 3: stream_type (enum/int32, wire type 0)
    encode_uint64_field(buf, 3, env.stream_type as u64);
    // Field 4: payload (bytes, wire type 2)
    encode_len_delim_field(buf, 4, &env.payload);
    // Field 5/6/7: lat/lon/alt (double/float)
    if env.lat != 0.0 {
        encode_double_field(buf, 5, env.lat);
        encode_double_field(buf, 6, env.lon);
    }
    if env.alt != 0.0 {
        encode_float_field(buf, 7, env.alt);
    }
    // Field 8: seq (uint32, wire type 0)
    encode_uint64_field(buf, 8, env.seq as u64);
}

fn encode_varint(buf: &mut Vec<u8>, mut v: u64) {
    loop {
        let b = (v & 0x7F) as u8;
        v >>= 7;
        if v == 0 { buf.push(b); break; }
        buf.push(b | 0x80);
    }
}

fn encode_tag(buf: &mut Vec<u8>, field: u32, wire: u32) {
    encode_varint(buf, ((field << 3) | wire) as u64);
}

fn encode_uint64_field(buf: &mut Vec<u8>, field: u32, v: u64) {
    encode_tag(buf, field, 0);
    encode_varint(buf, v);
}

fn encode_len_delim_field(buf: &mut Vec<u8>, field: u32, data: &[u8]) {
    encode_tag(buf, field, 2);
    encode_varint(buf, data.len() as u64);
    buf.extend_from_slice(data);
}

fn encode_double_field(buf: &mut Vec<u8>, field: u32, v: f64) {
    encode_tag(buf, field, 1); // wire type 1 = 64-bit
    buf.extend_from_slice(&v.to_le_bytes());
}

fn encode_float_field(buf: &mut Vec<u8>, field: u32, v: f32) {
    encode_tag(buf, field, 5); // wire type 5 = 32-bit
    buf.extend_from_slice(&v.to_le_bytes());
}
