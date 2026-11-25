// user_stats.go: read-only browsers over the calling user's own usage
// and billing rows, scoped by the account context. Personal context pins
// to the caller's personal rows; organization context (verified by the
// OrgContext middleware) widens to the whole org for owners/admins or the
// caller's own org rows for members. Query param "user_id" is NEVER
// trusted here.
package user

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/cache"
	"github.com/jucode/jucode/internal/data/ent"
	entbalancetx "github.com/jucode/jucode/internal/data/ent/balancetx"
	entbillinghold "github.com/jucode/jucode/internal/data/ent/billinghold"
	entgroup "github.com/jucode/jucode/internal/data/ent/modelgroup"
	"github.com/jucode/jucode/internal/data/ent/predicate"
	entusagelog "github.com/jucode/jucode/internal/data/ent/usagelog"
	entuser "github.com/jucode/jucode/internal/data/ent/user"
	"github.com/jucode/jucode/internal/middleware"
)

// statsScope is the account-context view a /me stats/logs read runs
// under, derived once per request from the OrgContext middleware values.
// Handlers must never widen it — membership was already verified there.
type statsScope struct {
	UserID         *uuid.UUID // set → restrict to this user's rows
	OrganizationID *uuid.UUID // set → restrict to this organization's rows
	PersonalOnly   bool       // personal context: exclude org-billed rows
	Manager        bool       // org context with owner/admin role
	// CacheKey uniquely identifies the visible data set. Every Redis key
	// for scoped reads must embed it — a key missing the scope would leak
	// one context's numbers into another.
	CacheKey string
}

// statsScopeFrom resolves the caller's scope, honouring the optional
// member_user_id drill-down filter for org managers. Writes the error
// response itself when it returns ok=false.
func (h *Handler) statsScopeFrom(c *gin.Context) (statsScope, bool) {
	uid, ok := middleware.UserIDFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "no user"})
		return statsScope{}, false
	}
	orgID, role, has := middleware.OrgContextFrom(c)
	if !has {
		return statsScope{UserID: &uid, PersonalOnly: true, CacheKey: uid.String()}, true
	}
	if !middleware.IsOrgManager(role) {
		return statsScope{OrganizationID: &orgID, UserID: &uid, CacheKey: "org:" + orgID.String() + ":u:" + uid.String()}, true
	}
	sc := statsScope{OrganizationID: &orgID, Manager: true, CacheKey: "org:" + orgID.String()}
	if v := strings.TrimSpace(c.Query("member_user_id")); v != "" {
		mid, err := uuid.Parse(v)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid member_user_id"})
			return statsScope{}, false
		}
		sc.UserID = &mid
		sc.CacheKey += ":m:" + mid.String()
	}
	return sc, true
}

func (sc statsScope) statsFilter() biz.StatsFilter {
	return biz.StatsFilter{
		UserID:         sc.UserID,
		OrganizationID: sc.OrganizationID,
		PersonalOnly:   sc.PersonalOnly,
	}
}

type usageLogRow struct {
	ID                      string    `json:"id"`
	RequestID               string    `json:"request_id"`
	CreatedAt               time.Time `json:"created_at"`
	Endpoint                string    `json:"endpoint"`
	Model                   string    `json:"model"`
	UpstreamModel           string    `json:"upstream_model"`
	TokensIn                int       `json:"tokens_in"`
	TokensOut               int       `json:"tokens_out"`
	CachedTokensIn          int       `json:"cached_tokens_in"`
	CacheCreationTokensIn   int       `json:"cache_creation_tokens_in"`
	CacheCreation5mTokensIn int       `json:"cache_creation_5m_tokens_in"`
	CacheCreation1hTokensIn int       `json:"cache_creation_1h_tokens_in"`
	ImageTokensIn           int       `json:"image_tokens_in"`
	ImageTokensOut          int       `json:"image_tokens_out"`
	AudioTokensIn           int       `json:"audio_tokens_in"`
	AudioTokensOut          int       `json:"audio_tokens_out"`
	CacheWriteEstimated     bool      `json:"cache_write_estimated"`
	OutputImages            int       `json:"output_images"`
	CostBase                string    `json:"cost_base"`
	CostFinal               string    `json:"cost_final"`
	Currency                string    `json:"currency"`
	BillingSource           string    `json:"billing_source"`
	// Group is the model group that actually served the call, and
	// GroupSwitches how many times auto-route had to move the request
	// between groups to get there (0 for the overwhelming majority).
	// Together they answer "why did this cost what it cost" for a key with
	// auto-route on. Upstream channel identity stays admin-only — the group
	// is a concept the user already picks in Lab and in key allowlists.
	GroupID       string `json:"group_id,omitempty"`
	Group         string `json:"group,omitempty"`
	GroupSwitches int    `json:"group_switches,omitempty"`
	Status        string `json:"status"`
	HTTPStatus    int    `json:"http_status"`
	// ReasonCode / Reason are the sanitized user-facing failure explanation
	// derived from the admin-only error_class + upstream body. Empty for ok
	// rows. The raw upstream body is never exposed here.
	ReasonCode string `json:"reason_code,omitempty"`
	Reason     string `json:"reason,omitempty"`
	LatencyMs  int    `json:"latency_ms"`
	TTFTMs     *int   `json:"ttft_ms"`
	TTFBMs     *int   `json:"ttfb_ms"`
	// FirstOutputMs is when the model started producing output, readable or
	// not — a reasoning item whose content is encrypted counts. FirstFrameMs
	// is when the upstream's first SSE frame arrived, which is a weaker proxy:
	// a Responses stream opens with lifecycle frames that say nothing about
	// the model. Both can be earlier than ttft_ms, which includes readable
	// reasoning deltas as well as visible text. Encrypted reasoning can still
	// delay ttft until the first readable token. The per-request TPS on the usage
	// page starts its window at the earliest of the three — otherwise the
	// thinking tokens, which the upstream counts in tokens_out, would be
	// divided by the answer's time alone. Null on rows that never took a
	// frame (non-streamed requests).
	FirstOutputMs   *int    `json:"first_output_ms"`
	FirstFrameMs    *int    `json:"first_frame_ms"`
	RetryCount      int     `json:"retry_count"`
	IP              string  `json:"ip"`
	UserAgent       string  `json:"user_agent"`
	ReasoningEffort string  `json:"reasoning_effort"`
	Stream          bool    `json:"stream"`
	APIKeyName      string  `json:"api_key_name"`
	APIKeyID        *string `json:"api_key_id,omitempty"`
	// UserID / UserEmail identify which org member produced the row. Only
	// populated for the org owner/admin view — personal and member scopes
	// only ever see the caller's own rows.
	UserID    string `json:"user_id,omitempty"`
	UserEmail string `json:"user_email,omitempty"`
}

type usageLogsPage struct {
	Logs  []usageLogRow `json:"logs"`
	Total int           `json:"total"`
	// TotalCapped marks Total as "at least" (biz.UsageLogCountCap hit).
	TotalCapped bool `json:"total_capped,omitempty"`
	Limit       int  `json:"limit"`
	Offset      int  `json:"offset"`
	// NextCursor pages forward without OFFSET (?cursor=); empty on the
	// last page.
	NextCursor string `json:"next_cursor,omitempty"`
	// DefaultStart is the lower bound the server applied because the
	// request had no start (biz.UsageLogDefaultWindow before end/now).
	DefaultStart *time.Time `json:"default_start,omitempty"`
}

// usageLogBadRequest is a filter-validation failure surfaced as 400; it
// travels through cache.GetOrCompute's error return.
type usageLogBadRequest string

func (e usageLogBadRequest) Error() string { return string(e) }

// latencyDetailMs lifts one admin-only latency_detail timing into the
// user-facing row. Nil-safe: the detail is optional and absent on rows
// written before the field existed.
func latencyDetailMs(detail map[string]int, key string) *int {
	v, ok := detail[key]
	if !ok {
		return nil
	}
	return &v
}

func (h *Handler) handleMyStatsUsageByModel(c *gin.Context) {
	scope, ok := h.statsScopeFrom(c)
	if !ok {
		return
	}
	f := scope.statsFilter()
	f.Bucket = c.DefaultQuery("bucket", "day")
	if v := c.Query("start"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			f.Start = t
		}
	}
	if v := c.Query("end"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			f.End = t
		}
	}
	// Same rolling-window problem as handleMyStatsUsage — snap to the shared
	// cachedStats TTL so a 30s poll reuses one key instead of minting a new
	// one per request. AggregateByModel has no hour-ring fast path, so every
	// miss here is a real SQL aggregation.
	f.SnapWindow(cachedStatsTTL)
	cacheKey := "me:stats:by-model:" + scope.CacheKey + ":" + f.CacheSuffix()
	rows, err := cachedStats(c, h, cacheKey, func(ctx context.Context) ([]biz.AggregateByModelRow, error) {
		return h.stats.AggregateByModel(ctx, f)
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"start": f.Start, "end": f.End, "bucket": f.Bucket, "rows": rows,
	})
}

// handleMyStatsUsageByKey serves the API-key dimension of the 统计 panel:
// which key spent the window's money. Same window/scope contract as
// handleMyStatsUsageByModel, minus the bucket in the answer — the response is
// a ranked list, not a series. The bucket parameter is still parsed because
// SnapWindow quantizes Start by it, and the cache key must match the sibling
// endpoints' notion of "the same window".
func (h *Handler) handleMyStatsUsageByKey(c *gin.Context) {
	scope, ok := h.statsScopeFrom(c)
	if !ok {
		return
	}
	f := scope.statsFilter()
	f.Bucket = c.DefaultQuery("bucket", "day")
	if v := c.Query("start"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			f.Start = t
		}
	}
	if v := c.Query("end"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			f.End = t
		}
	}
	// No hour-ring fast path here either (the Redis counters are not keyed by
	// api_key), so every miss is a real aggregation — snap to the TTL.
	f.SnapWindow(cachedStatsTTL)
	cacheKey := "me:stats:by-key:" + scope.CacheKey + ":" + f.CacheSuffix()
	rows, err := cachedStats(c, h, cacheKey, func(ctx context.Context) ([]biz.AggregateByAPIKeyRow, error) {
		return h.stats.AggregateByAPIKey(ctx, f)
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"start": f.Start, "end": f.End, "rows": rows,
	})
}

// cachedStatsTTL is also the window-snapping step for the handlers that use
// cachedStats: the key is only stable if the window it encodes is quantized to
// the same interval the entry lives for.
const cachedStatsTTL = 30 * time.Second

// cachedStats wraps a per-user stats aggregation in a short-TTL cache.
// 30s staleness is invisible on a dashboard but keeps repeat renders
// (tab switches, auto-refresh) off the multi-GB usage_logs table.
func cachedStats[T any](c *gin.Context, h *Handler, key string, fn func(context.Context) (T, error)) (T, error) {
	v, _, err := cache.GetOrCompute(c.Request.Context(), h.cache, key, cachedStatsTTL, fn)
	return v, err
}

func (h *Handler) handleMyStatsUsage(c *gin.Context) {
	scope, ok := h.statsScopeFrom(c)
	if !ok {
		return
	}
	f := scope.statsFilter()
	f.Bucket = c.DefaultQuery("bucket", "day")
	f.Model = c.Query("model")
	f.Status = c.Query("status")
	if v := c.Query("start"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err == nil {
			f.Start = t
		}
	}
	if v := c.Query("end"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err == nil {
			f.End = t
		}
	}
	// Short TTL only when the hour counters can serve the miss cheaply.
	ttl := 30 * time.Second
	if h.stats.HourBucketsEnabled() && f.Bucket == "hour" {
		ttl = 5 * time.Second
	}
	// Snap before the response echoes Start/End, and before the key is built.
	// The console recomputes a rolling window per poll, so without this the
	// window is unique every time: the key never repeats (100% miss) and the
	// unaligned Start disqualifies the Redis hour ring, sending every poll to
	// a SQL aggregation. Snapping to the TTL makes consecutive polls agree.
	f.SnapWindow(ttl)
	cacheKey := "me:stats:usage:" + scope.CacheKey + ":" + f.CacheSuffix()
	buckets, _, err := cache.GetOrCompute(c.Request.Context(), h.cache, cacheKey, ttl, func(ctx context.Context) ([]biz.StatsBucket, error) {
		return h.stats.Aggregate(ctx, f)
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"start": f.Start, "end": f.End, "bucket": f.Bucket, "buckets": buckets,
	})
}

// handleMyStatsLifetime serves the all-time totals card on the user
// overview. Longer TTL than the windowed stats — the numbers move
// slowly relative to their magnitude, and the underlying scan touches
// every historical row of the caller.
func (h *Handler) handleMyStatsLifetime(c *gin.Context) {
	scope, ok := h.statsScopeFrom(c)
	if !ok {
		return
	}
	cacheKey := "me:stats:lifetime:" + scope.CacheKey
	out, _, err := cache.GetOrCompute(c.Request.Context(), h.cache, cacheKey, 120*time.Second, func(ctx context.Context) (biz.LifetimeStats, error) {
		return h.stats.Lifetime(ctx, scope.statsFilter())
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, out)
}

func (h *Handler) handleMyUsageLogs(c *gin.Context) {
	scope, ok := h.statsScopeFrom(c)
	if !ok {
		return
	}
	limit := 50
	if v := c.Query("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}
	offset := 0
	if v := c.Query("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			offset = n
		}
	}
	// A cursor replaces offset (see admin listUsageLogs).
	cursorParam := strings.TrimSpace(c.Query("cursor"))
	if cursorParam != "" {
		offset = 0
	}
	cacheKey := "me:usage-logs:" + scope.CacheKey + ":" + c.Request.URL.RawQuery
	out, _, err := cache.GetOrCompute[usageLogsPage](c.Request.Context(), h.cache, cacheKey, 11*time.Second, func(ctx context.Context) (usageLogsPage, error) {
		// Filter set — all params are optional. `q` is a fuzzy match across
		// request_id / model / upstream_model / api_key_name. The rest are
		// strict equality (enums) or substring (ip / api_key) / time range.
		// Every filter lands in preds so the capped count and the page
		// query share one WHERE clause.
		var preds []predicate.UsageLog
		empty := usageLogsPage{Logs: []usageLogRow{}, Limit: limit, Offset: offset}
		if scope.UserID != nil {
			preds = append(preds, entusagelog.UserIDEQ(*scope.UserID))
		}
		if scope.OrganizationID != nil {
			preds = append(preds, entusagelog.OrganizationIDEQ(*scope.OrganizationID))
		} else if scope.PersonalOnly {
			preds = append(preds, entusagelog.OrganizationIDIsNil())
		}
		if q := strings.TrimSpace(c.Query("q")); q != "" {
			if utf8.RuneCountInString(q) < biz.UsageLogMinQueryLen {
				return empty, usageLogBadRequest("搜索词至少 3 个字符")
			}
			preds = append(preds, entusagelog.Or(
				entusagelog.RequestIDContainsFold(q),
				entusagelog.ModelContainsFold(q),
				entusagelog.UpstreamModelContainsFold(q),
				entusagelog.APIKeyNameContainsFold(q),
			))
		}
		if s := c.Query("status"); s != "" {
			preds = append(preds, entusagelog.StatusEQ(entusagelog.Status(s)))
		}
		if v := c.Query("endpoint"); v != "" {
			preds = append(preds, entusagelog.EndpointEQ(entusagelog.Endpoint(v)))
		}
		if v := c.Query("billing_source"); v != "" {
			preds = append(preds, entusagelog.BillingSourceEQ(entusagelog.BillingSource(v)))
		}
		if v := strings.TrimSpace(c.Query("model")); v != "" {
			p, err := h.stats.UsageLogModelFilter(ctx, v)
			if err != nil {
				return empty, err
			}
			preds = append(preds, p)
		}
		if v := strings.TrimSpace(c.Query("ip")); v != "" {
			preds = append(preds, entusagelog.IPContainsFold(v))
		}
		if v := strings.TrimSpace(c.Query("api_key")); v != "" {
			preds = append(preds, entusagelog.APIKeyNameContainsFold(v))
		}
		if v := strings.TrimSpace(c.Query("group_id")); v != "" {
			gid, err := uuid.Parse(v)
			if err != nil {
				return empty, usageLogBadRequest("invalid group_id")
			}
			preds = append(preds, entusagelog.GroupIDEQ(gid))
		}
		var requestedStart, requestedEnd *time.Time
		if v := c.Query("start"); v != "" {
			if t, err := time.Parse(time.RFC3339, v); err == nil {
				requestedStart = &t
			}
		}
		if v := c.Query("end"); v != "" {
			if t, err := time.Parse(time.RFC3339, v); err == nil {
				requestedEnd = &t
			}
		}
		// No start means the last 7 days: a heavy user's unbounded count
		// bitmap-scans every row they ever produced.
		start := biz.DefaultUsageLogStart(time.Now(), requestedStart, requestedEnd)
		var defaultStart *time.Time
		if requestedStart == nil {
			defaultStart = &start
		}
		preds = append(preds, entusagelog.CreatedAtGTE(start))
		if requestedEnd != nil {
			preds = append(preds, entusagelog.CreatedAtLT(*requestedEnd))
		}
		total, capped, err := h.stats.CountUsageLogsCapped(ctx, preds, biz.UsageLogCountCap)
		if err != nil {
			return empty, err
		}
		query := h.users.UsageLog.Query().Where(preds...)
		if cursorParam != "" {
			ts, id, err := biz.DecodeUsageLogCursor(cursorParam)
			if err != nil {
				return empty, usageLogBadRequest(err.Error())
			}
			query = query.Where(biz.UsageLogBefore(ts, id))
		} else {
			query = query.Offset(offset)
		}
		logs, err := query.
			Order(ent.Desc(entusagelog.FieldCreatedAt), ent.Desc(entusagelog.FieldID)).
			Limit(limit).
			All(ctx)
		if err != nil {
			return empty, err
		}
		var nextCursor string
		if len(logs) == limit {
			last := logs[len(logs)-1]
			nextCursor = biz.EncodeUsageLogCursor(last.CreatedAt, last.ID)
		}
		// Org-manager view: attribute each row to the member that produced
		// it. One batch email lookup instead of a per-row join.
		emailByUser := map[uuid.UUID]string{}
		if scope.Manager {
			ids := make([]uuid.UUID, 0, len(logs))
			seen := map[uuid.UUID]struct{}{}
			for _, l := range logs {
				if _, dup := seen[l.UserID]; !dup {
					seen[l.UserID] = struct{}{}
					ids = append(ids, l.UserID)
				}
			}
			if len(ids) > 0 {
				users, err := h.users.User.Query().Where(entuser.IDIn(ids...)).All(ctx)
				if err != nil {
					return usageLogsPage{}, err
				}
				for _, u := range users {
					emailByUser[u.ID] = u.Email
				}
			}
		}
		// One batch lookup for the groups these rows landed in, same shape
		// as the email batch above.
		groupIDs := make([]uuid.UUID, 0, len(logs))
		seenGroup := map[uuid.UUID]struct{}{}
		for _, l := range logs {
			if l.GroupID == nil {
				continue
			}
			if _, dup := seenGroup[*l.GroupID]; dup {
				continue
			}
			seenGroup[*l.GroupID] = struct{}{}
			groupIDs = append(groupIDs, *l.GroupID)
		}
		groupNameByID := map[uuid.UUID]string{}
		if len(groupIDs) > 0 {
			groups, gerr := h.users.ModelGroup.Query().Where(entgroup.IDIn(groupIDs...)).All(ctx)
			if gerr != nil {
				return usageLogsPage{}, gerr
			}
			for _, g := range groups {
				groupNameByID[g.ID] = g.Name
			}
		}
		out := make([]usageLogRow, 0, len(logs))
		for _, l := range logs {
			row := usageLogRow{
				ID:                      l.ID.String(),
				RequestID:               l.RequestID,
				CreatedAt:               l.CreatedAt,
				Endpoint:                string(l.Endpoint),
				Model:                   l.Model,
				UpstreamModel:           l.UpstreamModel,
				TokensIn:                l.TokensIn,
				TokensOut:               l.TokensOut,
				CachedTokensIn:          l.CachedTokensIn,
				CacheCreationTokensIn:   l.CacheCreationTokensIn,
				CacheCreation5mTokensIn: l.CacheCreation5mTokensIn,
				CacheCreation1hTokensIn: l.CacheCreation1hTokensIn,
				ImageTokensIn:           l.ImageTokensIn,
				ImageTokensOut:          l.ImageTokensOut,
				AudioTokensIn:           l.AudioTokensIn,
				AudioTokensOut:          l.AudioTokensOut,
				CacheWriteEstimated:     l.CacheWriteEstimated,
				OutputImages:            l.OutputImages,
				CostBase:                l.CostBase.String(),
				CostFinal:               l.CostFinal.String(),
				Currency:                l.Currency,
				BillingSource:           string(l.BillingSource),
				Status:                  string(l.Status),
				HTTPStatus:              l.HTTPStatus,
				LatencyMs:               l.LatencyMs,
				TTFTMs:                  l.TtftMs,
				TTFBMs:                  l.TtfbMs,
				FirstOutputMs:           latencyDetailMs(l.LatencyDetail, "first_output_ms"),
				FirstFrameMs:            latencyDetailMs(l.LatencyDetail, "first_sse_frame_ms"),
				RetryCount:              l.RetryCount,
				IP:                      l.IP,
				UserAgent:               l.UserAgent,
				ReasoningEffort:         l.ReasoningEffort,
				Stream:                  l.Stream,
				APIKeyName:              l.APIKeyName,
			}
			if l.APIKeyID != nil {
				id := l.APIKeyID.String()
				row.APIKeyID = &id
			}
			if l.GroupID != nil {
				row.GroupID = l.GroupID.String()
				// A group deleted since the row was written stays unnamed
				// rather than leaking a bare UUID into the user-facing table.
				row.Group = groupNameByID[*l.GroupID]
			}
			if n, ok := l.LatencyDetail["cross_group_switches"]; ok && n > 0 {
				row.GroupSwitches = n
			}
			if scope.Manager {
				row.UserID = l.UserID.String()
				row.UserEmail = emailByUser[l.UserID]
			}
			if r := biz.ResolveUsageReason(string(l.Status), l.ErrorClass, l.HTTPStatus, l.UpstreamRawError, l.ErrorMessage); r.Code != "" {
				row.ReasonCode = r.Code
				row.Reason = r.Text
			}
			out = append(out, row)
		}
		return usageLogsPage{
			Logs: out, Total: total, TotalCapped: capped,
			Limit: limit, Offset: offset,
			NextCursor: nextCursor, DefaultStart: defaultStart,
		}, nil
	})
	if err != nil {
		var bad usageLogBadRequest
		if errors.As(err, &bad) {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, out)
}

// handleMyBilling lists the calling user's BalanceTx history — every
// topup (admin or redeem-code), refund, deduct (per-request charge)
// and adjust. Powers the /me/billing page.
//
// kind filter is optional; q does substring match on note + currency.
func (h *Handler) handleMyBilling(c *gin.Context) {
	uid, ok := middleware.UserIDFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "no user"})
		return
	}
	limit := 50
	if v := c.Query("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}
	offset := 0
	if v := c.Query("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			offset = n
		}
	}
	from, to := parseBillingRange(c)
	base := h.users.BalanceTx.Query().Where(entbalancetx.UserIDEQ(uid))
	if kind := strings.TrimSpace(c.Query("kind")); kind != "" {
		base = base.Where(entbalancetx.KindEQ(entbalancetx.Kind(kind)))
	}
	if q := strings.TrimSpace(c.Query("q")); q != "" {
		base = base.Where(entbalancetx.NoteContainsFold(q))
	}
	if !from.IsZero() {
		base = base.Where(entbalancetx.CreatedAtGTE(from))
	}
	if !to.IsZero() {
		base = base.Where(entbalancetx.CreatedAtLT(to))
	}
	total, err := base.Clone().Count(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	rows, err := base.
		Order(ent.Desc("created_at")).
		Limit(limit).
		Offset(offset).
		All(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	notes := make([]string, 0, len(rows))
	for _, r := range rows {
		if r.Kind == entbalancetx.KindDeduct {
			notes = append(notes, r.Note)
		}
	}
	holds, err := h.settleHoldAmounts(c.Request.Context(), entbillinghold.UserIDEQ(uid), notes)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	out := make([]gin.H, 0, len(rows))
	for _, r := range rows {
		row := gin.H{
			"id":            r.ID.String(),
			"created_at":    r.CreatedAt,
			"kind":          r.Kind,
			"amount":        r.Amount.String(),
			"balance_after": r.BalanceAfter.String(),
			"currency":      r.Currency,
			"note":          r.Note,
		}
		if id, ok := requestIDFromLedgerNote(r.Note); ok {
			if amt, found := holds[id]; found {
				row["hold_amount"] = amt.String()
			}
		}
		if r.RelatedUsageLogID != nil {
			row["related_usage_log_id"] = r.RelatedUsageLogID.String()
		}
		out = append(out, row)
	}
	c.JSON(http.StatusOK, gin.H{
		"items":  out,
		"total":  total,
		"limit":  limit,
		"offset": offset,
	})
}

// requestIDFromLedgerNote extracts the request id a settle/reserve note
// carries as its last token ("llm gateway settle <id>" ↔ "llm gateway
// reserve <id>"). It is the only handle joining a ledger row to its
// billing_hold — the hold's request_key is an independent server uuid by
// design (see newBillingHoldKey).
func requestIDFromLedgerNote(note string) (string, bool) {
	i := strings.LastIndexByte(note, ' ')
	if i < 0 || i+1 == len(note) {
		return "", false
	}
	tok := note[i+1:]
	// WS settle notes append the upstream response id
	// ("llm gateway settle <uuid>:<resp_…>"); the turn's reserve note
	// carries the bare request id.
	if j := strings.IndexByte(tok, ':'); j >= 0 {
		tok = tok[:j]
	}
	if _, err := uuid.Parse(tok); err != nil {
		return "", false
	}
	return tok, true
}

// heldAmount sums the reservations still frozen for one balance owner. The
// reserve already took them out of balance, so the UI shows this beside the
// balance to explain why it dropped mid-request. Only in-flight requests
// hold, so the row count stays small.
func (h *Handler) heldAmount(ctx context.Context, account predicate.BillingHold) (decimal.Decimal, error) {
	holds, err := h.users.BillingHold.Query().
		Where(account, entbillinghold.StatusEQ(entbillinghold.StatusHeld)).
		Select(entbillinghold.FieldAmount).
		All(ctx)
	if err != nil {
		return decimal.Zero, err
	}
	sum := decimal.Zero
	for _, hd := range holds {
		sum = sum.Add(hd.Amount)
	}
	return sum, nil
}

// settleHoldAmounts maps request ids parsed from settle ledger notes to the
// amount their reservation froze, so a deduct row can display "预扣 X".
// account scopes holds to one balance owner (user or organization). Only
// settled holds can match — the ledger row exists iff the settle
// committed. The latest matching hold wins because one request may hold
// twice (cross-group re-reserve, the video settle-fallback hold).
func (h *Handler) settleHoldAmounts(ctx context.Context, account predicate.BillingHold, notes []string) (map[string]decimal.Decimal, error) {
	ids := map[string]struct{}{}
	for _, n := range notes {
		if id, ok := requestIDFromLedgerNote(n); ok {
			ids[id] = struct{}{}
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	or := make([]predicate.BillingHold, 0, len(ids))
	for id := range ids {
		or = append(or, entbillinghold.NoteHasSuffix(id))
	}
	holds, err := h.users.BillingHold.Query().
		Where(account, entbillinghold.StatusEQ(entbillinghold.StatusSettled), entbillinghold.Or(or...)).
		Select(entbillinghold.FieldNote, entbillinghold.FieldAmount, entbillinghold.FieldCreatedAt).
		All(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]decimal.Decimal{}
	at := map[string]time.Time{}
	for _, hd := range holds {
		id, ok := requestIDFromLedgerNote(hd.Note)
		if !ok {
			continue
		}
		if t, seen := at[id]; !seen || hd.CreatedAt.After(t) {
			at[id] = hd.CreatedAt
			out[id] = hd.Amount
		}
	}
	return out, nil
}

func (h *Handler) handleMyBillingSummary(c *gin.Context) {
	uid, ok := middleware.UserIDFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "no user"})
		return
	}
	from, to := parseBillingRange(c)
	sum, err := h.account.BillingSummary(c.Request.Context(), uid, from, to)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, ledgerSummaryJSON(sum))
}

func parseBillingRange(c *gin.Context) (time.Time, time.Time) {
	from, fromDate := parseBillingTime(c.Query("from"))
	to, toDate := parseBillingTime(c.Query("to"))
	return biz.NormalizeLedgerRange(from, to, fromDate, toDate)
}

func parseBillingTime(v string) (time.Time, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
		return t, false
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, false
	}
	if t, err := time.ParseInLocation("2006-01-02T15:04:05", v, biz.Location()); err == nil {
		return t, false
	}
	if t, err := time.ParseInLocation("2006-01-02", v, biz.Location()); err == nil {
		return t, true
	}
	return time.Time{}, false
}

func ledgerSummaryJSON(sum *biz.LedgerSummary) gin.H {
	days := make([]gin.H, 0, len(sum.Days))
	for _, d := range sum.Days {
		days = append(days, gin.H{
			"date":    d.Date,
			"income":  d.Income.String(),
			"expense": d.Expense.String(),
		})
	}
	return gin.H{
		"from":    sum.From,
		"to":      sum.To,
		"income":  sum.Income.String(),
		"expense": sum.Expense.String(),
		"days":    days,
	}
}

// handleMyRate reports the caller's live RPM/TPM, shown above their usage log.
//
// Not cached: the widget's whole point is that it tracks the last 60 seconds,
// and the read is two pipelined HGETALLs against Redis — cheaper than the
// cache bookkeeping would be. The caps are resolved per call for the same
// reason; they come from a tiny indexed row.
//
// Always the caller's own rate, never the organization's: it mirrors what the
// per-user caps count, so the number lines up with the limit beside it.
// Identity comes from CtxUserID, like every other handler in this file.
// middleware.UserFrom reads CtxUser, which only the API-key (ClientAuth) and
// OAuth paths populate — the JWT middleware guarding /v1/me sets CtxUserID
// alone. Using UserFrom here made this endpoint 401 for every browser session,
// and because the widget polls, the frontend's 401 interceptor signed people
// out seconds after they opened the usage page.
func (h *Handler) handleMyRate(c *gin.Context) {
	uid, ok := middleware.UserIDFrom(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "no user"})
		return
	}
	rate, err := h.stats.UserRate(c.Request.Context(), uid)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	// Caps are a label on the widget, not the answer to it: if the row cannot
	// be read we still report the rate rather than failing the request.
	if h.users != nil {
		if u, uerr := h.users.User.Get(c.Request.Context(), uid); uerr == nil {
			rate.RPMLimit, rate.TPMLimit = h.rateLimit.EffectiveLimitsForUser(c.Request.Context(), u)
		}
	}
	c.JSON(http.StatusOK, rate)
}
