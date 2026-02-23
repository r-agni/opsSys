// Package auth provides JWT validation middleware for HTTP and gRPC handlers.
//
// Tokens are issued by Keycloak (OIDC). Each token carries claims:
//   - sub:         operator user ID
//   - org_id:      organization identifier (multi-tenant isolation)
//   - vehicle_set: list of vehicle IDs this operator may access
//   - role:        "viewer" | "operator" | "admin"
//   - exp:         expiry (standard JWT claim)
//
// The JWKS (public key set) is fetched from Keycloak at startup and
// refreshed every 5 minutes. Tokens are validated locally — no network
// call per request (critical for low-latency command path).
package auth

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// ──────────────────────────────────────────────────────────────────────────────
// Claims
// ──────────────────────────────────────────────────────────────────────────────

// Claims represents the JWT payload issued by Keycloak or the apikey-service.
type Claims struct {
	jwt.RegisteredClaims
	OrgID      string   `json:"org_id"`
	VehicleSet []string `json:"vehicle_set"` // vehicle IDs this operator may access
	Role       Role     `json:"role"`
	// Fields populated by apikey-service (empty for Keycloak-issued tokens):
	ProjectID string `json:"project_id,omitempty"`
	DeviceID  string `json:"device_id,omitempty"`
	KeyID     string `json:"key_id,omitempty"`
}

// Role defines the operator's permission level.
type Role string

const (
	RoleViewer   Role = "viewer"   // read-only telemetry
	RoleOperator Role = "operator" // can send commands
	RoleAdmin    Role = "admin"    // full access including fleet management
)

// CanSendCommand returns true if the role allows sending commands to vehicles.
func (r Role) CanSendCommand() bool {
	return r == RoleOperator || r == RoleAdmin
}

// CanAccessVehicle returns true if the claims allow access to the given vehicle ID.
func (c *Claims) CanAccessVehicle(vehicleID string) bool {
	if c.Role == RoleAdmin {
		return true // admins access everything in their org
	}
	for _, id := range c.VehicleSet {
		if id == vehicleID {
			return true
		}
	}
	return false
}

// ──────────────────────────────────────────────────────────────────────────────
// Validator — validates JWT tokens using JWKS from Keycloak
// ──────────────────────────────────────────────────────────────────────────────

// Validator fetches JWKS from one or more providers and validates JWT tokens locally.
// Supports multiple JWKS URLs (e.g. Keycloak + apikey-service) — keys from all
// providers are merged into a single map keyed by kid.
type Validator struct {
	jwksURLs []string
	keys     map[string]*rsa.PublicKey // kid → public key
	mu       sync.RWMutex
}

// NewValidator creates a Validator from a comma-separated list of JWKS URLs.
// A single URL (existing callers) works unchanged.
// e.g. "https://keycloak.internal/.../certs,https://apikey-service:8080/v1/.well-known/jwks.json"
func NewValidator(jwksURLs string) (*Validator, error) {
	urls := strings.Split(jwksURLs, ",")
	v := &Validator{
		jwksURLs: urls,
		keys:     make(map[string]*rsa.PublicKey),
	}
	if err := v.refreshKeys(); err != nil {
		return nil, fmt.Errorf("initial JWKS fetch: %w", err)
	}
	go v.refreshLoop()
	return v, nil
}

func (v *Validator) refreshLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		// Errors here are non-fatal — keep using existing cached keys
		_ = v.refreshKeys()
	}
}

type jwksResponse struct {
	Keys []struct {
		Kid string `json:"kid"`
		N   string `json:"n"`
		E   string `json:"e"`
		Kty string `json:"kty"`
		Use string `json:"use"`
	} `json:"keys"`
}

func (v *Validator) refreshKeys() error {
	newKeys := make(map[string]*rsa.PublicKey)
	var lastErr error
	for _, url := range v.jwksURLs {
		url = strings.TrimSpace(url)
		if url == "" {
			continue
		}
		resp, err := http.Get(url) //nolint:gosec // URL is operator-configured
		if err != nil {
			lastErr = fmt.Errorf("JWKS fetch %s: %w", url, err)
			continue
		}
		var jwks jwksResponse
		if err := json.NewDecoder(resp.Body).Decode(&jwks); err != nil {
			resp.Body.Close()
			lastErr = fmt.Errorf("JWKS decode %s: %w", url, err)
			continue
		}
		resp.Body.Close()
		for _, k := range jwks.Keys {
			if k.Kty != "RSA" || k.Use != "sig" {
				continue
			}
			pub, err := parseRSAPublicKey(k.N, k.E)
			if err != nil {
				continue
			}
			newKeys[k.Kid] = pub
		}
	}
	if len(newKeys) == 0 && lastErr != nil {
		return lastErr
	}
	v.mu.Lock()
	v.keys = newKeys
	v.mu.Unlock()
	return nil
}

// Validate parses and validates a JWT Bearer token string.
// Returns the Claims on success. This is called on every request — must be fast.
func (v *Validator) Validate(tokenStr string) (*Claims, error) {
	claims := &Claims{}
	token, err := jwt.ParseWithClaims(tokenStr, claims, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodRSA); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}
		kid, _ := token.Header["kid"].(string)
		v.mu.RLock()
		key, ok := v.keys[kid]
		v.mu.RUnlock()
		if !ok {
			return nil, fmt.Errorf("unknown key ID: %q", kid)
		}
		return key, nil
	})
	if err != nil {
		return nil, fmt.Errorf("token invalid: %w", err)
	}
	if !token.Valid {
		return nil, fmt.Errorf("token not valid")
	}
	return claims, nil
}

// ──────────────────────────────────────────────────────────────────────────────
// HTTP middleware
// ──────────────────────────────────────────────────────────────────────────────

type contextKey string

const claimsKey contextKey = "claims"

// ClaimsFromContext retrieves validated claims from the request context.
func ClaimsFromContext(ctx context.Context) (*Claims, bool) {
	c, ok := ctx.Value(claimsKey).(*Claims)
	return c, ok
}

// HTTPMiddleware returns an HTTP middleware that validates the Authorization header.
func HTTPMiddleware(v *Validator) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, err := extractBearerToken(r.Header.Get("Authorization"))
			if err != nil {
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
			claims, err := v.Validate(token)
			if err != nil {
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
			ctx := context.WithValue(r.Context(), claimsKey, claims)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// gRPC interceptors
// ──────────────────────────────────────────────────────────────────────────────

// GRPCUnaryInterceptor validates Authorization metadata on every unary gRPC call.
func GRPCUnaryInterceptor(v *Validator) grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req interface{},
		_ *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (interface{}, error) {
		ctx, err := grpcAuth(ctx, v)
		if err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

// GRPCStreamInterceptor validates Authorization metadata on streaming gRPC calls.
// Called once per stream establishment, not per message.
func GRPCStreamInterceptor(v *Validator) grpc.StreamServerInterceptor {
	return func(
		srv interface{},
		stream grpc.ServerStream,
		_ *grpc.StreamServerInfo,
		handler grpc.StreamHandler,
	) error {
		ctx, err := grpcAuth(stream.Context(), v)
		if err != nil {
			return err
		}
		return handler(srv, &wrappedStream{stream, ctx})
	}
}

func grpcAuth(ctx context.Context, v *Validator) (context.Context, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "missing metadata")
	}
	authVals := md.Get("authorization")
	if len(authVals) == 0 {
		return nil, status.Error(codes.Unauthenticated, "missing authorization header")
	}
	tokenStr, err := extractBearerToken(authVals[0])
	if err != nil {
		return nil, status.Error(codes.Unauthenticated, "invalid authorization header")
	}
	claims, err := v.Validate(tokenStr)
	if err != nil {
		return nil, status.Error(codes.Unauthenticated, err.Error())
	}
	return context.WithValue(ctx, claimsKey, claims), nil
}

type wrappedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (w *wrappedStream) Context() context.Context { return w.ctx }

// ──────────────────────────────────────────────────────────────────────────────
// Helpers
// ──────────────────────────────────────────────────────────────────────────────

func extractBearerToken(header string) (string, error) {
	if !strings.HasPrefix(header, "Bearer ") {
		return "", fmt.Errorf("not a Bearer token")
	}
	token := strings.TrimPrefix(header, "Bearer ")
	if token == "" {
		return "", fmt.Errorf("empty token")
	}
	return token, nil
}

// parseRSAPublicKey converts base64url-encoded modulus (n) and exponent (e)
// from a JWKS entry into an *rsa.PublicKey for jwt signature verification.
func parseRSAPublicKey(nStr, eStr string) (*rsa.PublicKey, error) {
	nBytes, err := base64.RawURLEncoding.DecodeString(nStr)
	if err != nil {
		return nil, fmt.Errorf("decode modulus: %w", err)
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(eStr)
	if err != nil {
		return nil, fmt.Errorf("decode exponent: %w", err)
	}

	n := new(big.Int).SetBytes(nBytes)

	var eInt int
	for _, b := range eBytes {
		eInt = eInt<<8 + int(b)
	}

	return &rsa.PublicKey{N: n, E: eInt}, nil
}
