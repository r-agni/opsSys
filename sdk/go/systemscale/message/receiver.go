// Package message provides discrete (event-driven) message sending and receiving.
package message

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ACKFunc is a callback for sending command acknowledgements back to the agent.
type ACKFunc func(commandID, status, message string) error

// Command represents an inbound command from the cloud platform.
type Command struct {
	ID       string
	Type     string
	Data     map[string]any
	Priority string

	ackFn  ACKFunc
	mu     sync.Mutex
	acked  bool
	logger *slog.Logger
}

// Ack signals successful execution.
func (c *Command) Ack() error { return c.sendACK("completed", "") }

// AckMsg signals success with an informational message.
func (c *Command) AckMsg(msg string) error { return c.sendACK("completed", msg) }

// Reject signals the command was refused.
func (c *Command) Reject(reason string) error { return c.sendACK("rejected", reason) }

// Fail signals that execution started but failed.
func (c *Command) Fail(reason string) error { return c.sendACK("failed", reason) }

func (c *Command) sendACK(status, msg string) error {
	c.mu.Lock()
	if c.acked {
		c.mu.Unlock()
		return fmt.Errorf("command %s already acked", c.ID)
	}
	c.acked = true
	c.mu.Unlock()
	return c.ackFn(c.ID, status, msg)
}

// AssistanceResponse is received when an operator responds to a device's request.
type AssistanceResponse struct {
	RequestID   string
	Approved    bool
	Instruction string
}

// ReceiverConfig holds MessageReceiver constructor options.
type ReceiverConfig struct {
	AgentAPI string
	Logger   *slog.Logger
	ACKFn    ACKFunc // optional override; defaults to posting to /v1/commands/{id}/ack
}

// MessageReceiver listens for incoming commands and assistance responses via SSE.
//
// Standalone usage:
//
//	recv := message.NewReceiver(message.ReceiverConfig{AgentAPI: "http://127.0.0.1:7777"})
//	ctx, cancel := context.WithCancel(context.Background())
//	go recv.Run(ctx)
//	recv.OnCommand(func(cmd *message.Command) error {
//	    return cmd.Ack()
//	})
type MessageReceiver struct {
	agentAPI string
	logger   *slog.Logger
	ackFn    ACKFunc

	handlerMu sync.RWMutex
	handler   func(*Command) error

	cmdCh chan *Command

	assistMu      sync.Mutex
	assistPending map[string]chan *AssistanceResponse
}

// NewReceiver creates a MessageReceiver. Call Run(ctx) in a goroutine.
func NewReceiver(cfg ReceiverConfig) *MessageReceiver {
	if cfg.AgentAPI == "" {
		cfg.AgentAPI = "http://127.0.0.1:7777"
	}
	cfg.AgentAPI = strings.TrimRight(cfg.AgentAPI, "/")
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	r := &MessageReceiver{
		agentAPI:      cfg.AgentAPI,
		logger:        cfg.Logger,
		cmdCh:         make(chan *Command, 256),
		assistPending: make(map[string]chan *AssistanceResponse),
	}
	if cfg.ACKFn != nil {
		r.ackFn = cfg.ACKFn
	} else {
		r.ackFn = r.defaultACK
	}
	return r
}

// Run starts the SSE command listener. Blocks until ctx is cancelled.
func (r *MessageReceiver) Run(ctx context.Context) {
	u       := r.agentAPI + "/v1/commands"
	backoff := 250 * time.Millisecond
	max     := 10 * time.Second

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		if err := r.consumeSSE(ctx, u); err != nil && ctx.Err() == nil {
			r.logger.Warn("SSE disconnected — reconnecting",
				"err", err, "backoff", backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = time.Duration(math.Min(float64(backoff*2), float64(max)))
		} else {
			backoff = 250 * time.Millisecond
		}
	}
}

// OnCommand registers fn as the command handler. Each command runs fn in its
// own goroutine. Replaces any previously registered handler.
func (r *MessageReceiver) OnCommand(fn func(*Command) error) {
	r.handlerMu.Lock()
	r.handler = fn
	r.handlerMu.Unlock()
}

// NextCommand blocks for up to d and returns the next command, or (nil, false).
func (r *MessageReceiver) NextCommand(d time.Duration) (*Command, bool) {
	if d <= 0 {
		select {
		case cmd := <-r.cmdCh:
			return cmd, true
		default:
			return nil, false
		}
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case cmd := <-r.cmdCh:
		return cmd, true
	case <-timer.C:
		return nil, false
	}
}

// Listen returns the raw command channel for select-based usage.
func (r *MessageReceiver) Listen() <-chan *Command { return r.cmdCh }

// RegisterAssistanceWaiter registers a channel to receive the assistance
// response for the given requestID.
func (r *MessageReceiver) RegisterAssistanceWaiter(requestID string) chan *AssistanceResponse {
	ch := make(chan *AssistanceResponse, 1)
	r.assistMu.Lock()
	r.assistPending[requestID] = ch
	r.assistMu.Unlock()
	return ch
}

// UnregisterAssistanceWaiter removes the waiter for the given requestID.
func (r *MessageReceiver) UnregisterAssistanceWaiter(requestID string) {
	r.assistMu.Lock()
	delete(r.assistPending, requestID)
	r.assistMu.Unlock()
}

// ── SSE internals ─────────────────────────────────────────────────────────────

func (r *MessageReceiver) consumeSSE(ctx context.Context, rawURL string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")

	resp, err := (&http.Client{
		Transport: &http.Transport{
			ResponseHeaderTimeout: 10 * time.Second,
		},
	}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	r.logger.Info("SSE command stream connected")
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
			r.handleEvent(raw)
		}
	}
	return scanner.Err()
}

func (r *MessageReceiver) handleEvent(raw string) {
	var payload struct {
		ID          string         `json:"id"`
		CommandType string         `json:"command_type"`
		Type        string         `json:"type"`
		Data        map[string]any `json:"data"`
		Priority    string         `json:"priority"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		r.logger.Warn("malformed SSE event", "err", err)
		return
	}
	cmdType := payload.CommandType
	if cmdType == "" {
		cmdType = payload.Type
	}
	if cmdType == "" || payload.ID == "" {
		return
	}

	// Intercept assistance responses
	if cmdType == "_assistance_response" {
		r.resolveAssistance(payload.Data)
		return
	}

	cmd := &Command{
		ID:       payload.ID,
		Type:     cmdType,
		Data:     payload.Data,
		Priority: payload.Priority,
		ackFn:    r.ackFn,
		logger:   r.logger,
	}

	r.handlerMu.RLock()
	handler := r.handler
	r.handlerMu.RUnlock()

	if handler != nil {
		go r.callHandler(handler, cmd)
	} else {
		select {
		case r.cmdCh <- cmd:
		default:
			r.logger.Warn("command queue full — dropping", "id", cmd.ID)
			_ = cmd.Fail("command queue full on device")
		}
	}
}

func (r *MessageReceiver) resolveAssistance(data map[string]any) {
	rid, _ := data["_request_id"].(string)
	if rid == "" {
		rid, _ = data["request_id"].(string)
	}
	if rid == "" {
		return
	}
	approved, _    := data["approved"].(bool)
	instruction, _ := data["instruction"].(string)

	r.assistMu.Lock()
	ch, ok := r.assistPending[rid]
	r.assistMu.Unlock()
	if !ok {
		return
	}
	select {
	case ch <- &AssistanceResponse{
		RequestID:   rid,
		Approved:    approved,
		Instruction: instruction,
	}:
	default:
	}
}

func (r *MessageReceiver) callHandler(fn func(*Command) error, cmd *Command) {
	defer func() {
		if rec := recover(); rec != nil {
			r.logger.Error("command handler panicked", "id", cmd.ID, "panic", rec)
			_ = cmd.Fail(fmt.Sprintf("handler panic: %v", rec))
		}
	}()
	if err := fn(cmd); err != nil {
		r.logger.Error("command handler returned error", "id", cmd.ID, "err", err)
		cmd.mu.Lock()
		acked := cmd.acked
		cmd.mu.Unlock()
		if !acked {
			_ = cmd.Fail(err.Error())
		}
		return
	}
	cmd.mu.Lock()
	acked := cmd.acked
	cmd.mu.Unlock()
	if !acked {
		r.logger.Warn("handler returned without ACK — sending failed", "id", cmd.ID)
		_ = cmd.Fail("handler returned without ACK")
	}
}

func (r *MessageReceiver) defaultACK(commandID, status, msg string) error {
	ackURL := fmt.Sprintf("%s/v1/commands/%s/ack", r.agentAPI, commandID)
	body, _ := json.Marshal(map[string]string{"status": status, "message": msg})
	req, err := http.NewRequest(http.MethodPost, ackURL, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("ACK HTTP %d", resp.StatusCode)
	}
	return nil
}
