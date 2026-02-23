// Package provision handles automatic device registration and agent startup.
package provision

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Config holds parameters for device provisioning.
type Config struct {
	APIKey    string
	APIKeyURL string
	FleetAPI  string
	Project   string
	Device    string
	AgentAPI  string
}

// Provision exchanges an API key for a device certificate + agent config,
// writes config files to disk, and starts the edge agent process.
//
// Steps:
//  1. Exchange API key → JWT via apikey-service
//  2. Call fleet-api POST /v1/projects/{project}/devices → provisioning bundle
//  3. Write agent.yaml + TLS certs to /etc/systemscale/
//  4. Spawn the edge agent process (background)
//  5. Poll agent /healthz until ready (30 s timeout)
func Provision(cfg Config) error {
	cfg.APIKeyURL = strings.TrimRight(cfg.APIKeyURL, "/")
	cfg.FleetAPI  = strings.TrimRight(cfg.FleetAPI, "/")
	cfg.AgentAPI  = strings.TrimRight(cfg.AgentAPI, "/")

	jwt, err := exchangeToken(cfg.APIKey, cfg.APIKeyURL)
	if err != nil {
		return fmt.Errorf("token exchange: %w", err)
	}

	bundle, err := provisionDevice(jwt, cfg.Project, cfg.Device, cfg.FleetAPI)
	if err != nil {
		return fmt.Errorf("fleet-api provision: %w", err)
	}

	if err := writeBundle(bundle); err != nil {
		return fmt.Errorf("write config: %w", err)
	}

	if err := spawnAgent(); err != nil {
		return fmt.Errorf("start agent: %w", err)
	}

	return waitForAgent(cfg.AgentAPI, 30*time.Second)
}

func exchangeToken(apiKey, apikeyURL string) (string, error) {
	body, _ := json.Marshal(map[string]string{"api_key": apiKey})
	req, err := http.NewRequest(http.MethodPost, apikeyURL+"/v1/token", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
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
	json.NewDecoder(resp.Body).Decode(&out) //nolint:errcheck
	return out.Token, nil
}

func provisionDevice(jwt, project, device, fleetAPI string) (map[string]any, error) {
	body, _ := json.Marshal(map[string]string{"display_name": device})
	u := fmt.Sprintf("%s/v1/projects/%s/devices", fleetAPI, url.PathEscape(project))
	req, err := http.NewRequest(http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+jwt)
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, b)
	}
	var bundle map[string]any
	json.NewDecoder(resp.Body).Decode(&bundle) //nolint:errcheck
	return bundle, nil
}

func writeBundle(bundle map[string]any) error {
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
			if err := os.WriteFile(certDir+"/"+filename, []byte(pem), 0o600); err != nil {
				return err
			}
		}
	}
	return nil
}

func spawnAgent() error {
	bin := os.Getenv("SYSTEMSCALE_AGENT_BIN")
	if bin == "" {
		bin = "systemscale-agent"
	}
	cfgDir := os.Getenv("SYSTEMSCALE_CONFIG_DIR")
	if cfgDir == "" {
		cfgDir = "/etc/systemscale"
	}
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(), "SYSTEMSCALE_AGENT_CONFIG="+cfgDir+"/agent.yaml")
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("binary %q not found — "+
			"install with: pip install systemscale-agent OR apt install systemscale-agent",
			bin)
	}
	return nil
}

func waitForAgent(agentAPI string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	delay    := 250 * time.Millisecond
	for time.Now().Before(deadline) {
		resp, err := (&http.Client{Timeout: 1 * time.Second}).Get(agentAPI + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return nil
			}
		}
		time.Sleep(delay)
		delay = time.Duration(math.Min(float64(delay*3/2), float64(2*time.Second)))
	}
	return fmt.Errorf("edge agent did not become healthy within %v", timeout)
}
