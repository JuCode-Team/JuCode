package cache

// Centralized cache key registry. Every entry must be busted from at
// least one admin mutating endpoint (or rely on TTL when the underlying
// data is best-effort fresh). When you add a new key here, also add the
// invalidate call in the corresponding admin handler — grep this file
// to find the wire-up site.
const (
	// Site settings — single-row config; busted by PUT /v1/admin/settings.
	KeySiteInfo = "cache:public:site-info"

	// Active subscription plans — list; busted by plan CUD.
	KeyPlansActive = "cache:public:plans:active"

	// Vendor list — small static-ish master data; busted by vendor CUD.
	KeyVendors = "cache:public:vendors"

	// Enabled public models + their vendor join. Busted by model CUD
	// AND vendor CUD (models embed vendor name/slug).
	KeyModelsPublic = "cache:public:models:v2"

	// new-api-compatible catalog behind GET /api/pricing. Same source data
	// as KeyModelsPublic but a different assembled shape (it also carries
	// the group list and the derived ratios), so it needs its own entry —
	// and therefore has to be busted everywhere KeyModelsPublic is.
	KeyPricingCompat = "cache:public:pricing:v2"

	// Public uptime snapshot — busted on TTL only (probe rows update
	// hourly, 30s TTL is plenty).
	KeyStatusSnapshot = "cache:public:status"

	// Per-group running state for the API key picker, including exclusive
	// groups the public snapshot hides. TTL-only, same reason as
	// KeyStatusSnapshot: it summarizes recent traffic.
	KeyGroupLiveStates = "cache:groups:live-states"

	// Mirror of the OpenAI / Anthropic vendor status pages. Unlike the
	// keys above this is not a read-through cache: the leader's poll loop
	// (biz.UpstreamStatusSvc) is the sole writer and /public/status only
	// reads it, so there is nothing to bust — a stale record is detected
	// by its own polled_at timestamp and rendered as "unknown".
	KeyUpstreamStatus = "cache:public:upstream-status"

	// Budget summary — shared by GET /v1/public/budget. Busted on TTL (60s)
	// plus provider mutations.
	KeyBudgetSummary = "cache:budget:summary"

	// Admin overview headline — DashboardSvc.GetOverview() multi-table
	// aggregation. 30s TTL.
	KeyAdminOverview = "cache:admin:stats:overview"

	// Rolling-hour provider latency/success aggregation backing the admin
	// provider table (three percentile_cont sorts over usage_logs). Computed
	// for all providers at once so the paginated list and the detail page
	// share one entry. TTL-only: it summarizes traffic, not configuration, so
	// no provider mutation can make it wrong — only time can.
	KeyProviderPerformance = "cache:admin:providers:performance"

	// Admin observability aggregations (admin_observability.go). Prefixes,
	// not full keys: the suffix encodes dim/window/entity plus the snapped
	// window start, so distinct charts get distinct entries while rolling
	// polls of the same chart share one. TTL-only (30s) for the same reason
	// as KeyProviderPerformance — they summarize traffic, and no admin
	// mutation can make a traffic summary wrong.
	KeyObservabilitySeriesPrefix          = "cache:admin:obs:series:"
	KeyObservabilityLeaderboardPrefix     = "cache:admin:obs:leaderboard:"
	KeyObservabilitySLAPrefix             = "cache:admin:obs:sla:"
	KeyObservabilityProviderAttemptPrefix = "cache:admin:obs:provider_attempts:"
	KeyObservabilityErrorsPrefix          = "cache:admin:obs:errors:"
)
