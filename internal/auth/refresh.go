package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"ymca-wellness-dapp/internal/database"
)

// RefreshTokenBytes is the entropy of each raw refresh token before
// base64 encoding. 32 bytes = 256 bits, plenty.
const RefreshTokenBytes = 32

// Sentinel errors so handlers can map to 401 vs 500 distinctly.
var (
	ErrRefreshInvalid = errors.New("auth: refresh token invalid")
	ErrRefreshExpired = errors.New("auth: refresh token expired")
	ErrRefreshRevoked = errors.New("auth: refresh token revoked")
	// ErrRefreshReused signals that a previously-revoked refresh token
	// was presented — a strong theft indicator. The caller should revoke
	// the user's whole token family.
	ErrRefreshReused = errors.New("auth: refresh token reused after revocation")
)

// IssueRefresh generates a raw token, stores its sha256 hash, and
// returns the raw token to the caller (the only time the raw value is
// visible).
func IssueRefresh(ctx context.Context, userID uuid.UUID, ttl time.Duration) (raw string, expiresAt time.Time, err error) {
	buf := make([]byte, RefreshTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", time.Time{}, fmt.Errorf("auth: refresh entropy: %w", err)
	}
	raw = base64.RawURLEncoding.EncodeToString(buf)
	expiresAt = time.Now().Add(ttl)

	if _, err := database.InsertRefreshToken(ctx, userID, hashToken(raw), expiresAt); err != nil {
		return "", time.Time{}, err
	}
	return raw, expiresAt, nil
}

// RotateRefresh validates the presented raw token, revokes it, and
// issues a new pair (raw refresh token + new user id). Returns
// ErrRefreshReused if the presented token was already revoked — the
// caller should revoke the user's whole family in that case.
func RotateRefresh(ctx context.Context, raw string, ttl time.Duration) (newRaw string, newExpiresAt time.Time, userID uuid.UUID, err error) {
	if raw == "" {
		return "", time.Time{}, uuid.Nil, ErrRefreshInvalid
	}
	row, gerr := database.GetRefreshTokenByHash(ctx, hashToken(raw))
	if errors.Is(gerr, database.ErrNotFound) {
		return "", time.Time{}, uuid.Nil, ErrRefreshInvalid
	}
	if gerr != nil {
		return "", time.Time{}, uuid.Nil, gerr
	}
	if row.RevokedAt != nil {
		return "", time.Time{}, row.UserID, ErrRefreshReused
	}
	if time.Now().After(row.ExpiresAt) {
		return "", time.Time{}, row.UserID, ErrRefreshExpired
	}

	// Issue the replacement first so we can record replaced_by on the
	// outgoing row. If issue fails, the old token stays valid.
	newRaw, newExpiresAt, ierr := IssueRefresh(ctx, row.UserID, ttl)
	if ierr != nil {
		return "", time.Time{}, row.UserID, ierr
	}
	newRow, gerr := database.GetRefreshTokenByHash(ctx, hashToken(newRaw))
	if gerr != nil {
		return "", time.Time{}, row.UserID, gerr
	}
	if rerr := database.RevokeRefreshToken(ctx, row.ID, &newRow.ID); rerr != nil {
		return "", time.Time{}, row.UserID, rerr
	}
	return newRaw, newExpiresAt, row.UserID, nil
}

// RevokeRefresh marks a raw refresh token revoked. Returns nil if the
// token isn't found — logout should be idempotent.
func RevokeRefresh(ctx context.Context, raw string) error {
	if raw == "" {
		return nil
	}
	row, err := database.GetRefreshTokenByHash(ctx, hashToken(raw))
	if errors.Is(err, database.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if row.RevokedAt != nil {
		return nil
	}
	return database.RevokeRefreshToken(ctx, row.ID, nil)
}

// hashToken returns the lowercase hex sha256 of a raw refresh token.
// Storing only the hash means a DB leak doesn't expose live tokens.
func hashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}
