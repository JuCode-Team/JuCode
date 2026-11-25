// admin_model_groups.go: model-group CRUD plus the attach/detach edges
// that link a group to specific models or providers. Distinct from
// user_groups (which describes a user audience).
package admin

import (
	"errors"
	"math"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/cache"
	"github.com/jucode/jucode/internal/data/ent"
	entgrpmodel "github.com/jucode/jucode/internal/data/ent/modelgroupmodel"
	entgrpprov "github.com/jucode/jucode/internal/data/ent/modelgroupprovider"
	entschema "github.com/jucode/jucode/internal/data/ent/schema"
)

type groupView struct {
	ID                  string                      `json:"id"`
	Name                string                      `json:"name"`
	BillingSource       string                      `json:"billing_source"`
	RateMultiplier      float64                     `json:"rate_multiplier"`
	Description         string                      `json:"description,omitempty"`
	Profile             []entschema.GroupProfileDim `json:"profile,omitempty"`
	AllowedUserGroupIDs []string                    `json:"allowed_user_group_ids"`
	GrantedUserIDs      []string                    `json:"granted_user_ids"`
	GrantedUserCount    int                         `json:"granted_user_count"`
	Exclusive           bool                        `json:"exclusive"`
	Enabled             bool                        `json:"enabled"`
	ModelIDs            []string                    `json:"model_ids"`
	ModelLinks          []modelLinkView             `json:"model_links"`
	ProviderIDs         []string                    `json:"provider_ids"`
	ProviderLinks       []providerLinkView          `json:"provider_links"`
	SortOrder           int                         `json:"sort_order"`
}

type providerLinkView struct {
	ProviderID string `json:"provider_id"`
	Priority   int    `json:"priority"`
	Weight     int    `json:"weight"`
}

// modelLinkView carries the per-link settings that model_ids alone cannot: the
// same model in two groups is probed independently, so the switch and cadence
// belong to the link rather than to the model.
type modelLinkView struct {
	ModelID              string `json:"model_id"`
	ProbeEnabled         bool   `json:"probe_enabled"`
	ProbeIntervalSeconds int    `json:"probe_interval_seconds"`
	ProbeDisableThinking bool   `json:"probe_disable_thinking"`
	// The model's window through this group; absent = the model default.
	ContextWindow *int `json:"context_window,omitempty"`
}

func toGroupView(g *ent.ModelGroup) groupView {
	ids := make([]string, 0, len(g.AllowedUserGroupIds))
	for _, id := range g.AllowedUserGroupIds {
		ids = append(ids, id.String())
	}
	return groupView{
		ID: g.ID.String(), Name: g.Name,
		BillingSource: string(g.BillingSource), RateMultiplier: g.RateMultiplier,
		Description: g.Description, Profile: g.Profile, AllowedUserGroupIDs: ids,
		GrantedUserIDs: []string{}, GrantedUserCount: 0,
		Exclusive: g.Exclusive,
		Enabled:   g.Enabled,
		ModelIDs:  []string{}, ModelLinks: []modelLinkView{},
		ProviderIDs: []string{}, ProviderLinks: []providerLinkView{},
		SortOrder: g.SortOrder,
	}
}

func applyGrants(v *groupView, grants biz.GroupGrants) {
	v.AllowedUserGroupIDs = make([]string, 0, len(grants.UserGroupIDs))
	for _, id := range grants.UserGroupIDs {
		v.AllowedUserGroupIDs = append(v.AllowedUserGroupIDs, id.String())
	}
	v.GrantedUserIDs = make([]string, 0, len(grants.UserIDs))
	for _, id := range grants.UserIDs {
		v.GrantedUserIDs = append(v.GrantedUserIDs, id.String())
	}
	v.GrantedUserCount = len(grants.UserIDs)
}

func (h *Handler) listGroups(c *gin.Context) {
	gs, err := h.models.ListGroups(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	q := strings.ToLower(strings.TrimSpace(c.Query("q")))
	if q != "" {
		filtered := make([]*ent.ModelGroup, 0, len(gs))
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
	groupIDs := make([]uuid.UUID, 0, len(page))
	for _, g := range page {
		groupIDs = append(groupIDs, g.ID)
	}
	modelIDs := map[uuid.UUID][]string{}
	modelLinks := map[uuid.UUID][]modelLinkView{}
	providerIDs := map[uuid.UUID][]string{}
	providerLinks := map[uuid.UUID][]providerLinkView{}
	if len(groupIDs) > 0 {
		links, err := h.users.ModelGroupModel.Query().
			Where(entgrpmodel.GroupIDIn(groupIDs...)).
			All(c.Request.Context())
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		for _, link := range links {
			modelIDs[link.GroupID] = append(modelIDs[link.GroupID], link.ModelID.String())
			modelLinks[link.GroupID] = append(modelLinks[link.GroupID], modelLinkView{
				ModelID:              link.ModelID.String(),
				ProbeEnabled:         link.ProbeEnabled,
				ProbeIntervalSeconds: link.ProbeIntervalSeconds,
				ProbeDisableThinking: link.ProbeDisableThinking,
				ContextWindow:        link.ContextWindow,
			})
		}
		plinks, err := h.users.ModelGroupProvider.Query().
			Where(entgrpprov.GroupIDIn(groupIDs...)).
			All(c.Request.Context())
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		for _, link := range plinks {
			providerIDs[link.GroupID] = append(providerIDs[link.GroupID], link.ProviderID.String())
			providerLinks[link.GroupID] = append(providerLinks[link.GroupID], providerLinkView{
				ProviderID: link.ProviderID.String(),
				Priority:   link.Priority,
				Weight:     link.Weight,
			})
		}
	}
	out := make([]groupView, 0, len(page))
	for _, g := range page {
		v := toGroupView(g)
		if grants, err := h.models.GroupGrants(c.Request.Context(), g.ID); err == nil {
			applyGrants(&v, grants)
		}
		if ids, ok := modelIDs[g.ID]; ok {
			v.ModelIDs = ids
		}
		if links, ok := modelLinks[g.ID]; ok {
			v.ModelLinks = links
		}
		if ids, ok := providerIDs[g.ID]; ok {
			v.ProviderIDs = ids
		}
		if links, ok := providerLinks[g.ID]; ok {
			v.ProviderLinks = links
		}
		out = append(out, v)
	}
	c.JSON(http.StatusOK, gin.H{"groups": out, "total": total, "limit": limit, "offset": offset})
}

type createGroupReq struct {
	Name           string                       `json:"name" binding:"required"`
	BillingSource  string                       `json:"billing_source" binding:"required,oneof=plan_only balance_only"`
	RateMultiplier float64                      `json:"rate_multiplier"`
	Description    string                       `json:"description"`
	Profile        *[]entschema.GroupProfileDim `json:"profile"`
	Enabled        *bool                        `json:"enabled"`
}

// normalizeGroupProfile trims axis labels, drops rows left blank (unset dims
// must not appear on the public radar chart), and clamps scores into the
// 0-100 range the chart is drawn on. More than six named axes is rejected —
// the card is a six-dimension chart, so extras would never render.
func normalizeGroupProfile(in []entschema.GroupProfileDim) ([]entschema.GroupProfileDim, error) {
	out := make([]entschema.GroupProfileDim, 0, len(in))
	for _, d := range in {
		name := strings.TrimSpace(d.Name)
		if name == "" {
			continue
		}
		if math.IsNaN(d.Value) || math.IsInf(d.Value, 0) {
			return nil, errors.New("profile value for " + name + " must be a finite number")
		}
		v := d.Value
		if v < 0 {
			v = 0
		} else if v > 100 {
			v = 100
		}
		out = append(out, entschema.GroupProfileDim{Name: name, Value: v})
	}
	if len(out) > 6 {
		return nil, errors.New("profile supports at most 6 dimensions")
	}
	return out, nil
}

func (h *Handler) createGroup(c *gin.Context) {
	var req createGroupReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	var profile []entschema.GroupProfileDim
	if req.Profile != nil {
		p, err := normalizeGroupProfile(*req.Profile)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		profile = p
	}
	g, err := h.models.CreateGroup(c.Request.Context(), biz.CreateGroupInput{
		Name: req.Name, BillingSource: req.BillingSource,
		RateMultiplier: req.RateMultiplier, Description: req.Description,
		Profile: profile,
		Enabled: req.Enabled,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	h.cache.Invalidate(c.Request.Context(), cache.KeyModelsPublic, cache.KeyPricingCompat)
	c.JSON(http.StatusCreated, gin.H{"group": toGroupView(g)})
}

func (h *Handler) deleteGroup(c *gin.Context) {
	id, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	if err := h.models.DeleteGroup(c.Request.Context(), id); err != nil {
		notFoundOr(c, err)
		return
	}
	if !h.unbindRetiredGroup(c, id) {
		return
	}
	h.cache.Invalidate(c.Request.Context(), cache.KeyModelsPublic, cache.KeyPricingCompat)
	c.Status(http.StatusNoContent)
}

// reorderGroups only touches display order, so KeyStatusSnapshot stays valid:
// the public status page orders its cards by the admin-arranged
// status_page_group_ids list, not by this column.
func (h *Handler) reorderGroups(c *gin.Context) {
	ids, ok := parseReorderIDs(c)
	if !ok {
		return
	}
	if err := h.models.ReorderGroups(c.Request.Context(), ids); err != nil {
		notFoundOr(c, err)
		return
	}
	h.cache.Invalidate(c.Request.Context(), cache.KeyModelsPublic, cache.KeyPricingCompat)
	c.Status(http.StatusNoContent)
}

type updateGroupReq struct {
	Name                *string                      `json:"name"`
	BillingSource       *string                      `json:"billing_source"`
	RateMultiplier      *float64                     `json:"rate_multiplier"`
	Description         *string                      `json:"description"`
	Profile             *[]entschema.GroupProfileDim `json:"profile"`
	AllowedUserGroupIDs *[]string                    `json:"allowed_user_group_ids"`
	Enabled             *bool                        `json:"enabled"`
	Exclusive           *bool                        `json:"exclusive"`
}

func (h *Handler) updateGroup(c *gin.Context) {
	id, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	var req updateGroupReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	in := biz.UpdateGroupInput{
		Name: req.Name, BillingSource: req.BillingSource,
		RateMultiplier: req.RateMultiplier, Description: req.Description,
		Enabled: req.Enabled, Exclusive: req.Exclusive,
	}
	if req.Profile != nil {
		p, err := normalizeGroupProfile(*req.Profile)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		in.Profile = &p
	}
	if req.AllowedUserGroupIDs != nil {
		ids := make([]uuid.UUID, 0, len(*req.AllowedUserGroupIDs))
		for _, s := range *req.AllowedUserGroupIDs {
			u, err := uuid.Parse(s)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid allowed_user_group_id: " + s})
				return
			}
			ids = append(ids, u)
		}
		in.AllowedUserGroupIDs = &ids
	}
	g, err := h.models.UpdateGroup(c.Request.Context(), id, in)
	if err != nil {
		notFoundOr(c, err)
		return
	}
	if req.Enabled != nil && !*req.Enabled && !h.unbindRetiredGroup(c, id) {
		return
	}
	h.cache.Invalidate(c.Request.Context(), cache.KeyModelsPublic, cache.KeyPricingCompat, cache.KeyStatusSnapshot)
	c.JSON(http.StatusOK, gin.H{"group": toGroupView(g)})
}

// unbindRetiredGroup drops the group from every API key allow-list. A disabled
// or deleted group must not stay bound, and later key queries must not
// return it.
func (h *Handler) unbindRetiredGroup(c *gin.Context, id uuid.UUID) bool {
	if h.apikeys == nil {
		return true
	}
	if err := h.apikeys.UnbindGroup(c.Request.Context(), id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return false
	}
	return true
}

func (h *Handler) attachModelToGroup(c *gin.Context) {
	gid, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	mid, ok := parseUUID(c, "mid")
	if !ok {
		return
	}
	if err := h.models.AttachModelToGroup(c.Request.Context(), gid, mid); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	h.cache.Invalidate(c.Request.Context(), cache.KeyModelsPublic, cache.KeyPricingCompat)
	c.Status(http.StatusNoContent)
}

// Pointers so a caller can flip the switch without knowing the interval, and
// change the interval without touching the switch.
type setModelProbeReq struct {
	ProbeEnabled         *bool `json:"probe_enabled"`
	ProbeIntervalSeconds *int  `json:"probe_interval_seconds"`
	ProbeDisableThinking *bool `json:"probe_disable_thinking"`
	// The model's window through this group: absent = no change; 0 = clear.
	ContextWindow *int `json:"context_window"`
}

func (h *Handler) setModelProbe(c *gin.Context) {
	gid, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	mid, ok := parseUUID(c, "mid")
	if !ok {
		return
	}
	var req setModelProbeReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.ContextWindow != nil && *req.ContextWindow < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "context_window must be >= 0"})
		return
	}
	err := h.models.SetModelProbe(c.Request.Context(), gid, mid, req.ProbeEnabled, req.ProbeIntervalSeconds, req.ProbeDisableThinking, req.ContextWindow)
	if errors.Is(err, biz.ErrModelNotInGroup) {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	// The prober picks the change up on its own reconcile tick; the status
	// snapshot is invalidated because enabling a probe changes what the page
	// will have to say about that model within the next window.
	h.cache.Invalidate(c.Request.Context(), cache.KeyModelsPublic, cache.KeyPricingCompat, cache.KeyStatusSnapshot)
	c.Status(http.StatusNoContent)
}

func (h *Handler) detachModelFromGroup(c *gin.Context) {
	gid, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	mid, ok := parseUUID(c, "mid")
	if !ok {
		return
	}
	if err := h.models.DetachModelFromGroup(c.Request.Context(), gid, mid); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	h.cache.Invalidate(c.Request.Context(), cache.KeyModelsPublic, cache.KeyPricingCompat)
	c.Status(http.StatusNoContent)
}

type attachProviderReq struct {
	Priority int `json:"priority"`
	Weight   int `json:"weight"`
}

func (h *Handler) attachProviderToGroup(c *gin.Context) {
	gid, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	pid, ok := parseUUID(c, "pid")
	if !ok {
		return
	}
	var req attachProviderReq
	if c.Request.ContentLength > 0 {
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
	}
	if err := h.models.AttachProviderToGroup(c.Request.Context(), gid, pid, req.Priority, req.Weight); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	h.cache.Invalidate(c.Request.Context(), cache.KeyModelsPublic, cache.KeyPricingCompat)
	c.Status(http.StatusNoContent)
}

func (h *Handler) detachProviderFromGroup(c *gin.Context) {
	gid, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	pid, ok := parseUUID(c, "pid")
	if !ok {
		return
	}
	if err := h.models.DetachProviderFromGroup(c.Request.Context(), gid, pid); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	h.cache.Invalidate(c.Request.Context(), cache.KeyModelsPublic, cache.KeyPricingCompat)
	c.Status(http.StatusNoContent)
}

type cloneGroupReq struct {
	Name           *string  `json:"name"`
	Exclusive      *bool    `json:"exclusive"`
	RateMultiplier *float64 `json:"rate_multiplier"`
}

func (h *Handler) cloneGroup(c *gin.Context) {
	id, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	var req cloneGroupReq
	if c.Request.ContentLength > 0 {
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
	}
	g, err := h.models.CloneGroup(c.Request.Context(), id, biz.CloneGroupInput{
		Name: req.Name, Exclusive: req.Exclusive, RateMultiplier: req.RateMultiplier,
	})
	if err != nil {
		notFoundOr(c, err)
		return
	}
	h.cache.Invalidate(c.Request.Context(), cache.KeyModelsPublic, cache.KeyPricingCompat, cache.KeyStatusSnapshot)
	c.JSON(http.StatusCreated, gin.H{"group": toGroupView(g)})
}

type groupGrantsReq struct {
	UserIDs      []string `json:"user_ids"`
	UserGroupIDs []string `json:"user_group_ids"`
}

func (h *Handler) getGroupGrants(c *gin.Context) {
	id, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	grants, err := h.models.GroupGrants(c.Request.Context(), id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"user_ids":       uuidStrings(grants.UserIDs),
		"user_group_ids": uuidStrings(grants.UserGroupIDs),
	})
}

func (h *Handler) putGroupGrants(c *gin.Context) {
	id, ok := parseUUID(c, "id")
	if !ok {
		return
	}
	var req groupGrantsReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	users, ok := parseUUIDList(c, req.UserIDs)
	if !ok {
		return
	}
	ugs, ok := parseUUIDList(c, req.UserGroupIDs)
	if !ok {
		return
	}
	if err := h.models.ReplaceGroupGrants(c.Request.Context(), id, biz.GroupGrants{
		UserIDs: users, UserGroupIDs: ugs,
	}); err != nil {
		notFoundOr(c, err)
		return
	}
	h.cache.Invalidate(c.Request.Context(), cache.KeyModelsPublic, cache.KeyPricingCompat, cache.KeyStatusSnapshot)
	c.Status(http.StatusNoContent)
}
