package server

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"ymca-wellness-dapp/internal/auth"
	"ymca-wellness-dapp/internal/database"
)

// handleLogin authenticates an operator and issues an access+refresh pair.
// Bcrypt comparison is constant-time; we use the same 401 message for
// "no such email" and "bad password" so an attacker can't enumerate
// accounts.
func (s *Server) handleLogin(c *gin.Context) {
	var req LoginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, errResponse{Error: "Validation failed", Message: err.Error()})
		return
	}
	email := strings.TrimSpace(req.Email)
	if email == "" || req.Password == "" {
		c.JSON(http.StatusBadRequest, errResponse{Error: "email and password are required"})
		return
	}

	user, err := database.GetAuthUserByEmail(c.Request.Context(), email)
	if err != nil {
		if errors.Is(err, database.ErrNotFound) {
			c.JSON(http.StatusUnauthorized, errResponse{Error: "invalid credentials"})
			return
		}
		c.JSON(http.StatusInternalServerError, errResponse{Error: "lookup failed", Message: err.Error()})
		return
	}
	if err := auth.CheckPassword(user.PasswordHash, req.Password); err != nil {
		if errors.Is(err, auth.ErrPasswordMismatch) {
			c.JSON(http.StatusUnauthorized, errResponse{Error: "invalid credentials"})
			return
		}
		c.JSON(http.StatusInternalServerError, errResponse{Error: "password check failed", Message: err.Error()})
		return
	}

	access, _, err := auth.IssueAccess(s.Keys, user.ID, user.Role, s.Cfg.Env.AccessTokenTTL)
	if err != nil {
		c.JSON(http.StatusInternalServerError, errResponse{Error: "token issue failed", Message: err.Error()})
		return
	}
	refresh, _, err := auth.IssueRefresh(c.Request.Context(), user.ID, s.Cfg.Env.RefreshTokenTTL)
	if err != nil {
		c.JSON(http.StatusInternalServerError, errResponse{Error: "refresh issue failed", Message: err.Error()})
		return
	}
	c.JSON(http.StatusOK, TokenPairResponse{
		AccessToken:  access,
		RefreshToken: refresh,
		ExpiresIn:    int(s.Cfg.Env.AccessTokenTTL.Seconds()),
		TokenType:    "Bearer",
	})
}

// handleRefresh rotates a refresh token, returning a new pair. If the
// presented refresh was already revoked, the entire user's token family
// is revoked (theft signal) and we return 401.
func (s *Server) handleRefresh(c *gin.Context) {
	var req RefreshRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, errResponse{Error: "Validation failed", Message: err.Error()})
		return
	}
	if req.RefreshToken == "" {
		c.JSON(http.StatusBadRequest, errResponse{Error: "refresh_token is required"})
		return
	}

	newRefresh, _, userID, err := auth.RotateRefresh(c.Request.Context(), req.RefreshToken, s.Cfg.Env.RefreshTokenTTL)
	if err != nil {
		if errors.Is(err, auth.ErrRefreshReused) && userID != uuid.Nil {
			// Theft signal: nuke the user's whole token family.
			_ = database.RevokeAllRefreshTokensForUser(c.Request.Context(), userID)
		}
		if errors.Is(err, auth.ErrRefreshInvalid) ||
			errors.Is(err, auth.ErrRefreshExpired) ||
			errors.Is(err, auth.ErrRefreshReused) ||
			errors.Is(err, auth.ErrRefreshRevoked) {
			c.JSON(http.StatusUnauthorized, errResponse{Error: "invalid refresh token"})
			return
		}
		c.JSON(http.StatusInternalServerError, errResponse{Error: "refresh failed", Message: err.Error()})
		return
	}

	user, err := database.GetAuthUserByID(c.Request.Context(), userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, errResponse{Error: "user lookup failed", Message: err.Error()})
		return
	}
	access, _, err := auth.IssueAccess(s.Keys, user.ID, user.Role, s.Cfg.Env.AccessTokenTTL)
	if err != nil {
		c.JSON(http.StatusInternalServerError, errResponse{Error: "token issue failed", Message: err.Error()})
		return
	}
	c.JSON(http.StatusOK, TokenPairResponse{
		AccessToken:  access,
		RefreshToken: newRefresh,
		ExpiresIn:    int(s.Cfg.Env.AccessTokenTTL.Seconds()),
		TokenType:    "Bearer",
	})
}

// handleLogout revokes either the refresh token in the body or, with
// all=true, every active refresh for the authenticated user.
func (s *Server) handleLogout(c *gin.Context) {
	var req LogoutRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, errResponse{Error: "Validation failed", Message: err.Error()})
		return
	}

	if req.All {
		userID, ok := auth.UserID(c)
		if !ok {
			c.JSON(http.StatusUnauthorized, errResponse{Error: "unauthorized"})
			return
		}
		if err := database.RevokeAllRefreshTokensForUser(c.Request.Context(), userID); err != nil {
			c.JSON(http.StatusInternalServerError, errResponse{Error: "logout-all failed", Message: err.Error()})
			return
		}
		c.JSON(http.StatusOK, okResponse{Status: true, Message: "all sessions revoked"})
		return
	}

	if req.RefreshToken == "" {
		c.JSON(http.StatusBadRequest, errResponse{Error: "refresh_token is required (or set all=true)"})
		return
	}
	if err := auth.RevokeRefresh(c.Request.Context(), req.RefreshToken); err != nil {
		c.JSON(http.StatusInternalServerError, errResponse{Error: "logout failed", Message: err.Error()})
		return
	}
	c.JSON(http.StatusOK, okResponse{Status: true, Message: "logged out"})
}

// handleMe returns the authenticated user's profile.
func (s *Server) handleMe(c *gin.Context) {
	userID, ok := auth.UserID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, errResponse{Error: "unauthorized"})
		return
	}
	user, err := database.GetAuthUserByID(c.Request.Context(), userID)
	if err != nil {
		if errors.Is(err, database.ErrNotFound) {
			c.JSON(http.StatusUnauthorized, errResponse{Error: "user no longer exists"})
			return
		}
		c.JSON(http.StatusInternalServerError, errResponse{Error: "lookup failed", Message: err.Error()})
		return
	}
	c.JSON(http.StatusOK, okResponse{Status: true, Data: gin.H{
		"id":         user.ID,
		"email":      user.Email,
		"role":       user.Role,
		"created_at": user.CreatedAt,
	}})
}

// CreateUserRequest is the body of POST /api/auth/users.
type CreateUserRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// handleCreateUser provisions an additional operator account. Sits behind
// RequireAuth, so an existing operator is the trust boundary — the
// env-seeded bootstrap user is the root of that chain.
//
// Note: with a single role, any account created here has the same
// privileges as its creator, including the ability to create further
// accounts. Scoping is deferred alongside the per-admin work in
// internal/auth/middleware.go.
func (s *Server) handleCreateUser(c *gin.Context) {
	var req CreateUserRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, errResponse{Error: "Validation failed", Message: err.Error()})
		return
	}

	email := strings.TrimSpace(req.Email)
	if email == "" || !strings.Contains(email, "@") {
		c.JSON(http.StatusBadRequest, errResponse{Error: "a valid email is required"})
		return
	}

	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		if errors.Is(err, auth.ErrPasswordTooShort) {
			c.JSON(http.StatusBadRequest, errResponse{Error: err.Error()})
			return
		}
		c.JSON(http.StatusInternalServerError, errResponse{Error: "hash password", Message: err.Error()})
		return
	}

	user, err := database.CreateAuthUser(c.Request.Context(), email, hash, database.RoleOperator)
	if err != nil {
		// email is CITEXT UNIQUE — a duplicate is a client error, not a 500.
		if isUniqueViolation(err) {
			c.JSON(http.StatusConflict, errResponse{Error: "email already registered"})
			return
		}
		c.JSON(http.StatusInternalServerError, errResponse{Error: "create user", Message: err.Error()})
		return
	}

	c.JSON(http.StatusCreated, okResponse{Status: true, Data: gin.H{
		"id":         user.ID,
		"email":      user.Email,
		"role":       user.Role,
		"created_at": user.CreatedAt,
	}})
}

// isUniqueViolation reports whether err is a Postgres 23505 unique
// constraint violation, however deeply it is wrapped.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
