# SystemScale Python SDK

```
pip install systemscale
```

Zero dependencies. Requires Python 3.10+. Works on embedded Linux (Raspberry Pi, Jetson, etc.).

---

## Installation

```bash
pip install systemscale
```

Or for air-gapped vehicles, copy the wheel from the build server:

```bash
# On build server:
make sdk-python        # produces dist/sdk/python/systemscale-*.whl

# On vehicle:
pip install systemscale-0.1.0-py3-none-any.whl
```

---

## Reference

### `systemscale.init()`

```python
systemscale.init(
    vehicle_id: str | None = None,
    api_base:   str        = "http://127.0.0.1:7777",
    queue_size: int        = 8192,
) -> Client
```

Must be called once before any other SDK function. Starts background threads.

| Parameter    | Default                  | Description |
|-------------|--------------------------|-------------|
| `vehicle_id` | `SYSTEMSCALE_VEHICLE_ID` env → hostname | Vehicle identifier stamped on every log frame |
| `api_base`   | `http://127.0.0.1:7777`  | Edge agent local API URL |
| `queue_size` | `8192`                   | Log frame queue depth. Frames beyond this are dropped with a warning, not blocking the caller. |

---

### `systemscale.log()`

```python
systemscale.log(
    data:   dict,
    *,
    stream: str   = "telemetry",
    lat:    float = 0.0,
    lon:    float = 0.0,
    alt:    float = 0.0,
) -> None
```

Log a dictionary of metrics. **Non-blocking** — returns immediately regardless of network state.

```python
# Basic telemetry
systemscale.log({"altitude_m": 102.3, "battery_pct": 87, "airspeed_ms": 12.4})

# With location
systemscale.log({"altitude_m": 102.3}, lat=28.61, lon=77.20, alt=102.3)

# Sensor data
systemscale.log({"accel_x": 0.1, "accel_y": -0.05, "accel_z": 9.81}, stream="sensor")

# Log message (free-form text is fine in a dict)
systemscale.log({"message": "autopilot armed", "mode": "GUIDED"}, stream="log")
```

**Backpressure**: If the background queue is full (edge agent overwhelmed), frames are dropped with a `WARNING` log. The caller is never blocked. Queue capacity is controlled by `init(queue_size=...)`.

---

### `systemscale.event()`

```python
systemscale.event(
    name:     str,
    *,
    data:     dict | None = None,
    severity: str         = "info",
    lat:      float       = 0.0,
    lon:      float       = 0.0,
    alt:      float       = 0.0,
) -> None
```

Shorthand for `log()` with `stream="event"`. Adds `name` and `severity` to the payload automatically.

```python
systemscale.event("geofence_exit",  data={"zone": "alpha"},         severity="warning")
systemscale.event("failsafe_enter", data={"reason": "rc_loss"},     severity="critical")
systemscale.event("mission_start",  data={"waypoints": 12},         severity="info")
systemscale.event("landing",        data={"surface": "unprepared"},  lat=28.61, lon=77.20)
```

---

### `@systemscale.on_command`

```python
@systemscale.on_command
def handle(cmd: Command) -> None:
    ...
```

Decorator — registers `handle` as the command handler. Called in a **background thread** for each incoming command.

The handler **must** call exactly one of `cmd.ack()`, `cmd.reject(reason)`, or `cmd.fail(reason)` before returning. If it returns without calling any, a `"failed"` ACK is sent automatically with a warning logged.

```python
@systemscale.on_command
def handle(cmd):
    match cmd.type:
        case "goto":
            lat = cmd.data["lat"]
            lon = cmd.data["lon"]
            alt = cmd.data.get("alt", 50.0)
            navigate(lat, lon, alt)
            cmd.ack()

        case "rtl":
            return_to_launch()
            cmd.ack(f"RTL initiated")

        case "land":
            land_now()
            cmd.ack()

        case "set_speed":
            speed = cmd.data["ms"]
            if speed > MAX_SPEED:
                cmd.reject(f"speed {speed} exceeds maximum {MAX_SPEED}")
            else:
                set_airspeed(speed)
                cmd.ack()

        case _:
            cmd.reject(f"unknown command type: {cmd.type!r}")
```

---

### `systemscale.next_command()`

```python
systemscale.next_command(timeout: float = 0.0) -> Command | None
```

Poll for the next command (alternative to `@on_command`). Use this when you want to handle commands in your main loop.

```python
# Non-blocking poll
cmd = systemscale.next_command()
if cmd:
    handle(cmd)

# Wait up to 50ms
cmd = systemscale.next_command(timeout=0.05)
if cmd:
    handle(cmd)

# Blocking loop
while True:
    cmd = systemscale.next_command(timeout=1.0)
    if cmd:
        try:
            result = execute(cmd)
            cmd.ack(str(result))
        except ValueError as e:
            cmd.reject(str(e))
        except Exception as e:
            cmd.fail(str(e))
```

---

### `Command`

Object passed to the `@on_command` handler or returned by `next_command()`.

| Attribute  | Type   | Description |
|------------|--------|-------------|
| `cmd.id`       | `str`  | UUIDv7 of this command |
| `cmd.type`     | `str`  | Command type (e.g. `"goto"`, `"rtl"`, `"set_speed"`) |
| `cmd.data`     | `dict` | Arbitrary JSON payload from the sender |
| `cmd.priority` | `str`  | `"normal"`, `"high"`, or `"emergency"` |

| Method             | Description |
|--------------------|-------------|
| `cmd.ack(msg="")` | Signal successful execution |
| `cmd.reject(msg)` | Refuse the command (invalid state, unsupported type) |
| `cmd.fail(msg)`   | Execution started but failed |

Calling more than one ACK method on the same command logs a warning and is a no-op.

---

## Patterns

### High-frequency sensor loop

```python
import systemscale
import time

systemscale.init()

while True:
    imu = read_imu()      # your hardware read
    gps = read_gps()

    systemscale.log(
        {
            "roll_deg":  imu.roll,
            "pitch_deg": imu.pitch,
            "yaw_deg":   imu.yaw,
            "accel_x":   imu.ax,
            "accel_y":   imu.ay,
            "accel_z":   imu.az,
        },
        stream="sensor",
        lat=gps.lat, lon=gps.lon, alt=gps.alt,
    )
    time.sleep(0.02)  # 50 Hz
```

The SDK's background queue handles rate differences. At 50 Hz with `queue_size=8192`, you have ~160 seconds of buffering before frames are dropped.

### Long-running command with progress updates

```python
@systemscale.on_command
def handle(cmd):
    if cmd.type != "survey_area":
        return cmd.reject("unknown")

    waypoints = plan_survey(cmd.data)

    for i, wp in enumerate(waypoints):
        fly_to(wp)
        systemscale.event("waypoint_reached", data={
            "waypoint": i + 1,
            "total":    len(waypoints),
            "lat":      wp.lat,
            "lon":      wp.lon,
        })

    cmd.ack(f"survey complete, {len(waypoints)} waypoints")
```

### Priority-aware command handling

```python
@systemscale.on_command
def handle(cmd):
    if cmd.priority == "emergency":
        # Drop everything, execute immediately
        emergency_stop()
        cmd.ack("emergency stop executed")
        return

    if not autopilot.is_ready():
        cmd.reject("autopilot not ready")
        return

    # Normal dispatch
    dispatch(cmd)
```

### Waiting for relay connectivity before critical ops

```python
import systemscale
import urllib.request
import time

def wait_for_relay(timeout_s=30):
    deadline = time.monotonic() + timeout_s
    while time.monotonic() < deadline:
        try:
            urllib.request.urlopen("http://127.0.0.1:7777/healthz", timeout=1)
            return True
        except Exception:
            time.sleep(0.5)
    return False

systemscale.init()
if not wait_for_relay():
    raise RuntimeError("relay not reachable after 30s")

systemscale.log({"status": "mission_start"})
```

---

## Logging configuration

The SDK uses Python's standard `logging` module under the `systemscale` logger name.

```python
import logging
logging.getLogger("systemscale").setLevel(logging.DEBUG)
```

Log levels used:

| Level     | Events |
|-----------|--------|
| `INFO`    | SDK started, SSE connected |
| `WARNING` | Queue full, command handler missing ACK, SSE reconnect |
| `ERROR`   | Failed to deliver ACK |
| `DEBUG`   | Per-frame POST status, SSE reconnect attempts |
