package admin

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/jucode/jucode/internal/data/ent"
	entusagelog "github.com/jucode/jucode/internal/data/ent/usagelog"
	entuser "github.com/jucode/jucode/internal/data/ent/user"
)

// queryGetter fakes gin's c.Query for the parser tests.
func queryGetter(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestParseObsSeriesParams(t *testing.T) {
	t.Parallel()
	const goodUUID = "11111111-2222-3333-4444-555555555555"

	// Defaults: platform / 24h / synthetic excluded.
	p, err := parseObsSeriesParams(queryGetter(nil))
	if err != nil {
		t.Fatalf("defaults: %v", err)
	}
	if p.Dim != "platform" || p.Window != "24h" || p.Spec.Seconds != 86400 || p.Spec.BucketSeconds != 1800 || p.IncludeSynthetic {
		t.Fatalf("defaults = %+v", p)
	}

	// Window → bucket mapping is part of the locked contract.
	for window, want := range map[string][2]int{
		"1h":  {3600, 120},
		"6h":  {21600, 600},
		"24h": {86400, 1800},
	} {
		p, err := parseObsSeriesParams(queryGetter(map[string]string{"window": window}))
		if err != nil {
			t.Fatalf("window %s: %v", window, err)
		}
		if p.Spec.Seconds != want[0] || p.Spec.BucketSeconds != want[1] {
			t.Errorf("window %s → %+v, want %v", window, p.Spec, want)
		}
		if p.Spec.Seconds%p.Spec.BucketSeconds != 0 {
			t.Errorf("window %s does not divide evenly into buckets", window)
		}
	}

	rejected := []map[string]string{
		{"dim": "bogus"},
		{"dim": "provider"}, // provider_id required
		{"dim": "provider", "provider_id": "not-a-uuid"}, // bad uuid
		{"dim": "group"}, // group_id required
		{"dim": "group", "group_id": "not-a-uuid"},                     // bad uuid
		{"dim": "account"},                                             // account_id required
		{"dim": "account", "account_id": "bad"},                        // bad uuid
		{"dim": "model"},                                               // model required
		{"dim": "model", "model": "   "},                               // blank model
		{"window": "12h"},                                              // bad enum
		{"include_synthetic": "yes-please"},                            // bad bool
		{"dim": "provider", "provider_id": goodUUID, "window": "week"}, // bad enum with valid dim
	}
	for _, q := range rejected {
		if _, err := parseObsSeriesParams(queryGetter(q)); err == nil {
			t.Errorf("params %v accepted, want error", q)
		}
	}

	accepted := []map[string]string{
		{"dim": "provider", "provider_id": goodUUID},
		{"dim": "group", "group_id": goodUUID},
		{"dim": "account", "account_id": goodUUID},
		{"dim": "model", "model": "gpt-6"},
		{"include_synthetic": "true"},
		{"include_synthetic": "false"},
	}
	for _, q := range accepted {
		if _, err := parseObsSeriesParams(queryGetter(q)); err != nil {
			t.Errorf("params %v rejected: %v", q, err)
		}
	}
}

func TestParseObsLeaderboardParams(t *testing.T) {
	t.Parallel()
	if _, err := parseObsLeaderboardParams(queryGetter(nil)); err == nil {
		t.Error("missing dim accepted, want error")
	}
	if _, err := parseObsLeaderboardParams(queryGetter(map[string]string{"dim": "platform"})); err == nil {
		t.Error("dim=platform accepted, want error (leaderboard has no platform rows)")
	}
	p, err := parseObsLeaderboardParams(queryGetter(map[string]string{"dim": "provider"}))
	if err != nil {
		t.Fatalf("dim=provider: %v", err)
	}
	if p.Window != "1h" || p.Spec.Seconds != 3600 || p.IncludeSynthetic {
		t.Fatalf("leaderboard defaults = %+v, want window 1h, synthetic excluded", p)
	}
}

// queryProviderPerformance (admin_providers.go) interpolates obsTPSExpr, so
// the provider table and these charts share one formula by construction.
// The literal is pinned anyway: which rows count as TPS samples, and
// whether the mean is per-request or per-token, are product decisions that
// should not change as a side effect of editing a query.
func TestObsQueriesUseTheProviderPerformanceTPSExpression(t *testing.T) {
	t.Parallel()
	// Three product decisions are pinned here. Where the window starts: when
	// the model began producing output, not when the first visible token
	// arrived, which is what keeps reasoning models from reporting a decode
	// speed several times their real one. What makes a window a measurement:
	// a meaningful share of the request (the guard that decides stream vs
	// replay, and the one that scales). And the hard stop behind it: 2ms of
	// window per output token. The filter and the denominator must use the
	// same window.
	const win = "(latency_ms - LEAST(ttft_ms, COALESCE((latency_detail->>'first_output_ms')::bigint, (latency_detail->>'first_sse_frame_ms')::bigint, ttft_ms)))"
	const filter = "status = 'ok' AND tokens_out > 0 AND ttft_ms IS NOT NULL AND " + win + " > 0" +
		" AND " + win + " >= latency_ms * 0.05 AND " + win + " >= tokens_out * 2"
	const want = "SUM(CASE WHEN " + filter + " THEN tokens_out::float8 END) / SUM(CASE WHEN " + filter + " THEN " + win + "::float8 / 1000.0 END)"
	if obsTPSMinDeliveryShare != "0.05" {
		t.Fatalf("delivery-share bound drifted to %s", obsTPSMinDeliveryShare)
	}
	if obsTPSExpr != want {
		t.Fatalf("obsTPSExpr drifted:\n got %s\nwant %s", obsTPSExpr, want)
	}
	p, err := parseObsSeriesParams(queryGetter(nil))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	seriesQ, _ := buildObsSeriesQuery(p, now.Add(-24*time.Hour), now)
	if !strings.Contains(seriesQ, want) {
		t.Errorf("series query lost the pinned TPS expression:\n%s", seriesQ)
	}
	lp, err := parseObsLeaderboardParams(queryGetter(map[string]string{"dim": "model"}))
	if err != nil {
		t.Fatal(err)
	}
	lbQ, _ := buildObsLeaderboardQuery(lp, now.Add(-time.Hour), now)
	if !strings.Contains(lbQ, want) {
		t.Errorf("leaderboard query lost the pinned TPS expression:\n%s", lbQ)
	}
}

// The attempt-level provider view is the only one that reads provider_chain,
// so its SQL is pinned: it must parse the `attempt:` elements (not the bare
// provider ids the marshal-failure fallback can write), skip non-array chains,
// and apply the same synthetic-traffic filter as the other aggregations.
func TestBuildObsProviderAttemptsQuery(t *testing.T) {
	t.Parallel()
	q := buildObsProviderAttemptsQuery(false)
	for _, want := range []string{
		"jsonb_array_elements_text(u.provider_chain)",
		"substring(t.txt FROM 9)::jsonb",
		"t.txt LIKE 'attempt:%'",
		"jsonb_typeof(u.provider_chain) = 'array'",
		"cls IN " + obsProviderFaultClasses,
		"GROUP BY provider_id",
	} {
		if !strings.Contains(q, want) {
			t.Errorf("attempts query missing %q:\n%s", want, q)
		}
	}
	if !strings.Contains(q, "api_keys") {
		t.Errorf("default query does not exclude synthetic traffic:\n%s", q)
	}
	if all := buildObsProviderAttemptsQuery(true); strings.Contains(all, "api_keys") {
		t.Errorf("include_synthetic=true still filters api_keys:\n%s", all)
	}
}

func TestBuildObsSeriesQueryFilters(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	start := now.Add(-24 * time.Hour)

	// Default: synthetic traffic excluded, api_key_id IS NULL kept.
	p, _ := parseObsSeriesParams(queryGetter(nil))
	q, args := buildObsSeriesQuery(p, start, now)
	if !strings.Contains(q, "api_key_id IS NULL OR api_key_id NOT IN (SELECT id FROM api_keys WHERE internal)") {
		t.Errorf("default query does not exclude synthetic traffic:\n%s", q)
	}
	if len(args) != 3 {
		t.Errorf("platform args = %d, want 3 (start, bucket, end)", len(args))
	}

	// include_synthetic=true drops the filter.
	p, _ = parseObsSeriesParams(queryGetter(map[string]string{"include_synthetic": "true"}))
	q, _ = buildObsSeriesQuery(p, start, now)
	if strings.Contains(q, "api_keys") {
		t.Errorf("include_synthetic=true still filters api_keys:\n%s", q)
	}

	// Dim filters bind $4.
	p, _ = parseObsSeriesParams(queryGetter(map[string]string{"dim": "provider", "provider_id": "11111111-2222-3333-4444-555555555555"}))
	q, args = buildObsSeriesQuery(p, start, now)
	if !strings.Contains(q, "AND provider_id = $4") || len(args) != 4 {
		t.Errorf("provider filter missing: args=%d\n%s", len(args), q)
	}
	p, _ = parseObsSeriesParams(queryGetter(map[string]string{"dim": "model", "model": "gpt-6"}))
	q, args = buildObsSeriesQuery(p, start, now)
	if !strings.Contains(q, "AND model = $4") || len(args) != 4 {
		t.Errorf("model filter missing: args=%d\n%s", len(args), q)
	}
}

func TestBuildObsLeaderboardQueryKeys(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	start := now.Add(-time.Hour)
	cases := map[string][2]string{
		"provider": {"provider_id::text AS key", "AND provider_id IS NOT NULL"},
		"group":    {"group_id::text AS key", "AND group_id IS NOT NULL"},
		"account":  {"provider_account_id::text AS key", "AND provider_account_id IS NOT NULL"},
		"model":    {"model AS key", ""},
	}
	for dim, want := range cases {
		p, err := parseObsLeaderboardParams(queryGetter(map[string]string{"dim": dim}))
		if err != nil {
			t.Fatalf("dim %s: %v", dim, err)
		}
		q, args := buildObsLeaderboardQuery(p, start, now)
		if !strings.Contains(q, want[0]) {
			t.Errorf("dim %s: key column missing %q:\n%s", dim, want[0], q)
		}
		if want[1] != "" && !strings.Contains(q, want[1]) {
			t.Errorf("dim %s: null guard missing %q:\n%s", dim, want[1], q)
		}
		if len(args) != 3 {
			t.Errorf("dim %s: args = %d, want 3", dim, len(args))
		} else if rateStart, _ := args[2].(time.Time); !rateStart.Equal(now.Add(-time.Minute)) {
			t.Errorf("dim %s: rate window start = %v, want end - 1m", dim, args[2])
		}
	}
}

// The zero point's JSON is part of the locked response contract: counters 0,
// failure_rate 0, every nullable metric null. Frozen as a full document so a
// field rename or an accidental omitempty shows up as a test diff.
func TestZeroFilledObsPointsContract(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 8, 28, 0, 0, 0, 0, time.UTC)
	points := zeroFilledObsPoints(start, 1800, 48)
	if len(points) != 48 {
		t.Fatalf("points = %d, want 48", len(points))
	}
	if !points[1].Time.Equal(start.Add(30 * time.Minute)) {
		t.Errorf("bucket spacing wrong: points[1].Time = %v", points[1].Time)
	}
	got, err := json.Marshal(points[0])
	if err != nil {
		t.Fatal(err)
	}
	want := `{"time":"2026-08-28T00:00:00Z","requests":0,"ok":0,"failures":0,"failure_rate":0,"provider_faults":0,"provider_fault_rate":null,"capacity_faults":0,"avg_ttft_ms":null,"p50_ttft_ms":null,"p95_ttft_ms":null,"p99_ttft_ms":null,"ttft_count":0,"avg_tps":null,"tps_tokens":0,"avg_latency_ms":null,"p95_latency_ms":null,"tokens_out":0,"retries":0,"anomalies":0,"time_anomalies":0,"error_anomalies":0}`
	if string(got) != want {
		t.Errorf("zero point JSON:\n got %s\nwant %s", got, want)
	}
}

// Postgres-backed execution check: every dim × synthetic combination of the
// generated SQL must parse, execute and scan (ordered-set aggregates with
// FILTER clauses are exactly the kind of thing that only a real server
// validates). Same opt-in as the biz-package Postgres tests:
//
//	JUCODE_TEST_PG_DSN='postgres://postgres:test@127.0.0.1:5499/jucode_test?sslmode=disable' \
//	  go test ./internal/api/admin -run ObsQueriesExecute
func TestObsQueriesExecuteAgainstPostgres(t *testing.T) {
	dsn := os.Getenv("JUCODE_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("JUCODE_TEST_PG_DSN not set; skipping Postgres-backed observability query test")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	client := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	if err := client.Schema.Create(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	ctx := context.Background()

	u, err := client.User.Create().
		SetEmail(uuid.NewString() + "@test.invalid").
		SetPasswordHash("x").
		SetStatus(entuser.StatusActive).
		Save(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	model := "obs-exec-" + uuid.NewString()
	groupID := uuid.New()
	ttft := 1200
	if err := client.UsageLog.Create().
		SetUserID(u.ID).
		SetEndpoint(entusagelog.EndpointChatCompletions).
		SetModel(model).
		SetGroupID(groupID).
		SetStatus(entusagelog.StatusOk).
		SetLatencyMs(3000).
		SetTtftMs(ttft).
		SetTokensOut(500).
		Exec(ctx); err != nil {
		t.Fatalf("create usage log: %v", err)
	}

	now := time.Now().UTC()
	seriesQueries := []map[string]string{
		{},
		{"include_synthetic": "true"},
		{"dim": "provider", "provider_id": uuid.NewString()},
		{"dim": "group", "group_id": groupID.String()},
		{"dim": "account", "account_id": uuid.NewString()},
		{"dim": "model", "model": model, "window": "1h"},
	}
	for _, params := range seriesQueries {
		p, err := parseObsSeriesParams(queryGetter(params))
		if err != nil {
			t.Fatalf("params %v: %v", params, err)
		}
		q, args := buildObsSeriesQuery(p, now.Add(-time.Duration(p.Spec.Seconds)*time.Second), now)
		rows, err := db.QueryContext(ctx, q, args...)
		if err != nil {
			t.Fatalf("series %v failed to execute: %v\n%s", params, err, q)
		}
		for rows.Next() {
			var idx int
			var m obsMetricsScan
			if err := rows.Scan(append([]any{&idx}, m.dest()...)...); err != nil {
				t.Fatalf("series %v scan: %v", params, err)
			}
		}
		if err := rows.Close(); err != nil {
			t.Fatalf("series %v close: %v", params, err)
		}
	}

	for _, dim := range []string{"provider", "group", "account", "model"} {
		p, err := parseObsLeaderboardParams(queryGetter(map[string]string{"dim": dim, "window": "24h"}))
		if err != nil {
			t.Fatalf("leaderboard dim %s: %v", dim, err)
		}
		q, args := buildObsLeaderboardQuery(p, now.Add(-24*time.Hour), now)
		rows, err := db.QueryContext(ctx, q, args...)
		if err != nil {
			t.Fatalf("leaderboard %s failed to execute: %v\n%s", dim, err, q)
		}
		found := false
		for rows.Next() {
			var key string
			var m obsMetricsScan
			var rpm, tpm int64
			if err := rows.Scan(append(append([]any{&key}, m.dest()...), &rpm, &tpm)...); err != nil {
				t.Fatalf("leaderboard %s scan: %v", dim, err)
			}
			if dim == "model" && key == model {
				found = true
				// The fixture row was written just now, inside the trailing minute.
				if rpm != 1 || tpm != 500 {
					t.Errorf("model rpm/tpm = %d/%d, want 1/500", rpm, tpm)
				}
				v := m.view()
				if v.Requests != 1 || v.OK != 1 || v.TokensOut != 500 {
					t.Errorf("model row = %+v, want 1 request / 1 ok / 500 tokens_out", v)
				}
				if v.AvgTTFTMs == nil || *v.AvgTTFTMs != 1200 {
					t.Errorf("model avg_ttft = %v, want 1200", v.AvgTTFTMs)
				}
				// TPS per the pinned formula: 500 tokens over (3000-1200)ms.
				// Served unrounded so consumers can recover the denominator.
				if v.AvgTPS == nil || math.Abs(*v.AvgTPS-500.0/1.8) > 1e-9 {
					t.Errorf("model avg_tps = %v, want %v", v.AvgTPS, 500.0/1.8)
				}
			}
		}
		if err := rows.Close(); err != nil {
			t.Fatalf("leaderboard %s close: %v", dim, err)
		}
		if dim == "model" && !found {
			t.Errorf("leaderboard model dim did not return the fixture row")
		}
	}
}

// TPS is the metric this file gets wrong most easily, and both ways it has
// been wrong were invisible to a single-row fixture: folding rows with no
// ttft_ms in at their whole latency understated it, and averaging per
// request let replies that ended a millisecond after their first token
// overstate it. This fixture holds one of each trap next to two real
// generations, so either mistake moves the number by an order of magnitude.
//
//	JUCODE_TEST_PG_DSN='postgres://postgres:test@127.0.0.1:5499/jucode_test?sslmode=disable' \
//	  go test ./internal/api/admin -run ObsTPSIgnores
func TestObsTPSIgnoresUnmeasurableRowsAndWeighsByToken(t *testing.T) {
	dsn := os.Getenv("JUCODE_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("JUCODE_TEST_PG_DSN not set; skipping Postgres-backed TPS test")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	client := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	if err := client.Schema.Create(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	ctx := context.Background()

	u, err := client.User.Create().
		SetEmail(uuid.NewString() + "@test.invalid").
		SetPasswordHash("x").
		SetStatus(entuser.StatusActive).
		Save(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	model := "obs-tps-" + uuid.NewString()

	// 600 tokens over 2.8s of generation = 214.29 t/s. Every other row in the
	// fixture must be excluded by one of the documented bounds:
	//
	//	no ttft_ms            → no measurable generation window at all
	//	status != ok          → a failed stream is not a decode rate sample
	//	zero-length window    → the whole response landed in the first frame
	//	delivery < 5% of t/r  → the upstream assembled the answer, not streamed
	//	< 2ms window per token→ beyond what any model decodes
	//
	// The 20 tiny replies are the per-request-averaging trap: each ends 1ms
	// after its first token, so a mean of per-request rates reads them as
	// ~2000 t/s each and drags the platform figure up by roughly 8x. They are
	// excluded here by the delivery-share bound, which is the point: the
	// guard that protects the published number is what the fixture pins.
	add := func(latency int, ttft *int, tokens int, status entusagelog.Status) {
		t.Helper()
		q := client.UsageLog.Create().
			SetUserID(u.ID).
			SetEndpoint(entusagelog.EndpointChatCompletions).
			SetModel(model).
			SetStatus(status).
			SetLatencyMs(latency).
			SetTokensOut(tokens)
		if ttft != nil {
			q = q.SetTtftMs(*ttft)
		}
		if err := q.Exec(ctx); err != nil {
			t.Fatalf("create usage log: %v", err)
		}
	}
	ms := func(v int) *int { return &v }
	add(3000, ms(1200), 500, entusagelog.StatusOk) // eligible: 1800ms window
	add(1200, ms(200), 100, entusagelog.StatusOk)  // eligible: 1000ms window
	add(8000, nil, 50, entusagelog.StatusOk)       // never streamed: no window to divide by
	add(900, ms(300), 40, entusagelog.StatusUpstreamError)
	add(900, ms(900), 10, entusagelog.StatusOk) // zero-length window
	for range 20 {
		add(821, ms(820), 2, entusagelog.StatusOk) // 1ms window: a replay, not a decode
	}

	p, err := parseObsLeaderboardParams(queryGetter(map[string]string{"dim": "model", "model": model, "window": "1h"}))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	q, args := buildObsLeaderboardQuery(p, now.Add(-time.Hour), now)
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		t.Fatalf("leaderboard failed to execute: %v\n%s", err, q)
	}
	defer rows.Close()

	found := false
	for rows.Next() {
		var key string
		var m obsMetricsScan
		var rpm, tpm int64
		if err := rows.Scan(append(append([]any{&key}, m.dest()...), &rpm, &tpm)...); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if key != model {
			continue
		}
		found = true
		v := m.view()
		// The two eligible rows: 600 tokens over 1.8s + 1.0s of generation.
		if v.AvgTPS == nil || *v.AvgTPS < 210 || *v.AvgTPS > 220 {
			t.Errorf("avg_tps = %v, want ~214.3 (600 tokens over 2.8s of generation)", v.AvgTPS)
		}
		// The numerator's own sample: exactly the two eligible rows. Neither
		// ttft_count (24, it counts the failed row) nor SUM(tokens_out) (740,
		// it counts every row TPS skipped) is this number, which is why it is
		// published as the rate's coverage figure.
		if v.TPSTokens != 600 {
			t.Errorf("tps_tokens = %d, want 600", v.TPSTokens)
		}
		if v.TTFTCount != 24 || v.TokensOut != 740 {
			t.Errorf("ttft_count/tokens_out = %d/%d, want 24/740 — the fixture no longer distinguishes them from tps_tokens", v.TTFTCount, v.TokensOut)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("leaderboard did not return the fixture model")
	}
}

// provider_fault_rate uses eligible outcomes (ok + provider faults) as its
// denominator, not total requests — user-caused failures neither count for
// nor against the channel.
func TestObsMetricsViewRates(t *testing.T) {
	t.Parallel()
	m := obsMetricsScan{requests: 10, ok: 6, failures: 4, providerFaults: 2}
	v := m.view()
	if v.FailureRate != 0.4 {
		t.Errorf("failure_rate = %v, want 0.4", v.FailureRate)
	}
	if v.ProviderFaultRate == nil || *v.ProviderFaultRate != 0.25 {
		t.Errorf("provider_fault_rate = %v, want 0.25 (2 faults / (6 ok + 2 faults))", v.ProviderFaultRate)
	}

	// No eligible outcomes at all → null, not zero: "no evidence" and
	// "perfect record" must not collapse into the same number.
	m = obsMetricsScan{requests: 3, failures: 3}
	if v := m.view(); v.ProviderFaultRate != nil {
		t.Errorf("provider_fault_rate = %v, want null with no eligible outcomes", *v.ProviderFaultRate)
	}
}
