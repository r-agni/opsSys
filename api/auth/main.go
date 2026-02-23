// apikey-service: Issues and validates SystemScale API keys.
//
// API keys (format: ssk_live_<32-byte base58>) are long-lived secrets that
// companies use to authenticate. This service exchanges them for short-lived
// JWTs (1h for device tokens, 24h for operator tokens) that all other services
// accept via their existing shared/auth JWT middleware.
//
// This service exposes its own JWKS endpoint so other services can validate
// the JWTs it issues. Configure other services with:
//   JWKS_URLS=<keycloak-url>,http://apikey-service:8081/v1/.well-known/jwks.json
//
// Endpoints:
//   POST /v1/token                      exchange API key for JWT
//   POST /v1/keys                       create API key (admin JWT required)
//   GET  /v1/keys                       list API keys for org (admin JWT required)
//   DELETE /v1/keys/{id}               revoke API key (admin JWT required)
//   GET  /v1/.well-known/jwks.json      JWKS for JWT validation
//   GET  /healthz
package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	_ "github.com/lib/pq"
	"github.com/systemscale/services/shared/auth"
)

// ──────────────────────────────────────────────────────────────────────────────
// Key generation and loading
// ──────────────────────────────────────────────────────────────────────────────

const signingKeyID = "apikey-v1"

// loadOrGenerateKey loads the RSA signing key from SIGNING_KEY_PATH env var.
// If the path is empty or the file doesn't exist, generates a new 2048-bit key.
// In production, always provide a stable key so tokens survive restarts.
func loadOrGenerateKey() (*rsa.PrivateKey, error) {
	path := os.Getenv("SIGNING_KEY_PATH")
	if path != "" {
		data, err := os.ReadFile(path)
		if err == nil {
			block, _ := pem.Decode(data)
			if block != nil {
				key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
				if err == nil {
					slog.Info("Loaded RSA signing key", "path", path)
					return key, nil
				}
			}
		}
	}
	slog.Warn("Generating ephemeral RSA signing key — set SIGNING_KEY_PATH for production")
	return rsa.GenerateKey(rand.Reader, 2048)
}

// ──────────────────────────────────────────────────────────────────────────────
// API key generation
// ──────────────────────────────────────────────────────────────────────────────

const base58Alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

// generateAPIKey creates a new API key in the format ssk_live_<base58(32 bytes)>.
func generateAPIKey() (rawKey, prefix string, err error) {
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return
	}
	encoded := base58Encode(b)
	rawKey = "ssk_live_" + encoded
	if len(rawKey) > 12 {
		prefix = rawKey[:12]
	} else {
		prefix = rawKey
	}
	return
}

func base58Encode(input []byte) string {
	n := new(big.Int).SetBytes(input)
	base := big.NewInt(58)
	zero := big.NewInt(0)
	mod := new(big.Int)
	var result []byte
	for n.Cmp(zero) > 0 {
		n.DivMod(n, base, mod)
		result = append(result, base58Alphabet[mod.Int64()])
	}
	// Reverse
	for i, j := 0, len(result)-1; i < j; i, j = i+1, j-1 {
		result[i], result[j] = result[j], result[i]
	}
	return string(result)
}

// hashKey returns SHA-256 of the raw key, hex-encoded.
func hashKey(rawKey string) string {
	h := sha256.Sum256([]byte(rawKey))
	return fmt.Sprintf("%x", h)
}

// ──────────────────────────────────────────────────────────────────────────────
// JWT issuance
// ──────────────────────────────────────────────────────────────────────────────

type apiKeyClaims struct {
	jwt.RegisteredClaims
	OrgID      string   `json:"org_id"`
	VehicleSet []string `json:"vehicle_set"`
	Role       string   `json:"role"`
	ProjectID  string   `json:"project_id,omitempty"`
	DeviceID   string   `json:"device_id,omitempty"`
	KeyID      string   `json:"key_id"`
	Scopes     []string `json:"scopes,omitempty"`
}

func issueToken(privKey *rsa.PrivateKey, orgID, keyID, role, projectID, deviceID string,
	vehicleSet, scopes []string, ttl time.Duration) (string, error) {

	now := time.Now()
	claims := apiKeyClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
			Issuer:    "systemscale-apikey-service",
		},
		OrgID:      orgID,
		VehicleSet: vehicleSet,
		Role:       role,
		ProjectID:  projectID,
		DeviceID:   deviceID,
		KeyID:      keyID,
		Scopes:     scopes,
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = signingKeyID
	return token.SignedString(privKey)
}

// ──────────────────────────────────────────────────────────────────────────────
// JWKS endpoint
// ──────────────────────────────────────────────────────────────────────────────

type jwkKey struct {
	Kid string `json:"kid"`
	Kty string `json:"kty"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	N   string `json:"n"`
	E   string `json:"e"`
}

type jwksResponse struct {
	Keys []jwkKey `json:"keys"`
}

func buildJWKS(privKey *rsa.PrivateKey) jwksResponse {
	pub := &privKey.PublicKey
	nBytes := pub.N.Bytes()
	eBytes := big.NewInt(int64(pub.E)).Bytes()
	return jwksResponse{
		Keys: []jwkKey{{
			Kid: signingKeyID,
			Kty: "RSA",
			Use: "sig",
			Alg: "RS256",
			N:   base64.RawURLEncoding.EncodeToString(nBytes),
			E:   base64.RawURLEncoding.EncodeToString(eBytes),
		}},
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Service
// ──────────────────────────────────────────────────────────────────────────────

type apikeyService struct {
	db      *sql.DB
	privKey *rsa.PrivateKey
	jwks    jwksResponse
	v       *auth.Validator // validates admin JWTs from Keycloak (for key management)
}

// handleToken POST /v1/token — exchange API key for JWT
func (s *apikeyService) handleToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		APIKey    string `json:"api_key"`
		ProjectID string `json:"project_id,omitempty"`
		DeviceID  string `json:"device_id,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Bad Request: invalid JSON", http.StatusBadRequest)
		return
	}
	if !strings.HasPrefix(req.APIKey, "ssk_") {
		http.Error(w, "Bad Request: invalid api_key format", http.StatusBadRequest)
		return
	}

	hash := hashKey(req.APIKey)

	var (
		keyID      string
		orgID      string
		name       string
		scopesJSON string
		expiresAt  sql.NullTime
		revokedAt  sql.NullTime
	)
	err := s.db.QueryRowContext(r.Context(),
		`SELECT id, org_id, name, scopes::text, expires_at, revoked_at
		 FROM api_keys WHERE key_hash = $1`,
		hash,
	).Scan(&keyID, &orgID, &name, &scopesJSON, &expiresAt, &revokedAt)
	if err == sql.ErrNoRows {
		http.Error(w, "Unauthorized: invalid api_key", http.StatusUnauthorized)
		return
	}
	if err != nil {
		slog.Error("api_keys DB query", "err", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	if revokedAt.Valid {
		http.Error(w, "Unauthorized: api_key has been revoked", http.StatusUnauthorized)
		return
	}
	if expiresAt.Valid && time.Now().After(expiresAt.Time) {
		http.Error(w, "Unauthorized: api_key has expired", http.StatusUnauthorized)
		return
	}

	// Update last_used_at (best-effort, non-blocking)
	go func() {
		_, _ = s.db.Exec(`UPDATE api_keys SET last_used_at = $1 WHERE id = $2`,
			time.Now().UTC(), keyID)
	}()

	// Parse scopes to determine role and TTL
	var scopes []string
	_ = json.Unmarshal([]byte(scopesJSON), &scopes)

	role := "operator"
	ttl := time.Hour
	for _, sc := range scopes {
		if sc == "admin" {
			role = "admin"
		}
		if sc == "viewer" && role != "admin" && role != "operator" {
			role = "viewer"
		}
	}

	// Device tokens get a 1h TTL; operator tokens get 24h
	vehicleSet := []string{}
	if req.DeviceID != "" {
		vehicleSet = []string{req.DeviceID}
		ttl = time.Hour
	} else {
		ttl = 24 * time.Hour
	}

	tokenStr, err := issueToken(s.privKey, orgID, keyID, role, req.ProjectID, req.DeviceID, vehicleSet, scopes, ttl)
	if err != nil {
		slog.Error("JWT signing", "err", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"access_token": tokenStr,
		"token_type":   "Bearer",
		"expires_in":   int(ttl.Seconds()),
	})
}

// handleCreateKey POST /v1/keys — create a new API key (admin JWT required)
func (s *apikeyService) handleCreateKey(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	claims, ok := s.authAdmin(w, r)
	if !ok {
		return
	}

	var req struct {
		Name      string   `json:"name"`
		Scopes    []string `json:"scopes"`
		ExpiresAt string   `json:"expires_at,omitempty"` // RFC3339 or empty
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
		http.Error(w, "Bad Request: name required", http.StatusBadRequest)
		return
	}

	rawKey, prefix, err := generateAPIKey()
	if err != nil {
		slog.Error("key generation", "err", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	keyID := uuid.New().String()
	hash := hashKey(rawKey)
	now := time.Now().UTC()

	var expiresAt *time.Time
	if req.ExpiresAt != "" {
		t, err := time.Parse(time.RFC3339, req.ExpiresAt)
		if err != nil {
			http.Error(w, "Bad Request: invalid expires_at", http.StatusBadRequest)
			return
		}
		expiresAt = &t
	}

	scopesJSON, _ := json.Marshal(req.Scopes)

	_, err = s.db.ExecContext(r.Context(),
		`INSERT INTO api_keys (id, key_hash, key_prefix, org_id, name, scopes, created_at, expires_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		keyID, hash, prefix, claims.OrgID, req.Name, string(scopesJSON), now, expiresAt,
	)
	if err != nil {
		slog.Error("api_keys insert", "err", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"id":         keyID,
		"api_key":    rawKey, // only returned once
		"key_prefix": prefix,
		"org_id":     claims.OrgID,
		"name":       req.Name,
		"scopes":     req.Scopes,
		"created_at": now,
		"expires_at": expiresAt,
	})
}

// handleListKeys GET /v1/keys
func (s *apikeyService) handleListKeys(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	claims, ok := s.authAdmin(w, r)
	if !ok {
		return
	}

	rows, err := s.db.QueryContext(r.Context(),
		`SELECT id, key_prefix, org_id, name, scopes::text, created_at, expires_at, revoked_at, last_used_at
		 FROM api_keys WHERE org_id = $1 ORDER BY created_at DESC`,
		claims.OrgID,
	)
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type keyRow struct {
		ID          string     `json:"id"`
		KeyPrefix   string     `json:"key_prefix"`
		OrgID       string     `json:"org_id"`
		Name        string     `json:"name"`
		Scopes      []string   `json:"scopes"`
		CreatedAt   time.Time  `json:"created_at"`
		ExpiresAt   *time.Time `json:"expires_at,omitempty"`
		RevokedAt   *time.Time `json:"revoked_at,omitempty"`
		LastUsedAt  *time.Time `json:"last_used_at,omitempty"`
	}
	var keys []keyRow
	for rows.Next() {
		var k keyRow
		var scopesJSON string
		var expiresAt, revokedAt, lastUsedAt sql.NullTime
		if err := rows.Scan(&k.ID, &k.KeyPrefix, &k.OrgID, &k.Name, &scopesJSON,
			&k.CreatedAt, &expiresAt, &revokedAt, &lastUsedAt); err != nil {
			continue
		}
		_ = json.Unmarshal([]byte(scopesJSON), &k.Scopes)
		if expiresAt.Valid { k.ExpiresAt = &expiresAt.Time }
		if revokedAt.Valid { k.RevokedAt = &revokedAt.Time }
		if lastUsedAt.Valid { k.LastUsedAt = &lastUsedAt.Time }
		keys = append(keys, k)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"keys": keys, "count": len(keys)})
}

// handleRevokeKey DELETE /v1/keys/{id}
func (s *apikeyService) handleRevokeKey(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	claims, ok := s.authAdmin(w, r)
	if !ok {
		return
	}

	keyID := strings.TrimPrefix(r.URL.Path, "/v1/keys/")
	if keyID == "" {
		http.Error(w, "Bad Request: missing key ID", http.StatusBadRequest)
		return
	}

	result, err := s.db.ExecContext(r.Context(),
		`UPDATE api_keys SET revoked_at = $1 WHERE id = $2 AND org_id = $3 AND revoked_at IS NULL`,
		time.Now().UTC(), keyID, claims.OrgID,
	)
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
}

// authAdmin validates the Bearer JWT and checks for admin role.
func (s *apikeyService) authAdmin(w http.ResponseWriter, r *http.Request) (*auth.Claims, bool) {
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, "Bearer ") {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return nil, false
	}
	claims, err := s.v.Validate(strings.TrimPrefix(header, "Bearer "))
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return nil, false
	}
	if claims.Role != auth.RoleAdmin {
		http.Error(w, "Forbidden: admin role required", http.StatusForbidden)
		return nil, false
	}
	return claims, true
}

// ──────────────────────────────────────────────────────────────────────────────
// Schema
// ──────────────────────────────────────────────────────────────────────────────

func applySchema(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS api_keys (
			id           VARCHAR(36)  PRIMARY KEY,
			key_hash     VARCHAR(64)  NOT NULL UNIQUE,
			key_prefix   VARCHAR(16)  NOT NULL,
			org_id       VARCHAR(36)  NOT NULL,
			name         VARCHAR(255) NOT NULL,
			scopes       JSONB        NOT NULL DEFAULT '[]',
			created_at   TIMESTAMPTZ  NOT NULL,
			expires_at   TIMESTAMPTZ,
			revoked_at   TIMESTAMPTZ,
			last_used_at TIMESTAMPTZ,
			INDEX idx_api_keys_org  (org_id),
			INDEX idx_api_keys_hash (key_hash)
		);
	`)
	return err
}

// ──────────────────────────────────────────────────────────────────────────────
// Router and main
// ──────────────────────────────────────────────────────────────────────────────

func (s *apikeyService) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/v1/token",                    s.handleToken)
	mux.HandleFunc("/v1/keys",                     s.handleKeys)
	mux.HandleFunc("/v1/keys/",                    s.handleRevokeKey)
	mux.HandleFunc("/v1/.well-known/jwks.json",    s.handleJWKS)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	})

	return mux
}

// handleKeys routes GET and POST on /v1/keys
func (s *apikeyService) handleKeys(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleListKeys(w, r)
	case http.MethodPost:
		s.handleCreateKey(w, r)
	default:
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
	}
}

func (s *apikeyService) handleJWKS(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.jwks)
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	httpAddr := envOr("HTTP_ADDR", ":8081")
	jwksURLs := envOr("JWKS_URLS", "https://keycloak.internal/realms/systemscale/protocol/openid-connect/certs")
	dbURL    := envOr("DATABASE_URL", "postgresql://root@localhost:26257/systemscale?sslmode=disable")

	slog.Info("Starting apikey-service", "addr", httpAddr)

	privKey, err := loadOrGenerateKey()
	if err != nil {
		slog.Error("RSA key error", "err", err)
		os.Exit(1)
	}

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
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)

	if err := applySchema(db); err != nil {
		slog.Error("schema migration failed", "err", err)
		os.Exit(1)
	}

	svc := &apikeyService{
		db:      db,
		privKey: privKey,
		jwks:    buildJWKS(privKey),
		v:       validator,
	}

	server := &http.Server{
		Addr:         httpAddr,
		Handler:      corsMiddleware(svc.routes()),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	go func() {
		slog.Info("apikey-service ready", "addr", httpAddr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("HTTP serve error", "err", err)
		}
	}()

	<-ctx.Done()
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutCancel()
	server.Shutdown(shutCtx)
	slog.Info("apikey-service shutdown complete")
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
