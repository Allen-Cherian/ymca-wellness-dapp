package auth

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// Issuer is the JWT `iss` claim. Constant; not configurable for now.
const Issuer = "ymca-wellness-dapp"

// Claims is the JWT payload for access tokens. JTI is included so
// future server-side revocation (denylist) is feasible without a schema
// change.
type Claims struct {
	Role string `json:"role"`
	jwt.RegisteredClaims
}

// IssueAccess signs an RS256 access token for the given user.
func IssueAccess(k *Keys, userID uuid.UUID, role string, ttl time.Duration) (string, time.Time, error) {
	now := time.Now()
	exp := now.Add(ttl)
	claims := Claims{
		Role: role,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    Issuer,
			Subject:   userID.String(),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(exp),
			ID:        uuid.NewString(),
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	signed, err := tok.SignedString(k.Private)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("auth: sign access token: %w", err)
	}
	return signed, exp, nil
}

// ParseAccess validates signature, issuer, and expiry, returning the
// claims on success.
func ParseAccess(k *Keys, raw string) (*Claims, error) {
	parsed, err := jwt.ParseWithClaims(raw, &Claims{}, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodRSA); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return k.Public, nil
	}, jwt.WithIssuer(Issuer), jwt.WithValidMethods([]string{"RS256"}))
	if err != nil {
		return nil, err
	}
	claims, ok := parsed.Claims.(*Claims)
	if !ok || !parsed.Valid {
		return nil, errors.New("auth: invalid token")
	}
	return claims, nil
}
