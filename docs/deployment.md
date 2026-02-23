# SystemScale: Production Deployment Guide

This guide takes you from zero to a running fleet. The architecture is designed so that **scaling is purely an infrastructure operation** — no code changes, no config file changes on vehicles. You provision more of what's already there.

---

## Deployment tiers

Start small. Grow without touching a line of code.

| Tier | Vehicles | Relay | Cloud | Approx. cost |
|------|----------|-------|-------|-------------|
| **Dev / test** | 1–10 | 1 × t3.xlarge (single region) | 1 × t3.xlarge (all services on one VM) | ~$100/mo |
| **Small prod** | 10–100 | 1–3 × c6in.xlarge, 1–2 regions | EKS 2-node + r6i.2xlarge QuestDB | ~$800/mo |
| **Medium** | 100–500 | 3 × c6in.4xlarge per region, 3 regions | EKS 4-node + r6i.4xlarge QuestDB | ~$4k/mo |
| **Full scale** | 1000+ | c6in.8xlarge × 3 per region × 8 regions | EKS 6–12 node + r6i.8xlarge QuestDB + replicas | ~$25k/mo |

**What changes between tiers:** Only Terraform variable values (instance type, count, region list) and Helm replica counts. The code, the vehicle config, and the protocol are identical at every tier.

**Scale-up path (no code changes):**
- Add relay nodes → `desired_count` in ASG Terraform variable
- Add relay regions → `terraform apply` in new region; NATS leaf auto-joins cluster
- Add cloud capacity → `kubectl scale`; EKS node group auto-scales
- QuestDB read replicas → already in Terraform, uncomment and apply

---

## Prerequisites

| Tool | Min version | Purpose |
|------|------------|---------|
| Terraform | 1.7 | Relay VM + cloud cluster provisioning |
| Ansible | 2.16 | Relay OS tuning |
| Helm | 3.14 | Kubernetes service deployment |
| buf | 1.30 | Protobuf code generation |
| AWS CLI | 2.x | AWS account access |
| step (step-ca) | 0.26 | Vehicle certificate issuance |

You also need:
- An **AWS account** (t3.xlarge quota is default — no limit increase needed for dev tier)
- A **domain name** for relay endpoints (e.g. `relay.yourcompany.io`)
- **Cloudflare** account (free plan works for dev; Argo Smart Routing for prod)

---

## Architecture overview

```
Vehicles (any region)
  → QUIC UDP 443 → nearest relay node (Anycast IP)
  → NATS leaf (co-located on relay, localhost publish)
  → NATS leaf-to-cluster replication (private backbone)
  → Central cloud cluster (NATS + QuestDB + services)
  → Operator browser (WebSocket from relay ws-gateway, or REST/gRPC from cloud)
```

Commands flow in reverse. Video takes a parallel SRT path (never mixed with telemetry).

---

## Dev / single-VM setup

For development, run everything on one machine with Docker Compose. This is the fastest way to get started.

```bash
# 1. Build all Docker images
make docker-relay
make docker-api

# 2. Start cloud services (NATS, QuestDB, commands, fleet, etc.)
docker compose -f deploy/docker/docker-compose.dev.yml up -d

# 3. Start a relay (or just point the edge agent directly at the cloud NATS)
#    For dev, you can skip the relay and have the edge agent connect to NATS directly.

# 4. Run the edge agent locally (or on a vehicle)
SYSTEMSCALE_CONFIG=path/to/agent.yaml ./systemscale-agent
```

The cloud services expose:
- `localhost:4222` — NATS
- `localhost:9000` — QuestDB HTTP + ILP
- `localhost:8080` — fleet REST API
- `localhost:8081` — query REST API
- `localhost:8082` — commands REST + gRPC
- `localhost:8083` — auth / apikey service
- `localhost:8084` — ws-gateway (WebSocket)
- `localhost:7777` — edge agent local SDK API (when agent is running)

---

## Step 1 — Generate protobuf code

Run once before building anything:

```bash
make proto
```

This runs `buf generate` and writes generated code to:
- `proto/gen/go/` — used by Go cloud services
- `proto/gen/rust/` — used by Rust edge agent and relay

Generated files are not committed to git. Run `make proto` after any `.proto` change.

---

## Step 2 — Provision relay nodes

```bash
cd deploy/terraform/relay-node

terraform init

# Dev: single small relay in one region
terraform apply \
  -var="region=us-east-1" \
  -var="relay_count=1" \
  -var="instance_type=t3.xlarge"

# Production: 3 nodes per region, network-optimized instances
terraform apply \
  -var="region=ap-south-1" \
  -var="relay_count=3" \
  -var="instance_type=c6in.8xlarge"
```

What Terraform creates:
- EC2 instances with the configured instance type
- Auto Scaling Group (scale out by increasing `relay_count`)
- Security groups: UDP 443 inbound (QUIC), TCP 8443 (WS gateway), UDP 9000–9099 (SRT video)
- IAM roles for CloudWatch and SSM access
- Elastic IP (dev) or Global Accelerator Anycast IP (prod)

For multiple regions, run `terraform apply` in each region directory. NATS leaf nodes auto-join the central cluster on startup.

---

## Step 3 — Tune relay OS

```bash
cd deploy/ansible
ansible-playbook -i inventory.ini udp-tuning.yml
```

What this configures:
- `net.core.rmem_max = 134217728` (128 MB UDP receive buffer)
- `net.core.wmem_max = 134217728` (128 MB UDP send buffer)
- `SO_REUSEPORT` multi-queue sockets (one per CPU core for parallel QUIC)
- CPU isolation for cores 4–31 (relay hot path runs on dedicated cores)
- TCP BBR congestion control
- NVMe scheduler set to `none` (for SRT video ring buffer I/O)

**This step is mandatory for production.** Without it, relay latency targets are not achievable regardless of instance type or code quality.

---

## Step 4 — Provision cloud cluster

```bash
cd deploy/terraform/cloud-cluster

# Dev: smallest viable cluster (or use docker-compose instead — see Step 0)
terraform apply \
  -var="eks_node_type=t3.xlarge" \
  -var="eks_desired=2" \
  -var="questdb_instance=r6i.xlarge"

# Production: full cluster
terraform apply
```

Dev creates: EKS (t3.xlarge × 2), QuestDB r6i.xlarge (single node), S3 buckets.

Production creates: EKS (c6i.4xlarge × 6), QuestDB r6i.8xlarge + 2 read replicas, S3 buckets for telemetry archive and video recording.

---

## Step 5 — Deploy cloud services (Helm)

```bash
kubectl create namespace systemscale

# Core infrastructure
helm install nats-cluster    deploy/helm/nats-cluster/    -n systemscale
helm install questdb         deploy/helm/questdb/          -n systemscale
helm install livekit         deploy/helm/livekit/          -n systemscale
helm install keycloak        deploy/helm/keycloak/         -n systemscale

# Application services
helm install ingest    deploy/helm/ingest/    -n systemscale
helm install commands  deploy/helm/commands/  -n systemscale
helm install fleet     deploy/helm/fleet/     -n systemscale
helm install query     deploy/helm/query/     -n systemscale
helm install video     deploy/helm/video/     -n systemscale
```

For dev, reduce replica counts:

```bash
helm install ingest deploy/helm/ingest/ \
  -n systemscale --set replicaCount=1
```

---

## Step 6 — Build and deploy relay binaries

```bash
# Build relay binary (native x86-64)
cargo build --release --manifest-path relay/Cargo.toml

# Copy to relay nodes via Ansible
ansible -i deploy/ansible/inventory.ini relay_nodes \
  -m copy \
  -a "src=relay/target/release/systemscale-relay dest=/usr/local/bin/ mode=0755"

# Restart the relay service on all nodes
ansible -i deploy/ansible/inventory.ini relay_nodes \
  -m systemd -a "name=systemscale-relay state=restarted"
```

The relay runs as a systemd service. The unit file is deployed by Ansible during OS tuning (Step 3).

---

## Step 7 — Issue vehicle certificates

Each vehicle needs a client X.509 certificate. The relay validates this on the QUIC handshake and extracts the vehicle ID from the Subject CN.

```bash
# Issue a certificate for vehicle "drone-001"
step ca certificate "drone-001" vehicle.crt vehicle.key \
  --ca-url https://ca.internal.yourcompany.io \
  --root root_ca.crt

# The Subject CN must exactly match the vehicle_id in agent.yaml
```

For dev, you can run step-ca on the cloud VM:

```bash
step ca init --name="SystemScale Dev CA" --dns="ca.internal" --address=":8443" --provisioner="admin"
step-ca $(step path)/config/ca.json
```

Certificate rotation is automated: set up a cron job or systemd timer to run `step ca renew` before expiry. The relay checks OCSP every 5 minutes; revoke a cert via `step ca revoke` to cut off a vehicle within 5 minutes.

---

## Step 8 — Register vehicles

```bash
# Register a new vehicle in the fleet registry (fleet-api REST)
curl -X POST https://api.yourcompany.io/v1/vehicles \
  -H "Authorization: Bearer $OPERATOR_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "vehicle_id": "drone-001",
    "type": "fixed-wing",
    "name": "Survey Alpha"
  }'
```

Now deploy the edge agent to the vehicle (see [hardware-integration.md](hardware-integration.md)).

---

## Verification checklist

After completing all steps, verify the end-to-end pipeline:

```bash
# 1. Check relay is reachable (UDP 443 — telnet will fail, that's correct for QUIC)
nc -u relay.yourcompany.io 443

# 2. Start the edge agent on a vehicle (or SITL)
SYSTEMSCALE_CONFIG=/etc/systemscale/agent.yaml systemscale-agent

# 3. Check edge agent connected to relay
curl http://localhost:7777/healthz
# Expected: 200 OK, body "ok"

# 4. Send a test telemetry frame via the local SDK API
curl -X POST http://localhost:7777/v1/log \
  -H "Content-Type: application/json" \
  -d '{"data":{"test":1,"altitude_m":100.0},"stream":"telemetry"}'
# Expected: 200 OK

# 5. Query QuestDB 10s later to confirm data landed
curl "http://questdb.yourcompany.io:9000/exec?query=SELECT%20*%20FROM%20telemetry%20LIMIT%2010"

# 6. Send a command (via command-api)
curl -X POST https://api.yourcompany.io/v1/commands \
  -H "Authorization: Bearer $OPERATOR_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"vehicle_id":"drone-001","type":"rtl","data":{},"priority":"normal"}'
# Expected: 200 with command ID; vehicle logs should show the command received + ACK
```

---

## Ongoing operations

**Adding a vehicle:** Issue a cert (Step 7), register it (Step 8), deploy the edge agent binary and config. No changes to relay or cloud.

**Adding a relay region:** Run `terraform apply` in the new region directory. NATS leaf auto-joins. Update Anycast routing weights if using Global Accelerator.

**Updating the edge agent:** Cross-compile (`make build-agent-arm64`), copy binary to vehicle, restart systemd service. No downtime — ring buffer absorbs the gap.

**Rotating certificates:** Run `step ca renew` on the vehicle before expiry. The new cert takes effect on the next QUIC reconnect.

**Scaling up cloud services:**
```bash
kubectl scale deployment telemetry-ingest -n systemscale --replicas=4
```
