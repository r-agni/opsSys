package auth

import (
	"fmt"
	"strings"

	"github.com/MicahParks/keyfunc/v3"
	"github.com/golang-jwt/jwt/v5"
)

// Validator validates JWTs against one or more JWKS endpoints.
type Validator struct {
	kf jwt.Keyfunc
}

// NewValidator fetches JWKS from one or more comma-separated URLs and returns
// a Validator that can parse and verify JWTs signed by any of those key sets.
func NewValidator(jwksURLs string) (*Validator, error) {
	urls := splitTrimmed(jwksURLs)
	if len(urls) == 0 {
		return nil, fmt.Errorf("auth: no JWKS URLs provided")
	}

	k, err := keyfunc.NewDefault(urls)
	if err != nil {
		return nil, fmt.Errorf("auth: fetch JWKS: %w", err)
	}

	return &Validator{kf: k.Keyfunc}, nil
}

// Validate parses and verifies a raw JWT string and returns the extracted Claims.
func (v *Validator) Validate(tokenStr string) (*Claims, error) {
	token, err := jwt.Parse(tokenStr, v.kf, jwt.WithValidMethods([]string{"RS256", "ES256"}))
	if err != nil {
		return nil, fmt.Errorf("auth: parse token: %w", err)
	}
	if !token.Valid {
		return nil, fmt.Errorf("auth: token invalid")
	}

	mapClaims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return nil, fmt.Errorf("auth: unexpected claims type")
	}

	claims := &Claims{
		Subject: stringClaim(mapClaims, "sub"),
		Email:   stringClaim(mapClaims, "email"),
		OrgID:   stringClaim(mapClaims, "org_id"),
		Role:    Role(stringClaim(mapClaims, "role")),
	}

	if vs, ok := mapClaims["vehicle_set"].([]interface{}); ok {
		for _, item := range vs {
			if s, ok := item.(string); ok {
				claims.VehicleSet = append(claims.VehicleSet, s)
			}
		}
	}

	return claims, nil
}

func stringClaim(m jwt.MapClaims, key string) string {
	v, _ := m[key].(string)
	return v
}

func splitTrimmed(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
