// Package core provides shared utilities for the SystemScale Go SDK.
package core

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// tokenCache caches JWT tokens keyed by API key.
type tokenEntry struct {
	token     string
	expiresAt time.Time
}

var (
	tokenMu    sync.Mutex
	tokenCache = map[string]tokenEntry{}
)

// ExchangeToken exchanges an API key for a short-lived JWT from the
// apikey-service. Cached tokens are re-used until 45 minutes before expiry.
func ExchangeToken(apiKey, apikeyURL string) (string, error) {
	tokenMu.Lock()
	if e, ok := tokenCache[apiKey]; ok && time.Now().Before(e.expiresAt) {
		tokenMu.Unlock()
		return e.token, nil
	}
	tokenMu.Unlock()

	body, _ := json.Marshal(map[string]string{"api_key": apiKey})
	req, err := http.NewRequest(http.MethodPost, apikeyURL+"/v1/token", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return "", fmt.Errorf("token exchange: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("token exchange HTTP %d: %s", resp.StatusCode, b)
	}
	var out struct {
		Token     string `json:"token"`
		ExpiresIn int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	ttl := out.ExpiresIn
	if ttl <= 0 {
		ttl = 3600
	}

	tokenMu.Lock()
	tokenCache[apiKey] = tokenEntry{
		token:     out.Token,
		expiresAt: time.Now().Add(time.Duration(ttl)*time.Second - 45*time.Minute),
	}
	tokenMu.Unlock()
	return out.Token, nil
}
