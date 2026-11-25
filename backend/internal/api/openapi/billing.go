package openapi

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/cache"
	"github.com/jucode/jucode/internal/data/ent"
	"github.com/jucode/jucode/internal/middleware"
)

// The OpenAI legacy billing endpoints, as reimplemented by one-api / new-api
// and as reimplemented here.
//
// ## Why clone a deprecated OpenAI API
//
// OpenAI retired /dashboard/billing/* years ago, but one-api adopted it as
// its balance API and new-api inherited it. Every "查余额" tool in the
// Chinese LLM-gateway ecosystem — key checkers, Cherry Studio, uni-api,
// NextChat forks — probes exactly these two paths. Implementing them is what
// makes a JuCode key work in those tools without a JuCode-specific plugin.
//
// ## What the numbers mean here
//
// The wire format is frozen, and it names its fields `*_usd`. JuCode's unit
// of account is 积分 (1 积分 = 1 元). There is no currency field to correct
// this with, and converting to USD would make the number disagree with the
// console at whatever exchange rate we picked. So these fields carry 元, and
// a client that prints a `$` in front is printing the wrong symbol next to
// the right number. new-api behaves identically when its operator sets
// quota_display_type=CNY. The native GET /v1/open/balance states its currency
// explicitly and should be preferred by anything JuCode-specific.
//
// The subscription/usage pair is designed to be *subtracted*: every checker
// computes remaining = soft_limit_usd − total_usage/100. So subscription
// reports total granted (remaining + spent) and usage reports spent, which
// makes that subtraction land on the real balance.

// billingScope is the pair of numbers both endpoints project, resolved once
// so subscription and usage can never disagree with each other.
type billingScope struct {
	granted     decimal.Decimal
	used        decimal.Decimal
	accessUntil int64
	name        string
}

// resolveBillingScope decides whether to report the account or the key.
//
// A key carrying a total cost limit is a sub-budget: its holder cares about
// that ceiling, not the account balance behind it, and reporting the account
// would let a capped key claim the full balance. This mirrors new-api's
// DisplayTokenStatEnabled behavior, where a token with its own quota reports
// per-token numbers.
//
// Everything else — uncapped keys, OAuth device tokens — reports the account.
func (h *Handler) resolveBillingScope(ctx context.Context, uid uuid.UUID, key *ent.ApiKey) (billingScope, error) {
	if isCappedKey(key) {
		usage, err := h.keyCostUsage(ctx, key.ID)
		if err != nil {
			return billingScope{}, err
		}
		return billingScope{
			granted:     *key.TotalCostLimit,
			used:        usage.Total,
			accessUntil: expiryUnix(key),
			name:        key.Name,
		}, nil
	}

	balance, err := h.account.Balance(ctx, uid)
	if err != nil {
		return billingScope{}, err
	}
	spend, err := h.lifetimeSpend(ctx, uid)
	if err != nil {
		return billingScope{}, err
	}
	sc := billingScope{granted: balance.Add(spend), used: spend}
	if key != nil {
		sc.accessUntil = expiryUnix(key)
		sc.name = key.Name
	}
	return sc, nil
}

// isCappedKey reports whether this key carries a sub-budget of its own.
//
// A nil or zero limit means "no cap", not "a cap of zero" — the column is
// nillable precisely so those two can be told apart, and treating an unset
// limit as a ceiling would report every uncapped key as having no funds.
func isCappedKey(key *ent.ApiKey) bool {
	return key != nil && key.TotalCostLimit != nil && key.TotalCostLimit.Sign() > 0
}

func expiryUnix(key *ent.ApiKey) int64 {
	if key == nil || key.ExpiresAt == nil || key.ExpiresAt.IsZero() {
		return 0
	}
	return key.ExpiresAt.Unix()
}

// lifetimeSpend is every 积分 this user has ever spent.
//
// StatsSvc.Lifetime touches every historical usage_logs row for the account,
// so it is cached hard. 60s of staleness on a lifetime total is invisible —
// the value only ever grows, and by fractions of itself.
func (h *Handler) lifetimeSpend(ctx context.Context, uid uuid.UUID) (decimal.Decimal, error) {
	out, _, err := cache.GetOrCompute[string](ctx, h.cache,
		"openapi:lifetime-spend:"+uid.String(), 60*time.Second, func(ctx context.Context) (string, error) {
			lt, err := h.stats.Lifetime(ctx, biz.StatsFilter{UserID: &uid})
			if err != nil {
				return "", err
			}
			return lt.TotalCost, nil
		})
	if err != nil {
		return decimal.Zero, err
	}
	return decimal.NewFromString(out)
}

// keyCostUsage is the per-key daily/monthly/total spend, cached briefly.
// The underlying query is three filtered SUMs over the key's usage_logs
// partition; a balance widget polling every few seconds must not re-run it.
func (h *Handler) keyCostUsage(ctx context.Context, keyID uuid.UUID) (biz.CostUsage, error) {
	type view struct{ Daily, Monthly, Total string }
	v, _, err := cache.GetOrCompute[view](ctx, h.cache,
		"openapi:key-cost:"+keyID.String(), 15*time.Second, func(ctx context.Context) (view, error) {
			u, err := h.apikeys.CostUsage(ctx, keyID, time.Now())
			if err != nil {
				return view{}, err
			}
			return view{u.Daily.String(), u.Monthly.String(), u.Total.String()}, nil
		})
	if err != nil {
		return biz.CostUsage{}, err
	}
	d, _ := decimal.NewFromString(v.Daily)
	m, _ := decimal.NewFromString(v.Monthly)
	t, _ := decimal.NewFromString(v.Total)
	return biz.CostUsage{Daily: d, Monthly: m, Total: t}, nil
}

// GET /dashboard/billing/subscription (and the /v1 alias).
//
// Query parameters are ignored, as they are upstream.
func (h *Handler) handleBillingSubscription(c *gin.Context) {
	uid, ok := middlewareUserID(c)
	if !ok {
		return
	}
	sc, err := h.resolveBillingScope(c.Request.Context(), uid, callerKey(c))
	if err != nil {
		compatError(c, "upstream_error", err.Error())
		return
	}
	c.JSON(http.StatusOK, subscriptionBody(sc))
}

// subscriptionBody is separated from the handler so the exact field names can
// be pinned by a test. They are upstream's, not ours: renaming one does not
// break a build, it breaks every key checker silently.
func subscriptionBody(sc billingScope) gin.H {
	amount := sc.granted.InexactFloat64()
	return gin.H{
		// has_payment_method is hardcoded true upstream; some clients refuse
		// to display a balance without it.
		"object":                "billing_subscription",
		"has_payment_method":    true,
		"soft_limit_usd":        amount,
		"hard_limit_usd":        amount,
		"system_hard_limit_usd": amount,
		"access_until":          sc.accessUntil,
	}
}

// GET /dashboard/billing/usage (and the /v1 alias).
//
// start_date / end_date are accepted and ignored — the response is always the
// lifetime total. That is upstream's behavior, and every client that sends
// those parameters already expects a lifetime number back.
//
// total_usage is in 分 (hundredths), matching upstream's "unit: 0.01 dollar".
func (h *Handler) handleBillingUsage(c *gin.Context) {
	uid, ok := middlewareUserID(c)
	if !ok {
		return
	}
	sc, err := h.resolveBillingScope(c.Request.Context(), uid, callerKey(c))
	if err != nil {
		compatError(c, "new_api_error", err.Error())
		return
	}
	c.JSON(http.StatusOK, usageBody(sc))
}

// usageBody converts spend to 分. Separated for the same reason as
// subscriptionBody, and because the ×100 is the one arithmetic step in this
// file that a reader would plausibly "simplify" away.
func usageBody(sc billingScope) gin.H {
	return gin.H{
		"object":      "list",
		"total_usage": sc.used.Mul(decimal.NewFromInt(100)).InexactFloat64(),
	}
}

// GET /api/usage/token/ — new-api's richer balance endpoint.
//
// Unlike the two above it reports granted / used / available as three
// separate numbers, so a client never has to do the subtraction itself. The
// amounts are in 积分 (= 元) rather than upstream's raw quota integers; the
// added `currency` field says so, and is the JuCode extension that makes this
// endpoint unambiguous where the *_usd pair cannot be.
func (h *Handler) handleTokenUsage(c *gin.Context) {
	uid, ok := middlewareUserID(c)
	if !ok {
		return
	}
	key := callerKey(c)
	sc, err := h.resolveBillingScope(c.Request.Context(), uid, key)
	if err != nil {
		compatError(c, "new_api_error", err.Error())
		return
	}
	available := sc.granted.Sub(sc.used)
	if available.Sign() < 0 {
		available = decimal.Zero
	}
	// model_limits mirrors upstream's per-token model allow-list. JuCode
	// restricts by model *group* instead, so the list is the group names the
	// key may route to — same question, different unit of restriction.
	limits := gin.H{}
	limitsEnabled := false
	if key != nil && len(key.AllowedGroupIds) > 0 {
		limitsEnabled = true
		for _, id := range key.AllowedGroupIds {
			limits[id.String()] = true
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"code":    true,
		"message": "ok",
		"data": gin.H{
			"object":        "token_usage",
			"name":          sc.name,
			"total_granted": sc.granted.InexactFloat64(),
			"total_used":    sc.used.InexactFloat64(),
			// Always false: JuCode has no unlimited-quota credential. The
			// field stays because upstream emits it and clients branch on it.
			"total_available":      available.InexactFloat64(),
			"unlimited_quota":      false,
			"model_limits":         limits,
			"model_limits_enabled": limitsEnabled,
			"expires_at":           sc.accessUntil,
			"currency":             biz.CurrencyCNY,
		},
	})
}

// compatError writes new-api's billing-handler error envelope: HTTP 200 with
// an OpenAI-shaped error object. The status code is deliberately 200 —
// upstream does the same, and clients parse the body.
func compatError(c *gin.Context, kind, msg string) {
	c.JSON(http.StatusOK, gin.H{
		"error": gin.H{"message": msg, "type": kind, "param": "", "code": nil},
	})
}

// middlewareUserID is requireUser with the compat error envelope, so a compat
// endpoint never leaks the native {"error": "..."} shape.
func middlewareUserID(c *gin.Context) (uuid.UUID, bool) {
	uid, ok := middleware.UserIDFrom(c)
	if !ok {
		compatError(c, "new_api_error", "unauthorized")
		return uuid.Nil, false
	}
	return uid, true
}
