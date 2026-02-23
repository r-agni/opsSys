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
	"time"

	_ "github.com/lib/pq"
	"github.com/systemscale/services/shared/auth"
)

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

type config struct {
	HTTPAddr           string
	DatabaseURL        string
	JWKSURLs           string
	CORSAllowOrigin    string
	KeycloakURL        string
	KeycloakRealm      string
	KeycloakClientID   string
	KeycloakClientSecret string
}

func loadConfig() config {
	return config{
		HTTPAddr:             envOr("HTTP_ADDR", ":8080"),
		DatabaseURL:          envOr("DATABASE_URL", "postgresql://root@localhost:26257/systemscale?sslmode=disable"),
		JWKSURLs:             envOr("JWKS_URLS", ""),
		CORSAllowOrigin:      envOr("CORS_ALLOW_ORIGIN", "*"),
		KeycloakURL:          envOr("KEYCLOAK_URL", "http://localhost:8180"),
		KeycloakRealm:        envOr("KEYCLOAK_REALM", "systemscale"),
		KeycloakClientID:     envOr("KEYCLOAK_CLIENT_ID", "fleet-api-service"),
		KeycloakClientSecret: envOr("KEYCLOAK_CLIENT_SECRET", "fleet-api-secret"),
	}
}

// ---------------------------------------------------------------------------
// Database schema
// ---------------------------------------------------------------------------

func applySchema(db *sql.DB) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS projects (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			org_id TEXT NOT NULL,
			name TEXT NOT NULL,
			display_name TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			active BOOL NOT NULL DEFAULT true,
			UNIQUE (org_id, name)
		)`,
		`CREATE TABLE IF NOT EXISTS vehicles (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			org_id TEXT NOT NULL,
			project_id UUID NOT NULL REFERENCES projects(id),
			display_name TEXT NOT NULL,
			vehicle_type TEXT NOT NULL DEFAULT 'generic',
			adapter_type TEXT NOT NULL DEFAULT 'custom_unix',
			region TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			active BOOL NOT NULL DEFAULT true
		)`,
		`CREATE TABLE IF NOT EXISTS assistance_requests (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			request_id TEXT NOT NULL UNIQUE,
			device_id UUID NOT NULL,
			project_id UUID NOT NULL,
			org_id TEXT NOT NULL,
			reason TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'pending',
			approved BOOL,
			instruction TEXT NOT NULL DEFAULT '',
			timeout_s INT NOT NULL DEFAULT 120,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			responded_at TIMESTAMPTZ
		)`,
	}
	for _, stmt := range statements {
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("apply schema: %w", err)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Keycloak admin client
// ---------------------------------------------------------------------------

type keycloakClient struct {
	baseURL      string
	realm        string
	clientID     string
	clientSecret string

	mu          sync.Mutex
	accessToken string
	expiry      time.Time
}

func (kc *keycloakClient) token(ctx context.Context) (string, error) {
	kc.mu.Lock()
	defer kc.mu.Unlock()

	if kc.accessToken != "" && time.Now().Before(kc.expiry.Add(-30*time.Second)) {
		return kc.accessToken, nil
	}

	tokenURL := fmt.Sprintf("%s/realms/%s/protocol/openid-connect/token", kc.baseURL, kc.realm)
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {kc.clientID},
		"client_secret": {kc.clientSecret},
	}

	req, err := http.NewRequestWithContext(ctx, "POST", tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("keycloak token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("keycloak token fetch: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("keycloak token: %d: %s", resp.StatusCode, body)
	}

	var tok struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
		return "", fmt.Errorf("keycloak token decode: %w", err)
	}

	kc.accessToken = tok.AccessToken
	kc.expiry = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
	return kc.accessToken, nil
}

type kcUser struct {
	ID         string              `json:"id"`
	Username   string              `json:"username"`
	Email      string              `json:"email"`
	FirstName  string              `json:"firstName"`
	LastName   string              `json:"lastName"`
	Enabled    bool                `json:"enabled"`
	Attributes map[string][]string `json:"attributes"`
}

// ---------------------------------------------------------------------------
// Server
// ---------------------------------------------------------------------------

type server struct {
	db        *sql.DB
	validator *auth.Validator
	kc        *keycloakClient
}

// ---------------------------------------------------------------------------
// Handlers — health
// ---------------------------------------------------------------------------

func (s *server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if err := s.db.PingContext(r.Context()); err != nil {
		http.Error(w, "db unreachable", http.StatusServiceUnavailable)
		return
	}
	fmt.Fprint(w, "ok")
}

// ---------------------------------------------------------------------------
// Handlers — projects
// ---------------------------------------------------------------------------

func (s *server) handleListProjects(w http.ResponseWriter, r *http.Request) {
	claims, ok := auth.ClaimsFromContext(r.Context())
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	rows, err := s.db.QueryContext(r.Context(),
		`SELECT id, org_id, name, display_name, created_at, active
		 FROM projects WHERE org_id = $1 AND active = true ORDER BY created_at DESC`, claims.OrgID)
	if err != nil {
		slog.Error("list projects", "err", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type project struct {
		ID          string    `json:"id"`
		OrgID       string    `json:"org_id"`
		Name        string    `json:"name"`
		DisplayName string    `json:"display_name"`
		CreatedAt   time.Time `json:"created_at"`
		Active      bool      `json:"active"`
	}

	projects := make([]project, 0)
	for rows.Next() {
		var p project
		if err := rows.Scan(&p.ID, &p.OrgID, &p.Name, &p.DisplayName, &p.CreatedAt, &p.Active); err != nil {
			slog.Error("scan project", "err", err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		projects = append(projects, p)
	}
	if err := rows.Err(); err != nil {
		slog.Error("rows project", "err", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"projects": projects,
		"count":    len(projects),
	})
}

func (s *server) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	claims, ok := auth.ClaimsFromContext(r.Context())
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
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

	type project struct {
		ID          string    `json:"id"`
		OrgID       string    `json:"org_id"`
		Name        string    `json:"name"`
		DisplayName string    `json:"display_name"`
		CreatedAt   time.Time `json:"created_at"`
		Active      bool      `json:"active"`
	}

	var p project
	err := s.db.QueryRowContext(r.Context(),
		`INSERT INTO projects (org_id, name, display_name)
		 VALUES ($1, $2, $3)
		 RETURNING id, org_id, name, display_name, created_at, active`,
		claims.OrgID, req.Name, req.DisplayName,
	).Scan(&p.ID, &p.OrgID, &p.Name, &p.DisplayName, &p.CreatedAt, &p.Active)
	if err != nil {
		if strings.Contains(err.Error(), "duplicate key") {
			http.Error(w, "Conflict: project name already exists", http.StatusConflict)
			return
		}
		slog.Error("create project", "err", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusCreated, p)
}

// ---------------------------------------------------------------------------
// Handlers — devices (vehicles scoped to a project)
// ---------------------------------------------------------------------------

type vehicleRow struct {
	ID          string    `json:"id"`
	OrgID       string    `json:"org_id"`
	ProjectID   string    `json:"project_id"`
	DisplayName string    `json:"display_name"`
	VehicleType string    `json:"vehicle_type"`
	AdapterType string    `json:"adapter_type"`
	Region      string    `json:"region"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	Active      bool      `json:"active"`
}

func (s *server) handleListProjectDevices(w http.ResponseWriter, r *http.Request) {
	claims, ok := auth.ClaimsFromContext(r.Context())
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	projectName := r.PathValue("name")
	if projectName == "" {
		http.Error(w, "Bad Request: project name required", http.StatusBadRequest)
		return
	}

	var projectID string
	err := s.db.QueryRowContext(r.Context(),
		`SELECT id FROM projects WHERE name = $1 AND org_id = $2 AND active = true`,
		projectName, claims.OrgID,
	).Scan(&projectID)
	if err == sql.ErrNoRows {
		http.Error(w, "Not Found: project not found", http.StatusNotFound)
		return
	}
	if err != nil {
		slog.Error("lookup project", "err", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	rows, err := s.db.QueryContext(r.Context(),
		`SELECT id, org_id, project_id, display_name, vehicle_type, adapter_type, region, created_at, updated_at, active
		 FROM vehicles WHERE project_id = $1 AND org_id = $2 AND active = true ORDER BY created_at DESC`,
		projectID, claims.OrgID)
	if err != nil {
		slog.Error("list project devices", "err", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	devices := make([]vehicleRow, 0)
	for rows.Next() {
		var v vehicleRow
		if err := rows.Scan(&v.ID, &v.OrgID, &v.ProjectID, &v.DisplayName, &v.VehicleType, &v.AdapterType, &v.Region, &v.CreatedAt, &v.UpdatedAt, &v.Active); err != nil {
			slog.Error("scan device", "err", err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		devices = append(devices, v)
	}
	if err := rows.Err(); err != nil {
		slog.Error("rows device", "err", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"devices": devices,
		"count":   len(devices),
	})
}

func (s *server) handleCreateProjectDevice(w http.ResponseWriter, r *http.Request) {
	claims, ok := auth.ClaimsFromContext(r.Context())
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	projectName := r.PathValue("name")
	if projectName == "" {
		http.Error(w, "Bad Request: project name required", http.StatusBadRequest)
		return
	}

	var projectID string
	err := s.db.QueryRowContext(r.Context(),
		`SELECT id FROM projects WHERE name = $1 AND org_id = $2 AND active = true`,
		projectName, claims.OrgID,
	).Scan(&projectID)
	if err == sql.ErrNoRows {
		http.Error(w, "Not Found: project not found", http.StatusNotFound)
		return
	}
	if err != nil {
		slog.Error("lookup project for device", "err", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
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
	if req.VehicleType == "" {
		req.VehicleType = "generic"
	}
	if req.AdapterType == "" {
		req.AdapterType = "custom_unix"
	}

	var resp struct {
		ID          string `json:"id"`
		DisplayName string `json:"display_name"`
		VehicleType string `json:"vehicle_type"`
		ProjectID   string `json:"project_id"`
	}
	err = s.db.QueryRowContext(r.Context(),
		`INSERT INTO vehicles (org_id, project_id, display_name, vehicle_type, adapter_type, region)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 RETURNING id, display_name, vehicle_type, project_id`,
		claims.OrgID, projectID, req.DisplayName, req.VehicleType, req.AdapterType, req.Region,
	).Scan(&resp.ID, &resp.DisplayName, &resp.VehicleType, &resp.ProjectID)
	if err != nil {
		slog.Error("create device", "err", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusCreated, resp)
}

// ---------------------------------------------------------------------------
// Handlers — vehicles (org-wide)
// ---------------------------------------------------------------------------

func (s *server) handleListVehicles(w http.ResponseWriter, r *http.Request) {
	claims, ok := auth.ClaimsFromContext(r.Context())
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	rows, err := s.db.QueryContext(r.Context(),
		`SELECT id, org_id, project_id, display_name, vehicle_type, adapter_type, region, created_at, updated_at, active
		 FROM vehicles WHERE org_id = $1 AND active = true ORDER BY created_at DESC`, claims.OrgID)
	if err != nil {
		slog.Error("list vehicles", "err", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	vehicles := make([]vehicleRow, 0)
	for rows.Next() {
		var v vehicleRow
		if err := rows.Scan(&v.ID, &v.OrgID, &v.ProjectID, &v.DisplayName, &v.VehicleType, &v.AdapterType, &v.Region, &v.CreatedAt, &v.UpdatedAt, &v.Active); err != nil {
			slog.Error("scan vehicle", "err", err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		vehicles = append(vehicles, v)
	}
	if err := rows.Err(); err != nil {
		slog.Error("rows vehicle", "err", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"vehicles": vehicles,
		"count":    len(vehicles),
	})
}

func (s *server) handleGetVehicle(w http.ResponseWriter, r *http.Request) {
	claims, ok := auth.ClaimsFromContext(r.Context())
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	vehicleID := r.PathValue("id")
	if vehicleID == "" {
		http.Error(w, "Bad Request: vehicle id required", http.StatusBadRequest)
		return
	}

	var v vehicleRow
	err := s.db.QueryRowContext(r.Context(),
		`SELECT id, org_id, project_id, display_name, vehicle_type, adapter_type, region, created_at, updated_at, active
		 FROM vehicles WHERE id = $1 AND org_id = $2`,
		vehicleID, claims.OrgID,
	).Scan(&v.ID, &v.OrgID, &v.ProjectID, &v.DisplayName, &v.VehicleType, &v.AdapterType, &v.Region, &v.CreatedAt, &v.UpdatedAt, &v.Active)
	if err == sql.ErrNoRows {
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}
	if err != nil {
		slog.Error("get vehicle", "err", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, v)
}

// ---------------------------------------------------------------------------
// Handlers — assistance requests
// ---------------------------------------------------------------------------

func (s *server) handleCreateAssistance(w http.ResponseWriter, r *http.Request) {
	claims, ok := auth.ClaimsFromContext(r.Context())
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	var req struct {
		RequestID string `json:"request_id"`
		DeviceID  string `json:"device_id"`
		ProjectID string `json:"project_id"`
		Reason    string `json:"reason"`
		TimeoutS  int    `json:"timeout_s"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.RequestID == "" || req.DeviceID == "" || req.ProjectID == "" {
		http.Error(w, "Bad Request: request_id, device_id, project_id required", http.StatusBadRequest)
		return
	}
	if req.TimeoutS <= 0 {
		req.TimeoutS = 120
	}

	type assistanceResp struct {
		ID        string    `json:"id"`
		RequestID string    `json:"request_id"`
		DeviceID  string    `json:"device_id"`
		ProjectID string    `json:"project_id"`
		OrgID     string    `json:"org_id"`
		Reason    string    `json:"reason"`
		Status    string    `json:"status"`
		TimeoutS  int       `json:"timeout_s"`
		CreatedAt time.Time `json:"created_at"`
	}

	var a assistanceResp
	err := s.db.QueryRowContext(r.Context(),
		`INSERT INTO assistance_requests (request_id, device_id, project_id, org_id, reason, timeout_s)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 RETURNING id, request_id, device_id, project_id, org_id, reason, status, timeout_s, created_at`,
		req.RequestID, req.DeviceID, req.ProjectID, claims.OrgID, req.Reason, req.TimeoutS,
	).Scan(&a.ID, &a.RequestID, &a.DeviceID, &a.ProjectID, &a.OrgID, &a.Reason, &a.Status, &a.TimeoutS, &a.CreatedAt)
	if err != nil {
		if strings.Contains(err.Error(), "duplicate key") {
			http.Error(w, "Conflict: request_id already exists", http.StatusConflict)
			return
		}
		slog.Error("create assistance", "err", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusCreated, a)
}

func (s *server) handleListAssistance(w http.ResponseWriter, r *http.Request) {
	claims, ok := auth.ClaimsFromContext(r.Context())
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	rows, err := s.db.QueryContext(r.Context(),
		`SELECT id, request_id, device_id, project_id, org_id, reason, status, timeout_s, created_at
		 FROM assistance_requests WHERE org_id = $1 AND status = 'pending' ORDER BY created_at DESC`,
		claims.OrgID)
	if err != nil {
		slog.Error("list assistance", "err", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type assistanceItem struct {
		ID        string    `json:"id"`
		RequestID string    `json:"request_id"`
		DeviceID  string    `json:"device_id"`
		ProjectID string    `json:"project_id"`
		OrgID     string    `json:"org_id"`
		Reason    string    `json:"reason"`
		Status    string    `json:"status"`
		TimeoutS  int       `json:"timeout_s"`
		CreatedAt time.Time `json:"created_at"`
	}

	requests := make([]assistanceItem, 0)
	for rows.Next() {
		var a assistanceItem
		if err := rows.Scan(&a.ID, &a.RequestID, &a.DeviceID, &a.ProjectID, &a.OrgID, &a.Reason, &a.Status, &a.TimeoutS, &a.CreatedAt); err != nil {
			slog.Error("scan assistance", "err", err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		requests = append(requests, a)
	}
	if err := rows.Err(); err != nil {
		slog.Error("rows assistance", "err", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"requests": requests,
	})
}

func (s *server) handleRespondAssistance(w http.ResponseWriter, r *http.Request) {
	claims, ok := auth.ClaimsFromContext(r.Context())
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	assistanceID := r.PathValue("id")
	if assistanceID == "" {
		http.Error(w, "Bad Request: assistance id required", http.StatusBadRequest)
		return
	}

	var req struct {
		Approved    *bool  `json:"approved"`
		Instruction string `json:"instruction"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Approved == nil {
		http.Error(w, "Bad Request: approved required", http.StatusBadRequest)
		return
	}

	res, err := s.db.ExecContext(r.Context(),
		`UPDATE assistance_requests
		 SET status = 'responded', approved = $1, instruction = $2, responded_at = now()
		 WHERE request_id = $3 AND org_id = $4 AND status = 'pending'`,
		*req.Approved, req.Instruction, assistanceID, claims.OrgID)
	if err != nil {
		slog.Error("respond assistance", "err", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		http.Error(w, "Not Found: assistance request not found or already responded", http.StatusNotFound)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "responded"})
}

// ---------------------------------------------------------------------------
// Handlers — org member management (Keycloak proxy)
// ---------------------------------------------------------------------------

type orgMember struct {
	ID        string `json:"id"`
	Username  string `json:"username"`
	Email     string `json:"email"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Role      string `json:"role"`
	Enabled   bool   `json:"enabled"`
}

func (s *server) handleListOrgMembers(w http.ResponseWriter, r *http.Request) {
	claims, ok := auth.ClaimsFromContext(r.Context())
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	tok, err := s.kc.token(r.Context())
	if err != nil {
		slog.Error("keycloak token", "err", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	kcURL := fmt.Sprintf("%s/admin/realms/%s/users?q=%s&max=500",
		s.kc.baseURL, s.kc.realm,
		url.QueryEscape("org_id:"+claims.OrgID))

	req, err := http.NewRequestWithContext(r.Context(), "GET", kcURL, nil)
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	req.Header.Set("Authorization", "Bearer "+tok)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		slog.Error("keycloak list users", "err", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		slog.Error("keycloak list users", "status", resp.StatusCode, "body", string(body))
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	var kcUsers []kcUser
	if err := json.NewDecoder(resp.Body).Decode(&kcUsers); err != nil {
		slog.Error("decode keycloak users", "err", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	members := make([]orgMember, 0, len(kcUsers))
	for _, u := range kcUsers {
		role := ""
		if roles, ok := u.Attributes["role"]; ok && len(roles) > 0 {
			role = roles[0]
		}
		members = append(members, orgMember{
			ID:        u.ID,
			Username:  u.Username,
			Email:     u.Email,
			FirstName: u.FirstName,
			LastName:  u.LastName,
			Role:      role,
			Enabled:   u.Enabled,
		})
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"members": members,
		"count":   len(members),
	})
}

func (s *server) handleInviteMember(w http.ResponseWriter, r *http.Request) {
	claims, ok := auth.ClaimsFromContext(r.Context())
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	var body struct {
		Email     string `json:"email"`
		Username  string `json:"username"`
		FirstName string `json:"first_name"`
		LastName  string `json:"last_name"`
		Role      string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Email == "" {
		http.Error(w, "Bad Request: email required", http.StatusBadRequest)
		return
	}
	if body.Username == "" {
		body.Username = body.Email
	}
	if body.Role == "" {
		body.Role = "viewer"
	}

	tok, err := s.kc.token(r.Context())
	if err != nil {
		slog.Error("keycloak token", "err", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	kcBody, _ := json.Marshal(map[string]interface{}{
		"username":  body.Username,
		"email":     body.Email,
		"firstName": body.FirstName,
		"lastName":  body.LastName,
		"enabled":   true,
		"attributes": map[string][]string{
			"org_id": {claims.OrgID},
			"role":   {body.Role},
		},
	})

	kcURL := fmt.Sprintf("%s/admin/realms/%s/users", s.kc.baseURL, s.kc.realm)
	req, err := http.NewRequestWithContext(r.Context(), "POST", kcURL, bytes.NewReader(kcBody))
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		slog.Error("keycloak create user", "err", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		errBody, _ := io.ReadAll(resp.Body)
		slog.Error("keycloak create user", "status", resp.StatusCode, "body", string(errBody))
		http.Error(w, fmt.Sprintf("Keycloak error: %s", string(errBody)), resp.StatusCode)
		return
	}

	writeJSON(w, http.StatusCreated, map[string]string{"status": "invited"})
}

func (s *server) handleUpdateMemberAccess(w http.ResponseWriter, r *http.Request) {
	claims, ok := auth.ClaimsFromContext(r.Context())
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	userID := r.PathValue("userId")
	if userID == "" {
		http.Error(w, "Bad Request: userId required", http.StatusBadRequest)
		return
	}

	var body struct {
		Role string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Role == "" {
		http.Error(w, "Bad Request: role required", http.StatusBadRequest)
		return
	}

	tok, err := s.kc.token(r.Context())
	if err != nil {
		slog.Error("keycloak token", "err", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	// Fetch the user to verify org membership
	user, err := s.kcGetUser(r.Context(), tok, userID)
	if err != nil {
		slog.Error("keycloak get user", "err", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	if user == nil {
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}
	if orgIDs, ok := user.Attributes["org_id"]; !ok || len(orgIDs) == 0 || orgIDs[0] != claims.OrgID {
		http.Error(w, "Forbidden: user not in your org", http.StatusForbidden)
		return
	}

	user.Attributes["role"] = []string{body.Role}
	kcBody, _ := json.Marshal(map[string]interface{}{
		"attributes": user.Attributes,
	})

	kcURL := fmt.Sprintf("%s/admin/realms/%s/users/%s", s.kc.baseURL, s.kc.realm, userID)
	req, err := http.NewRequestWithContext(r.Context(), "PUT", kcURL, bytes.NewReader(kcBody))
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		slog.Error("keycloak update user", "err", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(resp.Body)
		slog.Error("keycloak update user", "status", resp.StatusCode, "body", string(errBody))
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

func (s *server) handleDeleteMember(w http.ResponseWriter, r *http.Request) {
	claims, ok := auth.ClaimsFromContext(r.Context())
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	userID := r.PathValue("userId")
	if userID == "" {
		http.Error(w, "Bad Request: userId required", http.StatusBadRequest)
		return
	}

	tok, err := s.kc.token(r.Context())
	if err != nil {
		slog.Error("keycloak token", "err", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	// Verify user belongs to the same org before deleting
	user, err := s.kcGetUser(r.Context(), tok, userID)
	if err != nil {
		slog.Error("keycloak get user", "err", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	if user == nil {
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}
	if orgIDs, ok := user.Attributes["org_id"]; !ok || len(orgIDs) == 0 || orgIDs[0] != claims.OrgID {
		http.Error(w, "Forbidden: user not in your org", http.StatusForbidden)
		return
	}

	kcURL := fmt.Sprintf("%s/admin/realms/%s/users/%s", s.kc.baseURL, s.kc.realm, userID)
	req, err := http.NewRequestWithContext(r.Context(), "DELETE", kcURL, nil)
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	req.Header.Set("Authorization", "Bearer "+tok)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		slog.Error("keycloak delete user", "err", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(resp.Body)
		slog.Error("keycloak delete user", "status", resp.StatusCode, "body", string(errBody))
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// kcGetUser fetches a single Keycloak user by ID.
func (s *server) kcGetUser(ctx context.Context, token, userID string) (*kcUser, error) {
	kcURL := fmt.Sprintf("%s/admin/realms/%s/users/%s", s.kc.baseURL, s.kc.realm, userID)
	req, err := http.NewRequestWithContext(ctx, "GET", kcURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("keycloak get user %s: %d: %s", userID, resp.StatusCode, body)
	}

	var u kcUser
	if err := json.NewDecoder(resp.Body).Decode(&u); err != nil {
		return nil, err
	}
	return &u, nil
}

// ---------------------------------------------------------------------------
// Routing
// ---------------------------------------------------------------------------

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	protect := auth.HTTPMiddleware(s.validator)

	mux.HandleFunc("GET /healthz", s.handleHealthz)

	mux.Handle("GET /v1/projects", protect(http.HandlerFunc(s.handleListProjects)))
	mux.Handle("POST /v1/projects", protect(http.HandlerFunc(s.handleCreateProject)))
	mux.Handle("GET /v1/projects/{name}/devices", protect(http.HandlerFunc(s.handleListProjectDevices)))
	mux.Handle("POST /v1/projects/{name}/devices", protect(http.HandlerFunc(s.handleCreateProjectDevice)))

	mux.Handle("GET /v1/vehicles", protect(http.HandlerFunc(s.handleListVehicles)))
	mux.Handle("GET /v1/vehicles/{id}", protect(http.HandlerFunc(s.handleGetVehicle)))

	mux.Handle("POST /v1/assistance", protect(http.HandlerFunc(s.handleCreateAssistance)))
	mux.Handle("GET /v1/assistance", protect(http.HandlerFunc(s.handleListAssistance)))
	mux.Handle("POST /v1/assistance/{id}/respond", protect(http.HandlerFunc(s.handleRespondAssistance)))

	mux.Handle("GET /v1/org/members", protect(http.HandlerFunc(s.handleListOrgMembers)))
	mux.Handle("POST /v1/org/invite", protect(http.HandlerFunc(s.handleInviteMember)))
	mux.Handle("PUT /v1/org/members/{userId}/access", protect(http.HandlerFunc(s.handleUpdateMemberAccess)))
	mux.Handle("DELETE /v1/org/members/{userId}", protect(http.HandlerFunc(s.handleDeleteMember)))

	return corsMiddleware(mux)
}

// ---------------------------------------------------------------------------
// Middleware & helpers
// ---------------------------------------------------------------------------

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := envOr("CORS_ALLOW_ORIGIN", "*")
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

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	cfg := loadConfig()
	slog.Info("starting fleet-api", "addr", cfg.HTTPAddr)

	db, err := sql.Open("postgres", cfg.DatabaseURL)
	if err != nil {
		slog.Error("db open", "err", err)
		os.Exit(1)
	}
	defer db.Close()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	if err := db.PingContext(ctx); err != nil {
		slog.Error("db ping", "err", err)
		os.Exit(1)
	}
	slog.Info("connected to CockroachDB")

	if err := applySchema(db); err != nil {
		slog.Error("apply schema", "err", err)
		os.Exit(1)
	}
	slog.Info("schema applied")

	validator, err := auth.NewValidator(cfg.JWKSURLs)
	if err != nil {
		slog.Error("auth validator init", "err", err)
		os.Exit(1)
	}

	srv := &server{
		db:        db,
		validator: validator,
		kc: &keycloakClient{
			baseURL:      cfg.KeycloakURL,
			realm:        cfg.KeycloakRealm,
			clientID:     cfg.KeycloakClientID,
			clientSecret: cfg.KeycloakClientSecret,
		},
	}

	httpServer := &http.Server{
		Addr:         cfg.HTTPAddr,
		Handler:      srv.routes(),
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	go func() {
		<-ctx.Done()
		slog.Info("graceful shutdown initiated")
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutCancel()
		if err := httpServer.Shutdown(shutCtx); err != nil {
			slog.Error("http shutdown", "err", err)
		}
	}()

	slog.Info("fleet-api ready", "addr", cfg.HTTPAddr)
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("http serve", "err", err)
		os.Exit(1)
	}
}
