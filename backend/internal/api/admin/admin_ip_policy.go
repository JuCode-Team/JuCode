// admin_ip_policy.go: CRUD endpoints for the IP allow/block/trusted-
// proxy list. Mounted alongside the rest of /v1/admin so the standard
// JWT + AdminOnly + AdminAudit chain applies.
package admin

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/data/ent"
	"github.com/jucode/jucode/internal/middleware"
)

type ipPolicyAddBody struct {
	CIDR      string  `json:"cidr"`
	Kind      string  `json:"kind"`
	Note      string  `json:"note"`
	ExpiresAt *string `json:"expires_at,omitempty"` // RFC3339; optional
}

func (h *Handler) listIPPolicies(c *gin.Context) {
	kind := biz.PolicyKind(c.Query("kind"))
	rows, err := h.ipPolicy.List(c.Request.Context(), kind)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"policies": rows})
}

func (h *Handler) createIPPolicy(c *gin.Context) {
	var body ipPolicyAddBody
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	in := biz.AddInput{
		CIDR: body.CIDR,
		Kind: biz.PolicyKind(body.Kind),
		Note: body.Note,
	}
	if body.ExpiresAt != nil && *body.ExpiresAt != "" {
		t, err := time.Parse(time.RFC3339, *body.ExpiresAt)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "expires_at must be RFC3339"})
			return
		}
		in.ExpiresAt = &t
	}
	if uid, ok := middleware.UserIDFrom(c); ok {
		in.CreatedBy = &uid
	}
	entry, err := h.ipPolicy.Add(c.Request.Context(), in)
	if err != nil {
		if err == biz.ErrInvalidCIDR {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, entry)
}

func (h *Handler) deleteIPPolicy(c *gin.Context) {
	id, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	if err := h.ipPolicy.Delete(c.Request.Context(), id); err != nil {
		if ent.IsNotFound(err) {
			c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
