package biz

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/data"
)

// StatsSvc serves time-bucketed aggregations of usage_logs for the
// admin / user dashboards. Implemented with raw SQL because Ent's
// group-by composition is awkward for date_trunc.
type StatsSvc struct {
	d *data.Data
	// bucketsEnabled routes eligible recent-hour queries to the Redis hour
	// counters. Off keeps every query on SQL — the rollback switch.
	bucketsEnabled bool
}

func NewStatsSvc(d *data.Data) *StatsSvc { return &StatsSvc{d: d} }

// EnableHourBuckets turns on the Redis-backed fast path for recent hourly
// windows. Safe to leave off: the SQL path is unchanged and authoritative.
func (s *StatsSvc) EnableHourBuckets(on bool) { s.bucketsEnabled = on }

// HourBucketsEnabled lets HTTP handlers pick a response-cache TTL that matches
// what a miss actually costs. With the counters on, a miss is one pipelined
// Redis round trip and the cache can be short; with them off it is a full 24h
// scan of usage_logs and the cache is the only thing keeping that rare.
func (s *StatsSvc) HourBucketsEnabled() bool { return s.bucketsEnabled }

type StatsBucket struct {
	Time                  time.Time `json:"time"`
	Requests              int       `json:"requests"`
	TokensIn              int64     `json:"tokens_in"`
	TokensOut             int64     `json:"tokens_out"`
	CachedTokensIn        int64     `json:"cached_tokens_in"`
	CacheCreationTokensIn int64     `json:"cache_creation_tokens_in"`
	AudioTokensIn         int64     `json:"audio_tokens_in"`
	AudioTokensOut        int64     `json:"audio_tokens_out"`
	CostFinalSum          string    `json:"cost_final_sum"`
	Errors                int       `json:"errors"`
}

type StatsFilter struct {
	Start  time.Time
	End    time.Time
	Bucket string // "hour" | "day"
	UserID *uuid.UUID
	// OrganizationID narrows to one organization's rows (org account
	// context). Combine with UserID for the member view (own rows within
	// the org); leave UserID nil for the owner/admin org-wide view.
	OrganizationID *uuid.UUID
	// PersonalOnly excludes organization-billed rows (organization_id IS
	// NULL). Set for the personal account context so org spending — which
	// never touched the personal balance — doesn't inflate personal charts.
	PersonalOnly bool
	Model        string
	Status       string
	GroupID      *uuid.UUID
}

// maxStatsSpan caps how much of usage_logs one stats query may scan.
// The table is the largest in the DB and these aggregations read every
// row in [start, end) — an unbounded client-supplied range is a multi-GB
// scan. Oversized windows keep their end and lose the excess history.
const maxStatsSpan = 92 * 24 * time.Hour

// NormalizeWindow is normalizeWindow for HTTP handlers, which need concrete
// Start/End values to echo back in the response before handing the filter to
// Aggregate. Idempotent, so Aggregate re-applying it is harmless.
func (f *StatsFilter) NormalizeWindow() { f.normalizeWindow() }

// normalizeWindow applies the shared window defaulting (end=now,
// start=end-7d) and clamps the span to maxStatsSpan.
func (f *StatsFilter) normalizeWindow() {
	if f.End.IsZero() {
		f.End = time.Now()
	}
	if f.Start.IsZero() {
		f.Start = f.End.AddDate(0, 0, -7)
	}
	if f.End.Sub(f.Start) > maxStatsSpan {
		f.Start = f.End.Add(-maxStatsSpan)
	}
}

// SnapWindow quantizes the window so that repeated polls of a rolling range
// resolve to the same window instead of a new one every time.
//
// Two separate mechanisms depend on this, and both were silently disabled
// without it:
//
//   - The Redis hour ring only answers windows whose Start sits exactly on a
//     UTC hour (see bucketSeriesEligible in usage_buckets.go). A console
//     computing `start = now - 24h` carries minutes and seconds, so it never
//     qualified and every poll fell through to a SQL aggregation.
//   - The HTTP response caches key on the window. A window recomputed per poll
//     produces a unique key each time, so the cache took a 100% miss rate and
//     singleflight never collapsed concurrent pollers either.
//
// Start snaps down to the bucket boundary — a window opening mid-hour already
// yields a partial leading bucket, so this changes the shape of the answer far
// less than it looks. End snaps down to step, which is the caller's cache TTL:
// within one TTL every poll then agrees on the same window.
//
// Day buckets snap to midnight in the business timezone, matching finance
// windows and the operator calendar. Hour buckets stay on UTC hour edges,
// which coincide with local hours in any whole-hour zone.
//
// step <= 0 leaves End alone. Snapping never inverts the window.
func (f *StatsFilter) SnapWindow(step time.Duration) {
	f.normalizeWindow()

	switch strings.ToLower(strings.TrimSpace(f.Bucket)) {
	case "hour":
		f.Start = f.Start.UTC().Truncate(time.Hour)
	case "day":
		f.Start = startOfDay(f.Start)
	default:
		return
	}

	end := f.End.UTC()
	if step > 0 {
		end = end.Truncate(step)
	}
	if !end.After(f.Start) {
		// Sub-step window, or a caller asking for a range narrower than one
		// bucket. Keep the original end rather than returning an empty range.
		end = f.End.UTC()
	}
	f.End = end
}

// statsTimeTruncSQL is the GROUP BY expression for a usage time series.
// Day buckets are calendar days in the business timezone; hour buckets stay
// on timestamptz hour edges.
func statsTimeTruncSQL(bucket string) string {
	if strings.ToLower(strings.TrimSpace(bucket)) == "hour" {
		return "date_trunc('hour', created_at)"
	}
	// The name is checked by SetTimezone (tzNamePattern) before it can land here.
	tz := "'" + TimezoneName() + "'"
	return "date_trunc('day', created_at AT TIME ZONE " + tz + ") AT TIME ZONE " + tz
}

// CacheSuffix is a canonical identity for everything this filter selects,
// for use as a response-cache key.
//
// Deliberately built from the parsed filter rather than the raw query string:
// the query string carries millisecond-precision timestamps and arbitrary
// parameter ordering, neither of which changes the answer but both of which
// change the key. Pair with SnapWindow so the timestamps are stable too.
//
// Covers only the window and the dimension filters. The identity of WHO may
// see the rows — UserID, OrganizationID, PersonalOnly — is deliberately absent,
// because callers already prefix with a scope key that encodes it. Using this
// as a whole key would leak one tenant's aggregates to another.
func (f StatsFilter) CacheSuffix() string {
	var b strings.Builder
	b.WriteString(f.Start.UTC().Format(time.RFC3339))
	b.WriteByte('|')
	b.WriteString(f.End.UTC().Format(time.RFC3339))
	b.WriteByte('|')
	b.WriteString(strings.ToLower(strings.TrimSpace(f.Bucket)))
	b.WriteString("|m=")
	b.WriteString(f.Model)
	b.WriteString("|s=")
	b.WriteString(f.Status)
	if f.GroupID != nil {
		b.WriteString("|g=")
		b.WriteString(f.GroupID.String())
	}
	return b.String()
}

// Aggregate returns one bucket per time slot in [start, end]. End-exclusive.
// Empty buckets are NOT zero-filled — caller can do it client-side if needed.
func (s *StatsSvc) Aggregate(ctx context.Context, f StatsFilter) ([]StatsBucket, error) {
	bucket := strings.ToLower(strings.TrimSpace(f.Bucket))
	if bucket != "hour" && bucket != "day" {
		bucket = "day"
	}
	f.normalizeWindow()
	f.Bucket = bucket

	// Recent hourly windows are served from the Redis hour counters instead
	// of scanning usage_logs; see usage_buckets.go. Anything the counters
	// cannot answer exactly (older window, non-hour grouping, any dimension
	// filter) falls through to the SQL below, and a Redis error does too.
	if s.bucketsEnabled {
		if series, served, err := s.aggregateFromBuckets(ctx, f); err == nil && served {
			return series, nil
		}
	}

	// Build WHERE clauses dynamically with positional params.
	where := []string{"created_at >= $1", "created_at < $2"}
	args := []any{f.Start, f.End}
	addArg := func(clause string, v any) {
		args = append(args, v)
		where = append(where, fmt.Sprintf(clause, len(args)))
	}
	if f.UserID != nil {
		addArg("user_id = $%d", *f.UserID)
	}
	if f.OrganizationID != nil {
		addArg("organization_id = $%d", *f.OrganizationID)
	} else if f.PersonalOnly {
		where = append(where, "organization_id IS NULL")
	}
	if f.Model != "" {
		addArg("model = $%d", f.Model)
	}
	if f.Status != "" {
		addArg("status = $%d", f.Status)
	}
	if f.GroupID != nil {
		addArg("group_id = $%d", *f.GroupID)
	}

	q := fmt.Sprintf(`
		SELECT
		  %s AS t,
		  COUNT(*)                              AS requests,
		  COALESCE(SUM(tokens_in), 0)           AS tokens_in,
		  COALESCE(SUM(tokens_out), 0)          AS tokens_out,
		  COALESCE(SUM(cached_tokens_in), 0)    AS cached_tokens_in,
		  COALESCE(SUM(cache_creation_tokens_in), 0) AS cache_creation_tokens_in,
		  COALESCE(SUM(audio_tokens_in), 0)     AS audio_tokens_in,
		  COALESCE(SUM(audio_tokens_out), 0)    AS audio_tokens_out,
		  COALESCE(SUM(cost_final), 0)::text    AS cost_final_sum,
		  COUNT(*) FILTER (WHERE status <> 'ok') AS errors
		FROM usage_logs
		WHERE %s
		GROUP BY t
		ORDER BY t ASC
	`, statsTimeTruncSQL(bucket), strings.Join(where, " AND "))

	rows, err := s.d.DB().QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]StatsBucket, 0, 30)
	for rows.Next() {
		var b StatsBucket
		var t sql.NullTime
		if err := rows.Scan(&t, &b.Requests, &b.TokensIn, &b.TokensOut, &b.CachedTokensIn, &b.CacheCreationTokensIn,
			&b.AudioTokensIn, &b.AudioTokensOut, &b.CostFinalSum, &b.Errors); err != nil {
			return nil, err
		}
		b.Time = t.Time
		out = append(out, b)
	}
	return out, rows.Err()
}

// LifetimeStats is the all-time rollup for one user's usage — not
// window-clamped like Aggregate (maxStatsSpan), so it can honestly label
// itself "总计" on the dashboard.
type LifetimeStats struct {
	TotalRequests int    `json:"total_requests"`
	TotalCost     string `json:"total_cost"`
}

// Lifetime aggregates every historical row matching the filter's account
// scope (UserID / OrganizationID / PersonalOnly — the time window and
// bucket fields are ignored). Scans stay index-served via (user_id,
// created_at) or the partial (organization_id, created_at) index. Callers
// must cache the result — on heavy accounts this still touches every
// historical row in scope.
func (s *StatsSvc) Lifetime(ctx context.Context, f StatsFilter) (LifetimeStats, error) {
	where := []string{}
	args := []any{}
	if f.UserID != nil {
		args = append(args, *f.UserID)
		where = append(where, fmt.Sprintf("user_id = $%d", len(args)))
	}
	if f.OrganizationID != nil {
		args = append(args, *f.OrganizationID)
		where = append(where, fmt.Sprintf("organization_id = $%d", len(args)))
	} else if f.PersonalOnly {
		where = append(where, "organization_id IS NULL")
	}
	if len(where) == 0 {
		return LifetimeStats{}, fmt.Errorf("lifetime stats require a user or organization scope")
	}
	var out LifetimeStats
	err := s.d.DB().QueryRowContext(ctx, fmt.Sprintf(`
		SELECT COUNT(*), COALESCE(SUM(cost_final), 0)::text
		FROM usage_logs
		WHERE %s
	`, strings.Join(where, " AND ")), args...).Scan(&out.TotalRequests, &out.TotalCost)
	return out, err
}

// Breakdown returns aggregated rows grouped by a single dimension (model
// or status), no time bucketing. Useful for the admin pie/bar of "top
// models in the last 24h".
type StatsBreakdownRow struct {
	Key          string `json:"key"`
	Label        string `json:"label,omitempty"` // human-readable hint; populated for dimensions whose key is a UUID (e.g. "user" → email, "provider" → name)
	Requests     int    `json:"requests"`
	Errors       int    `json:"errors"` // non-ok requests in this bucket (for error-rate views)
	TokensIn     int64  `json:"tokens_in"`
	TokensOut    int64  `json:"tokens_out"`
	CostFinalSum string `json:"cost_final_sum"`
}

func (s *StatsSvc) Breakdown(ctx context.Context, dimension string, f StatsFilter) ([]StatsBreakdownRow, error) {
	f.normalizeWindow()

	// "user" needs an extra column (email via join). Keep it as a separate
	// query path rather than smearing label-derivation into every dimension.
	if dimension == "user" {
		return s.breakdownByUser(ctx, f)
	}

	if dimension == "provider" {
		return s.breakdownByProvider(ctx, f)
	}

	// col is a trusted SQL fragment chosen from a fixed allow-list (never
	// user input) so it is safe to interpolate. error_class normalizes the
	// empty default to a visible "(none)" bucket; http_status casts to text.
	col := ""
	switch dimension {
	case "model":
		col = "model"
	case "status":
		col = "status"
	case "billing_source":
		col = "billing_source"
	case "error_class":
		col = "COALESCE(NULLIF(error_class, ''), '(none)')"
	case "http_status":
		col = "http_status::text"
	default:
		return nil, fmt.Errorf("unsupported dimension: %s", dimension)
	}
	where := []string{"created_at >= $1", "created_at < $2"}
	args := []any{f.Start, f.End}
	if f.UserID != nil {
		args = append(args, *f.UserID)
		where = append(where, fmt.Sprintf("user_id = $%d", len(args)))
	}
	if f.OrganizationID != nil {
		args = append(args, *f.OrganizationID)
		where = append(where, fmt.Sprintf("organization_id = $%d", len(args)))
	} else if f.PersonalOnly {
		where = append(where, "organization_id IS NULL")
	}
	// audio_tokens_* fold into the text terms here and in the sibling
	// breakdowns below. These rows report one input and one output number
	// with no modality columns, and audio used to live inside tokens_in /
	// tokens_out — adding it back is what keeps a voice-heavy model from
	// suddenly charting at a fraction of its real volume. (Cached tokens were
	// never part of these sums, so they stay out.)
	q := fmt.Sprintf(`
		SELECT %s AS k,
		       COUNT(*) AS requests,
		       COUNT(*) FILTER (WHERE status <> 'ok') AS errors,
		       COALESCE(SUM(tokens_in + audio_tokens_in),0) AS tokens_in,
		       COALESCE(SUM(tokens_out + audio_tokens_out),0) AS tokens_out,
		       COALESCE(SUM(cost_final),0)::text AS cost_sum
		FROM usage_logs
		WHERE %s
		GROUP BY k
		ORDER BY requests DESC
		LIMIT 50
	`, col, strings.Join(where, " AND "))

	rows, err := s.d.DB().QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []StatsBreakdownRow{}
	for rows.Next() {
		var r StatsBreakdownRow
		if err := rows.Scan(&r.Key, &r.Requests, &r.Errors, &r.TokensIn, &r.TokensOut, &r.CostFinalSum); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// breakdownByProvider groups by the FINAL provider_id and joins providers
// for the name label. This attributes each request to the channel that
// ultimately served (or last failed) it — per-attempt attribution would
// require parsing provider_chain and is intentionally out of scope here.
func (s *StatsSvc) breakdownByProvider(ctx context.Context, f StatsFilter) ([]StatsBreakdownRow, error) {
	where := []string{"ul.created_at >= $1", "ul.created_at < $2"}
	args := []any{f.Start, f.End}
	if f.UserID != nil {
		args = append(args, *f.UserID)
		where = append(where, fmt.Sprintf("ul.user_id = $%d", len(args)))
	}
	if f.OrganizationID != nil {
		args = append(args, *f.OrganizationID)
		where = append(where, fmt.Sprintf("ul.organization_id = $%d", len(args)))
	} else if f.PersonalOnly {
		where = append(where, "ul.organization_id IS NULL")
	}
	q := fmt.Sprintf(`
		SELECT COALESCE(ul.provider_id::text, '(none)') AS k,
		       COALESCE(p.name, '')                     AS label,
		       COUNT(*)                                  AS requests,
		       COUNT(*) FILTER (WHERE ul.status <> 'ok') AS errors,
		       COALESCE(SUM(ul.tokens_in + ul.audio_tokens_in), 0)   AS tokens_in,
		       COALESCE(SUM(ul.tokens_out + ul.audio_tokens_out), 0) AS tokens_out,
		       COALESCE(SUM(ul.cost_final), 0)::text     AS cost_sum
		FROM usage_logs ul
		LEFT JOIN providers p ON p.id = ul.provider_id
		WHERE %s
		GROUP BY ul.provider_id, p.name
		ORDER BY requests DESC
		LIMIT 50
	`, strings.Join(where, " AND "))
	rows, err := s.d.DB().QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []StatsBreakdownRow{}
	for rows.Next() {
		var r StatsBreakdownRow
		if err := rows.Scan(&r.Key, &r.Label, &r.Requests, &r.Errors, &r.TokensIn, &r.TokensOut, &r.CostFinalSum); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// AggregateByModelRow is one (time, model) slice of usage. Caller pivots
// in the frontend so the chart can render one stacked series per model.
type AggregateByModelRow struct {
	Time         time.Time `json:"time"`
	Model        string    `json:"model"`
	Requests     int       `json:"requests"`
	TokensIn     int64     `json:"tokens_in"`
	TokensOut    int64     `json:"tokens_out"`
	CostFinalSum string    `json:"cost_final_sum"`
}

// AggregateByModel groups usage rows by (time bucket, model). Shares the
// same filter shape as Aggregate.
func (s *StatsSvc) AggregateByModel(ctx context.Context, f StatsFilter) ([]AggregateByModelRow, error) {
	bucket := strings.ToLower(strings.TrimSpace(f.Bucket))
	if bucket != "hour" && bucket != "day" {
		bucket = "day"
	}
	f.normalizeWindow()
	where := []string{"created_at >= $1", "created_at < $2"}
	args := []any{f.Start, f.End}
	addArg := func(clause string, v any) {
		args = append(args, v)
		where = append(where, fmt.Sprintf(clause, len(args)))
	}
	if f.UserID != nil {
		addArg("user_id = $%d", *f.UserID)
	}
	if f.OrganizationID != nil {
		addArg("organization_id = $%d", *f.OrganizationID)
	} else if f.PersonalOnly {
		where = append(where, "organization_id IS NULL")
	}
	if f.Model != "" {
		addArg("model = $%d", f.Model)
	}

	q := fmt.Sprintf(`
		SELECT %s AS t,
		       model,
		       COUNT(*)                              AS requests,
		       COALESCE(SUM(tokens_in + audio_tokens_in), 0)   AS tokens_in,
		       COALESCE(SUM(tokens_out + audio_tokens_out), 0) AS tokens_out,
		       COALESCE(SUM(cost_final), 0)::text    AS cost_final_sum
		FROM usage_logs
		WHERE %s
		GROUP BY t, model
		ORDER BY t ASC, model ASC
	`, statsTimeTruncSQL(bucket), strings.Join(where, " AND "))

	rows, err := s.d.DB().QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]AggregateByModelRow, 0, 64)
	for rows.Next() {
		var r AggregateByModelRow
		var t sql.NullTime
		if err := rows.Scan(&t, &r.Model, &r.Requests, &r.TokensIn, &r.TokensOut, &r.CostFinalSum); err != nil {
			return nil, err
		}
		r.Time = t.Time
		out = append(out, r)
	}
	return out, rows.Err()
}

// AggregateByAPIKeyRow is one API key's slice of a usage window.
//
// Unlike AggregateByModelRow there is no time dimension: the console renders
// this as a ranked list, and grouping by (bucket, key) would multiply the row
// count for an answer nobody plots over time.
//
// APIKeyID is empty for calls that carried no key — OAuth device sessions
// (CLI/Desktop) authenticate via authorization_id and leave api_key_id NULL.
// Those collapse into a single row so the shares still sum to the window's
// total spend; the caller labels it.
type AggregateByAPIKeyRow struct {
	APIKeyID     string `json:"api_key_id"`
	APIKeyName   string `json:"api_key_name"`
	Revoked      bool   `json:"revoked"`
	Requests     int    `json:"requests"`
	TokensIn     int64  `json:"tokens_in"`
	TokensOut    int64  `json:"tokens_out"`
	CostFinalSum string `json:"cost_final_sum"`
}

// aggregateByAPIKeyLimit caps the ranked list. A user with more live keys than
// this is not reading past the cut anyway, and it bounds the join below.
const aggregateByAPIKeyLimit = 50

// AggregateByAPIKey groups usage rows by api_key_id over the whole window.
// Shares the filter shape of Aggregate/AggregateByModel; f.Bucket is ignored
// except that callers still set it so SnapWindow and CacheSuffix agree with
// the sibling endpoints.
//
// The name is resolved by joining api_keys *after* the aggregation, so the
// join runs over at most one row per key rather than over the scan. Renaming a
// key therefore relabels its whole history, which is what a distribution chart
// wants; usage_logs.api_key_name (the at-request-time snapshot the audit log
// shows) is the fallback for keys that no longer exist.
func (s *StatsSvc) AggregateByAPIKey(ctx context.Context, f StatsFilter) ([]AggregateByAPIKeyRow, error) {
	f.normalizeWindow()
	where := []string{"created_at >= $1", "created_at < $2"}
	args := []any{f.Start, f.End}
	addArg := func(clause string, v any) {
		args = append(args, v)
		where = append(where, fmt.Sprintf(clause, len(args)))
	}
	if f.UserID != nil {
		addArg("user_id = $%d", *f.UserID)
	}
	if f.OrganizationID != nil {
		addArg("organization_id = $%d", *f.OrganizationID)
	} else if f.PersonalOnly {
		where = append(where, "organization_id IS NULL")
	}
	if f.Model != "" {
		addArg("model = $%d", f.Model)
	}

	// MAX(api_key_name) rather than the newest snapshot: it is only read when
	// the key row is gone (hard-deleted), and picking the lexical max avoids
	// sorting the scan just to break a tie that revoked keys never hit —
	// revocation sets revoked_at, it does not delete the row.
	q := fmt.Sprintf(`
		WITH agg AS (
			SELECT api_key_id,
			       MAX(api_key_name)                     AS snapshot_name,
			       COUNT(*)                              AS requests,
			       COALESCE(SUM(tokens_in + audio_tokens_in), 0)   AS tokens_in,
			       COALESCE(SUM(tokens_out + audio_tokens_out), 0) AS tokens_out,
			       COALESCE(SUM(cost_final), 0)          AS cost_final_sum
			FROM usage_logs
			WHERE %s
			GROUP BY api_key_id
		)
		SELECT COALESCE(a.api_key_id::text, '')                            AS id,
		       COALESCE(NULLIF(k.name, ''), COALESCE(a.snapshot_name, ''))  AS name,
		       (k.revoked_at IS NOT NULL)                                   AS revoked,
		       a.requests, a.tokens_in, a.tokens_out,
		       a.cost_final_sum::text
		FROM agg a
		LEFT JOIN api_keys k ON k.id = a.api_key_id
		ORDER BY a.cost_final_sum DESC, a.requests DESC
		LIMIT %d
	`, strings.Join(where, " AND "), aggregateByAPIKeyLimit)

	rows, err := s.d.DB().QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]AggregateByAPIKeyRow, 0, 16)
	for rows.Next() {
		var r AggregateByAPIKeyRow
		if err := rows.Scan(&r.APIKeyID, &r.APIKeyName, &r.Revoked, &r.Requests, &r.TokensIn, &r.TokensOut, &r.CostFinalSum); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// breakdownByUser returns the top-20 users by request count, joining the
// users table so the UI can show emails without a follow-up N+1.
func (s *StatsSvc) breakdownByUser(ctx context.Context, f StatsFilter) ([]StatsBreakdownRow, error) {
	q := `
		SELECT ul.user_id::text          AS k,
		       COALESCE(u.email, '')     AS email,
		       COUNT(*)                  AS requests,
		       COALESCE(SUM(ul.tokens_in + ul.audio_tokens_in), 0)  AS tokens_in,
		       COALESCE(SUM(ul.tokens_out + ul.audio_tokens_out), 0) AS tokens_out,
		       COALESCE(SUM(ul.cost_final), 0)::text AS cost_sum
		FROM usage_logs ul
		LEFT JOIN users u ON u.id = ul.user_id
		WHERE ul.created_at >= $1 AND ul.created_at < $2
		GROUP BY ul.user_id, u.email
		ORDER BY requests DESC
		LIMIT 20
	`
	rows, err := s.d.DB().QueryContext(ctx, q, f.Start, f.End)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []StatsBreakdownRow{}
	for rows.Next() {
		var r StatsBreakdownRow
		if err := rows.Scan(&r.Key, &r.Label, &r.Requests, &r.TokensIn, &r.TokensOut, &r.CostFinalSum); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
