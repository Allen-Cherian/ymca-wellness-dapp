// Package auth provides bearer-token authentication: RS256 JWT access
// tokens, hashed refresh tokens with rotation, bcrypt password hashing,
// and Gin middleware. Access tokens are stateless; refresh tokens are
// persisted (hashed) so they can be revoked.
package auth

import (
	"crypto/rsa"
	"errors"
	"fmt"
	"os"

	"github.com/golang-jwt/jwt/v5"
)

// Keys holds the RS256 keypair used to sign and verify JWTs.
type Keys struct {
	Private *rsa.PrivateKey
	Public  *rsa.PublicKey
}

// LoadKeys reads PEM-encoded RSA private and public keys from disk.
// Both files are required — this fails fast at startup so a misconfigured
// deployment cannot accidentally accept unsigned tokens.
func LoadKeys(privatePath, publicPath string) (*Keys, error) {
	if privatePath == "" || publicPath == "" {
		return nil, errors.New("auth: JWT key paths are required")
	}

	privPEM, err := os.ReadFile(privatePath)
	if err != nil {
		return nil, fmt.Errorf("auth: read private key %s: %w", privatePath, err)
	}
	priv, err := jwt.ParseRSAPrivateKeyFromPEM(privPEM)
	if err != nil {
		return nil, fmt.Errorf("auth: parse private key: %w", err)
	}

	pubPEM, err := os.ReadFile(publicPath)
	if err != nil {
		return nil, fmt.Errorf("auth: read public key %s: %w", publicPath, err)
	}
	pub, err := jwt.ParseRSAPublicKeyFromPEM(pubPEM)
	if err != nil {
		return nil, fmt.Errorf("auth: parse public key: %w", err)
	}

	return &Keys{Private: priv, Public: pub}, nil
}
