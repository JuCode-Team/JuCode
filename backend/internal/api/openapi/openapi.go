// Package openapi implements the endpoints a user can call with nothing but
// an API key — no console session, no browser.
//
// It covers two surfaces that deliberately do NOT share a response style:
//
//   - The new-api / one-api compatibility layer (GET /api/pricing,
//     GET /dashboard/billing/*, GET /api/usage/token/). These reproduce
//     another project's wire format field-for-field so the large ecosystem
//     of key checkers and price aggregators built against it works against
//     JuCode unmodified. Their shapes are frozen by that contract: money is
//     a JSON number, errors follow new-api's envelope, and fields that make
//     no sense here (has_payment_method) are still emitted. Do not "fix"
//     them — every deviation is a client that silently misreads a balance.
//
//   - The native open API (GET /v1/open/*), which follows house style:
//     money is a decimal string, errors are {"error": "..."}, and nothing is
//     inherited from anyone else's schema.
//
// Both are read-only. Nothing here mutates state, so an API key leak costs a
// caller their usage history, never their balance.
package openapi

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/cache"
	"github.com/jucode/jucode/internal/data/ent"
	"github.com/jucode/jucode/internal/middleware"
)

type Handler struct {
	users   *ent.Client
	models  *biz.ModelSvc
	plans   *biz.PlanSvc
	apikeys *biz.ApiKeySvc
	stats   *biz.StatsSvc
	account *biz.Account
	cache   *cache.Cache
}

func New(users *ent.Client, models *biz.ModelSvc, plans *biz.PlanSvc, apikeys *biz.ApiKeySvc, stats *biz.StatsSvc, account *biz.Account, cacheSvc *cache.Cache) *Handler {
	return &Handler{users: users, models: models, plans: plans, apikeys: apikeys, stats: stats, account: account, cache: cacheSvc}
}

// MountBilling registers the OpenAI-legacy billing pair. The group must
// already carry ClientAuth.
//
// Mount it twice — once at the root and once under /v1 — because upstream
// serves both spellings and clients are split roughly evenly between them.
func (h *Handler) MountBilling(rg *gin.RouterGroup) {
	rg.GET("/dashboard/billing/subscription", h.handleBillingSubscription)
	rg.GET("/dashboard/billing/usage", h.handleBillingUsage)
}

// MountTokenUsage registers new-api's richer balance endpoint on an /api
// group. The group must already carry ClientAuth.
//
// The trailing slash is not a typo — upstream registers the route that way
// and clients hardcode it. Gin's redirectTrailingSlash covers the other
// spelling with a 301.
func (h *Handler) MountTokenUsage(rg *gin.RouterGroup) {
	rg.GET("/usage/token/", h.handleTokenUsage)
}

// MountPricing registers the unauthenticated model/pricing catalog on an
// /api group.
func (h *Handler) MountPricing(rg *gin.RouterGroup) {
	rg.GET("/pricing", h.handlePricing)
}

// MountOpen registers the native open API. The group must already carry
// ClientAuth.
func (h *Handler) MountOpen(rg *gin.RouterGroup) {
	rg.GET("/balance", h.handleBalance)
	rg.GET("/key", h.handleKey)
	rg.GET("/groups", h.handleGroups)
	rg.GET("/usage", h.handleUsage)
	rg.GET("/usage/models", h.handleUsageByModel)
	rg.GET("/logs", h.handleLogs)
}

// callerKey returns the API key backing this request, or nil when the caller
// authenticated with an OAuth device token.
//
// ClientAuth synthesizes a non-persisted ent.ApiKey for OAuth callers so the
// routing and billing paths downstream need no special case. That synthetic
// record has a zero ID and zero limits, and reporting it as a real key would
// show a CLI user an "API key" they cannot find in their console. Every
// key-scoped field in this package is therefore gated on this returning
// non-nil.
func callerKey(c *gin.Context) *ent.ApiKey {
	k, ok := middleware.ApiKeyFrom(c)
	if !ok || k == nil || k.ID == uuid.Nil {
		return nil
	}
	return k
}

// callerScope is the usage-log filter for the authenticated caller.
//
// Scoped by user, not by organization: an org-billed key still writes
// usage_logs.user_id = the caller, so this shows "calls I made" regardless of
// whose balance paid. That is the question an API caller is asking, and it
// keeps one member from reading the whole org's traffic through a key they
// happen to hold.
func callerScope(uid uuid.UUID) biz.StatsFilter {
	return biz.StatsFilter{UserID: &uid}
}

// parseWindow reads start/end/bucket from the query string.
//
// Both timestamps accept RFC3339 or a bare unix second count — CLI users
// reach for `date +%s` far more often than for RFC3339, and rejecting it
// would be a pointless papercut. Unparseable values fall back to the default
// window rather than erroring: this is a read-only stats endpoint, and a
// caller who fat-fingers a date is better served by "here is the last week"
// than by a 400 they have to go read docs about.
func parseWindow(c *gin.Context) (start, end time.Time, bucket string) {
	bucket = strings.ToLower(strings.TrimSpace(c.DefaultQuery("bucket", "day")))
	if bucket != "hour" && bucket != "day" {
		bucket = "day"
	}
	return parseTime(c.Query("start")), parseTime(c.Query("end")), bucket
}

func parseTime(v string) time.Time {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
		return time.Unix(n, 0)
	}
	return time.Time{}
}

// parsePage reads limit/offset with a hard cap. Out-of-range and unparseable
// values fall back to the default for the same reason parseWindow does.
func parsePage(c *gin.Context, def, maxLimit int) (limit, offset int) {
	limit, offset = def, 0
	if v := c.Query("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= maxLimit {
			limit = n
		}
	}
	if v := c.Query("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			offset = n
		}
	}
	return limit, offset
}

// fail writes the native open API's error envelope: {"error": "..."}, the
// same shape /v1/models uses. The compat endpoints do NOT use this.
func fail(c *gin.Context, status int, msg string) {
	c.JSON(status, gin.H{"error": msg})
}

// requireUser resolves the caller's id, writing a 401 and returning false if
// the middleware somehow let an unauthenticated request through.
func requireUser(c *gin.Context) (uuid.UUID, bool) {
	uid, ok := middleware.UserIDFrom(c)
	if !ok {
		fail(c, http.StatusUnauthorized, "no user")
		return uuid.Nil, false
	}
	return uid, true
}

// decStr renders a decimal for the native API: always a string, always at the
// ledger's own precision, so a caller can compare two amounts without
// worrying about float rounding.
func decStr(d decimal.Decimal) string { return d.String() }

// decPtrStr renders an optional decimal, returning nil for "no limit set" so
// clients can distinguish it from a limit of zero.
func decPtrStr(d *decimal.Decimal) *string {
	if d == nil {
		return nil
	}
	s := d.String()
	return &s
}
