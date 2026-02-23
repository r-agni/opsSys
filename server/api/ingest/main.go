// telemetry-ingest: bridges NATS JetStream to QuestDB via ILP (InfluxDB Line Protocol).
//
// Consumes protobuf-encoded DataEnvelope messages from the "telemetry.>" NATS
// subjects, converts them to QuestDB ILP lines, and writes in batches over TCP.
// No HTTP endpoints except /healthz for Docker health checks.
package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/systemscale/services/shared/router"
)

// ──────────────────────────────────────────────────────────────────────────────
// Configuration
// ──────────────────────────────────────────────────────────────────────────────

type config struct {
	NATSUrl       string
	QuestDBAddr   string
	BatchSize     int
	BatchInterval time.Duration
	HTTPAddr      string
}

func loadConfig() config {
	batchSize := 500
	if s := os.Getenv("BATCH_SIZE"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			batchSize = n
		}
	}

	batchInterval := 500 * time.Millisecond
	if s := os.Getenv("BATCH_INTERVAL"); s != "" {
		if d, err := time.ParseDuration(s); err == nil && d > 0 {
			batchInterval = d
		}
	}

	return config{
		NATSUrl:       envOr("NATS_URL", "nats://localhost:4222"),
		QuestDBAddr:   envOr("QUESTDB_ADDR", "localhost:9009"),
		BatchSize:     batchSize,
		BatchInterval: batchInterval,
		HTTPAddr:      envOr("HTTP_ADDR", ":8085"),
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Main
// ──────────────────────────────────────────────────────────────────────────────

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	cfg := loadConfig()
	slog.Info("starting telemetry-ingest",
		"nats", cfg.NATSUrl,
		"questdb", cfg.QuestDBAddr,
		"batch_size", cfg.BatchSize,
		"batch_interval", cfg.BatchInterval,
	)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	nr, err := router.NewNATSRouter(cfg.NATSUrl, "telemetry-ingest")
	if err != nil {
		slog.Error("NATS connect", "err", err)
		os.Exit(1)
	}
	defer nr.Close()
	slog.Info("NATS connected")

	if err := nr.EnsureStream(ctx, "telemetry", []string{"telemetry.>"}); err != nil {
		slog.Error("ensure telemetry stream", "err", err)
		os.Exit(1)
	}
	slog.Info("JetStream telemetry stream ready")

	writer, err := newILPWriter(cfg.QuestDBAddr, cfg.BatchSize, cfg.BatchInterval)
	if err != nil {
		slog.Error("ILP writer init", "err", err)
		os.Exit(1)
	}
	defer writer.Close()
	slog.Info("QuestDB ILP writer ready")

	ch, err := nr.Subscribe(ctx, "telemetry.>", router.SubOptions{Durable: "ingest-worker"})
	if err != nil {
		slog.Error("subscribe telemetry", "err", err)
		os.Exit(1)
	}

	go processMessages(ch, writer)

	httpServer := &http.Server{
		Addr: cfg.HTTPAddr,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, "ok")
		}),
	}
	go func() {
		slog.Info("ingest health check ready", "addr", cfg.HTTPAddr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("health check serve", "err", err)
		}
	}()

	slog.Info("telemetry-ingest ready")
	<-ctx.Done()

	slog.Info("shutting down telemetry-ingest")
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutCancel()
	httpServer.Shutdown(shutCtx)
}

func processMessages(ch <-chan *router.Message, writer *ilpWriter) {
	var processed, errors uint64
	for msg := range ch {
		env, err := decodeDataEnvelope(msg.Data)
		if err != nil {
			slog.Warn("decode DataEnvelope", "err", err, "subject", msg.Subject)
			errors++
			continue
		}

		line := buildILPLine(env)
		writer.Add(line)
		processed++

		if processed%10000 == 0 {
			slog.Info("ingest progress", "processed", processed, "errors", errors)
		}
	}
	slog.Info("message channel closed", "total_processed", processed, "total_errors", errors)
}

// ──────────────────────────────────────────────────────────────────────────────
// DataEnvelope — manual protobuf decoding (matches proto/core/envelope.proto)
// ──────────────────────────────────────────────────────────────────────────────

type dataEnvelope struct {
	VehicleID   string  // field 1  (wire type 2, string)
	TimestampNs uint64  // field 2  (wire type 0, varint)
	StreamType  uint64  // field 3  (wire type 0, varint)
	Payload     []byte  // field 4  (wire type 2, bytes)
	Lat         float64 // field 5  (wire type 1, double)
	Lon         float64 // field 6  (wire type 1, double)
	Alt         float32 // field 7  (wire type 5, float)
	Seq         uint64  // field 8  (wire type 0, varint)
	FleetID     string  // field 16 (wire type 2, string) — project_id
	OrgID       string  // field 17 (wire type 2, string)
	StreamName  string  // field 18 (wire type 2, string)
}

func decodeDataEnvelope(data []byte) (*dataEnvelope, error) {
	env := &dataEnvelope{}
	pos := 0
	for pos < len(data) {
		tag, n := consumeVarint(data[pos:])
		if n == 0 {
			return nil, fmt.Errorf("truncated tag at offset %d", pos)
		}
		pos += n
		fieldNum := tag >> 3
		wireType := tag & 0x7

		switch wireType {
		case 0: // varint
			val, n2 := consumeVarint(data[pos:])
			if n2 == 0 {
				return nil, fmt.Errorf("truncated varint field %d", fieldNum)
			}
			pos += n2
			switch fieldNum {
			case 2:
				env.TimestampNs = val
			case 3:
				env.StreamType = val
			case 8:
				env.Seq = val
			}

		case 1: // 64-bit fixed (double)
			if pos+8 > len(data) {
				return nil, fmt.Errorf("truncated fixed64 field %d", fieldNum)
			}
			bits := binary.LittleEndian.Uint64(data[pos : pos+8])
			pos += 8
			switch fieldNum {
			case 5:
				env.Lat = math.Float64frombits(bits)
			case 6:
				env.Lon = math.Float64frombits(bits)
			}

		case 2: // length-delimited (string, bytes)
			length, n2 := consumeVarint(data[pos:])
			if n2 == 0 {
				return nil, fmt.Errorf("truncated length field %d", fieldNum)
			}
			pos += n2
			end := pos + int(length)
			if end > len(data) {
				return nil, fmt.Errorf("truncated bytes field %d", fieldNum)
			}
			b := data[pos:end]
			pos = end
			switch fieldNum {
			case 1:
				env.VehicleID = string(b)
			case 4:
				env.Payload = append([]byte(nil), b...) // copy
			case 16:
				env.FleetID = string(b)
			case 17:
				env.OrgID = string(b)
			case 18:
				env.StreamName = string(b)
			}

		case 5: // 32-bit fixed (float)
			if pos+4 > len(data) {
				return nil, fmt.Errorf("truncated fixed32 field %d", fieldNum)
			}
			bits := binary.LittleEndian.Uint32(data[pos : pos+4])
			pos += 4
			switch fieldNum {
			case 7:
				env.Alt = math.Float32frombits(bits)
			}

		default:
			return nil, fmt.Errorf("unsupported wire type %d at field %d", wireType, fieldNum)
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
			return 0, 0
		}
	}
	return 0, 0
}

// ──────────────────────────────────────────────────────────────────────────────
// ILP line builder
// ──────────────────────────────────────────────────────────────────────────────

var streamTypeNames = map[uint64]string{
	1: "telemetry",
	2: "event",
	3: "sensor",
	5: "log",
}

func buildILPLine(env *dataEnvelope) []byte {
	stName := streamTypeNames[env.StreamType]
	if stName == "" {
		stName = fmt.Sprintf("type_%d", env.StreamType)
	}

	var buf bytes.Buffer

	// Measurement + tags (no spaces between tags)
	buf.WriteString("telemetry,vehicle_id=")
	buf.WriteString(escapeTagValue(env.VehicleID))
	buf.WriteString(",stream_type=")
	buf.WriteString(escapeTagValue(stName))
	if env.FleetID != "" {
		buf.WriteString(",project_id=")
		buf.WriteString(escapeTagValue(env.FleetID))
	}
	if env.OrgID != "" {
		buf.WriteString(",org_id=")
		buf.WriteString(escapeTagValue(env.OrgID))
	}
	if env.StreamName != "" {
		buf.WriteString(",stream_name=")
		buf.WriteString(escapeTagValue(env.StreamName))
	}

	// Fields (space-separated from tags)
	buf.WriteByte(' ')
	buf.WriteString("lat=")
	buf.WriteString(formatILPFloat(env.Lat))
	buf.WriteString(",lon=")
	buf.WriteString(formatILPFloat(env.Lon))
	buf.WriteString(",alt_m=")
	buf.WriteString(formatILPFloat(float64(env.Alt)))
	buf.WriteString(",seq=")
	buf.WriteString(strconv.FormatUint(env.Seq, 10))
	buf.WriteByte('i')

	if len(env.Payload) > 0 {
		buf.WriteString(",payload_size=")
		buf.WriteString(strconv.Itoa(len(env.Payload)))
		buf.WriteByte('i')

		buf.WriteString(",payload_json=\"")
		buf.WriteString(escapeFieldString(string(env.Payload)))
		buf.WriteByte('"')

		if env.StreamType == 1 {
			appendTelemetryFields(&buf, env.Payload)
		}
	}

	// Timestamp in nanoseconds
	buf.WriteByte(' ')
	if env.TimestampNs > 0 {
		buf.WriteString(strconv.FormatUint(env.TimestampNs, 10))
	} else {
		buf.WriteString(strconv.FormatInt(time.Now().UnixNano(), 10))
	}

	return buf.Bytes()
}

// appendTelemetryFields extracts battery_pct, speed_ms, heading from the JSON
// payload and appends them as ILP fields for efficient querying in QuestDB.
func appendTelemetryFields(buf *bytes.Buffer, payload []byte) {
	var m map[string]interface{}
	if json.Unmarshal(payload, &m) != nil {
		return
	}

	if v, ok := m["battery_pct"]; ok {
		if f, ok := toFloat64(v); ok {
			buf.WriteString(",battery_pct=")
			buf.WriteString(strconv.FormatInt(int64(f), 10))
			buf.WriteByte('i')
		}
	}
	if v, ok := m["speed_ms"]; ok {
		if f, ok := toFloat64(v); ok {
			buf.WriteString(",speed_ms=")
			buf.WriteString(formatILPFloat(f))
		}
	}
	if v, ok := m["heading"]; ok {
		if f, ok := toFloat64(v); ok {
			buf.WriteString(",heading=")
			buf.WriteString(formatILPFloat(f))
		}
	}
}

func toFloat64(v interface{}) (float64, bool) {
	switch val := v.(type) {
	case float64:
		return val, true
	case float32:
		return float64(val), true
	case int:
		return float64(val), true
	case int64:
		return float64(val), true
	case json.Number:
		f, err := val.Float64()
		return f, err == nil
	}
	return 0, false
}

func formatILPFloat(v float64) string {
	s := strconv.FormatFloat(v, 'f', -1, 64)
	if !strings.Contains(s, ".") {
		return s + ".0"
	}
	return s
}

func escapeTagValue(s string) string {
	s = strings.ReplaceAll(s, " ", "\\ ")
	s = strings.ReplaceAll(s, ",", "\\,")
	s = strings.ReplaceAll(s, "=", "\\=")
	return s
}

func escapeFieldString(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "\"", "\\\"")
	s = strings.ReplaceAll(s, "\n", "\\n")
	s = strings.ReplaceAll(s, "\r", "\\r")
	return s
}

// ──────────────────────────────────────────────────────────────────────────────
// ILP TCP writer — batched writes to QuestDB
// ──────────────────────────────────────────────────────────────────────────────

type ilpWriter struct {
	addr          string
	conn          net.Conn
	mu            sync.Mutex
	buf           bytes.Buffer
	count         int
	batchSize     int
	batchInterval time.Duration
	ticker        *time.Ticker
	done          chan struct{}
}

func newILPWriter(addr string, batchSize int, batchInterval time.Duration) (*ilpWriter, error) {
	w := &ilpWriter{
		addr:          addr,
		batchSize:     batchSize,
		batchInterval: batchInterval,
		done:          make(chan struct{}),
	}
	if err := w.connect(); err != nil {
		return nil, fmt.Errorf("ILP connect %s: %w", addr, err)
	}
	w.ticker = time.NewTicker(batchInterval)
	go w.flushLoop()
	return w, nil
}

func (w *ilpWriter) connect() error {
	conn, err := net.DialTimeout("tcp", w.addr, 5*time.Second)
	if err != nil {
		return err
	}
	w.conn = conn
	return nil
}

// Add appends an ILP line to the buffer and flushes if the batch is full.
func (w *ilpWriter) Add(line []byte) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf.Write(line)
	w.buf.WriteByte('\n')
	w.count++
	if w.count >= w.batchSize {
		w.flush()
	}
}

// flush writes the buffered ILP lines to QuestDB over TCP.
// Retries up to 3 times with reconnection on failure; drops batch if exhausted.
// Caller must hold w.mu.
func (w *ilpWriter) flush() {
	if w.count == 0 {
		return
	}
	data := make([]byte, w.buf.Len())
	copy(data, w.buf.Bytes())

	for attempt := 0; attempt < 3; attempt++ {
		if w.conn == nil {
			if err := w.connect(); err != nil {
				slog.Error("ILP reconnect failed", "err", err, "attempt", attempt+1)
				time.Sleep(time.Duration(attempt+1) * 100 * time.Millisecond)
				continue
			}
		}

		w.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		_, err := w.conn.Write(data)
		if err == nil {
			w.buf.Reset()
			w.count = 0
			return
		}

		slog.Error("ILP write failed", "err", err, "attempt", attempt+1)
		w.conn.Close()
		w.conn = nil
	}

	slog.Error("ILP flush failed after retries, dropping batch", "lines", w.count)
	w.buf.Reset()
	w.count = 0
}

func (w *ilpWriter) flushLoop() {
	for {
		select {
		case <-w.ticker.C:
			w.mu.Lock()
			w.flush()
			w.mu.Unlock()
		case <-w.done:
			return
		}
	}
}

func (w *ilpWriter) Close() {
	close(w.done)
	w.ticker.Stop()
	w.mu.Lock()
	w.flush()
	w.mu.Unlock()
	if w.conn != nil {
		w.conn.Close()
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Utility
// ──────────────────────────────────────────────────────────────────────────────

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
