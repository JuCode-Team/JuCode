// Package api wires every HTTP route group into a single gin.Engine.
package api

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"runtime/debug"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/api/admin"
	apiagent "github.com/jucode/jucode/internal/api/agent"
	"github.com/jucode/jucode/internal/api/auth"
	"github.com/jucode/jucode/internal/api/cs"
	apillm "github.com/jucode/jucode/internal/api/llm"
	apioauth "github.com/jucode/jucode/internal/api/oauth"
	apiopenapi "github.com/jucode/jucode/internal/api/openapi"
	"github.com/jucode/jucode/internal/api/user"
	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/cache"
	"github.com/jucode/jucode/internal/data"
	"github.com/jucode/jucode/internal/data/ent"
	entgroup "github.com/jucode/jucode/internal/data/ent/modelgroup"
	"github.com/jucode/jucode/internal/llm"
	"github.com/jucode/jucode/internal/metrics"
	"github.com/jucode/jucode/internal/middleware"
	jwtpkg "github.com/jucode/jucode/internal/pkg/jwt"
	regionpkg "github.com/jucode/jucode/internal/region"
	regioncontrol "github.com/jucode/jucode/internal/region/control"
	"github.com/jucode/jucode/internal/relay"
)

type Deps struct {
	Data      *data.Data
	Signer    *jwtpkg.Signer
	Account   *biz.Account
	ApiKeySvc *biz.ApiKeySvc
	OAuth     *biz.OAuthSvc
	// AccountPool is the account layer: the single upstream entity (endpoint,
	// dialect, credential, scheduling, health, quota). It replaced the old
	// provider/channel layer. Optional: nil hides the /accounts admin routes.
	AccountPool *biz.AccountPoolSvc
	// WebTools holds the web tool engines' prices and switches and picks their
	// vendor accounts. Optional: nil hides the admin page and the public prices.
	WebTools *biz.WebToolSvc
	// ProviderDefinitions carries the per-vendor payloads. Optional: nil hides
	// the definition admin routes.
	ProviderDefinitions *biz.ProviderDefinitionSvc
	// RemoteCatalog is the cached public model catalog (models.dev).
	// Optional: nil hides the /model-catalog routes.
	RemoteCatalog   *biz.RemoteCatalogSvc
	Dashboard       *biz.DashboardSvc
	Models          *biz.ModelSvc
	Plans           *biz.PlanSvc
	Redeem          *biz.RedeemSvc
	Stats           *biz.StatsSvc
	Settings        *biz.SettingsSvc
	Status          *biz.StatusSvc
	SystemIncidents *biz.SystemIncidentSvc
	Policy          *biz.PolicySvc
	Incidents       *biz.IncidentSvc
	Audit           *biz.AuditSvc
	Billing         *biz.BillingSvc
	Gateway         *llm.Gateway
	IPPolicy        *biz.IPPolicySvc
	Roles           *biz.RoleSvc
	Tickets         *biz.TicketSvc
	Agents          *biz.AgentSvc
	Finance         *biz.FinanceSvc
	Alipay          PaymentCallbackParser
	Organizations   *biz.OrganizationSvc
	Cache           *cache.Cache
	Drain           *biz.DrainSvc
	Invites         *biz.InviteSvc
	RateLimit       *biz.RateLimitGate // optional; only used to display effective caps
	// SiteAssetDir holds admin-uploaded site images (such as the contact
	// group's QR code).
	// main puts it inside the ticket attachment dir because production
	// bind-mounts that directory into both rolling instances.
	SiteAssetDir  string
	RegionControl *regioncontrol.Handler // nil unless config region.enabled
	// TrustedProxies is the final merged CIDR list (loopback + CF +
	// config + DB) resolved at boot. NewRouter applies it via
	// gin.SetTrustedProxies so c.ClientIP() honors X-Forwarded-For
	// from these peers only.
	TrustedProxies []string
	// OAuthClients are the first-party OAuth clients (config oauth.*).
	OAuthClients apioauth.Clients
	// Computers and Relay: the account's desktops and the relay that reaches
	// them (/v1/relay). Optional: a nil Relay hides the relay routes.
	Computers *biz.ComputerSvc
	Relay     *relay.Server
}

type PaymentCallbackParser interface {
	ParseCallback(headers http.Header, body []byte) (*biz.PaymentCallbackResult, error)
}

// recoverWithoutRequestDump replaces gin.Recovery, which dumps the raw request
// on a panic (and on every broken pipe) with only the Authorization header
// masked. This gateway carries the caller's key in three other places a dump
// would expose verbatim — the api_key query parameter, x-api-key, and the
// openai-insecure-api-key.<key> WebSocket subprotocol — and clients dropping
// mid-stream make broken pipes routine rather than exceptional.
//
// Method and route pattern are safe to log; the query string and headers are
// not, so neither is recorded.
func recoverWithoutRequestDump() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			// ErrAbortHandler is a request to tear the connection down, not
			// an error to report. Swallowing it would let net/http finish the
			// response normally, which tells a streaming client the stream
			// ended cleanly when it was actually cut short; re-panic so the
			// server aborts the connection (it suppresses the stack itself).
			if err, ok := rec.(error); ok && errors.Is(err, http.ErrAbortHandler) {
				panic(rec)
			}
			// A dead peer cannot be answered, and the panic is its
			// disconnect rather than a bug: close the connection quietly.
			if brokenPipe(rec) {
				_ = c.Error(fmt.Errorf("broken pipe: %v", rec))
				c.Abort()
				return
			}
			slog.Error("panic recovered",
				"method", c.Request.Method,
				"route", c.FullPath(),
				"request_id", middleware.RequestIDFrom(c),
				"panic", fmt.Sprint(rec),
				"stack", string(debug.Stack()))
			c.AbortWithStatus(http.StatusInternalServerError)
		}()
		c.Next()
	}
}

// peerGoneMessages are matched against the syscall error text because the
// portable errno constants do not line up: a reset arrives as EPIPE/ECONNRESET
// on unix but as WSAECONNRESET/WSAECONNABORTED on Windows, and neither
// satisfies errors.Is(err, syscall.ECONNRESET) there.
var peerGoneMessages = []string{
	"broken pipe",
	"connection reset by peer",
	"forcibly closed by the remote host",
	"aborted by the software in your host machine",
}

func brokenPipe(rec any) bool {
	ne, ok := rec.(error)
	if !ok {
		return false
	}
	var se *os.SyscallError
	if !errors.As(ne, &se) {
		return false
	}
	msg := strings.ToLower(se.Error())
	for _, m := range peerGoneMessages {
		if strings.Contains(msg, m) {
			return true
		}
	}
	return false
}

func NewRouter(d Deps) *gin.Engine {
	r := gin.New()
	if len(d.TrustedProxies) > 0 {
		if err := r.SetTrustedProxies(d.TrustedProxies); err != nil {
			slog.Warn("gin.SetTrustedProxies failed; falling back to defaults",
				"err", err.Error(), "count", len(d.TrustedProxies))
		}
	}
	r.Use(recoverWithoutRequestDump())
	r.Use(middleware.RequestID())
	r.Use(middleware.CORS())
	if d.Drain != nil {
		r.Use(d.Drain.Middleware())
	}

	r.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})
	r.GET("/readyz", func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
		defer cancel()
		if err := d.Data.Redis.Ping(ctx).Err(); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"status": "redis_unreachable", "err": err.Error()})
			return
		}
		if _, err := d.Data.Ent.User.Query().Limit(1).IDs(ctx); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"status": "db_unreachable", "err": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ready"})
	})

	// ---- /internal/* management endpoints ----
	// Every /internal route requires its own bearer token — reverse-proxy
	// isolation alone is not trusted. Comparisons are constant-time.
	//
	// Drain control uses INTERNAL_API_TOKEN. Unset = endpoints disabled
	// (fail closed): set the env before using drain in a deploy hook.
	internalAuth := func(c *gin.Context) bool {
		want := strings.TrimSpace(os.Getenv("INTERNAL_API_TOKEN"))
		if want == "" {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "internal api disabled: INTERNAL_API_TOKEN not set"})
			return false
		}
		got := strings.TrimSpace(strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer "))
		if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			return false
		}
		return true
	}
	r.GET("/internal/drain/status", func(c *gin.Context) {
		if !internalAuth(c) {
			return
		}
		active := int64(0)
		draining := false
		if d.Drain != nil {
			active = d.Drain.Active()
			draining = d.Drain.IsDraining()
		}
		c.JSON(http.StatusOK, gin.H{"active_requests": active, "draining": draining})
	})
	r.POST("/internal/drain/start", func(c *gin.Context) {
		if !internalAuth(c) {
			return
		}
		if d.Drain != nil {
			d.Drain.SetDraining(true)
		}
		// Relay sockets would hold the drain open for hours: close them
		// (4503) so their daemons and clients reconnect to the other slot.
		if d.Relay != nil {
			d.Relay.Drain()
		}
		c.JSON(http.StatusOK, gin.H{"status": "draining"})
	})
	r.POST("/internal/drain/stop", func(c *gin.Context) {
		if !internalAuth(c) {
			return
		}
		if d.Drain != nil {
			d.Drain.SetDraining(false)
		}
		c.JSON(http.StatusOK, gin.H{"status": "ready"})
	})
	r.GET("/internal/metrics", func(c *gin.Context) {
		if !internalAuth(c) {
			return
		}
		metrics.Handler().ServeHTTP(c.Writer, c.Request)
	})

	if d.RegionControl != nil {
		regionGroup := r.Group(regionpkg.APIPrefix)
		d.RegionControl.Mount(regionGroup)
	}

	v1 := r.Group("/v1")
	siteAssetDir := d.SiteAssetDir
	if siteAssetDir == "" {
		siteAssetDir = defaultSiteAssetDir
	}
	mountSiteAssets(v1, siteAssetDir)

	// Public read-only endpoints (no auth). All cached via internal/cache:
	// the underlying svc call only fires on miss; otherwise we re-shape
	// the cached typed value back into the wire format on every hit.
	v1.GET("/public/site-info", func(c *gin.Context) {
		s, hit, err := cache.GetOrCompute[*biz.SiteSettings](c.Request.Context(), d.Cache,
			cache.KeySiteInfo, 5*time.Minute, d.Settings.GetSite)
		setCacheHeader(c, hit)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "site temporarily unavailable"})
			return
		}
		// invite_program is redacted to the public subset: the full object
		// carries risk thresholds/caps that must not be published. The
		// shadowing field wins over the embedded struct's at marshal time.
		program := biz.DefaultInviteProgram().Public()
		if s.InviteProgram != nil {
			program = s.InviteProgram.Public()
		}
		c.JSON(http.StatusOK, publicSiteInfo{SiteSettings: s, InviteProgram: program})
	})
	v1.GET("/public/plans", func(c *gin.Context) {
		ps, hit, err := cache.GetOrCompute[[]*ent.Plan](c.Request.Context(), d.Cache,
			cache.KeyPlansActive, 5*time.Minute, func(ctx context.Context) ([]*ent.Plan, error) {
				return d.Plans.ListPlans(ctx, true /*onlyActive*/)
			})
		setCacheHeader(c, hit)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "plans temporarily unavailable"})
			return
		}
		out := make([]gin.H, 0, len(ps))
		for _, p := range ps {
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
				"monthly_spend_limit":   p.MonthlySpendLimit.String(),
				"auto_reload_enabled":   p.AutoReloadEnabled,
				"auto_reload_threshold": p.AutoReloadThreshold.String(),
				"auto_reload_amount":    p.AutoReloadAmount.String(),
			})
		}
		c.JSON(http.StatusOK, gin.H{"plans": out})
	})
	v1.GET("/public/models", func(c *gin.Context) {
		ctx := c.Request.Context()
		bundle, hit, err := cache.GetOrCompute[publicModelsBundle](ctx, d.Cache,
			cache.KeyModelsPublic, 5*time.Minute, func(ctx context.Context) (publicModelsBundle, error) {
				ms, err := d.Models.ListModels(ctx)
				if err != nil {
					return publicModelsBundle{}, err
				}
				enabled := make([]*ent.Model, 0, len(ms))
				for _, m := range ms {
					if m.Enabled {
						enabled = append(enabled, m)
					}
				}
				vendors, err := d.Models.LoadVendors(ctx, enabled)
				if err != nil {
					return publicModelsBundle{}, err
				}
				groups, err := d.Models.LoadGroupsForModels(ctx, enabled)
				if err != nil {
					return publicModelsBundle{}, err
				}
				for id, gs := range groups {
					groups[id] = biz.PublicCatalogGroups(gs)
				}
				return publicModelsBundle{Models: enabled, VendorByID: vendors, GroupsByModelID: groups}, nil
			})
		setCacheHeader(c, hit)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "models temporarily unavailable"})
			return
		}
		out := make([]gin.H, 0, len(bundle.Models))
		for _, m := range bundle.Models {
			row := gin.H{
				"name":         m.Name,
				"billing_mode": m.BillingMode,
				"currency":     m.Currency,
				"description":  m.Description,
			}
			if m.PricePerRequest != nil {
				row["price_per_request"] = m.PricePerRequest.String()
			}
			if m.InputUnitPrice != nil {
				row["input_unit_price"] = m.InputUnitPrice.String()
			}
			if m.OutputUnitPrice != nil {
				row["output_unit_price"] = m.OutputUnitPrice.String()
			}
			if m.CacheReadInputPrice != nil {
				row["cache_read_input_price"] = m.CacheReadInputPrice.String()
			}
			if m.CacheWriteInputPrice != nil {
				row["cache_write_input_price"] = m.CacheWriteInputPrice.String()
			}
			if m.CacheWrite1hInputPrice != nil {
				row["cache_write_1h_input_price"] = m.CacheWrite1hInputPrice.String()
			}
			if m.ImageInputUnitPrice != nil {
				row["image_input_unit_price"] = m.ImageInputUnitPrice.String()
			}
			if m.ImageOutputUnitPrice != nil {
				row["image_output_unit_price"] = m.ImageOutputUnitPrice.String()
			}
			if m.AudioInputUnitPrice != nil {
				row["audio_input_unit_price"] = m.AudioInputUnitPrice.String()
			}
			if m.AudioOutputUnitPrice != nil {
				row["audio_output_unit_price"] = m.AudioOutputUnitPrice.String()
			}
			if m.OutputImagePrice != nil {
				row["output_image_price"] = m.OutputImagePrice.String()
			}
			// decimal.Decimal marshals as a quoted string, so tier prices
			// serialize consistently with the flat price fields above.
			if len(m.PriceTiers) > 0 {
				row["price_tiers"] = m.PriceTiers
			}
			if m.VendorID != nil {
				if v, ok := bundle.VendorByID[*m.VendorID]; ok {
					row["vendor"] = gin.H{
						"id":   v.ID.String(),
						"name": v.Name,
						"slug": v.Slug,
					}
				}
			}
			if groups := bundle.GroupsByModelID[m.ID]; len(groups) > 0 {
				modelGroups := make([]gin.H, 0, len(groups))
				for _, g := range biz.PublicCatalogGroups(groups) {
					gv := gin.H{
						"id":              g.ID.String(),
						"name":            g.Name,
						"billing_source":  g.BillingSource,
						"rate_multiplier": g.RateMultiplier,
					}
					if g.Description != "" {
						gv["description"] = g.Description
					}
					if len(g.Profile) > 0 {
						gv["profile"] = g.Profile
					}
					modelGroups = append(modelGroups, gv)
				}
				row["groups"] = modelGroups
			}
			out = append(out, row)
		}
		c.JSON(http.StatusOK, gin.H{"models": out})
	})
	v1.GET("/public/status", func(c *gin.Context) {
		snap, hit, err := cache.GetOrCompute[*biz.PublicStatus](c.Request.Context(), d.Cache,
			cache.KeyStatusSnapshot, 30*time.Second, d.Status.Snapshot)
		setCacheHeader(c, hit)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "status temporarily unavailable"})
			return
		}
		c.JSON(http.StatusOK, snap)
	})
	// Public-friendly view of the account-pool budget. Same shape as the
	// admin endpoint but stripped of fields that leak operational scale
	// (account counts). Operators want to share "supply outlook" with
	// users as a trust signal without exposing pool size.
	// RUM ingest from the browser's web-vitals library. Public + no
	// auth so anonymous visitors can report too. Capped at 16 metrics
	// per request inside the handler.
	v1.POST("/public/web-vitals", admin.IngestWebVitals(d.Data.Ent))

	alipayNotify := func(c *gin.Context) {
		if d.Alipay == nil || d.Finance == nil {
			c.String(http.StatusServiceUnavailable, "failure")
			return
		}
		body, err := io.ReadAll(io.LimitReader(c.Request.Body, 2<<20))
		if err != nil {
			c.String(http.StatusBadRequest, "failure")
			return
		}
		result, err := d.Alipay.ParseCallback(c.Request.Header, body)
		if err != nil {
			c.String(http.StatusBadRequest, "failure")
			return
		}
		if _, err := d.Finance.ApplyAlipayCallback(c.Request.Context(), *result); err != nil {
			if errors.Is(err, biz.ErrOrderInvalidState) {
				c.String(http.StatusOK, "success")
				return
			}
			slog.Warn("alipay callback apply failed", "err", err.Error(), "order_no", result.OrderNo)
			c.String(http.StatusInternalServerError, "failure")
			return
		}
		c.String(http.StatusOK, "success")
	}
	v1.POST("/payments/alipay/notify", alipayNotify)

	v1.GET("/public/vendors", func(c *gin.Context) {
		vs, hit, err := cache.GetOrCompute[[]*ent.ModelVendor](c.Request.Context(), d.Cache,
			cache.KeyVendors, 10*time.Minute, d.Models.ListVendors)
		setCacheHeader(c, hit)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "vendors temporarily unavailable"})
			return
		}
		out := make([]gin.H, 0, len(vs))
		for _, v := range vs {
			out = append(out, gin.H{
				"id":          v.ID.String(),
				"name":        v.Name,
				"slug":        v.Slug,
				"description": v.Description,
				"sort_order":  v.SortOrder,
			})
		}
		c.JSON(http.StatusOK, gin.H{"vendors": out})
	})

	// public auth routes — IP rate-limited to slow brute-force probing.
	authH := auth.New(d.Account)
	authH.SetInvites(d.Invites)
	authGrp := v1.Group("/auth")
	authGrp.Use(middleware.IPRateLimit(d.Data.Redis, 30, 60))
	authH.Register(authGrp)

	oauthH := apioauth.New(d.Data.Redis, d.OAuth, d.Signer, d.OAuthClients)
	// POST /v1/oauth/token — public (authorization_code + refresh_token grants).
	oauthPublicGrp := v1.Group("/oauth")
	oauthH.MountPublic(oauthPublicGrp)
	// POST /v1/oauth/authorize — browser consent, gated by the web session JWT.
	// Personal access tokens: console APIs for scripts and agents, acting as
	// the owner with their current role (see middleware.JWT).
	accessTokens := biz.NewAccessTokenSvc(d.Data)
	oauthAuthorizedGrp := v1.Group("/oauth")
	// Authorizing a device mints credentials, so it needs a real session.
	oauthAuthorizedGrp.Use(middleware.JWT(d.Signer, accessTokens, d.Account.IsUserActive), middleware.SessionOnly())
	oauthH.MountAuthorized(oauthAuthorizedGrp)

	// authenticated /v1/me/*
	userH := user.New(d.Data.Ent, d.ApiKeySvc, d.OAuth, d.Plans, d.Redeem, d.Stats, d.Models, d.Account, d.Tickets, d.Roles, d.Finance, d.Organizations, d.Cache)
	userH.SetRateLimitGate(d.RateLimit)
	userH.SetInvites(d.Invites)
	userH.SetAgents(d.Agents)
	userH.SetStatus(d.Status)
	userH.SetAccessTokens(accessTokens)
	meGrp := v1.Group("/me")
	meGrp.Use(middleware.JWT(d.Signer, accessTokens, d.Account.IsUserActive), middleware.OrgContext(d.Organizations))
	userH.Mount(meGrp)

	// OAuth-token read endpoints: GET /v1/oauth/userinfo|usage|stats/usage|usage-logs.
	// Reachable with a device access token so the CLI/Desktop can query
	// 套餐 / 用量 / 余额 / 调用详情 without a web account JWT. Reuses the /me handlers.
	oauthReadGrp := v1.Group("/oauth")
	oauthReadGrp.Use(middleware.OAuthAccess(d.OAuth, d.Signer))
	userH.MountReadOnly(oauthReadGrp)
	// JuCode Desktop feedback: POST /v1/oauth/tickets, a ticket with
	// screenshots and a gzipped log file.
	userH.MountFeedback(oauthReadGrp,
		middleware.UserScopedRateLimit(d.Data.Redis, "desktop_feedback", 10, 3600),
		middleware.BodyLimit(16<<20))
	// JuCode Desktop agent usage + settings sync: /v1/oauth/agent-usage*, /v1/oauth/settings.
	apioauth.NewDevice(biz.NewAgentUsageSvc(d.Data), d.Data.Redis).Mount(oauthReadGrp)
	// Exact cost of one gateway call by its X-Request-Id.
	apioauth.MountUsageByRequest(oauthReadGrp, d.Data,
		middleware.UserScopedRateLimit(d.Data.Redis, "usage_by_request", 120, 60))
	// The account's computers: /v1/oauth/computers*, and the relay that
	// reaches them: /v1/relay/host (daemons) and /v1/relay/connect
	// (clients), both with an OAuth device token.
	computersH := apioauth.NewComputers(d.Computers, d.Data.Redis)
	computersH.Mount(oauthReadGrp)
	if d.Relay != nil {
		d.Relay.Mount(v1.Group("/relay", middleware.OAuthAccess(d.OAuth, d.Signer)))
	}

	// customer-service /v1/cs/* — requires the "cs" functional role
	// (admins implicitly pass). Mutating actions land in admin_audit_logs
	// via the same middleware as /v1/admin/*.
	csH := cs.New(d.Data.Ent, d.Tickets, d.Plans, d.ApiKeySvc, d.Roles, d.Account, d.Finance, d.Data.DB())
	csH.SetStepUpGuard(middleware.RequireTOTPStepUp(d.Account))
	csGrp := v1.Group("/cs")
	csGrp.Use(
		middleware.JWT(d.Signer, accessTokens, d.Account.IsUserActive),
		middleware.RequireRoleAny(d.Roles, biz.RoleCS),
		middleware.AdminAudit(d.Audit),
	)
	csH.Mount(csGrp)

	// agent /v1/agent/* — requires the "agent" functional role.
	agentH := apiagent.New(d.Agents, d.Redeem)
	agentGrp := v1.Group("/agent")
	agentGrp.Use(
		middleware.JWT(d.Signer, accessTokens, d.Account.IsUserActive),
		middleware.RequireRoleAny(d.Roles, biz.RoleAgent),
		middleware.AdminAudit(d.Audit),
	)
	agentH.Mount(agentGrp)

	// admin /v1/admin/*
	adminH := admin.New(d.AccountPool, d.Dashboard, d.Models, d.Plans, d.Redeem, d.Stats, d.Settings, d.ApiKeySvc, d.Policy, d.Incidents, d.Audit, d.Account, d.IPPolicy, d.Roles, d.Agents, d.Finance, d.Organizations, d.Cache, d.Data.Ent, d.Data.DB())
	adminH.SetInvites(d.Invites)
	adminH.SetSiteAssetDir(siteAssetDir)
	adminH.SetStepUpGuard(middleware.RequireTOTPStepUp(d.Account))
	adminH.SetSystemIncidents(d.SystemIncidents)
	adminH.SetProviderDefinitions(d.ProviderDefinitions)
	adminH.SetRemoteCatalog(d.RemoteCatalog)
	adminH.SetWebTools(d.WebTools)
	adminGrp := v1.Group("/admin")
	adminGrp.Use(
		middleware.JWT(d.Signer, accessTokens, d.Account.IsUserActive),
		middleware.AdminOnly(),
		// Every mutating admin action is recorded into admin_audit_logs
		// after the handler returns. GETs are skipped inside the
		// middleware itself.
		middleware.AdminAudit(d.Audit),
	)
	adminH.Mount(adminGrp)

	// LLM gateway: OpenAI-compatible generation endpoints are governed by
	// ConcurrencyGate only. A zero/unset concurrency limit means unlimited.
	// ClientAuth accepts EITHER a classic sk-juc-* API key OR an OAuth
	// device access token (CLI / Desktop). Both resolve to the same
	// CtxUser + CtxApiKey shape so routing/billing is unchanged.
	llmH := apillm.New(d.Gateway, d.Models)
	// groupOverride: an optional X-JuCode-Group header pins routing to one
	// specific ModelGroup (used by the console lab). It can only narrow the
	// key's allowed groups — biz.Resolve re-applies the allowed filter.
	// X-JuCode-Billing-Source (plan_only / balance_only) narrows auto-routing to
	// one billing source, like the group pin it only ever narrows.
	groupOverride := func(c *gin.Context) {
		if v := strings.TrimSpace(c.GetHeader("X-JuCode-Group")); v != "" {
			if id, err := uuid.Parse(v); err == nil {
				c.Request = c.Request.WithContext(biz.WithGroupOverride(c.Request.Context(), id))
			}
		}
		switch source := entgroup.BillingSource(strings.TrimSpace(c.GetHeader("X-JuCode-Billing-Source"))); source {
		case entgroup.BillingSourcePlanOnly, entgroup.BillingSourceBalanceOnly:
			c.Request = c.Request.WithContext(biz.WithBillingSourceFilter(c.Request.Context(), source))
		}
		c.Next()
	}
	llmGenGrp := v1.Group("")
	llmGenGrp.Use(middleware.ClientAuth(d.ApiKeySvc, d.OAuth, d.Signer), groupOverride)
	llmH.MountGeneration(llmGenGrp)
	// Non-chat generation endpoints (embeddings / image / audio / rerank /
	// video). Same ClientAuth + ConcurrencyGate governance as chat.
	llmH.MountMediaGeneration(llmGenGrp)

	llmModelsGrp := v1.Group("")
	llmModelsGrp.Use(middleware.ClientAuth(d.ApiKeySvc, d.OAuth, d.Signer), middleware.UserRateLimit(d.Data.Redis, 240, 60))
	llmH.MountModels(llmModelsGrp)

	// Open API: everything a user can query with just an API key — balance,
	// quota, usage, catalog. Two surfaces, see internal/api/openapi:
	// the new-api-compatible endpoints (frozen wire format, for the existing
	// ecosystem of key checkers) and the native /v1/open/* ones.
	//
	// All read-only, all rate limited at half the /v1/models allowance:
	// balance widgets poll, and these queries aggregate over usage_logs.
	openapiH := apiopenapi.New(d.Data.Ent, d.Models, d.Plans, d.ApiKeySvc, d.Stats, d.Account, d.Cache)
	clientAuthed := func(g *gin.RouterGroup) *gin.RouterGroup {
		g.Use(middleware.ClientAuth(d.ApiKeySvc, d.OAuth, d.Signer), middleware.UserRateLimit(d.Data.Redis, 120, 60))
		return g
	}
	// GET /api/pricing — public catalog, no credential needed.
	openapiH.MountPricing(r.Group("/api"))
	// GET /api/tools/pricing — the enabled web tool engines and their per-call
	// prices, public like /api/pricing.
	if d.WebTools != nil {
		r.GET("/api/tools/pricing", func(c *gin.Context) {
			prices, err := d.WebTools.PublicPrices(c.Request.Context())
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "prices temporarily unavailable"})
				return
			}
			c.JSON(http.StatusOK, gin.H{"currency": biz.CurrencyCNY, "tools": prices})
		})
	}
	// GET /api/usage/token/
	openapiH.MountTokenUsage(clientAuthed(r.Group("/api")))
	// GET /dashboard/billing/* and the /v1/dashboard/billing/* alias.
	openapiH.MountBilling(clientAuthed(r.Group("")))
	openapiH.MountBilling(clientAuthed(v1.Group("")))
	// GET /v1/open/*
	openapiH.MountOpen(clientAuthed(v1.Group("/open")))

	// Anthropic Messages-API generation is also governed by ConcurrencyGate
	// only. A zero/unset concurrency limit means unlimited.
	anthMsgGrp := r.Group("/anthropic/v1")
	anthMsgGrp.Use(middleware.ClientAuth(d.ApiKeySvc, d.OAuth, d.Signer), groupOverride)
	llmH.MountAnthropicMessages(anthMsgGrp)

	// Platform tools: JuCode's own API, not a vendor-compatible surface. Same
	// ClientAuth and routing as generation.
	toolsGrp := r.Group("/tools/v1")
	toolsGrp.Use(middleware.ClientAuth(d.ApiKeySvc, d.OAuth, d.Signer), groupOverride)
	llmH.MountTools(toolsGrp)

	anthModelsGrp := r.Group("/anthropic/v1")
	anthModelsGrp.Use(middleware.ClientAuth(d.ApiKeySvc, d.OAuth, d.Signer), middleware.UserRateLimit(d.Data.Redis, 240, 60))
	llmH.MountAnthropicModels(anthModelsGrp)

	// Console laboratory. Session JWT plus the org header, never the lab
	// key secret. The same generation handlers run with that key attached.
	labGrp := meGrp.Group("/lab")
	labGrp.Use(userH.AttachLabKey(), groupOverride)
	llmH.MountGeneration(labGrp)
	llmH.MountMediaGeneration(labGrp)
	llmH.MountTools(labGrp.Group("/tools"))
	llmH.MountAnthropicMessages(labGrp.Group("/anthropic"))

	// The console's computer list: /v1/me/computers*.
	computersH.Mount(meGrp)

	return r
}

// publicSiteInfo is the /v1/public/site-info wire shape: the full site
// settings with invite_program overridden by its public projection (risk/cap
// fields redacted) and refund auto-approval left out. Admin GET /v1/admin/settings keeps the full object.
type publicSiteInfo struct {
	*biz.SiteSettings
	InviteProgram biz.InviteProgramPublic `json:"invite_program"`
	// Refund auto-approval stays private: publishing the cap would tell
	// users how to stay under it. The zero shadows are omitted.
	RefundAutoApproveEnabled bool   `json:"refund_auto_approve_enabled,omitempty"`
	RefundAutoApproveMax     string `json:"refund_auto_approve_max,omitempty"`
}

// publicModelsBundle is the typed value cached at KeyModelsPublic.
// Keeps the enabled-models list, vendor lookup, and group lookup together so
// one cache key covers the entire /public/models response.
type publicModelsBundle struct {
	Models          []*ent.Model                    `json:"models"`
	VendorByID      map[uuid.UUID]*ent.ModelVendor  `json:"vendors"`
	GroupsByModelID map[uuid.UUID][]*ent.ModelGroup `json:"groups_by_model_id"`
}

// setCacheHeader writes X-Cache: HIT|MISS for the just-served response.
// Done as a tiny helper so the handlers above stay one-liners around
// cache.GetOrCompute.
func setCacheHeader(c *gin.Context, hit bool) {
	if hit {
		c.Header("X-Cache", "HIT")
	} else {
		c.Header("X-Cache", "MISS")
	}
}
