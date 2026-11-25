package openapi

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/middleware"
)

// GET /v1/open/balance — everything that can stop your next request from
// going through, in one response.
//
// A key holder who is about to be cut off is cut off for one of three
// reasons: the account ran out of 积分, the subscription window's quota is
// exhausted, or the key's own cost cap was hit. Those live in three different
// stores (Postgres, Redis quota counters, an aggregate over usage_logs), and
// splitting them across three endpoints would just mean every client makes
// three calls and reassembles them. So they are assembled here.
//
// Money is a decimal string throughout — the ledger is numeric(20,8), and
// round-tripping that through a float loses digits that matter when you are
// comparing a spend against a limit.
func (h *Handler) handleBalance(c *gin.Context) {
	uid, ok := requireUser(c)
	if !ok {
		return
	}
	ctx := c.Request.Context()

	balance, err := h.account.Balance(ctx, uid)
	if err != nil {
		fail(c, http.StatusInternalServerError, err.Error())
		return
	}
	spend, err := h.lifetimeSpend(ctx, uid)
	if err != nil {
		fail(c, http.StatusInternalServerError, err.Error())
		return
	}

	out := gin.H{
		"currency":       biz.CurrencyCNY,
		"balance":        decStr(balance),
		"lifetime_spend": decStr(spend),
		"plan":           h.planView(c),
		"api_key":        h.keyLimitView(c),
	}
	c.JSON(http.StatusOK, out)
}

// planView is the active subscription's quota windows, or has_active_plan
// false when the caller is pay-as-you-go off their balance.
//
// A plan error is reported as "no active plan" rather than as a 500: the
// quota counters live in Redis, and a Redis blip should degrade this endpoint
// to "here is your balance" instead of failing the whole response.
func (h *Handler) planView(c *gin.Context) gin.H {
	uid, ok := middleware.UserIDFrom(c)
	if !ok {
		return gin.H{"has_active_plan": false}
	}
	ctx := c.Request.Context()
	up, plan, err := h.plans.ActiveUserPlan(ctx, uid)
	if err != nil {
		return gin.H{"has_active_plan": false}
	}
	view, err := h.plans.PeekQuota(ctx, up)
	if err != nil {
		return gin.H{"has_active_plan": false}
	}
	windows := biz.QuotaWindowsAt(up, time.Now())
	out := gin.H{
		"has_active_plan":  true,
		"name":             plan.Name,
		"type":             plan.Type,
		"expire_at":        up.ExpireAt,
		"quota_5h":         decStr(plan.Quota5h),
		"used_5h":          decStr(view.Used5h),
		"quota_weekly":     decStr(plan.QuotaWeekly),
		"used_weekly":      decStr(view.UsedWeekly),
		"quota_monthly":    decStr(plan.QuotaMonthly),
		"used_monthly":     decStr(view.UsedMonthly),
		"reset_5h_at":      windows[0].End,
		"reset_weekly_at":  windows[1].End,
		"reset_monthly_at": windows[2].End,
	}
	if plan.Type == "pay_as_you_go" {
		used, err := h.plans.PayAsYouGoUsageViewThisMonth(ctx, up, plan)
		if err == nil {
			out["monthly_spend_limit"] = decStr(plan.MonthlySpendLimit)
			out["monthly_spend_used"] = decStr(used)
		}
	}
	return out
}

// keyLimitView is the calling key's own caps and how much of each is spent.
// Nil for OAuth device tokens, which have no key record to report — see
// callerKey.
func (h *Handler) keyLimitView(c *gin.Context) gin.H {
	key := callerKey(c)
	if key == nil {
		return nil
	}
	out := gin.H{
		"name":               key.Name,
		"prefix":             key.Prefix,
		"daily_cost_limit":   decPtrStr(key.DailyCostLimit),
		"monthly_cost_limit": decPtrStr(key.MonthlyCostLimit),
		"total_cost_limit":   decPtrStr(key.TotalCostLimit),
	}
	if key.ExpiresAt != nil {
		out["expires_at"] = key.ExpiresAt
	}
	// The spend figures cost a query, so only run it when at least one cap
	// exists to compare them against.
	if key.DailyCostLimit != nil || key.MonthlyCostLimit != nil || key.TotalCostLimit != nil {
		if u, err := h.keyCostUsage(c.Request.Context(), key.ID); err == nil {
			out["daily_cost_used"] = decStr(u.Daily)
			out["monthly_cost_used"] = decStr(u.Monthly)
			out["total_cost_used"] = decStr(u.Total)
		}
	}
	return out
}

// GET /v1/open/key — what this credential is and what it may do.
//
// Deliberately never echoes the secret or its hash: the caller already has
// the secret, and a response that repeats it turns every proxy log into a
// credential leak.
func (h *Handler) handleKey(c *gin.Context) {
	if _, ok := requireUser(c); !ok {
		return
	}
	key := callerKey(c)
	if key == nil {
		// An OAuth device token. Saying so is more useful than a 404,
		// because the caller's next question is "why is this empty".
		c.JSON(http.StatusOK, gin.H{
			"auth_type": "oauth_device_token",
			"note":      "this credential is an OAuth device token, not an API key; it has no key-level limits",
		})
		return
	}
	// A nil slice marshals to null, and the docs promise an empty array means
	// "unrestricted" — null would read as a third, undefined state.
	ipWhitelist := key.IPWhitelist
	if ipWhitelist == nil {
		ipWhitelist = []string{}
	}
	out := gin.H{
		"auth_type":          "api_key",
		"name":               key.Name,
		"prefix":             key.Prefix,
		"created_at":         key.CreatedAt,
		"last_used_at":       key.LastUsedAt,
		"expires_at":         key.ExpiresAt,
		"auto_route":         key.AutoRoute,
		"billing_scope":      key.BillingScope,
		"ip_whitelist":       ipWhitelist,
		"daily_cost_limit":   decPtrStr(key.DailyCostLimit),
		"monthly_cost_limit": decPtrStr(key.MonthlyCostLimit),
		"total_cost_limit":   decPtrStr(key.TotalCostLimit),
	}
	// allowed_groups is empty when the key is unrestricted, which reads
	// identically to "restricted to nothing". Say which it is.
	names, err := h.keyGroupNames(c, key.AllowedGroupIds)
	if err == nil {
		out["allowed_groups"] = names
		out["allowed_groups_unrestricted"] = len(key.AllowedGroupIds) == 0
	}
	c.JSON(http.StatusOK, out)
}

func (h *Handler) keyGroupNames(c *gin.Context, ids []uuid.UUID) ([]string, error) {
	if len(ids) == 0 {
		return []string{}, nil
	}
	groups, err := h.models.ListGroups(c.Request.Context())
	if err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, id := range ids {
		want[id.String()] = true
	}
	out := make([]string, 0, len(ids))
	for _, g := range groups {
		if want[g.ID.String()] {
			out = append(out, g.Name)
		}
	}
	return out, nil
}

// GET /v1/open/groups — the model groups this caller may route to, with the
// rate multiplier each one applies to the base price.
func (h *Handler) handleGroups(c *gin.Context) {
	if _, ok := requireUser(c); !ok {
		return
	}
	user, _ := middleware.UserFrom(c)
	groups, err := h.models.AccessibleGroups(c.Request.Context(), user)
	if err != nil {
		fail(c, http.StatusInternalServerError, err.Error())
		return
	}
	key := callerKey(c)
	allowed := map[string]bool{}
	if key != nil {
		for _, id := range key.AllowedGroupIds {
			allowed[id.String()] = true
		}
	}
	ids := make([]uuid.UUID, 0, len(groups))
	for _, g := range groups {
		ids = append(ids, g.ID)
	}
	models, err := h.models.GroupModelNames(c.Request.Context(), ids)
	if err != nil {
		fail(c, http.StatusInternalServerError, err.Error())
		return
	}
	// Each model's context window through each group (a group's channel may
	// give it another one than the rest), as /v1/models computes it.
	ms, groupsByModel, err := h.models.AccessibleModelGroups(c.Request.Context(), user, key)
	if err != nil {
		fail(c, http.StatusInternalServerError, err.Error())
		return
	}
	caps, err := h.models.ModelCapabilities(c.Request.Context(), ms, groupsByModel)
	if err != nil {
		fail(c, http.StatusInternalServerError, err.Error())
		return
	}
	windows := map[uuid.UUID]gin.H{}
	for _, m := range ms {
		for id, w := range caps[m.ID].Groups {
			if windows[id] == nil {
				windows[id] = gin.H{}
			}
			windows[id][m.Name] = gin.H{"context_window": w.ContextWindow, "max_context_window": w.MaxContextWindow}
		}
	}
	out := make([]gin.H, 0, len(groups))
	for _, g := range groups {
		// A key with an explicit allow-list narrows what the account itself
		// could reach. Report the intersection, since that is what a request
		// on this credential would actually be routed to.
		if len(allowed) > 0 && !allowed[g.ID.String()] {
			continue
		}
		out = append(out, gin.H{
			"id":              g.ID.String(),
			"name":            g.Name,
			"description":     g.Description,
			"billing_source":  g.BillingSource,
			"rate_multiplier": g.RateMultiplier,
			"models":          models[g.ID],
			"context_windows": windows[g.ID],
		})
	}
	c.JSON(http.StatusOK, gin.H{"groups": out})
}
