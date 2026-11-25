// admin_stats.go: admin-facing usage statistics and the raw usage_logs
// browser. Aggregation lives in biz.StatsSvc; these handlers shape the
// HTTP layer (query parsing, paging, response envelope).
package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/cache"
	"github.com/jucode/jucode/internal/data/ent"
	entgroup "github.com/jucode/jucode/internal/data/ent/modelgroup"
	"github.com/jucode/jucode/internal/data/ent/predicate"
	entprovider "github.com/jucode/jucode/internal/data/ent/provider"
	entusagelog "github.com/jucode/jucode/internal/data/ent/usagelog"
	entuser "github.com/jucode/jucode/internal/data/ent/user"
)

type usageLogRow struct {
	ID                      string            `json:"id"`
	RequestID               string            `json:"request_id"`
	UserID                  string            `json:"user_id"`
	UserEmail               string            `json:"user_email,omitempty"`
	UserName                string            `json:"user_name,omitempty"`     // optional username handle
	UserNickname            string            `json:"user_nickname,omitempty"` // display nickname
	Endpoint                string            `json:"endpoint"`
	Model                   string            `json:"model"`
	UpstreamModel           string            `json:"upstream_model"`
	TokensIn                int               `json:"tokens_in"`
	TokensOut               int               `json:"tokens_out"`
	CachedTokensIn          int               `json:"cached_tokens_in"`
	CacheCreationTokensIn   int               `json:"cache_creation_tokens_in"`
	CacheCreation5mTokensIn int               `json:"cache_creation_5m_tokens_in"`
	CacheCreation1hTokensIn int               `json:"cache_creation_1h_tokens_in"`
	ImageTokensIn           int               `json:"image_tokens_in"`
	ImageTokensOut          int               `json:"image_tokens_out"`
	AudioTokensIn           int               `json:"audio_tokens_in"`
	AudioTokensOut          int               `json:"audio_tokens_out"`
	CacheWriteEstimated     bool              `json:"cache_write_estimated"`
	OutputImages            int               `json:"output_images"`
	CostFinal               string            `json:"cost_final"`
	Currency                string            `json:"currency"`
	BillingSource           string            `json:"billing_source"`
	Status                  string            `json:"status"`
	HTTPStatus              int               `json:"http_status"`
	LatencyMs               int               `json:"latency_ms"`
	TTFTMs                  *int              `json:"ttft_ms"`
	TTFBMs                  *int              `json:"ttfb_ms"`
	LatencyDetail           map[string]int    `json:"latency_detail,omitempty"`
	RetryCount              int               `json:"retry_count"`
	CreatedAt               time.Time         `json:"created_at"`
	IP                      string            `json:"ip"`
	UserAgent               string            `json:"user_agent"`
	ReasoningEffort         string            `json:"reasoning_effort"`
	Stream                  bool              `json:"stream"`
	APIKeyName              string            `json:"api_key_name"`
	APIKeyID                *string           `json:"api_key_id,omitempty"`
	ErrorMessage            string            `json:"error_message,omitempty"`
	ErrorClass              string            `json:"error_class,omitempty"`         // structured outcome of the final attempt
	UpstreamRequestID       string            `json:"upstream_request_id,omitempty"` // provider's own request id (admin reconciliation)
	GroupID                 string            `json:"group_id,omitempty"`            // model group the request was finally billed against
	Group                   string            `json:"group,omitempty"`               // resolved name of that group
	Provider                string            `json:"provider,omitempty"`            // resolved name of the final provider
	ProviderChain           []string          `json:"provider_chain,omitempty"`      // resolved names of every provider tried
	Attempts                []usageAttemptRow `json:"attempts,omitempty"`
	Anomaly                 biz.AnomalyFlags  `json:"anomaly"` // see internal/biz/anomaly.go
}

type usageAttemptRow struct {
	Attempt          int            `json:"attempt"`
	ProviderID       string         `json:"provider_id"`
	Provider         string         `json:"provider"`
	ProviderName     string         `json:"provider_name,omitempty"`
	ProviderKind     string         `json:"provider_kind,omitempty"`
	UpstreamModel    string         `json:"upstream_model,omitempty"`
	StrategyPriority int            `json:"strategy_priority"`
	HTTPStatus       int            `json:"http_status,omitempty"`
	ErrorMessage     string         `json:"error_message,omitempty"`
	ErrorClass       string         `json:"error_class,omitempty"`
	SwitchStrategy   string         `json:"switch_strategy,omitempty"`
	LatencyMs        map[string]int `json:"latency_ms,omitempty"`
	Final            bool           `json:"final,omitempty"`
}

type usageLogsPage struct {
	Logs  []usageLogRow `json:"logs"`
	Total int           `json:"total"`
	// TotalCapped marks Total as "at least": counting stopped at
	// biz.UsageLogCountCap. The UI should render it as "10000+".
	TotalCapped bool `json:"total_capped,omitempty"`
	Limit       int  `json:"limit"`
	Offset      int  `json:"offset"`
	// NextCursor pages forward without OFFSET: pass it back as ?cursor=.
	// Empty on the last page.
	NextCursor string `json:"next_cursor,omitempty"`
	// DefaultStart is the lower time bound the server applied because the
	// request had none (biz.UsageLogDefaultWindow before end/now), so the
	// UI can say "showing the last 7 days" instead of implying all time.
	DefaultStart *time.Time `json:"default_start,omitempty"`
}

// usageLogBadRequest is a filter-validation failure surfaced as 400. It
// travels through cache.GetOrCompute's error return, which is why the
// handler has to classify errors after the fact.
type usageLogBadRequest string

func (e usageLogBadRequest) Error() string { return string(e) }

// usageLogUserMatchLimit caps how many users a fuzzy `user` filter may
// resolve to before the browser asks for a narrower needle. The IDs go
// into `user_id IN (...)`, which the (user_id, created_at) index serves;
// the old `user_id IN (SELECT ...)` subquery made the planner mis-estimate
// and bitmap-scan every matching row.
const usageLogUserMatchLimit = 200

// adminUsageLogColumns is every usage_logs column except
// upstream_raw_error: the admin row never serializes it, so reading it is
// pure heap I/O.
var adminUsageLogColumns = func() []string {
	cols := make([]string, 0, len(entusagelog.Columns))
	for _, c := range entusagelog.Columns {
		if c != entusagelog.FieldUpstreamRawError {
			cols = append(cols, c)
		}
	}
	return cols
}()

const usageAttemptPrefix = "attempt:"

const usageLogAnomalyMaxWindow = 24 * time.Hour

// boundedAnomalyStart enforces the usage_logs scan ceiling for anomaly
// searches. An explicit end anchors historical searches; otherwise the range
// ends now. Existing non-anomaly log queries retain their original bounds.
func boundedAnomalyStart(anomaly string, now time.Time, requestedStart, requestedEnd *time.Time) *time.Time {
	if anomaly == "" {
		return requestedStart
	}
	end := now
	if requestedEnd != nil {
		end = *requestedEnd
	}
	floor := end.Add(-usageLogAnomalyMaxWindow)
	if requestedStart != nil && requestedStart.After(floor) {
		return requestedStart
	}
	return &floor
}

func parseUsageAttemptEntry(s string) (usageAttemptRow, bool) {
	if !strings.HasPrefix(s, usageAttemptPrefix) {
		return usageAttemptRow{}, false
	}
	var a usageAttemptRow
	if err := json.Unmarshal([]byte(strings.TrimPrefix(s, usageAttemptPrefix)), &a); err != nil {
		return usageAttemptRow{}, false
	}
	if a.Provider == "" {
		a.Provider = a.ProviderName
	}
	return a, true
}

// parseStatsFilter extracts the shared {start, end, bucket, model,
// status, user_id, group_id} filter from query params. Wrong format
// → returns the partially-built filter plus a descriptive error.
func parseStatsFilter(c *gin.Context) (biz.StatsFilter, error) {
	f := biz.StatsFilter{
		Bucket: c.DefaultQuery("bucket", "day"),
		Model:  c.Query("model"),
		Status: c.Query("status"),
	}
	if v := c.Query("start"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return f, fmt.Errorf("start must be RFC3339: %w", err)
		}
		f.Start = t
	}
	if v := c.Query("end"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return f, fmt.Errorf("end must be RFC3339: %w", err)
		}
		f.End = t
	}
	if v := c.Query("user_id"); v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			return f, fmt.Errorf("invalid user_id: %w", err)
		}
		f.UserID = &id
	}
	if v := c.Query("group_id"); v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			return f, fmt.Errorf("invalid group_id: %w", err)
		}
		f.GroupID = &id
	}
	return f, nil
}

// statsUsageTTL matches the response-cache TTL to what a miss costs.
//
// The usage panel polls every 2s. When the hour counters serve the query a
// miss is one pipelined Redis round trip, so a short TTL is affordable and the
// chart actually tracks live traffic. When they are off (or the window is not
// one they can answer) a miss scans 24h of usage_logs, and the longer TTL is
// the only thing keeping that off the hot path.
func statsUsageTTL(stats *biz.StatsSvc, f biz.StatsFilter) time.Duration {
	if stats.HourBucketsEnabled() && f.Bucket == "hour" {
		return 5 * time.Second
	}
	return 30 * time.Second
}

func (h *Handler) handleStatsUsage(c *gin.Context) {
	f, err := parseStatsFilter(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	// Snap before echoing Start/End and before keying: any caller sending a
	// rolling `end` would otherwise mint a unique key per poll and, with an
	// unaligned Start, bypass the Redis hour ring into a full SQL aggregation.
	ttl := statsUsageTTL(h.stats, f)
	f.SnapWindow(ttl)
	cacheKey := "admin:stats:usage:" + f.CacheSuffix()
	buckets, hit, err := cache.GetOrCompute(c.Request.Context(), h.cache, cacheKey, ttl, func(ctx context.Context) ([]biz.StatsBucket, error) {
		return h.stats.Aggregate(ctx, f)
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	setCacheHeader(c, hit)
	c.JSON(http.StatusOK, gin.H{
		"start": f.Start, "end": f.End, "bucket": f.Bucket,
		"buckets": buckets,
	})
}

func (h *Handler) handleStatsBreakdown(c *gin.Context) {
	dim := c.DefaultQuery("by", "model")
	f, err := parseStatsFilter(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	// dim is no longer carried by RawQuery now that the key comes from the
	// parsed filter, so it needs its own segment.
	const breakdownTTL = 30 * time.Second
	f.SnapWindow(breakdownTTL)
	cacheKey := "admin:stats:breakdown:" + dim + ":" + f.CacheSuffix()
	rows, hit, err := cache.GetOrCompute(c.Request.Context(), h.cache, cacheKey, breakdownTTL, func(ctx context.Context) ([]biz.StatsBreakdownRow, error) {
		return h.stats.Breakdown(ctx, dim, f)
	})
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	setCacheHeader(c, hit)
	c.JSON(http.StatusOK, gin.H{"by": dim, "rows": rows})
}

func (h *Handler) listUsageLogs(c *gin.Context) {
	limit, offset := parsePage(c, 200)
	// The anomaly filter is validated before the cache lookup: a bad value
	// must 400 rather than cache an error page under its own key.
	anomalyParam := c.Query("anomaly")
	switch anomalyParam {
	case "", "any", "time", "error", "none":
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "anomaly must be one of any|time|error|none"})
		return
	}
	// A cursor replaces offset: the page starts right after the row the
	// cursor names, and the response echoes offset 0.
	cursorParam := strings.TrimSpace(c.Query("cursor"))
	if cursorParam != "" {
		offset = 0
	}
	cacheKey := "admin:usage-logs:" + c.Request.URL.RawQuery
	out, _, err := cache.GetOrCompute[usageLogsPage](c.Request.Context(), h.cache, cacheKey, 6*time.Second, func(ctx context.Context) (usageLogsPage, error) {
		// Every filter lands in preds so the capped count and the page
		// query see exactly the same WHERE clause.
		var preds []predicate.UsageLog
		empty := usageLogsPage{Logs: []usageLogRow{}, Limit: limit, Offset: offset}
		// Anomaly predicates come from biz/anomaly.go — the same definitions
		// the observability series counts with, so the drill-down from a
		// chart spike always finds the rows behind the number.
		switch anomalyParam {
		case "any":
			preds = append(preds, entusagelog.Or(biz.AnomalyTimePredicate(), biz.AnomalyErrorPredicate()))
		case "time":
			preds = append(preds, biz.AnomalyTimePredicate())
		case "error":
			preds = append(preds, biz.AnomalyErrorPredicate())
		case "none":
			preds = append(preds,
				entusagelog.Not(biz.AnomalyTimePredicate()),
				entusagelog.Not(biz.AnomalyErrorPredicate()),
			)
		}
		// Fuzzy: request_id / model / upstream_model / api_key_name. Four
		// ILIKEs can only scan, so a needle shorter than three characters
		// is refused rather than run.
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
		// request_id is pasted whole from a row or a support ticket, so it
		// matches exactly (the partial request_id index) instead of by
		// substring. Rows without a request_id show their primary key in
		// the UI, so a UUID also matches id. A point lookup needs no time
		// window.
		pointLookup := false
		if v := strings.TrimSpace(c.Query("request_id")); v != "" {
			p := entusagelog.RequestIDEQ(v)
			if id, err := uuid.Parse(v); err == nil {
				p = entusagelog.Or(p, entusagelog.IDEQ(id))
			}
			preds = append(preds, p)
			pointLookup = true
		}
		if v := strings.TrimSpace(c.Query("model")); v != "" {
			p, err := h.stats.UsageLogModelFilter(ctx, v)
			if err != nil {
				return empty, err
			}
			preds = append(preds, p)
		}
		if v := c.Query("status"); v != "" {
			preds = append(preds, entusagelog.StatusEQ(entusagelog.Status(v)))
		}
		if v := c.Query("endpoint"); v != "" {
			preds = append(preds, entusagelog.EndpointEQ(entusagelog.Endpoint(v)))
		}
		if v := c.Query("billing_source"); v != "" {
			preds = append(preds, entusagelog.BillingSourceEQ(entusagelog.BillingSource(v)))
		}
		if v := strings.TrimSpace(c.Query("error_class")); v != "" {
			preds = append(preds, entusagelog.ErrorClassEQ(v))
		}
		if v := strings.TrimSpace(c.Query("ip")); v != "" {
			preds = append(preds, entusagelog.IPContainsFold(v))
		}
		if v := strings.TrimSpace(c.Query("api_key")); v != "" {
			preds = append(preds, entusagelog.APIKeyNameContainsFold(v))
		}
		if v := c.Query("user_id"); v != "" {
			uid, err := uuid.Parse(v)
			if err != nil {
				return empty, usageLogBadRequest("invalid user_id")
			}
			preds = append(preds, entusagelog.UserIDEQ(uid))
		}
		// group_id answers "where did auto-route actually send this traffic",
		// which the stats endpoints could already filter on but the log
		// browser could not — the drill-down from a routing incident had
		// nowhere to land.
		if v := strings.TrimSpace(c.Query("group_id")); v != "" {
			gid, err := uuid.Parse(v)
			if err != nil {
				return empty, usageLogBadRequest("invalid group_id")
			}
			preds = append(preds, entusagelog.GroupIDEQ(gid))
		}
		// provider_id: the UI has sent it (and shown a "渠道" chip) since
		// the channel filter shipped; the handler never read it, so the
		// chip filtered nothing.
		if v := strings.TrimSpace(c.Query("provider_id")); v != "" {
			pid, err := uuid.Parse(v)
			if err != nil {
				return empty, usageLogBadRequest("invalid provider_id")
			}
			preds = append(preds, entusagelog.ProviderIDEQ(pid))
		}
		if v := strings.TrimSpace(c.Query("user")); v != "" {
			ids, err := h.users.User.Query().
				Where(entuser.Or(
					entuser.EmailContainsFold(v),
					entuser.NicknameContainsFold(v),
				)).
				Limit(usageLogUserMatchLimit + 1).
				IDs(ctx)
			if err != nil {
				return empty, err
			}
			if len(ids) > usageLogUserMatchLimit {
				return empty, usageLogBadRequest("匹配到的用户过多，请输入更完整的邮箱或昵称")
			}
			if len(ids) == 0 {
				return empty, nil
			}
			preds = append(preds, entusagelog.UserIDIn(ids...))
		}
		var requestedStart, requestedEnd *time.Time
		if v := c.Query("start"); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				return empty, usageLogBadRequest("start must be RFC3339")
			}
			requestedStart = &t
		}
		if v := c.Query("end"); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				return empty, usageLogBadRequest("end must be RFC3339")
			}
			requestedEnd = &t
		}
		// Lower time bound: anomaly searches keep their 24h ceiling, a
		// request_id lookup needs none, and everything else defaults to
		// the last 7 days unless the caller set a start.
		var start *time.Time
		var defaultStart *time.Time
		switch {
		case anomalyParam != "":
			start = boundedAnomalyStart(anomalyParam, time.Now(), requestedStart, requestedEnd)
		case pointLookup:
			start = requestedStart
		default:
			t := biz.DefaultUsageLogStart(time.Now(), requestedStart, requestedEnd)
			start = &t
			if requestedStart == nil {
				defaultStart = &t
			}
		}
		if start != nil {
			preds = append(preds, entusagelog.CreatedAtGTE(*start))
		}
		if requestedEnd != nil {
			preds = append(preds, entusagelog.CreatedAtLT(*requestedEnd))
		}

		// The list browser counts once per filter set and turns pages with
		// skip_count=1: even capped, the filtered COUNT scans up to the cap
		// (ILIKE predicates cannot use an index), so repeating it per page
		// doubled the cost of paging. total = -1 tells the client to keep
		// the number it already has.
		total, capped := -1, false
		if c.Query("skip_count") != "1" {
			n, cp, err := h.stats.CountUsageLogsCapped(ctx, preds, biz.UsageLogCountCap)
			if err != nil {
				return empty, err
			}
			total, capped = n, cp
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
		// id breaks created_at ties so the keyset cursor is a total order.
		logs, err := query.
			Select(adminUsageLogColumns...).
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
		// Resolve provider IDs referenced by this page (final provider +
		// every retry-chain entry) to names in one batch query, so the
		// log browser can show "which channel failed" instead of UUIDs.
		provIDs := map[uuid.UUID]struct{}{}
		for _, l := range logs {
			if l.ProviderID != nil {
				provIDs[*l.ProviderID] = struct{}{}
			}
			for _, s := range l.ProviderChain {
				if a, ok := parseUsageAttemptEntry(s); ok {
					if id, perr := uuid.Parse(a.ProviderID); perr == nil {
						provIDs[id] = struct{}{}
					}
					continue
				}
				if id, perr := uuid.Parse(s); perr == nil {
					provIDs[id] = struct{}{}
				}
			}
		}
		nameByID := map[uuid.UUID]string{}
		if len(provIDs) > 0 {
			ids := make([]uuid.UUID, 0, len(provIDs))
			for id := range provIDs {
				ids = append(ids, id)
			}
			provs, perr := h.users.Provider.Query().Where(entprovider.IDIn(ids...)).All(ctx)
			if perr != nil {
				return usageLogsPage{}, perr
			}
			for _, p := range provs {
				nameByID[p.ID] = p.Name
			}
		}
		// providerName maps an id to its name, falling back to a short id
		// prefix for providers deleted since the log was written.
		providerName := func(idStr string) string {
			id, perr := uuid.Parse(idStr)
			if perr != nil {
				return idStr
			}
			if n, ok := nameByID[id]; ok {
				return n
			}
			if len(idStr) > 8 {
				return idStr[:8]
			}
			return idStr
		}

		// Same batch-then-fallback shape for the model group each row was
		// billed against: with auto-route and cross-group retry in play, the
		// group is the answer to "where did this request actually go", and a
		// raw UUID makes that unreadable.
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

		// Resolve the user behind each row to a name/email in one batch
		// query, so the log browser can show "who made this call" instead
		// of a raw UUID.
		userIDs := make(map[uuid.UUID]struct{}, len(logs))
		for _, l := range logs {
			userIDs[l.UserID] = struct{}{}
		}
		userByID := map[uuid.UUID]*ent.User{}
		if len(userIDs) > 0 {
			ids := make([]uuid.UUID, 0, len(userIDs))
			for id := range userIDs {
				ids = append(ids, id)
			}
			users, uerr := h.users.User.Query().Where(entuser.IDIn(ids...)).All(ctx)
			if uerr != nil {
				return usageLogsPage{}, uerr
			}
			for _, u := range users {
				userByID[u.ID] = u
			}
		}

		out := make([]usageLogRow, 0, len(logs))
		for _, l := range logs {
			row := usageLogRow{
				ID:                      l.ID.String(),
				RequestID:               l.RequestID,
				UserID:                  l.UserID.String(),
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
				CostFinal:               l.CostFinal.String(),
				Currency:                l.Currency,
				BillingSource:           string(l.BillingSource),
				Status:                  string(l.Status),
				HTTPStatus:              l.HTTPStatus,
				LatencyMs:               l.LatencyMs,
				TTFTMs:                  l.TtftMs,
				TTFBMs:                  l.TtfbMs,
				LatencyDetail:           l.LatencyDetail,
				RetryCount:              l.RetryCount,
				CreatedAt:               l.CreatedAt,
				IP:                      l.IP,
				UserAgent:               l.UserAgent,
				ReasoningEffort:         l.ReasoningEffort,
				Stream:                  l.Stream,
				APIKeyName:              l.APIKeyName,
				Anomaly:                 biz.ClassifyAnomaly(l.TtftMs, l.LatencyMs, l.RetryCount, string(l.Status), l.ErrorClass),
			}
			if u, ok := userByID[l.UserID]; ok {
				row.UserEmail = u.Email
				if u.Username != nil {
					row.UserName = *u.Username
				}
				if u.Nickname != nil {
					row.UserNickname = *u.Nickname
				}
			}
			if l.APIKeyID != nil {
				id := l.APIKeyID.String()
				row.APIKeyID = &id
			}
			if l.GroupID != nil {
				row.GroupID = l.GroupID.String()
				if n, ok := groupNameByID[*l.GroupID]; ok {
					row.Group = n
				} else if len(row.GroupID) > 8 {
					row.Group = row.GroupID[:8]
				}
			}
			row.ErrorMessage = l.ErrorMessage
			row.ErrorClass = l.ErrorClass
			row.UpstreamRequestID = l.UpstreamRequestID
			if l.ProviderID != nil {
				row.Provider = providerName(l.ProviderID.String())
			}
			if len(l.ProviderChain) > 0 {
				chain := make([]string, 0, len(l.ProviderChain))
				attemptRows := make([]usageAttemptRow, 0, len(l.ProviderChain))
				for _, s := range l.ProviderChain {
					if a, ok := parseUsageAttemptEntry(s); ok {
						if a.ProviderID != "" {
							a.Provider = providerName(a.ProviderID)
						} else if a.Provider == "" {
							a.Provider = a.ProviderName
						}
						if a.Provider != "" {
							chain = append(chain, a.Provider)
						}
						attemptRows = append(attemptRows, a)
						continue
					}
					chain = append(chain, providerName(s))
				}
				row.ProviderChain = chain
				if len(attemptRows) > 0 {
					row.Attempts = attemptRows
				}
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

// handleStatsRate reports platform-wide live RPM/TPM, shown above the admin
// usage log. No cap applies at the platform level, so the limit fields stay 0.
func (h *Handler) handleStatsRate(c *gin.Context) {
	rate, err := h.stats.PlatformRate(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, rate)
}
