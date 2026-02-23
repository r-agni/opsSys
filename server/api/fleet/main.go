// fleet-api: REST service for fleet management operations.
//
// All endpoints require a valid JWT Bearer token (validated by shared/auth).
// Multi-tenant: all queries are scoped to the operator's org_id from JWT claims.
//
// API surface:
//   GET  /v1/vehicles                                list vehicles in org
//   GET  /v1/vehicles/{id}                           get vehicle details
//   POST /v1/vehicles                                register new vehicle (legacy)
//   POST /v1/vehicles/{id}/missions                  upload mission plan
//   GET  /v1/projects                                list projects in org
//   POST /v1/projects/{name}/devices                 provision device (upsert project + device)
//   GET  /v1/projects/{name}/devices                 list devices in project
//   GET  /v1/assistance                              list pending assistance requests for org
//   POST /v1/assistance/{request_id}/respond         operator responds to assistance request
//
// Storage: CockroachDB (PostgreSQL-compatible wire protocol).
package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"text/template"
	"time"

	"github.com/google/uuid"
	_ "github.com/lib/pq"
	"github.com/systemscale/services/shared/auth"
)

// ──────────────────────────────────────────────────────────────────────────────
// Domain types
// ──────────────────────────────────────────────────────────────────────────────

type Vehicle struct {
	ID          string    `json:"id"`
	OrgID       string    `json:"org_id"`
	ProjectID   string    `json:"project_id,omitempty"`
	DisplayName string    `json:"display_name"`
	VehicleType string    `json:"vehicle_type"`
	AdapterType string    `json:"adapter_type"`
	Region      string    `json:"region"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	Active      bool      `json:"active"`
}

type Project struct {
	ID          string    `json:"id"`
	OrgID       string    `json:"org_id"`
	Name        string    `json:"name"`
	DisplayName string    `json:"display_name"`
	CreatedAt   time.Time `json:"created_at"`
	Active      bool      `json:"active"`
}

type Mission struct {
	ID        string          `json:"id"`
	VehicleID string          `json:"vehicle_id"`
	OrgID     string          `json:"org_id"`
	Name      string          `json:"name"`
	Plan      json.RawMessage `json:"plan"`
	CreatedAt time.Time       `json:"created_at"`
	Status    string          `json:"status"`
}

type AssistanceRequest struct {
	RequestID   string          `json:"request_id"`
	DeviceID    string          `json:"device_id"`
	OrgID       string          `json:"org_id"`
	ProjectID   string          `json:"project_id,omitempty"`
	Reason      string          `json:"reason"`
	Metadata    json.RawMessage `json:"metadata,omitempty"`
	Status      string          `json:"status"`
	CreatedAt   time.Time       `json:"created_at"`
	ExpiresAt   *time.Time      `json:"expires_at,omitempty"`
	RespondedAt *time.Time      `json:"responded_at,omitempty"`
	Instruction string          `json:"instruction,omitempty"`
}

// ProvisionResponse is returned when a device is provisioned via the SDK.
type ProvisionResponse struct {
	DeviceID        string `json:"device_id"`
	AgentConfigYAML string `json:"agent_config_yaml"`
	CertPEM         string `json:"cert_pem,omitempty"`
	KeyPEM          string `json:"key_pem,omitempty"`
	CAPEM           string `json:"ca_pem,omitempty"`
}

// ──────────────────────────────────────────────────────────────────────────────
// HTTP handlers
// ──────────────────────────────────────────────────────────────────────────────

type fleetAPI struct {
	db        *sql.DB
	relayAddr string // relay address for generated agent configs
	region    string
	kc        *keycloakClient
}

// ──────────────────────────────────────────────────────────────────────────────
// Keycloak admin API client (service-account credentials)
// ──────────────────────────────────────────────────────────────────────────────

type keycloakClient struct {
	baseURL      string // e.g. http://keycloak:8080
	realm        string
	clientID     string
	clientSecret string

	mu    sync.Mutex
	token string
	exp   time.Time
}

func (k *keycloakClient) adminToken() (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.token != "" && time.Now().Before(k.exp) {
		return k.token, nil
	}
	data := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {k.clientID},
		"client_secret": {k.clientSecret},
	}
	resp, err := http.PostForm(k.baseURL+"/realms/"+k.realm+"/protocol/openid-connect/token", data)
	if err != nil {
		return "", fmt.Errorf("keycloak token request: %w", err)
	}
	defer resp.Body.Close()
	var tok struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
		return "", fmt.Errorf("keycloak token decode: %w", err)
	}
	k.token = tok.AccessToken
	k.exp = time.Now().Add(time.Duration(tok.ExpiresIn-30) * time.Second)
	return k.token, nil
}

func (k *keycloakClient) do(method, path string, body interface{}) (*http.Response, error) {
	token, err := k.adminToken()
	if err != nil {
		return nil, err
	}
	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		bodyReader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, k.baseURL+path, bodyReader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return http.DefaultClient.Do(req)
}

type kcUser struct {
	ID         string              `json:"id,omitempty"`
	Username   string              `json:"username"`
	Email      string              `json:"email"`
	FirstName  string              `json:"firstName,omitempty"`
	LastName   string              `json:"lastName,omitempty"`
	Enabled    bool                `json:"enabled"`
	Attributes map[string][]string `json:"attributes,omitempty"`
}

type OrgMember struct {
	ID        string   `json:"id"`
	Email     string   `json:"email"`
	Name      string   `json:"name"`
	Role      string   `json:"role"`
	DeviceIDs []string `json:"device_ids"`
}

// listVehicles GET /v1/vehicles
func (f *fleetAPI) listVehicles(w http.ResponseWriter, r *http.Request) {
	claims, ok := auth.ClaimsFromContext(r.Context())
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	rows, err := f.db.QueryContext(r.Context(),
		`SELECT id, org_id, COALESCE(project_id,''), display_name, vehicle_type, adapter_type, region, created_at, updated_at, active
		 FROM vehicles WHERE org_id = $1 AND active = true ORDER BY display_name`,
		claims.OrgID,
	)
	if err != nil {
		slog.Error("list vehicles DB query", "err", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	vehicles := make([]Vehicle, 0, 64)
	for rows.Next() {
		var v Vehicle
		if err := rows.Scan(&v.ID, &v.OrgID, &v.ProjectID, &v.DisplayName, &v.VehicleType,
			&v.AdapterType, &v.Region, &v.CreatedAt, &v.UpdatedAt, &v.Active); err != nil {
			slog.Warn("scan vehicle row", "err", err)
			continue
		}
		vehicles = append(vehicles, v)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"vehicles": vehicles, "count": len(vehicles)})
}

// getVehicle GET /v1/vehicles/{id}
func (f *fleetAPI) getVehicle(w http.ResponseWriter, r *http.Request) {
	claims, ok := auth.ClaimsFromContext(r.Context())
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	vehicleID := pathSegmentFromEnd(r.URL.Path, 0)
	if vehicleID == "" {
		http.Error(w, "Bad Request: missing vehicle ID", http.StatusBadRequest)
		return
	}
	if !claims.CanAccessVehicle(vehicleID) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	var v Vehicle
	err := f.db.QueryRowContext(r.Context(),
		`SELECT id, org_id, COALESCE(project_id,''), display_name, vehicle_type, adapter_type, region, created_at, updated_at, active
		 FROM vehicles WHERE id = $1 AND org_id = $2`,
		vehicleID, claims.OrgID,
	).Scan(&v.ID, &v.OrgID, &v.ProjectID, &v.DisplayName, &v.VehicleType, &v.AdapterType,
		&v.Region, &v.CreatedAt, &v.UpdatedAt, &v.Active)
	if err == sql.ErrNoRows {
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// registerVehicle POST /v1/vehicles (legacy endpoint)
func (f *fleetAPI) registerVehicle(w http.ResponseWriter, r *http.Request) {
	claims, ok := auth.ClaimsFromContext(r.Context())
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if claims.Role != auth.RoleAdmin {
		http.Error(w, "Forbidden: admin role required", http.StatusForbidden)
		return
	}
	var req struct {
		DisplayName string `json:"display_name"`
		VehicleType string `json:"vehicle_type"`
		AdapterType string `json:"adapter_type"`
		Region      string `json:"region"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.DisplayName == "" {
		http.Error(w, "Bad Request: display_name required", http.StatusBadRequest)
		return
	}

	vehicleID := uuid.New().String()
	now := time.Now().UTC()
	_, err := f.db.ExecContext(r.Context(),
		`INSERT INTO vehicles (id, org_id, display_name, vehicle_type, adapter_type, region, created_at, updated_at, active)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, true)`,
		vehicleID, claims.OrgID, req.DisplayName, req.VehicleType, req.AdapterType, req.Region, now, now,
	)
	if err != nil {
		slog.Error("register vehicle DB insert", "err", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"vehicle_id": vehicleID,
		"message":    "Vehicle registered.",
	})
}

// createMission POST /v1/vehicles/{id}/missions
func (f *fleetAPI) createMission(w http.ResponseWriter, r *http.Request) {
	claims, ok := auth.ClaimsFromContext(r.Context())
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	vehicleID := pathSegmentFromEnd(r.URL.Path, 1) // /v1/vehicles/{id}/missions
	if !claims.CanAccessVehicle(vehicleID) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	var req struct {
		Name string          `json:"name"`
		Plan json.RawMessage `json:"plan"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	missionID := uuid.New().String()
	now := time.Now().UTC()
	planJSON, err := req.Plan.MarshalJSON()
	if err != nil {
		http.Error(w, "Bad Request: invalid plan JSON", http.StatusBadRequest)
		return
	}
	_, err = f.db.ExecContext(r.Context(),
		`INSERT INTO missions (id, vehicle_id, org_id, name, plan, created_at, status)
		 VALUES ($1, $2, $3, $4, $5, $6, 'draft')`,
		missionID, vehicleID, claims.OrgID, req.Name, string(planJSON), now,
	)
	if err != nil {
		slog.Error("create mission DB insert", "err", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"mission_id": missionID, "status": "draft"})
}

// ──────────────────────────────────────────────────────────────────────────────
// Projects
// ──────────────────────────────────────────────────────────────────────────────

// listProjects GET /v1/projects
func (f *fleetAPI) listProjects(w http.ResponseWriter, r *http.Request) {
	claims, ok := auth.ClaimsFromContext(r.Context())
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	rows, err := f.db.QueryContext(r.Context(),
		`SELECT id, org_id, name, display_name, created_at, active
		 FROM projects WHERE org_id = $1 AND active = true ORDER BY name`,
		claims.OrgID,
	)
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	var projects []Project
	for rows.Next() {
		var p Project
		if err := rows.Scan(&p.ID, &p.OrgID, &p.Name, &p.DisplayName, &p.CreatedAt, &p.Active); err != nil {
			slog.Warn("scan project row", "err", err)
			continue
		}
		projects = append(projects, p)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"projects": projects, "count": len(projects)})
}

// createProject POST /v1/projects — create a new project (admin only)
func (f *fleetAPI) createProject(w http.ResponseWriter, r *http.Request) {
	claims, ok := auth.ClaimsFromContext(r.Context())
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if claims.Role != auth.RoleAdmin {
		http.Error(w, "Forbidden: admin role required", http.StatusForbidden)
		return
	}
	var req struct {
		Name        string `json:"name"`
		DisplayName string `json:"display_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
		http.Error(w, "Bad Request: name required", http.StatusBadRequest)
		return
	}
	if req.DisplayName == "" {
		req.DisplayName = req.Name
	}

	projectID := uuid.New().String()
	now := time.Now().UTC()
	_, err := f.db.ExecContext(r.Context(),
		`INSERT INTO projects (id, org_id, name, display_name, created_at, active)
		 VALUES ($1, $2, $3, $4, $5, true)`,
		projectID, claims.OrgID, req.Name, req.DisplayName, now,
	)
	if err != nil {
		if strings.Contains(err.Error(), "duplicate key") || strings.Contains(err.Error(), "unique") {
			http.Error(w, "Conflict: project name already exists", http.StatusConflict)
			return
		}
		slog.Error("create project DB insert", "err", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, Project{
		ID: projectID, OrgID: claims.OrgID, Name: req.Name,
		DisplayName: req.DisplayName, CreatedAt: now, Active: true,
	})
}

// ──────────────────────────────────────────────────────────────────────────────
// Organization member management (proxied to Keycloak admin API)
// ──────────────────────────────────────────────────────────────────────────────

// listOrgMembers GET /v1/org/members
func (f *fleetAPI) listOrgMembers(w http.ResponseWriter, r *http.Request) {
	claims, ok := auth.ClaimsFromContext(r.Context())
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if claims.Role != auth.RoleAdmin {
		http.Error(w, "Forbidden: admin role required", http.StatusForbidden)
		return
	}

	path := fmt.Sprintf("/admin/realms/%s/users?max=200", f.kc.realm)
	resp, err := f.kc.do("GET", path, nil)
	if err != nil {
		slog.Error("keycloak list users", "err", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	var users []kcUser
	if err := json.NewDecoder(resp.Body).Decode(&users); err != nil {
		slog.Error("keycloak decode users", "err", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	var members []OrgMember
	for _, u := range users {
		orgID := firstAttr(u.Attributes, "org_id")
		if orgID != claims.OrgID {
			continue
		}
		members = append(members, OrgMember{
			ID:        u.ID,
			Email:     u.Email,
			Name:      strings.TrimSpace(u.FirstName + " " + u.LastName),
			Role:      firstAttr(u.Attributes, "role"),
			DeviceIDs: u.Attributes["vehicle_set"],
		})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"members": members, "count": len(members)})
}

// inviteMember POST /v1/org/invite
func (f *fleetAPI) inviteMember(w http.ResponseWriter, r *http.Request) {
	claims, ok := auth.ClaimsFromContext(r.Context())
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if claims.Role != auth.RoleAdmin {
		http.Error(w, "Forbidden: admin role required", http.StatusForbidden)
		return
	}

	var req struct {
		Email     string   `json:"email"`
		Role      string   `json:"role"`
		DeviceIDs []string `json:"device_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Email == "" {
		http.Error(w, "Bad Request: email required", http.StatusBadRequest)
		return
	}
	if req.Role == "" {
		req.Role = "viewer"
	}

	attrs := map[string][]string{
		"org_id": {claims.OrgID},
		"role":   {req.Role},
	}
	if len(req.DeviceIDs) > 0 {
		attrs["vehicle_set"] = req.DeviceIDs
	}

	newUser := kcUser{
		Username:   req.Email,
		Email:      req.Email,
		Enabled:    true,
		Attributes: attrs,
	}

	path := fmt.Sprintf("/admin/realms/%s/users", f.kc.realm)
	resp, err := f.kc.do("POST", path, newUser)
	if err != nil {
		slog.Error("keycloak create user", "err", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode == 409 {
		http.Error(w, "Conflict: user with this email already exists", http.StatusConflict)
		return
	}
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		slog.Error("keycloak create user failed", "status", resp.StatusCode, "body", string(body))
		http.Error(w, "Failed to create user in identity provider", http.StatusInternalServerError)
		return
	}

	// Extract new user ID from Location header
	loc := resp.Header.Get("Location")
	userID := loc[strings.LastIndex(loc, "/")+1:]

	// Trigger "set password" action email so the invited user can set credentials
	actionsPath := fmt.Sprintf("/admin/realms/%s/users/%s/execute-actions-email", f.kc.realm, userID)
	actResp, err := f.kc.do("PUT", actionsPath, []string{"UPDATE_PASSWORD"})
	if err != nil {
		slog.Warn("keycloak execute-actions-email", "err", err)
	} else {
		actResp.Body.Close()
	}

	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"id":    userID,
		"email": req.Email,
		"role":  req.Role,
	})
}

// updateMemberAccess PUT /v1/org/members/{id}/access
func (f *fleetAPI) updateMemberAccess(w http.ResponseWriter, r *http.Request) {
	claims, ok := auth.ClaimsFromContext(r.Context())
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if claims.Role != auth.RoleAdmin {
		http.Error(w, "Forbidden: admin role required", http.StatusForbidden)
		return
	}

	// Path: /v1/org/members/{id}/access → id is segment from end index 1
	memberID := pathSegmentFromEnd(r.URL.Path, 1)
	if memberID == "" {
		http.Error(w, "Bad Request: missing member ID", http.StatusBadRequest)
		return
	}

	var req struct {
		Role      string   `json:"role"`
		DeviceIDs []string `json:"device_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}

	// Fetch existing user from Keycloak
	path := fmt.Sprintf("/admin/realms/%s/users/%s", f.kc.realm, memberID)
	resp, err := f.kc.do("GET", path, nil)
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}

	var user kcUser
	if err := json.NewDecoder(resp.Body).Decode(&user); err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	// Verify same org
	if firstAttr(user.Attributes, "org_id") != claims.OrgID {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	if user.Attributes == nil {
		user.Attributes = make(map[string][]string)
	}
	if req.Role != "" {
		user.Attributes["role"] = []string{req.Role}
	}
	if req.DeviceIDs != nil {
		user.Attributes["vehicle_set"] = req.DeviceIDs
	}

	putResp, err := f.kc.do("PUT", path, user)
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	putResp.Body.Close()

	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

// removeMember DELETE /v1/org/members/{id}
func (f *fleetAPI) removeMember(w http.ResponseWriter, r *http.Request) {
	claims, ok := auth.ClaimsFromContext(r.Context())
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if claims.Role != auth.RoleAdmin {
		http.Error(w, "Forbidden: admin role required", http.StatusForbidden)
		return
	}

	memberID := pathSegmentFromEnd(r.URL.Path, 0)
	if memberID == "" {
		http.Error(w, "Bad Request: missing member ID", http.StatusBadRequest)
		return
	}

	// Verify same org before deleting
	path := fmt.Sprintf("/admin/realms/%s/users/%s", f.kc.realm, memberID)
	resp, err := f.kc.do("GET", path, nil)
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}
	var user kcUser
	json.NewDecoder(resp.Body).Decode(&user)
	if firstAttr(user.Attributes, "org_id") != claims.OrgID {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	delResp, err := f.kc.do("DELETE", path, nil)
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	delResp.Body.Close()
	writeJSON(w, http.StatusOK, map[string]string{"status": "removed"})
}

func firstAttr(attrs map[string][]string, key string) string {
	if vals, ok := attrs[key]; ok && len(vals) > 0 {
		return vals[0]
	}
	return ""
}

// agentConfigTemplate is the YAML template for edge agent config.
var agentConfigTemplate = template.Must(template.New("agent").Parse(`vehicle_id: "{{.VehicleID}}"
log_level: "info"
adapter: custom_unix
custom_unix:
  socket_path: /var/run/systemscale/ingest.sock
  vehicle_id: "{{.VehicleID}}"

quic:
  relay_addr: "{{.RelayAddr}}"
  relay_hostname: "relay.systemscale.io"
  cert_path: /etc/systemscale/certs/vehicle.pem
  key_path:  /etc/systemscale/certs/vehicle.key
  ca_cert_path: /etc/systemscale/certs/relay-ca.pem

ring_buffer:
  path: /var/lib/systemscale/ringbuf.bin
  capacity_bytes: 536870912

metrics:
  bind_addr: 0.0.0.0:9090
`))

// provisionDevice POST /v1/projects/{name}/devices
// Upserts the project, creates or returns the device, and returns a provisioning bundle.
func (f *fleetAPI) provisionDevice(w http.ResponseWriter, r *http.Request) {
	claims, ok := auth.ClaimsFromContext(r.Context())
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	projectName := pathSegmentFromEnd(r.URL.Path, 1) // /v1/projects/{name}/devices
	if projectName == "" {
		http.Error(w, "Bad Request: missing project name", http.StatusBadRequest)
		return
	}

	var req struct {
		DisplayName string `json:"display_name"` // device name, e.g. "drone-001"
		VehicleType string `json:"vehicle_type"`  // default: "unknown"
		AdapterType string `json:"adapter_type"`  // default: "custom_unix"
		Region      string `json:"region"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.DisplayName == "" {
		http.Error(w, "Bad Request: display_name required", http.StatusBadRequest)
		return
	}
	if req.VehicleType == "" { req.VehicleType = "unknown" }
	if req.AdapterType == "" { req.AdapterType = "custom_unix" }
	if req.Region == "" { req.Region = f.region }

	now := time.Now().UTC()

	// Upsert project by name within org
	var projectID string
	err := f.db.QueryRowContext(r.Context(),
		`INSERT INTO projects (id, org_id, name, display_name, created_at, active)
		 VALUES ($1, $2, $3, $4, $5, true)
		 ON CONFLICT (org_id, name) DO UPDATE SET active = true
		 RETURNING id`,
		uuid.New().String(), claims.OrgID, projectName, projectName, now,
	).Scan(&projectID)
	if err != nil {
		slog.Error("upsert project", "err", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	// Find existing device or create new one
	var deviceID string
	err = f.db.QueryRowContext(r.Context(),
		`SELECT id FROM vehicles WHERE org_id = $1 AND project_id = $2 AND display_name = $3 AND active = true`,
		claims.OrgID, projectID, req.DisplayName,
	).Scan(&deviceID)
	if err == sql.ErrNoRows {
		deviceID = uuid.New().String()
		_, err = f.db.ExecContext(r.Context(),
			`INSERT INTO vehicles (id, org_id, project_id, display_name, vehicle_type, adapter_type, region, created_at, updated_at, active)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, true)`,
			deviceID, claims.OrgID, projectID, req.DisplayName, req.VehicleType, req.AdapterType, req.Region, now, now,
		)
		if err != nil {
			slog.Error("insert device", "err", err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
	} else if err != nil {
		slog.Error("find device", "err", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	// Generate agent config YAML
	var configBuf strings.Builder
	if err := agentConfigTemplate.Execute(&configBuf, map[string]string{
		"VehicleID": deviceID,
		"RelayAddr": f.relayAddr,
	}); err != nil {
		slog.Error("agent config template", "err", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	// TODO: Issue X.509 cert via Step-CA API and include in response.
	// For now, return the config so the SDK can write it and use existing manual certs.
	writeJSON(w, http.StatusOK, ProvisionResponse{
		DeviceID:        deviceID,
		AgentConfigYAML: configBuf.String(),
	})
}

// listProjectDevices GET /v1/projects/{name}/devices
func (f *fleetAPI) listProjectDevices(w http.ResponseWriter, r *http.Request) {
	claims, ok := auth.ClaimsFromContext(r.Context())
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	projectName := pathSegmentFromEnd(r.URL.Path, 1) // /v1/projects/{name}/devices
	var projectID string
	if err := f.db.QueryRowContext(r.Context(),
		`SELECT id FROM projects WHERE org_id = $1 AND name = $2`,
		claims.OrgID, projectName,
	).Scan(&projectID); err == sql.ErrNoRows {
		writeJSON(w, http.StatusOK, map[string]interface{}{"devices": []Vehicle{}, "count": 0})
		return
	} else if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	rows, err := f.db.QueryContext(r.Context(),
		`SELECT id, org_id, COALESCE(project_id,''), display_name, vehicle_type, adapter_type, region, created_at, updated_at, active
		 FROM vehicles WHERE org_id = $1 AND project_id = $2 AND active = true ORDER BY display_name`,
		claims.OrgID, projectID,
	)
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	var devices []Vehicle
	for rows.Next() {
		var v Vehicle
		if err := rows.Scan(&v.ID, &v.OrgID, &v.ProjectID, &v.DisplayName, &v.VehicleType,
			&v.AdapterType, &v.Region, &v.CreatedAt, &v.UpdatedAt, &v.Active); err != nil {
			slog.Warn("scan device row", "err", err)
			continue
		}
		devices = append(devices, v)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"devices": devices, "count": len(devices)})
}

// ──────────────────────────────────────────────────────────────────────────────
// Assistance requests
// ──────────────────────────────────────────────────────────────────────────────

// listAssistance GET /v1/assistance — list pending requests for org
func (f *fleetAPI) listAssistance(w http.ResponseWriter, r *http.Request) {
	claims, ok := auth.ClaimsFromContext(r.Context())
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	rows, err := f.db.QueryContext(r.Context(),
		`SELECT request_id, device_id, org_id, COALESCE(project_id,''), reason,
		        COALESCE(metadata::text, 'null'), status, created_at, expires_at, responded_at, COALESCE(instruction,'')
		 FROM assistance_requests
		 WHERE org_id = $1 AND status = 'pending'
		   AND (expires_at IS NULL OR expires_at > now())
		 ORDER BY created_at`,
		claims.OrgID,
	)
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var reqs []AssistanceRequest
	for rows.Next() {
		var ar AssistanceRequest
		var metaStr string
		var expiresAt, respondedAt sql.NullTime
		if err := rows.Scan(&ar.RequestID, &ar.DeviceID, &ar.OrgID, &ar.ProjectID,
			&ar.Reason, &metaStr, &ar.Status, &ar.CreatedAt,
			&expiresAt, &respondedAt, &ar.Instruction); err != nil {
			slog.Warn("scan assistance row", "err", err)
			continue
		}
		ar.Metadata = json.RawMessage(metaStr)
		if expiresAt.Valid { ar.ExpiresAt = &expiresAt.Time }
		if respondedAt.Valid { ar.RespondedAt = &respondedAt.Time }
		reqs = append(reqs, ar)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"requests": reqs, "count": len(reqs)})
}

// respondToAssistance POST /v1/assistance/{request_id}/respond
func (f *fleetAPI) respondToAssistance(w http.ResponseWriter, r *http.Request) {
	claims, ok := auth.ClaimsFromContext(r.Context())
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if !claims.Role.CanSendCommand() {
		http.Error(w, "Forbidden: operator role required", http.StatusForbidden)
		return
	}

	requestID := pathSegmentFromEnd(r.URL.Path, 1) // /v1/assistance/{request_id}/respond
	if requestID == "" {
		http.Error(w, "Bad Request: missing request_id", http.StatusBadRequest)
		return
	}

	var req struct {
		Approved    bool   `json:"approved"`
		Instruction string `json:"instruction"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Bad Request: invalid JSON", http.StatusBadRequest)
		return
	}

	status := "denied"
	if req.Approved {
		status = "approved"
	}

	now := time.Now().UTC()
	result, err := f.db.ExecContext(r.Context(),
		`UPDATE assistance_requests
		 SET status = $1, instruction = $2, responded_at = $3
		 WHERE request_id = $4 AND org_id = $5 AND status = 'pending'`,
		status, req.Instruction, now, requestID, claims.OrgID,
	)
	if err != nil {
		slog.Error("respond to assistance", "err", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		http.Error(w, "Not Found or already responded", http.StatusNotFound)
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"request_id":  requestID,
		"status":      status,
		"instruction": req.Instruction,
		"responded_at": now,
	})
}

// upsertAssistanceRequest POST /v1/assistance — called by edge agent SDK internally
// (device-side) to register an assistance request that came through the event stream.
func (f *fleetAPI) upsertAssistanceRequest(w http.ResponseWriter, r *http.Request) {
	claims, ok := auth.ClaimsFromContext(r.Context())
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	var req struct {
		RequestID string          `json:"request_id"`
		DeviceID  string          `json:"device_id"`
		ProjectID string          `json:"project_id"`
		Reason    string          `json:"reason"`
		Metadata  json.RawMessage `json:"metadata,omitempty"`
		TimeoutS  int             `json:"timeout_s"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.RequestID == "" || req.DeviceID == "" {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	now := time.Now().UTC()
	var expiresAt *time.Time
	if req.TimeoutS > 0 {
		t := now.Add(time.Duration(req.TimeoutS) * time.Second)
		expiresAt = &t
	}
	metaJSON := []byte("null")
	if req.Metadata != nil {
		metaJSON = req.Metadata
	}
	_, err := f.db.ExecContext(r.Context(),
		`INSERT INTO assistance_requests
		 (request_id, device_id, org_id, project_id, reason, metadata, status, created_at, expires_at)
		 VALUES ($1, $2, $3, $4, $5, $6, 'pending', $7, $8)
		 ON CONFLICT (request_id) DO NOTHING`,
		req.RequestID, req.DeviceID, claims.OrgID, req.ProjectID,
		req.Reason, string(metaJSON), now, expiresAt,
	)
	if err != nil {
		slog.Error("upsert assistance request", "err", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"status": "pending"})
}

// ──────────────────────────────────────────────────────────────────────────────
// Router
// ──────────────────────────────────────────────────────────────────────────────

func (f *fleetAPI) routes(v *auth.Validator) http.Handler {
	mux := http.NewServeMux()
	protect := auth.HTTPMiddleware(v)

	// Go 1.21 ServeMux: no method routing, no {param} wildcards.
	// Exact patterns match only that path; trailing-slash patterns match any prefix.

	// /v1/vehicles — GET list, POST create
	mux.Handle("/v1/vehicles", protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			f.listVehicles(w, r)
		case http.MethodPost:
			f.registerVehicle(w, r)
		default:
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		}
	})))

	// /v1/vehicles/{id}              GET  — single vehicle
	// /v1/vehicles/{id}/missions     POST — create mission
	mux.Handle("/v1/vehicles/", protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		suffix := strings.TrimPrefix(r.URL.Path, "/v1/vehicles/")
		parts := strings.SplitN(suffix, "/", 2)
		switch {
		case len(parts) == 1 && r.Method == http.MethodGet:
			f.getVehicle(w, r)
		case len(parts) == 2 && parts[1] == "missions" && r.Method == http.MethodPost:
			f.createMission(w, r)
		default:
			http.NotFound(w, r)
		}
	})))

	// /v1/projects — GET list, POST create
	mux.Handle("/v1/projects", protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			f.listProjects(w, r)
		case http.MethodPost:
			f.createProject(w, r)
		default:
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		}
	})))

	// /v1/projects/{name}/devices    GET list, POST provision
	mux.Handle("/v1/projects/", protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		suffix := strings.TrimPrefix(r.URL.Path, "/v1/projects/")
		parts := strings.SplitN(suffix, "/", 2)
		if len(parts) == 2 && parts[1] == "devices" {
			switch r.Method {
			case http.MethodPost:
				f.provisionDevice(w, r)
			case http.MethodGet:
				f.listProjectDevices(w, r)
			default:
				http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			}
		} else {
			http.NotFound(w, r)
		}
	})))

	// /v1/assistance                          GET list, POST create
	// /v1/assistance/{request_id}/respond     POST respond
	mux.Handle("/v1/assistance", protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			f.listAssistance(w, r)
		case http.MethodPost:
			f.upsertAssistanceRequest(w, r)
		default:
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		}
	})))

	mux.Handle("/v1/assistance/", protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		suffix := strings.TrimPrefix(r.URL.Path, "/v1/assistance/")
		parts := strings.SplitN(suffix, "/", 2)
		if len(parts) == 2 && parts[1] == "respond" && r.Method == http.MethodPost {
			f.respondToAssistance(w, r)
		} else {
			http.NotFound(w, r)
		}
	})))

	// /v1/org/members       GET list
	// /v1/org/invite        POST invite
	mux.Handle("/v1/org/members", protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			f.listOrgMembers(w, r)
		} else {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		}
	})))

	// /v1/org/members/{id}         DELETE remove
	// /v1/org/members/{id}/access  PUT update
	mux.Handle("/v1/org/members/", protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		suffix := strings.TrimPrefix(r.URL.Path, "/v1/org/members/")
		parts := strings.SplitN(suffix, "/", 2)
		switch {
		case len(parts) == 2 && parts[1] == "access" && r.Method == http.MethodPut:
			f.updateMemberAccess(w, r)
		case len(parts) == 1 && r.Method == http.MethodDelete:
			f.removeMember(w, r)
		default:
			http.NotFound(w, r)
		}
	})))

	mux.Handle("/v1/org/invite", protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			f.inviteMember(w, r)
		} else {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		}
	})))

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok")
	})
	return mux
}

// pathSegmentFromEnd returns the nth URL path segment counting from the right (0-based).
// Trailing slashes are ignored.
//   - pathSegmentFromEnd("/v1/vehicles/abc-123", 0)         → "abc-123"
//   - pathSegmentFromEnd("/v1/vehicles/abc-123/missions", 1) → "abc-123"
//   - pathSegmentFromEnd("/v1/projects/my-fleet/devices", 1) → "my-fleet"
func pathSegmentFromEnd(urlPath string, n int) string {
	urlPath = strings.TrimSuffix(urlPath, "/")
	parts := strings.Split(urlPath, "/")
	idx := len(parts) - 1 - n
	if idx < 0 {
		return ""
	}
	return parts[idx]
}

// ──────────────────────────────────────────────────────────────────────────────
// DB schema migration (idempotent)
// ──────────────────────────────────────────────────────────────────────────────

func applySchema(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS vehicles (
			id           VARCHAR(36)  PRIMARY KEY,
			org_id       VARCHAR(36)  NOT NULL,
			project_id   VARCHAR(36),
			display_name VARCHAR(255) NOT NULL,
			vehicle_type VARCHAR(64),
			adapter_type VARCHAR(64),
			region       VARCHAR(64),
			created_at   TIMESTAMPTZ  NOT NULL,
			updated_at   TIMESTAMPTZ  NOT NULL,
			active       BOOL         NOT NULL DEFAULT true,
			INDEX idx_vehicles_org_active (org_id, active),
			INDEX idx_vehicles_project    (project_id)
		);
		CREATE TABLE IF NOT EXISTS missions (
			id         VARCHAR(36) PRIMARY KEY,
			vehicle_id VARCHAR(36) NOT NULL,
			org_id     VARCHAR(36) NOT NULL,
			name       VARCHAR(255),
			plan       TEXT,
			created_at TIMESTAMPTZ NOT NULL,
			status     VARCHAR(32) NOT NULL DEFAULT 'draft',
			INDEX idx_missions_vehicle (vehicle_id),
			INDEX idx_missions_org     (org_id)
		);
		CREATE TABLE IF NOT EXISTS projects (
			id           VARCHAR(36)  PRIMARY KEY,
			org_id       VARCHAR(36)  NOT NULL,
			name         VARCHAR(255) NOT NULL,
			display_name VARCHAR(255) NOT NULL DEFAULT '',
			created_at   TIMESTAMPTZ  NOT NULL,
			active       BOOL         NOT NULL DEFAULT true,
			UNIQUE INDEX idx_projects_org_name   (org_id, name),
			INDEX        idx_projects_org_active (org_id, active)
		);
		CREATE TABLE IF NOT EXISTS assistance_requests (
			request_id   VARCHAR(36) PRIMARY KEY,
			device_id    VARCHAR(36) NOT NULL,
			org_id       VARCHAR(36) NOT NULL,
			project_id   VARCHAR(36),
			reason       TEXT        NOT NULL,
			metadata     JSONB,
			status       VARCHAR(32) NOT NULL DEFAULT 'pending',
			created_at   TIMESTAMPTZ NOT NULL,
			expires_at   TIMESTAMPTZ,
			responded_at TIMESTAMPTZ,
			instruction  TEXT,
			INDEX idx_assist_org_status (org_id, status),
			INDEX idx_assist_device     (device_id)
		);
	`)
	return err
}

// ──────────────────────────────────────────────────────────────────────────────
// Main
// ──────────────────────────────────────────────────────────────────────────────

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	httpAddr  := envOr("HTTP_ADDR", ":8080")
	jwksURLs  := envOr("JWKS_URLS", envOr("JWKS_URL",
		"https://keycloak.internal/realms/systemscale/protocol/openid-connect/certs"))
	dbURL     := envOr("DATABASE_URL", "postgresql://root@localhost:26257/systemscale?sslmode=disable")
	regionID  := envOr("REGION_ID", "local")
	relayAddr := envOr("RELAY_ADDR", "relay.systemscale.io:443")

	kcBaseURL      := envOr("KEYCLOAK_URL", "http://keycloak:8080")
	kcRealm        := envOr("KEYCLOAK_REALM", "systemscale")
	kcClientID     := envOr("KEYCLOAK_CLIENT_ID", "fleet-api-service")
	kcClientSecret := envOr("KEYCLOAK_CLIENT_SECRET", "fleet-api-secret")

	slog.Info("Starting fleet-api", "addr", httpAddr, "region", regionID)

	validator, err := auth.NewValidator(jwksURLs)
	if err != nil {
		slog.Error("auth validator init", "err", err)
		os.Exit(1)
	}

	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		slog.Error("DB open", "err", err)
		os.Exit(1)
	}
	defer db.Close()
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(10)
	db.SetConnMaxLifetime(5 * time.Minute)

	if err := applySchema(db); err != nil {
		slog.Error("schema migration failed", "err", err)
		os.Exit(1)
	}

	kc := &keycloakClient{
		baseURL: kcBaseURL, realm: kcRealm,
		clientID: kcClientID, clientSecret: kcClientSecret,
	}
	api := &fleetAPI{db: db, relayAddr: relayAddr, region: regionID, kc: kc}
	server := &http.Server{
		Addr:         httpAddr,
		Handler:      corsMiddleware(api.routes(validator)),
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	go func() {
		slog.Info("fleet-api HTTP server ready", "addr", httpAddr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("HTTP serve error", "err", err)
		}
	}()

	<-ctx.Done()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	server.Shutdown(shutdownCtx)
	slog.Info("fleet-api shutdown complete")
}

// ──────────────────────────────────────────────────────────────────────────────
// Helpers
// ──────────────────────────────────────────────────────────────────────────────

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := os.Getenv("CORS_ALLOW_ORIGIN")
		if origin == "" {
			origin = "*"
		}
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
