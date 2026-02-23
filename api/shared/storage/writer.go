// Package storage defines the StorageWriter interface for time-series telemetry.
// The interface decouples all services from a specific database.
// Swap the database by providing a different implementation — no service code changes.
package storage

import (
	"context"
	"time"
)

// TelemetryPoint is a single decoded telemetry measurement ready for storage.
// Fields are the intersection of what all storage backends can efficiently store.
// Raw protobuf payload is also included for backends that store opaque blobs.
type TelemetryPoint struct {
	VehicleID   string    // partition key
	ProjectID   string    // logical project / fleet grouping (DataEnvelope.fleet_id)
	OrgID       string    // operator organization (DataEnvelope.org_id, filled by relay)
	StreamName  string    // custom sub-label within StreamType (DataEnvelope.stream_name)
	Timestamp   time.Time // nanosecond precision (stored as int64 ns since epoch)
	StreamType  string    // "telemetry" | "event" | "sensor" | "video_meta" | "log"
	Lat         float32   // WGS84 latitude (0 if not GPS data)
	Lon         float32   // WGS84 longitude
	AltM        float32   // altitude in meters
	SeqNum      uint32    // monotonic sequence number (for gap detection)
	PayloadSize int       // raw protobuf size in bytes (for bandwidth metrics)
	Payload     []byte    // raw DataEnvelope protobuf bytes (full fidelity archive)

	// Hierarchical key fields extracted from JSON payloads (W&B-style SDK data).
	// Keys use slash→double-underscore substitution for ILP column names:
	//   "sensors/temp" → ILP field "sensors__temp=85.0"
	// Enables fast range queries on individual sensor keys without JSON parsing.
	HierarchicalFields map[string]float64

	// Full JSON blob of the payload (when payload was a JSON object).
	// Stored as a string field "payload_json" for arbitrary-key queries that
	// fall outside the indexed HierarchicalFields columns.
	PayloadJSON string

	// Low-cardinality tag columns extracted from SDK "_tags" payload key.
	// Stored as ILP tag columns (indexed as symbols in QuestDB).
	// Example: {"mission": "survey-1"} → tag column mission=survey-1
	Tags map[string]string
}

// StorageWriter is the interface all time-series storage backends implement.
// All methods must be safe to call from multiple goroutines concurrently.
type StorageWriter interface {
	// WriteBatch writes multiple telemetry points in a single I/O operation.
	// The batch is committed atomically. On error, the entire batch is retried
	// by the caller. Implementations should aim for <5ms latency per 1000 points.
	WriteBatch(ctx context.Context, points []TelemetryPoint) error

	// Flush forces any buffered writes to the underlying storage.
	// Called on graceful shutdown.
	Flush(ctx context.Context) error

	// Close releases resources. No further writes after Close.
	Close() error
}

// BatchAccumulator is a helper for services that receive one message at a time
// but want to batch writes for throughput. It is not part of the StorageWriter
// interface — it's a utility used by telemetry-ingest.
type BatchAccumulator struct {
	points   []TelemetryPoint
	maxSize  int
	maxAge   time.Duration
	lastFlush time.Time
}

// NewBatchAccumulator creates an accumulator that flushes when maxSize points
// are accumulated OR maxAge has elapsed since the last flush, whichever is first.
func NewBatchAccumulator(maxSize int, maxAge time.Duration) *BatchAccumulator {
	return &BatchAccumulator{
		points:    make([]TelemetryPoint, 0, maxSize),
		maxSize:   maxSize,
		maxAge:    maxAge,
		lastFlush: time.Now(),
	}
}

// Add appends a point. Returns (points, true) when the batch is ready to flush,
// (nil, false) otherwise.
func (b *BatchAccumulator) Add(p TelemetryPoint) ([]TelemetryPoint, bool) {
	b.points = append(b.points, p)
	if len(b.points) >= b.maxSize || time.Since(b.lastFlush) >= b.maxAge {
		return b.Drain(), true
	}
	return nil, false
}

// Drain returns all accumulated points and resets the accumulator.
func (b *BatchAccumulator) Drain() []TelemetryPoint {
	out := b.points
	b.points = make([]TelemetryPoint, 0, b.maxSize)
	b.lastFlush = time.Now()
	return out
}
