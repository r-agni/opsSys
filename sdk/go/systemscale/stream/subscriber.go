package stream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DataFrame is a real-time telemetry frame received from the ws-gateway.
type DataFrame struct {
	Device    string
	Stream    string
	Timestamp string
	Fields    map[string]any
}

// StreamSubscriber subscribes to the ws-gateway WebSocket and returns frames
// through a channel. Requires a websocket library or uses the built-in minimal
// ws client from operator.go (accessed via callback in the parent package).
//
// This package provides the Subscribe HTTP helper; actual WebSocket management
// lives in the parent OperatorClient which calls into this package.

// SubscribeConfig holds configuration for a subscription.
type SubscribeConfig struct {
	WSURL      string
	Token      string
	VehicleIDs []string
	Streams    []string
	FromActor  string // "type:id" filter; empty = no filter
	Logger     *slog.Logger
}

// ReceiveFrames opens a WebSocket subscription and sends frames to the returned
// channel until ctx is cancelled. The channel is closed when the function returns.
//
// This wraps the generic ws-gateway JSON subscription protocol; the actual
// low-level WebSocket dial is delegated to the provided dialFn to keep this
// package dependency-free.
//
// dialFn(url, onMessage func([]byte)) → closeFunc, error
func ReceiveFrames(
	ctx context.Context,
	cfg SubscribeConfig,
	dialFn func(wsURL string, onMessage func([]byte)) (func(), error),
) (<-chan *DataFrame, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	subMsg, _ := json.Marshal(map[string]any{
		"vehicle_ids": cfg.VehicleIDs,
		"streams":     cfg.Streams,
		"format":      "json",
	})

	ch := make(chan *DataFrame, 4096)

	var filterType, filterID string
	if cfg.FromActor != "" {
		parts := strings.SplitN(cfg.FromActor, ":", 2)
		filterType = parts[0]
		if len(parts) > 1 {
			filterID = parts[1]
		}
	}

	go func() {
		defer close(ch)
		backoff := 500 * time.Millisecond
		for {
			wsURL := fmt.Sprintf("%s/ws?token=%s", strings.TrimRight(cfg.WSURL, "/"),
				url.QueryEscape(cfg.Token))

			closeConn, err := dialFn(wsURL, func(raw []byte) {
				var data map[string]any
				if err := json.Unmarshal(raw, &data); err != nil {
					return
				}
				// Apply from_actor filter
				if filterType != "" {
					if st, _ := data["sender_type"].(string); st != filterType {
						return
					}
				}
				if filterID != "" && filterID != "*" {
					if sid, _ := data["sender_id"].(string); sid != filterID {
						return
					}
				}
				ts := ""
				if t, ok := data["ts"]; ok {
					ts = fmt.Sprintf("%v", t)
				}
				device, _ := data["vehicle_id"].(string)
				if device == "" {
					device, _ = data["device"].(string)
				}
				streamType, _ := data["type"].(string)
				frame := &DataFrame{
					Device:    device,
					Stream:    streamType,
					Timestamp: ts,
					Fields:    data,
				}
				select {
				case ch <- frame:
				case <-ctx.Done():
				default:
				}
			})
			if err != nil {
				cfg.Logger.Warn("WS dial failed", "err", err, "backoff", backoff)
			} else {
				// Send subscription message — handled by parent via callback
				// (dialFn is expected to call onConnect internally)
				_ = subMsg
				cfg.Logger.Info("WS subscription active")
				backoff = 500 * time.Millisecond

				select {
				case <-ctx.Done():
					closeConn()
					return
				}
			}

			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
				backoff = time.Duration(float64(backoff) * 1.5)
				if backoff > 30*time.Second {
					backoff = 30 * time.Second
				}
			}
		}
	}()

	return ch, nil
}

// ResolveVehicleIDs looks up vehicle IDs for a project (and optionally device)
// using the fleet-api.
func ResolveVehicleIDs(fleetAPI, project, device, token string) ([]string, error) {
	u := fmt.Sprintf("%s/v1/projects/%s/devices",
		strings.TrimRight(fleetAPI, "/"),
		url.PathEscape(project),
	)
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("fleet-api HTTP %d: %s", resp.StatusCode, b)
	}

	var out struct {
		Devices []struct {
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
		} `json:"devices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}

	var ids []string
	for _, d := range out.Devices {
		if device == "" || d.DisplayName == device {
			ids = append(ids, d.ID)
		}
	}
	return ids, nil
}
