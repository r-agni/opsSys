# Build context: api/
FROM golang:1.21-alpine AS builder
WORKDIR /build
RUN apk add --no-cache git

# Copy workspace config first (changes rarely → good cache layer)
COPY go.work go.work.sum ./

# Copy go.mod/go.sum for all workspace modules so the workspace resolves
COPY auth/go.mod     auth/go.sum     ./auth/
COPY commands/go.mod commands/go.sum ./commands/
COPY fleet/go.mod    fleet/go.sum    ./fleet/
COPY query/go.mod    query/go.sum    ./query/
COPY ingest/go.mod   ingest/go.sum   ./ingest/
COPY video/go.mod    video/go.sum    ./video/
COPY shared/auth/go.mod    shared/auth/go.sum    ./shared/auth/
COPY shared/router/go.mod  shared/router/go.sum  ./shared/router/
COPY shared/storage/go.mod shared/storage/go.sum ./shared/storage/

# Download dependencies (cached as long as go.mod/go.sum unchanged)
RUN cd fleet && go mod download

# Copy source for this service and its workspace dependencies
COPY shared/auth/ ./shared/auth/
COPY fleet/       ./fleet/

# Build (ldflags strip debug info → ~60% smaller binary)
RUN cd fleet && go build -ldflags="-s -w" -o /service .

FROM alpine:3.19
RUN apk add --no-cache ca-certificates tzdata
COPY --from=builder /service /service
ENTRYPOINT ["/service"]
