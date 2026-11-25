package biz

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/redis/go-redis/v9"
	"github.com/shopspring/decimal"

	"github.com/jucode/jucode/internal/data"
	"github.com/jucode/jucode/internal/data/ent"
	entusagelog "github.com/jucode/jucode/internal/data/ent/usagelog"
)

// Parity between the Redis hour counters and the SQL aggregation cannot be
// argued from the code — the two accumulate independently. These tests need
// both backends:
//
//	docker run -d --rm -p 6399:6379 --name jucode-redis-test redis:7-alpine
//	docker run -d --rm -e POSTGRES_PASSWORD=test -e POSTGRES_DB=jucode_test \
//	  -p 5499:5432 --name jucode-pg-test postgres:16-alpine
//	JUCODE_TEST_REDIS_ADDR=127.0.0.1:6399 \
//	JUCODE_TEST_PG_DSN='postgres://postgres:test@127.0.0.1:5499/jucode_test?sslmode=disable' \
//	  go test ./internal/biz -run HourBucket
func testStatsEnv(t *testing.T) (*BillingSvc, *StatsSvc, *ent.Client) {
	t.Helper()
	dsn := os.Getenv("JUCODE_TEST_PG_DSN")
	addr := os.Getenv("JUCODE_TEST_REDIS_ADDR")
	if dsn == "" || addr == "" {
		t.Skip("JUCODE_TEST_PG_DSN / JUCODE_TEST_REDIS_ADDR not set; skipping hour-bucket parity tests")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open pg: %v", err)
	}
	client := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	ctx := context.Background()
	if err := client.Schema.Create(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Fatalf("redis ping: %v", err)
	}
	t.Cleanup(func() { _ = client.Close(); _ = rdb.Close() })

	d := data.NewWith(client, rdb, db)
	stats := NewStatsSvc(d)
	stats.EnableHourBuckets(true)

	// Warm the ring well past any window these tests ask for. Reads refuse
	// windows starting before the ring began collecting, so without this a
	// fresh Redis would decline every query and the tests would be asserting
	// the cold-start guard instead of what they mean to cover.
	// TestHourBucketsRefuseWindowsPredatingTheRing clears it deliberately.
	rdb.Set(ctx, usageBucketSinceKey, usageBucketHour(time.Now().Add(-72*time.Hour)), 0)
	t.Cleanup(func() { rdb.Del(context.Background(), usageBucketSinceKey) })

	return &BillingSvc{d: d}, stats, client
}

// waitForBuckets lets the fire-and-forget counter goroutines land.
func waitForBuckets(t *testing.T, stats *StatsSvc, f StatsFilter, wantRequests int) []StatsBucket {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last []StatsBucket
	for time.Now().Before(deadline) {
		series, served, err := stats.aggregateFromBuckets(context.Background(), f)
		if err != nil {
			t.Fatalf("bucket read: %v", err)
		}
		if !served {
			t.Fatalf("filter was not eligible for the counter path: %+v", f)
		}
		last = series
		total := 0
		for _, b := range series {
			total += b.Requests
		}
		if total == wantRequests {
			return series
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("counters never reached %d requests; last=%+v", wantRequests, last)
	return nil
}

// usage_logs.organization_id is a real foreign key.
func testOrganization(t *testing.T, client *ent.Client) uuid.UUID {
	t.Helper()
	o, err := client.Organization.Create().
		SetName("org-" + uuid.NewString()).
		SetOwnerUserID(testUser(t, client)).
		Save(context.Background())
	if err != nil {
		t.Fatalf("create organization: %v", err)
	}
	return o.ID
}

func sumSeries(series []StatsBucket) (requests, errors int, tin, tout, tcin, tccin int64, cost decimal.Decimal) {
	cost = decimal.Zero
	for _, b := range series {
		requests += b.Requests
		errors += b.Errors
		tin += b.TokensIn
		tout += b.TokensOut
		tcin += b.CachedTokensIn
		tccin += b.CacheCreationTokensIn
		c, err := decimal.NewFromString(b.CostFinalSum)
		if err == nil {
			cost = cost.Add(c)
		}
	}
	return
}

// The headline requirement: the counters must reproduce the SQL answer
// exactly, cost included. Money is compared with decimal equality, never a
// float delta — the whole reason cost is accumulated as scaled int64 via
// HINCRBY instead of HINCRBYFLOAT.
func TestHourBucketsMatchSQLExactly(t *testing.T) {
	billing, stats, client := testStatsEnv(t)
	ctx := context.Background()
	userID := testUser(t, client)

	// The last three carry more than 8 decimals on purpose: that is what
	// CalcCost actually produces (price / 1000, then the group rate
	// multiplier), and both sides must land on the same numeric(20,8) value.
	// With costs that all fit in 8 places this test passes even when the
	// counters drop every over-precise increment on the floor.
	costs := []string{
		"0.00012345", "1.10000000", "0.00000001", "12.34567891", "0.99999999",
		"0.000105375", "0.0000092475", "0.000000005",
	}
	const n = 40
	wantCost := decimal.Zero
	for i := 0; i < n; i++ {
		in := sampleUsageInput(userID, uuid.NewString(), nil)
		in.CostFinal = decimal.RequireFromString(costs[i%len(costs)])
		in.TokensIn = 100 + i
		in.TokensOut = 7 * i
		in.CachedTokensIn = i
		in.CacheCreationTokensIn = 2 * i
		if i%5 == 0 {
			in.Status = entusagelog.StatusUpstreamError
		}
		// Rounded, not raw: numeric(20,8) is the ledger's precision, so this is
		// the value both the SQL sum and the counters are expected to reach.
		wantCost = wantCost.Add(in.CostFinal.Round(moneyScaleExp))
		if _, err := billing.WriteUsageLog(ctx, in); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}

	// Hour-aligned start, end = now: exactly the shape the dashboard sends.
	f := StatsFilter{
		Start:        time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Hour),
		End:          time.Now(),
		Bucket:       "hour",
		UserID:       &userID,
		PersonalOnly: true,
	}

	fromBuckets := waitForBuckets(t, stats, f, n)

	sqlStats := NewStatsSvc(stats.d) // buckets off → authoritative SQL path
	fromSQL, err := sqlStats.Aggregate(ctx, f)
	if err != nil {
		t.Fatalf("sql aggregate: %v", err)
	}

	bReq, bErr, bIn, bOut, bCached, bCacheW, bCost := sumSeries(fromBuckets)
	sReq, sErr, sIn, sOut, sCached, sCacheW, sCost := sumSeries(fromSQL)

	if bReq != sReq || bErr != sErr {
		t.Errorf("requests/errors: buckets=%d/%d sql=%d/%d", bReq, bErr, sReq, sErr)
	}
	if bIn != sIn || bOut != sOut || bCached != sCached || bCacheW != sCacheW {
		t.Errorf("tokens: buckets in=%d out=%d cached=%d cw=%d; sql in=%d out=%d cached=%d cw=%d",
			bIn, bOut, bCached, bCacheW, sIn, sOut, sCached, sCacheW)
	}
	if !bCost.Equal(sCost) {
		t.Errorf("cost mismatch: buckets=%s sql=%s (diff=%s)", bCost, sCost, bCost.Sub(sCost))
	}
	if !bCost.Equal(wantCost) {
		t.Errorf("cost drifted from the exact sum of inputs: got %s want %s", bCost, wantCost)
	}
	if len(fromBuckets) != len(fromSQL) {
		t.Errorf("bucket count: buckets=%d sql=%d", len(fromBuckets), len(fromSQL))
	}
	for i := range fromBuckets {
		if i >= len(fromSQL) {
			break
		}
		if !fromBuckets[i].Time.Equal(fromSQL[i].Time.UTC()) {
			t.Errorf("bucket %d time: buckets=%s sql=%s", i, fromBuckets[i].Time, fromSQL[i].Time.UTC())
		}
	}
}

// Personal charts filter on `organization_id IS NULL`. If the counters did not
// mirror that exclusion, org spend would show up on the personal chart even
// though it never touched the personal balance.
func TestHourBucketsExcludeOrgRowsFromPersonalScope(t *testing.T) {
	billing, stats, client := testStatsEnv(t)
	ctx := context.Background()
	userID := testUser(t, client)
	orgID := testOrganization(t, client)

	personal := sampleUsageInput(userID, uuid.NewString(), nil)
	if _, err := billing.WriteUsageLog(ctx, personal); err != nil {
		t.Fatalf("personal write: %v", err)
	}
	orgRow := sampleUsageInput(userID, uuid.NewString(), nil)
	orgRow.OrganizationID = &orgID
	if _, err := billing.WriteUsageLog(ctx, orgRow); err != nil {
		t.Fatalf("org write: %v", err)
	}

	f := StatsFilter{
		Start:        time.Now().UTC().Add(-time.Hour).Truncate(time.Hour),
		End:          time.Now(),
		Bucket:       "hour",
		UserID:       &userID,
		PersonalOnly: true,
	}
	series := waitForBuckets(t, stats, f, 1) // only the personal row
	req, _, _, _, _, _, _ := sumSeries(series)
	if req != 1 {
		t.Fatalf("personal scope counted %d requests; org-billed row leaked in", req)
	}
}

// Every filter the ring does not shard by, and every window it cannot
// reconstruct exactly, must decline rather than answer approximately.
func TestHourBucketsDeclineIneligibleFilters(t *testing.T) {
	_, stats, client := testStatsEnv(t)
	userID := testUser(t, client)
	alignedStart := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Hour)
	now := time.Now()

	base := StatsFilter{Start: alignedStart, End: now, Bucket: "hour"}

	cases := []struct {
		name string
		mut  func(f *StatsFilter)
	}{
		{"day grouping", func(f *StatsFilter) { f.Bucket = "day" }},
		{"unaligned start", func(f *StatsFilter) { f.Start = alignedStart.Add(30 * time.Minute) }},
		{"historical window", func(f *StatsFilter) {
			f.Start = alignedStart.Add(-48 * time.Hour)
			f.End = now.Add(-40 * time.Hour)
		}},
		{"window longer than the ring", func(f *StatsFilter) { f.Start = alignedStart.Add(-usageBucketCount * time.Hour) }},
		{"model filter", func(f *StatsFilter) { f.Model = "gpt-4o" }},
		{"status filter", func(f *StatsFilter) { f.Status = "ok" }},
		{"group filter", func(f *StatsFilter) { g := uuid.New(); f.GroupID = &g }},
		{"organization scope", func(f *StatsFilter) { o := uuid.New(); f.OrganizationID = &o }},
		{"user without PersonalOnly", func(f *StatsFilter) { f.UserID = &userID }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := base
			tc.mut(&f)
			if _, served, err := stats.aggregateFromBuckets(context.Background(), f); served || err != nil {
				t.Fatalf("served=%v err=%v; want the SQL fallback", served, err)
			}
		})
	}

	// The admin global series (no scoping at all) must still be eligible,
	// otherwise the whole fast path is dead code.
	if _, served, err := stats.aggregateFromBuckets(context.Background(), base); !served || err != nil {
		t.Fatalf("unfiltered admin series: served=%v err=%v; want the counter path", served, err)
	}
}

// The exact shape the dashboard sends: start = top of the hour 24h ago, no
// end. That spans 25 hour buckets and `now - start` sits between 24h and 25h,
// so an eligibility bound expressed as a 24h duration silently rejects it and
// the fast path never fires in production while every test still passes.
func TestHourBucketsServeTheDashboardWindow(t *testing.T) {
	_, stats, client := testStatsEnv(t)
	userID := testUser(t, client)
	now := time.Now()

	// Mirrors rangeToParams("24h") in TokenUsageSection.tsx.
	start := now.Add(-24 * time.Hour).UTC().Truncate(time.Hour)

	for _, tc := range []struct {
		name string
		f    StatsFilter
	}{
		{"admin global", StatsFilter{Start: start, End: now, Bucket: "hour"}},
		{"user personal", StatsFilter{Start: start, End: now, Bucket: "hour", UserID: &userID, PersonalOnly: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if span := usageBucketSpan(tc.f); span != 25 {
				t.Fatalf("dashboard window spans %d buckets, expected 25", span)
			}
			if _, served, err := stats.aggregateFromBuckets(context.Background(), tc.f); !served || err != nil {
				t.Fatalf("served=%v err=%v; the dashboard's own query must hit the counters", served, err)
			}
		})
	}
}

// A window reaching back before the ring started collecting must fall back to
// SQL. Serving it from the ring would render the missing hours as zeros, which
// on a usage chart is indistinguishable from a traffic collapse — and the
// totals beneath it would be quietly wrong.
func TestHourBucketsRefuseWindowsPredatingTheRing(t *testing.T) {
	billing, stats, client := testStatsEnv(t)
	ctx := context.Background()
	userID := testUser(t, client)

	// Clear the mark, then write one row so the ring re-arms at "now".
	stats.d.Redis.Del(ctx, usageBucketSinceKey)
	t.Cleanup(func() { stats.d.Redis.Del(ctx, usageBucketSinceKey) })
	if _, err := billing.WriteUsageLog(ctx, sampleUsageInput(userID, uuid.NewString(), nil)); err != nil {
		t.Fatalf("seed write: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if stats.d.Redis.Exists(ctx, usageBucketSinceKey).Val() == 1 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}

	now := time.Now()
	nowHour := time.Now().UTC().Truncate(time.Hour)

	// The mark must name the first COMPLETE hour, not the partial one the
	// process happened to start in. Serving that partial hour as if it were
	// whole is the same silent under-count the mark exists to prevent.
	since, err := stats.d.Redis.Get(ctx, usageBucketSinceKey).Int64()
	if err != nil {
		t.Fatalf("since mark not written: %v", err)
	}
	if want := usageBucketHour(nowHour) + 1; since != want {
		t.Fatalf("since = %d, want %d (the hour after the partial start hour)", since, want)
	}

	// Starts in the partial start hour: must be refused.
	partial := StatsFilter{Start: nowHour, End: now, Bucket: "hour"}
	if _, ok, err := stats.aggregateFromBuckets(ctx, partial); ok || err != nil {
		t.Fatalf("partial start hour: served=%v err=%v; its first bucket is incomplete", ok, err)
	}

	// Starts 12 hours ago: the ring only armed this hour, so it cannot answer.
	cold := StatsFilter{Start: nowHour.Add(-12 * time.Hour), End: now, Bucket: "hour"}
	if _, ok, err := stats.aggregateFromBuckets(ctx, cold); ok || err != nil {
		t.Fatalf("pre-warm window: served=%v err=%v; it would have reported zeros for hours the ring never saw", ok, err)
	}
}

// Pure unit: the fixed-point conversion is what keeps money exact at the
// ledger's own precision.
func TestUsageCostFixedPoint(t *testing.T) {
	for _, s := range []string{"0", "0.00000001", "1.10000000", "12.34567891", "-0.00000002", "99999.99999999"} {
		d := decimal.RequireFromString(s)
		n, ok := moneyToFixed(d)
		if !ok {
			t.Fatalf("%s: not representable", s)
		}
		if got := moneyFromFixed(n); !got.Equal(d) {
			t.Errorf("%s: round trip gave %s", s, got)
		}
	}
}

// Costs carrying more precision than numeric(20,8) are routine — CalcCost
// divides by 1000 per component, so a 6-decimal price reaches 9 decimals before
// the group rate multiplier is applied. These must be counted at the ledger's
// precision, not dropped: dropping them under-counted every cost series by
// 3-6% per hour in production.
func TestUsageCostFixedPointQuantizesRatherThanDropping(t *testing.T) {
	cases := []struct{ in, want string }{
		{"0.000000001", "0.00000000"},   // below the ledger's resolution, rounds to zero
		{"0.000000005", "0.00000001"},   // half-away-from-zero, matching Postgres numeric
		{"0.000105375", "0.00010538"},   // 6-decimal price / 1000, the common shape
		{"-0.000000005", "-0.00000001"}, // symmetric for refunds
	}
	for _, c := range cases {
		n, ok := moneyToFixed(decimal.RequireFromString(c.in))
		if !ok {
			t.Fatalf("%s: refused; the increment would have been dropped", c.in)
		}
		if got := moneyFromFixed(n); !got.Equal(decimal.RequireFromString(c.want)) {
			t.Errorf("%s: got %s, want %s", c.in, got, c.want)
		}
	}
}

// The one case that must still be refused: a value so large that accumulating
// it would wrap the counter. Dropping beats reporting a negative total.
func TestUsageCostFixedPointRefusesOverflow(t *testing.T) {
	if _, ok := moneyToFixed(decimal.RequireFromString("1e12")); ok {
		t.Error("1e12 accepted; scaled by 1e8 that overflows the int64 counter")
	}
}
