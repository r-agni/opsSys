# SystemScale Go SDK

```
go get github.com/systemscale/sdk/go
```

Zero external dependencies (stdlib only). Requires Go 1.23+.

---

## Import

```go
import "github.com/systemscale/sdk/go/systemscale"
```

---

## Reference

### `systemscale.New()`

```go
func New(cfg Config) *Client
```

Creates and starts a client. Call once at startup. Launches background goroutines immediately.

```go
ss := systemscale.New(systemscale.Config{
    VehicleID: "drone-001",
})
```

**`Config` fields**:

| Field       | Type          | Default                          | Description |
|-------------|---------------|----------------------------------|-------------|
| `VehicleID` | `string`      | `SYSTEMSCALE_VEHICLE_ID` env → `os.Hostname()` | Vehicle identifier |
| `APIBase`   | `string`      | `"http://127.0.0.1:7777"`        | Edge agent local API URL |
| `QueueSize` | `int`         | `8192`                           | Log frame queue depth |
| `Logger`    | `*slog.Logger`| `slog.Default()`                 | Structured logger for SDK internals |

---

### `(*Client).Log()`

```go
func (c *Client) Log(data map[string]any, opts ...LogOption)
```

Log a map of metrics. **Non-blocking** — returns immediately.

```go
// Basic telemetry
ss.Log(map[string]any{"altitude_m": 102.3, "battery_pct": 87})

// With location
ss.Log(map[string]any{"altitude_m": 102.3},
    systemscale.WithLocation(28.61, 77.20, 102.3))

// Sensor stream
ss.Log(map[string]any{"accel_x": 0.1, "accel_y": -0.05},
    systemscale.WithStream("sensor"))
```

**Log options**:

| Option                         | Description |
|-------------------------------|-------------|
| `WithStream(s string)`        | One of `"telemetry"` (default), `"sensor"`, `"event"`, `"log"` |
| `WithLocation(lat, lon, alt float64)` | Attach WGS84 coordinates to the frame |

---

### `(*Client).Event()`

```go
func (c *Client) Event(name string, data map[string]any, opts ...LogOption)
```

Shorthand for `Log()` with `stream="event"`. Adds `"name"` to the payload automatically.

```go
ss.Event("geofence_exit",  map[string]any{"zone": "alpha"})
ss.Event("failsafe_enter", map[string]any{"reason": "rc_loss"})
ss.Event("landing",        nil, systemscale.WithLocation(28.61, 77.20, 0))
```

---

### `(*Client).OnCommand()`

```go
func (c *Client) OnCommand(fn func(*Command) error)
```

Register a handler called for each incoming command. The handler runs in its own goroutine. If the handler returns a non-nil error or panics, a `"failed"` ACK is sent automatically.

```go
ss.OnCommand(func(cmd *systemscale.Command) error {
    switch cmd.Type {
    case "goto":
        lat := cmd.Data["lat"].(float64)
        lon := cmd.Data["lon"].(float64)
        alt := cmd.Data["alt"].(float64)
        if err := navigate(lat, lon, alt); err != nil {
            return cmd.Fail(err.Error())
        }
        return cmd.Ack()

    case "rtl":
        returnToLaunch()
        return cmd.Ack()

    case "set_speed":
        ms := cmd.Data["ms"].(float64)
        if ms > maxSpeed {
            return cmd.Reject(fmt.Sprintf("speed %.1f exceeds max %.1f", ms, maxSpeed))
        }
        setAirspeed(ms)
        return cmd.Ack()

    default:
        return cmd.Reject("unknown command type: " + cmd.Type)
    }
})
```

---

### `(*Client).NextCommand()`

```go
func (c *Client) NextCommand(d time.Duration) (*Command, bool)
```

Poll for the next command. Returns `(nil, false)` if no command arrived within `d`.

```go
// Non-blocking
if cmd, ok := ss.NextCommand(0); ok {
    handleCmd(cmd)
}

// Wait up to 50ms
if cmd, ok := ss.NextCommand(50 * time.Millisecond); ok {
    handleCmd(cmd)
}

// Main loop
for {
    cmd, ok := ss.NextCommand(time.Second)
    if !ok {
        continue
    }
    if err := execute(cmd); err != nil {
        cmd.Fail(err.Error())
    } else {
        cmd.Ack()
    }
}
```

---

### `(*Client).Stop()`

```go
func (c *Client) Stop()
```

Gracefully shuts down background goroutines. Pending log frames in the queue are not flushed — call before process exit only.

---

### `Command`

| Field      | Type           | Description |
|------------|----------------|-------------|
| `ID`       | `string`       | UUIDv7 of this command |
| `Type`     | `string`       | Command type (e.g. `"goto"`, `"rtl"`) |
| `Data`     | `map[string]any` | JSON payload from the sender |
| `Priority` | `string`       | `"normal"`, `"high"`, `"emergency"` |

| Method               | Returns | Description |
|----------------------|---------|-------------|
| `cmd.Ack()`          | `error` | Signal successful execution |
| `cmd.AckMsg(msg)`    | `error` | Signal success with a message |
| `cmd.Reject(reason)` | `error` | Refuse the command |
| `cmd.Fail(reason)`   | `error` | Report execution failure |

Calling an ACK method twice returns an error and is otherwise a no-op.

---

## Full example

```go
package main

import (
    "log/slog"
    "os"
    "time"

    "github.com/systemscale/sdk/go/systemscale"
)

func main() {
    ss := systemscale.New(systemscale.Config{
        VehicleID: os.Getenv("VEHICLE_ID"),
        Logger:    slog.Default(),
    })
    defer ss.Stop()

    // Wait for relay connectivity
    waitForRelay(ss)

    // Register command handler
    ss.OnCommand(handleCommand)

    // Telemetry loop at 10 Hz
    ticker := time.NewTicker(100 * time.Millisecond)
    defer ticker.Stop()
    for range ticker.C {
        pos := readGPS()
        batt := readBattery()
        ss.Log(
            map[string]any{
                "battery_pct": batt.Percent,
                "battery_v":   batt.Voltage,
                "speed_ms":    pos.Speed,
            },
            systemscale.WithLocation(pos.Lat, pos.Lon, pos.Alt),
        )
    }
}

func handleCommand(cmd *systemscale.Command) error {
    slog.Info("command received", "type", cmd.Type, "priority", cmd.Priority)
    switch cmd.Type {
    case "goto":
        lat := cmd.Data["lat"].(float64)
        lon := cmd.Data["lon"].(float64)
        return navigate(lat, lon, cmd.Ack)
    case "rtl":
        go returnToLaunch()
        return cmd.Ack()
    default:
        return cmd.Reject("unsupported: " + cmd.Type)
    }
}

func waitForRelay(ss *systemscale.Client) {
    // /healthz returns 200 when QUIC is connected
    for {
        // The SDK doesn't expose healthz directly; poll via stdlib
        resp, err := http.Get("http://127.0.0.1:7777/healthz")
        if err == nil && resp.StatusCode == 200 {
            slog.Info("relay connected")
            return
        }
        time.Sleep(500 * time.Millisecond)
    }
}
```

---

## Patterns

### Concurrent command execution with context

```go
ss.OnCommand(func(cmd *systemscale.Command) error {
    if cmd.Type != "survey" {
        return cmd.Reject("not a survey command")
    }

    ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
    defer cancel()

    // Run survey; report progress as events
    waypoints := planSurvey(cmd.Data)
    for i, wp := range waypoints {
        if err := flyTo(ctx, wp); err != nil {
            return cmd.Fail(fmt.Sprintf("failed at waypoint %d: %v", i, err))
        }
        ss.Event("waypoint_reached", map[string]any{"index": i, "total": len(waypoints)},
            systemscale.WithLocation(wp.Lat, wp.Lon, wp.Alt))
    }
    return cmd.Ack()
})
```

### Filtering commands by priority

```go
ss.OnCommand(func(cmd *systemscale.Command) error {
    if cmd.Priority == "emergency" {
        emergencyStop()
        return cmd.Ack("emergency stop executed")
    }
    if !autopilot.IsArmed() {
        return cmd.Reject("autopilot not armed")
    }
    return dispatch(cmd)
})
```

### Structured logging for command audit trail

```go
logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

ss.OnCommand(func(cmd *systemscale.Command) error {
    logger.Info("command received",
        "id",       cmd.ID,
        "type",     cmd.Type,
        "priority", cmd.Priority,
    )
    err := execute(cmd)
    if err != nil {
        logger.Error("command failed", "id", cmd.ID, "err", err)
        return cmd.Fail(err.Error())
    }
    logger.Info("command completed", "id", cmd.ID)
    return cmd.Ack()
})
```

---

## Thread safety

All `*Client` methods are safe to call from multiple goroutines simultaneously. The log channel and HTTP client are concurrency-safe by design.

## Error handling

`Log()` and `Event()` never return errors — they silently drop frames when the queue is full (logged at `WARN`). This is intentional: your flight control loop must never block on telemetry delivery.

`OnCommand` handler errors and ACK delivery failures are logged at `ERROR` level via the configured `slog.Logger`.
