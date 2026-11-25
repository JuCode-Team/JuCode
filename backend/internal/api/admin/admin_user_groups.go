// admin_user_groups.go: CRUD for user_groups + set-default. Decoupled
// from model groups (which live in admin_model_groups.go) — they share
// the word "group" but represent different domain concepts.
package admin

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/cache"
	"github.com/jucode/jucode/internal/data/ent"
)

type userGroupView struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	Description      string `json:"description,omitempty"`
	SortOrder        int    `json:"sort_order"`
	ConcurrencyLimit int    `json:"concurrency_limit"`
	RpmLimit         int    `json:"rpm_limit"`
	TpmLimit         int    `json:"tpm_limit"`
	IsDefault        bool   `json:"is_default"`
}

func toUserGroupView(g *ent.UserGroup) userGroupView {
	return userGroupView{
		ID: g.ID.String(), Name: g.Name,
		Description: g.Description, SortOrder: g.SortOrder,
		ConcurrencyLimit: g.ConcurrencyLimit, RpmLimit: g.RpmLimit, TpmLimit: g.TpmLimit, IsDefault: g.IsDefault,
	}
}

func (h *Handler) listUserGroups(c *gin.Context) {
	gs, err := h.models.ListUserGroups(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	q := strings.ToLower(strings.TrimSpace(c.Query("q")))
	if q != "" {
		filtered := make([]*ent.UserGroup, 0, len(gs))
		for _, g := range gs {
			if strings.Contains(strings.ToLower(g.Name), q) ||
				strings.Contains(strings.ToLower(g.Description), q) {
				filtered = append(filtered, g)
			}
		}
		gs = filtered
	}
	total := len(gs)
	limit, offset := parsePage(c, 500)
	start := offset
	if start > total {
		start = total
	}
	end := start + limit
	if end > total {
		end = total
	}
	page := gs[start:end]
	out := make([]userGroupView, 0, len(page))
	for _, g := range page {
		out = append(out, toUserGroupView(g))
	}
	c.JSON(http.StatusOK, gin.H{"user_groups": out, "total": total, "limit": limit, "offset": offset})
}

type createUserGroupReq struct {
	Name             string `json:"name" binding:"required"`
	Description      string `json:"description"`
	SortOrder        int    `json:"sort_order"`
	ConcurrencyLimit int    `json:"concurrency_limit"`
	RpmLimit         int    `json:"rpm_limit"`
	TpmLimit         int    `json:"tpm_limit"`
}

func (h *Handler) createUserGroup(c *gin.Context) {
	var req createUserGroupReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	g, err := h.models.CreateUserGroup(c.Request.Context(), biz.CreateUserGroupInput{
		Name: req.Name, Description: req.Description, SortOrder: req.SortOrder,
		ConcurrencyLimit: req.ConcurrencyLimit,
		RpmLimit:         req.RpmLimit,
		TpmLimit:         req.TpmLimit,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"user_group": toUserGroupView(g)})
}

type updateUserGroupReq struct {
	Name             *string `json:"name"`
	Description      *string `json:"description"`
	SortOrder        *int    `json:"sort_order"`
	ConcurrencyLimit *int    `json:"concurrency_limit"`
	RpmLimit         *int    `json:"rpm_limit"`
	TpmLimit         *int    `json:"tpm_limit"`
}

func (h *Handler) updateUserGroup(c *gin.Context) {
	id, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	var req updateUserGroupReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	g, err := h.models.UpdateUserGroup(c.Request.Context(), id, biz.UpdateUserGroupInput{
		Name: req.Name, Description: req.Description, SortOrder: req.SortOrder,
		ConcurrencyLimit: req.ConcurrencyLimit,
		RpmLimit:         req.RpmLimit,
		TpmLimit:         req.TpmLimit,
	})
	if err != nil {
		notFoundOr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"user_group": toUserGroupView(g)})
}

func (h *Handler) deleteUserGroup(c *gin.Context) {
	id, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	if err := h.models.DeleteUserGroup(c.Request.Context(), id); err != nil {
		notFoundOr(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *Handler) setDefaultUserGroup(c *gin.Context) {
	id, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	if err := h.models.SetDefaultUserGroup(c.Request.Context(), id); err != nil {
		notFoundOr(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

type modelGroupGrantsReq struct {
	GroupIDs []string `json:"group_ids"`
}

func (h *Handler) getUserGroupModelGroupGrants(c *gin.Context) {
	id, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	ids, err := h.models.UserGroupModelGroupGrants(c.Request.Context(), id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"group_ids": uuidStrings(ids)})
}

func (h *Handler) putUserGroupModelGroupGrants(c *gin.Context) {
	id, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	var req modelGroupGrantsReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	gids, ok := parseUUIDList(c, req.GroupIDs)
	if !ok {
		return
	}
	if err := h.models.ReplaceUserGroupModelGroupGrants(c.Request.Context(), id, gids); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	h.cache.Invalidate(c.Request.Context(), cache.KeyModelsPublic, cache.KeyPricingCompat, cache.KeyStatusSnapshot)
	c.Status(http.StatusNoContent)
}
