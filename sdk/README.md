# SystemScale SDK

Send service telemetry and receive commands in three lines of code.

```python
client = systemscale.init(api_key="ssk_live_...", project="my-fleet", service="drone-001")
client.log({"nav/altitude": 102.3, "battery/pct": 87})
```

---

## Contents

- [How it works](#how-it-works)
- [Installation](#installation)
- [Continuous vs discrete data](#continuous-vs-discrete-data)
- [Python SDK — Service side](#python-sdk--service-side)
  - [init()](#init)
  - [Module-level shortcuts](#module-level-shortcuts)
  - [Client methods](#client-methods)
    - [log()](#clientlog)
    - [alert()](#clientalert)
    - [on_command / next_command / listen](#commands)
    - [request_assistance()](#clientrequest_assistance)
    - [stop()](#clientstop)
  - [Sub-module access](#sub-module-access)
- [Python SDK — Operator side](#python-sdk--operator-side)
  - [connect()](#connect)
  - [OperatorClient methods](#operatorclient-methods)
    - [services()](#opservices)
    - [query()](#opquery)
    - [subscribe()](#opsubscribe)
    - [send_command()](#opsend_command)
    - [assistance_requests()](#opassistance_requests)
  - [Return types](#python-return-types)
- [Go SDK — Service side](#go-sdk--service-side)
  - [Init()](#init-1)
  - [Client methods](#go-client-methods)
  - [Log options](#log-options)
  - [Alert options](#alert-options)
  - [Config struct](#config-struct)
- [Go SDK — Operator side](#go-sdk--operator-side)
  - [Connect()](#connect-1)
  - [OperatorClient methods](#go-operatorclient-methods)
  - [OperatorConfig struct](#operatorconfig-struct)
  - [Return types](#go-return-types)
- [Standalone sub-packages (Python)](#standalone-sub-packages-python)
- [Standalone sub-packages (Go)](#standalone-sub-packages-go)
- [Agentless mode](#agentless-mode)
- [Self-hosting](#self-hosting)
- [Environment variables](#environment-variables)
- [Edge agent local API](#edge-agent-local-api)
- [Stream types](#stream-types)
- [Data formats](#data-formats)

---

## How it works

```
Your service code
  → SDK  (HTTP POST to localhost:7777, non-blocking)
  → Edge agent  (ring buffer, sequence numbers, hardware adapters)
  → QUIC  (resilient UDP transport)
  → Relay node
  → NATS  (cloud message bus)
  → Cloud APIs  (fleet, query, command)
  → Operator dashboard / SDK
```

The **edge agent** (`systemscale-agent`) is a compiled Rust binary that runs as a background process on each device. The SDK only talks to it over HTTP on `localhost:7777`. This means:

- No cloud credentials on the device — the agent holds all TLS certs.
- Works behind NAT, cellular, satellite — any network topology.
- The ring buffer absorbs up to 60 seconds of comms loss and replays on reconnect.

If no agent binary is found, the SDK enters **agentless mode** automatically. See [Agentless mode](#agentless-mode).

---

## Installation

**Python** (service or operator side):
```bash
pip install systemscale
```

**Go** (service side):
```bash
go get github.com/systemscale/sdk/go/systemscale
```

**Edge agent binary** (service side, optional but recommended):

Download the pre-built binary from **Dashboard → Settings → Devices → Download agent**, then:
```bash
sudo install -m 755 systemscale-agent /usr/local/bin/systemscale-agent
```

The SDK auto-detects the binary. If found and not yet running, the SDK provisions the service and starts the agent automatically.

---

## Continuous vs discrete data

The SDK handles two fundamentally different data modes:

### Continuous (time-series) — `client.log()`

High-frequency key-value pairs streamed through a non-blocking queue to the edge agent, which writes them to QuestDB for later querying.

```python
# Non-blocking — can be called at 50 Hz+ without slowing your loop
client.log({"nav/altitude": 102.3, "battery/pct": 87, "gps/fix": 3})
```

- **Delivery**: queued → background thread → agent → QuestDB
- **Query later**: `ops.query(service="drone-001", keys=["nav/altitude"], start="-1h")`
- **Real-time view**: `ops.subscribe(service="drone-001")` streams frames as they arrive

### Discrete (event-driven) — `alert()` and `on_command`

Immediate events that need attention now, not archival later.

**`client.alert()`** — fire-and-forget event (battery critical, geofence breach, obstacle detected):
```python
client.alert("Low battery", level="warning", data={"pct": 12})
```
Delivered immediately via HTTP POST. Appears in the dashboard alert feed in real time.

**`@client.on_command`** — receive instructions from operators:
```python
@client.on_command
def handle(cmd):
    # Fires when an operator calls ops.send_command("drone-001", cmd_type="goto", ...)
    # from the dashboard or their own code.
    if cmd.type == "goto":
        navigate(cmd.data["lat"], cmd.data["lon"])
        cmd.ack()          # tells the operator send_command() call it completed
    else:
        cmd.reject("unknown command")
```

The operator's `ops.send_command()` **blocks** until your service calls `cmd.ack()`, `cmd.reject()`, or `cmd.fail()`. This creates a synchronous request/response flow between operator and service, even over unreliable links.

---

## Python SDK — Service side

### `init()`

```python
import systemscale

client = systemscale.init(
    api_key  = "ssk_live_...",         # required — from Dashboard → API Keys
    project  = "my-fleet",             # required — project slug
    service  = "drone-001",            # optional — defaults to $SYSTEMSCALE_SERVICE or hostname
    location = (28.61, 77.20, 102.3),  # optional — default (lat, lon, alt_m) for every log() call
    queue_size = 8192,                 # optional — in-process log queue depth
)
```

Returns the `Client` singleton. Also stores it internally so [module-level shortcuts](#module-level-shortcuts) work without passing the client around.

`init()` is non-blocking. It probes the local agent, provisions if needed (binary auto-detection), and starts background threads. If no agent binary is found, it enters agentless mode automatically.

Server URLs resolve in this order: explicit `init()` kwarg → `$SYSTEMSCALE_*` env var → hosted default. Since the defaults cover 100% of cases, most users never touch them. See [Self-hosting](#self-hosting) and [Environment variables](#environment-variables).

---

### Module-level shortcuts

After calling `init()`, all `Client` methods are available at the module level:

```python
systemscale.init(api_key="...", project="my-fleet", service="drone-001")

systemscale.log({"altitude": 102.3})            # → client.log(...)
systemscale.alert("Low battery")                # → client.alert(...)
systemscale.next_command(timeout=0.5)           # → client.next_command(...)

@systemscale.on_command
def handle(cmd):
    cmd.ack()
```

These call `_require_client()` internally — raises `RuntimeError` if `init()` has not been called.

---

### Client methods

#### `client.log()`

Log a dict of key-value pairs. **Non-blocking** — enqueues and returns immediately.

```python
# Basic
client.log({"nav/altitude": 102.3, "battery/pct": 87})

# Uses the default location set in init(location=...) automatically
client.log({"sensors/temp": 85.2})

# Override location for this frame
client.log({"sensors/temp": 85.2}, lat=28.61, lon=77.20, alt=102.3)

# With stream type and sub-label
client.log({"points": lidar.hex()}, stream="sensor", stream_name="lidar_scan")

# With low-cardinality tags (stored as indexed SYMBOL columns in QuestDB)
client.log({"cpu_pct": 74}, tags={"region": "south", "mode": "auto"})

# Route to a specific actor instead of broadcasting
client.log({"video/fps": 30}, to="operator:ground-control")
client.log({"nav/alt": 102.3}, to="operator:*")       # all operators
client.log({"state": "hover"}, to="device:drone-002")  # peer device

# Nested dicts are flattened automatically
client.log({"sensors": {"temp": 85.2, "pressure": 1013}})
# Stored as: sensors/temp=85.2, sensors/pressure=1013
```

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `data` | `dict` | required | Key-value payload. Nested dicts flattened with `/` separator. Any JSON-serialisable values. |
| `stream` | `str` | `"telemetry"` | `"telemetry"`, `"sensor"`, `"event"`, or `"log"` |
| `stream_name` | `str` | `""` | Sub-label within the stream (e.g. `"lidar_scan"`, `"imu_raw"`) |
| `lat` | `float` | `0.0` | WGS84 latitude. Falls back to `init(location=...)` if `0.0`. |
| `lon` | `float` | `0.0` | WGS84 longitude. Falls back to `init(location=...)` if `0.0`. |
| `alt` | `float` | `0.0` | Altitude in metres. Falls back to `init(location=...)` if `0.0`. |
| `tags` | `dict` | `None` | Low-cardinality string labels for dashboard filtering |
| `to` | `str` | `None` | Target actor `"type:id"`. Types: `"operator"`, `"device"`, `"human"`, `"service"`. Use `"*"` as id for all of that type. |

Frames are **dropped with a warning** (never blocking) when the internal queue is full (`queue_size`, default 8192).

---

#### `client.alert()`

Send a discrete alert event. **Non-blocking**.

```python
client.alert("Low battery", level="warning", data={"pct": 12})
client.alert("Geofence breach", level="error", data={"lat": 28.61, "lon": 77.20})
client.alert("Mission complete", level="info")
client.alert("Engine failure", level="critical", to="operator:ground-control")
```

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `message` | `str` | required | Human-readable alert text shown in the dashboard |
| `level` | `str` | `"info"` | `"info"`, `"warning"`, `"error"`, `"critical"` |
| `data` | `dict` | `None` | Additional metadata attached to the alert |
| `to` | `str` | `None` | Target actor (same format as `log`) |

---

#### Commands

Commands are operator instructions sent to your service via `ops.send_command()` or the dashboard. Use `on_command` to handle them.

**Callback style** (recommended — runs each command handler in its own thread):

```python
@client.on_command
def handle(cmd):
    """
    Fires when an operator calls:
      ops.send_command("drone-001", cmd_type="goto", data={"lat": 28.6, "lon": 77.2})

    The operator's call blocks until this handler calls ack/reject/fail.
    """
    if cmd.type == "goto":
        navigate(cmd.data["lat"], cmd.data["lon"])
        cmd.ack()                        # signals success back to the operator
    elif cmd.type == "land":
        land()
        cmd.ack("landed successfully")   # optional message
    else:
        cmd.reject(f"unknown command type: {cmd.type}")

# Imperative form (same effect, no decorator syntax):
client.set_command_handler(handle)
```

**Poll style** (for existing event loops):

```python
# Block up to 0.1 s, return None on timeout
cmd = client.next_command(timeout=0.1)
if cmd:
    process(cmd)
```

**`Command` object attributes:**

| Attribute | Type | Description |
|-----------|------|-------------|
| `cmd.id` | `str` | UUID used for ACK routing |
| `cmd.type` | `str` | Command type string set by the operator (e.g. `"goto"`) |
| `cmd.data` | `dict` | Arbitrary JSON payload from the operator |
| `cmd.priority` | `str` | `"normal"`, `"high"`, or `"emergency"` |

**ACK methods** — call **exactly one** per command:

| Method | Meaning |
|--------|---------|
| `cmd.ack()` | Execution completed successfully |
| `cmd.ack(message="Done")` | Completion with an info message |
| `cmd.reject(reason)` | Command refused (invalid state, unsupported type) |
| `cmd.fail(reason)` | Execution started but failed |

If a handler returns without calling any ACK method, a `"failed"` ACK is sent automatically with a warning logged.

---

#### `client.request_assistance()`

Block the calling thread until a human operator responds. Use for mandatory human-in-the-loop authorisation.

```python
try:
    resp = client.request_assistance(
        "Obstacle in path — proceed?",
        data={"obstacle_type": "vehicle", "distance_m": 4.2},
        timeout=120,   # seconds; None = wait forever
    )
    if resp.approved:
        proceed(resp.instruction)   # resp.instruction is the operator's text reply
    else:
        hold()
except TimeoutError:
    emergency_land()
```

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `reason` | `str` | required | Shown to the operator in the dashboard |
| `data` | `dict` | `None` | Additional context (location, sensor readings, etc.) |
| `timeout` | `float` | `None` | Seconds to wait. `None` = wait forever. |

Raises `TimeoutError` if no operator responds within the timeout.

**`AssistanceResponse` attributes:**

| Attribute | Type | Description |
|-----------|------|-------------|
| `resp.approved` | `bool` | `True` if the operator approved |
| `resp.instruction` | `str` | Optional text from the operator |
| `resp.request_id` | `str` | UUID of this request |

---

#### `client.stop()`

Gracefully flush the queue and stop background threads. Call before process exit if you need a clean shutdown.

```python
client.stop()
```

---

### Sub-module access

The `Client` exposes its internals as attributes:

```python
client.stream   # StreamEmitter — the telemetry queue
client.message  # MessageSender — discrete alerts/events
client.inbox    # MessageReceiver — incoming commands
```

#### `client.stream` — StreamEmitter

```python
client.stream.send({"nav/alt": 102.3}, stream="telemetry", to="operator:*")
```

`client.log(...)` is a thin wrapper around `client.stream.send(...)` with default-location injection.

#### `client.message` — MessageSender

```python
client.message.send("Low battery", type="alert", level="warning", data={"pct": 12})
```

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `content` | `str` | required | Alert text (service mode) or command type string (operator mode) |
| `to` | `str` | `None` | Target actor `"type:id"` |
| `type` | `str` | `"alert"` | `"alert"`, `"event"`, or `"command"` |
| `data` | `dict` | `None` | Extra payload |
| `level` | `str` | `"info"` | Alert severity (service mode only) |
| `priority` | `str` | `"normal"` | Command priority (operator mode only) |
| `timeout` | `float` | `30.0` | Seconds to wait for command ACK (operator mode only) |

Returns `CommandResult` when `type="command"`, `None` for alerts/events.

#### `client.inbox` — MessageReceiver

| Method | Description |
|--------|-------------|
| `inbox.on_command(fn)` | Register `fn` as the command handler |
| `inbox.next_command(timeout=0.0)` | Block up to `timeout` seconds and return next `Command`, or `None`. `0` = block forever; negative = non-blocking. |
| `inbox.listen()` | Generator that yields `Command` objects as they arrive |
| `inbox.start()` | Start the background SSE thread (called automatically by `Client.start()`) |
| `inbox.stop()` | Stop the background thread |

---

## Python SDK — Operator side

### `connect()`

```python
import systemscale

ops = systemscale.connect(
    api_key     = "ssk_live_...",
    project     = "my-fleet",
    fleet_api   = "https://api.systemscale.io",    # default
    apikey_url  = "https://keys.systemscale.io",   # default
    ws_url      = "wss://ws.systemscale.io",        # default
    command_api = "https://cmd.systemscale.io",     # default
)
```

Returns an `OperatorClient`. All URL parameters are optional and default to the hosted service.

---

### OperatorClient methods

#### `ops.services()`

List all services (registered devices) in the project.

```python
services = ops.services()
# Returns: list of dicts
# [{"id": "...", "display_name": "drone-001", "status": "online", ...}, ...]
for s in services:
    print(s["display_name"], s["status"])
```

---

#### `ops.query()`

Query historical telemetry from QuestDB.

```python
rows = ops.query(
    service   = "drone-001",         # None = all services in project
    keys      = ["nav/altitude", "battery/pct"],  # None = all fields
    start     = "-1h",               # relative or RFC3339 absolute ("2024-01-15T12:00:00Z")
    end       = None,                # defaults to now
    sample_by = "1m",                # QuestDB SAMPLE BY: "5s", "1m", "1h", etc. None = raw
    limit     = 500,                 # max rows returned (default: 1000)
)

for row in rows:
    print(row.timestamp)             # str — ISO 8601
    print(row.service)               # str — service display_name
    print(row.fields)                # dict — {"nav/altitude": 102.3, "battery/pct": 87}
```

---

#### `ops.subscribe()`

Subscribe to real-time telemetry via WebSocket. Blocking generator.

```python
# All streams from one service
for frame in ops.subscribe(service="drone-001"):
    print(frame.service, frame.stream, frame.fields)

# Specific streams only
for frame in ops.subscribe(service="drone-001", streams=["telemetry", "event"]):
    print(frame.fields)

# All services in the project
for frame in ops.subscribe():
    print(frame.service, frame.fields)

# Filter by sender actor (services that used to=... routing)
for frame in ops.stream.receive(service="drone-001", from_actor="device:drone-001"):
    print(frame.fields)
```

**`DataFrame` attributes:**

| Attribute | Type | Description |
|-----------|------|-------------|
| `frame.service` | `str` | Service display name |
| `frame.stream` | `str` | Stream type (e.g. `"telemetry"`, `"event"`) |
| `frame.timestamp` | `str` | ISO 8601 timestamp string |
| `frame.fields` | `dict` | Full raw frame including all metadata fields |

Requires `pip install websocket-client`.

---

#### `ops.send_command()`

Send a command to a service and wait for its ACK.

```python
result = ops.send_command(
    "drone-001",
    cmd_type = "goto",
    data     = {"lat": 28.61, "lon": 77.20, "alt": 50.0},
    priority = "normal",     # "normal" | "high" | "emergency"
    timeout  = 30.0,         # seconds to wait for ACK
)

print(result.status)     # "completed" | "accepted" | "rejected" | "failed" | "timeout"
print(result.message)    # optional message from the service
print(result.command_id) # UUID for audit logs
```

This call **blocks** until the service's `on_command` handler calls `cmd.ack()`, `cmd.reject()`, or `cmd.fail()`.

**`CommandResult` attributes:**

| Attribute | Type | Description |
|-----------|------|-------------|
| `result.command_id` | `str` | UUID assigned to this command |
| `result.status` | `str` | `"completed"`, `"accepted"`, `"rejected"`, `"failed"`, or `"timeout"` |
| `result.message` | `str` | Optional human-readable message from the service |

---

#### `ops.assistance_requests()`

Blocking generator yielding incoming assistance requests from services.

```python
for req in ops.assistance_requests():
    print(f"{req.service}: {req.reason}")

    if safe_to_proceed(req):
        req.approve("Proceed with caution")
    else:
        req.deny("Too risky — hold position")
```

Polls every second indefinitely. Only yields requests with `status="pending"`.

**`AssistanceRequest` attributes:**

| Attribute | Type | Description |
|-----------|------|-------------|
| `req.request_id` | `str` | UUID of the request |
| `req.service` | `str` | Service display name that sent the request |
| `req.reason` | `str` | Reason text from the service |

**`AssistanceRequest` methods:**

| Method | Description |
|--------|-------------|
| `req.approve(instruction="")` | Approve and optionally send an instruction string to the service |
| `req.deny(reason="")` | Deny with an optional explanation |

---

### Python return types

| Type | Used in | Key attributes |
|------|---------|---------------|
| `DataRow` | `query()` | `.service`, `.timestamp`, `.fields` |
| `DataFrame` | `subscribe()` | `.service`, `.stream`, `.timestamp`, `.fields` |
| `CommandResult` | `send_command()`, `message.send(..., type="command")` | `.command_id`, `.status`, `.message` |
| `AssistanceRequest` | `assistance_requests()` | `.request_id`, `.service`, `.reason` |
| `AssistanceResponse` | `request_assistance()` | `.approved`, `.instruction`, `.request_id` |
| `Command` | `on_command`, `next_command`, `listen` | `.id`, `.type`, `.data`, `.priority` |

---

## Go SDK — Service side

### `Init()`

```go
import "github.com/systemscale/sdk/go/systemscale"

client, err := systemscale.Init(systemscale.Config{
    APIKey:  "ssk_live_...",  // required
    Project: "my-fleet",      // required
    Service: "drone-001",     // optional: defaults to SYSTEMSCALE_SERVICE env var or os.Hostname()
})
if err != nil {
    log.Fatal(err)
}
defer client.Stop()
```

`Init()` probes the local agent, auto-provisions if needed, and starts background goroutines. Returns an error only if provisioning failed (agent binary missing or unreachable after provisioning).

---

### Go Client methods

#### `client.Log()`

```go
// Basic
client.Log(map[string]any{"nav/altitude": 102.3, "battery/pct": 87})

// With GPS
client.Log(
    map[string]any{"sensors/temp": 85.2},
    systemscale.WithLocation(28.61, 77.20, 102.3),
)

// Nested map (flattened automatically)
client.Log(map[string]any{
    "sensors": map[string]any{"temp": 85.2, "pressure": 1013.2},
    "nav":     map[string]any{"altitude": 102.3, "airspeed": 12.4},
})
// Stored as: sensors/temp, sensors/pressure, nav/altitude, nav/airspeed

// Route to a specific actor
client.Log(data, systemscale.WithReceiver("ground-control", "operator"))
client.Log(data, systemscale.WithReceiver("*", "operator"))       // all operators
client.Log(data, systemscale.WithReceiver("drone-002", "device")) // peer device

// With tags
client.Log(data, systemscale.WithTags(map[string]string{"region": "south"}))

// Named sub-stream
client.Log(data, systemscale.WithStream("sensor"), systemscale.WithStreamName("lidar"))
```

Non-blocking — frames are dropped with a Warn log when the queue is full.

---

#### `client.Alert()`

```go
client.Alert("Low battery",
    systemscale.WithAlertLevel("warning"),
    systemscale.WithAlertData(map[string]any{"pct": 12}),
)
client.Alert("Geofence breach", systemscale.WithAlertLevel("error"))
```

---

#### Go Commands

**Callback style:**

```go
client.OnCommand(func(cmd *systemscale.Command) error {
    // Fires when an operator calls:
    //   ops.SendCommand(ctx, systemscale.CommandRequest{Service: "drone-001", Type: "goto", ...})
    // The operator's call blocks until this handler returns and an ACK is sent.
    switch cmd.Type {
    case "goto":
        err := navigate(cmd.Data["lat"].(float64), cmd.Data["lon"].(float64))
        if err != nil {
            return cmd.Fail(err.Error())
        }
        return cmd.Ack()
    case "land":
        land()
        return cmd.AckMsg("landed successfully")
    default:
        return cmd.Reject("unknown command type: " + cmd.Type)
    }
})
```

**Poll style:**

```go
// Block up to 100ms
cmd, ok := client.NextCommand(100 * time.Millisecond)
if ok {
    // handle cmd
}
```

**`*Command` fields and methods:**

| Field | Type | Description |
|-------|------|-------------|
| `cmd.ID` | `string` | UUID for ACK routing |
| `cmd.Type` | `string` | Command type string (e.g. `"goto"`) |
| `cmd.Data` | `map[string]any` | Arbitrary JSON payload |
| `cmd.Priority` | `string` | `"normal"`, `"high"`, or `"emergency"` |

| Method | Returns | Description |
|--------|---------|-------------|
| `cmd.Ack()` | `error` | Execution completed successfully |
| `cmd.AckMsg(msg string)` | `error` | Completion with an info message |
| `cmd.Reject(reason string)` | `error` | Command refused |
| `cmd.Fail(reason string)` | `error` | Execution started but failed |

---

#### `client.RequestAssistance()`

```go
resp, err := client.RequestAssistance(
    "Obstacle in path",
    map[string]any{"distance_m": 4.2, "type": "vehicle"},
    60*time.Second,
)
if err != nil {
    emergencyLand()
    return
}
if resp.Approved {
    proceed(resp.Instruction)
}
```

---

#### `client.Stream()`

Returns a `*StreamWriter` bound to a named sub-stream:

```go
imu   := client.Stream("imu_raw")
lidar := client.Stream("lidar_scan")

imu.Log(map[string]any{"ax": 0.02, "ay": -0.01, "az": 9.81})
lidar.Log(map[string]any{"points": hex, "count": 768},
    systemscale.WithLocation(lat, lon, alt))
```

---

### Log options

| Option | Signature | Description |
|--------|-----------|-------------|
| `WithStream` | `WithStream(s string)` | Stream type: `"telemetry"` (default), `"sensor"`, `"event"`, `"log"` |
| `WithStreamName` | `WithStreamName(n string)` | Sub-label (e.g. `"lidar_scan"`) |
| `WithLocation` | `WithLocation(lat, lon, altM float64)` | WGS84 coordinates |
| `WithTags` | `WithTags(tags map[string]string)` | Low-cardinality labels for filtering |
| `WithReceiver` | `WithReceiver(id, receiverType string)` | Route to actor: `WithReceiver("*", "operator")` |
| `WithSenderType` | `WithSenderType(t string)` | Override `sender_type` field (default: `"device"`) |

---

### Alert options

| Option | Signature | Description |
|--------|-----------|-------------|
| `WithAlertLevel` | `WithAlertLevel(l string)` | `"info"` (default), `"warning"`, `"error"`, `"critical"` |
| `WithAlertData` | `WithAlertData(d map[string]any)` | Arbitrary metadata attached to the alert |

---

### Config struct

```go
systemscale.Config{
    APIKey:    "ssk_live_...",            // required
    Project:   "my-fleet",               // required
    Service:   "drone-001",              // optional; default: $SYSTEMSCALE_SERVICE or hostname
    Mode:      "auto",                   // "auto" (default) or "service" (fail if no agent)
    APIBase:   "http://127.0.0.1:7777",  // edge agent local URL
    FleetAPI:  "https://fleet.systemscale.io",
    APIKeyURL: "https://apikey.systemscale.io",
    QueueSize: 8192,
    Logger:    slog.Default(),
}
```

`Mode: "service"` makes `Init()` return an error if the local agent is not running (no auto-provisioning).

---

## Go SDK — Operator side

### `Connect()`

```go
ops, err := systemscale.Connect(systemscale.OperatorConfig{
    APIKey:  "ssk_live_...",  // required
    Project: "my-fleet",      // required
    // All URL fields optional — default to hosted service
})
if err != nil {
    log.Fatal(err)
}
```

---

### Go OperatorClient methods

#### `ops.Services(ctx)`

```go
services, err := ops.Services(ctx)
// Returns: []map[string]any
for _, s := range services {
    fmt.Println(s["display_name"], s["status"])
}
```

---

#### `ops.Query(ctx, QueryRequest)`

```go
rows, err := ops.Query(ctx, systemscale.QueryRequest{
    Service:  "drone-001",        // empty = all services
    Keys:     []string{"nav/altitude", "battery/pct"},
    Start:    "-1h",
    SampleBy: "1m",
    Limit:    500,
})

for _, row := range rows {
    fmt.Println(row.Timestamp, row.Service, row.Fields)
}
```

**`DataRow` fields:**

| Field | Type | Description |
|-------|------|-------------|
| `row.Timestamp` | `time.Time` | Frame timestamp |
| `row.Service` | `string` | Service display name |
| `row.Fields` | `map[string]any` | Telemetry key-value pairs |

---

#### `ops.Subscribe(ctx, service, streams)`

```go
ch, err := ops.Subscribe(ctx, "drone-001", nil)
for frame := range ch {
    fmt.Println(frame.Service, frame.Stream, frame.Fields)
}
```

**`*DataFrame` fields:**

| Field | Type | Description |
|-------|------|-------------|
| `frame.Timestamp` | `time.Time` | Frame timestamp |
| `frame.Service` | `string` | Service display name |
| `frame.Stream` | `string` | Stream type |
| `frame.Fields` | `map[string]any` | Full telemetry payload |

---

#### `ops.SendCommand(ctx, CommandRequest)`

```go
result, err := ops.SendCommand(ctx, systemscale.CommandRequest{
    Service:  "drone-001",
    Type:     "goto",
    Data:     map[string]any{"lat": 28.61, "lon": 77.20, "alt": 50.0},
    Priority: "normal",
    Timeout:  30 * time.Second,
})
fmt.Println(result.Status, result.Message)
```

This call **blocks** until the service's `OnCommand` handler sends an ACK.

---

#### `ops.AssistanceRequests(ctx)`

```go
arCh, err := ops.AssistanceRequests(ctx)
for ar := range arCh {
    fmt.Printf("%s: %s\n", ar.ServiceID, ar.Reason)
    if safeToGo(ar) {
        ar.Approve("Proceed with caution")
    } else {
        ar.Deny("Too risky")
    }
}
```

**`*AssistanceRequest` fields:**

| Field | Type | Description |
|-------|------|-------------|
| `ar.RequestID` | `string` | UUID of the request |
| `ar.ServiceID` | `string` | Service name that sent the request |
| `ar.Reason` | `string` | Reason text |
| `ar.Metadata` | `map[string]any` | Additional data from the service |
| `ar.CreatedAt` | `time.Time` | When the request was created |

---

### OperatorConfig struct

```go
systemscale.OperatorConfig{
    APIKey:     "ssk_live_...",                       // required
    Project:    "my-fleet",                           // required
    APIKeyURL:  "https://apikey.systemscale.io",      // default
    FleetAPI:   "https://fleet.systemscale.io",       // default
    QueryAPI:   "https://query.systemscale.io",       // default
    CommandAPI: "https://command.systemscale.io",     // default
    GatewayWS:  "wss://gateway.systemscale.io",       // default
    Logger:     slog.Default(),
}
```

---

### Go return types

| Type | Used in | Fields |
|------|---------|--------|
| `DataRow` | `Query()` | `.Timestamp`, `.Service`, `.Fields` |
| `DataFrame` | `Subscribe()` | `.Timestamp`, `.Service`, `.Stream`, `.Fields` |
| `CommandResult` | `SendCommand()` | `.CommandID`, `.Status`, `.Message` |
| `AssistanceRequest` | `AssistanceRequests()` | `.RequestID`, `.ServiceID`, `.Reason`, `.Metadata`, `.CreatedAt` |
| `AssistanceResponse` | `RequestAssistance()` | `.RequestID`, `.Approved`, `.Instruction` |
| `Command` | `OnCommand`, `NextCommand` | `.ID`, `.Type`, `.Data`, `.Priority` |

---

## Standalone sub-packages (Python)

### StreamEmitter

```python
from systemscale.stream import StreamEmitter

emitter = StreamEmitter(
    api_key    = "ssk_live_...",
    project    = "my-fleet",
    actor      = "drone-001",           # optional; defaults to $SYSTEMSCALE_SERVICE or hostname
    agent_api  = "http://127.0.0.1:7777",
    queue_size = 8192,
)
emitter.start()
emitter.send({"sensors/temp": 85.2}, to="operator:*")
emitter.stop()
```

### StreamSubscriber (operator side)

```python
from systemscale.stream import StreamSubscriber

sub = StreamSubscriber(
    api_key    = "ssk_live_...",
    project    = "my-fleet",
    apikey_url = "https://keys.systemscale.io",
    ws_url     = "wss://ws.systemscale.io",
    fleet_api  = "https://api.systemscale.io",
)

for frame in sub.receive(service="drone-001", streams=["telemetry"],
                          from_actor="device:drone-001"):
    print(frame.fields)
```

`receive()` parameters:

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `service` | `str` | `None` | Service name; `None` = all in project |
| `streams` | `list[str]` | `["telemetry", "event"]` | Stream types to subscribe to |
| `from_actor` | `str` | `None` | Filter: only yield frames from this actor (`"type:id"`) |

### MessageSender

```python
from systemscale.message import MessageSender

# Service-side alert
sender = MessageSender(api_key="...", project="my-fleet", actor="drone-001",
                       agent_api="http://127.0.0.1:7777")
sender.send("obstacle ahead", type="alert", level="warning", to="human:pilot-1")

# Operator-side command
sender = MessageSender(
    api_key     = "...",
    project     = "my-fleet",
    command_api = "https://cmd.systemscale.io",
    apikey_url  = "https://keys.systemscale.io",
    fleet_api   = "https://api.systemscale.io",
)
result = sender.send("goto", type="command", to="device:drone-001",
                     data={"lat": 28.6, "lon": 77.2}, timeout=30.0)
```

### MessageReceiver

```python
from systemscale.message import MessageReceiver

inbox = MessageReceiver(agent_api="http://127.0.0.1:7777")
inbox.start()

@inbox.on_command
def handle(cmd):
    cmd.ack()

# Or poll
cmd = inbox.next_command(timeout=1.0)

inbox.stop()
```

---

## Standalone sub-packages (Go)

### stream package

```go
import "github.com/systemscale/sdk/go/systemscale/stream"

emitter := stream.NewEmitter(stream.EmitterConfig{
    AgentAPI:  "http://127.0.0.1:7777",
    Project:   "my-fleet",
    QueueSize: 8192,
})
ctx, cancel := context.WithCancel(context.Background())
go emitter.Run(ctx)

emitter.Send(map[string]any{"nav/alt": 102.3}, stream.To("operator:*"))
emitter.Send(data, stream.WithStream("sensor"), stream.WithStreamName("lidar"))
```

**`stream` package EmitOptions:**

| Option | Description |
|--------|-------------|
| `stream.To(actor string)` | Route to actor `"type:id"` (e.g. `"operator:*"`) |
| `stream.WithStream(s string)` | Stream type |
| `stream.WithStreamName(n string)` | Sub-label |
| `stream.WithLocation(lat, lon, alt float64)` | WGS84 coordinates |
| `stream.WithTags(tags map[string]string)` | Low-cardinality labels |

```go
// ResolveVehicleIDs — look up internal UUIDs from display names
ids, err := stream.ResolveVehicleIDs(fleetAPI, project, service, token)
```

### message package

```go
import "github.com/systemscale/sdk/go/systemscale/message"

// Sender — service-side alerts
sender := message.NewSender(message.SenderConfig{
    AgentAPI: "http://127.0.0.1:7777",
    Project:  "my-fleet",
})
sender.Send("obstacle ahead",
    message.WithTo("human:pilot-1"),
    message.WithType("alert"),
    message.WithLevel("warning"),
)

// Sender — operator-side commands
sender := message.NewSender(message.SenderConfig{
    APIKey:     "ssk_live_...",
    APIKeyURL:  "https://apikey.systemscale.io",
    CommandAPI: "https://cmd.systemscale.io",
    FleetAPI:   "https://fleet.systemscale.io",
    Project:    "my-fleet",
})
result, err := sender.Send("goto",
    message.WithTo("device:drone-001"),
    message.WithType("command"),
    message.WithData(map[string]any{"lat": 28.6, "lon": 77.2}),
    message.WithTimeout(30*time.Second),
)
```

```go
// Receiver
recv := message.NewReceiver(message.ReceiverConfig{AgentAPI: "http://127.0.0.1:7777"})
go recv.Run(ctx)

recv.OnCommand(func(cmd *message.Command) error {
    return cmd.Ack()
})
```

---

## Agentless mode

If no `systemscale-agent` binary is found, the SDK enters agentless mode automatically:

- Telemetry frames queue **in-process** (up to `queue_size`).
- A single warning is logged at startup — no repeated errors.
- A background thread probes for the agent every 30 seconds.
- When the agent becomes available, the SDK switches out of agentless mode and begins delivering queued frames.

```
WARNING: systemscale-agent binary not found. Running in agentless mode.
         Frames will queue locally until the agent becomes available.
```

No code changes needed — it's fully transparent to your application.

**To fail hard if no agent** (Go only):
```go
client, err := systemscale.Init(systemscale.Config{..., Mode: "service"})
// Returns error if agent not running
```

---

## Self-hosting

Override the default hosted service URLs via environment variables (recommended) or kwargs on `connect()` / Go Config structs.

**Environment variables** (apply to both `init()` and `connect()`):
```bash
export SYSTEMSCALE_API_BASE=http://192.168.1.10:7777      # edge agent (service side)
export SYSTEMSCALE_FLEET_API=http://192.168.1.10:8080
export SYSTEMSCALE_APIKEY_URL=http://192.168.1.10:8083
```

**Python operator — via `connect()` kwargs:**
```python
ops = systemscale.connect(
    api_key     = "ssk_live_...",
    project     = "my-fleet",
    fleet_api   = "http://192.168.1.10:8080",
    apikey_url  = "http://192.168.1.10:8083",
    ws_url      = "ws://192.168.1.10:8084",
    command_api = "http://192.168.1.10:8082",
)
```

**Go — via Config / OperatorConfig:**
```go
ops, err := systemscale.Connect(systemscale.OperatorConfig{
    APIKey:     "ssk_live_...",
    Project:    "my-fleet",
    FleetAPI:   "http://192.168.1.10:8080",
    APIKeyURL:  "http://192.168.1.10:8083",
    GatewayWS:  "ws://192.168.1.10:8084",
    CommandAPI: "http://192.168.1.10:8082",
    QueryAPI:   "http://192.168.1.10:8081",
})
```

---

## Environment variables

| Variable | Default | Description |
|----------|---------|-------------|
| `SYSTEMSCALE_SERVICE` | system hostname | Service name if not passed to `init()` |
| `SYSTEMSCALE_API_BASE` | `http://127.0.0.1:7777` | Edge agent local HTTP URL |
| `SYSTEMSCALE_FLEET_API` | `https://api.systemscale.io` | Fleet / project API |
| `SYSTEMSCALE_APIKEY_URL` | `https://keys.systemscale.io` | API key → JWT exchange service |
| `SYSTEMSCALE_AGENT_BIN` | (auto-detected) | Explicit path to the `systemscale-agent` binary |
| `SYSTEMSCALE_CONFIG_DIR` | `/etc/systemscale` | Where the agent config (`agent.yaml`) is written |
| `SYSTEMSCALE_CERT_DIR` | `$SYSTEMSCALE_CONFIG_DIR/certs` | Where TLS certs are written |

**Agent binary detection order** (when `SYSTEMSCALE_AGENT_BIN` is not set):

1. `systemscale-agent` anywhere in `$PATH`
2. `/usr/local/bin/systemscale-agent`
3. `~/.local/bin/systemscale-agent`
4. `/usr/bin/systemscale-agent`

---

## Edge agent local API

Any language that can make HTTP requests can integrate without the SDK.

### `POST /v1/log`

```bash
curl -X POST http://127.0.0.1:7777/v1/log \
  -H "Content-Type: application/json" \
  -d '{
    "data":        {"nav/altitude": 102.3, "battery/pct": 87},
    "stream":      "telemetry",
    "lat":         28.61,
    "lon":         77.20,
    "alt":         102.3,
    "tags":        {"region": "south"},
    "to":          "operator:*"
  }'
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `data` | object | yes | Key-value payload |
| `stream` | string | no | `"telemetry"` (default), `"sensor"`, `"event"`, `"log"` |
| `stream_name` | string | no | Sub-label within the stream |
| `lat`, `lon`, `alt` | float | no | WGS84 coordinates |
| `tags` | object | no | String key-value labels for filtering |
| `to` | string | no | Target actor `"type:id"` |

Responses: `200 OK` — queued. `400` — bad JSON. `503` — ring buffer full.

### `GET /v1/commands`

Server-Sent Events stream. Each event:
```
data: {"id":"01923abc-...","command_type":"goto","data":{"lat":28.61},"priority":"normal"}
```

Keep the connection open for the lifetime of your process. Reconnect with exponential backoff if it drops.

### `POST /v1/commands/{id}/ack`

```json
{"status": "completed", "message": ""}
```

| Status | Meaning |
|--------|---------|
| `accepted` | Command received, execution in progress |
| `completed` | Execution finished successfully |
| `rejected` | Command refused |
| `failed` | Execution started but failed |

### `GET /healthz`

Returns `200 ok` when QUIC pipeline is active, `503 reconnecting` otherwise.

---

## Stream types

| Stream | Use for | Stored in | Queryable |
|--------|---------|-----------|-----------|
| `telemetry` | High-frequency numeric sensors (position, attitude, battery) | QuestDB hot tier | SQL |
| `sensor` | Raw sensor payloads (LiDAR, IMU, camera metadata, binary) | QuestDB hot tier | SQL |
| `event` | Discrete occurrences (geofence breach, failsafe, alert) | QuestDB + NATS JetStream | SQL + SSE push |
| `log` | Free-form text log lines | QuestDB hot tier | SQL |

All streams are retained for 30 days in QuestDB, then archived to Parquet on S3 indefinitely.

---

## Data formats

### Actor routing (`to=`)

Route frames to specific sessions instead of broadcasting:

```python
client.log(data, to="operator:*")                 # all operators
client.log(data, to="operator:ground-control")    # named operator session
client.log(data, to="device:drone-002")           # peer device
client.log(data, to="human:pilot-1")              # named human actor
client.log(data, to="service:analytics-pipeline") # backend service
```

Go equivalent:

```go
client.Log(data, systemscale.WithReceiver("ground-control", "operator"))
client.Log(data, systemscale.WithReceiver("*", "operator"))
```

Actor types (`operator`, `device`, `human`, `service`) are protocol-level labels — cross-functional and not tied to any specific deployment role.

### Nested key flattening

```python
client.log({
    "sensors": {"temp": 85.2, "pressure": 1013.2},
    "nav":     {"altitude": 102.3, "airspeed": 12.4},
})
# Stored as: sensors/temp, sensors/pressure, nav/altitude, nav/airspeed
```

### Binary payloads

```python
buf = struct.pack("<fff", ax, ay, az)
client.log({"imu_raw": buf.hex(), "format": "vec3f_le"}, stream="sensor")
```

Max frame size: **4 MB**.
