// Package user implements the /v1/me/* HTTP routes (authenticated user
// self-service). Each domain (apikeys / plans / stats / account-mutations)
// lives in its own sibling file; user.go owns the Handler struct,
// dependency wiring, route table, and the small "who am I" + quota peek
// handlers that don't fit any larger sub-domain.
package user

import (
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/cache"
	"github.com/jucode/jucode/internal/data/ent"
	entbillinghold "github.com/jucode/jucode/internal/data/ent/billinghold"
	"github.com/jucode/jucode/internal/middleware"
)

type Handler struct {
	users         *ent.Client
	apikeys       *biz.ApiKeySvc
	accessTokens  *biz.AccessTokenSvc
	oauth         *biz.OAuthSvc
	plans         *biz.PlanSvc
	redeem        *biz.RedeemSvc
	stats         *biz.StatsSvc
	models        *biz.ModelSvc
	account       *biz.Account
	tickets       *biz.TicketSvc
	roles         *biz.RoleSvc
	finance       *biz.FinanceSvc
	organizations *biz.OrganizationSvc
	cache         *cache.Cache
	// rateLimit is display-only: it resolves the caps shown beside the live
	// rate. nil simply means no caps are shown.
	rateLimit *biz.RateLimitGate
	// invites backs GET /invite. Optional (setter, not constructor arg);
	// nil returns 503 from that route only.
	invites *biz.InviteSvc
	agents  *biz.AgentSvc
	status  *biz.StatusSvc
}

// SetRateLimitGate wires the gate used to label the live-rate widget with the
// caller's effective caps. Optional, hence a setter rather than another
// argument on an already long constructor.
func (h *Handler) SetRateLimitGate(g *biz.RateLimitGate) { h.rateLimit = g }

// SetInvites wires the invite service in after construction.
func (h *Handler) SetInvites(inv *biz.InviteSvc) { h.invites = inv }

func (h *Handler) SetAgents(a *biz.AgentSvc) { h.agents = a }

// SetStatus wires the status service used to label available groups with
// their current running state. Optional: a nil service leaves every group
// as unknown rather than failing the picker.
func (h *Handler) SetStatus(s *biz.StatusSvc) { h.status = s }

// SetAccessTokens wires personal access token management (/v1/me/access-tokens).
func (h *Handler) SetAccessTokens(s *biz.AccessTokenSvc) { h.accessTokens = s }

func New(users *ent.Client, apikeys *biz.ApiKeySvc, oauth *biz.OAuthSvc, plans *biz.PlanSvc, redeem *biz.RedeemSvc, stats *biz.StatsSvc, models *biz.ModelSvc, account *biz.Account, tickets *biz.TicketSvc, roles *biz.RoleSvc, finance *biz.FinanceSvc, organizations *biz.OrganizationSvc, cacheSvc *cache.Cache) *Handler {
	return &Handler{users: users, apikeys: apikeys, oauth: oauth, plans: plans, redeem: redeem, stats: stats, models: models, account: account, tickets: tickets, roles: roles, finance: finance, organizations: organizations, cache: cacheSvc}
}

func (h *Handler) Mount(rg *gin.RouterGroup) {
	rg.GET("", h.handleMe)
	rg.PATCH("", h.handleUpdateMe)
	// Account security and credential management refuse personal access
	// tokens: a leaked token must not lock the owner out or mint more tokens.
	session := middleware.SessionOnly()
	rg.POST("/change-password", session, h.handleChangePassword)
	rg.POST("/change-email", session, h.handleChangeEmail)
	rg.POST("/send-email-verification", session, h.handleSendEmailVerification)
	rg.GET("/totp", h.handleTOTPStatus)
	rg.POST("/totp/setup", session, h.handleTOTPSetup)
	rg.POST("/totp/confirm", session, h.handleTOTPConfirm)
	rg.POST("/totp/disable", session, h.handleTOTPDisable)
	rg.GET("/access-tokens", session, h.listAccessTokens)
	rg.POST("/access-tokens", session, h.createAccessToken)
	rg.DELETE("/access-tokens/:id", session, h.revokeAccessToken)
	rg.GET("/usage", h.handleUsage)
	rg.POST("/banked-resets/:id/use", h.useBankedReset)
	rg.GET("/apikeys", h.listApiKeys)
	rg.POST("/apikeys", h.createApiKey)
	rg.POST("/lab-key", h.ensureLabKey)
	rg.PATCH("/apikeys/:id", h.updateApiKey)
	rg.POST("/apikeys/:id/rotate", h.rotateApiKey)
	rg.DELETE("/apikeys/:id", h.revokeApiKey)
	rg.GET("/apikeys/:id/reveal", h.revealApiKey)
	rg.GET("/devices", h.listDevices)
	rg.DELETE("/devices/:id", h.revokeDevice)
	rg.GET("/plans", h.listAvailablePlans)
	rg.POST("/redeem", h.redeemCode)
	rg.GET("/invite", h.handleMyInvite)
	rg.GET("/coupon/quote", h.handleCouponQuote)
	rg.GET("/stats/usage", h.handleMyStatsUsage)
	rg.GET("/stats/usage-by-model", h.handleMyStatsUsageByModel)
	rg.GET("/stats/usage-by-key", h.handleMyStatsUsageByKey)
	rg.GET("/stats/lifetime", h.handleMyStatsLifetime)
	rg.GET("/stats/rate", h.handleMyRate) // live RPM/TPM shown above the usage log
	rg.GET("/usage-logs", h.handleMyUsageLogs)
	rg.GET("/available-groups", h.listAvailableGroups)
	rg.GET("/billing/summary", h.handleMyBillingSummary)
	rg.GET("/billing", h.handleMyBilling)
	rg.GET("/organizations", h.listMyOrganizations)
	rg.GET("/organizations/:id", h.getMyOrganization)
	rg.GET("/organizations/:id/overview", h.getMyOrganizationOverview)
	rg.GET("/organizations/:id/billing/summary", h.getMyOrganizationBillingSummary)
	rg.GET("/organizations/:id/billing", h.listMyOrganizationBilling)
	rg.PATCH("/organizations/:id", h.updateMyOrganization)
	rg.POST("/organizations/:id/topup", h.topupMyOrganization)
	// Membership grants org management that outlives any token, and can
	// demote the owner, so it needs a signed-in session.
	rg.PUT("/organizations/:id/members/:uid", session, h.upsertMyOrganizationMember)
	rg.DELETE("/organizations/:id/members/:uid", session, h.removeMyOrganizationMember)
	rg.GET("/orders", h.listMyOrders)
	rg.POST("/orders", h.createMyOrder)
	rg.GET("/self-refund", h.getMySelfRefund)
	rg.GET("/orders/:id/refundable", h.getMyOrderRefundable)
	rg.POST("/self-refund", session, h.runMySelfRefund)
	rg.POST("/orders/:id/alipay/page-pay", h.createAlipayPagePay)
	rg.POST("/orders/:id/alipay/native", h.createAlipayPagePay)
	rg.POST("/orders/:id/sync", h.syncOrderPayment)
	rg.POST("/orders/:id/refunds", h.createMyRefund)
	rg.GET("/tickets", h.listMyTickets)
	rg.POST("/tickets", h.createMyTicket)
	rg.GET("/tickets/:id", h.getMyTicket)
	rg.GET("/tickets/:id/attachments/:file", h.getMyTicketAttachment)
	rg.POST("/tickets/:id/reply", h.replyMyTicket)
	rg.POST("/tickets/:id/close", h.closeMyTicket)
	rg.POST("/tickets/:id/reopen", h.reopenMyTicket)
}

func (h *Handler) handleMe(c *gin.Context) {
	uid, ok := middleware.UserIDFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "no user"})
		return
	}
	u, err := h.users.User.Get(c.Request.Context(), uid)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		return
	}
	resp := gin.H{
		"id":         u.ID.String(),
		"email":      u.Email,
		"role":       u.Role,
		"status":     u.Status,
		"balance":    u.Balance.String(),
		"currency":   u.Currency,
		"nickname":   u.Nickname,
		"created_at": u.CreatedAt,
	}
	held, err := h.heldAmount(c.Request.Context(), entbillinghold.And(
		entbillinghold.UserIDEQ(uid), entbillinghold.ScopeEQ(entbillinghold.ScopeUser)))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	resp["held_balance"] = held.String()
	if h.roles != nil {
		if rs, err := h.roles.ListForUser(c.Request.Context(), uid); err == nil {
			resp["roles"] = rs
		}
	}
	resp["totp_enabled"] = u.TotpEnabled
	// Privileged accounts (admin / cs / agent) that have not bound TOTP
	// yet: the frontend uses this to show the "please bind" nudge.
	resp["totp_setup_required"] = !u.TotpEnabled && h.account.TOTPRequiredForUser(c.Request.Context(), u)
	if up, plan, err := h.plans.ActiveUserPlan(c.Request.Context(), uid); err == nil {
		resp["active_plan"] = gin.H{
			"user_plan_id":        up.ID.String(),
			"plan_id":             plan.ID.String(),
			"name":                plan.Name,
			"type":                plan.Type,
			"start_at":            up.StartAt,
			"expire_at":           up.ExpireAt,
			"quota_5h":            plan.Quota5h.String(),
			"quota_weekly":        plan.QuotaWeekly.String(),
			"quota_monthly":       plan.QuotaMonthly.String(),
			"monthly_spend_limit": plan.MonthlySpendLimit.String(),
		}
	}
	c.JSON(http.StatusOK, resp)
}

type updateMeReq struct {
	Nickname *string `json:"nickname"`
}

func (h *Handler) handleUpdateMe(c *gin.Context) {
	uid, ok := middleware.UserIDFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "no user"})
		return
	}
	var req updateMeReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	q := h.users.User.UpdateOneID(uid)
	if req.Nickname != nil {
		// Empty string → clear (display falls back to email prefix).
		if *req.Nickname == "" {
			q = q.ClearNickname()
		} else {
			q = q.SetNickname(*req.Nickname)
		}
	}
	if _, err := q.Save(c.Request.Context()); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	// Defer to handleMe so the client gets the same shape it already reads.
	h.handleMe(c)
}

// handleUsage returns the calling user's active-plan quota snapshot
// (5h / weekly / monthly used vs cap). When no plan is active it
// returns has_active_plan=false with zeros so the UI doesn't need to
// special-case the 404.
func (h *Handler) handleUsage(c *gin.Context) {
	uid, ok := middleware.UserIDFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "no user"})
		return
	}
	up, plan, err := h.plans.ActiveUserPlan(c.Request.Context(), uid)
	if err != nil {
		// No active plan — return zeros so frontend can render without 404.
		c.JSON(http.StatusOK, gin.H{
			"has_active_plan": false,
		})
		return
	}
	view, err := h.plans.PeekQuota(c.Request.Context(), up)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	paygoUsed := "0"
	if plan.Type == "pay_as_you_go" {
		used, err := h.plans.PayAsYouGoUsageViewThisMonth(c.Request.Context(), up, plan)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		paygoUsed = used.String()
	}
	banked, err := h.plans.UnusedBankedResets(c.Request.Context(), up.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	bankedResets := make([]gin.H, 0, len(banked[up.ID]))
	for _, r := range banked[up.ID] {
		bankedResets = append(bankedResets, gin.H{"id": r.ID, "window": r.Window, "created_at": r.CreatedAt})
	}
	windows := biz.QuotaWindowsAt(up, time.Now())
	c.JSON(http.StatusOK, gin.H{
		"has_active_plan":       true,
		"plan_name":             plan.Name,
		"plan_type":             plan.Type,
		"expire_at":             up.ExpireAt,
		"currency":              plan.Currency,
		"quota_5h":              plan.Quota5h.String(),
		"used_5h":               view.Used5h.String(),
		"quota_weekly":          plan.QuotaWeekly.String(),
		"used_weekly":           view.UsedWeekly.String(),
		"quota_monthly":         plan.QuotaMonthly.String(),
		"used_monthly":          view.UsedMonthly.String(),
		"reset_5h_at":           windows[0].End,
		"reset_weekly_at":       windows[1].End,
		"reset_monthly_at":      windows[2].End,
		"monthly_spend_limit":   plan.MonthlySpendLimit.String(),
		"monthly_spend_used":    paygoUsed,
		"auto_reload_enabled":   plan.AutoReloadEnabled,
		"auto_reload_threshold": plan.AutoReloadThreshold.String(),
		"auto_reload_amount":    plan.AutoReloadAmount.String(),
		"banked_resets":         bankedResets,
	})
}

func (h *Handler) getMySelfRefund(c *gin.Context) {
	uid, ok := middleware.UserIDFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "no user"})
		return
	}
	plan, err := h.finance.PlanSelfRefund(c.Request.Context(), uid)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, plan)
}

func (h *Handler) runMySelfRefund(c *gin.Context) {
	uid, ok := middleware.UserIDFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "no user"})
		return
	}
	res, err := h.finance.ExecuteSelfRefund(c.Request.Context(), uid)
	if err != nil {
		status := http.StatusInternalServerError
		switch {
		case errors.Is(err, biz.ErrSelfRefundDisabled), errors.Is(err, biz.ErrRefundGatewayUnavailable):
			status = http.StatusForbidden
		case errors.Is(err, biz.ErrSelfRefundBusy), errors.Is(err, biz.ErrSelfRefundUnresolved):
			status = http.StatusConflict
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, res)
}

func (h *Handler) getMyOrderRefundable(c *gin.Context) {
	uid, ok := middleware.UserIDFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "no user"})
		return
	}
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	o, err := h.users.SiteOrder.Get(c.Request.Context(), id)
	if err != nil || o.UserID != uid {
		c.JSON(http.StatusNotFound, gin.H{"error": "order not found"})
		return
	}
	ref, err := h.finance.OrderRefundableFor(c.Request.Context(), o)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, ref)
}
