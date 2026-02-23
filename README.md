# SystemScale

Operator dashboard and telemetry platform for managing vehicle/drone fleets.

## Architecture

```
Vehicles → [Edge Agent] → QUIC → [Relay] → NATS → [Cloud APIs] → [Dashboard]
```

| Layer | Technology | Purpose |
|-------|-----------|---------|
| Edge | Rust (agent/) | Telemetry collection, command dispatch, ring buffer |
| Relay | Rust (server/relay/) | QUIC server, WebSocket gateway, NATS bridge |
| Cloud | Go (server/api/) | Fleet CRUD, telemetry queries, auth, command dispatch |
| Frontend | React + TypeScript (server/dashboard/) | Operator web UI |
| SDKs | Python + Go (sdk/) | Device-side integration libraries |

## Directory Layout

```
agent/          Rust edge agent (runs on each vehicle)
sdk/            Python and Go SDKs for device integration
server/
  api/          Go microservices (fleet, query, commands, auth, ingest)
  relay/        Rust relay services (ws-gateway, QUIC server, SRT)
  dashboard/    React 19 SPA (Vite + TypeScript)
infra/          Docker Compose, Terraform, Helm charts, Ansible
proto/          Protobuf definitions (DataEnvelope, CommandEnvelope)
docs/           Deployment guide, data formats, hardware integration
scripts/        Dev utilities (seed fleet, simulate telemetry, test SDK)
```

## Service Ports

| Service | Port | Protocol |
|---------|------|----------|
| fleet-api | 8080 | HTTP REST |
| query-api | 8081 | HTTP REST |
| command-api | 8082 / 9090 | HTTP REST / gRPC |
| apikey-service | 8083 | HTTP REST |
| ws-gateway | 8084 | WebSocket |
| dashboard (dev) | 5173 | HTTP |
| NATS | 4222 | TCP |
| QuestDB ILP | 9009 | TCP |
| QuestDB SQL | 8812 | PostgreSQL wire |
| CockroachDB | 26257 | PostgreSQL wire |
| Edge agent local API | 7777 | HTTP |

## Quick Start (Local Dev)

**Prerequisites**: Docker, Go 1.21+, Rust (stable), Node 20+, `buf` CLI

```bash
# 1. Start infrastructure
make dev-up

# 2. Build and run cloud services
make build-api
# Run each: go run ./server/api/fleet, ./server/api/query, etc.

# 3. Start dashboard
cd server/dashboard && npm install && npm run dev

# 4. Seed test data
python scripts/seed_fleet.py
```

Copy `.env` and fill in any secrets, then `source .env` before running services.

## Edge Agent

Runs on each vehicle. Reads from hardware (MAVLink, Unix socket), buffers in a ring buffer, streams to relay via QUIC.

```bash
make build                          # builds agent + relay (release)
make build-agent-arm64              # cross-compile for Raspberry Pi / Jetson
```

Config: `/etc/systemscale/agent.yaml` — see `agent/agent-core/config.example.yaml`

## SDKs

See [sdk/README.md](sdk/README.md) for full API reference.

**Python**
```python
import systemscale
systemscale.init(vehicle_id="drone-001")
systemscale.log({"altitude_m": 102.3}, stream="telemetry")
```

**Go**
```go
ss := systemscale.New(systemscale.Config{VehicleID: "drone-001"})
ss.Log(map[string]any{"altitude_m": 102.3})
```

SDKs post to the edge agent's local HTTP API (`localhost:7777`). No direct cloud connection required from the device.

## Protobuf

```bash
make proto    # regenerates Go, Rust, TypeScript from proto/
```

Core types: `DataEnvelope` and `CommandEnvelope` in `proto/core/envelope.proto`.

## Deployment

See [docs/deployment.md](docs/deployment.md) for full instructions across dev, staging, and production tiers.

```bash
make docker-relay   # build relay image
make docker-api     # build API images
# then: terraform apply, helm upgrade
```
