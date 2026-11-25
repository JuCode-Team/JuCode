package middleware

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/biz"
)

// Gin context keys for the organization context (set by OrgContext).
const (
	CtxOrgID   = "jucode.org_id"
	CtxOrgRole = "jucode.org_role"
)

// HeaderOrganizationID selects the account context for /v1/me/* requests.
// Absent header → personal context; present → the caller must be a member
// of that organization or the request is rejected outright.
const HeaderOrganizationID = "X-Organization-Id"

// OrgContext must run AFTER JWT(). It resolves the X-Organization-Id
// header into a verified (orgID, role) pair on the gin context. Rejecting
// non-members here (rather than per-handler) is what lets downstream
// handlers trust OrgContextFrom without re-checking membership.
func OrgContext(orgs *biz.OrganizationSvc) gin.HandlerFunc {
	return func(c *gin.Context) {
		raw := strings.TrimSpace(c.GetHeader(HeaderOrganizationID))
		if raw == "" {
			c.Next()
			return
		}
		orgID, err := uuid.Parse(raw)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "invalid " + HeaderOrganizationID, "code": "org_context_invalid"})
			return
		}
		uid, ok := UserIDFrom(c)
		if !ok {
			respondUnauthorized(c, "missing identity")
			return
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), 500*time.Millisecond)
		m, err := orgs.Member(ctx, orgID, uid)
		cancel()
		if err != nil {
			if errors.Is(err, biz.ErrOrganizationForbidden) {
				c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "not a member of this organization", "code": "org_context_invalid"})
				return
			}
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "organization check failed"})
			return
		}
		c.Set(CtxOrgID, orgID)
		c.Set(CtxOrgRole, string(m.Role))
		c.Next()
	}
}

// OrgContextFrom returns the verified organization context, or ok=false
// when the request runs in personal context (no header, or a route group
// without the OrgContext middleware — e.g. the OAuth device read routes).
func OrgContextFrom(c *gin.Context) (orgID uuid.UUID, role string, ok bool) {
	v, exists := c.Get(CtxOrgID)
	if !exists {
		return uuid.Nil, "", false
	}
	id, idOK := v.(uuid.UUID)
	if !idOK {
		return uuid.Nil, "", false
	}
	r, _ := c.Get(CtxOrgRole)
	roleStr, _ := r.(string)
	return id, roleStr, true
}

// IsOrgManager reports whether an organization role may see and manage
// org-wide data (all members' keys/usage/billing).
func IsOrgManager(role string) bool {
	return role == "owner" || role == "admin"
}
