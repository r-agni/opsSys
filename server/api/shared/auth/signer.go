package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"fmt"
	"math/big"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// Signer holds an ephemeral RSA keypair for signing JWTs (e.g. API-key tokens).
// The keypair is generated at startup and lives only in memory.
type Signer struct {
	privateKey *rsa.PrivateKey
	keyID      string
}

// NewSigner generates a fresh 2048-bit RSA keypair and returns a Signer.
func NewSigner() (*Signer, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("auth: generate RSA key: %w", err)
	}
	return &Signer{
		privateKey: key,
		keyID:      uuid.New().String(),
	}, nil
}

// SignToken creates an RS256-signed JWT from the given claims map.
func (s *Signer) SignToken(claims jwt.MapClaims) (string, error) {
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = s.keyID
	return token.SignedString(s.privateKey)
}

// JWKS returns a JSON-serialisable JWK Set containing the public key.
func (s *Signer) JWKS() map[string]interface{} {
	pub := &s.privateKey.PublicKey
	return map[string]interface{}{
		"keys": []map[string]interface{}{
			{
				"kty": "RSA",
				"kid": s.keyID,
				"use": "sig",
				"alg": "RS256",
				"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
				"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
			},
		},
	}
}
