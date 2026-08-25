package database

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Role values for AuthUser.Role. Single role for now; per-DID scoping is
// deferred to a follow-up (see internal/auth/middleware.go).
const (
	RoleOperator = "operator"
)

// AuthUser mirrors a row in auth_users. password_hash is bcrypt.
type AuthUser struct {
	ID           uuid.UUID
	Email        string
	PasswordHash string
	Role         string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// RefreshToken mirrors a row in refresh_tokens. TokenHash is sha256 of
// the raw token (the raw is only ever returned to the client at issue).
type RefreshToken struct {
	ID         uuid.UUID
	UserID     uuid.UUID
	TokenHash  string
	ExpiresAt  time.Time
	RevokedAt  *time.Time
	ReplacedBy *uuid.UUID
	CreatedAt  time.Time
}

// ---------------------------------------------------------------------------
// auth_users
// ---------------------------------------------------------------------------

// CreateAuthUser inserts a new auth_user. Returns the persisted row (with id).
func CreateAuthUser(ctx context.Context, email, passwordHash, role string) (*AuthUser, error) {
	if role == "" {
		role = RoleOperator
	}
	var u AuthUser
	err := Pool.QueryRow(ctx, `
		INSERT INTO auth_users (email, password_hash, role)
		VALUES ($1, $2, $3)
		RETURNING id, email, password_hash, role, created_at, updated_at
	`, email, passwordHash, role).
		Scan(&u.ID, &u.Email, &u.PasswordHash, &u.Role, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("CreateAuthUser: %w", err)
	}
	return &u, nil
}

// GetAuthUserByEmail looks up an operator by email. Returns ErrNotFound
// if no row matches.
func GetAuthUserByEmail(ctx context.Context, email string) (*AuthUser, error) {
	var u AuthUser
	err := Pool.QueryRow(ctx, `
		SELECT id, email, password_hash, role, created_at, updated_at
		FROM auth_users WHERE email = $1
	`, email).
		Scan(&u.ID, &u.Email, &u.PasswordHash, &u.Role, &u.CreatedAt, &u.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("GetAuthUserByEmail: %w", err)
	}
	return &u, nil
}

// GetAuthUserByID looks up an operator by id. Used by middleware to
// confirm the token's subject still exists.
func GetAuthUserByID(ctx context.Context, id uuid.UUID) (*AuthUser, error) {
	var u AuthUser
	err := Pool.QueryRow(ctx, `
		SELECT id, email, password_hash, role, created_at, updated_at
		FROM auth_users WHERE id = $1
	`, id).
		Scan(&u.ID, &u.Email, &u.PasswordHash, &u.Role, &u.CreatedAt, &u.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("GetAuthUserByID: %w", err)
	}
	return &u, nil
}

// CountAuthUsers returns the total number of operator rows. Used by the
// startup bootstrap to decide whether to seed.
func CountAuthUsers(ctx context.Context) (int, error) {
	var n int
	if err := Pool.QueryRow(ctx, `SELECT COUNT(*) FROM auth_users`).Scan(&n); err != nil {
		return 0, fmt.Errorf("CountAuthUsers: %w", err)
	}
	return n, nil
}

// ---------------------------------------------------------------------------
// refresh_tokens
// ---------------------------------------------------------------------------

// InsertRefreshToken stores a hashed refresh token.
func InsertRefreshToken(ctx context.Context, userID uuid.UUID, tokenHash string, expiresAt time.Time) (*RefreshToken, error) {
	var r RefreshToken
	err := Pool.QueryRow(ctx, `
		INSERT INTO refresh_tokens (user_id, token_hash, expires_at)
		VALUES ($1, $2, $3)
		RETURNING id, user_id, token_hash, expires_at, revoked_at, replaced_by, created_at
	`, userID, tokenHash, expiresAt).
		Scan(&r.ID, &r.UserID, &r.TokenHash, &r.ExpiresAt, &r.RevokedAt, &r.ReplacedBy, &r.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("InsertRefreshToken: %w", err)
	}
	return &r, nil
}

// GetRefreshTokenByHash returns the row for a token hash regardless of
// revocation/expiry — callers (rotation flow) need to see revoked rows
// to detect reuse-after-revocation.
func GetRefreshTokenByHash(ctx context.Context, tokenHash string) (*RefreshToken, error) {
	var r RefreshToken
	err := Pool.QueryRow(ctx, `
		SELECT id, user_id, token_hash, expires_at, revoked_at, replaced_by, created_at
		FROM refresh_tokens WHERE token_hash = $1
	`, tokenHash).
		Scan(&r.ID, &r.UserID, &r.TokenHash, &r.ExpiresAt, &r.RevokedAt, &r.ReplacedBy, &r.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("GetRefreshTokenByHash: %w", err)
	}
	return &r, nil
}

// RevokeRefreshToken marks a refresh token revoked. replacedBy is
// optional — set when rotation issued a new token.
func RevokeRefreshToken(ctx context.Context, id uuid.UUID, replacedBy *uuid.UUID) error {
	_, err := Pool.Exec(ctx, `
		UPDATE refresh_tokens
		SET revoked_at = NOW(), replaced_by = $2
		WHERE id = $1 AND revoked_at IS NULL
	`, id, replacedBy)
	if err != nil {
		return fmt.Errorf("RevokeRefreshToken: %w", err)
	}
	return nil
}

// RevokeAllRefreshTokensForUser marks every active refresh token for a
// user as revoked. Used on logout-all and on detected token theft.
func RevokeAllRefreshTokensForUser(ctx context.Context, userID uuid.UUID) error {
	_, err := Pool.Exec(ctx, `
		UPDATE refresh_tokens
		SET revoked_at = NOW()
		WHERE user_id = $1 AND revoked_at IS NULL
	`, userID)
	if err != nil {
		return fmt.Errorf("RevokeAllRefreshTokensForUser: %w", err)
	}
	return nil
}
