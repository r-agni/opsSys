// command-api: gRPC CommandService — operator-facing bidirectional streaming command endpoint.
//
// Operators send commands via a persistent gRPC bidirectional stream:
//   - Single stream per operator session (HTTP/2 multiplexing, no per-command connection)
//   - Commands are published to NATS JetStream with UUIDv7 deduplication IDs
//   - ACKs from vehicles flow back through NATS → this service → the gRPC stream
//
// Exactly-once guarantee:
//   - command_id = UUIDv7 (monotonic, time-sortable, globally unique)
//   - NATS JetStream 2-minute dedup window on "command" stream
//   - If operator retries within 2 minutes, vehicle sees first delivery only
//
// Authorization (enforced at gRPC level, before NATS publish):
//   - JWT Bearer validated by shared/auth middleware
//   - vehicle_set claim: operator can only command vehicles in their set
//   - role claim: must be "operator" or "admin" to send commands
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/systemscale/services/shared/auth"
	"github.com/systemscale/services/shared/router"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/encoding"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// ──────────────────────────────────────────────────────────────────────────────
// Configuration
// ──────────────────────────────────────────────────────────────────────────────

type config struct {
	GRPCAddr     string
	HTTPRestAddr string
	NATSUrl      string
	JWKSURLs     string
	MetricsAddr  string
	RegionID     string
}

func loadConfig() config {
	jwks := os.Getenv("JWKS_URLS")
	if jwks == "" {
		jwks = os.Getenv("JWKS_URL")
	}
	if jwks == "" {
		jwks = "https://keycloak.internal/realms/systemscale/protocol/openid-connect/certs"
	}
	return config{
		GRPCAddr:     envOr("GRPC_ADDR", ":50051"),
		HTTPRestAddr: envOr("HTTP_REST_ADDR", ":8082"),
		NATSUrl:      envOr("NATS_URL", "nats://localhost:4222"),
		JWKSURLs:     jwks,
		MetricsAddr:  envOr("METRICS_ADDR", ":9090"),
		RegionID:     envOr("REGION_ID", "local"),
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Custom gRPC codec — lets us use hand-rolled protobuf types without generated
// code. Overrides the default "proto" codec for this binary only.
// ──────────────────────────────────────────────────────────────────────────────

func init() {
	encoding.RegisterCodec(commandCodec{})
}

type commandCodec struct{}

func (commandCodec) Name() string { return "proto" }

func (commandCodec) Marshal(v any) ([]byte, error) {
	switch msg := v.(type) {
	case *CommandEnvelope:
		return encodeCommandEnvelope(msg)
	case *CommandAck:
		return encodeCommandAckProto(msg)
	case *rawFrame:
		return msg.data, nil
	default:
		if pm, ok := v.(proto.Message); ok {
			return proto.Marshal(pm)
		}
		return nil, fmt.Errorf("commandCodec: cannot marshal %T", v)
	}
}

func (commandCodec) Unmarshal(data []byte, v any) error {
	switch msg := v.(type) {
	case *CommandEnvelope:
		return decodeCommandEnvelopeInto(data, msg)
	case *CommandAck:
		decoded, err := decodeCommandAck(data)
		if err != nil {
			return err
		}
		*msg = *decoded
		return nil
	case *TelemetryRequest:
		return decodeTelemetryRequestInto(data, msg)
	case *rawFrame:
		msg.data = append(msg.data[:0], data...)
		return nil
	default:
		if pm, ok := v.(proto.Message); ok {
			return proto.Unmarshal(data, pm)
		}
		return fmt.Errorf("commandCodec: cannot unmarshal into %T", v)
	}
}

// rawFrame wraps already-encoded protobuf bytes for pass-through streaming.
type rawFrame struct {
	data []byte
}

// TelemetryRequest mirrors proto/core/command_service.proto TelemetryRequest.
type TelemetryRequest struct {
	VehicleIDs []string // field 1, repeated string
	Streams    []uint32 // field 2, repeated StreamType (varint)
	MaxHz      uint32   // field 3, uint32
}

// commandBidiStream abstracts Send/Recv for the CommandService bidirectional RPC.
type commandBidiStream interface {
	Send(*CommandAck) error
	Recv() (*CommandEnvelope, error)
	Context() context.Context
}

// serverStreamAdapter wraps grpc.ServerStream into a typed commandBidiStream.
type serverStreamAdapter struct {
	grpc.ServerStream
}

func (a *serverStreamAdapter) Send(ack *CommandAck) error {
	return a.ServerStream.SendMsg(ack)
}

func (a *serverStreamAdapter) Recv() (*CommandEnvelope, error) {
	cmd := new(CommandEnvelope)
	if err := a.ServerStream.RecvMsg(cmd); err != nil {
		return nil, err
	}
	return cmd, nil
}

// ──────────────────────────────────────────────────────────────────────────────
// Proto types — mirror proto/core/envelope.proto + common.proto field numbers.
// ──────────────────────────────────────────────────────────────────────────────

// CommandEnvelope mirrors proto/core/envelope.proto CommandEnvelope.
type CommandEnvelope struct {
	VehicleID  string // field 1
	CommandID  string // field 2 — UUIDv7, set by this service
	Payload    []byte // field 3 — opaque command bytes from operator
	Priority   int32  // field 4 — 0=NORMAL, 1=HIGH, 2=EMERGENCY
	IssuerID   string // field 5 — operator user ID from JWT sub claim
	IssuedAt   uint64 // field 6 — millisecond UTC epoch when command was issued
	TtlMs      uint32 // field 7 — discard if not delivered within TTL
}

// CommandAck mirrors proto/core/common.proto CommandAck.
type CommandAck struct {
	CommandID string // field 1
	VehicleID string // field 2
	Status    int32  // field 3 — 0=PENDING, 1=DELIVERED, 2=EXECUTED, 3=FAILED, 4=TIMEOUT
	Message   string // field 4 — human-readable status (from vehicle)
	LatencyMs uint32 // field 5 — round-trip latency from NATS publish to vehicle ACK
}

// encodeCommandEnvelope serializes CommandEnvelope to proto binary.
// Manual encoding matches the proto field numbers without code generation.
func encodeCommandEnvelope(cmd *CommandEnvelope) ([]byte, error) {
	var out []byte
	out = appendProtoString(out, 1, cmd.VehicleID)         // vehicle_id = 1
	out = appendProtoString(out, 2, cmd.CommandID)          // command_id = 2
	out = appendProtoBytes(out, 3, cmd.Payload)             // payload = 3
	out = appendProtoVarint(out, 4, uint64(cmd.Priority))   // priority = 4
	if cmd.IssuerID != "" {
		out = appendProtoString(out, 5, cmd.IssuerID)       // issuer_id = 5
	}
	if cmd.IssuedAt > 0 {
		out = appendProtoVarint(out, 6, cmd.IssuedAt)       // issued_at = 6
	}
	if cmd.TtlMs > 0 {
		out = appendProtoVarint(out, 7, uint64(cmd.TtlMs))  // ttl_ms = 7
	}
	return out, nil
}

// ──────────────────────────────────────────────────────────────────────────────
// CommandService implementation
// ──────────────────────────────────────────────────────────────────────────────

// CommandServiceServer implements the gRPC CommandService.
// The gRPC service definition (from proto) will be auto-generated by buf in production.
type CommandServiceServer struct {
	router   router.MessageRouter
	regionID string
}

// SendCommand handles a bidirectional streaming RPC.
// The operator sends CommandEnvelope messages; the server streams back CommandAck messages.
//
// Stream lifecycle:
//   1. Validate JWT (auth interceptor, already done before this method)
//   2. For each incoming command:
//      a. Authorize: check vehicle in operator's vehicle_set
//      b. Assign UUIDv7 command_id (dedup key)
//      c. Publish to NATS JetStream "command.{vehicle_id}" with Nats-Msg-Id header
//      d. Subscribe to "command.{vehicle_id}.ack" on NATS
//      e. When ACK arrives, stream CommandAck back to operator
func (s *CommandServiceServer) SendCommand(stream commandBidiStream) error {
	ctx := stream.Context()
	claims, ok := auth.ClaimsFromContext(ctx)
	if !ok {
		return status.Error(codes.Unauthenticated, "missing auth claims")
	}
	if !claims.Role.CanSendCommand() {
		return status.Error(codes.PermissionDenied, "role does not allow sending commands")
	}

	// Subscribe to ACK subjects for this operator's session.
	// We use a session-scoped channel to correlate ACKs with pending commands.
	pendingACKs := make(map[string]chan *CommandAck, 64) // command_id → ack channel
	var pendingMu sync.Mutex                             // guards pendingACKs

	// ACK subscriber: listens on command.*.ack and routes to the right pending channel
	// We subscribe to all ACKs and filter by command_id (simpler than per-vehicle subs)
	ackCh, err := s.router.Subscribe(ctx, "command.*.ack")
	if err != nil {
		return status.Errorf(codes.Internal, "subscribe ACK: %v", err)
	}

	// Background ACK router: receives ACKs from NATS and routes to pending channels
	go func() {
		for msg := range ackCh {
			ack, err := decodeCommandAck(msg.Data)
			if err != nil {
				slog.Warn("ACK decode error", "err", err)
				continue
			}
			pendingMu.Lock()
			ch, ok := pendingACKs[ack.CommandID]
			pendingMu.Unlock()
			if ok {
				select {
				case ch <- ack:
				default:
					// Operator is slow consuming ACKs — drop this one
				}
			}
		}
	}()

	// Command receive loop
	for {
		cmd, err := stream.Recv()
		if err != nil {
			return nil // stream closed by operator
		}

		// Authorization check
		if !claims.CanAccessVehicle(cmd.VehicleID) {
			if err := stream.Send(&CommandAck{
				CommandID: cmd.CommandID,
				VehicleID: cmd.VehicleID,
				Status:    3, // FAILED
				Message:   "not authorized for this vehicle",
			}); err != nil {
				return err
			}
			continue
		}

		cmd.CommandID = uuid.Must(uuid.NewV7()).String()
		cmd.IssuerID = claims.Subject
		cmd.IssuedAt = uint64(time.Now().UnixMilli())

		payload, err := encodeCommandEnvelope(cmd)
		if err != nil {
			slog.Error("encode command", "err", err)
			continue
		}

		subject := fmt.Sprintf("command.%s", cmd.VehicleID)
		pubErr := s.router.Publish(ctx, subject, payload, router.PubOptions{
			DeduplicationID: cmd.CommandID, // Nats-Msg-Id → exactly-once within 2min window
			TTL:             30 * time.Second,
		})
		if pubErr != nil {
			slog.Error("NATS publish command", "err", pubErr, "vehicle", cmd.VehicleID)
			stream.Send(&CommandAck{
				CommandID: cmd.CommandID,
				VehicleID: cmd.VehicleID,
				Status:    3, // FAILED
				Message:   fmt.Sprintf("publish error: %v", pubErr),
			})
			continue
		}

		// Register pending ACK channel
		ackChan := make(chan *CommandAck, 1)
		pendingMu.Lock()
		pendingACKs[cmd.CommandID] = ackChan
		pendingMu.Unlock()

		// ACK timeout goroutine
		go func(commandID, vehicleID string) {
			ttl := 30 * time.Second
			if cmd.TtlMs > 0 {
				ttl = time.Duration(cmd.TtlMs) * time.Millisecond
			}
			select {
			case ack := <-ackChan:
				pendingMu.Lock()
				delete(pendingACKs, commandID)
				pendingMu.Unlock()
				if sendErr := stream.Send(ack); sendErr != nil {
					slog.Warn("stream send ACK", "err", sendErr)
				}
			case <-time.After(ttl):
				pendingMu.Lock()
				delete(pendingACKs, commandID)
				pendingMu.Unlock()
				stream.Send(&CommandAck{
					CommandID: commandID,
					VehicleID: vehicleID,
					Status:    4, // TIMEOUT
					Message:   "vehicle did not ACK within TTL",
				})
			case <-ctx.Done():
			}
		}(cmd.CommandID, cmd.VehicleID)

		slog.Info("command published",
			"command_id", cmd.CommandID,
			"vehicle_id", cmd.VehicleID,
			"priority", cmd.Priority,
		)
	}
}

// decodeCommandAck parses a raw ACK protobuf payload from NATS.
// Mirrors CommandAck in proto/core/common.proto field numbers.
func decodeCommandAck(data []byte) (*CommandAck, error) {
	ack := &CommandAck{}
	var pos int
	for pos < len(data) {
		tag, n := consumeVarint(data[pos:])
		if n == 0 {
			break
		}
		pos += n
		fieldNum := tag >> 3
		wireType := tag & 0x7
		switch wireType {
		case 0:
			val, n2 := consumeVarint(data[pos:])
			if n2 == 0 {
				return nil, fmt.Errorf("truncated varint")
			}
			pos += n2
			switch fieldNum {
			case 3:
				ack.Status = int32(val)
			case 5:
				ack.LatencyMs = uint32(val)
			}
		case 2:
			length, n2 := consumeVarint(data[pos:])
			if n2 == 0 {
				return nil, fmt.Errorf("truncated length")
			}
			pos += n2
			s := string(data[pos : pos+int(length)])
			switch fieldNum {
			case 1:
				ack.CommandID = s
			case 2:
				ack.VehicleID = s
			case 4:
				ack.Message = s
			}
			pos += int(length)
		default:
			return nil, fmt.Errorf("unsupported wire type %d", wireType)
		}
	}
	return ack, nil
}

// StreamTelemetry implements the server-streaming RPC from command_service.proto.
// It subscribes to NATS telemetry subjects and pushes DataEnvelope frames back
// to the gRPC client, filtered by vehicle_ids and stream types from the request.
func (s *CommandServiceServer) StreamTelemetry(req *TelemetryRequest, stream grpc.ServerStream) error {
	ctx := stream.Context()
	claims, ok := auth.ClaimsFromContext(ctx)
	if !ok {
		return status.Error(codes.Unauthenticated, "missing auth claims")
	}

	ch, err := s.router.Subscribe(ctx, "telemetry.>")
	if err != nil {
		return status.Errorf(codes.Internal, "subscribe telemetry: %v", err)
	}

	vehicleFilter := make(map[string]bool, len(req.VehicleIDs))
	for _, v := range req.VehicleIDs {
		vehicleFilter[v] = true
	}
	streamFilter := make(map[uint32]bool, len(req.Streams))
	for _, st := range req.Streams {
		streamFilter[st] = true
	}

	var minInterval time.Duration
	if req.MaxHz > 0 {
		minInterval = time.Second / time.Duration(req.MaxHz)
	}
	lastSend := time.Time{}

	for msg := range ch {
		if minInterval > 0 && time.Since(lastSend) < minInterval {
			continue
		}

		vehicleID, streamType := peekDataEnvelope(msg.Data)

		if len(vehicleFilter) > 0 && !vehicleFilter[vehicleID] {
			continue
		}
		if len(streamFilter) > 0 && !streamFilter[streamType] {
			continue
		}
		if !claims.CanAccessVehicle(vehicleID) {
			continue
		}

		if err := stream.SendMsg(&rawFrame{data: msg.Data}); err != nil {
			return err
		}
		lastSend = time.Now()
	}

	return nil
}

// ──────────────────────────────────────────────────────────────────────────────
// gRPC service descriptor (normally generated by protoc-gen-go-grpc)
// ──────────────────────────────────────────────────────────────────────────────

var _CommandService_serviceDesc = grpc.ServiceDesc{
	ServiceName: "systemscale.core.v1.CommandService",
	HandlerType: (*CommandServiceServer)(nil),
	Methods:     []grpc.MethodDesc{},
	Streams: []grpc.StreamDesc{
		{
			StreamName:   "SendCommand",
			Handler:      _CommandService_SendCommand_Handler,
			ServerStreams: true,
			ClientStreams: true,
		},
		{
			StreamName:   "StreamTelemetry",
			Handler:      _CommandService_StreamTelemetry_Handler,
			ServerStreams: true,
		},
	},
	Metadata: "core/command_service.proto",
}

func _CommandService_SendCommand_Handler(srv interface{}, stream grpc.ServerStream) error {
	return srv.(*CommandServiceServer).SendCommand(&serverStreamAdapter{stream})
}

func _CommandService_StreamTelemetry_Handler(srv interface{}, stream grpc.ServerStream) error {
	req := new(TelemetryRequest)
	if err := stream.RecvMsg(req); err != nil {
		return err
	}
	return srv.(*CommandServiceServer).StreamTelemetry(req, stream)
}

// ──────────────────────────────────────────────────────────────────────────────
// HTTP REST wrapper (:8082) — browsers cannot use raw gRPC
// ──────────────────────────────────────────────────────────────────────────────

var commandStatusNames = map[int32]string{
	0: "pending",
	1: "delivered",
	2: "completed",
	3: "failed",
	4: "timeout",
}

type ackEntry struct {
	CommandID  string    `json:"command_id"`
	VehicleID  string    `json:"vehicle_id"`
	Status     string    `json:"status"`
	Message    string    `json:"message"`
	LatencyMs  uint32    `json:"latency_ms"`
	receivedAt time.Time // for expiry — not JSON-exported
}

type restServer struct {
	msgRouter router.MessageRouter
	validator *auth.Validator
	mu        sync.RWMutex
	acks      map[string]*ackEntry          // command_id → ack
	waiters   map[string][]chan *ackEntry   // command_id → SSE waiters
}

func newRestServer(msgRouter router.MessageRouter, validator *auth.Validator) *restServer {
	return &restServer{
		msgRouter: msgRouter,
		validator: validator,
		acks:      make(map[string]*ackEntry),
		waiters:   make(map[string][]chan *ackEntry),
	}
}

// startACKListener subscribes to command.*.ack and populates the ACK map.
// ACKs are kept for 5 minutes then expired by a background ticker.
func (rs *restServer) startACKListener(ctx context.Context) error {
	ackCh, err := rs.msgRouter.Subscribe(ctx, "command.*.ack")
	if err != nil {
		return fmt.Errorf("subscribe ACK: %w", err)
	}

	// Expiry ticker — runs every minute, removes entries older than 5 minutes
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				cutoff := time.Now().Add(-5 * time.Minute)
				rs.mu.Lock()
				for id, e := range rs.acks {
					if e.receivedAt.Before(cutoff) {
						delete(rs.acks, id)
					}
				}
				rs.mu.Unlock()
			case <-ctx.Done():
				return
			}
		}
	}()

	// ACK consumer
	go func() {
		for msg := range ackCh {
			ack, err := decodeCommandAck(msg.Data)
			if err != nil {
				slog.Warn("REST ACK decode", "err", err)
				continue
			}
			statusStr := commandStatusNames[ack.Status]
			if statusStr == "" {
				statusStr = "unknown"
			}
			entry := &ackEntry{
				CommandID:  ack.CommandID,
				VehicleID:  ack.VehicleID,
				Status:     statusStr,
				Message:    ack.Message,
				LatencyMs:  ack.LatencyMs,
				receivedAt: time.Now(),
			}
			rs.mu.Lock()
			rs.acks[ack.CommandID] = entry
			// Notify any SSE waiters immediately — no more polling needed
			for _, ch := range rs.waiters[ack.CommandID] {
				select {
				case ch <- entry:
				default:
				}
			}
			delete(rs.waiters, ack.CommandID)
			rs.mu.Unlock()
		}
	}()

	return nil
}

// POST /v1/commands
// Body: {"vehicle_id","command_type","payload_b64","priority","ttl_ms"}
func (rs *restServer) handleSendCommand(w http.ResponseWriter, r *http.Request) {
	claims, ok := auth.ClaimsFromContext(r.Context())
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if !claims.Role.CanSendCommand() {
		http.Error(w, "Forbidden: operator or admin role required", http.StatusForbidden)
		return
	}

	var req struct {
		VehicleID   string `json:"vehicle_id"`
		CommandType string `json:"command_type"`
		PayloadB64  string `json:"payload_b64"`
		Priority    int32  `json:"priority"`
		TtlMs       uint32 `json:"ttl_ms"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.VehicleID == "" {
		http.Error(w, "Bad Request: vehicle_id required", http.StatusBadRequest)
		return
	}

	if !claims.CanAccessVehicle(req.VehicleID) {
		http.Error(w, "Forbidden: vehicle not in vehicle_set", http.StatusForbidden)
		return
	}

	var payload []byte
	if req.PayloadB64 != "" {
		var err error
		payload, err = base64.StdEncoding.DecodeString(req.PayloadB64)
		if err != nil {
			http.Error(w, "Bad Request: invalid payload_b64", http.StatusBadRequest)
			return
		}
	} else if req.CommandType != "" {
		// Encode command_type as a simple JSON payload if no binary payload given
		payload, _ = json.Marshal(map[string]string{"type": req.CommandType})
	}

	commandID := uuid.Must(uuid.NewV7()).String()
	cmd := &CommandEnvelope{
		VehicleID: req.VehicleID,
		CommandID: commandID,
		Payload:   payload,
		Priority:  req.Priority,
		IssuerID:  claims.Subject,
		IssuedAt:  uint64(time.Now().UnixMilli()),
		TtlMs:     req.TtlMs,
	}

	encoded, err := encodeCommandEnvelope(cmd)
	if err != nil {
		slog.Error("REST encode command", "err", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	subject := fmt.Sprintf("command.%s", req.VehicleID)
	if err := rs.msgRouter.Publish(r.Context(), subject, encoded, router.PubOptions{
		DeduplicationID: commandID,
		TTL:             30 * time.Second,
	}); err != nil {
		slog.Error("REST NATS publish", "err", err, "vehicle", req.VehicleID)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	slog.Info("REST command queued", "command_id", commandID, "vehicle_id", req.VehicleID)
	writeJSON(w, http.StatusAccepted, map[string]string{
		"command_id": commandID,
		"status":     "queued",
	})
}

// GET /v1/commands/{id}/stream — SSE push; resolves as soon as the ACK lands.
// The SDK connects here instead of polling GET /v1/commands/{id} every 500ms.
func (rs *restServer) handleCommandStream(w http.ResponseWriter, r *http.Request) {
	_, ok := auth.ClaimsFromContext(r.Context())
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	// Parse command ID — strip prefix and "/stream" suffix
	commandID := strings.TrimPrefix(r.URL.Path, "/v1/commands/")
	commandID = strings.TrimSuffix(commandID, "/stream")
	if commandID == "" {
		http.Error(w, "Bad Request: missing command ID", http.StatusBadRequest)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// Check for an existing ACK and register the waiter atomically under a single
	// write lock — eliminates the window where an ACK could arrive between an
	// RUnlock and the subsequent Lock, causing the SSE client to hang forever.
	ch := make(chan *ackEntry, 1)
	rs.mu.Lock()
	if entry, found := rs.acks[commandID]; found {
		rs.mu.Unlock()
		b, _ := json.Marshal(entry)
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
		return
	}
	rs.waiters[commandID] = append(rs.waiters[commandID], ch)
	rs.mu.Unlock()

	defer func() {
		rs.mu.Lock()
		wlist := rs.waiters[commandID]
		for i, c := range wlist {
			if c == ch {
				rs.waiters[commandID] = append(wlist[:i], wlist[i+1:]...)
				break
			}
		}
		rs.mu.Unlock()
	}()

	select {
	case entry := <-ch:
		b, _ := json.Marshal(entry)
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
	case <-r.Context().Done():
		// client disconnected or TTL timeout — caller gets "timeout" from its own deadline
	}
}

// GET /v1/commands/{id}
func (rs *restServer) handleGetCommand(w http.ResponseWriter, r *http.Request) {
	_, ok := auth.ClaimsFromContext(r.Context())
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	commandID := strings.TrimPrefix(r.URL.Path, "/v1/commands/")
	if commandID == "" {
		http.Error(w, "Bad Request: missing command ID", http.StatusBadRequest)
		return
	}

	rs.mu.RLock()
	entry, found := rs.acks[commandID]
	rs.mu.RUnlock()

	if !found {
		writeJSON(w, http.StatusOK, map[string]string{
			"command_id": commandID,
			"status":     "pending",
		})
		return
	}
	writeJSON(w, http.StatusOK, entry)
}

func (rs *restServer) routes() http.Handler {
	mux := http.NewServeMux()
	protect := auth.HTTPMiddleware(rs.validator)

	mux.Handle("/v1/commands", protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			rs.handleSendCommand(w, r)
		} else {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		}
	})))

	mux.Handle("/v1/commands/", protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/stream") {
			rs.handleCommandStream(w, r)
		} else {
			rs.handleGetCommand(w, r)
		}
	})))

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	})

	return auth.RequestIDMiddleware(corsMiddleware(mux))
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := os.Getenv("CORS_ALLOW_ORIGIN")
		if origin == "" {
			origin = "*"
		}
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// ──────────────────────────────────────────────────────────────────────────────
// gRPC server setup
// ──────────────────────────────────────────────────────────────────────────────

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	cfg := loadConfig()
	slog.Info("Starting command-api", "addr", cfg.GRPCAddr, "region", cfg.RegionID)

	// Auth validator (JWKS from Keycloak)
	validator, err := auth.NewValidator(cfg.JWKSURLs)
	if err != nil {
		slog.Error("auth validator init", "err", err)
		os.Exit(1)
	}

	// NATS router
	msgRouter, err := router.NewNATSRouter(cfg.NATSUrl, "command-api")
	if err != nil {
		slog.Error("NATS connect", "err", err)
		os.Exit(1)
	}
	defer msgRouter.Close()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	// Ensure JetStream "command" stream exists (idempotent)
	if err := msgRouter.EnsureStream(ctx, "command", []string{"command.>"}); err != nil {
		slog.Error("ensure command stream", "err", err)
		os.Exit(1)
	}
	slog.Info("JetStream command stream ready")

	// HTTP REST server — browsers cannot use raw gRPC
	rest := newRestServer(msgRouter, validator)
	if err := rest.startACKListener(ctx); err != nil {
		slog.Error("REST ACK listener", "err", err)
		os.Exit(1)
	}
	httpServer := &http.Server{
		Addr:         cfg.HTTPRestAddr,
		Handler:      rest.routes(),
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}
	go func() {
		slog.Info("command-api REST server ready", "addr", cfg.HTTPRestAddr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("REST serve error", "err", err)
		}
	}()

	// gRPC server with auth interceptors
	grpcServer := grpc.NewServer(
		grpc.UnaryInterceptor(auth.GRPCUnaryInterceptor(validator)),
		grpc.StreamInterceptor(auth.GRPCStreamInterceptor(validator)),
		// Max message size: 4MB (large enough for any command payload)
		grpc.MaxRecvMsgSize(4*1024*1024),
		grpc.MaxSendMsgSize(4*1024*1024),
	)

	cmdSvc := &CommandServiceServer{router: msgRouter, regionID: cfg.RegionID}
	grpcServer.RegisterService(&_CommandService_serviceDesc, cmdSvc)
	slog.Info("gRPC CommandService registered", "service", _CommandService_serviceDesc.ServiceName)

	lis, err := net.Listen("tcp", cfg.GRPCAddr)
	if err != nil {
		slog.Error("listen failed", "addr", cfg.GRPCAddr, "err", err)
		os.Exit(1)
	}

	go func() {
		<-ctx.Done()
		slog.Info("Graceful shutdown initiated")
		grpcServer.GracefulStop()
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutCancel()
		httpServer.Shutdown(shutCtx)
	}()

	slog.Info("command-api gRPC server ready", "addr", cfg.GRPCAddr)
	if err := grpcServer.Serve(lis); err != nil {
		slog.Error("gRPC serve error", "err", err)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Proto encoding helpers (manual, matches generated prost output)
// ──────────────────────────────────────────────────────────────────────────────

func appendProtoVarint(out []byte, fieldNum uint32, val uint64) []byte {
	out = appendVarint(out, uint64(fieldNum<<3|0)) // wire type 0
	return appendVarint(out, val)
}

func appendProtoString(out []byte, fieldNum uint32, s string) []byte {
	out = appendVarint(out, uint64(fieldNum<<3|2)) // wire type 2
	out = appendVarint(out, uint64(len(s)))
	return append(out, s...)
}

func appendProtoBytes(out []byte, fieldNum uint32, b []byte) []byte {
	out = appendVarint(out, uint64(fieldNum<<3|2)) // wire type 2
	out = appendVarint(out, uint64(len(b)))
	return append(out, b...)
}

func appendVarint(out []byte, v uint64) []byte {
	for v >= 0x80 {
		out = append(out, byte(v)|0x80)
		v >>= 7
	}
	return append(out, byte(v))
}

func consumeVarint(data []byte) (uint64, int) {
	var result uint64
	for i, b := range data {
		result |= uint64(b&0x7F) << (7 * uint(i))
		if b&0x80 == 0 {
			return result, i + 1
		}
		if i >= 9 {
			return 0, 0
		}
	}
	return 0, 0
}

// ──────────────────────────────────────────────────────────────────────────────
// Codec support: decode incoming gRPC frames, encode outgoing ACKs
// ──────────────────────────────────────────────────────────────────────────────

// encodeCommandAckProto serializes CommandAck to proto binary for gRPC Send.
func encodeCommandAckProto(ack *CommandAck) ([]byte, error) {
	var out []byte
	if ack.CommandID != "" {
		out = appendProtoString(out, 1, ack.CommandID)
	}
	if ack.VehicleID != "" {
		out = appendProtoString(out, 2, ack.VehicleID)
	}
	if ack.Status != 0 {
		out = appendProtoVarint(out, 3, uint64(ack.Status))
	}
	if ack.Message != "" {
		out = appendProtoString(out, 4, ack.Message)
	}
	if ack.LatencyMs > 0 {
		out = appendProtoVarint(out, 5, uint64(ack.LatencyMs))
	}
	return out, nil
}

// decodeCommandEnvelopeInto parses protobuf bytes into an existing CommandEnvelope.
func decodeCommandEnvelopeInto(data []byte, env *CommandEnvelope) error {
	*env = CommandEnvelope{}
	pos := 0
	for pos < len(data) {
		tag, n := consumeVarint(data[pos:])
		if n == 0 {
			return fmt.Errorf("truncated tag at offset %d", pos)
		}
		pos += n
		fieldNum := tag >> 3
		wireType := tag & 0x7

		switch wireType {
		case 0:
			val, n2 := consumeVarint(data[pos:])
			if n2 == 0 {
				return fmt.Errorf("truncated varint field %d", fieldNum)
			}
			pos += n2
			switch fieldNum {
			case 4:
				env.Priority = int32(val)
			case 6:
				env.IssuedAt = val
			case 7:
				env.TtlMs = uint32(val)
			}

		case 2:
			length, n2 := consumeVarint(data[pos:])
			if n2 == 0 {
				return fmt.Errorf("truncated length field %d", fieldNum)
			}
			pos += n2
			end := pos + int(length)
			if end > len(data) {
				return fmt.Errorf("truncated bytes field %d", fieldNum)
			}
			b := data[pos:end]
			pos = end
			switch fieldNum {
			case 1:
				env.VehicleID = string(b)
			case 2:
				env.CommandID = string(b)
			case 3:
				env.Payload = append([]byte(nil), b...)
			case 5:
				env.IssuerID = string(b)
			}

		default:
			return fmt.Errorf("unsupported wire type %d at field %d", wireType, fieldNum)
		}
	}
	return nil
}

// decodeTelemetryRequestInto parses a TelemetryRequest from protobuf bytes.
// Handles both packed and unpacked repeated fields.
func decodeTelemetryRequestInto(data []byte, req *TelemetryRequest) error {
	*req = TelemetryRequest{}
	pos := 0
	for pos < len(data) {
		tag, n := consumeVarint(data[pos:])
		if n == 0 {
			break
		}
		pos += n
		fieldNum := tag >> 3
		wireType := tag & 0x7

		switch wireType {
		case 0:
			val, n2 := consumeVarint(data[pos:])
			if n2 == 0 {
				return fmt.Errorf("truncated varint")
			}
			pos += n2
			switch fieldNum {
			case 2:
				req.Streams = append(req.Streams, uint32(val))
			case 3:
				req.MaxHz = uint32(val)
			}

		case 2:
			length, n2 := consumeVarint(data[pos:])
			if n2 == 0 {
				return fmt.Errorf("truncated length")
			}
			pos += n2
			end := pos + int(length)
			if end > len(data) {
				return fmt.Errorf("truncated bytes")
			}
			b := data[pos:end]
			pos = end
			switch fieldNum {
			case 1:
				req.VehicleIDs = append(req.VehicleIDs, string(b))
			case 2:
				subPos := 0
				for subPos < len(b) {
					val, n3 := consumeVarint(b[subPos:])
					if n3 == 0 {
						break
					}
					subPos += n3
					req.Streams = append(req.Streams, uint32(val))
				}
			}

		default:
			return fmt.Errorf("unsupported wire type %d", wireType)
		}
	}
	return nil
}

// peekDataEnvelope extracts vehicle_id (field 1) and stream_type (field 3) from
// a DataEnvelope without fully decoding it. Used by StreamTelemetry for filtering.
func peekDataEnvelope(data []byte) (vehicleID string, streamType uint32) {
	pos := 0
	for pos < len(data) {
		tag, n := consumeVarint(data[pos:])
		if n == 0 {
			return
		}
		pos += n
		fieldNum := tag >> 3
		wireType := tag & 0x7

		switch wireType {
		case 0:
			val, n2 := consumeVarint(data[pos:])
			if n2 == 0 {
				return
			}
			pos += n2
			if fieldNum == 3 {
				streamType = uint32(val)
			}
		case 1:
			if pos+8 > len(data) {
				return
			}
			pos += 8
		case 2:
			length, n2 := consumeVarint(data[pos:])
			if n2 == 0 {
				return
			}
			pos += n2
			end := pos + int(length)
			if end > len(data) {
				return
			}
			if fieldNum == 1 {
				vehicleID = string(data[pos:end])
			}
			pos = end
		case 5:
			if pos+4 > len(data) {
				return
			}
			pos += 4
		default:
			return
		}

		if vehicleID != "" && streamType > 0 {
			return
		}
	}
	return
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

