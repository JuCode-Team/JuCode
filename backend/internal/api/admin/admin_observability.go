// admin_observability.go: the /v1/admin/observability/* read-only surface —
// time series and leaderboards over usage_logs, and the business-only SLA
// report over availability_observations. Route registration stays in
// admin.go's Mount like every other domain.
//
// Query-cost contract: the series and leaderboard aggregations scan
// usage_logs bounded ONLY by the created_at range index — window ceiling is
// 24h, no new usage_logs indexes were added for this (see the schema comment
// on UsageLog.Indexes), and the 30s response cache plus singleflight are
// what keep the scan off the hot path, exactly like queryProviderPerformance.
package admin

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/cache"
	entgroup "github.com/jucode/jucode/internal/data/ent/modelgroup"
	entprovider "github.com/jucode/jucode/internal/data/ent/provider"
	entaccount "github.com/jucode/jucode/internal/data/ent/upstreamaccount"
)

const (
	// observabilityTTL matches the other admin traffic summaries (provider
	// performance, stats breakdown): TTL-only, no mutation can invalidate a
	// traffic aggregate — only time can.
	observabilityTTL = 30 * time.Second
	// observabilityQueryTimeout bounds one aggregation. A 24h platform scan
	// is the worst case; if it cannot finish in 15s the answer is "the table
	// needs attention", not "hold the request longer".
	observabilityQueryTimeout = 15 * time.Second
)

// obsWindowSpec pins each window to its bucket width. Every mapping yields
// bucket counts in the tens (30 / 36 / 48), matching the public status bar's
// resolution philosophy: enough cells to see shape, enough traffic per cell
// that a verdict means something.
type obsWindowSpec struct {
	Seconds       int
	BucketSeconds int
}

var obsSeriesWindows = map[string]obsWindowSpec{
	"1h":  {Seconds: 3600, BucketSeconds: 120},
	"6h":  {Seconds: 21600, BucketSeconds: 600},
	"24h": {Seconds: 86400, BucketSeconds: 1800},
}

// obsProviderFaultClasses defines "the channel failed us": the four
// retryable channel-level classes from llm/error_class.go. pool_exhausted is
// deliberately separate (capacity_faults) — a dry account pool is a global
// capacity condition, not a bad channel. And note the timeout asymmetry
// documented in docs/observability.md: timeout counts as a provider fault
// here while the SLA pipeline excludes it from availability.
const obsProviderFaultClasses = "('provider_failure','rate_limit','timeout','auth')"

// obsTPSWindowExpr is the generation window TPS divides by: from the moment
// the model began producing output until the client held the whole response.
//
// It starts at the EARLIER of the first readable token (ttft_ms) and the
// moment the model started producing (latency_detail.first_output_ms).
// Readable reasoning deltas count toward TTFT, but encrypted reasoning can
// produce tokens without a readable delta. Using ttft_ms alone would omit
// that time while tokens_out still includes those reasoning tokens.
//
// first_output_ms comes from the pipe's OnFirstOutput hook, which fires on
// the first frame showing the model producing anything — including a
// reasoning item whose content is encrypted and never readable. Where it is
// absent (rows written before it existed, and the chat-completions bridge,
// where readable reasoning arrives as a delta anyway) the window falls back
// to first_sse_frame_ms, the first upstream frame; where that is absent too,
// to ttft_ms.
const obsTPSWindowExpr = "(latency_ms - LEAST(ttft_ms, COALESCE((latency_detail->>'first_output_ms')::bigint, (latency_detail->>'first_sse_frame_ms')::bigint, ttft_ms)))"

// obsTPSMinDeliveryShare is how much of a request's wall time the delivery
// has to occupy before its rate counts as a measurement: a twentieth.
//
// The condition it feeds says "the answer was streamed to us over a
// meaningful part of the request". A relay that assembles the whole answer
// before replaying it — or a provider that answers stream=true with one
// pre-generated chunk — spends nearly the entire request silent and then
// delivers everything in a burst, so its window measures the replay rather
// than the model: one observed 1487-token answer arrived 52ms before the end
// of a 34s request, 0.15% of it.
//
// Why a share and not a rate ceiling: an absolute t/s bound encodes a claim
// about how fast models decode, which the query cannot know, and has to be
// re-tuned the moment the fleet gains a faster provider. A fraction of the
// request is dimensionless and self-scaling. At 5% it keeps 98.3% of output
// tokens and leaves nothing in the fleet above 1000 t/s. The cost is that a
// legitimate long-prefill / short-answer request reads as unmeasurable too —
// no arrival-time signal can tell that apart from a replay. `tps_tokens` is
// the published coverage figure; read it next to `avg_tps`.
const obsTPSMinDeliveryShare = "0.05"

// obsTPSRowFilter selects the rows TPS is computed from.
//
// Rows the gateway never streamed carry no ttft_ms, so they have no
// measurable generation window; they are skipped rather than divided by their
// whole latency, which would bill prefill, routing, queueing and failed
// retries to decode speed.
//
// The three trailing bounds apply that same judgement to rows that do carry a
// window:
//
//   - a positive window drops the degenerate rows where the whole response
//     landed inside the first frame's millisecond.
//   - the delivery has to occupy at least obsTPSMinDeliveryShare of the
//     request; below that the upstream was assembling the answer, not
//     streaming it, and the real generation time is unobservable from here.
//     This is the guard that decides "stream or replay", and it is the one
//     that scales.
//   - at least 2ms of window per output token, a hard stop at an implied
//     500 t/s. What the share bound cannot rule out is a request that was
//     short end to end and still carried a lot of tokens: a relay replaying
//     a cached answer responds in tens of milliseconds, and a twentieth of
//     that is still tens of milliseconds. No arrival-time signal tells that
//     apart from a real decode, so the ceiling stays as the final stop —
//     it claims only what is physically safe, and nothing about a model
//     beyond "not 500 tokens a second". In this fleet it removes the last
//     rows above 500 t/s and moves the platform figure by under 1%.
//
// A row that fails any bound is skipped exactly like a null ttft_ms row
// rather than divided into a six-figure rate.
const obsTPSRowFilter = "status = 'ok' AND tokens_out > 0 AND ttft_ms IS NOT NULL" +
	" AND " + obsTPSWindowExpr + " > 0" +
	" AND " + obsTPSWindowExpr + " >= latency_ms * " + obsTPSMinDeliveryShare +
	" AND " + obsTPSWindowExpr + " >= tokens_out * 2"

// obsTPSExpr is the single definition of TPS on the admin surface:
// queryProviderPerformance (admin_providers.go) interpolates this same
// constant, so the provider table and the observability charts cannot
// disagree about what "TPS" means. admin_observability_test.go pins it.
//
// A pooled ratio, not a mean of per-request rates — see docs/observability.md
// for why. Null when no row qualifies: both SUMs are then null.
const obsTPSExpr = "SUM(CASE WHEN " + obsTPSRowFilter + " THEN tokens_out::float8 END)" +
	" / SUM(CASE WHEN " + obsTPSRowFilter + " THEN " + obsTPSWindowExpr + "::float8 / 1000.0 END)"

// obsTPSTokensExpr is avg_tps's own numerator, published so consumers can
// tell what the rate is based on. ttft_count cannot serve: it is a bare
// COUNT(ttft_ms) with no status or token filter, so a provider whose streams
// mostly break mid-flight reports a healthy ttft_count beside a null avg_tps.
//
// It is NOT the weight for recombining several buckets into one rate. The
// weight of a ratio is its denominator; recover a bucket's seconds as
// tps_tokens / avg_tps and sum those. That recovery is why avg_tps ships
// unrounded (see view()).
const obsTPSTokensExpr = "COALESCE(SUM(CASE WHEN " + obsTPSRowFilter + " THEN tokens_out END), 0)::bigint"

// obsSyntheticFilter drops traffic from platform-internal API keys (the
// status-page prober). api_key_id IS NULL rows are kept on purpose: OAuth
// device sessions carry no key, and they are real customer traffic. The
// subquery reads the tiny api_keys table once; keeping the join out of the
// usage_logs scan.
const obsSyntheticFilter = " AND (api_key_id IS NULL OR api_key_id NOT IN (SELECT id FROM api_keys WHERE internal))"

// observabilityMetrics is one point/row of the series and leaderboard
// responses. Pointer fields serialize as null when no eligible rows exist —
// a bucket with zero requests reports failure_rate 0 (a count-derived rate
// over nothing is vacuously zero, matching providerPerformanceView) but
// avg_ttft_ms null (an average over nothing is not a number).
type observabilityMetrics struct {
	Requests          int      `json:"requests"`
	OK                int      `json:"ok"`
	Failures          int      `json:"failures"`
	FailureRate       float64  `json:"failure_rate"`
	ProviderFaults    int      `json:"provider_faults"`
	ProviderFaultRate *float64 `json:"provider_fault_rate"`
	CapacityFaults    int      `json:"capacity_faults"`
	AvgTTFTMs         *int     `json:"avg_ttft_ms"`
	P50TTFTMs         *int     `json:"p50_ttft_ms"`
	P95TTFTMs         *int     `json:"p95_ttft_ms"`
	P99TTFTMs         *int     `json:"p99_ttft_ms"`
	TTFTCount         int      `json:"ttft_count"`
	AvgTPS            *float64 `json:"avg_tps"`
	TPSTokens         int64    `json:"tps_tokens"`
	AvgLatencyMs      *int     `json:"avg_latency_ms"`
	P95LatencyMs      *int     `json:"p95_latency_ms"`
	TokensOut         int64    `json:"tokens_out"`
	Retries           int64    `json:"retries"`
	Anomalies         int      `json:"anomalies"`
	TimeAnomalies     int      `json:"time_anomalies"`
	ErrorAnomalies    int      `json:"error_anomalies"`
}

type observabilityPoint struct {
	Time time.Time `json:"time"`
	observabilityMetrics
}

// --- parameter parsing ---
// Parsers take a plain getter instead of *gin.Context so the validation
// matrix is unit-testable without an HTTP harness (the admin package's
// test convention — see admin_models_price_test.go).

type obsSeriesParams struct {
	Dim              string
	ProviderID       uuid.UUID
	GroupID          uuid.UUID
	AccountID        uuid.UUID
	Model            string
	Window           string
	Spec             obsWindowSpec
	IncludeSynthetic bool
}

func parseObsSeriesParams(get func(string) string) (obsSeriesParams, error) {
	p := obsSeriesParams{Dim: get("dim")}
	if p.Dim == "" {
		p.Dim = "platform"
	}
	switch p.Dim {
	case "platform":
	case "provider":
		id, err := uuid.Parse(get("provider_id"))
		if err != nil {
			return p, fmt.Errorf("dim=provider requires a valid provider_id")
		}
		p.ProviderID = id
	case "group":
		id, err := uuid.Parse(get("group_id"))
		if err != nil {
			return p, fmt.Errorf("dim=group requires a valid group_id")
		}
		p.GroupID = id
	case "account":
		id, err := uuid.Parse(get("account_id"))
		if err != nil {
			return p, fmt.Errorf("dim=account requires a valid account_id")
		}
		p.AccountID = id
	case "model":
		p.Model = strings.TrimSpace(get("model"))
		if p.Model == "" {
			return p, fmt.Errorf("dim=model requires model")
		}
	default:
		return p, fmt.Errorf("dim must be one of platform|provider|group|account|model")
	}
	var err error
	if p.Window, p.Spec, err = parseObsWindow(get("window"), "24h"); err != nil {
		return p, err
	}
	if p.IncludeSynthetic, err = parseObsBool(get("include_synthetic")); err != nil {
		return p, err
	}
	return p, nil
}

func parseObsWindow(v, def string) (string, obsWindowSpec, error) {
	if v == "" {
		v = def
	}
	spec, ok := obsSeriesWindows[v]
	if !ok {
		return v, obsWindowSpec{}, fmt.Errorf("window must be one of 1h|6h|24h")
	}
	return v, spec, nil
}

func parseObsBool(v string) (bool, error) {
	if v == "" {
		return false, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("include_synthetic must be true or false")
	}
	return b, nil
}

// obsSeriesDimFilter returns the dim-scoped WHERE fragment referencing $4
// plus its argument. Platform scopes nothing.
func (p obsSeriesParams) dimFilter() (string, []any) {
	switch p.Dim {
	case "provider":
		return " AND provider_id = $4", []any{p.ProviderID}
	case "group":
		return " AND group_id = $4", []any{p.GroupID}
	case "account":
		return " AND provider_account_id = $4", []any{p.AccountID}
	case "model":
		return " AND model = $4", []any{p.Model}
	}
	return "", nil
}

// dimEcho returns the response field echoing which entity was selected.
func (p obsSeriesParams) dimEcho() (string, string) {
	switch p.Dim {
	case "provider":
		return "provider_id", p.ProviderID.String()
	case "group":
		return "group_id", p.GroupID.String()
	case "account":
		return "account_id", p.AccountID.String()
	case "model":
		return "model", p.Model
	}
	return "", ""
}

// cacheKeySuffix identifies everything the series answer depends on. start
// is bucket-aligned by the caller, so rolling polls inside one bucket share
// the key and the 30s TTL decides freshness.
func (p obsSeriesParams) cacheKeySuffix(start time.Time) string {
	_, dimVal := p.dimEcho()
	return p.Dim + ":" + dimVal + ":" + p.Window + ":syn=" + strconv.FormatBool(p.IncludeSynthetic) + ":" + start.UTC().Format(time.RFC3339)
}

// --- SQL assembly ---

// obsMetricColumns is the SELECT list shared by the series and leaderboard
// queries; scanObsMetrics scans it in the same order. failure_rate and
// provider_fault_rate are derived in Go from the counts.
func obsMetricColumns() string {
	return `COUNT(*)::bigint,
  COUNT(*) FILTER (WHERE status = 'ok')::bigint,
  COUNT(*) FILTER (WHERE status <> 'ok')::bigint,
  COUNT(*) FILTER (WHERE error_class IN ` + obsProviderFaultClasses + `)::bigint,
  COUNT(*) FILTER (WHERE error_class = 'pool_exhausted')::bigint,
  COUNT(ttft_ms)::bigint,
  AVG(ttft_ms)::float8,
  percentile_cont(0.50) WITHIN GROUP (ORDER BY ttft_ms) FILTER (WHERE ttft_ms IS NOT NULL),
  percentile_cont(0.95) WITHIN GROUP (ORDER BY ttft_ms) FILTER (WHERE ttft_ms IS NOT NULL),
  percentile_cont(0.99) WITHIN GROUP (ORDER BY ttft_ms) FILTER (WHERE ttft_ms IS NOT NULL),
  ` + obsTPSExpr + `,
  ` + obsTPSTokensExpr + `,
  AVG(latency_ms) FILTER (WHERE status = 'ok' AND latency_ms > 0)::float8,
  percentile_cont(0.95) WITHIN GROUP (ORDER BY latency_ms) FILTER (WHERE status = 'ok' AND latency_ms > 0),
  COALESCE(SUM(tokens_out), 0)::bigint,
  COALESCE(SUM(retry_count), 0)::bigint,
  COUNT(*) FILTER (WHERE ` + biz.AnomalyAnySQL() + `)::bigint,
  COUNT(*) FILTER (WHERE ` + biz.AnomalyTimeSQL() + `)::bigint,
  COUNT(*) FILTER (WHERE ` + biz.AnomalyErrorSQL() + `)::bigint`
}

// buildObsSeriesQuery: $1 = window start (doubles as the bucket-grid
// origin), $2 = bucket seconds, $3 = window end, $4 = optional dim value.
func buildObsSeriesQuery(p obsSeriesParams, start, end time.Time) (string, []any) {
	args := []any{start, p.Spec.BucketSeconds, end}
	dimWhere, dimArgs := p.dimFilter()
	args = append(args, dimArgs...)
	synthetic := ""
	if !p.IncludeSynthetic {
		synthetic = obsSyntheticFilter
	}
	q := `SELECT
  floor(extract(epoch FROM (created_at - $1)) / $2)::int AS bucket_index,
  ` + obsMetricColumns() + `
FROM usage_logs
WHERE created_at >= $1 AND created_at < $3` + dimWhere + synthetic + `
GROUP BY bucket_index
ORDER BY bucket_index`
	return q, args
}

type obsLeaderboardParams struct {
	Dim              string
	Window           string
	Spec             obsWindowSpec
	IncludeSynthetic bool
}

func parseObsLeaderboardParams(get func(string) string) (obsLeaderboardParams, error) {
	p := obsLeaderboardParams{Dim: get("dim")}
	switch p.Dim {
	case "provider", "group", "account", "model":
	case "":
		return p, fmt.Errorf("dim is required: provider|group|account|model")
	default:
		return p, fmt.Errorf("dim must be one of provider|group|account|model")
	}
	var err error
	if p.Window, p.Spec, err = parseObsWindow(get("window"), "1h"); err != nil {
		return p, err
	}
	if p.IncludeSynthetic, err = parseObsBool(get("include_synthetic")); err != nil {
		return p, err
	}
	return p, nil
}

// obsRateColumns are the leaderboard's live RPM / TPM: counts over the
// trailing minute ($3 .. end). TPM counts output tokens only, the same
// definition as the platform rate (biz.usageRateTokens) and the TPM cap, so
// the per-row figures add up to the platform readout.
const obsRateColumns = `COUNT(*) FILTER (WHERE created_at >= $3)::bigint,
  COALESCE(SUM(tokens_out + audio_tokens_out) FILTER (WHERE created_at >= $3), 0)::bigint`

// buildObsLeaderboardQuery: $1 = start, $2 = end, $3 = end minus one minute
// (the RPM / TPM window). Rows failed before routing carry no provider_id /
// group_id / provider_account_id and cannot be attributed to a leaderboard
// entry, so those dims skip them; model is always present.
func buildObsLeaderboardQuery(p obsLeaderboardParams, start, end time.Time) (string, []any) {
	var keyCol, keyWhere string
	switch p.Dim {
	case "provider":
		keyCol, keyWhere = "provider_id::text", " AND provider_id IS NOT NULL"
	case "group":
		keyCol, keyWhere = "group_id::text", " AND group_id IS NOT NULL"
	case "account":
		keyCol, keyWhere = "provider_account_id::text", " AND provider_account_id IS NOT NULL"
	default: // model
		keyCol, keyWhere = "model", ""
	}
	synthetic := ""
	if !p.IncludeSynthetic {
		synthetic = obsSyntheticFilter
	}
	q := `SELECT
  ` + keyCol + ` AS key,
  ` + obsMetricColumns() + `,
  ` + obsRateColumns + `
FROM usage_logs
WHERE created_at >= $1 AND created_at < $2` + keyWhere + synthetic + `
GROUP BY key
ORDER BY 2 DESC`
	return q, []any{start, end, end.Add(-time.Minute)}
}

// --- scanning ---

// obsMetricsScan holds the raw scan targets for obsMetricColumns.
type obsMetricsScan struct {
	requests, ok, failures, providerFaults, capacityFaults, ttftCount int64
	avgTTFT, p50TTFT, p95TTFT, p99TTFT                                sql.NullFloat64
	avgTPS, avgLatency, p95Latency                                    sql.NullFloat64
	tpsTokens                                                         int64
	tokensOut, retries                                                int64
	anomalies, timeAnomalies, errorAnomalies                          int64
}

func (m *obsMetricsScan) dest() []any {
	return []any{
		&m.requests, &m.ok, &m.failures, &m.providerFaults, &m.capacityFaults, &m.ttftCount,
		&m.avgTTFT, &m.p50TTFT, &m.p95TTFT, &m.p99TTFT,
		&m.avgTPS, &m.tpsTokens, &m.avgLatency, &m.p95Latency,
		&m.tokensOut, &m.retries,
		&m.anomalies, &m.timeAnomalies, &m.errorAnomalies,
	}
}

func (m *obsMetricsScan) view() observabilityMetrics {
	v := observabilityMetrics{
		Requests:       int(m.requests),
		OK:             int(m.ok),
		Failures:       int(m.failures),
		ProviderFaults: int(m.providerFaults),
		CapacityFaults: int(m.capacityFaults),
		TTFTCount:      int(m.ttftCount),
		TPSTokens:      m.tpsTokens,
		TokensOut:      m.tokensOut,
		Retries:        m.retries,
		Anomalies:      int(m.anomalies),
		TimeAnomalies:  int(m.timeAnomalies),
		ErrorAnomalies: int(m.errorAnomalies),
	}
	if m.requests > 0 {
		v.FailureRate = float64(m.failures) / float64(m.requests)
	}
	// Denominator is eligible outcomes only: ok + provider faults. User-side
	// failures neither count for nor against the channel.
	if denom := m.ok + m.providerFaults; denom > 0 {
		rate := float64(m.providerFaults) / float64(denom)
		v.ProviderFaultRate = &rate
	}
	if m.avgTTFT.Valid {
		v.AvgTTFTMs = roundedIntPtr(m.avgTTFT.Float64)
	}
	if m.p50TTFT.Valid {
		v.P50TTFTMs = roundedIntPtr(m.p50TTFT.Float64)
	}
	if m.p95TTFT.Valid {
		v.P95TTFTMs = roundedIntPtr(m.p95TTFT.Float64)
	}
	if m.p99TTFT.Valid {
		v.P99TTFTMs = roundedIntPtr(m.p99TTFT.Float64)
	}
	if m.avgTPS.Valid {
		// Unrounded on purpose. Consumers recombine buckets by recovering
		// each one's seconds as tps_tokens / avg_tps, and 2-decimal rounding
		// makes that lossy — worst case a stalled bucket under 0.005 t/s
		// rounds to zero and drops out of the aggregate entirely, which is
		// the one bucket anybody is looking for. Formatting is the UI's job.
		tps := m.avgTPS.Float64
		v.AvgTPS = &tps
	}
	if m.avgLatency.Valid {
		v.AvgLatencyMs = roundedIntPtr(m.avgLatency.Float64)
	}
	if m.p95Latency.Valid {
		v.P95LatencyMs = roundedIntPtr(m.p95Latency.Float64)
	}
	return v
}

// zeroFilledObsPoints seeds one point per bucket. Zero-value metrics are
// exactly the contract's empty point: counts 0, failure_rate 0, every
// nullable metric null.
func zeroFilledObsPoints(start time.Time, bucketSeconds, count int) []observabilityPoint {
	points := make([]observabilityPoint, count)
	for i := range points {
		points[i].Time = start.Add(time.Duration(i*bucketSeconds) * time.Second)
	}
	return points
}

// --- handlers ---

func (h *Handler) handleObservabilitySeries(c *gin.Context) {
	p, err := parseObsSeriesParams(c.Query)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	// Grid aligned to bucket boundaries, like the public status bar: the
	// trailing bucket is the in-progress one, and every poll inside one
	// bucket resolves to the same window — which is what lets the cache key
	// collapse rolling polls instead of minting a new key per request.
	bucketDuration := time.Duration(p.Spec.BucketSeconds) * time.Second
	end := time.Now().UTC().Truncate(bucketDuration).Add(bucketDuration)
	start := end.Add(-time.Duration(p.Spec.Seconds) * time.Second)
	bucketCount := p.Spec.Seconds / p.Spec.BucketSeconds

	cacheKey := cache.KeyObservabilitySeriesPrefix + p.cacheKeySuffix(start)
	points, hit, err := cache.GetOrCompute(c.Request.Context(), h.cache, cacheKey, observabilityTTL, func(ctx context.Context) ([]observabilityPoint, error) {
		ctx, cancel := context.WithTimeout(ctx, observabilityQueryTimeout)
		defer cancel()
		return h.queryObsSeries(ctx, p, start, end, bucketCount)
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	setCacheHeader(c, hit)
	out := gin.H{
		"dim":            p.Dim,
		"window_seconds": p.Spec.Seconds,
		"bucket_seconds": p.Spec.BucketSeconds,
		"start":          start,
		"end":            end,
		"points":         points,
	}
	if field, val := p.dimEcho(); field != "" {
		out[field] = val
	}
	c.JSON(http.StatusOK, out)
}

func (h *Handler) queryObsSeries(ctx context.Context, p obsSeriesParams, start, end time.Time, bucketCount int) ([]observabilityPoint, error) {
	points := zeroFilledObsPoints(start, p.Spec.BucketSeconds, bucketCount)
	if h.rawDB == nil {
		return points, nil
	}
	query, args := buildObsSeriesQuery(p, start, end)
	rows, err := h.rawDB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var idx int
		var m obsMetricsScan
		if err := rows.Scan(append([]any{&idx}, m.dest()...)...); err != nil {
			return nil, err
		}
		if idx >= 0 && idx < bucketCount {
			points[idx].observabilityMetrics = m.view()
		}
	}
	return points, rows.Err()
}

type observabilityLeaderboardRow struct {
	// Key carries the dim identity: provider_id / group_id as UUID text, or
	// the model name. The JSON tag matters even though the HTTP response
	// renames it per dim — this struct is what round-trips through the Redis
	// response cache, and "-" would drop the identity on every cache hit.
	Key  string `json:"key"`
	Name string `json:"name"`
	// Trailing-minute throughput, independent of the leaderboard window.
	RPM int64 `json:"rpm"`
	TPM int64 `json:"tpm"`
	observabilityMetrics
}

func (h *Handler) handleObservabilityLeaderboard(c *gin.Context) {
	p, err := parseObsLeaderboardParams(c.Query)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	// No bucket grid here, so the snap step is the cache TTL itself — every
	// poll within one TTL agrees on the window, exactly the SnapWindow
	// rationale in biz/stats.go.
	end := time.Now().UTC().Truncate(observabilityTTL)
	start := end.Add(-time.Duration(p.Spec.Seconds) * time.Second)

	cacheKey := cache.KeyObservabilityLeaderboardPrefix + p.Dim + ":" + p.Window + ":syn=" + strconv.FormatBool(p.IncludeSynthetic) + ":" + end.Format(time.RFC3339)
	rowsOut, hit, err := cache.GetOrCompute(c.Request.Context(), h.cache, cacheKey, observabilityTTL, func(ctx context.Context) ([]observabilityLeaderboardRow, error) {
		ctx, cancel := context.WithTimeout(ctx, observabilityQueryTimeout)
		defer cancel()
		return h.queryObsLeaderboard(ctx, p, start, end)
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	setCacheHeader(c, hit)

	keyField := map[string]string{"provider": "provider_id", "group": "group_id", "account": "account_id", "model": "model"}[p.Dim]
	shaped := make([]gin.H, 0, len(rowsOut))
	for _, r := range rowsOut {
		row := gin.H{
			keyField:              r.Key,
			"name":                r.Name,
			"rpm":                 r.RPM,
			"tpm":                 r.TPM,
			"requests":            r.Requests,
			"ok":                  r.OK,
			"failures":            r.Failures,
			"failure_rate":        r.FailureRate,
			"provider_faults":     r.ProviderFaults,
			"provider_fault_rate": r.ProviderFaultRate,
			"capacity_faults":     r.CapacityFaults,
			"avg_ttft_ms":         r.AvgTTFTMs,
			"p50_ttft_ms":         r.P50TTFTMs,
			"p95_ttft_ms":         r.P95TTFTMs,
			"p99_ttft_ms":         r.P99TTFTMs,
			"ttft_count":          r.TTFTCount,
			"avg_tps":             r.AvgTPS,
			"tps_tokens":          r.TPSTokens,
			"avg_latency_ms":      r.AvgLatencyMs,
			"p95_latency_ms":      r.P95LatencyMs,
			"tokens_out":          r.TokensOut,
			"retries":             r.Retries,
			"anomalies":           r.Anomalies,
			"time_anomalies":      r.TimeAnomalies,
			"error_anomalies":     r.ErrorAnomalies,
		}
		shaped = append(shaped, row)
	}
	c.JSON(http.StatusOK, gin.H{
		"dim":            p.Dim,
		"window_seconds": p.Spec.Seconds,
		"start":          start,
		"end":            end,
		"rows":           shaped,
	})
}

func (h *Handler) queryObsLeaderboard(ctx context.Context, p obsLeaderboardParams, start, end time.Time) ([]observabilityLeaderboardRow, error) {
	out := []observabilityLeaderboardRow{}
	if h.rawDB == nil {
		return out, nil
	}
	query, args := buildObsLeaderboardQuery(p, start, end)
	rows, err := h.rawDB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var r observabilityLeaderboardRow
		var m obsMetricsScan
		if err := rows.Scan(append(append([]any{&r.Key}, m.dest()...), &r.RPM, &r.TPM)...); err != nil {
			return nil, err
		}
		r.observabilityMetrics = m.view()
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return h.resolveObsNames(ctx, p.Dim, out)
}

// resolveObsNames fills Name for each leaderboard row in one batch query.
// Entities deleted since the logs were written fall back to an 8-char id
// prefix, same convention as the usage-log browser's providerName.
func (h *Handler) resolveObsNames(ctx context.Context, dim string, rows []observabilityLeaderboardRow) ([]observabilityLeaderboardRow, error) {
	if dim == "model" {
		for i := range rows {
			rows[i].Name = rows[i].Key
		}
		return rows, nil
	}
	ids := make([]uuid.UUID, 0, len(rows))
	for _, r := range rows {
		if id, err := uuid.Parse(r.Key); err == nil {
			ids = append(ids, id)
		}
	}
	nameByID := map[string]string{}
	if len(ids) > 0 {
		switch dim {
		case "provider":
			names, err := h.providerNamesByID(ctx, ids)
			if err != nil {
				return nil, err
			}
			nameByID = names
		case "group":
			gs, err := h.users.ModelGroup.Query().Where(entgroup.IDIn(ids...)).All(ctx)
			if err != nil {
				return nil, err
			}
			for _, g := range gs {
				nameByID[g.ID.String()] = g.Name
			}
		case "account":
			as, err := h.users.UpstreamAccount.Query().Where(entaccount.IDIn(ids...)).All(ctx)
			if err != nil {
				return nil, err
			}
			for _, a := range as {
				nameByID[a.ID.String()] = a.Name
			}
		}
	}
	for i := range rows {
		if n, ok := nameByID[rows[i].Key]; ok {
			rows[i].Name = n
		} else if len(rows[i].Key) > 8 {
			rows[i].Name = rows[i].Key[:8]
		} else {
			rows[i].Name = rows[i].Key
		}
	}
	return rows, nil
}

// --- attempt-level provider stats ---

// The leaderboard's provider dim attributes a request to the provider that
// FINALLY served it (usage_logs.provider_id), so a channel that fails every
// attempt and is rescued by failover shows up with almost no requests — its
// misbehavior lives only in provider_chain, the Redis health buckets and the
// Prometheus counter. This endpoint reads provider_chain directly and counts
// every attempt each channel handled, which is the view an operator needs to
// answer "is this channel actually healthy", independent of who ended up
// serving the request.
//
// Cost: parsing jsonb per row is far heavier than the column aggregates the
// other endpoints run — ~25ms for a 1h window and ~900ms for 24h on
// production data (measured, 61k rows). That is inside the 15s budget and
// behind the same 30s cache, so it does not need its own index; if the window
// ever grows past 24h, re-measure before loosening it.

// buildObsProviderAttemptsQuery: $1 = start, $2 = end. The inner group
// produces one row per (provider, class); the outer rolls those into the
// per-provider totals plus the raw class breakdown. Elements are
// `attempt:{json}` strings (usageAttemptRecorder.Chain); the LIKE guard keeps
// a malformed element (the marshal-failure fallback writes a bare provider id)
// from failing the cast for the whole query. The synthetic filter has to land
// inside the inner scan — api_key_id is not visible past the aggregate.
func buildObsProviderAttemptsQuery(includeSynthetic bool) string {
	synthetic := ""
	if !includeSynthetic {
		synthetic = obsSyntheticFilter
	}
	return `SELECT
  provider_id,
  SUM(n)::bigint AS attempts,
  COALESCE(SUM(n) FILTER (WHERE cls = 'ok'), 0)::bigint AS ok,
  COALESCE(SUM(n) FILTER (WHERE cls IN ` + obsProviderFaultClasses + `), 0)::bigint AS provider_faults,
  jsonb_object_agg(cls, n) AS by_class
FROM (
  SELECT a->>'provider_id' AS provider_id,
         COALESCE(NULLIF(a->>'error_class', ''), 'unknown') AS cls,
         COUNT(*)::bigint AS n
  FROM usage_logs u
  CROSS JOIN LATERAL jsonb_array_elements_text(u.provider_chain) AS t(txt)
  CROSS JOIN LATERAL (SELECT substring(t.txt FROM 9)::jsonb AS a WHERE t.txt LIKE 'attempt:%') x
  WHERE u.created_at >= $1 AND u.created_at < $2
    AND jsonb_typeof(u.provider_chain) = 'array'
    AND COALESCE(a->>'provider_id', '') <> ''` + synthetic + `
  GROUP BY 1, 2
) per_class
GROUP BY provider_id
ORDER BY attempts DESC`
}

type observabilityProviderAttemptRow struct {
	ProviderID        string           `json:"provider_id"`
	Name              string           `json:"name"`
	Attempts          int64            `json:"attempts"`
	OK                int64            `json:"ok"`
	ProviderFaults    int64            `json:"provider_faults"`
	ProviderFaultRate *float64         `json:"provider_fault_rate"`
	ByClass           map[string]int64 `json:"by_class"`
}

func (h *Handler) handleObservabilityProviderAttempts(c *gin.Context) {
	window, spec, err := parseObsWindow(c.Query("window"), "1h")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	includeSynthetic, err := parseObsBool(c.Query("include_synthetic"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	end := time.Now().UTC().Truncate(observabilityTTL)
	start := end.Add(-time.Duration(spec.Seconds) * time.Second)

	cacheKey := cache.KeyObservabilityProviderAttemptPrefix + window + ":syn=" + strconv.FormatBool(includeSynthetic) + ":" + end.Format(time.RFC3339)
	rows, hit, err := cache.GetOrCompute(c.Request.Context(), h.cache, cacheKey, observabilityTTL, func(ctx context.Context) ([]observabilityProviderAttemptRow, error) {
		ctx, cancel := context.WithTimeout(ctx, observabilityQueryTimeout)
		defer cancel()
		return h.queryProviderAttempts(ctx, start, end, includeSynthetic)
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	setCacheHeader(c, hit)
	c.JSON(http.StatusOK, gin.H{
		"window":         window,
		"window_seconds": spec.Seconds,
		"start":          start,
		"end":            end,
		"rows":           rows,
	})
}

func (h *Handler) queryProviderAttempts(ctx context.Context, start, end time.Time, includeSynthetic bool) ([]observabilityProviderAttemptRow, error) {
	out := []observabilityProviderAttemptRow{}
	if h.rawDB == nil {
		return out, nil
	}
	rows, err := h.rawDB.QueryContext(ctx, buildObsProviderAttemptsQuery(includeSynthetic), start, end)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []uuid.UUID{}
	for rows.Next() {
		var r observabilityProviderAttemptRow
		var byClassRaw []byte
		if err := rows.Scan(&r.ProviderID, &r.Attempts, &r.OK, &r.ProviderFaults, &byClassRaw); err != nil {
			return nil, err
		}
		if len(byClassRaw) > 0 {
			_ = json.Unmarshal(byClassRaw, &r.ByClass)
		}
		// Same denominator as provider_fault_rate elsewhere: only eligible
		// outcomes count, so user-caused attempts (client_canceled,
		// user_request) neither help nor hurt the channel.
		if denom := r.OK + r.ProviderFaults; denom > 0 {
			rate := float64(r.ProviderFaults) / float64(denom)
			r.ProviderFaultRate = &rate
		}
		if id, err := uuid.Parse(r.ProviderID); err == nil {
			ids = append(ids, id)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	names, err := h.providerNamesByID(ctx, ids)
	if err != nil {
		return nil, err
	}
	for i := range out {
		if n, ok := names[out[i].ProviderID]; ok {
			out[i].Name = n
		} else if len(out[i].ProviderID) > 8 {
			out[i].Name = out[i].ProviderID[:8]
		} else {
			out[i].Name = out[i].ProviderID
		}
	}
	return out, nil
}

// providerNamesByID resolves provider display names in one query.
func (h *Handler) providerNamesByID(ctx context.Context, ids []uuid.UUID) (map[string]string, error) {
	out := map[string]string{}
	if len(ids) == 0 {
		return out, nil
	}
	ps, err := h.users.Provider.Query().Where(entprovider.IDIn(ids...)).All(ctx)
	if err != nil {
		return nil, err
	}
	for _, p := range ps {
		out[p.ID.String()] = p.Name
	}
	return out, nil
}

// --- SLA ---

var obsSLAWindows = map[string]int{"24h": 24, "7d": 168, "30d": 720}

func (h *Handler) handleObservabilitySLA(c *gin.Context) {
	windowParam := c.DefaultQuery("window", "24h")
	hours, ok := obsSLAWindows[windowParam]
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "window must be one of 24h|7d|30d"})
		return
	}
	var groupID *uuid.UUID
	groupKey := "all"
	if v := c.Query("group_id"); v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid group_id"})
			return
		}
		groupID = &id
		groupKey = id.String()
	}
	// Key on the bucket-aligned grid origin so rolling polls share an entry;
	// the 30s TTL, not the grid step, decides how fresh the numbers are.
	bucketDuration := time.Duration(hours) * time.Hour / 48
	gridStart := time.Now().UTC().Truncate(bucketDuration)
	cacheKey := cache.KeyObservabilitySLAPrefix + windowParam + ":" + groupKey + ":" + gridStart.Format(time.RFC3339)

	out, hit, err := cache.GetOrCompute(c.Request.Context(), h.cache, cacheKey, observabilityTTL, func(ctx context.Context) (*biz.AdminSLA, error) {
		ctx, cancel := context.WithTimeout(ctx, observabilityQueryTimeout)
		defer cancel()
		return biz.AdminSLASnapshot(ctx, h.users, h.rawDB, h.settings, hours, groupID)
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	setCacheHeader(c, hit)
	c.JSON(http.StatusOK, out)
}

// --- load map (routing topology canvas) ---

// loadMapEdge is one (group, account) edge's traffic inside the window. The
// canvas draws one edge per row; node values are the row sums on each side.
// Keyed on provider_account_id — the live account attribution — since the
// window is short enough that legacy provider_id rows never appear.
type loadMapEdge struct {
	GroupID        string `json:"group_id"`
	AccountID      string `json:"account_id"`
	Requests       int    `json:"requests"`
	Failures       int    `json:"failures"`
	ProviderFaults int    `json:"provider_faults"`
	AvgLatencyMs   *int   `json:"avg_latency_ms"`
	TokensOut      int64  `json:"tokens_out"`
}

// The canvas polls on a short cycle; windows stay compact so "load" means
// now, not today.
var loadMapWindows = map[string]int{"5m": 300, "15m": 900, "1h": 3600}

func (h *Handler) handleObservabilityLoadMap(c *gin.Context) {
	seconds, ok := loadMapWindows[c.DefaultQuery("window", "15m")]
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "window must be 5m, 15m or 1h"})
		return
	}
	end := time.Now().UTC()
	start := end.Add(-time.Duration(seconds) * time.Second)
	edges, err := h.queryLoadMap(c.Request.Context(), start, end)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"window_seconds": seconds,
		"start":          start,
		"end":            end,
		"edges":          edges,
	})
}

func (h *Handler) queryLoadMap(ctx context.Context, start, end time.Time) ([]loadMapEdge, error) {
	if h.rawDB == nil {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, observabilityQueryTimeout)
	defer cancel()
	rows, err := h.rawDB.QueryContext(ctx, `SELECT
  group_id::text,
  provider_account_id::text,
  COUNT(*)::bigint,
  COUNT(*) FILTER (WHERE status <> 'ok')::bigint,
  COUNT(*) FILTER (WHERE error_class IN `+obsProviderFaultClasses+`)::bigint,
  AVG(latency_ms) FILTER (WHERE status = 'ok' AND latency_ms > 0)::float8,
  COALESCE(SUM(tokens_out), 0)::bigint
FROM usage_logs
WHERE created_at >= $1 AND created_at < $2
  AND group_id IS NOT NULL
  AND provider_account_id IS NOT NULL`+obsSyntheticFilter+`
GROUP BY 1, 2`, start, end)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []loadMapEdge{}
	for rows.Next() {
		var e loadMapEdge
		var latency sql.NullFloat64
		if err := rows.Scan(&e.GroupID, &e.AccountID, &e.Requests, &e.Failures, &e.ProviderFaults, &latency, &e.TokensOut); err != nil {
			return nil, err
		}
		if latency.Valid {
			v := int(math.Round(latency.Float64))
			e.AvgLatencyMs = &v
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// --- live error monitor ---
//
// /observability/errors answers "what is failing right now": a per-class
// breakdown with shares and first/last sighting, a zero-filled per-bucket
// error timeline on the same grid style as the series endpoint, and the
// newest raw error rows for the feed. Shorter windows than the perf series —
// 15m is the default because the question is "now", not "today".
//
// "Error" here means a service-side failure: biz.AnomalyErrorSQL, the same
// definition the usage-log anomaly filter uses. Caller-side failures (bad
// request parameters, client disconnects, content-safety blocks, quota /
// balance / key rejections) are not errors of the system and would otherwise
// bury real faults; they are only counted, as excluded_errors, so the page can
// say how many were left out.

var obsErrorWindows = map[string]obsWindowSpec{
	"15m": {Seconds: 900, BucketSeconds: 30},
	"1h":  {Seconds: 3600, BucketSeconds: 120},
	"6h":  {Seconds: 21600, BucketSeconds: 600},
	"24h": {Seconds: 86400, BucketSeconds: 1800},
}

type obsErrorBreakdownRow struct {
	ErrorClass string    `json:"error_class"`
	Count      int       `json:"count"`
	Share      float64   `json:"share"`
	FirstSeen  time.Time `json:"first_seen"`
	LastSeen   time.Time `json:"last_seen"`
	Sample     string    `json:"sample"`
	TopAccount string    `json:"top_account"`
	TopModel   string    `json:"top_model"`
}

type obsErrorBucket struct {
	Time     time.Time `json:"t"`
	Requests int       `json:"requests"`
	Errors   int       `json:"errors"`
}

type obsErrorRecentRow struct {
	At         time.Time `json:"at"`
	ErrorClass string    `json:"error_class"`
	HTTPStatus int       `json:"http_status"`
	Model      string    `json:"model"`
	Account    string    `json:"account"`
	Group      string    `json:"group"`
	Message    string    `json:"message"`
}

type obsErrorsResponse struct {
	WindowSeconds  int                    `json:"window_seconds"`
	BucketSeconds  int                    `json:"bucket_seconds"`
	Start          time.Time              `json:"start"`
	End            time.Time              `json:"end"`
	TotalRequests  int                    `json:"total_requests"`
	TotalErrors    int                    `json:"total_errors"`
	ExcludedErrors int                    `json:"excluded_errors"`
	Breakdown      []obsErrorBreakdownRow `json:"breakdown"`
	Buckets        []obsErrorBucket       `json:"buckets"`
	Recent         []obsErrorRecentRow    `json:"recent"`
}

func (h *Handler) handleObservabilityErrors(c *gin.Context) {
	window := c.DefaultQuery("window", "15m")
	spec, ok := obsErrorWindows[window]
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "window must be one of 15m|1h|6h|24h"})
		return
	}
	includeSynthetic, err := parseObsBool(c.Query("include_synthetic"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	limit := 50
	if v := c.Query("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = min(n, 200)
		}
	}
	// Snap the window to the cache TTL like the leaderboard does, so rolling
	// polls inside one TTL collapse onto one cached answer.
	end := time.Now().UTC().Truncate(observabilityTTL)
	start := end.Add(-time.Duration(spec.Seconds) * time.Second)

	cacheKey := cache.KeyObservabilityErrorsPrefix + window + ":syn=" + strconv.FormatBool(includeSynthetic) + ":" + strconv.Itoa(limit) + ":" + end.Format(time.RFC3339)
	resp, hit, err := cache.GetOrCompute(c.Request.Context(), h.cache, cacheKey, observabilityTTL, func(ctx context.Context) (*obsErrorsResponse, error) {
		ctx, cancel := context.WithTimeout(ctx, observabilityQueryTimeout)
		defer cancel()
		return h.queryObsErrors(ctx, spec, includeSynthetic, limit, start, end)
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	setCacheHeader(c, hit)
	c.JSON(http.StatusOK, resp)
}

func (h *Handler) queryObsErrors(ctx context.Context, spec obsWindowSpec, includeSynthetic bool, limit int, start, end time.Time) (*obsErrorsResponse, error) {
	resp := &obsErrorsResponse{
		WindowSeconds: spec.Seconds,
		BucketSeconds: spec.BucketSeconds,
		Start:         start,
		End:           end,
		Breakdown:     []obsErrorBreakdownRow{},
		Recent:        []obsErrorRecentRow{},
	}
	bucketCount := spec.Seconds / spec.BucketSeconds
	resp.Buckets = make([]obsErrorBucket, bucketCount)
	for i := range resp.Buckets {
		resp.Buckets[i].Time = start.Add(time.Duration(i*spec.BucketSeconds) * time.Second)
	}
	if h.rawDB == nil {
		return resp, nil
	}
	synthetic := ""
	if !includeSynthetic {
		synthetic = obsSyntheticFilter
	}

	// 1. Per-class breakdown. mode() picks the most frequent sample message /
	// account / model so each row answers "what does this class look like"
	// without shipping raw rows to the client.
	bdRows, err := h.rawDB.QueryContext(ctx, `SELECT
  COALESCE(NULLIF(l.error_class, ''), 'unknown') AS cls,
  COUNT(*)::bigint,
  min(l.created_at),
  max(l.created_at),
  COALESCE(mode() WITHIN GROUP (ORDER BY left(l.error_message, 300)), ''),
  COALESCE(mode() WITHIN GROUP (ORDER BY COALESCE(a.name, '')), ''),
  COALESCE(mode() WITHIN GROUP (ORDER BY l.model), '')
FROM (SELECT * FROM usage_logs
  WHERE created_at >= $1 AND created_at < $2 AND `+biz.AnomalyErrorSQL()+synthetic+`) l
LEFT JOIN upstream_accounts a ON a.id = l.provider_account_id
GROUP BY cls
ORDER BY 2 DESC`, start, end)
	if err != nil {
		return nil, err
	}
	defer bdRows.Close()
	for bdRows.Next() {
		var r obsErrorBreakdownRow
		if err := bdRows.Scan(&r.ErrorClass, &r.Count, &r.FirstSeen, &r.LastSeen, &r.Sample, &r.TopAccount, &r.TopModel); err != nil {
			return nil, err
		}
		resp.TotalErrors += r.Count
		resp.Breakdown = append(resp.Breakdown, r)
	}
	if err := bdRows.Err(); err != nil {
		return nil, err
	}
	for i := range resp.Breakdown {
		resp.Breakdown[i].Share = float64(resp.Breakdown[i].Count) / float64(resp.TotalErrors)
	}

	// 2. Zero-filled bucket grid: total requests + errors per bucket.
	bucketRows, err := h.rawDB.QueryContext(ctx, `SELECT
  floor(extract(epoch FROM (created_at - $1)) / $2)::int AS bucket_index,
  COUNT(*)::bigint,
  COUNT(*) FILTER (WHERE `+biz.AnomalyErrorSQL()+`)::bigint,
  COUNT(*) FILTER (WHERE status <> 'ok' AND NOT `+biz.AnomalyErrorSQL()+`)::bigint
FROM usage_logs
WHERE created_at >= $1 AND created_at < $3`+synthetic+`
GROUP BY bucket_index`, start, spec.BucketSeconds, end)
	if err != nil {
		return nil, err
	}
	defer bucketRows.Close()
	for bucketRows.Next() {
		var idx, reqs, errs, excluded int
		if err := bucketRows.Scan(&idx, &reqs, &errs, &excluded); err != nil {
			return nil, err
		}
		resp.TotalRequests += reqs
		resp.ExcludedErrors += excluded
		if idx >= 0 && idx < bucketCount {
			resp.Buckets[idx].Requests = reqs
			resp.Buckets[idx].Errors = errs
		}
	}
	if err := bucketRows.Err(); err != nil {
		return nil, err
	}

	// 3. Newest raw rows for the live feed.
	recentRows, err := h.rawDB.QueryContext(ctx, `SELECT
  l.created_at,
  COALESCE(NULLIF(l.error_class, ''), 'unknown'),
  l.http_status,
  l.model,
  COALESCE(a.name, ''),
  COALESCE(g.name, ''),
  COALESCE(left(l.error_message, 300), '')
FROM (SELECT * FROM usage_logs
  WHERE created_at >= $1 AND created_at < $2 AND `+biz.AnomalyErrorSQL()+synthetic+`) l
LEFT JOIN upstream_accounts a ON a.id = l.provider_account_id
LEFT JOIN model_groups g ON g.id = l.group_id
ORDER BY l.created_at DESC
LIMIT $3`, start, end, limit)
	if err != nil {
		return nil, err
	}
	defer recentRows.Close()
	for recentRows.Next() {
		var r obsErrorRecentRow
		if err := recentRows.Scan(&r.At, &r.ErrorClass, &r.HTTPStatus, &r.Model, &r.Account, &r.Group, &r.Message); err != nil {
			return nil, err
		}
		resp.Recent = append(resp.Recent, r)
	}
	return resp, recentRows.Err()
}
