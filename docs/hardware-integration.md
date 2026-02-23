# SystemScale: On-Vehicle Hardware Integration

This guide covers setting up the edge agent on vehicle compute hardware, connecting it to flight controllers or sensors, and verifying the pipeline end-to-end.

---

## Supported hardware

| Platform | Architecture | Notes |
|----------|-------------|-------|
| Raspberry Pi 4 / 5 | ARM64 | Sufficient for MAVLink telemetry + SDK; no hardware video encode |
| NVIDIA Jetson Orin / Nano | ARM64 | GPU H.265 encode (NVENC) for low-latency video; ideal for video-sending vehicles |
| NVIDIA Jetson AGX Orin | ARM64 | High-throughput sensor fusion; GPU available for ML preprocessing |
| Intel NUC / x86 compute modules | x86-64 | Native binary; software video encode (x265) |
| Any Linux ARM64 SBC | ARM64 | Cross-compile via `make build-agent-arm64` |

**OS requirement:** Linux kernel 5.15+ (Ubuntu 22.04 LTS or later recommended).

No RTOS support — the edge agent is a Linux userspace process. For real-time control loops, run the autopilot on dedicated hardware (PX4/ArduPilot on a Pixhawk) and connect to the edge agent via MAVLink.

---

## Step 1 — Build the edge agent binary

### On the target hardware (easiest)

```bash
# Install Rust
curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh
source ~/.cargo/env

# Clone the repo and build
git clone https://github.com/yourorg/systemscale.git
cd systemscale
cargo build --release --manifest-path edge-agent/Cargo.toml --bin systemscale-agent
# Binary: edge-agent/target/release/systemscale-agent
```

### Cross-compile from a dev machine (faster for Jetson/Pi)

```bash
# Install the ARM64 cross-compilation toolchain (Ubuntu/Debian)
sudo apt install gcc-aarch64-linux-gnu
rustup target add aarch64-unknown-linux-gnu

# From the repo root:
make build-agent-arm64
# Binary: edge-agent/target/aarch64-unknown-linux-gnu/release/systemscale-agent

# Copy to the vehicle
scp edge-agent/target/aarch64-unknown-linux-gnu/release/systemscale-agent \
    user@vehicle-ip:/usr/local/bin/systemscale-agent
```

---

## Step 2 — Create directories and install certificates

Run on the vehicle:

```bash
sudo mkdir -p /etc/systemscale /var/lib/systemscale /var/run/systemscale

# Install the vehicle's TLS certificate (issued via step-ca — see deployment.md Step 7)
sudo cp vehicle.crt vehicle.key /etc/systemscale/
sudo cp relay-ca.crt /etc/systemscale/
sudo chmod 600 /etc/systemscale/vehicle.key

# Install the binary
sudo cp systemscale-agent /usr/local/bin/
sudo chmod 755 /usr/local/bin/systemscale-agent
```

---

## Step 3 — Configuration file

Create `/etc/systemscale/agent.yaml`. Annotated example with all fields:

```yaml
# ──────────────────────────────────────────────────────
# Vehicle identity
# ──────────────────────────────────────────────────────

# Globally unique vehicle identifier.
# MUST match the Subject CN of the vehicle's X.509 certificate.
vehicle_id: drone-001


# ──────────────────────────────────────────────────────
# Protocol adapter (choose one)
# ──────────────────────────────────────────────────────

# Options: mavlink_udp | mavlink_serial | ros2 | custom_unix
adapter: mavlink_udp

# MAVLink via UDP (required when adapter = mavlink_udp)
# mavlink-router forwards flight controller UART → UDP 14550
mavlink:
  udp_addr: "127.0.0.1:14550"

# MAVLink via serial (required when adapter = mavlink_serial)
# mavlink:
#   serial_port: /dev/ttyACM0
#   baud_rate: 57600

# Custom Unix socket (required when adapter = custom_unix)
# For Python/Go SDK use — the HTTP API (localhost:7777) works alongside any adapter.
# custom_unix:
#   socket_path: /var/run/systemscale/ingest.sock


# ──────────────────────────────────────────────────────
# QUIC transport (relay connection)
# ──────────────────────────────────────────────────────

quic:
  # IP:port of the nearest relay node. Use the Anycast IP for production.
  relay_addr: "203.0.113.1:443"
  # SNI hostname — must match the relay's TLS certificate.
  relay_hostname: "relay.yourcompany.io"
  # mTLS — vehicle presents this cert to authenticate with the relay.
  cert_path:    /etc/systemscale/vehicle.crt
  key_path:     /etc/systemscale/vehicle.key
  ca_cert_path: /etc/systemscale/relay-ca.crt


# ──────────────────────────────────────────────────────
# Ring buffer (local durability)
# ──────────────────────────────────────────────────────

ring_buffer:
  # Store on fast storage. NVMe preferred; SD card works but limits replay speed.
  path: /var/lib/systemscale/ring.buf
  # 512 MB ≈ 60s of telemetry at 1 Mbps. Increase for longer disconnection tolerance.
  size_mb: 512


# ──────────────────────────────────────────────────────
# Metrics (optional, Prometheus scrape endpoint)
# ──────────────────────────────────────────────────────

metrics:
  bind_addr: "0.0.0.0:9090"


# ──────────────────────────────────────────────────────
# Log level
# ──────────────────────────────────────────────────────

# trace | debug | info | warn | error
log_level: info
```

---

## Step 4 — Run as a systemd service

Create `/etc/systemd/system/systemscale-agent.service`:

```ini
[Unit]
Description=SystemScale Edge Agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/systemscale-agent
Environment=SYSTEMSCALE_CONFIG=/etc/systemscale/agent.yaml
Environment=RUST_LOG=info

# Always restart — ring buffer ensures no data loss across restarts
Restart=always
RestartSec=2s

# Resource limits
LimitNOFILE=65536
LimitMEMLOCK=infinity

# Security hardening
PrivateTmp=true
NoNewPrivileges=true
ProtectSystem=strict
ReadWritePaths=/var/lib/systemscale /var/run/systemscale

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable systemscale-agent
sudo systemctl start systemscale-agent

# Check status
sudo systemctl status systemscale-agent
sudo journalctl -u systemscale-agent -f
```

---

## Hardware integration paths

### MAVLink (PX4 / ArduPilot / Pixhawk)

Most UAVs use MAVLink to communicate between the flight controller and a companion computer.

**Architecture:**

```
Flight controller (UART /dev/ttyACM0 or /dev/ttyS0)
  → mavlink-router (routes UART ↔ UDP; handles multiplexing)
  → UDP 127.0.0.1:14550
  → systemscale-agent (mavlink_udp adapter)
```

**Install mavlink-router:**

```bash
# Ubuntu / Debian ARM64
sudo apt install meson ninja-build pkg-config python3
git clone https://github.com/mavlink-router/mavlink-router.git --recursive
cd mavlink-router
meson setup build . --buildtype=release
ninja -C build
sudo ninja -C build install
```

**Configure mavlink-router** (`/etc/mavlink-router/main.conf`):

```ini
[General]
TcpServerPort = 0          # disable TCP
ReportStats = false

[UartEndpoint alpha]
Device = /dev/ttyACM0      # flight controller serial port
Baud = 57600               # match your flight controller baud rate

[UdpEndpoint gcs]
Mode = Server
Address = 127.0.0.1
Port = 14550               # edge agent connects here
```

```bash
sudo systemctl enable mavlink-router
sudo systemctl start mavlink-router
```

**agent.yaml:**

```yaml
adapter: mavlink_udp
mavlink:
  udp_addr: "127.0.0.1:14550"
```

### ROS2

For vehicles using ROS2 for sensor fusion, navigation, or SLAM:

```
ROS2 nodes (DDS topics on the vehicle's ROS domain)
  → systemscale-agent ROS2 adapter (subscribes to standard topics)
  → DataEnvelope (mapped from message types)
```

The ROS2 adapter subscribes to:
- `/mavros/imu/data` → `STREAM_TYPE_SENSOR`
- `/mavros/global_position/global` → `STREAM_TYPE_TELEMETRY` (lat/lon/alt)
- `/mavros/battery` → `STREAM_TYPE_TELEMETRY`
- `/mavros/state` → `STREAM_TYPE_EVENT` (arm/disarm, mode changes)

**agent.yaml:**

```yaml
adapter: ros2
```

No additional config — the adapter uses the ROS domain ID from the `ROS_DOMAIN_ID` environment variable.

```bash
# Set the same domain ID as your ROS2 nodes
export ROS_DOMAIN_ID=0
systemctl restart systemscale-agent
```

### Python / Go SDK (custom hardware)

For sensors, microcontrollers, or any hardware not using MAVLink or ROS2 — use the local HTTP API. No adapter config needed; the HTTP API is always enabled on `localhost:7777`.

```python
import systemscale

systemscale.init(vehicle_id="drone-001")

# Log sensor data from any hardware
while True:
    temp = read_temperature_sensor()      # your hardware read
    imu  = read_imu()

    systemscale.log({
        "temperature_c": temp,
        "accel_x": imu.ax,
        "accel_y": imu.ay,
        "accel_z": imu.az,
    }, stream="sensor")

    time.sleep(0.01)  # 100 Hz
```

**agent.yaml for SDK-only use:**

```yaml
adapter: custom_unix
custom_unix:
  socket_path: /var/run/systemscale/ingest.sock
```

Or run with any other adapter — the HTTP API coexists with any adapter choice. Your Python code always talks to `localhost:7777` regardless.

### CAN bus

No native CAN bus adapter. Bridge via Python and the SDK:

```python
import can
import systemscale

bus = can.interface.Bus('can0', bustype='socketcan')
systemscale.init()

for msg in bus:
    systemscale.log({
        "can_id":  hex(msg.arbitration_id),
        "can_data": msg.data.hex(),
        "dlc":     msg.dlc,
    }, stream="sensor")
```

Requires `python-can`: `pip install python-can`

---

## Video streaming setup (optional)

If the vehicle has a camera and the operator needs live video:

```bash
# Install system dependencies (Jetson example)
sudo apt install gstreamer1.0-tools gstreamer1.0-plugins-bad \
                 gstreamer1.0-plugins-good libgstreamer1.0-dev

# Install the video sender binary
sudo cp systemscale-video-sender /usr/local/bin/
```

Create `/etc/systemscale/video.yaml`:

```yaml
vehicle_id:     drone-001
camera_device:  /dev/video0
width:          1920
height:         1080
framerate:      30
bitrate_kbps:   4000
relay_srt_addr: "203.0.113.1:9000"  # relay SRT listener port
srt_passphrase: "your-secret-key"   # must match relay config
srt_latency_ms: 500
encoder:        jetson               # jetson | x265 | x264
```

Create `/etc/systemd/system/systemscale-video.service`:

```ini
[Unit]
Description=SystemScale Video Sender
After=network-online.target systemscale-agent.service

[Service]
Type=simple
ExecStart=/usr/local/bin/systemscale-video-sender
Environment=SYSTEMSCALE_VIDEO_CONFIG=/etc/systemscale/video.yaml
Restart=always
RestartSec=5s

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl enable systemscale-video
sudo systemctl start systemscale-video
```

---

## Verification

```bash
# 1. Check edge agent connected to relay
curl http://localhost:7777/healthz
# 200 OK  →  connected to relay
# 503     →  reconnecting (normal during startup; check QUIC config if persistent)

# 2. Send a manual telemetry frame
curl -X POST http://localhost:7777/v1/log \
  -H "Content-Type: application/json" \
  -d '{"data":{"test":1},"stream":"telemetry"}'
# 200 OK  →  frame accepted

# 3. Check edge agent logs
journalctl -u systemscale-agent -n 50

# 4. Query QuestDB 10–30s later (from a machine with access to the cloud cluster)
curl "http://questdb.internal:9000/exec?query=SELECT+*+FROM+telemetry+LATEST+BY+vehicle_id"

# 5. Test command round-trip
# POST a command to command-api, watch the edge agent logs for "command received"
```

---

## Troubleshooting

| Symptom | Likely cause | Fix |
|---------|-------------|-----|
| `/healthz` returns 503 permanently | Wrong relay IP/port, or cert mismatch | Check `quic.relay_addr`, verify cert CN matches `vehicle_id` |
| No data in QuestDB | QUIC connected but ring buffer not flushing | Check `ring_buffer.path` is writable; `journalctl -u systemscale-agent` |
| MAVLink adapter: no data stream | mavlink-router not running, wrong port | `systemctl status mavlink-router`, check UDP 14550 with `ss -ulnp` |
| High CPU on edge agent | Adapter producing data faster than QUIC can send | Reduce adapter update rate; increase `ring_buffer.size_mb` as buffer |
| Video: SRT connect timeout | Wrong `relay_srt_addr` or SRT port blocked | Check relay security group: UDP 9000–9099 inbound |
| Certificate rejected by relay | Cert expired, or CN doesn't match `vehicle_id` | Re-issue cert via `step ca certificate`; verify CN |
