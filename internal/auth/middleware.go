package auth

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// Context keys set by RequireAuth on successful authentication.
const (
	CtxUserID = "auth.user_id"
	CtxRole   = "auth.role"
)

// RequireAuth returns a Gin middleware that:
//   - parses Authorization: Bearer <jwt>
//   - validates signature and expiry against the provided keys
//   - enforces the role allowlist (empty = any authenticated role passes)
//
// On success it sets CtxUserID and CtxRole on the gin.Context for
// downstream handlers.
//
// TODO(scoping): per-admin authorization (does this operator own the
// :admin_did being acted on?) is intentionally not enforced here. When
// the auth_user_admins join table lands, add the check below the role
// allowlist — handler code shouldn't need to change.
func RequireAuth(keys *Keys, allowedRoles ...string) gin.HandlerFunc {
	allow := make(map[string]struct{}, len(allowedRoles))
	for _, r := range allowedRoles {
		allow[r] = struct{}{}
	}

	return func(c *gin.Context) {
		raw := bearerToken(c.GetHeader("Authorization"))
		if raw == "" {
			abort(c, http.StatusUnauthorized, "missing or malformed Authorization header")
			return
		}
		claims, err := ParseAccess(keys, raw)
		if err != nil {
			abort(c, http.StatusUnauthorized, "invalid or expired token")
			return
		}
		userID, err := uuid.Parse(claims.Subject)
		if err != nil {
			abort(c, http.StatusUnauthorized, "invalid token subject")
			return
		}
		if len(allow) > 0 {
			if _, ok := allow[claims.Role]; !ok {
				abort(c, http.StatusForbidden, "insufficient role")
				return
			}
		}
		c.Set(CtxUserID, userID)
		c.Set(CtxRole, claims.Role)
		c.Next()
	}
}

// UserID returns the authenticated user id set by RequireAuth, or
// uuid.Nil and false if the request was not authenticated.
func UserID(c *gin.Context) (uuid.UUID, bool) {
	v, ok := c.Get(CtxUserID)
	if !ok {
		return uuid.Nil, false
	}
	id, ok := v.(uuid.UUID)
	return id, ok
}

func bearerToken(header string) string {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return ""
	}
	return strings.TrimSpace(header[len(prefix):])
}

func abort(c *gin.Context, code int, msg string) {
	c.AbortWithStatusJSON(code, gin.H{
		"status":  false,
		"error":   "unauthorized",
		"message": msg,
	})
}
