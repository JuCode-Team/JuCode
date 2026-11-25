package user

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/data/ent"
	"github.com/jucode/jucode/internal/middleware"
)

func toAccessTokenView(t *ent.PersonalAccessToken) gin.H {
	return gin.H{
		"id": t.ID.String(), "name": t.Name, "prefix": t.Prefix,
		"created_at": t.CreatedAt, "expires_at": t.ExpiresAt, "last_used_at": t.LastUsedAt,
	}
}

func (h *Handler) listAccessTokens(c *gin.Context) {
	uid, ok := middleware.UserIDFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "no user"})
		return
	}
	tokens, err := h.accessTokens.List(c.Request.Context(), uid)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	out := make([]gin.H, 0, len(tokens))
	for _, t := range tokens {
		out = append(out, toAccessTokenView(t))
	}
	c.JSON(http.StatusOK, gin.H{"tokens": out})
}

func (h *Handler) createAccessToken(c *gin.Context) {
	uid, ok := middleware.UserIDFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "no user"})
		return
	}
	var req struct {
		Name string `json:"name"`
		// 0 or omitted = never expires.
		ExpiresInDays int `json:"expires_in_days"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.Name) == "" || len([]rune(req.Name)) > 64 || req.ExpiresInDays < 0 || req.ExpiresInDays > 3650 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请填写 64 字以内的名称，有效期为 0（不过期）到 3650 天"})
		return
	}
	var expiresAt *time.Time
	if req.ExpiresInDays > 0 {
		t := time.Now().AddDate(0, 0, req.ExpiresInDays)
		expiresAt = &t
	}
	t, secret, err := h.accessTokens.Create(c.Request.Context(), uid, strings.TrimSpace(req.Name), expiresAt)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	view := toAccessTokenView(t)
	view["token"] = secret
	c.JSON(http.StatusCreated, view)
}

func (h *Handler) revokeAccessToken(c *gin.Context) {
	uid, ok := middleware.UserIDFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "no user"})
		return
	}
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid token id"})
		return
	}
	if err := h.accessTokens.Revoke(c.Request.Context(), uid, id); err != nil {
		if errors.Is(err, biz.ErrAccessTokenNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "token not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"revoked": true})
}
