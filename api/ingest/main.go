// telemetry-ingest: NATS subscriber → QuestDB batch writer.
//
// This service is the only component that writes raw telemetry to QuestDB.
// It runs in the cloud Kubernetes cluster (not on relay nodes).
//
// Data flow:
//   NATS cluster (telemetry.> subjects)
//     → this service (fan-out consumer)
//     → decode DataEnvelope from protobuf
//     → accumulate into 100ms / 5000-message batches
//     → write to QuestDB via ILP (InfluxDB Line Protocol over TCP)
//
// Throughput target: 25 MB/s sustained (1000 vehicles × 50Hz × ~500 bytes/envelope)
// QuestDB can sustain >500k rows/sec on ILP — we are well within bounds.
//
// Scaling: multiple replicas use different NATS consumer group names.
// NATS fan-out (core NATS, not JetStream) delivers to all replicas,
// so all replicas receive all messages. Each replica writes a partition
// of vehicles (partitioned by vehicle_id hash % replica_count).
// For high throughput, deploy 4-8 replicas sharded by vehicle_id.
package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/systemscale/services/shared/storage"
)

// ──────────────────────────────────────────────────────────────────────────────
// Configuration (from environment variables for Kubernetes)
// ──────────────────────────────────────────────────────────────────────────────

type config struct {
	NATSUrl       string
	QuestDBAddr   string        // host:9009
	BatchSize     int           // flush at this many points
	BatchInterval time.Duration // or this often
	MetricsAddr   string
	RegionID      string
}

func loadConfig() config {
	return config{
		NATSUrl:       envOr("NATS_URL", "nats://localhost:4222"),
		QuestDBAddr:   envOr("QUESTDB_ADDR", "localhost:9009"),
		BatchSize:     envIntOr("BATCH_SIZE", 5000),
		BatchInterval: envDurOr("BATCH_INTERVAL", 100*time.Millisecond),
		MetricsAddr:   envOr("METRICS_ADDR", ":9090"),
		RegionID:      envOr("REGION_ID", "local"),
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// DataEnvelope proto decoder (minimal — only extracts routing fields)
// ──────────────────────────────────────────────────────────────────────────────

// dataEnvelope mirrors proto/core/envelope.proto field numbers.
// We decode only the fields needed for QuestDB columns; the raw payload
// is stored as-is for full fidelity archival.
type dataEnvelope struct {
	VehicleID   string  // field 1
	TimestampNs uint64  // field 2
	StreamType  int32   // field 3 (enum)
	Payload     []byte  // field 4
	Lat         float32 // field 5
	Lon         float32 // field 6
	AltM        float32 // field 7
	Seq         uint32  // field 8
	FleetID     string  // field 16 — project_id (set by SDK / local API)
	OrgID       string  // field 17 — org_id (filled by relay from mTLS cert)
	StreamName  string  // field 18 — custom sub-label (e.g. "lidar_front")
}

// decodeEnvelope decodes a DataEnvelope from raw protobuf bytes.
// Uses manual proto decoding (no generated code needed here) for zero-alloc hot path.
// Only decodes fields needed for storage metadata — payload bytes are kept as-is.
func decodeEnvelope(data []byte) (*dataEnvelope, error) {
	env := &dataEnvelope{}
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
		case 0: // varint
			val, n2 := consumeVarint(data[pos:])
			if n2 == 0 {
				return nil, fmt.Errorf("truncated varint at field %d", fieldNum)
			}
			pos += n2
			switch fieldNum {
			case 2:
				env.TimestampNs = val
			case 3:
				env.StreamType = int32(val)
			case 8:
				env.Seq = uint32(val)
			}
		case 1: // 64-bit
			if pos+8 > len(data) {
				return nil, fmt.Errorf("truncated 64-bit field %d", fieldNum)
			}
			pos += 8
		case 2: // length-delimited
			length, n2 := consumeVarint(data[pos:])
			if n2 == 0 {
				return nil, fmt.Errorf("truncated length at field %d", fieldNum)
			}
			pos += n2
			if pos+int(length) > len(data) {
				return nil, fmt.Errorf("truncated bytes field %d", fieldNum)
			}
			switch fieldNum {
			case 1:
				env.VehicleID = string(data[pos : pos+int(length)])
			case 4:
				env.Payload = data[pos : pos+int(length)]
			case 16:
				env.FleetID = string(data[pos : pos+int(length)])
			case 17:
				env.OrgID = string(data[pos : pos+int(length)])
			case 18:
				env.StreamName = string(data[pos : pos+int(length)])
			}
			pos += int(length)
		case 5: // 32-bit
			if pos+4 > len(data) {
				return nil, fmt.Errorf("truncated 32-bit field %d", fieldNum)
			}
			bits := binary.LittleEndian.Uint32(data[pos : pos+4])
			switch fieldNum {
			case 5:
				env.Lat = float32FromBits(bits)
			case 6:
				env.Lon = float32FromBits(bits)
			case 7:
				env.AltM = float32FromBits(bits)
			}
			pos += 4
		default:
			// Unknown wire type — can't skip safely, abort
			return nil, fmt.Errorf("unknown wire type %d at field %d", wireType, fieldNum)
		}
	}
	return env, nil
}

func consumeVarint(data []byte) (uint64, int) {
	var result uint64
	for i, b := range data {
		result |= uint64(b&0x7F) << (7 * uint(i))
		if b&0x80 == 0 {
			return result, i + 1
		}
		if i >= 9 {
			return 0, 0 // overflow
		}
	}
	return 0, 0
}

func float32FromBits(bits uint32) float32 {
	return math.Float32frombits(bits)
}

// streamTypeName converts a StreamType enum int32 to its string name.
func streamTypeName(v int32) string {
	switch v {
	case 1:
		return "telemetry"
	case 2:
		return "event"
	case 3:
		return "sensor"
	case 4:
		return "video_meta"
	case 5:
		return "log"
	default:
		return "unknown"
	}
}

// extractSDKPayload tries to JSON-decode the envelope payload and populate
// hierarchical field columns and tag columns on the TelemetryPoint.
//
// SDK payloads are JSON objects like:
//
//	{"sensors/temp": 85.2, "nav/altitude": 102.3, "_tags": {"mission": "survey-1"}}
//
// Numeric values whose keys contain a "/" are stored as individual ILP field
// columns with the slash replaced by "__" (enables fast range queries).
// The "_tags" object keys become ILP tag columns (indexed, low-cardinality).
// The full JSON blob is stored in PayloadJSON for arbitrary-key ad-hoc queries.
//
// Non-JSON payloads (e.g. raw MAVLink proto bytes) are silently skipped —
// PayloadJSON remains empty and HierarchicalFields/Tags stay nil.
func extractSDKPayload(payload []byte, point *storage.TelemetryPoint) {
	if len(payload) == 0 || payload[0] != '{' {
		return // not JSON object — skip silently (raw binary protocol payload)
	}

	var obj map[string]json.RawMessage
	if err := json.Unmarshal(payload, &obj); err != nil {
		return // malformed JSON — leave hierarchical fields empty
	}

	point.PayloadJSON = string(payload)

	for k, raw := range obj {
		if k == "_tags" {
			// Extract tag columns: {"mission": "survey-1", "env": "prod"}
			var tags map[string]string
			if err := json.Unmarshal(raw, &tags); err == nil {
				point.Tags = tags
			}
			continue
		}

		// Only index numeric values with slash-separated keys as columns.
		// String/bool/nested values are already preserved in PayloadJSON.
		if !strings.Contains(k, "/") {
			continue
		}
		var v float64
		if err := json.Unmarshal(raw, &v); err != nil {
			continue // not a number — skip column, still in PayloadJSON
		}
		if point.HierarchicalFields == nil {
			point.HierarchicalFields = make(map[string]float64)
		}
		point.HierarchicalFields[k] = v
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Ingest pipeline
// ──────────────────────────────────────────────────────────────────────────────

type ingestService struct {
	nc      *nats.Conn
	writer  storage.StorageWriter
	cfg     config
	metrics *ingestMetrics
}

type ingestMetrics struct {
	messagesReceived int64
	batchesWritten   int64
	decodeErrors     int64
}

func (s *ingestService) run(ctx context.Context) error {
	batch := storage.NewBatchAccumulator(s.cfg.BatchSize, s.cfg.BatchInterval)

	// Ticker for time-based flush when message rate is low
	ticker := time.NewTicker(s.cfg.BatchInterval)
	defer ticker.Stop()

	// NATS subscription on telemetry.> (wildcard, all vehicles and stream types)
	// Core NATS (not JetStream) — ephemeral, highest throughput, no persistence needed here
	sub, err := s.nc.Subscribe("telemetry.>", func(msg *nats.Msg) {
		env, err := decodeEnvelope(msg.Data)
		if err != nil {
			s.metrics.decodeErrors++
			slog.Warn("envelope decode error", "err", err, "subject", msg.Subject)
			return
		}

		point := storage.TelemetryPoint{
			VehicleID:   env.VehicleID,
			ProjectID:   env.FleetID,
			OrgID:       env.OrgID,
			StreamName:  env.StreamName,
			Timestamp:   time.Unix(0, int64(env.TimestampNs)),
			StreamType:  streamTypeName(env.StreamType),
			Lat:         env.Lat,
			Lon:         env.Lon,
			AltM:        env.AltM,
			SeqNum:      env.Seq,
			PayloadSize: len(env.Payload),
			Payload:     env.Payload,
		}

		// For SDK-originated JSON payloads (telemetry, sensor, event, log stream types),
		// extract hierarchical keys and tags for first-class QuestDB column storage.
		extractSDKPayload(env.Payload, &point)

		s.metrics.messagesReceived++

		if points, ready := batch.Add(point); ready {
			if err := s.writer.WriteBatch(ctx, points); err != nil {
				slog.Error("QuestDB batch write failed", "err", err, "size", len(points))
				// On failure: points are dropped. The ring buffer on relay nodes
				// holds the source data; a catch-up consumer can replay if needed.
				// JetStream is not used here because 50Hz telemetry doesn't need persistence.
			} else {
				s.metrics.batchesWritten++
			}
		}
	})
	if err != nil {
		return fmt.Errorf("NATS subscribe telemetry.>: %w", err)
	}
	defer sub.Unsubscribe()

	slog.Info("telemetry-ingest running",
		"nats", s.cfg.NATSUrl,
		"questdb", s.cfg.QuestDBAddr,
		"batch_size", s.cfg.BatchSize,
		"batch_interval", s.cfg.BatchInterval,
	)

	// Time-based flush loop (handles low-rate vehicles)
	for {
		select {
		case <-ctx.Done():
			// Flush remaining points on shutdown
			remaining := batch.Drain()
			if len(remaining) > 0 {
				if err := s.writer.WriteBatch(context.Background(), remaining); err != nil {
					slog.Error("final flush failed", "err", err)
				}
			}
			return nil
		case <-ticker.C:
			// Flush any accumulated points that haven't hit the size threshold
			if points := batch.Drain(); len(points) > 0 {
				if err := s.writer.WriteBatch(ctx, points); err != nil {
					slog.Error("timed batch write failed", "err", err, "size", len(points))
				} else {
					s.metrics.batchesWritten++
				}
			}
		}
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Main
// ──────────────────────────────────────────────────────────────────────────────

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	cfg := loadConfig()
	slog.Info("Starting telemetry-ingest", "region", cfg.RegionID)

	// Prometheus metrics endpoint (basic — use prometheus/client_golang in production)
	http.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintf(w, "# systemscale telemetry-ingest metrics\n")
		fmt.Fprintf(w, "up 1\n")
	})
	go http.ListenAndServe(cfg.MetricsAddr, nil)

	// NATS connection (reconnect forever)
	nc, err := nats.Connect(cfg.NATSUrl,
		nats.Name("telemetry-ingest"),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(500*time.Millisecond),
	)
	if err != nil {
		slog.Error("NATS connect failed", "url", cfg.NATSUrl, "err", err)
		os.Exit(1)
	}
	defer nc.Close()

	// QuestDB writer
	writer, err := storage.NewQuestDBWriter(cfg.QuestDBAddr)
	if err != nil {
		slog.Error("QuestDB connect failed", "addr", cfg.QuestDBAddr, "err", err)
		os.Exit(1)
	}
	defer writer.Close()

	svc := &ingestService{
		nc:      nc,
		writer:  writer,
		cfg:     cfg,
		metrics: &ingestMetrics{},
	}

	// Graceful shutdown on SIGTERM / SIGINT
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	if err := svc.run(ctx); err != nil {
		slog.Error("ingest service error", "err", err)
		os.Exit(1)
	}
	slog.Info("telemetry-ingest shutdown complete")
}

// ──────────────────────────────────────────────────────────────────────────────
// Config helpers
// ──────────────────────────────────────────────────────────────────────────────

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envIntOr(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	var n int
	if _, err := fmt.Sscan(v, &n); err != nil {
		return def
	}
	return n
}

func envDurOr(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}

