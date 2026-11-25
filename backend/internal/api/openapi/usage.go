package openapi

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/cache"
	"github.com/jucode/jucode/internal/data/ent"
	entusagelog "github.com/jucode/jucode/internal/data/ent/usagelog"
	"github.com/jucode/jucode/internal/middleware"
)

// GET /v1/open/usage — a time series of your own traffic.
//
// Windows are snapped to the bucket boundary before use. That is not
// cosmetic: the Redis hour-counter fast path only answers windows starting
// exactly on a UTC hour, and the response cache keys on the window, so an
// unsnapped `now - 24h` computed fresh on every poll would miss both and turn
// a polling client into a repeated full scan of usage_logs.
func (h *Handler) handleUsage(c *gin.Context) {
	uid, ok := requireUser(c)
	if !ok {
		return
	}
	f := callerScope(uid)
	f.Start, f.End, f.Bucket = parseWindow(c)
	f.Model = strings.TrimSpace(c.Query("model"))
	f.Status = strings.TrimSpace(c.Query("status"))

	const ttl = 30 * time.Second
	f.SnapWindow(ttl)

	key := "openapi:usage:" + uid.String() + ":" + f.CacheSuffix()
	out, hit, err := cache.GetOrCompute[usageSeries](c.Request.Context(), h.cache, key, ttl,
		func(ctx context.Context) (usageSeries, error) {
			buckets, err := h.stats.Aggregate(ctx, f)
			if err != nil {
				return usageSeries{}, err
			}
			return newUsageSeries(f, buckets), nil
		})
	setCacheHeader(c, hit)
	if err != nil {
		fail(c, http.StatusInternalServerError, err.Error())
		return
	}
	c.JSON(http.StatusOK, out)
}

type usageSeries struct {
	Start   time.Time         `json:"start"`
	End     time.Time         `json:"end"`
	Bucket  string            `json:"bucket"`
	Buckets []biz.StatsBucket `json:"buckets"`
	Totals  usageTotals       `json:"totals"`
}

// usageTotals saves every caller from summing the series themselves, which is
// the first thing all of them do.
type usageTotals struct {
	Requests              int   `json:"requests"`
	Errors                int   `json:"errors"`
	TokensIn              int64 `json:"tokens_in"`
	TokensOut             int64 `json:"tokens_out"`
	CachedTokensIn        int64 `json:"cached_tokens_in"`
	CacheCreationTokensIn int64 `json:"cache_creation_tokens_in"`
	// Audio is reported separately for the same reason the cache counts are:
	// tokens_in/tokens_out are the text-only remainder, so these are what let a
	// caller add the modalities back up and reconcile them against cost.
	AudioTokensIn  int64  `json:"audio_tokens_in"`
	AudioTokensOut int64  `json:"audio_tokens_out"`
	Cost           string `json:"cost"`
	Currency       string `json:"currency"`
}

func newUsageSeries(f biz.StatsFilter, buckets []biz.StatsBucket) usageSeries {
	totals := usageTotals{Currency: biz.CurrencyCNY}
	cost := decimal.Zero
	for _, b := range buckets {
		totals.Requests += b.Requests
		totals.Errors += b.Errors
		totals.TokensIn += b.TokensIn
		totals.TokensOut += b.TokensOut
		totals.CachedTokensIn += b.CachedTokensIn
		totals.CacheCreationTokensIn += b.CacheCreationTokensIn
		totals.AudioTokensIn += b.AudioTokensIn
		totals.AudioTokensOut += b.AudioTokensOut
		if d, err := decimal.NewFromString(b.CostFinalSum); err == nil {
			cost = cost.Add(d)
		}
	}
	totals.Cost = cost.String()
	if buckets == nil {
		buckets = []biz.StatsBucket{}
	}
	return usageSeries{Start: f.Start, End: f.End, Bucket: f.Bucket, Buckets: buckets, Totals: totals}
}

// GET /v1/open/usage/models — the same window, grouped by model instead of by
// time. This is the "what am I actually spending money on" view.
func (h *Handler) handleUsageByModel(c *gin.Context) {
	uid, ok := requireUser(c)
	if !ok {
		return
	}
	f := callerScope(uid)
	f.Start, f.End, f.Bucket = parseWindow(c)

	const ttl = 30 * time.Second
	f.SnapWindow(ttl)

	key := "openapi:usage-models:" + uid.String() + ":" + f.CacheSuffix()
	out, hit, err := cache.GetOrCompute[usageByModel](c.Request.Context(), h.cache, key, ttl,
		func(ctx context.Context) (usageByModel, error) {
			rows, err := h.stats.Breakdown(ctx, "model", f)
			if err != nil {
				return usageByModel{}, err
			}
			if rows == nil {
				rows = []biz.StatsBreakdownRow{}
			}
			return usageByModel{Start: f.Start, End: f.End, Models: rows, Currency: biz.CurrencyCNY}, nil
		})
	setCacheHeader(c, hit)
	if err != nil {
		fail(c, http.StatusInternalServerError, err.Error())
		return
	}
	c.JSON(http.StatusOK, out)
}

type usageByModel struct {
	Start    time.Time               `json:"start"`
	End      time.Time               `json:"end"`
	Models   []biz.StatsBreakdownRow `json:"models"`
	Currency string                  `json:"currency"`
}

// GET /v1/open/logs — one row per request, newest first.
//
// The admin-only columns on usage_logs (latency_detail, error_class,
// upstream_request_id, upstream_raw_error) are not projected here. They name
// upstream accounts and carry provider error details; a user needs to know
// that their request failed and roughly why, not which of our upstream
// accounts served it.
func (h *Handler) handleLogs(c *gin.Context) {
	uid, ok := requireUser(c)
	if !ok {
		return
	}
	limit, offset := parsePage(c, 50, 200)

	// scope=self —— 只看「本凭证自己产生的」用量。
	//
	// 默认口径是「这个用户的全部用量」，对第三方应用（OAuth 客户端）来说那是
	// 错的：它会把用户在 CLI、Desktop、别的 API key 上的调用一起显示出来，
	// 应用自己的消耗被淹没在里面，也对不出自己这一份账。
	// 只有 OAuth 授权调用才有 authorization_id，API key 调用没有，所以这个
	// 参数只对 OAuth 主体生效；用 API key 调时给了也不会误伤（authz 为 nil）。
	selfOnly := strings.TrimSpace(c.Query("scope")) == "self"
	authzID := middleware.AuthzIDFrom(c)

	key := "openapi:logs:" + uid.String() + ":" + c.Request.URL.RawQuery
	out, hit, err := cache.GetOrCompute[logsPage](c.Request.Context(), h.cache, key, 11*time.Second,
		func(ctx context.Context) (logsPage, error) {
			q := h.users.UsageLog.Query().Where(entusagelog.UserIDEQ(uid))
			if selfOnly && authzID != nil {
				q = q.Where(entusagelog.AuthorizationIDEQ(*authzID))
			}
			if v := strings.TrimSpace(c.Query("model")); v != "" {
				q = q.Where(entusagelog.ModelContainsFold(v))
			}
			if v := strings.TrimSpace(c.Query("status")); v != "" {
				q = q.Where(entusagelog.StatusEQ(entusagelog.Status(v)))
			}
			if v := strings.TrimSpace(c.Query("endpoint")); v != "" {
				q = q.Where(entusagelog.EndpointEQ(entusagelog.Endpoint(v)))
			}
			if t := parseTime(c.Query("start")); !t.IsZero() {
				q = q.Where(entusagelog.CreatedAtGTE(t))
			}
			if t := parseTime(c.Query("end")); !t.IsZero() {
				q = q.Where(entusagelog.CreatedAtLT(t))
			}
			total, err := q.Clone().Count(ctx)
			if err != nil {
				return logsPage{}, err
			}
			rows, err := q.Order(ent.Desc(entusagelog.FieldCreatedAt)).Limit(limit).Offset(offset).All(ctx)
			if err != nil {
				return logsPage{}, err
			}
			page := logsPage{Logs: make([]logRow, 0, len(rows)), Total: total, Limit: limit, Offset: offset}
			for _, r := range rows {
				page.Logs = append(page.Logs, logRow{
					ID:                      r.ID.String(),
					RequestID:               r.RequestID,
					CreatedAt:               r.CreatedAt,
					Endpoint:                string(r.Endpoint),
					Model:                   r.Model,
					TokensIn:                r.TokensIn,
					TokensOut:               r.TokensOut,
					CachedTokensIn:          r.CachedTokensIn,
					CacheCreationTokensIn:   r.CacheCreationTokensIn,
					CacheCreation5mTokensIn: r.CacheCreation5mTokensIn,
					CacheCreation1hTokensIn: r.CacheCreation1hTokensIn,
					AudioTokensIn:           r.AudioTokensIn,
					AudioTokensOut:          r.AudioTokensOut,
					Cost:                    r.CostFinal.String(),
					Currency:                r.Currency,
					BillingSource:           string(r.BillingSource),
					Status:                  string(r.Status),
					HTTPStatus:              r.HTTPStatus,
					LatencyMs:               r.LatencyMs,
					Stream:                  r.Stream,
					ReasoningEffort:         r.ReasoningEffort,
					APIKeyName:              r.APIKeyName,
				})
			}
			return page, nil
		})
	setCacheHeader(c, hit)
	if err != nil {
		fail(c, http.StatusInternalServerError, err.Error())
		return
	}
	c.JSON(http.StatusOK, out)
}

type logsPage struct {
	Logs   []logRow `json:"logs"`
	Total  int      `json:"total"`
	Limit  int      `json:"limit"`
	Offset int      `json:"offset"`
}

type logRow struct {
	ID                      string    `json:"id"`
	RequestID               string    `json:"request_id"`
	CreatedAt               time.Time `json:"created_at"`
	Endpoint                string    `json:"endpoint"`
	Model                   string    `json:"model"`
	TokensIn                int       `json:"tokens_in"`
	TokensOut               int       `json:"tokens_out"`
	CachedTokensIn          int       `json:"cached_tokens_in"`
	CacheCreationTokensIn   int       `json:"cache_creation_tokens_in"`
	CacheCreation5mTokensIn int       `json:"cache_creation_5m_tokens_in"`
	CacheCreation1hTokensIn int       `json:"cache_creation_1h_tokens_in"`
	AudioTokensIn           int       `json:"audio_tokens_in"`
	AudioTokensOut          int       `json:"audio_tokens_out"`
	Cost                    string    `json:"cost"`
	Currency                string    `json:"currency"`
	BillingSource           string    `json:"billing_source"`
	Status                  string    `json:"status"`
	HTTPStatus              int       `json:"http_status"`
	LatencyMs               int       `json:"latency_ms"`
	Stream                  bool      `json:"stream"`
	ReasoningEffort         string    `json:"reasoning_effort"`
	APIKeyName              string    `json:"api_key_name"`
}
