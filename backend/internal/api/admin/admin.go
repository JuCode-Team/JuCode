// Package admin implements /v1/admin/* HTTP routes. All routes are gated
// by the JWT + AdminOnly middleware chain at the caller (router.go).
//
// Each business domain lives in its own admin_<domain>.go file (users,
// providers, plans, models, model_groups, user_groups, vendors, redeem,
// settings, stats, policy, web_vitals,
// This file owns the shared Handler struct, dependency
// wiring, the central Mount() that registers every route, and the small
// helpers that every domain handler reaches for (parsePage / parseUUID /
// notFoundOr).
package admin

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/cache"
	"github.com/jucode/jucode/internal/data/ent"
	"github.com/jucode/jucode/internal/middleware"
)

type Handler struct {
	// accounts is the account-pool service: the single upstream entity
	// (endpoint, dialect, credential, scheduling, health, quota). It
	// replaced the old provider/channel layer (ProviderSvc and the
	// provider-account layer). Nil hides the /accounts routes.
	accounts  *biz.AccountPoolSvc
	dashboard *biz.DashboardSvc
	models    *biz.ModelSvc
	plans     *biz.PlanSvc
	redeem    *biz.RedeemSvc
	stats     *biz.StatsSvc
	settings  *biz.SettingsSvc
	apikeys   *biz.ApiKeySvc
	policy    *biz.PolicySvc
	incidents *biz.IncidentSvc
	audit     *biz.AuditSvc
	// account is used to invalidate the JWT auth-status cache when
	// admin mutates user.status (ban / approve / reject / delete /
	// PATCH). Without this the cached "active" flag can lag up to 60s
	// after a ban.
	account       *biz.Account
	ipPolicy      *biz.IPPolicySvc
	roles         *biz.RoleSvc
	agents        *biz.AgentSvc
	finance       *biz.FinanceSvc
	organizations *biz.OrganizationSvc
	cache         *cache.Cache
	// invites is wired post-construction via SetInvites. Nil disables the
	// /invite/* routes' actions gracefully.
	invites *biz.InviteSvc
	// stepUp guards the sensitive mutations (balance topup, refund
	// approval, redeem-code minting) with a fresh TOTP check. Wired via
	// SetStepUpGuard; nil = no gate (tests).
	stepUp gin.HandlerFunc
	users  *ent.Client
	// rawDB is the *sql.DB underlying ent — used for raw SQL queries
	// that ent's typed API can't express (percentile_cont in particular).
	rawDB           *sql.DB
	siteAssetDir    string
	systemIncidents *biz.SystemIncidentSvc
	// providerDefinitions carries the per-vendor definition payloads. Wired
	// via SetProviderDefinitions; nil hides those routes.
	providerDefinitions *biz.ProviderDefinitionSvc
	// webTools holds the web tool engines' prices and switches. Wired via
	// SetWebTools; nil hides the /web-tools routes.
	webTools *biz.WebToolSvc
	// remoteCatalog is the cached public model catalog (models.dev). Wired
	// via SetRemoteCatalog; nil hides the /model-catalog routes.
	remoteCatalog *biz.RemoteCatalogSvc
}

func New(accounts *biz.AccountPoolSvc, dashboard *biz.DashboardSvc, models *biz.ModelSvc, plans *biz.PlanSvc, redeem *biz.RedeemSvc, stats *biz.StatsSvc, settings *biz.SettingsSvc, apikeys *biz.ApiKeySvc, policy *biz.PolicySvc, incidents *biz.IncidentSvc, audit *biz.AuditSvc, account *biz.Account, ipPolicy *biz.IPPolicySvc, roles *biz.RoleSvc, agents *biz.AgentSvc, finance *biz.FinanceSvc, organizations *biz.OrganizationSvc, cacheSvc *cache.Cache, users *ent.Client, rawDB *sql.DB) *Handler {
	return &Handler{accounts: accounts, dashboard: dashboard, models: models, plans: plans, redeem: redeem, stats: stats, settings: settings, apikeys: apikeys, policy: policy, incidents: incidents, audit: audit, account: account, ipPolicy: ipPolicy, roles: roles, agents: agents, finance: finance, organizations: organizations, cache: cacheSvc, users: users, rawDB: rawDB}
}

// SetInvites wires the invite service in after construction.
func (h *Handler) SetInvites(inv *biz.InviteSvc) { h.invites = inv }

// SetStepUpGuard wires the TOTP step-up middleware for sensitive routes.
func (h *Handler) SetStepUpGuard(fn gin.HandlerFunc) { h.stepUp = fn }

// stepUpGuard returns the wired step-up middleware, or a pass-through
// when none was set (unit tests).
//
// What belongs behind it: anything that mints or moves money (topup,
// settle, mark-paid, redeem-code generation), grants privilege
// (role changes), or takes over an account (password reset). The gate is
// only as strong as its weakest equivalent path — PATCH /users/:id used to
// accept a `balance` field and was the exact same capability as the guarded
// topup endpoint, which made the guard decorative. When adding an endpoint,
// ask what it lets an operator walk away with, not which file it lives in.
//
// What deliberately stays out: reads, and writes whose outcome is decided
// upstream rather than by the operator (e.g. syncing an order against the
// payment gateway — it reports what the gateway says and cannot conjure a
// payment). Over-guarding trains operators to turn 2FA off.
func (h *Handler) stepUpGuard() gin.HandlerFunc {
	if h.stepUp != nil {
		return h.stepUp
	}
	return func(c *gin.Context) { c.Next() }
}

func (h *Handler) SetSystemIncidents(svc *biz.SystemIncidentSvc) { h.systemIncidents = svc }

func (h *Handler) Mount(rg *gin.RouterGroup) {
	rg.GET("/stats/overview", h.handleOverview)
	rg.GET("/stats/usage", h.handleStatsUsage)
	rg.GET("/stats/breakdown", h.handleStatsBreakdown)
	rg.GET("/stats/rate", h.handleStatsRate) // platform-wide live RPM/TPM
	// Observability: read-only traffic/SLA aggregations (admin_observability.go).
	rg.GET("/observability/series", h.handleObservabilitySeries)
	rg.GET("/observability/leaderboard", h.handleObservabilityLeaderboard)
	rg.GET("/observability/sla", h.handleObservabilitySLA)
	rg.GET("/observability/provider-attempts", h.handleObservabilityProviderAttempts)
	rg.GET("/observability/load-map", h.handleObservabilityLoadMap)
	rg.GET("/observability/errors", h.handleObservabilityErrors)
	rg.GET("/model-catalog/providers", h.listCatalogProviders)
	rg.GET("/model-catalog/providers/:pid/models", h.listCatalogModels)
	rg.GET("/model-catalog/models", h.listCanonicalCatalogModels)
	rg.GET("/routing/board", h.handleRoutingBoard)
	rg.GET("/routing/preview", h.handleRoutingPreview)
	rg.GET("/users", h.listUsers)
	rg.GET("/users/:id", h.getUserDetail)
	rg.PATCH("/users/:id", h.updateUser)
	rg.DELETE("/users/:id", h.deleteUser)
	rg.POST("/users/:id/approve", h.approveUser)
	rg.POST("/users/:id/reject", h.rejectUser)
	rg.POST("/users/:id/reset-password", h.stepUpGuard(), h.resetUserPassword)
	rg.PUT("/users/:id/roles", h.stepUpGuard(), h.setUserRoles)
	rg.GET("/users/:id/plans", h.listUserPlans)
	rg.POST("/users/:id/plans", h.stepUpGuard(), h.grantUserPlan)
	rg.PATCH("/users/:id/plans/:upid", h.stepUpGuard(), h.updateUserPlan)
	rg.DELETE("/users/:id/plans/:upid", h.stepUpGuard(), h.deleteUserPlan)
	rg.POST("/users/:id/plans/:upid/reset-quota", h.stepUpGuard(), h.resetUserPlanQuota)
	rg.GET("/user-plans", h.listResettableUserPlans)
	rg.POST("/user-plans/reset", h.stepUpGuard(), h.startPlanReset)
	rg.GET("/user-plans/reset-jobs", h.listPlanResetJobs)
	rg.GET("/users/:id/apikeys", h.listUserApikeys)
	rg.GET("/users/:id/apikeys/:kid/reveal", h.revealUserApikey)
	rg.POST("/users/:id/apikeys", h.createUserApikey)
	rg.PATCH("/users/:id/apikeys/:kid", h.updateUserApikey)
	rg.DELETE("/users/:id/apikeys/:kid", h.revokeUserApikey)
	rg.GET("/organizations", h.listOrganizations)
	rg.POST("/organizations", h.createOrganization)
	rg.GET("/organizations/:id", h.getOrganization)
	rg.PATCH("/organizations/:id", h.updateOrganization)
	rg.POST("/organizations/:id/topup", h.stepUpGuard(), h.topupOrganization)
	// Membership and ownership grant org management that outlives any
	// token; updateOrganization refuses owner changes from tokens itself.
	rg.PUT("/organizations/:id/members/:uid", middleware.SessionOnly(), h.upsertOrganizationMember)
	rg.DELETE("/organizations/:id/members/:uid", middleware.SessionOnly(), h.removeOrganizationMember)
	rg.GET("/usage-logs", h.listUsageLogs)
	h.mountSystemIncidents(rg)

	rg.GET("/settings", h.getSiteSettings)
	// Site settings include SMTP and registration: pointing password-reset
	// mail elsewhere yields logins that outlive any token.
	rg.PUT("/settings", middleware.SessionOnly(), h.updateSiteSettings)
	rg.POST("/site-assets", h.uploadSiteAsset)

	// IP policy: trusted proxies + allow/block lists (PR-1).
	rg.GET("/ip-policies", h.listIPPolicies)
	rg.POST("/ip-policies", h.createIPPolicy)
	rg.DELETE("/ip-policies/:id", h.deleteIPPolicy)

	// Account pool (/v1/admin/accounts*): the single upstream entity.
	h.mountAccounts(rg)
	h.mountProviderDefinitions(rg)
	h.mountWebTools(rg)
	h.mountAccountPools(rg)
	// Per-model-group account bindings (/model-groups/:id/accounts*).
	h.mountGroupAccounts(rg)

	// Legacy read-only provider surface (kept for one release): GET
	// projects accounts, and every old write endpoint answers 410 Gone
	// pointing at /v1/admin/accounts.
	rg.GET("/providers", h.listLegacyProviders)
	rg.GET("/providers/:id", h.getLegacyProvider)
	h.mountProviderWriteGone(rg)

	rg.GET("/plans", h.listPlans)
	rg.POST("/plans", h.createPlan)
	rg.PATCH("/plans/:id", h.updatePlan)
	rg.DELETE("/plans/:id", h.deletePlan)

	rg.GET("/redeem-codes", h.listRedeemCodes)
	rg.POST("/redeem-codes", h.stepUpGuard(), h.generateRedeemCodes)
	rg.POST("/redeem-codes/:id/revoke", h.revokeRedeemCode)
	rg.POST("/redeem-codes/batch/:batch_id/revoke", h.revokeRedeemBatch)

	rg.POST("/users/:id/topup", h.stepUpGuard(), h.adminTopupUser)

	rg.GET("/invite/bindings", h.listInviteBindings)
	rg.GET("/invite/rewards", h.listInviteRewards)
	rg.GET("/invite/rebates", h.listInviteRebates)
	rg.POST("/invite/rewards/:id/pay", h.payInviteReward)
	rg.POST("/invite/rewards/:id/reject", h.rejectInviteReward)
	rg.POST("/invite/rebates/:id/pay", h.payInviteRebate)
	rg.POST("/invite/rebates/:id/reject", h.rejectInviteRebate)
	rg.POST("/invite/bindings/:id/void", h.voidInviteBinding)

	rg.GET("/models", h.listModels)
	rg.POST("/models", h.createModel)
	rg.PATCH("/models/:id", h.updateModel)
	rg.DELETE("/models/:id", h.deleteModel)
	rg.POST("/models/reorder", h.reorderModels)
	rg.POST("/models/import", h.importPlatformModels)

	rg.GET("/vendors", h.listVendors)
	rg.POST("/vendors", h.createVendor)
	rg.PATCH("/vendors/:id", h.updateVendor)
	rg.DELETE("/vendors/:id", h.deleteVendor)

	rg.GET("/model-groups", h.listGroups)
	rg.POST("/model-groups", h.createGroup)
	rg.PATCH("/model-groups/:id", h.updateGroup)
	rg.DELETE("/model-groups/:id", h.deleteGroup)
	rg.POST("/model-groups/reorder", h.reorderGroups)

	rg.GET("/user-groups", h.listUserGroups)
	rg.POST("/user-groups", h.createUserGroup)
	rg.PATCH("/user-groups/:id", h.updateUserGroup)
	rg.DELETE("/user-groups/:id", h.deleteUserGroup)
	rg.POST("/user-groups/:id/set-default", h.setDefaultUserGroup)
	rg.GET("/user-groups/:id/model-group-grants", h.getUserGroupModelGroupGrants)
	rg.PUT("/user-groups/:id/model-group-grants", h.putUserGroupModelGroupGrants)
	rg.GET("/users/:id/model-group-grants", h.getUserModelGroupGrants)
	rg.PUT("/users/:id/model-group-grants", h.putUserModelGroupGrants)
	rg.POST("/model-groups/:id/models/:mid", h.attachModelToGroup)
	rg.PATCH("/model-groups/:id/models/:mid", h.setModelProbe)
	rg.DELETE("/model-groups/:id/models/:mid", h.detachModelFromGroup)
	rg.POST("/model-groups/:id/providers/:pid", h.attachProviderToGroup)
	rg.DELETE("/model-groups/:id/providers/:pid", h.detachProviderFromGroup)
	rg.POST("/model-groups/:id/clone", h.cloneGroup)
	rg.GET("/model-groups/:id/grants", h.getGroupGrants)
	rg.PUT("/model-groups/:id/grants", h.putGroupGrants)

	rg.GET("/web-vitals", h.listWebVitals)

	rg.GET("/agents", h.listAgents)
	rg.PATCH("/agents/:id", h.updateAgent)
	rg.GET("/agents/:id/entries", h.getAgentEntries)
	rg.GET("/agents/:id/refunds", h.getAgentRefunds)
	rg.GET("/agents/:id/settlements", h.getAgentSettlements)
	rg.POST("/agents/:id/settle", h.stepUpGuard(), h.settleAgent)

	rg.GET("/finance/summary", h.financeSummary)
	rg.GET("/finance/refund-policy", h.getRefundPolicy)
	rg.PUT("/finance/refund-policy", h.updateRefundPolicy)
	rg.GET("/finance/provider-costs", h.listProviderCosts)
	rg.POST("/finance/provider-costs", h.createProviderCost)
	rg.DELETE("/finance/provider-costs/:id", h.deleteProviderCost)
	rg.GET("/finance/provider-settlements", h.listProviderSettlements)
	rg.POST("/finance/provider-settlements/:provider_id/confirm", h.stepUpGuard(), h.confirmProviderSettlement)
	rg.GET("/finance/orders", h.listOrders)
	rg.POST("/finance/orders", h.createOrder)
	rg.POST("/finance/orders/:id/pay", h.stepUpGuard(), h.markOrderPaid)
	rg.POST("/finance/orders/:id/sync", h.syncOrderPayment)
	rg.POST("/finance/orders/:id/cancel", h.cancelOrder)
	rg.POST("/finance/orders/:id/refunds", h.createRefund)
	rg.GET("/finance/orders/:id/refundable", h.orderRefundable)
	rg.GET("/finance/refunds", h.listRefunds)
	rg.POST("/finance/refunds/:id/approve", h.stepUpGuard(), h.approveRefund)
	rg.POST("/finance/refunds/:id/reject", h.stepUpGuard(), h.rejectRefund)

	rg.GET("/ops", h.opsReport)
	rg.GET("/ops/export", h.opsExport)

	// Policy moderation + user incident + admin audit. Mounted as a
	// separate group so admin_policy.go owns the routing as well as
	// the handlers (less surface to mis-wire).
	h.mountPolicy(rg)
}

// handleOverview is the only handler that lives here — it's a one-liner
// shared by no other domain. Everything larger goes into a sibling
// admin_<domain>.go.
func (h *Handler) handleOverview(c *gin.Context) {
	o, hit, err := cache.GetOrCompute(c.Request.Context(), h.cache, cache.KeyAdminOverview, 30*time.Second, func(ctx context.Context) (*biz.Overview, error) {
		return h.dashboard.GetOverview(ctx)
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	setCacheHeader(c, hit)
	c.JSON(http.StatusOK, o)
}

// --- shared helpers used by every admin_<domain>.go ---

// setCacheHeader writes X-Cache: HIT|MISS for the just-served response.
// Mirrors the helper of the same name in package api (router.go).
func setCacheHeader(c *gin.Context, hit bool) {
	if hit {
		c.Header("X-Cache", "HIT")
	} else {
		c.Header("X-Cache", "MISS")
	}
}

// parsePage extracts limit (clamped to [1,max]) and offset (>=0) from query.
func parsePage(c *gin.Context, maxLimit int) (limit, offset int) {
	limit = 50
	if v := c.Query("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			if n > maxLimit {
				n = maxLimit
			}
			limit = n
		}
	}
	if v := c.Query("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			offset = n
		}
	}
	return
}

// parseUUID extracts and validates a UUID route parameter. On invalid
// input it writes a 400 response and returns ok=false — callers should
// return immediately.
func parseUUID(c *gin.Context, name string) (uuid.UUID, bool) {
	id, err := uuid.Parse(c.Param(name))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid " + name})
		return uuid.Nil, false
	}
	return id, true
}

// notFoundOr maps the well-known "not found" sentinel errors to 404 and
// everything else to 500.
func notFoundOr(c *gin.Context, err error) {
	if errors.Is(err, biz.ErrProviderNotFound) || errors.Is(err, biz.ErrAccountNotFound) || errors.Is(err, biz.ErrModelNotFound) || errors.Is(err, biz.ErrPlanNotFound) || errors.Is(err, biz.ErrGroupNotFound) || ent.IsNotFound(err) {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
}

func uuidStrings(ids []uuid.UUID) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, id.String())
	}
	return out
}

func parseUUIDList(c *gin.Context, raw []string) ([]uuid.UUID, bool) {
	out := make([]uuid.UUID, 0, len(raw))
	for _, s := range raw {
		id, err := uuid.Parse(s)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id: " + s})
			return nil, false
		}
		out = append(out, id)
	}
	return out, true
}
