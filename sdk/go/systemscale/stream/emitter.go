// Package stream provides continuous (high-frequency) data emission and subscription.
package stream

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
)

// EmitOption is a functional option for StreamEmitter.Send().
type EmitOption func(*emitOpt)

type emitOpt struct {
	stream     string
	streamName string
	lat        float64
	lon        float64
	alt        float64
	tags       map[string]string
	to         string // "type:id" actor routing, e.g. "operator:*"
}

// WithStream sets the stream type for emitted frames.
func WithStream(s string) EmitOption { return func(o *emitOpt) { o.stream = s } }

// WithStreamName sets a sub-label within the stream type.
func WithStreamName(n string) EmitOption { return func(o *emitOpt) { o.streamName = n } }

// WithLocation attaches WGS84 coordinates to the emitted frame.
func WithLocation(lat, lon, altM float64) EmitOption {
	return func(o *emitOpt) { o.lat = lat; o.lon = lon; o.alt = altM }
}

// WithTags attaches low-cardinality string labels to the emitted frame.
func WithTags(tags map[string]string) EmitOption { return func(o *emitOpt) { o.tags = tags } }

// To routes the emitted frame to a specific actor type and ID.
//
// Format: "type:id" where type is one of "operator", "device", "human", "service".
// Use "*" as the ID to target all actors of that type (e.g. "operator:*").
// An empty string (default) broadcasts to all actors in the project.
//
//	emitter.Send(data, To("operator:ground-control"))
//	emitter.Send(data, To("operator:*"))
//	emitter.Send(data, To("device:drone-002"))
func To(actor string) EmitOption { return func(o *emitOpt) { o.to = actor } }

// emitFrame is the JSON body sent to /v1/log.
type emitFrame struct {
	Data       map[string]any    `json:"data"`
	Stream     string            `json:"stream"`
	StreamName string            `json:"stream_name,omitempty"`
	ProjectID  string            `json:"project_id,omitempty"`
	Tags       map[string]string `json:"tags,omitempty"`
	Lat        float64           `json:"lat"`
	Lon        float64           `json:"lon"`
	Alt        float64           `json:"alt"`
	To         string            `json:"to,omitempty"` // actor routing
}

// StreamEmitter queues continuous telemetry frames and sends them to the
// local edge agent in a background goroutine.
//
// Standalone usage:
//
//	emitter := stream.NewEmitter(stream.EmitterConfig{
//	    AgentAPI:  "http://127.0.0.1:7777",
//	    Project:   "my-fleet",
//	    QueueSize: 8192,
//	})
//	ctx, cancel := context.WithCancel(context.Background())
//	go emitter.Run(ctx)
//	emitter.Send(map[string]any{"nav/alt": 102.3}, stream.To("operator:*"))
type StreamEmitter struct {
	agentAPI  string
	project   string
	logCh     chan emitFrame
	logger    *slog.Logger
	httpClient *http.Client
}

// EmitterConfig holds StreamEmitter constructor options.
type EmitterConfig struct {
	AgentAPI  string
	Project   string
	QueueSize int
	Logger    *slog.Logger
}

// NewEmitter creates a new StreamEmitter. Call Run(ctx) in a goroutine to start sending.
func NewEmitter(cfg EmitterConfig) *StreamEmitter {
	if cfg.AgentAPI == "" {
		cfg.AgentAPI = "http://127.0.0.1:7777"
	}
	cfg.AgentAPI = strings.TrimRight(cfg.AgentAPI, "/")
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = 8192
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &StreamEmitter{
		agentAPI:   cfg.AgentAPI,
		project:    cfg.Project,
		logCh:      make(chan emitFrame, cfg.QueueSize),
		logger:     cfg.Logger,
		httpClient: &http.Client{Timeout: 5 * time.Second},
	}
}

// Run drains the send queue and posts frames to the edge agent. Blocks until ctx is cancelled.
func (e *StreamEmitter) Run(ctx context.Context) {
	u := e.agentAPI + "/v1/log"
	for {
		select {
		case <-ctx.Done():
			return
		case frame := <-e.logCh:
			body, err := json.Marshal(frame)
			if err != nil {
				e.logger.Warn("failed to marshal log frame", "err", err)
				continue
			}
			e.postWithRetry(ctx, u, body, 3)
		}
	}
}

// Send enqueues a data frame. Non-blocking: frames are dropped when the queue is full.
//
// Nested maps are flattened using slash-separated keys:
//
//	{"sensors": {"temp": 85}} → {"sensors/temp": 85}
func (e *StreamEmitter) Send(data map[string]any, opts ...EmitOption) {
	o := emitOpt{stream: "telemetry"}
	for _, fn := range opts {
		fn(&o)
	}
	frame := emitFrame{
		Data:       FlattenKeys(data, ""),
		Stream:     o.stream,
		StreamName: o.streamName,
		ProjectID:  e.project,
		Tags:       o.tags,
		Lat:        o.lat,
		Lon:        o.lon,
		Alt:        o.alt,
		To:         o.to,
	}
	select {
	case e.logCh <- frame:
	default:
		e.logger.Warn("emit queue full — dropping frame",
			"project", e.project)
	}
}

// LogCh returns the internal frame channel. Used by Client to share the channel.
func (e *StreamEmitter) LogCh() chan emitFrame { return e.logCh }

// FlattenKeys recursively flattens nested maps using slash-separated keys.
func FlattenKeys(data map[string]any, prefix string) map[string]any {
	result := make(map[string]any, len(data))
	for k, v := range data {
		key := k
		if prefix != "" {
			key = prefix + "/" + k
		}
		if nested, ok := v.(map[string]any); ok {
			for fk, fv := range FlattenKeys(nested, key) {
				result[fk] = fv
			}
		} else {
			result[key] = v
		}
	}
	return result
}

func (e *StreamEmitter) postWithRetry(ctx context.Context, u string, body []byte, max int) bool {
	delay := 50 * time.Millisecond
	for i := 0; i < max; i++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
		if err != nil {
			return false
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := e.httpClient.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return false
			}
			time.Sleep(delay)
			delay *= 2
			continue
		}
		io.Copy(io.Discard, resp.Body) //nolint:errcheck
		resp.Body.Close()
		return resp.StatusCode >= 200 && resp.StatusCode < 300
	}
	return false
}

// ServiceName returns the resolved service name (hostname fallback).
func ServiceName(configured string) string {
	if configured != "" {
		return configured
	}
	if d := os.Getenv("SYSTEMSCALE_SERVICE"); d != "" {
		return d
	}
	if h, err := os.Hostname(); err == nil {
		return h
	}
	return "unknown"
}

// EmitFrame is the exported alias for use by the parent package.
type EmitFrame = emitFrame
