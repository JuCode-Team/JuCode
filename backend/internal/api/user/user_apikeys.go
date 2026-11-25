// user_apikeys.go: self-service API key CRUD under /v1/me/apikeys,
// scoped by the account context. Personal context sees the caller's
// personal keys; organization context sees the caller's org keys (all of
// them for owners/admins, who may also rename/limit/revoke members'
// keys — but never reveal their plaintext). A key's billing scope is
// fixed at creation by the active context and can never be changed.
// Ownership is enforced inside biz.ApiKeySvc via the scope's WHERE
// predicates; handlers don't have to re-check.
package user

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/data/ent"
	entschema "github.com/jucode/jucode/internal/data/ent/schema"
	"github.com/jucode/jucode/internal/middleware"
)

type apiKeyView struct {
	ID              string     `json:"id"`
	Prefix          string     `json:"prefix"`
	Name            string     `json:"name"`
	LastUsedAt      *time.Time `json:"last_used_at,omitempty"`
	ExpiresAt       *time.Time `json:"expires_at,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	AllowedGroupIDs []string   `json:"allowed_group_ids"`
	// AllowedGroupsUnrestricted follows the stored list, not the shown one:
	// a key whose groups were all disabled or deleted shows an empty
	// allowed_group_ids yet still cannot route anywhere.
	AllowedGroupsUnrestricted bool       `json:"allowed_groups_unrestricted"`
	IPWhitelist               []string   `json:"ip_whitelist"`
	AutoRoute                 bool       `json:"auto_route"`
	AutoRouteMode             string     `json:"auto_route_mode"`
	AutoRouteMaxRate          float64    `json:"auto_route_max_rate"`
	ModelPins                 []modelPin `json:"model_pins"`
	BillingScope              string     `json:"billing_scope"`
	OrganizationID            *string    `json:"organization_id,omitempty"`
	DailyCostLimit            *string    `json:"daily_cost_limit"`
	MonthlyCostLimit          *string    `json:"monthly_cost_limit"`
	TotalCostLimit            *string    `json:"total_cost_limit"`
	DailyUsed                 string     `json:"daily_cost_used"`
	MonthlyUsed               string     `json:"monthly_cost_used"`
	TotalUsed                 string     `json:"total_cost_used"`
	System                    bool       `json:"system"`
	// UserID / UserEmail identify the key's creator. Only populated for
	// the org owner/admin view, where the list spans multiple members.
	UserID    string `json:"user_id,omitempty"`
	UserEmail string `json:"user_email,omitempty"`
}

// toApiKeyView hides disabled and deleted groups from allowed_group_ids;
// live comes from ApiKeySvc.LiveGroups.
func toApiKeyView(k *ent.ApiKey, live map[uuid.UUID]struct{}) apiKeyView {
	v := apiKeyView{
		ID:                        k.ID.String(),
		Prefix:                    k.Prefix,
		Name:                      k.Name,
		LastUsedAt:                k.LastUsedAt,
		ExpiresAt:                 k.ExpiresAt,
		CreatedAt:                 k.CreatedAt,
		AllowedGroupIDs:           uuidListToStrings(biz.VisibleGroupIDs(k.AllowedGroupIds, live)),
		AllowedGroupsUnrestricted: len(k.AllowedGroupIds) == 0,
		IPWhitelist:               append([]string{}, k.IPWhitelist...),
		AutoRoute:                 k.AutoRoute,
		AutoRouteMode:             string(k.AutoRouteMode),
		AutoRouteMaxRate:          k.AutoRouteMaxRate,
		ModelPins:                 modelPinsToView(k.ModelPins),
		BillingScope:              string(k.BillingScope),
		DailyCostLimit:            decimalPtrString(k.DailyCostLimit), MonthlyCostLimit: decimalPtrString(k.MonthlyCostLimit), TotalCostLimit: decimalPtrString(k.TotalCostLimit),
		System: k.System,
	}
	if k.OrganizationID != nil {
		s := k.OrganizationID.String()
		v.OrganizationID = &s
	}
	return v
}

func uuidListToStrings(ids []uuid.UUID) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, id.String())
	}
	return out
}

type modelPin struct {
	Model   string `json:"model"`
	GroupID string `json:"group_id"`
}

func modelPinsToView(pins []entschema.ModelPin) []modelPin {
	out := make([]modelPin, 0, len(pins))
	for _, pin := range pins {
		out = append(out, modelPin{Model: pin.Model, GroupID: pin.GroupID.String()})
	}
	return out
}

func modelPinsFromView(pins []modelPin) []entschema.ModelPin {
	out := make([]entschema.ModelPin, 0, len(pins))
	for _, pin := range pins {
		id, err := uuid.Parse(pin.GroupID)
		if err != nil || strings.TrimSpace(pin.Model) == "" {
			continue
		}
		out = append(out, entschema.ModelPin{Model: pin.Model, GroupID: id})
	}
	return out
}

func modelPinsFromRaw(v any) ([]entschema.ModelPin, error) {
	if v == nil {
		return []entschema.ModelPin{}, nil
	}
	arr, ok := v.([]any)
	if !ok {
		return nil, errors.New("invalid model_pins")
	}
	out := make([]entschema.ModelPin, 0, len(arr))
	for _, item := range arr {
		row, ok := item.(map[string]any)
		if !ok {
			return nil, errors.New("invalid model_pins")
		}
		model, _ := row["model"].(string)
		group, _ := row["group_id"].(string)
		id, err := uuid.Parse(group)
		if err != nil || strings.TrimSpace(model) == "" {
			return nil, errors.New("invalid model_pins")
		}
		out = append(out, entschema.ModelPin{Model: model, GroupID: id})
	}
	return out, nil
}

// liveGroups loads which of ids are enabled groups for toApiKeyView. Handlers
// that return a one-time secret call it before mutating, so a failure here
// cannot swallow a freshly minted key.
func (h *Handler) liveGroups(c *gin.Context, ids []uuid.UUID) (map[uuid.UUID]struct{}, bool) {
	live, err := h.apikeys.LiveGroups(c.Request.Context(), ids)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return nil, false
	}
	return live, true
}

// apiKeyScopeFrom builds the account-context scope for apikey CRUD from
// the values the OrgContext middleware already verified. Writes the error
// response itself when it returns ok=false.
func (h *Handler) apiKeyScopeFrom(c *gin.Context) (biz.ApiKeyScope, bool) {
	uid, ok := middleware.UserIDFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "no user"})
		return biz.ApiKeyScope{}, false
	}
	sc := biz.ApiKeyScope{UserID: uid}
	if orgID, role, has := middleware.OrgContextFrom(c); has {
		sc.OrgID = &orgID
		sc.OrgManager = middleware.IsOrgManager(role)
	}
	return sc, true
}

func (h *Handler) listApiKeys(c *gin.Context) {
	scope, ok := h.apiKeyScopeFrom(c)
	if !ok {
		return
	}
	keys, err := h.apikeys.ListScoped(c.Request.Context(), scope)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	var ids []uuid.UUID
	for _, k := range keys {
		ids = append(ids, k.Key.AllowedGroupIds...)
	}
	live, ok := h.liveGroups(c, ids)
	if !ok {
		return
	}
	views := make([]apiKeyView, 0, len(keys))
	for _, k := range keys {
		v := toApiKeyView(k.Key, live)
		v.DailyUsed, v.MonthlyUsed, v.TotalUsed = k.DailyUsed.String(), k.MonthlyUsed.String(), k.TotalUsed.String()
		if scope.OrgManager && scope.OrgID != nil {
			v.UserID = k.Key.UserID.String()
			if u, err := k.Key.Edges.UserOrErr(); err == nil {
				v.UserEmail = u.Email
			}
		}
		views = append(views, v)
	}
	c.JSON(http.StatusOK, gin.H{"api_keys": views})
}

type createApiKeyReq struct {
	Name             string     `json:"name"`
	ExpiresAt        *time.Time `json:"expires_at"`
	AllowedGroupIDs  []string   `json:"allowed_group_ids"`
	IPWhitelist      []string   `json:"ip_whitelist"`
	AutoRoute        bool       `json:"auto_route"`
	AutoRouteMode    string     `json:"auto_route_mode"`
	AutoRouteMaxRate float64    `json:"auto_route_max_rate"`
	ModelPins        []modelPin `json:"model_pins"`
	BillingScope     string     `json:"billing_scope"`
	OrganizationID   *string    `json:"organization_id"`
	DailyCostLimit   *string    `json:"daily_cost_limit"`
	MonthlyCostLimit *string    `json:"monthly_cost_limit"`
	TotalCostLimit   *string    `json:"total_cost_limit"`
}

func (h *Handler) createApiKey(c *gin.Context) {
	uid, ok := middleware.UserIDFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "no user"})
		return
	}
	var req createApiKeyReq
	// All fields are optional; empty body is fine. But malformed JSON
	// should not silently land at zero values.
	if c.Request.ContentLength > 0 {
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
	}
	in := biz.CreateInput{
		Name:             req.Name,
		ExpiresAt:        req.ExpiresAt,
		IPWhitelist:      req.IPWhitelist,
		AutoRoute:        req.AutoRoute,
		AutoRouteMode:    req.AutoRouteMode,
		AutoRouteMaxRate: req.AutoRouteMaxRate,
		ModelPins:        modelPinsFromView(req.ModelPins),
		DailyCostLimit:   parseLimit(req.DailyCostLimit), MonthlyCostLimit: parseLimit(req.MonthlyCostLimit), TotalCostLimit: parseLimit(req.TotalCostLimit),
	}
	// Billing scope follows the account context — the request body cannot
	// choose it. Org context pins the key to the active organization;
	// personal context rejects org fields outright so a stale client
	// can't silently create a personal key it meant to be org-billed.
	if orgID, _, has := middleware.OrgContextFrom(c); has {
		in.BillingScope = "organization"
		in.OrganizationID = &orgID
	} else {
		if req.BillingScope == "organization" || (req.OrganizationID != nil && *req.OrganizationID != "") {
			c.JSON(http.StatusBadRequest, gin.H{"error": "switch to the organization context to create organization keys"})
			return
		}
		in.BillingScope = "personal"
	}
	for _, s := range req.AllowedGroupIDs {
		id, err := uuid.Parse(s)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid allowed_group_id: " + s})
			return
		}
		in.AllowedGroupIDs = append(in.AllowedGroupIDs, id)
	}
	live, ok := h.liveGroups(c, in.AllowedGroupIDs)
	if !ok {
		return
	}
	res, err := h.apikeys.Create(c.Request.Context(), uid, in)
	if err != nil {
		apiKeyError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{
		"api_key": toApiKeyView(res.Record, live),
		"secret":  res.Secret,
	})
}

// updateApiKey parses the raw body so we can distinguish "field absent"
// from "field explicitly empty/null" — important for allowed_group_ids
// (an empty array means "remove all", while absence means "no change").
func (h *Handler) updateApiKey(c *gin.Context) {
	scope, ok := h.apiKeyScopeFrom(c)
	if !ok {
		return
	}
	apiKeyID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid api key id"})
		return
	}
	var raw map[string]any
	if err := c.ShouldBindJSON(&raw); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	// A key's billing scope is fixed at creation — migrating it would
	// silently change whose balance pays for its traffic.
	if _, has := raw["billing_scope"]; has {
		c.JSON(http.StatusBadRequest, gin.H{"error": "billing_scope of an existing key cannot be changed"})
		return
	}
	if _, has := raw["organization_id"]; has {
		c.JSON(http.StatusBadRequest, gin.H{"error": "organization_id of an existing key cannot be changed"})
		return
	}
	in := biz.UpdateInput{}
	for name, dst := range map[string]***decimal.Decimal{"daily_cost_limit": &in.DailyCostLimit, "monthly_cost_limit": &in.MonthlyCostLimit, "total_cost_limit": &in.TotalCostLimit} {
		v, e := patchLimit(raw, name)
		if e != nil {
			c.JSON(400, gin.H{"error": e.Error()})
			return
		}
		*dst = v
	}
	if v, ok := raw["name"].(string); ok {
		in.Name = &v
	}
	// expires_at: absent = keep, null = clear, RFC3339 string = set.
	if v, ok := raw["expires_at"]; ok {
		if v == nil {
			var cleared *time.Time
			in.ExpiresAt = &cleared
		} else {
			str, _ := v.(string)
			t, err := time.Parse(time.RFC3339, str)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid expires_at: expected RFC3339 timestamp or null"})
				return
			}
			tp := &t
			in.ExpiresAt = &tp
		}
	}
	// ip_whitelist: absent = keep, null / empty array = unrestricted.
	if v, ok := raw["ip_whitelist"]; ok {
		entries := []string{}
		if v != nil {
			arr, ok := v.([]any)
			if !ok {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid ip_whitelist: expected array of strings"})
				return
			}
			for _, item := range arr {
				str, ok := item.(string)
				if !ok {
					c.JSON(http.StatusBadRequest, gin.H{"error": "invalid ip_whitelist: expected array of strings"})
					return
				}
				entries = append(entries, str)
			}
		}
		in.IPWhitelist = &entries
	}
	if v, ok := raw["allowed_group_ids"].([]any); ok {
		ids := make([]uuid.UUID, 0, len(v))
		for _, s := range v {
			str, _ := s.(string)
			id, err := uuid.Parse(str)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid allowed_group_id: " + str})
				return
			}
			ids = append(ids, id)
		}
		in.AllowedGroupIDs = &ids
	}
	if v, ok := raw["auto_route"].(bool); ok {
		in.AutoRoute = &v
	}
	if v, ok := raw["auto_route_mode"].(string); ok {
		in.AutoRouteMode = &v
	}
	if v, ok := raw["auto_route_max_rate"].(float64); ok {
		in.AutoRouteMaxRate = &v
	}
	if v, ok := raw["model_pins"]; ok {
		pins, err := modelPinsFromRaw(v)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		in.ModelPins = &pins
	}
	rec, err := h.apikeys.UpdateScoped(c.Request.Context(), scope, apiKeyID, in)
	if err != nil {
		apiKeyError(c, err)
		return
	}
	live, ok := h.liveGroups(c, rec.AllowedGroupIds)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, gin.H{"api_key": toApiKeyView(rec, live)})
}

// revealApiKey returns the plaintext API key for the calling user's
// own key. Ownership is enforced inside ApiKeySvc.Reveal by passing
// uid; legacy keys without secret_enc return 410 Gone so the UI can
// nudge the user to rotate.
func (h *Handler) revealApiKey(c *gin.Context) {
	uid, ok := middleware.UserIDFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "no user"})
		return
	}
	apiKeyID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	secret, err := h.apikeys.Reveal(c.Request.Context(), apiKeyID, uid)
	if err != nil {
		switch {
		case errors.Is(err, biz.ErrApiKeyNotFound):
			c.JSON(http.StatusNotFound, gin.H{"error": "api key not found"})
		case errors.Is(err, biz.ErrApiKeyUnrevealable):
			c.JSON(http.StatusGone, gin.H{
				"error": err.Error(),
				"code":  "legacy_key",
			})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		}
		return
	}
	c.JSON(http.StatusOK, gin.H{"secret": secret})
}

func (h *Handler) revokeApiKey(c *gin.Context) {
	scope, ok := h.apiKeyScopeFrom(c)
	if !ok {
		return
	}
	apiKeyID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if err := h.apikeys.RevokeScoped(c.Request.Context(), scope, apiKeyID); err != nil {
		apiKeyError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *Handler) ensureLabKey(c *gin.Context) {
	scope, ok := h.apiKeyScopeFrom(c)
	if !ok {
		return
	}
	res, err := h.apikeys.EnsureLabKey(c.Request.Context(), scope)
	if err != nil {
		apiKeyError(c, err)
		return
	}
	// System keys are always unrestricted (readySystemKey), so there is no
	// group list to check.
	c.JSON(http.StatusOK, gin.H{"api_key": toApiKeyView(res.Record, nil)})
}

// AttachLabKey loads the caller's lab key onto the request so the console
// laboratory can use the generation handlers. The public gateway rejects
// that key's secret; this path is the only one that may call with it.
func (h *Handler) AttachLabKey() gin.HandlerFunc {
	return func(c *gin.Context) {
		scope, ok := h.apiKeyScopeFrom(c)
		if !ok {
			c.Abort()
			return
		}
		res, err := h.apikeys.EnsureLabKey(c.Request.Context(), scope)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		u, err := h.users.User.Get(c.Request.Context(), scope.UserID)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if u.Status != "active" {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "user is not active"})
			return
		}
		if u.FrozenUntil != nil && u.FrozenUntil.After(time.Now()) {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "account temporarily frozen"})
			return
		}
		c.Set(middleware.CtxUser, u)
		c.Set(middleware.CtxApiKey, res.Record)
		if res.Record.LastUsedAt == nil || time.Since(*res.Record.LastUsedAt) >= 30*time.Second {
			id := res.Record.ID
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				h.apikeys.TouchLastUsed(ctx, id)
			}()
		}
		c.Next()
	}
}

func (h *Handler) rotateApiKey(c *gin.Context) {
	scope, ok := h.apiKeyScopeFrom(c)
	if !ok {
		return
	}
	apiKeyID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	res, err := h.apikeys.RotateScoped(c.Request.Context(), scope, apiKeyID)
	if err != nil {
		if errors.Is(err, biz.ErrApiKeyUnrevealable) {
			c.JSON(http.StatusGone, gin.H{"error": err.Error()})
			return
		}
		apiKeyError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"api_key": toApiKeyView(res.Record, nil), // system key: always unrestricted
		"secret":  res.Secret,
	})
}

func apiKeyError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, biz.ErrApiKeyNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "api key not found"})
	case errors.Is(err, biz.ErrApiKeyOrgDenied):
		c.JSON(http.StatusForbidden, gin.H{"error": "not allowed"})
	case errors.Is(err, biz.ErrApiKeySystem):
		c.JSON(http.StatusForbidden, gin.H{"error": err.Error()})
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	}
}

func decimalPtrString(v *decimal.Decimal) *string {
	if v == nil {
		return nil
	}
	s := v.String()
	return &s
}

func parseLimit(v *string) *decimal.Decimal {
	if v == nil {
		return nil
	}
	d, err := decimal.NewFromString(*v)
	if err != nil {
		return nil
	}
	return &d
}

func patchLimit(raw map[string]any, name string) (**decimal.Decimal, error) {
	v, ok := raw[name]
	if !ok {
		return nil, nil
	}
	if v == nil {
		var x *decimal.Decimal
		return &x, nil
	}
	str, ok := v.(string)
	if !ok {
		return nil, errors.New("invalid " + name)
	}
	d, err := decimal.NewFromString(str)
	if err != nil {
		return nil, errors.New("invalid " + name)
	}
	x := &d
	return &x, nil
}
