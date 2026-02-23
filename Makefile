# SystemScale monorepo Makefile
#
# Targets:
#   make proto           — regenerate all protobuf code (requires buf)
#   make build           — build all Rust binaries (agent + relay)
#   make build-api       — build all Go cloud services
#   make build-all       — build everything
#   make sdk             — package Python + Go SDKs
#   make sdk-python      — build Python wheel
#   make sdk-go          — verify Go SDK compiles
#   make test            — run all tests
#   make test-rust       — run Rust tests
#   make test-go         — run Go tests
#   make test-python     — run Python tests (pytest)
#   make lint            — run clippy + golangci-lint
#   make clean           — remove build artefacts

.PHONY: proto build build-api build-all sdk sdk-python sdk-go \
        test test-rust test-go test-python lint clean

# ─────────────────────────────────────────────────────────────────────────────
# Proto generation
# ─────────────────────────────────────────────────────────────────────────────

# buf.yaml lives in proto/; generated code is written to:
#   proto/gen/go/     (Go — used by api/ services)
#   proto/gen/rust/   (Rust — used by agent/ + relay/ via prost-build)

proto:
	@echo "==> Generating protobuf code"
	buf generate --template proto/buf.gen.yaml proto/

# ─────────────────────────────────────────────────────────────────────────────
# Rust builds
# ─────────────────────────────────────────────────────────────────────────────

build:
	@echo "==> Building agent (release)"
	cargo build --release --manifest-path agent/Cargo.toml
	@echo "==> Building relay (release)"
	cargo build --release --manifest-path relay/Cargo.toml

build-debug:
	@echo "==> Building agent (debug)"
	cargo build --manifest-path agent/Cargo.toml
	@echo "==> Building relay (debug)"
	cargo build --manifest-path relay/Cargo.toml

# ─────────────────────────────────────────────────────────────────────────────
# Go services build
# ─────────────────────────────────────────────────────────────────────────────

build-api:
	@echo "==> Building Go cloud services (api/)"
	cd api && go build ./fleet/... ./query/... ./commands/... ./auth/... ./ingest/... ./video/...

# ─────────────────────────────────────────────────────────────────────────────
# Combined
# ─────────────────────────────────────────────────────────────────────────────

build-all: build build-api

# ─────────────────────────────────────────────────────────────────────────────
# SDK packaging
# ─────────────────────────────────────────────────────────────────────────────

SDK_DIST := dist/sdk

sdk: sdk-python sdk-go

sdk-python:
	@echo "==> Building Python SDK wheel"
	mkdir -p $(SDK_DIST)
	cd sdk/python && python -m build --outdir ../../$(SDK_DIST)/python
	@echo "    Wheel: $(SDK_DIST)/python/"

sdk-go:
	@echo "==> Verifying Go SDK compiles"
	cd sdk/go && go build ./...
	@echo "    Go SDK: ok"

# ─────────────────────────────────────────────────────────────────────────────
# Tests
# ─────────────────────────────────────────────────────────────────────────────

test: test-rust test-go test-python

test-rust:
	@echo "==> Running Rust tests"
	cargo test --manifest-path agent/Cargo.toml
	cargo test --manifest-path relay/Cargo.toml

test-go:
	@echo "==> Running Go tests (api/)"
	cd api && go test ./fleet/... ./query/... ./commands/... ./auth/... ./ingest/...
	@echo "==> Running Go tests (SDK)"
	cd sdk/go && go test ./...

test-python:
	@echo "==> Running Python SDK tests"
	cd sdk/python && python -m pytest tests/ -v

# ─────────────────────────────────────────────────────────────────────────────
# Linting
# ─────────────────────────────────────────────────────────────────────────────

lint:
	@echo "==> Rust: clippy"
	cargo clippy --manifest-path agent/Cargo.toml -- -D warnings
	cargo clippy --manifest-path relay/Cargo.toml -- -D warnings
	@echo "==> Go: golangci-lint"
	cd api    && golangci-lint run ./...
	cd sdk/go && golangci-lint run ./...

# ─────────────────────────────────────────────────────────────────────────────
# Docker images (for relay and cloud services)
# ─────────────────────────────────────────────────────────────────────────────

REGISTRY ?= ghcr.io/systemscale
TAG      ?= latest

docker-relay:
	@echo "==> Building relay Docker image"
	docker build -f deploy/docker/Dockerfile.relay \
	    --build-arg TARGET=x86_64-unknown-linux-gnu \
	    -t $(REGISTRY)/relay:$(TAG) .

docker-api:
	@echo "==> Building cloud service Docker images"
	for svc in ingest commands fleet query auth; do \
	    docker build -f api/$$svc/Dockerfile \
	        -t $(REGISTRY)/$$svc:$(TAG) api; \
	done

# ─────────────────────────────────────────────────────────────────────────────
# Agent cross-compilation for ARM64 (Raspberry Pi / Jetson)
# ─────────────────────────────────────────────────────────────────────────────

AGENT_TARGET ?= aarch64-unknown-linux-gnu

build-agent-arm64:
	@echo "==> Cross-compiling agent for $(AGENT_TARGET)"
	cargo build --release \
	    --manifest-path agent/Cargo.toml \
	    --bin systemscale-agent \
	    --target $(AGENT_TARGET)
	@echo "    Binary: agent/target/$(AGENT_TARGET)/release/systemscale-agent"

# ─────────────────────────────────────────────────────────────────────────────
# Dev environment
# ─────────────────────────────────────────────────────────────────────────────

dev-up:
	@echo "==> Starting local dev stack"
	docker compose -f deploy/docker/docker-compose.dev.yml up -d

dev-down:
	@echo "==> Stopping local dev stack"
	docker compose -f deploy/docker/docker-compose.dev.yml down

dev-build:
	@echo "==> Building dev Docker images"
	docker compose -f deploy/docker/docker-compose.dev.yml build

dev-logs:
	docker compose -f deploy/docker/docker-compose.dev.yml logs -f

# ─────────────────────────────────────────────────────────────────────────────
# Clean
# ─────────────────────────────────────────────────────────────────────────────

clean:
	cargo clean --manifest-path agent/Cargo.toml
	cargo clean --manifest-path relay/Cargo.toml
	cd api    && go clean ./...
	cd sdk/go && go clean ./...
	rm -rf $(SDK_DIST)
	rm -rf proto/gen/
