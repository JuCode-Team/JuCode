// user_plans.go: subscription / redeem-code / group discovery
// endpoints that are user-facing counterparts to the admin equivalents.
package user

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/cache"
	"github.com/jucode/jucode/internal/data/ent"
	"github.com/jucode/jucode/internal/data/ent/modelgroup"
	"github.com/jucode/jucode/internal/middleware"
)

func (h *Handler) listAvailableGroups(c *gin.Context) {
	uid, ok := middleware.UserIDFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "no user"})
		return
	}
	u, err := h.users.User.Get(c.Request.Context(), uid)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	groups, err := h.models.AccessibleGroups(c.Request.Context(), u)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	_, plan, planErr := h.plans.ActiveUserPlan(c.Request.Context(), uid)
	if planErr != nil && !errors.Is(planErr, biz.ErrNoActivePlan) {
		c.JSON(http.StatusInternalServerError, gin.H{"error": planErr.Error()})
		return
	}
	visible := make([]*ent.ModelGroup, 0, len(groups))
	for _, g := range groups {
		// Plan-only groups need a real subscription: a credit pack
		// (pay_as_you_go) only grants balance and unlocks nothing.
		if g.BillingSource == modelgroup.BillingSourcePlanOnly && (!biz.IsSubscriptionPlan(plan) || !biz.PlanAllowsGroup(plan, g.ID)) {
			continue
		}
		visible = append(visible, g)
	}
	ids := make([]uuid.UUID, len(visible))
	for i, g := range visible {
		ids[i] = g.ID
	}
	catalogs, err := h.models.GroupCatalogs(c.Request.Context(), ids)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	states, statesErr := h.cachedGroupStates(c.Request.Context())
	if statesErr != nil {
		states = map[uuid.UUID]biz.StatusState{}
	}
	out := make([]gin.H, 0, len(visible))
	for _, g := range visible {
		cat := catalogs[g.ID]
		state := states[g.ID]
		if state == "" {
			state = biz.StatusUnknown
		}
		out = append(out, gin.H{
			"id":              g.ID.String(),
			"name":            g.Name,
			"billing_source":  g.BillingSource,
			"rate_multiplier": g.RateMultiplier,
			"description":     g.Description,
			"exclusive":       g.Exclusive,
			"models":          cat.Models,
			"vendors":         cat.Vendors,
			"supports_image":  cat.SupportsImage,
			"status":          state,
		})
	}
	c.JSON(http.StatusOK, gin.H{"groups": out})
}

// cachedGroupStates is the picker status pill for every enabled group.
// One 30s entry is shared across users; exclusive groups are included
// because the public status snapshot hides them.
func (h *Handler) cachedGroupStates(ctx context.Context) (map[uuid.UUID]biz.StatusState, error) {
	if h.status == nil {
		return map[uuid.UUID]biz.StatusState{}, nil
	}
	raw, _, err := cache.GetOrCompute(ctx, h.cache, cache.KeyGroupLiveStates, 30*time.Second, func(ctx context.Context) (map[string]string, error) {
		groups, err := h.models.ListGroups(ctx)
		if err != nil {
			return nil, err
		}
		ids := make([]uuid.UUID, 0, len(groups))
		for _, g := range groups {
			if g.Enabled {
				ids = append(ids, g.ID)
			}
		}
		states, err := h.status.GroupLiveStates(ctx, ids)
		if err != nil {
			return nil, err
		}
		out := make(map[string]string, len(states))
		for id, state := range states {
			out[id.String()] = string(state)
		}
		return out, nil
	})
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]biz.StatusState, len(raw))
	for id, state := range raw {
		parsed, parseErr := uuid.Parse(id)
		if parseErr != nil {
			continue
		}
		out[parsed] = biz.StatusState(state)
	}
	return out, nil
}

func (h *Handler) listAvailablePlans(c *gin.Context) {
	ctx := c.Request.Context()
	ps, err := h.plans.ListPlans(ctx, true /*onlyActive*/)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	// Expand allowed_group_ids → {id, name, billing_source} so the UI can
	// show "buying this plan unlocks groups X, Y" without an N+1 round-trip.
	allGroups, err := h.models.ListGroups(ctx)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	groupByID := make(map[uuid.UUID]gin.H, len(allGroups))
	for _, g := range allGroups {
		if !g.Enabled || g.Exclusive {
			continue
		}
		groupByID[g.ID] = gin.H{
			"id":             g.ID.String(),
			"name":           g.Name,
			"billing_source": g.BillingSource,
		}
	}
	out := make([]gin.H, 0, len(ps))
	for _, p := range ps {
		groups := make([]gin.H, 0, len(p.AllowedGroupIds))
		for _, gid := range p.AllowedGroupIds {
			if g, ok := groupByID[gid]; ok {
				groups = append(groups, g)
			}
		}
		out = append(out, gin.H{
			"id":                    p.ID.String(),
			"name":                  p.Name,
			"price":                 p.Price.String(),
			"currency":              p.Currency,
			"type":                  p.Type,
			"period_days":           p.PeriodDays,
			"quota_5h":              p.Quota5h.String(),
			"quota_weekly":          p.QuotaWeekly.String(),
			"quota_monthly":         p.QuotaMonthly.String(),
			"description":           p.Description,
			"allowed_groups":        groups,
			"monthly_spend_limit":   p.MonthlySpendLimit.String(),
			"initial_balance":       p.InitialBalance.String(),
			"auto_reload_enabled":   p.AutoReloadEnabled,
			"auto_reload_threshold": p.AutoReloadThreshold.String(),
			"auto_reload_amount":    p.AutoReloadAmount.String(),
		})
	}
	c.JSON(http.StatusOK, gin.H{"plans": out})
}

type redeemReq struct {
	Code string `json:"code" binding:"required"`
}

func (h *Handler) redeemCode(c *gin.Context) {
	uid, ok := middleware.UserIDFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "no user"})
		return
	}
	var req redeemReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	res, err := h.redeem.Redeem(c.Request.Context(), uid, req.Code, c.ClientIP())
	if err != nil {
		switch {
		case errors.Is(err, biz.ErrRedeemCodeNotFound):
			c.JSON(http.StatusNotFound, gin.H{"error": "invalid code"})
		case errors.Is(err, biz.ErrRedeemCodeExpired):
			c.JSON(http.StatusGone, gin.H{"error": "code expired"})
		case errors.Is(err, biz.ErrRedeemCodeExhausted):
			c.JSON(http.StatusConflict, gin.H{"error": "code already fully claimed"})
		case errors.Is(err, biz.ErrRedeemAlreadyUsed):
			c.JSON(http.StatusConflict, gin.H{"error": "你已经兑换过这个码了"})
		case errors.Is(err, biz.ErrRedeemCodeInactive):
			c.JSON(http.StatusConflict, gin.H{"error": "code is not active"})
		case errors.Is(err, biz.ErrRedeemKindMismatch):
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "code misconfigured"})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		}
		return
	}
	c.JSON(http.StatusOK, res)
}
