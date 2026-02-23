# SystemScale: Data Formats

The platform handles numeric telemetry, binary sensor payloads, discrete events, free-form logs, live video, and audio. This document explains what each format is, how it travels through the system, and where it ends up.

---

## The data envelope

All non-video data travels in a `DataEnvelope` proto frame over QUIC. The `payload` field is opaque bytes — the platform routes and stores it without interpreting it.

```protobuf
message DataEnvelope {
  string vehicle_id   = 1;  // Globally unique vehicle ID
  uint64 timestamp_ns = 2;  // Nanosecond UTC epoch (vehicle clock)
  StreamType type     = 3;  // What kind of data this is
  bytes payload       = 4;  // Opaque bytes — format determined by StreamType
  float lat           = 5;  // WGS84 latitude (0 if not applicable)
  float lon           = 6;  // WGS84 longitude
  float alt_m         = 7;  // Altitude in metres
  uint32 seq          = 8;  // Monotonic sequence number (gap detection)
}
```

The relay forwards envelopes based on `StreamType` and `vehicle_id`. Consumers (QuestDB ingest, event processor, operator dashboard) decode `payload` using their own schema knowledge.

---

## Stream types

```protobuf
enum StreamType {
  STREAM_TYPE_UNSPECIFIED = 0;
  STREAM_TYPE_TELEMETRY   = 1;  // Numeric sensor/state data
  STREAM_TYPE_EVENT       = 2;  // Discrete occurrences
  STREAM_TYPE_SENSOR      = 3;  // High-bandwidth binary sensor data
  STREAM_TYPE_VIDEO_META  = 4;  // SRT session metadata (not the video bitstream)
  STREAM_TYPE_LOG         = 5;  // Free-form text log lines
  STREAM_TYPE_AUDIO       = 6;  // Raw audio frames: PCM or Opus-encoded bytes
}
```

---

## Telemetry data (numeric, structured)

**Use for:** position, attitude, battery, airspeed, RPM, temperature — any numeric state that changes continuously.

**`stream = "telemetry"` (default for SDK `log()` calls)**

```python
systemscale.log({
    "altitude_m":   102.3,
    "battery_pct":  87,
    "airspeed_ms":  12.4,
    "roll_deg":     2.1,
    "pitch_deg":    -0.3,
    "yaw_deg":      145.7,
}, lat=28.61, lon=77.20, alt=102.3)
```

- **Payload format:** JSON dict (SDK path) or serialized `TelemetryFrame` proto (MAVLink adapter path)
- **Max rate:** up to 500 Hz per vehicle sustained
- **Max frame size:** 64 KB
- **Storage:** QuestDB hot tier, 30-day retention
- **Queryable:** yes, via SQL (`SELECT * FROM telemetry WHERE vehicle_id = 'drone-001'`)

QuestDB stores each key as a dedicated float column for fast aggregation and filtering. The raw JSON payload is also stored for full fidelity.

---

## Binary sensor data (LiDAR, radar, thermal, IMU)

**Use for:** raw sensor payloads where the data is binary, large, or vendor-specific.

**`stream = "sensor"`**

```python
# Raw LiDAR point cloud (binary)
systemscale.log({"points": lidar.serialize()}, stream="sensor")

# Thermal image (FLIR raw bytes)
systemscale.log({"frame": thermal.raw_bytes.hex()}, stream="sensor")

# High-frequency IMU (custom binary format)
buf = struct.pack("<fff", imu.ax, imu.ay, imu.az)
systemscale.log({"imu_raw": buf.hex()}, stream="sensor")
```

- **Payload format:** opaque bytes — any binary format, vendor-specific, compressed, raw
- **Max frame size:** 4 MB (enforced by QUIC framing)
- **Max rate:** 100 Hz per vehicle
- **Storage:** QuestDB `payload` column as raw bytes (not decoded)
- **Queryable:** yes (metadata fields: `vehicle_id`, `timestamp`, `lat`/`lon`/`alt`), payload opaque

**For frames larger than 4 MB** (dense LiDAR at high resolution, full-resolution thermal video): split into chunks at the application layer and reassemble at the consumer. The platform guarantees in-order delivery within a vehicle's QUIC stream.

**Example: 10 Hz LiDAR at 300 KB/scan:**
- 10 × 300 KB = 3 MB/s per vehicle — within the 4 MB per-frame limit
- At 1000 vehicles: 3 GB/s total — size your QuestDB and network accordingly

---

## Events (discrete, timestamped)

**Use for:** discrete occurrences — arming, mode changes, geofence triggers, mission waypoints, anomalies.

**`stream = "event"` — use `systemscale.event()` shorthand**

```python
systemscale.event("geofence_exit",  data={"zone": "alpha"},          severity="warning")
systemscale.event("failsafe_enter", data={"reason": "rc_loss"},      severity="critical")
systemscale.event("mission_start",  data={"waypoints": 12},          severity="info")
systemscale.event("landing",        data={"surface": "unprepared"},  lat=28.61, lon=77.20)
```

- **Payload format:** JSON `{"name": "...", "severity": "...", ...arbitrary fields}`
- **Max rate:** burst (not rate-limited, but not intended for >10 Hz)
- **Max frame size:** 64 KB
- **Storage:** QuestDB (30-day) + NATS JetStream (7-day, for alerting consumers)
- **Queryable:** yes, via SQL

Events are stored in both tiers so that real-time alerting consumers (e.g., geofence monitoring) can subscribe to JetStream and receive events with guaranteed delivery, while historical analysis uses QuestDB.

---

## Log lines (free-form text)

**Use for:** debug output, status messages, anything that would normally go to stdout/stderr on the vehicle.

**`stream = "log"`**

```python
systemscale.log({"message": "autopilot armed", "mode": "GUIDED"}, stream="log")
systemscale.log({"message": "GPS lock acquired", "satellites": 12}, stream="log")
```

- **Payload format:** JSON dict with a `message` key (convention, not enforced)
- **Max rate:** burst
- **Storage:** QuestDB hot tier, 30-day retention
- **Queryable:** yes

---

## Live video (H.265 over SRT)

Video does **not** travel in `DataEnvelope`. It uses a dedicated SRT path alongside the QUIC telemetry connection.

**Why separate:** Video is gigabytes per hour. Mixing it with telemetry in the QUIC connection would cause head-of-line blocking during link congestion. SRT has its own congestion control tuned for video.

### Pipeline

```
Camera (v4l2)
  → H.265 encode (Jetson NVENC hardware: ~50ms; or x265 software: ~150-200ms)
  → SRT sender (UDP, AES-256 encryption)
  → Relay SRT listener (UDP 9000–9099)
      → NVMe ring buffer (60s instant replay cache)
      → SRT forward → LiveKit Ingress (private backbone)
  → LiveKit SFU
      → Simulcast layers (2 Mbps / 500 Kbps / 150 Kbps)
      → Adaptive bitrate per operator connection
      → WebRTC to operator browser
  → LiveKit Egress → S3 MP4 (mission recording, 90-day archive)
```

### Latency

| Path | Glass-to-glass |
|------|---------------|
| Intra-region (vehicle + operator in same city) | 100–150ms |
| Cross-region (e.g., India vehicle → EU operator) | 200–400ms |
| Physics minimum (170ms RTT floor on intercontinental) | ~210ms |

### Configuration

In `/etc/systemscale/video.yaml` (on the vehicle — see [hardware-integration.md](hardware-integration.md)):

```yaml
vehicle_id:     drone-001
camera_device:  /dev/video0
relay_srt_addr: "203.0.113.1:9000"
srt_passphrase: "your-aes-key"
bitrate_kbps:   4000
encoder:        jetson    # jetson | x265 | x264
```

### Operator access

```bash
# Get a LiveKit room token
curl -X POST https://api.yourcompany.io/v1/video/token?vehicle_id=drone-001 \
  -H "Authorization: Bearer $OPERATOR_TOKEN"
# Response: { "token": "...", "room": "drone-001" }

# Use the LiveKit Web SDK in the operator browser with this token
```

---

## Audio

### Intercom audio (operator ↔ vehicle, bidirectional voice)

LiveKit natively supports bidirectional audio WebRTC tracks alongside the video stream. No additional configuration required — when an operator joins a vehicle's LiveKit room, they can enable a microphone track.

This is the recommended path for operator-to-vehicle voice communication. Codec: Opus 32 Kbps. Latency follows the same path as video (100–400ms depending on region).

Audio intercom is **not recorded** by default. To record, enable LiveKit Egress with an audio-only track configuration.

### Acoustic sensor data (microphone, sonar, ultrasonic)

For sensors that produce audio data (acoustic anomaly detection, sonar, ultrasonic range), use `stream = "sensor"` with raw PCM or Opus-encoded bytes:

```python
import systemscale

systemscale.init()

# Opus-encoded audio frame (20ms @ 48kHz)
opus_frame = encode_opus(pcm_samples)   # your encoder
systemscale.log({
    "codec":       "opus",
    "sample_rate": 48000,
    "channels":    1,
    "duration_ms": 20,
    "data":        opus_frame.hex(),
}, stream="sensor")

# Raw PCM (uncompressed, larger frames)
systemscale.log({
    "codec":       "pcm_s16le",
    "sample_rate": 16000,
    "channels":    1,
    "data":        pcm_samples.hex(),
}, stream="sensor")
```

This stores audio as opaque bytes in QuestDB, queryable by timestamp and vehicle. Consumers decode it with the codec metadata in the JSON payload.

### Future: STREAM_TYPE_AUDIO

`STREAM_TYPE_AUDIO = 6` is defined in `proto/core/common.proto` for future use. When a dedicated audio codec path is implemented, audio sensor data will be routable separately from binary sensor data without a protocol version bump.

---

## CAN bus

No native CAN bus adapter. Bridge using Python and the SDK:

```python
import can, systemscale, struct

bus = can.interface.Bus('can0', bustype='socketcan')
systemscale.init()

for msg in bus:
    systemscale.log({
        "can_id":   hex(msg.arbitration_id),
        "can_data": msg.data.hex(),
        "dlc":      msg.dlc,
    }, stream="sensor")
```

For typed CAN data (e.g., motor controller telemetry on known CAN IDs), decode the frame before logging:

```python
MOTOR_TELEMETRY_ID = 0x301

for msg in bus:
    if msg.arbitration_id == MOTOR_TELEMETRY_ID:
        rpm, temp_c = struct.unpack("<Hh", msg.data[:4])
        systemscale.log({
            "motor_rpm":    rpm,
            "motor_temp_c": temp_c / 10.0,
        }, stream="telemetry")
```

---

## Data format summary

| Data type | Stream | Transport | Max rate | Max frame | Storage | Retention |
|-----------|--------|-----------|----------|-----------|---------|-----------|
| Numeric telemetry | `telemetry` | DataEnvelope → QUIC | 500 Hz/vehicle | 64 KB | QuestDB | 30 days |
| Binary sensor | `sensor` | DataEnvelope → QUIC | 100 Hz/vehicle | 4 MB | QuestDB (raw bytes) | 30 days |
| Events | `event` | DataEnvelope → QUIC | burst | 64 KB | QuestDB + JetStream | 30d / 7d |
| Log lines | `log` | DataEnvelope → QUIC | burst | 64 KB | QuestDB | 30 days |
| H.265 video | — | SRT (separate UDP) | 1–8 Mbps/stream | streaming | S3 MP4 | 90 days |
| Intercom audio | — | LiveKit WebRTC | 32 Kbps Opus | streaming | not recorded* | — |
| Acoustic sensor | `sensor` | DataEnvelope → QUIC | limited | 4 MB | QuestDB (raw bytes) | 30 days |
| CAN bus | `sensor` or `telemetry` | DataEnvelope → QUIC | bus-dependent | 64 KB | QuestDB | 30 days |

*Enable LiveKit Egress to record intercom audio to S3.

**Cold archive:** After 30 days, QuestDB data is exported to Parquet on S3 (indefinite retention). Query via DuckDB SQL over S3.

---

## Protocol buffer field numbers

Field numbers 1–15 use 1-byte tags (faster serialization). The most frequently updated fields occupy these positions in `DataEnvelope`:

| Field | Number | Notes |
|-------|--------|-------|
| `vehicle_id` | 1 | String — short IDs (< 16 chars) keep frames compact |
| `timestamp_ns` | 2 | uint64, nanosecond epoch |
| `type` | 3 | StreamType enum |
| `payload` | 4 | Opaque bytes |
| `lat` | 5 | float32 WGS84 |
| `lon` | 6 | float32 WGS84 |
| `alt_m` | 7 | float32 metres |
| `seq` | 8 | uint32 monotonic sequence |

**Never renumber these fields after first deployment.** Protobuf field numbers are part of the wire format and changing them breaks all existing decoders.
