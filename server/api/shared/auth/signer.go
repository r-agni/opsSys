package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// Signer holds an RSA keypair for signing JWTs (e.g. API-key tokens).
type Signer struct {
	privateKey *rsa.PrivateKey
	keyID      string
}

// NewSigner creates a Signer. If RSA_KEY_PATH is set, the PEM-encoded private
// key is loaded from that path so tokens survive service restarts. Otherwise a
// fresh ephemeral 2048-bit keypair is generated (dev-mode fallback).
func NewSigner() (*Signer, error) {
	if path := os.Getenv("RSA_KEY_PATH"); path != "" {
		return newSignerFromFile(path)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("auth: generate RSA key: %w", err)
	}
	return &Signer{
		privateKey: key,
		keyID:      uuid.New().String(),
	}, nil
}

func newSignerFromFile(path string) (*Signer, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("auth: read RSA key file %s: %w", path, err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("auth: no PEM block found in %s", path)
	}

	var key *rsa.PrivateKey
	switch block.Type {
	case "RSA PRIVATE KEY":
		key, err = x509.ParsePKCS1PrivateKey(block.Bytes)
	case "PRIVATE KEY":
		parsed, parseErr := x509.ParsePKCS8PrivateKey(block.Bytes)
		if parseErr != nil {
			return nil, fmt.Errorf("auth: parse PKCS8 key: %w", parseErr)
		}
		var ok bool
		key, ok = parsed.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("auth: key in %s is not RSA", path)
		}
	default:
		return nil, fmt.Errorf("auth: unsupported PEM type %q in %s", block.Type, path)
	}
	if err != nil {
		return nil, fmt.Errorf("auth: parse RSA key: %w", err)
	}

	kidEnv := os.Getenv("RSA_KEY_ID")
	if kidEnv == "" {
		kidEnv = uuid.New().String()
	}
	return &Signer{privateKey: key, keyID: kidEnv}, nil
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
