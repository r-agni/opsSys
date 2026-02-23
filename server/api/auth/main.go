package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/golang-jwt/jwt/v5"
	_ "github.com/lib/pq"
	"github.com/systemscale/services/shared/auth"
)

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

type config struct {
	HTTPAddr        string
	DatabaseURL     string
	JWKSURLs        string
	CORSAllowOrigin string
}

func loadConfig() config {
	return config{
		HTTPAddr:         envOr("HTTP_ADDR", ":8083"),
		DatabaseURL:      envOr("DATABASE_URL", "postgresql://root@localhost:26257/systemscale?sslmode=disable"),
		JWKSURLs:         envOr("JWKS_URLS", ""),
		CORSAllowOrigin:  envOr("CORS_ALLOW_ORIGIN", "*"),
	}
}

// ---------------------------------------------------------------------------
// Database schema
// ---------------------------------------------------------------------------

func applySchema(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS api_keys (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			org_id TEXT NOT NULL,
			name TEXT NOT NULL,
			key_prefix TEXT NOT NULL,
			key_hash TEXT NOT NULL,
			scopes TEXT[] NOT NULL DEFAULT '{}',
			created_by TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			revoked BOOL NOT NULL DEFAULT false,
			revoked_at TIMESTAMPTZ
		)`)
	return err
}

// ---------------------------------------------------------------------------
// Server
// ---------------------------------------------------------------------------

type server struct {
	db        *sql.DB
	validator *auth.Validator
	signer    *auth.Signer
}

// ---------------------------------------------------------------------------
// Handlers — public
// ---------------------------------------------------------------------------

func (s *server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if err := s.db.PingContext(r.Context()); err != nil {
		http.Error(w, "db unreachable", http.StatusServiceUnavailable)
		return
	}
	fmt.Fprint(w, "ok")
}

func (s *server) handleJWKS(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.signer.JWKS())
}

// POST /v1/token — exchange an API key for a short-lived JWT.
func (s *server) handleToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		APIKey string `json:"api_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.APIKey == "" {
		http.Error(w, "Bad Request: api_key required", http.StatusBadRequest)
		return
	}

	hash := sha256.Sum256([]byte(req.APIKey))
	hashHex := hex.EncodeToString(hash[:])

	var (
		keyID     string
		orgID     string
		name      string
		createdBy string
		revoked   bool
	)
	// lib/pq reads TEXT[] into a pq.StringArray; we scan into a helper slice.
	var scopesRaw []byte
	err := s.db.QueryRowContext(r.Context(),
		`SELECT id, org_id, name, created_by, revoked, scopes::TEXT
		 FROM api_keys WHERE key_hash = $1`, hashHex,
	).Scan(&keyID, &orgID, &name, &createdBy, &revoked, &scopesRaw)
	if err == sql.ErrNoRows {
		http.Error(w, "Unauthorized: invalid api key", http.StatusUnauthorized)
		return
	}
	if err != nil {
		slog.Error("token lookup", "err", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	if revoked {
		http.Error(w, "Unauthorized: api key revoked", http.StatusUnauthorized)
		return
	}

	scopes := parsePGTextArray(string(scopesRaw))

	role := "viewer"
	for _, sc := range scopes {
		if sc == "admin" {
			role = "admin"
			break
		}
	}

	now := time.Now()
	expiry := now.Add(time.Hour)

	claims := jwt.MapClaims{
		"sub":         keyID,
		"org_id":      orgID,
		"role":        role,
		"vehicle_set": []string{"*"},
		"email":       createdBy,
		"iss":         "systemscale-apikey",
		"iat":         now.Unix(),
		"exp":         expiry.Unix(),
	}

	tokenStr, err := s.signer.SignToken(claims)
	if err != nil {
		slog.Error("sign token", "err", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"access_token": tokenStr,
		"token_type":   "bearer",
		"expires_in":   3600,
	})
}

// ---------------------------------------------------------------------------
// Handlers — protected (Keycloak JWT required)
// ---------------------------------------------------------------------------

func (s *server) handleCreateKey(w http.ResponseWriter, r *http.Request) {
	claims, ok := auth.ClaimsFromContext(r.Context())
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	var req struct {
		Name   string   `json:"name"`
		Scopes []string `json:"scopes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
		http.Error(w, "Bad Request: name required", http.StatusBadRequest)
		return
	}
	if req.Scopes == nil {
		req.Scopes = []string{}
	}

	randomBytes := make([]byte, 16)
	if _, err := rand.Read(randomBytes); err != nil {
		slog.Error("generate key random", "err", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	randomHex := hex.EncodeToString(randomBytes) // 32 hex chars
	fullKey := "ssk_live_" + randomHex
	keyPrefix := randomHex[:12]

	hash := sha256.Sum256([]byte(fullKey))
	hashHex := hex.EncodeToString(hash[:])

	// Build Postgres array literal for scopes
	scopesPG := "{" + strings.Join(quotePGArray(req.Scopes), ",") + "}"

	var (
		id        string
		createdAt time.Time
	)
	err := s.db.QueryRowContext(r.Context(),
		`INSERT INTO api_keys (org_id, name, key_prefix, key_hash, scopes, created_by)
		 VALUES ($1, $2, $3, $4, $5::TEXT[], $6)
		 RETURNING id, created_at`,
		claims.OrgID, req.Name, keyPrefix, hashHex, scopesPG, claims.Email,
	).Scan(&id, &createdAt)
	if err != nil {
		slog.Error("create api key", "err", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"id":         id,
		"key_prefix": "ssk_live_" + keyPrefix,
		"api_key":    fullKey,
		"name":       req.Name,
		"scopes":     req.Scopes,
		"created_at": createdAt,
	})
}

func (s *server) handleListKeys(w http.ResponseWriter, r *http.Request) {
	claims, ok := auth.ClaimsFromContext(r.Context())
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	rows, err := s.db.QueryContext(r.Context(),
		`SELECT id, name, key_prefix, scopes::TEXT, created_by, created_at
		 FROM api_keys WHERE org_id = $1 AND revoked = false ORDER BY created_at DESC`,
		claims.OrgID)
	if err != nil {
		slog.Error("list keys", "err", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type keyItem struct {
		ID        string    `json:"id"`
		Name      string    `json:"name"`
		KeyPrefix string    `json:"key_prefix"`
		Scopes    []string  `json:"scopes"`
		CreatedBy string    `json:"created_by"`
		CreatedAt time.Time `json:"created_at"`
	}

	keys := make([]keyItem, 0)
	for rows.Next() {
		var k keyItem
		var scopesRaw string
		if err := rows.Scan(&k.ID, &k.Name, &k.KeyPrefix, &scopesRaw, &k.CreatedBy, &k.CreatedAt); err != nil {
			slog.Error("scan key", "err", err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		k.Scopes = parsePGTextArray(scopesRaw)
		k.KeyPrefix = "ssk_live_" + k.KeyPrefix
		keys = append(keys, k)
	}
	if err := rows.Err(); err != nil {
		slog.Error("rows keys", "err", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"keys":  keys,
		"count": len(keys),
	})
}

func (s *server) handleRevokeKey(w http.ResponseWriter, r *http.Request) {
	claims, ok := auth.ClaimsFromContext(r.Context())
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	keyID := r.PathValue("id")
	if keyID == "" {
		http.Error(w, "Bad Request: key id required", http.StatusBadRequest)
		return
	}

	res, err := s.db.ExecContext(r.Context(),
		`UPDATE api_keys SET revoked = true, revoked_at = now()
		 WHERE id = $1 AND org_id = $2 AND revoked = false`,
		keyID, claims.OrgID)
	if err != nil {
		slog.Error("revoke key", "err", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		http.Error(w, "Not Found: key not found or already revoked", http.StatusNotFound)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
}

// ---------------------------------------------------------------------------
// Routing
// ---------------------------------------------------------------------------

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	protect := auth.HTTPMiddleware(s.validator)

	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /v1/.well-known/jwks.json", s.handleJWKS)
	mux.HandleFunc("POST /v1/token", s.handleToken)

	mux.Handle("POST /v1/keys", protect(http.HandlerFunc(s.handleCreateKey)))
	mux.Handle("GET /v1/keys", protect(http.HandlerFunc(s.handleListKeys)))
	mux.Handle("DELETE /v1/keys/{id}", protect(http.HandlerFunc(s.handleRevokeKey)))

	return corsMiddleware(mux)
}

// ---------------------------------------------------------------------------
// Middleware & helpers
// ---------------------------------------------------------------------------

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := envOr("CORS_ALLOW_ORIGIN", "*")
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
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

// parsePGTextArray parses a Postgres text array literal like {foo,bar} into a Go slice.
func parsePGTextArray(s string) []string {
	s = strings.TrimSpace(s)
	if s == "{}" || s == "" {
		return []string{}
	}
	s = strings.TrimPrefix(s, "{")
	s = strings.TrimSuffix(s, "}")
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		p = strings.Trim(p, "\"")
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// quotePGArray wraps each element in double-quotes for a Postgres array literal.
func quotePGArray(ss []string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = "\"" + strings.ReplaceAll(s, "\"", "\\\"") + "\""
	}
	return out
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	cfg := loadConfig()
	slog.Info("starting auth-api", "addr", cfg.HTTPAddr)

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

	signer, err := auth.NewSigner()
	if err != nil {
		slog.Error("auth signer init", "err", err)
		os.Exit(1)
	}
	slog.Info("ephemeral RSA signer ready")

	srv := &server{
		db:        db,
		validator: validator,
		signer:    signer,
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

	slog.Info("auth-api ready", "addr", cfg.HTTPAddr)
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("http serve", "err", err)
		os.Exit(1)
	}
}
