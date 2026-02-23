// Package storage — QuestDB ILP (InfluxDB Line Protocol) implementation of StorageWriter.
//
// QuestDB accepts ILP over TCP on port 9009 (default). ILP is a line-based
// text protocol that QuestDB ingests at very high speed using its WAL engine.
//
// ILP format per line:
//   measurement,tag_key=tag_val field_key=field_val timestamp_ns
//
// We use a single measurement name "telemetry" with vehicle_id as a tag
// (enables O(1) partition pruning on vehicle queries).
//
// Performance notes:
//   - Write in batches of 5000 lines or 100ms windows (whichever first)
//   - TCP connection is persistent (no per-write handshake)
//   - ILP is ~3x more compact than JSON, reducing TCP send size
//   - QuestDB's WAL engine commits at microsecond granularity
package storage

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
)

// QuestDBWriter implements StorageWriter for QuestDB via ILP over TCP.
type QuestDBWriter struct {
	conn    net.Conn
	mu      sync.Mutex // protects conn writes
	addr    string
	timeout time.Duration
}

// NewQuestDBWriter creates a persistent TCP connection to QuestDB's ILP endpoint.
// addr: "questdb-host:9009"
func NewQuestDBWriter(addr string) (*QuestDBWriter, error) {
	conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		return nil, fmt.Errorf("questdb connect %s: %w", addr, err)
	}
	// Set TCP_NODELAY: disable Nagle's algorithm for lowest latency per batch
	if tc, ok := conn.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
	}
	return &QuestDBWriter{
		conn:    conn,
		addr:    addr,
		timeout: 5 * time.Second,
	}, nil
}

// WriteBatch serializes all points as ILP lines and sends in a single TCP write.
// Single write = single syscall = minimum kernel crossing overhead.
func (w *QuestDBWriter) WriteBatch(ctx context.Context, points []TelemetryPoint) error {
	if len(points) == 0 {
		return nil
	}

	// Pre-allocate buffer: ~120 bytes per ILP line is a reasonable estimate
	buf := bytes.NewBuffer(make([]byte, 0, len(points)*120))

	for _, p := range points {
		// ILP line format:
		//   <measurement>,<tags> <fields> <timestamp_ns>
		//
		// Tag columns (indexed, low cardinality — stored as QuestDB SYMBOL):
		//   vehicle_id, stream_type, project_id, org_id, stream_name, + SDK _tags
		//
		// Field columns (values — stored as DOUBLE / LONG / STRING):
		//   lat, lon, alt_m, seq, payload_size
		//   + hierarchical fields from SDK (e.g. sensors__temp=85.0)
		//   + payload_json (full JSON blob for ad-hoc queries)

		// ── tags ────────────────────────────────────────────────────────────
		tagBuf := &strings.Builder{}
		tagBuf.WriteString("telemetry")
		tagBuf.WriteString(",vehicle_id=")
		tagBuf.WriteString(escapeILP(p.VehicleID))
		tagBuf.WriteString(",stream_type=")
		tagBuf.WriteString(escapeILP(p.StreamType))
		if p.ProjectID != "" {
			tagBuf.WriteString(",project_id=")
			tagBuf.WriteString(escapeILP(p.ProjectID))
		}
		if p.OrgID != "" {
			tagBuf.WriteString(",org_id=")
			tagBuf.WriteString(escapeILP(p.OrgID))
		}
		if p.StreamName != "" {
			tagBuf.WriteString(",stream_name=")
			tagBuf.WriteString(escapeILP(p.StreamName))
		}
		// SDK _tags → additional ILP tag columns (low-cardinality, indexed)
		for k, v := range p.Tags {
			tagBuf.WriteString(",")
			tagBuf.WriteString(escapeILP(k))
			tagBuf.WriteString("=")
			tagBuf.WriteString(escapeILP(v))
		}

		// ── fields ──────────────────────────────────────────────────────────
		fieldBuf := &strings.Builder{}
		fmt.Fprintf(fieldBuf, "lat=%f,lon=%f,alt_m=%f,seq=%di,payload_size=%di",
			p.Lat, p.Lon, p.AltM, p.SeqNum, p.PayloadSize)

		// Hierarchical sensor fields (sensors/temp → sensors__temp)
		for k, v := range p.HierarchicalFields {
			fieldBuf.WriteString(",")
			fieldBuf.WriteString(escapeILPFieldKey(k))
			fmt.Fprintf(fieldBuf, "=%f", v)
		}

		// Full JSON blob for arbitrary key queries
		if p.PayloadJSON != "" {
			fieldBuf.WriteString(",payload_json=")
			fieldBuf.WriteString(escapeILPString(p.PayloadJSON))
		}

		fmt.Fprintf(buf, "%s %s %d\n",
			tagBuf.String(),
			fieldBuf.String(),
			p.Timestamp.UnixNano(),
		)
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	w.conn.SetWriteDeadline(time.Now().Add(w.timeout))
	_, err := w.conn.Write(buf.Bytes())
	if err != nil {
		// Attempt reconnect on write failure
		if reconnErr := w.reconnect(); reconnErr != nil {
			return fmt.Errorf("questdb write failed and reconnect failed: write=%w, reconnect=%v", err, reconnErr)
		}
		// Retry once after reconnect
		w.conn.SetWriteDeadline(time.Now().Add(w.timeout))
		_, err = w.conn.Write(buf.Bytes())
		if err != nil {
			return fmt.Errorf("questdb write after reconnect: %w", err)
		}
	}
	return nil
}

func (w *QuestDBWriter) reconnect() error {
	if w.conn != nil {
		w.conn.Close()
	}
	conn, err := net.DialTimeout("tcp", w.addr, 10*time.Second)
	if err != nil {
		return err
	}
	if tc, ok := conn.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
	}
	w.conn = conn
	return nil
}

func (w *QuestDBWriter) Flush(_ context.Context) error {
	// ILP over TCP is streaming — no explicit flush needed.
	// QuestDB commits on the WAL side asynchronously.
	return nil
}

func (w *QuestDBWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.conn != nil {
		return w.conn.Close()
	}
	return nil
}

// escapeILP escapes special characters in ILP tag key/value strings.
// ILP tag values must not contain commas, spaces, or equals signs.
func escapeILP(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case ',', ' ', '=', '\n', '\r':
			out = append(out, '\\', s[i])
		default:
			out = append(out, s[i])
		}
	}
	return string(out)
}

// escapeILPFieldKey converts a hierarchical SDK key to a valid ILP field column name.
// Slashes are replaced with double-underscores ("sensors/temp" → "sensors__temp").
// Other ILP-unsafe characters are escaped.
func escapeILPFieldKey(key string) string {
	key = strings.ReplaceAll(key, "/", "__")
	return escapeILP(key)
}

// escapeILPString wraps a string value for use as an ILP field value.
// ILP string fields are double-quoted; internal double-quotes and backslashes
// must be escaped with a backslash.
func escapeILPString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteByte(s[i])
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		default:
			b.WriteByte(s[i])
		}
	}
	b.WriteByte('"')
	return b.String()
}

// Ensure QuestDBWriter implements StorageWriter at compile time.
var _ StorageWriter = (*QuestDBWriter)(nil)
