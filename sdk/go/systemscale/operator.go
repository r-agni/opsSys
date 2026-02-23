// operator.go — Cloud-side OperatorClient for SystemScale Go SDK.
//
// Usage:
//
//	ops, err := systemscale.Connect(systemscale.OperatorConfig{
//	    APIKey:  "ssk_live_...",
//	    Project: "my-fleet",
//	})
//
//	rows, _ := ops.Query(ctx, systemscale.QueryRequest{
//	    Device: "drone-001",
//	    Keys:   []string{"sensors/temp"},
//	    Start:  "-1h",
//	})
//
//	ch, _ := ops.Subscribe(ctx, "drone-001", nil)
//	for frame := range ch { fmt.Println(frame.Fields) }
//
//	ops.SendCommand(ctx, systemscale.CommandRequest{
//	    Device: "drone-001",
//	    Type:   "goto",
//	    Data:   map[string]any{"lat": 28.6, "lon": 77.2},
//	})
//
//	arCh, _ := ops.AssistanceRequests(ctx)
//	for ar := range arCh { ar.Approve("proceed") }
package systemscale

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// OperatorConfig
// ─────────────────────────────────────────────────────────────────────────────

// OperatorConfig holds configuration for the cloud-side operator client.
type OperatorConfig struct {
	// APIKey is the SystemScale API key (format: ssk_live_<base58>).
	APIKey string

	// Project is the project name to operate on.
	Project string

	// APIKeyURL is the apikey-service base URL. Default: "https://apikey.systemscale.io".
	APIKeyURL string

	// FleetAPI is the fleet-api base URL (devices, assistance). Default: "https://fleet.systemscale.io".
	FleetAPI string

	// QueryAPI is the query-api base URL. Default: "https://query.systemscale.io".
	QueryAPI string

	// CommandAPI is the command-api base URL. Default: "https://command.systemscale.io".
	CommandAPI string

	// GatewayWS is the ws-gateway WebSocket base URL. Default: "wss://gateway.systemscale.io".
	GatewayWS string

	// Logger for SDK-internal messages. Defaults to slog.Default().
	Logger *slog.Logger
}

func (c *OperatorConfig) applyDefaults() {
	if c.APIKeyURL == "" {
		c.APIKeyURL = "https://apikey.systemscale.io"
	}
	c.APIKeyURL = strings.TrimRight(c.APIKeyURL, "/")
	if c.FleetAPI == "" {
		c.FleetAPI = "https://fleet.systemscale.io"
	}
	c.FleetAPI = strings.TrimRight(c.FleetAPI, "/")
	if c.QueryAPI == "" {
		c.QueryAPI = "https://query.systemscale.io"
	}
	c.QueryAPI = strings.TrimRight(c.QueryAPI, "/")
	if c.CommandAPI == "" {
		c.CommandAPI = "https://command.systemscale.io"
	}
	c.CommandAPI = strings.TrimRight(c.CommandAPI, "/")
	if c.GatewayWS == "" {
		c.GatewayWS = "wss://gateway.systemscale.io"
	}
	c.GatewayWS = strings.TrimRight(c.GatewayWS, "/")
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Data types
// ─────────────────────────────────────────────────────────────────────────────

// QueryRequest specifies parameters for a historical telemetry query.
type QueryRequest struct {
	// Device filters by device name. Empty means all devices in the project.
	Device string
	// Keys is a list of slash-separated field keys to return (e.g. "sensors/temp").
	// Empty returns all fields.
	Keys []string
	// Start is an absolute (RFC3339) or relative ("-1h", "-30m") start time.
	Start string
	// End is an absolute (RFC3339) or relative end time. Defaults to "now".
	End string
	// SampleBy is a QuestDB SAMPLE BY interval (e.g. "1m", "5s"). Empty = no sampling.
	SampleBy string
	// Limit caps the number of rows returned. Defaults to 1000.
	Limit int
}

// DataRow is one row returned by Query().
type DataRow struct {
	Timestamp time.Time      `json:"ts"`
	Device    string         `json:"device"`
	Fields    map[string]any `json:"fields"`
}

// DataFrame is a live telemetry frame received from Subscribe().
type DataFrame struct {
	Timestamp time.Time
	Device    string
	Stream    string
	Fields    map[string]any
}

// CommandRequest specifies a command to dispatch to a device.
type CommandRequest struct {
	// Device is the target device name.
	Device string
	// Type is the command type string (e.g. "goto", "land").
	Type string
	// Data is the arbitrary JSON payload.
	Data map[string]any
	// Priority is "normal" (default), "high", or "emergency".
	Priority string
	// Timeout is how long to wait for an ACK from the device. Default: 30s.
	Timeout time.Duration
}

// CommandResult is the outcome of a SendCommand() call.
type CommandResult struct {
	CommandID string `json:"command_id"`
	Status    string `json:"status"`  // "completed", "rejected", "failed", "timeout"
	Message   string `json:"message"`
}

// AssistanceRequest is a pending help request sent by a device via
// client.RequestAssistance().
type AssistanceRequest struct {
	RequestID string
	DeviceID  string
	Reason    string
	Metadata  map[string]any
	CreatedAt time.Time

	client *OperatorClient
}

// Approve sends an approval response and optional operator instruction to the device.
func (a *AssistanceRequest) Approve(instruction string) error {
	return a.client.postAssistanceResponse(a.RequestID, true, instruction, "")
}

// Deny sends a denial response and reason to the device.
func (a *AssistanceRequest) Deny(reason string) error {
	return a.client.postAssistanceResponse(a.RequestID, false, "", reason)
}

// ─────────────────────────────────────────────────────────────────────────────
// OperatorClient
// ─────────────────────────────────────────────────────────────────────────────

// OperatorClient provides cloud-side operations: querying telemetry, subscribing
// to live streams, sending commands, and handling assistance requests.
type OperatorClient struct {
	cfg        OperatorConfig
	httpClient *http.Client

	tokenMu  sync.Mutex
	token    string
	tokenExp time.Time
}

// Connect creates and authenticates an OperatorClient.
// Returns an error if the API key is invalid or the apikey-service is unreachable.
func Connect(cfg OperatorConfig) (*OperatorClient, error) {
	cfg.applyDefaults()
	o := &OperatorClient{
		cfg:        cfg,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
	// Eager auth check.
	if _, err := o.getToken(); err != nil {
		return nil, fmt.Errorf("systemscale: operator auth failed: %w", err)
	}
	return o, nil
}

// getToken returns a cached JWT, refreshing it when it expires within 5 minutes.
func (o *OperatorClient) getToken() (string, error) {
	o.tokenMu.Lock()
	defer o.tokenMu.Unlock()
	if o.token != "" && time.Now().Add(5*time.Minute).Before(o.tokenExp) {
		return o.token, nil
	}
	body, _ := json.Marshal(map[string]string{"api_key": o.cfg.APIKey})
	req, err := http.NewRequest(http.MethodPost, o.cfg.APIKeyURL+"/v1/token", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := o.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("token exchange HTTP %d: %s", resp.StatusCode, b)
	}
	var out struct {
		Token string `json:"token"`
		Exp   int64  `json:"exp"` // Unix timestamp; 0 if not provided
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	o.token = out.Token
	if out.Exp > 0 {
		o.tokenExp = time.Unix(out.Exp, 0)
	} else {
		o.tokenExp = time.Now().Add(24 * time.Hour)
	}
	return o.token, nil
}

// Devices lists all registered devices in the project.
func (o *OperatorClient) Devices(ctx context.Context) ([]map[string]any, error) {
	token, err := o.getToken()
	if err != nil {
		return nil, err
	}
	u := fmt.Sprintf("%s/v1/projects/%s/devices",
		o.cfg.FleetAPI, url.PathEscape(o.cfg.Project))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := o.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("devices HTTP %d: %s", resp.StatusCode, b)
	}
	var out []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// Query retrieves historical telemetry data matching the request criteria.
func (o *OperatorClient) Query(ctx context.Context, req QueryRequest) ([]DataRow, error) {
	token, err := o.getToken()
	if err != nil {
		return nil, err
	}
	if req.Limit <= 0 {
		req.Limit = 1000
	}
	q := url.Values{}
	q.Set("project", o.cfg.Project)
	if req.Device != "" {
		q.Set("device", req.Device)
	}
	if len(req.Keys) > 0 {
		q.Set("keys", strings.Join(req.Keys, ","))
	}
	if req.Start != "" {
		q.Set("start", req.Start)
	}
	if req.End != "" {
		q.Set("end", req.End)
	}
	if req.SampleBy != "" {
		q.Set("sample_by", req.SampleBy)
	}
	q.Set("limit", fmt.Sprintf("%d", req.Limit))

	u := o.cfg.QueryAPI + "/v1/query?" + q.Encode()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)
	resp, err := o.httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("query HTTP %d: %s", resp.StatusCode, b)
	}
	var rows []DataRow
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		return nil, err
	}
	return rows, nil
}

// Subscribe opens a WebSocket connection to the gateway and yields DataFrames
// for the given device and stream types. Pass nil streams for all streams.
// The returned channel is closed when ctx is cancelled or the connection drops.
func (o *OperatorClient) Subscribe(ctx context.Context, device string, streams []string) (<-chan *DataFrame, error) {
	token, err := o.getToken()
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	q.Set("project", o.cfg.Project)
	if device != "" {
		q.Set("vehicle", device)
	}
	if len(streams) > 0 {
		q.Set("streams", strings.Join(streams, ","))
	}
	wsURL := o.cfg.GatewayWS + "/ws?" + q.Encode()

	ws, err := wsDial(ctx, wsURL, token)
	if err != nil {
		return nil, fmt.Errorf("subscribe dial: %w", err)
	}

	ch := make(chan *DataFrame, 256)
	go func() {
		defer close(ch)
		defer ws.Close()
		for {
			if ctx.Err() != nil {
				return
			}
			msg, err := ws.ReadMessage()
			if err != nil {
				if ctx.Err() == nil {
					o.cfg.Logger.Warn("subscribe stream disconnected", "err", err)
				}
				return
			}
			var frame struct {
				Device string         `json:"device"`
				Stream string         `json:"stream"`
				TsNS   int64          `json:"ts_ns"`
				Fields map[string]any `json:"fields"`
			}
			if err := json.Unmarshal(msg, &frame); err != nil {
				continue
			}
			select {
			case ch <- &DataFrame{
				Timestamp: time.Unix(0, frame.TsNS),
				Device:    frame.Device,
				Stream:    frame.Stream,
				Fields:    frame.Fields,
			}:
			default:
				o.cfg.Logger.Warn("subscribe channel full — dropping frame")
			}
		}
	}()
	return ch, nil
}

// SendCommand dispatches a command to a device and waits for an ACK.
func (o *OperatorClient) SendCommand(ctx context.Context, req CommandRequest) (*CommandResult, error) {
	token, err := o.getToken()
	if err != nil {
		return nil, err
	}
	if req.Priority == "" {
		req.Priority = "normal"
	}
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	body, _ := json.Marshal(map[string]any{
		"vehicle_id":   req.Device,
		"command_type": req.Type,
		"data":         req.Data,
		"priority":     req.Priority,
		"timeout_s":    int(timeout.Seconds()),
	})
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		o.cfg.CommandAPI+"/v1/commands", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+token)
	resp, err := o.httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 && resp.StatusCode != 202 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("send_command HTTP %d: %s", resp.StatusCode, b)
	}
	var result CommandResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return &result, nil
}

// AssistanceRequests opens a WebSocket to the gateway and yields incoming
// AssistanceRequests from devices. The returned channel is closed when ctx
// is cancelled or the connection drops.
func (o *OperatorClient) AssistanceRequests(ctx context.Context) (<-chan *AssistanceRequest, error) {
	token, err := o.getToken()
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	q.Set("project", o.cfg.Project)
	q.Set("streams", "assistance")
	wsURL := o.cfg.GatewayWS + "/ws?" + q.Encode()

	ws, err := wsDial(ctx, wsURL, token)
	if err != nil {
		return nil, fmt.Errorf("assistance_requests dial: %w", err)
	}

	ch := make(chan *AssistanceRequest, 64)
	go func() {
		defer close(ch)
		defer ws.Close()
		for {
			if ctx.Err() != nil {
				return
			}
			msg, err := ws.ReadMessage()
			if err != nil {
				if ctx.Err() == nil {
					o.cfg.Logger.Warn("assistance stream disconnected", "err", err)
				}
				return
			}
			var frame struct {
				Type      string         `json:"type"`
				RequestID string         `json:"request_id"`
				Device    string         `json:"device"`
				Reason    string         `json:"reason"`
				Metadata  map[string]any `json:"metadata"`
				CreatedAt string         `json:"created_at"`
			}
			if err := json.Unmarshal(msg, &frame); err != nil {
				continue
			}
			if frame.Type != "assistance_request" {
				continue
			}
			ar := &AssistanceRequest{
				RequestID: frame.RequestID,
				DeviceID:  frame.Device,
				Reason:    frame.Reason,
				Metadata:  frame.Metadata,
				client:    o,
			}
			if t, err := time.Parse(time.RFC3339, frame.CreatedAt); err == nil {
				ar.CreatedAt = t
			}
			select {
			case ch <- ar:
			default:
				o.cfg.Logger.Warn("assistance channel full — dropping request",
					"request_id", frame.RequestID)
			}
		}
	}()
	return ch, nil
}

func (o *OperatorClient) postAssistanceResponse(requestID string, approved bool, instruction, reason string) error {
	token, err := o.getToken()
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]any{
		"approved":    approved,
		"instruction": instruction,
		"reason":      reason,
	})
	u := fmt.Sprintf("%s/v1/assistance/%s/respond",
		o.cfg.FleetAPI, url.PathEscape(requestID))
	req, err := http.NewRequest(http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := o.httpClient.Do(req)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, resp.Body) //nolint:errcheck
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("assistance respond HTTP %d", resp.StatusCode)
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Minimal RFC 6455 WebSocket client (stdlib only — no external dependencies)
// ─────────────────────────────────────────────────────────────────────────────

type wsConn struct {
	conn net.Conn
	r    *bufio.Reader
}

// wsDial performs a WebSocket handshake (RFC 6455 §4) and returns an open
// wsConn. Supports ws:// and wss:// schemes. The token is sent as a Bearer
// Authorization header so the gateway can authenticate the operator.
func wsDial(ctx context.Context, rawURL, token string) (*wsConn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}

	host := u.Host
	if !strings.Contains(host, ":") {
		if u.Scheme == "wss" {
			host += ":443"
		} else {
			host += ":80"
		}
	}

	// Generate the Sec-WebSocket-Key (16 random bytes, base64-encoded).
	var keyBytes [16]byte
	rand.Read(keyBytes[:]) //nolint:errcheck
	wsKey := base64.StdEncoding.EncodeToString(keyBytes[:])

	var conn net.Conn
	dialer := &net.Dialer{}
	if u.Scheme == "wss" {
		tlsDialer := &tls.Dialer{
			NetDialer: dialer,
			Config:    &tls.Config{ServerName: u.Hostname()},
		}
		conn, err = tlsDialer.DialContext(ctx, "tcp", host)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", host)
	}
	if err != nil {
		return nil, err
	}

	// Write HTTP Upgrade request.
	reqPath := u.RequestURI()
	fmt.Fprintf(conn, "GET %s HTTP/1.1\r\n", reqPath)
	fmt.Fprintf(conn, "Host: %s\r\n", u.Host)
	fmt.Fprintf(conn, "Upgrade: websocket\r\n")
	fmt.Fprintf(conn, "Connection: Upgrade\r\n")
	fmt.Fprintf(conn, "Sec-WebSocket-Key: %s\r\n", wsKey)
	fmt.Fprintf(conn, "Sec-WebSocket-Version: 13\r\n")
	if token != "" {
		fmt.Fprintf(conn, "Authorization: Bearer %s\r\n", token)
	}
	fmt.Fprintf(conn, "\r\n")

	// Read HTTP 101 Switching Protocols response.
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("websocket handshake read: %w", err)
	}
	// Body is empty for 101; close without consuming bytes from br.
	resp.Body.Close()
	if resp.StatusCode != 101 {
		conn.Close()
		return nil, fmt.Errorf("websocket upgrade: HTTP %d (expected 101)", resp.StatusCode)
	}

	return &wsConn{conn: conn, r: br}, nil
}

// ReadMessage reads one complete WebSocket message, reassembling fragments.
// Returns io.EOF when a close frame is received.
func (ws *wsConn) ReadMessage() ([]byte, error) {
	return ws.readFrames(nil)
}

func (ws *wsConn) readFrames(acc []byte) ([]byte, error) {
	// Read 2-byte fixed header.
	header := make([]byte, 2)
	if _, err := io.ReadFull(ws.r, header); err != nil {
		return nil, err
	}

	fin    := header[0]&0x80 != 0
	opcode := header[0] & 0x0f
	masked := header[1]&0x80 != 0
	plen   := int64(header[1] & 0x7f)

	// Extended payload length.
	switch plen {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(ws.r, ext[:]); err != nil {
			return nil, err
		}
		plen = int64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(ws.r, ext[:]); err != nil {
			return nil, err
		}
		plen = int64(binary.BigEndian.Uint64(ext[:]))
	}

	// Optional masking key (server → client frames are never masked per RFC 6455).
	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(ws.r, mask[:]); err != nil {
			return nil, err
		}
	}

	// Payload.
	payload := make([]byte, plen)
	if _, err := io.ReadFull(ws.r, payload); err != nil {
		return nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}

	switch opcode {
	case 0x0: // continuation frame
		acc = append(acc, payload...)
	case 0x1, 0x2: // text or binary frame
		acc = append(acc, payload...)
	case 0x8: // close frame
		return nil, io.EOF
	case 0x9: // ping — reply with pong, then continue reading
		ws.sendControl(0xa, payload)
		return ws.readFrames(acc)
	case 0xa: // pong — ignore, continue reading
		return ws.readFrames(acc)
	}

	if !fin {
		return ws.readFrames(acc)
	}
	return acc, nil
}

// sendControl sends a control frame (ping, pong, or close) with an unmasked payload.
func (ws *wsConn) sendControl(opcode byte, payload []byte) {
	frame := make([]byte, 2+len(payload))
	frame[0] = 0x80 | opcode // FIN=1
	frame[1] = byte(len(payload))
	copy(frame[2:], payload)
	ws.conn.Write(frame) //nolint:errcheck
}

// Close sends a close frame and closes the underlying connection.
func (ws *wsConn) Close() error {
	ws.sendControl(0x8, nil)
	return ws.conn.Close()
}
