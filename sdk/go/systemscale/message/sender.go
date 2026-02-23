package message

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/systemscale/sdk/go/systemscale/core"
)

// SendOption is a functional option for MessageSender.Send().
type SendOption func(*sendOpt)

type sendOpt struct {
	to       string
	msgType  string
	data     map[string]any
	level    string
	priority string
	timeout  time.Duration
}

// WithTo routes the message to a specific actor ("type:id" format).
func WithTo(actor string) SendOption { return func(o *sendOpt) { o.to = actor } }

// WithType sets the message type ("alert", "event", "command").
func WithType(t string) SendOption { return func(o *sendOpt) { o.msgType = t } }

// WithData attaches extra payload to the message.
func WithData(d map[string]any) SendOption { return func(o *sendOpt) { o.data = d } }

// WithLevel sets the alert severity ("info", "warning", "error", "critical").
func WithLevel(l string) SendOption { return func(o *sendOpt) { o.level = l } }

// WithPriority sets the command priority ("normal", "high", "emergency").
func WithPriority(p string) SendOption { return func(o *sendOpt) { o.priority = p } }

// WithTimeout sets how long to wait for a command ACK.
func WithTimeout(d time.Duration) SendOption { return func(o *sendOpt) { o.timeout = d } }

// CommandResult holds the outcome of a command.
type CommandResult struct {
	CommandID string
	Status    string // "completed" | "accepted" | "rejected" | "failed" | "timeout"
	Message   string
}

// SenderConfig holds MessageSender constructor options.
type SenderConfig struct {
	// APIKey and APIKeyURL are required for operator-mode command sending.
	APIKey    string
	APIKeyURL string

	// AgentAPI is the local edge agent base URL (device-mode alert sending).
	AgentAPI string
	// CommandAPI is the cloud command REST API (operator-mode command sending).
	CommandAPI string
	// FleetAPI is the fleet-api base URL (for device name → vehicle_id lookup).
	FleetAPI string

	Project string
	Logger  *slog.Logger

	// TokenFn overrides token exchange. If nil, uses core.ExchangeToken.
	TokenFn func() (string, error)
}

// MessageSender sends discrete (event-driven) messages and commands.
//
// Device mode (AgentAPI set): sends alerts/events to the local edge agent.
// Operator mode (CommandAPI + APIKeyURL set): sends commands via the cloud REST API.
//
// Standalone usage:
//
//	sender := message.NewSender(message.SenderConfig{
//	    AgentAPI: "http://127.0.0.1:7777",
//	    Project:  "my-fleet",
//	})
//	sender.Send("obstacle ahead", message.WithTo("human:pilot-1"), message.WithType("alert"))
type MessageSender struct {
	cfg        SenderConfig
	httpClient *http.Client // shared — TCP connections are reused across all calls
}

// NewSender creates a MessageSender.
func NewSender(cfg SenderConfig) *MessageSender {
	if cfg.AgentAPI != "" {
		cfg.AgentAPI = strings.TrimRight(cfg.AgentAPI, "/")
	}
	if cfg.CommandAPI != "" {
		cfg.CommandAPI = strings.TrimRight(cfg.CommandAPI, "/")
	}
	if cfg.FleetAPI != "" {
		cfg.FleetAPI = strings.TrimRight(cfg.FleetAPI, "/")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &MessageSender{
		cfg:        cfg,
		httpClient: &http.Client{Timeout: 15 * time.Second},
	}
}

// Send sends a discrete message or command.
//
//   - Device mode (alert): content is the alert message text.
//   - Operator mode (command): content is the command type string (e.g. "goto").
//
// Use WithTo("device:drone-002") to target a specific device when sending commands.
func (s *MessageSender) Send(content string, opts ...SendOption) (*CommandResult, error) {
	o := sendOpt{
		msgType:  "alert",
		level:    "info",
		priority: "normal",
		timeout:  30 * time.Second,
	}
	for _, fn := range opts {
		fn(&o)
	}

	if s.cfg.CommandAPI != "" && o.msgType == "command" {
		return s.sendCommand(content, o)
	}
	return nil, s.sendEvent(content, o)
}

// sendEvent posts an alert/event to the local edge agent.
func (s *MessageSender) sendEvent(message string, o sendOpt) error {
	payload := map[string]any{
		"_event_type": 101, // EVENT_TYPE_ALERT
		"level":       o.level,
		"message":     message,
	}
	for k, v := range o.data {
		payload[k] = v
	}
	body := map[string]any{
		"data":       payload,
		"stream":     "event",
		"project_id": s.cfg.Project,
		"lat":        0.0,
		"lon":        0.0,
		"alt":        0.0,
	}
	if o.to != "" {
		body["to"] = o.to
	}
	b, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, s.cfg.AgentAPI+"/v1/log", bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("send event: %w", err)
	}
	resp.Body.Close()
	return nil
}

// sendCommand posts a command to the cloud command REST API and waits for ACK via SSE push.
func (s *MessageSender) sendCommand(cmdType string, o sendOpt) (*CommandResult, error) {
	token, err := s.token()
	if err != nil {
		return nil, fmt.Errorf("token: %w", err)
	}
	headers := map[string]string{"Authorization": "Bearer " + token}

	vehicleID := ""
	if strings.HasPrefix(o.to, "device:") {
		serviceName := strings.TrimPrefix(o.to, "device:")
		vehicleID, _ = s.resolveVehicleID(serviceName, token)
	}

	postBody, _ := json.Marshal(map[string]any{
		"vehicle_id":   vehicleID,
		"command_type": cmdType,
		"data":         o.data,
		"priority":     o.priority,
		"ttl_ms":       int(o.timeout.Milliseconds()),
	})
	resp, err := s.httpPost(s.cfg.CommandAPI+"/v1/commands", postBody, headers)
	if err != nil {
		return nil, fmt.Errorf("send command: %w", err)
	}
	var posted struct {
		CommandID string `json:"command_id"`
	}
	if err := json.Unmarshal(resp, &posted); err != nil {
		return nil, err
	}

	// Wait for ACK via SSE push — latency = actual command RTT, no polling overhead.
	return s.waitForACK(token, posted.CommandID, o.timeout)
}

// waitForACK opens a persistent SSE connection to /v1/commands/{id}/stream and
// blocks until command-api pushes the ACK event. No polling loop, no fixed sleep.
func (s *MessageSender) waitForACK(token, commandID string, timeout time.Duration) (*CommandResult, error) {
	ackURL := fmt.Sprintf("%s/v1/commands/%s/stream",
		s.cfg.CommandAPI, url.PathEscape(commandID))

	// Deadline is command TTL + 5 s so the server's own timeout fires first.
	client := &http.Client{Timeout: timeout + 5*time.Second}
	req, err := http.NewRequest(http.MethodGet, ackURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")

	sseResp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ACK stream: %w", err)
	}
	defer sseResp.Body.Close()

	scanner := bufio.NewScanner(sseResp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		var ack struct {
			Status  string `json:"status"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal([]byte(data), &ack); err != nil {
			continue
		}
		return &CommandResult{
			CommandID: commandID,
			Status:    ack.Status,
			Message:   ack.Message,
		}, nil
	}
	return &CommandResult{CommandID: commandID, Status: "timeout"}, nil
}

// token returns a cached JWT, re-exchanging via core.ExchangeToken when near expiry.
func (s *MessageSender) token() (string, error) {
	if s.cfg.TokenFn != nil {
		return s.cfg.TokenFn()
	}
	return core.ExchangeToken(s.cfg.APIKey, strings.TrimRight(s.cfg.APIKeyURL, "/"))
}

func (s *MessageSender) resolveVehicleID(deviceName, token string) (string, error) {
	if s.cfg.FleetAPI == "" {
		return deviceName, nil
	}
	u := fmt.Sprintf("%s/v1/projects/%s/devices",
		s.cfg.FleetAPI, url.PathEscape(s.cfg.Project))
	b, err := s.httpGet(u, map[string]string{"Authorization": "Bearer " + token})
	if err != nil {
		return deviceName, err
	}
	var out struct {
		Devices []struct {
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
		} `json:"devices"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return deviceName, err
	}
	for _, d := range out.Devices {
		if d.DisplayName == deviceName {
			return d.ID, nil
		}
	}
	return deviceName, nil
}

func (s *MessageSender) httpPost(u string, body []byte, headers map[string]string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, b)
	}
	return b, nil
}

func (s *MessageSender) httpGet(u string, headers map[string]string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, b)
	}
	return b, nil
}
