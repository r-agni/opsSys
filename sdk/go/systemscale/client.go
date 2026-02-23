// Package systemscale is the SystemScale Go SDK for on-device telemetry.
//
// Usage:
//
//	client, err := systemscale.Init(systemscale.Config{
//	    APIKey:  "ssk_live_...",
//	    Project: "my-fleet",
//	    Device:  "drone-001",
//	})
//
//	// Log — non-blocking, nested maps flattened to slash-separated keys
//	client.Log(map[string]any{"sensors": map[string]any{"temp": 85.2}})
//	client.Log(map[string]any{"nav/altitude": 102.3},
//	    systemscale.WithLocation(28.61, 77.20, 102.3))
//
//	// Alert
//	client.Alert("Low battery", systemscale.WithAlertLevel("warning"),
//	    systemscale.WithAlertData(map[string]any{"pct": 12}))
//
//	// Blocking human-in-the-loop
//	resp, err := client.RequestAssistance("Obstacle detected", nil, 60*time.Second)
//	if resp.Approved { proceed() }
//
//	// Commands — callback style
//	client.OnCommand(func(cmd *systemscale.Command) error {
//	    switch cmd.Type {
//	    case "goto":
//	        navigate(cmd.Data["lat"].(float64), cmd.Data["lon"].(float64))
//	        return cmd.Ack()
//	    }
//	    return cmd.Reject("unknown type")
//	})
package systemscale

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// Config
// ─────────────────────────────────────────────────────────────────────────────

// Config holds initialisation options for the device SDK client.
type Config struct {
	// APIKey is the SystemScale API key (format: ssk_live_<base58>).
	APIKey string

	// Project is the project/fleet name (e.g. "my-fleet").
	Project string

	// Device is the device name within the project (e.g. "drone-001").
	// Defaults to SYSTEMSCALE_DEVICE env var, then os.Hostname().
	Device string

	// Mode controls how the client connects: "auto" (default), "device", or "cloud".
	// "auto" probes the local agent and auto-provisions if unreachable.
	// "device" requires a running local agent (Init returns error if absent).
	Mode string

	// APIBase is the edge agent local API URL. Defaults to "http://127.0.0.1:7777".
	APIBase string

	// APIKeyURL is the apikey-service base URL used for token exchange during
	// auto-provisioning. Defaults to "https://apikey.systemscale.io".
	APIKeyURL string

	// FleetAPI is the fleet-api base URL used for device provisioning.
	// Defaults to "https://fleet.systemscale.io".
	FleetAPI string

	// QueueSize is the capacity of the internal log frame queue.
	// Frames are dropped (logged at Warn) when the queue is full. Default: 8192.
	QueueSize int

	// Logger is used for SDK-internal messages. Defaults to slog.Default().
	Logger *slog.Logger
}

func (c *Config) applyDefaults() {
	if c.APIBase == "" {
		c.APIBase = "http://127.0.0.1:7777"
	}
	c.APIBase = strings.TrimRight(c.APIBase, "/")
	if c.Device == "" {
		c.Device = os.Getenv("SYSTEMSCALE_DEVICE")
	}
	if c.Device == "" {
		if h, err := os.Hostname(); err == nil {
			c.Device = h
		}
	}
	if c.APIKeyURL == "" {
		c.APIKeyURL = "https://apikey.systemscale.io"
	}
	c.APIKeyURL = strings.TrimRight(c.APIKeyURL, "/")
	if c.FleetAPI == "" {
		c.FleetAPI = "https://fleet.systemscale.io"
	}
	c.FleetAPI = strings.TrimRight(c.FleetAPI, "/")
	if c.Mode == "" {
		c.Mode = "auto"
	}
	if c.QueueSize <= 0 {
		c.QueueSize = 8192
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Log options
// ─────────────────────────────────────────────────────────────────────────────

type logOpt struct {
	stream       string
	streamName   string
	lat          float64
	lon          float64
	alt          float64
	tags         map[string]string
	to           string // "type:id" actor routing, e.g. "operator:*"
	senderType   string // override default sender_type (defaults to "device")
}

// LogOption is a functional option for Log() calls.
type LogOption func(*logOpt)

// WithStream sets the stream type. One of "telemetry", "sensor", "event", "log".
func WithStream(s string) LogOption { return func(o *logOpt) { o.stream = s } }

// WithStreamName sets a sub-label within the stream type (e.g. "lidar_scan").
func WithStreamName(n string) LogOption { return func(o *logOpt) { o.streamName = n } }

// WithLocation attaches WGS84 coordinates to the log frame.
func WithLocation(lat, lon, altM float64) LogOption {
	return func(o *logOpt) { o.lat = lat; o.lon = lon; o.alt = altM }
}

// WithTags attaches low-cardinality string labels used for partitioning and
// filtering in QuestDB (stored as indexed SYMBOL columns).
func WithTags(tags map[string]string) LogOption {
	return func(o *logOpt) { o.tags = tags }
}

// WithReceiver routes the log frame to a specific actor.
//
// Format: WithReceiver("ground-control", "operator") sets receiver_id and receiver_type
// on the DataEnvelope, so ws-gateway delivers it only to the matching actor session.
// Use "*" as id to target all actors of that type.
//
//	client.Log(data, systemscale.WithReceiver("ground-control", "operator"))
//	client.Log(data, systemscale.WithReceiver("*", "operator"))  // all operators
//	client.Log(data, systemscale.WithReceiver("drone-002", "device")) // peer device
func WithReceiver(id, receiverType string) LogOption {
	return func(o *logOpt) { o.to = receiverType + ":" + id }
}

// WithSenderType overrides the sender_type field (defaults to "device").
func WithSenderType(t string) LogOption { return func(o *logOpt) { o.senderType = t } }

// ─────────────────────────────────────────────────────────────────────────────
// Alert options
// ─────────────────────────────────────────────────────────────────────────────

type alertOpt struct {
	level string
	data  map[string]any
}

// AlertOption is a functional option for Alert() calls.
type AlertOption func(*alertOpt)

// WithAlertLevel sets the alert severity. One of "info", "warning", "error", "critical".
// Defaults to "info".
func WithAlertLevel(l string) AlertOption { return func(o *alertOpt) { o.level = l } }

// WithAlertData attaches arbitrary metadata to the alert payload.
func WithAlertData(d map[string]any) AlertOption { return func(o *alertOpt) { o.data = d } }

// ─────────────────────────────────────────────────────────────────────────────
// AssistanceResponse
// ─────────────────────────────────────────────────────────────────────────────

// AssistanceResponse is returned by RequestAssistance when an operator responds.
type AssistanceResponse struct {
	RequestID   string
	Approved    bool
	Instruction string
}

// ─────────────────────────────────────────────────────────────────────────────
// Command
// ─────────────────────────────────────────────────────────────────────────────

// Command represents an inbound command from the cloud platform.
type Command struct {
	// ID is the UUID of this command, used for ACK routing.
	ID string
	// Type is the command type string set by the sender (e.g. "goto").
	Type string
	// Data is the arbitrary JSON payload from the sender.
	Data map[string]any
	// Priority is "normal", "high", or "emergency".
	Priority string

	client *Client
	mu     sync.Mutex
	acked  bool
}

// Ack signals successful execution to the platform.
func (c *Command) Ack() error { return c.sendACK("completed", "") }

// AckMsg signals success with an informational message.
func (c *Command) AckMsg(msg string) error { return c.sendACK("completed", msg) }

// Reject signals the command was refused (invalid state, unsupported type, etc.).
func (c *Command) Reject(reason string) error { return c.sendACK("rejected", reason) }

// Fail signals that execution started but failed.
func (c *Command) Fail(reason string) error { return c.sendACK("failed", reason) }

func (c *Command) sendACK(status, message string) error {
	c.mu.Lock()
	if c.acked {
		c.mu.Unlock()
		return fmt.Errorf("command %s already acked", c.ID)
	}
	c.acked = true
	c.mu.Unlock()
	return c.client.postACK(c.ID, status, message)
}

// ─────────────────────────────────────────────────────────────────────────────
// StreamWriter
// ─────────────────────────────────────────────────────────────────────────────

// StreamWriter is a log writer bound to a named sub-stream. Obtain one from
// Client.Stream(). All frames it emits carry the given stream_name.
type StreamWriter struct {
	client     *Client
	streamName string
}

// Log logs a frame on this named stream.
func (s *StreamWriter) Log(data map[string]any, opts ...LogOption) {
	opts = append([]LogOption{WithStreamName(s.streamName)}, opts...)
	s.client.Log(data, opts...)
}

// ─────────────────────────────────────────────────────────────────────────────
// Internal log frame (matches edge agent /v1/log request body)
// ─────────────────────────────────────────────────────────────────────────────

type logFrame struct {
	Data       map[string]any    `json:"data"`
	Stream     string            `json:"stream"`
	StreamName string            `json:"stream_name,omitempty"`
	ProjectID  string            `json:"project_id,omitempty"`
	Tags       map[string]string `json:"tags,omitempty"`
	Lat        float64           `json:"lat"`
	Lon        float64           `json:"lon"`
	Alt        float64           `json:"alt"`
	To         string            `json:"to,omitempty"`         // "type:id" actor routing
	SenderType string            `json:"sender_type,omitempty"` // defaults to "device"
}

// ─────────────────────────────────────────────────────────────────────────────
// Client
// ─────────────────────────────────────────────────────────────────────────────

// Client is the on-device SDK client. Create one with Init().
type Client struct {
	cfg        Config
	httpClient *http.Client
	logCh      chan logFrame
	cmdCh      chan *Command

	handlerMu sync.RWMutex
	handler   func(*Command) error

	assistMu      sync.Mutex
	assistPending map[string]chan *AssistanceResponse

	ctx    context.Context
	cancel context.CancelFunc
}

// Init creates and starts a Client. Call it once at program startup.
// It probes the local edge agent and auto-provisions the device if needed.
// Returns an error only if the agent cannot be reached after provisioning.
func Init(cfg Config) (*Client, error) {
	cfg.applyDefaults()
	ctx, cancel := context.WithCancel(context.Background())
	c := &Client{
		cfg:           cfg,
		logCh:         make(chan logFrame, cfg.QueueSize),
		cmdCh:         make(chan *Command, 256),
		assistPending: make(map[string]chan *AssistanceResponse),
		ctx:           ctx,
		cancel:        cancel,
		httpClient:    &http.Client{Timeout: 5 * time.Second},
	}

	if err := c.ensureAgent(); err != nil {
		cancel()
		return nil, fmt.Errorf("systemscale: %w", err)
	}

	go c.runSender()
	go c.runCommandListener()
	cfg.Logger.Info("SystemScale SDK started",
		"project", cfg.Project,
		"device", cfg.Device,
		"api", cfg.APIBase,
	)
	return c, nil
}

// Stop gracefully shuts down background goroutines.
func (c *Client) Stop() {
	c.cancel()
}

// ─────────────────────────────────────────────────────────────────────────────
// Agent probe & provisioning
// ─────────────────────────────────────────────────────────────────────────────

func (c *Client) ensureAgent() error {
	if c.probeAgent() {
		return nil
	}
	if c.cfg.Mode == "device" {
		return fmt.Errorf("edge agent not reachable at %s (mode=device)", c.cfg.APIBase)
	}

	c.cfg.Logger.Info("edge agent not detected — provisioning device",
		"project", c.cfg.Project, "device", c.cfg.Device)

	if err := c.provision(); err != nil {
		return fmt.Errorf("auto-provisioning failed: %w", err)
	}

	// Poll up to 30s for the agent to become healthy.
	deadline := time.Now().Add(30 * time.Second)
	delay := 250 * time.Millisecond
	for time.Now().Before(deadline) {
		if c.probeAgent() {
			return nil
		}
		time.Sleep(delay)
		delay = time.Duration(math.Min(float64(delay*3/2), float64(2*time.Second)))
	}
	return fmt.Errorf("edge agent did not become healthy within 30s after provisioning")
}

func (c *Client) probeAgent() bool {
	resp, err := c.httpClient.Get(c.cfg.APIBase + "/healthz")
	if err != nil {
		return false
	}
	io.Copy(io.Discard, resp.Body) //nolint:errcheck
	resp.Body.Close()
	return resp.StatusCode == 200
}

// provision exchanges the API key for a JWT, registers the device via fleet-api,
// writes the agent config and TLS certs, then spawns the agent binary.
func (c *Client) provision() error {
	jwt, err := c.exchangeToken()
	if err != nil {
		return fmt.Errorf("token exchange: %w", err)
	}

	bundle, err := c.provisionDevice(jwt)
	if err != nil {
		return fmt.Errorf("fleet-api provision: %w", err)
	}

	if err := writeProvisionBundle(bundle); err != nil {
		return fmt.Errorf("write config: %w", err)
	}

	return c.spawnAgent()
}

func (c *Client) exchangeToken() (string, error) {
	body, _ := json.Marshal(map[string]string{"api_key": c.cfg.APIKey})
	req, err := http.NewRequest(http.MethodPost, c.cfg.APIKeyURL+"/v1/token", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, b)
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.Token, nil
}

func (c *Client) provisionDevice(jwt string) (map[string]any, error) {
	body, _ := json.Marshal(map[string]string{"display_name": c.cfg.Device})
	u := fmt.Sprintf("%s/v1/projects/%s/devices",
		c.cfg.FleetAPI, url.PathEscape(c.cfg.Project))
	req, err := http.NewRequest(http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+jwt)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, b)
	}
	var bundle map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&bundle); err != nil {
		return nil, err
	}
	return bundle, nil
}

func writeProvisionBundle(bundle map[string]any) error {
	cfgDir := os.Getenv("SYSTEMSCALE_CONFIG_DIR")
	if cfgDir == "" {
		cfgDir = "/etc/systemscale"
	}
	certDir := os.Getenv("SYSTEMSCALE_CERT_DIR")
	if certDir == "" {
		certDir = cfgDir + "/certs"
	}
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(certDir, 0o700); err != nil {
		return err
	}
	if yaml, ok := bundle["agent_config_yaml"].(string); ok {
		if err := os.WriteFile(cfgDir+"/agent.yaml", []byte(yaml), 0o644); err != nil {
			return err
		}
	}
	for filename, key := range map[string]string{
		"device.crt": "cert_pem",
		"device.key": "key_pem",
		"ca.crt":     "ca_pem",
	} {
		if pem, ok := bundle[key].(string); ok && pem != "" {
			path := certDir + "/" + filename
			if err := os.WriteFile(path, []byte(pem), 0o600); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *Client) spawnAgent() error {
	agentBin := os.Getenv("SYSTEMSCALE_AGENT_BIN")
	if agentBin == "" {
		agentBin = "systemscale-agent"
	}
	cfgDir := os.Getenv("SYSTEMSCALE_CONFIG_DIR")
	if cfgDir == "" {
		cfgDir = "/etc/systemscale"
	}
	cmd := exec.Command(agentBin)
	cmd.Env = append(os.Environ(), "SYSTEMSCALE_AGENT_CONFIG="+cfgDir+"/agent.yaml")
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start agent binary %q: %w — "+
			"install with: pip install systemscale-agent OR apt install systemscale-agent", agentBin, err)
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Logging
// ─────────────────────────────────────────────────────────────────────────────

// Log enqueues a telemetry frame for the edge agent. Non-blocking.
//
// Nested maps are flattened using slash-separated keys:
//
//	{"sensors": {"temp": 85.2}} → {"sensors/temp": 85.2}
func (c *Client) Log(data map[string]any, opts ...LogOption) {
	o := logOpt{stream: "telemetry"}
	for _, fn := range opts {
		fn(&o)
	}
	frame := logFrame{
		Data:       flattenKeys(data, ""),
		Stream:     o.stream,
		StreamName: o.streamName,
		ProjectID:  c.cfg.Project,
		Tags:       o.tags,
		Lat:        o.lat,
		Lon:        o.lon,
		Alt:        o.alt,
		To:         o.to,
		SenderType: o.senderType,
	}
	select {
	case c.logCh <- frame:
	default:
		c.cfg.Logger.Warn("log queue full — dropping frame")
	}
}

// Alert logs an alert event (EventType=101). Non-blocking.
func (c *Client) Alert(message string, opts ...AlertOption) {
	o := alertOpt{level: "info"}
	for _, fn := range opts {
		fn(&o)
	}
	payload := map[string]any{
		"_event_type": 101,
		"level":       o.level,
		"message":     message,
	}
	for k, v := range o.data {
		payload[k] = v
	}
	c.Log(payload, WithStream("event"))
}

// RequestAssistance sends a help request and blocks until an operator responds
// or the timeout elapses. A zero timeout defaults to 5 minutes.
func (c *Client) RequestAssistance(reason string, data map[string]any, timeout time.Duration) (*AssistanceResponse, error) {
	requestID := newUUID()
	payload := map[string]any{
		"_event_type":  102,
		"_request_id": requestID,
		"reason":      reason,
	}
	for k, v := range data {
		payload[k] = v
	}

	ch := make(chan *AssistanceResponse, 1)
	c.assistMu.Lock()
	c.assistPending[requestID] = ch
	c.assistMu.Unlock()
	defer func() {
		c.assistMu.Lock()
		delete(c.assistPending, requestID)
		c.assistMu.Unlock()
	}()

	c.Log(payload, WithStream("event"))

	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case resp := <-ch:
		return resp, nil
	case <-timer.C:
		return nil, fmt.Errorf("request_assistance %s timed out after %v", requestID, timeout)
	case <-c.ctx.Done():
		return nil, fmt.Errorf("client stopped")
	}
}

// Stream returns a StreamWriter bound to the given sub-stream name.
// All frames written through it carry that stream_name tag.
func (c *Client) Stream(name string) *StreamWriter {
	return &StreamWriter{client: c, streamName: name}
}

// flattenKeys recursively flattens nested maps using slash-separated keys.
//
//	{"sensors": {"temp": 85}} → {"sensors/temp": 85}
func flattenKeys(data map[string]any, prefix string) map[string]any {
	result := make(map[string]any, len(data))
	for k, v := range data {
		fullKey := k
		if prefix != "" {
			fullKey = prefix + "/" + k
		}
		if nested, ok := v.(map[string]any); ok {
			for fk, fv := range flattenKeys(nested, fullKey) {
				result[fk] = fv
			}
		} else {
			result[fullKey] = v
		}
	}
	return result
}

// newUUID returns a random UUID v4 string using crypto/rand.
func newUUID() string {
	var b [16]byte
	rand.Read(b[:]) //nolint:errcheck
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// ─────────────────────────────────────────────────────────────────────────────
// Commands
// ─────────────────────────────────────────────────────────────────────────────

// OnCommand registers fn as the command handler. Each inbound command runs fn
// in its own goroutine. If fn returns an error or panics, a "failed" ACK is
// sent automatically. Calling OnCommand again replaces the previous handler.
func (c *Client) OnCommand(fn func(*Command) error) {
	c.handlerMu.Lock()
	c.handler = fn
	c.handlerMu.Unlock()
}

// NextCommand returns the next pending command, or (nil, false) if none arrived
// within d. Use this for manual poll style instead of OnCommand.
func (c *Client) NextCommand(d time.Duration) (*Command, bool) {
	if d <= 0 {
		select {
		case cmd := <-c.cmdCh:
			return cmd, true
		default:
			return nil, false
		}
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case cmd := <-c.cmdCh:
		return cmd, true
	case <-timer.C:
		return nil, false
	case <-c.ctx.Done():
		return nil, false
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Background: log sender
// ─────────────────────────────────────────────────────────────────────────────

func (c *Client) runSender() {
	u := c.cfg.APIBase + "/v1/log"
	for {
		select {
		case <-c.ctx.Done():
			return
		case frame := <-c.logCh:
			body, err := json.Marshal(frame)
			if err != nil {
				c.cfg.Logger.Warn("failed to marshal log frame", "err", err)
				continue
			}
			c.postWithRetry(u, body, 3)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Background: SSE command listener
// ─────────────────────────────────────────────────────────────────────────────

func (c *Client) runCommandListener() {
	u       := c.cfg.APIBase + "/v1/commands"
	backoff := 250 * time.Millisecond
	maxBack := 10 * time.Second

	for {
		select {
		case <-c.ctx.Done():
			return
		default:
		}

		err := c.consumeSSE(u)
		if err != nil && c.ctx.Err() == nil {
			c.cfg.Logger.Warn("SSE stream disconnected — reconnecting",
				"err", err, "backoff", backoff)
			select {
			case <-c.ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = time.Duration(math.Min(float64(backoff*2), float64(maxBack)))
		} else {
			backoff = 250 * time.Millisecond
		}
	}
}

func (c *Client) consumeSSE(rawURL string) error {
	req, err := http.NewRequestWithContext(c.ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")

	// SSE connections are long-lived; use a client without timeout.
	sseClient := &http.Client{}
	resp, err := sseClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	c.cfg.Logger.Info("SSE command stream connected")

	scanner := bufio.NewScanner(resp.Body)
	var dataLines []string

	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "data:"):
			dataLines = append(dataLines, strings.TrimPrefix(line, "data:"))
		case line == "" && len(dataLines) > 0:
			raw := strings.Join(dataLines, "\n")
			dataLines = dataLines[:0]
			c.handleSSEEvent(raw)
		}
		// Ignore "id:", "event:", ":keep-alive" lines.
	}
	return scanner.Err()
}

func (c *Client) handleSSEEvent(raw string) {
	var payload struct {
		ID          string         `json:"id"`
		CommandType string         `json:"command_type"`
		Data        map[string]any `json:"data"`
		Priority    string         `json:"priority"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		c.cfg.Logger.Warn("malformed SSE event", "err", err)
		return
	}
	if payload.ID == "" {
		return
	}

	// Intercept assistance responses — not forwarded to the user handler.
	if payload.CommandType == "_assistance_response" {
		c.resolveAssistance(payload.Data)
		return
	}

	cmd := &Command{
		ID:       payload.ID,
		Type:     payload.CommandType,
		Data:     payload.Data,
		Priority: payload.Priority,
		client:   c,
	}

	c.handlerMu.RLock()
	handler := c.handler
	c.handlerMu.RUnlock()

	if handler != nil {
		go c.callHandler(handler, cmd)
	} else {
		select {
		case c.cmdCh <- cmd:
		default:
			c.cfg.Logger.Warn("command queue full — dropping command", "id", cmd.ID)
			_ = cmd.Fail("command queue full on device")
		}
	}
}

func (c *Client) resolveAssistance(data map[string]any) {
	requestID, _ := data["request_id"].(string)
	if requestID == "" {
		return
	}
	approved, _    := data["approved"].(bool)
	instruction, _ := data["instruction"].(string)

	c.assistMu.Lock()
	ch, ok := c.assistPending[requestID]
	c.assistMu.Unlock()
	if !ok {
		return
	}
	select {
	case ch <- &AssistanceResponse{
		RequestID:   requestID,
		Approved:    approved,
		Instruction: instruction,
	}:
	default:
	}
}

func (c *Client) callHandler(fn func(*Command) error, cmd *Command) {
	defer func() {
		if r := recover(); r != nil {
			c.cfg.Logger.Error("command handler panicked", "id", cmd.ID, "panic", r)
			_ = cmd.Fail(fmt.Sprintf("handler panic: %v", r))
		}
	}()
	if err := fn(cmd); err != nil {
		c.cfg.Logger.Error("command handler returned error", "id", cmd.ID, "err", err)
		cmd.mu.Lock()
		wasAcked := cmd.acked
		cmd.mu.Unlock()
		if !wasAcked {
			_ = cmd.Fail(err.Error())
		}
		return
	}
	cmd.mu.Lock()
	wasAcked := cmd.acked
	cmd.mu.Unlock()
	if !wasAcked {
		c.cfg.Logger.Warn("handler returned without ACK — sending failed", "id", cmd.ID)
		_ = cmd.Fail("handler returned without ACK")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// HTTP helpers
// ─────────────────────────────────────────────────────────────────────────────

func (c *Client) postACK(commandID, status, message string) error {
	u := fmt.Sprintf("%s/v1/commands/%s/ack",
		c.cfg.APIBase, url.PathEscape(commandID))
	body, _ := json.Marshal(map[string]string{"status": status, "message": message})
	if !c.postWithRetry(u, body, 3) {
		return fmt.Errorf("failed to deliver ACK for command %s", commandID)
	}
	return nil
}

// postWithRetry POSTs body to u and returns true on HTTP 2xx.
// Uses exponential backoff (50ms base) across maxRetries attempts.
func (c *Client) postWithRetry(u string, body []byte, maxRetries int) bool {
	delay := 50 * time.Millisecond
	for i := 0; i < maxRetries; i++ {
		req, err := http.NewRequestWithContext(c.ctx, http.MethodPost, u, bytes.NewReader(body))
		if err != nil {
			return false
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.httpClient.Do(req)
		if err != nil {
			if c.ctx.Err() != nil {
				return false
			}
			time.Sleep(delay)
			delay *= 2
			continue
		}
		io.Copy(io.Discard, resp.Body) //nolint:errcheck
		resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return true
		}
		c.cfg.Logger.Warn("POST returned non-2xx", "url", u, "status", resp.StatusCode)
		return false
	}
	return false
}
