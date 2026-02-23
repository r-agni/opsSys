# SystemScale SDK

Send vehicle telemetry and receive commands in three lines of code.

The SDK talks to the **edge agent** running on the same device (`localhost:7777`). The edge agent handles everything else: ring buffer, QUIC transport, relay routing, reconnection. Your code stays simple.

```python
import systemscale
systemscale.init()
systemscale.log({"altitude_m": 102.3, "battery_pct": 87})
```

---

## How it works

```
Your code
  → SDK (HTTP POST, non-blocking)
  → Edge agent localhost:7777
  → Ring buffer (survives 60s of comms loss)
  → QUIC → nearest relay node
  → NATS → cloud
  → Operator dashboard
```

Commands flow back the same way:

```
Cloud operator
  → NATS → relay → QUIC
  → Edge agent
  → SDK SSE stream (GET /v1/commands)
  → Your @on_command handler
  → POST /v1/commands/{id}/ack
  → back to operator
```

The SDK never knows the relay address. It only ever talks to `localhost:7777`. This means:
- Zero configuration beyond `vehicle_id`
- Works on any network topology
- Ring buffer handles comms drops transparently

---

## Languages

| Language | Install | File |
|----------|---------|------|
| Python   | `pip install systemscale` | [python/](python/) |
| Go       | `go get github.com/systemscale/sdk/go` | [go/](go/) |
| Any      | HTTP directly | [Local API reference](#local-api-reference) |

---

## Quickstart — Python

```python
import systemscale

# 1. Initialize (reads SYSTEMSCALE_VEHICLE_ID env var, falls back to hostname)
systemscale.init(vehicle_id="drone-001")

# 2. Log telemetry — non-blocking, fire-and-forget
systemscale.log({"altitude_m": 102.3, "battery_pct": 87, "airspeed_ms": 12.4},
                lat=28.61, lon=77.20, alt=102.3)

# 3. Handle commands
@systemscale.on_command
def handle(cmd):
    if cmd.type == "goto":
        navigate(cmd.data["lat"], cmd.data["lon"])
        return cmd.ack()
    return cmd.reject(f"unknown command type: {cmd.type}")
```

## Quickstart — Go

```go
package main

import "github.com/systemscale/sdk/go/systemscale"

func main() {
    ss := systemscale.New(systemscale.Config{VehicleID: "drone-001"})

    ss.Log(map[string]any{"altitude_m": 102.3, "battery_pct": 87},
           systemscale.WithLocation(28.61, 77.20, 102.3))

    ss.OnCommand(func(cmd *systemscale.Command) error {
        switch cmd.Type {
        case "goto":
            navigate(cmd.Data["lat"].(float64), cmd.Data["lon"].(float64))
            return cmd.Ack()
        default:
            return cmd.Reject("unknown command type")
        }
    })

    select {} // run forever
}
```

---

## Local API reference

The edge agent exposes a plain HTTP API on `http://127.0.0.1:7777`. Any language that can make HTTP requests can integrate without the SDK.

### POST /v1/log

Ingest a telemetry/sensor/event/log frame.

**Request body** (JSON):

```json
{
  "data":   { "altitude_m": 102.3, "battery_pct": 87 },
  "stream": "telemetry",
  "lat":    28.61,
  "lon":    77.20,
  "alt":    102.3
}
```

| Field    | Type   | Required | Description |
|----------|--------|----------|-------------|
| `data`   | object | yes      | Arbitrary key-value payload. Any JSON types. |
| `stream` | string | no       | `"telemetry"` (default), `"sensor"`, `"event"`, `"log"` |
| `lat`    | float  | no       | WGS84 latitude. Omit or pass `0` if not applicable. |
| `lon`    | float  | no       | WGS84 longitude. |
| `alt`    | float  | no       | Altitude in metres. |

**Responses**:
- `200 OK` — frame accepted and queued
- `400 Bad Request` — invalid JSON
- `503 Service Unavailable` — ring buffer full (back off and retry)

**Non-blocking contract**: The request returns as soon as the frame is written to the ring buffer. Delivery to the cloud is asynchronous. If the relay is unreachable, the ring buffer absorbs up to 60 seconds of data and replays it on reconnect.

---

### GET /v1/commands

**Server-Sent Events** stream of incoming commands.

Keep this connection open for the lifetime of your process. The edge agent pushes a new event each time a command arrives from the cloud.

**Response**: `Content-Type: text/event-stream`

Each event has this JSON payload in the `data` field:

```json
{
  "id":           "01923abc-...",
  "command_type": "goto",
  "data":         { "lat": 28.61, "lon": 77.20, "alt": 50.0 },
  "priority":     "normal"
}
```

| Field          | Type   | Description |
|----------------|--------|-------------|
| `id`           | string | UUIDv7 — use this to POST the ACK |
| `command_type` | string | Command type defined by the sender |
| `data`         | object | Arbitrary JSON payload from the sender |
| `priority`     | string | `"normal"`, `"high"`, or `"emergency"` |

The edge agent sends `: keep-alive` comments every 15 seconds to prevent proxy timeouts.

**Reconnecting**: If the connection drops (edge agent restart, network hiccup), reconnect with exponential backoff. The edge agent does not buffer commands for disconnected SSE clients — reconnecting misses commands that arrived during the gap.

---

### POST /v1/commands/{id}/ack

Acknowledge a command. **Must be called for every command received**, even if rejected.

`{id}` is the `id` field from the SSE event.

**Request body** (JSON):

```json
{
  "status":  "completed",
  "message": ""
}
```

| Field     | Type   | Values | Description |
|-----------|--------|--------|-------------|
| `status`  | string | `"completed"`, `"accepted"`, `"rejected"`, `"failed"` | Outcome of command execution |
| `message` | string | any    | Optional human-readable detail (shown in operator audit log) |

Status semantics:

| Status      | Meaning |
|-------------|---------|
| `accepted`  | Command received, execution in progress (use for long-running commands) |
| `completed` | Execution finished successfully |
| `rejected`  | Command refused (invalid state, unsupported type, etc.) |
| `failed`    | Execution started but failed |

**Responses**:
- `200 OK` — ACK accepted and queued for delivery to relay
- `503 Service Unavailable` — ACK channel full (retry immediately)

---

### GET /healthz

Returns the QUIC relay connection status.

**Responses**:
- `200 OK`, body `ok` — QUIC connected to relay, pipeline operational
- `503 Service Unavailable`, body `reconnecting` — edge agent is reconnecting

Use this at startup to wait until the pipeline is ready before sending critical data.

```python
import time, urllib.request
while True:
    try:
        urllib.request.urlopen("http://127.0.0.1:7777/healthz", timeout=1)
        break
    except Exception:
        time.sleep(0.5)
```

---

## Stream types

| Stream      | Use for | Stored in | Queryable |
|-------------|---------|-----------|-----------|
| `telemetry` | High-frequency numeric sensors (position, attitude, battery) | QuestDB hot tier | yes, SQL |
| `sensor`    | Raw sensor payloads (LiDAR, camera metadata, IMU) | QuestDB hot tier | yes |
| `event`     | Discrete occurrences (geofence exit, failsafe trigger) | NATS JetStream + QuestDB | yes |
| `log`       | Free-form text log lines | QuestDB hot tier | yes |

All streams are stored in QuestDB for 30 days, then archived to Parquet on S3 indefinitely.

---

## Configuration

The edge agent reads its configuration from `/etc/systemscale/agent.yaml` (or `SYSTEMSCALE_CONFIG` env var).

For SDK-only use (no MAVLink or ROS2 hardware), use the `custom_unix` adapter with the local API as your integration path:

```yaml
vehicle_id: drone-001

adapter: custom_unix
custom_unix:
  socket_path: /var/run/systemscale/ingest.sock

quic:
  relay_addr:    203.0.113.1:443
  relay_hostname: relay.systemscale.io
  cert_path:     /etc/systemscale/vehicle.crt
  key_path:      /etc/systemscale/vehicle.key
  ca_cert_path:  /etc/systemscale/relay-ca.crt

ring_buffer:
  path:          /var/lib/systemscale/ring.buf
  size_mb:       512
```

For the Python/Go SDK with the local HTTP API (`localhost:7777`), no additional config is needed — the HTTP API is always enabled alongside any adapter.

---

## Data formats beyond text

The platform handles more than JSON dicts. Here is what each transport path supports.

### Binary sensor payloads

Any `log()` call with `stream="sensor"` can carry arbitrary binary data. Encode it as hex in the JSON dict:

```python
import struct, systemscale

# Raw IMU packet (custom binary format)
buf = struct.pack("<fff", imu.ax, imu.ay, imu.az)
systemscale.log({"imu_raw": buf.hex(), "format": "pcm_s16le"}, stream="sensor")

# LiDAR point cloud (your serializer)
systemscale.log({"points": lidar.to_bytes().hex(), "count": len(lidar.points)}, stream="sensor")

# Thermal image
systemscale.log({"frame": thermal_bytes.hex(), "width": 320, "height": 240}, stream="sensor")
```

Max frame size: **4 MB**. Frames are stored as-is in QuestDB; decode at query time with your schema knowledge.

### Live video (H.265 / H.264 over SRT)

Video does not go through the SDK. It travels on a separate SRT UDP connection directly from the camera to the relay — mixing video into the telemetry QUIC stream would starve telemetry during congestion.

```
Camera → H.265 encode (Jetson NVENC) → SRT → relay → LiveKit SFU → WebRTC → operator browser
```

Glass-to-glass latency: 100–250ms intra-region, 200–400ms intercontinental.

To stream video, run the `systemscale-video-sender` binary alongside the edge agent. See [docs/hardware-integration.md](../docs/hardware-integration.md) for setup.

### Audio

**Intercom audio** (operator ↔ vehicle voice): built into LiveKit. When the operator joins the vehicle's LiveKit room, both sides can enable microphone tracks. No SDK integration needed.

**Acoustic sensor data** (sonar, microphone, ultrasonic): use `stream="sensor"` with Opus-encoded or raw PCM bytes:

```python
# Opus-encoded audio frame (20ms @ 48kHz)
opus_frame = encode_opus(pcm_samples)
systemscale.log({
    "codec":       "opus",
    "sample_rate": 48000,
    "channels":    1,
    "duration_ms": 20,
    "data":        opus_frame.hex(),
}, stream="sensor")
```

### CAN bus

Bridge CAN frames to the SDK using `python-can`:

```python
import can, systemscale

bus = can.interface.Bus('can0', bustype='socketcan')
systemscale.init()

for msg in bus:
    systemscale.log({
        "can_id":   hex(msg.arbitration_id),
        "can_data": msg.data.hex(),
        "dlc":      msg.dlc,
    }, stream="sensor")
```

### Format summary

| Data type | How to send | Stream | Max frame |
|-----------|------------|--------|-----------|
| Numeric telemetry | `systemscale.log({...})` | `telemetry` | 64 KB |
| Binary sensor data | `systemscale.log({"data": bytes.hex()}, stream="sensor")` | `sensor` | 4 MB |
| Events | `systemscale.event(name, data)` | `event` | 64 KB |
| Log lines | `systemscale.log({"message": "..."}, stream="log")` | `log` | 64 KB |
| Live video | `systemscale-video-sender` binary (separate process) | SRT | streaming |
| Intercom audio | LiveKit room track (no SDK call) | WebRTC | streaming |
| Acoustic sensor | `systemscale.log({"data": pcm.hex()}, stream="sensor")` | `sensor` | 4 MB |

For full format details see [docs/data-formats.md](../docs/data-formats.md).
