package biz

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/shopspring/decimal"

	"github.com/jucode/jucode/internal/data"
	"github.com/jucode/jucode/internal/data/ent"
)

// The quota counters gate whether a user on a plan may keep making requests,
// and they live entirely in Redis — the arithmetic cannot be argued from the
// Go code alone, so these tests need a real server:
//
//	docker run -d --rm -p 6399:6379 --name jucode-redis-test redis:7-alpine
//	JUCODE_TEST_REDIS_ADDR=127.0.0.1:6399 go test ./internal/biz -run PlanQuota
func testPlanQuotaEnv(t *testing.T) (*PlanSvc, *redis.Client, *ent.UserPlan, *ent.Plan) {
	t.Helper()
	addr := os.Getenv("JUCODE_TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("JUCODE_TEST_REDIS_ADDR not set; skipping plan quota tests")
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("redis ping: %v", err)
	}
	t.Cleanup(func() { _ = rdb.Close() })

	up := &ent.UserPlan{ID: uuid.New(), StartAt: time.Now().Add(-time.Hour)}
	plan := &ent.Plan{
		Quota5h:      decimal.RequireFromString("100"),
		QuotaWeekly:  decimal.RequireFromString("1000"),
		QuotaMonthly: decimal.RequireFromString("10000"),
	}
	t.Cleanup(func() {
		_ = rdb.Del(context.Background(), quotaKeys(up, time.Now())...).Err()
	})
	return NewPlanSvc(data.NewWith(nil, rdb, nil), nil), rdb, up, plan
}

// Repeated accumulation of a value with no exact binary representation must
// sum exactly.
//
// Worth stating what this does and does not prove: INCRBYFLOAT would also pass
// it. Redis accumulates in 80-bit long double and trims the formatted result,
// and decimal.NewFromFloat reconstructs via shortest-round-trip, so the old
// path had no measurable error at the magnitudes a quota window reaches. The
// counters are fixed-point because that is one fewer representation to reason
// about — identical to the usage buckets and to the numeric(20,8) ledger they
// are compared against — not because drift was ever observed in production.
func TestPlanQuotaAccumulatesExactly(t *testing.T) {
	svc, _, up, plan := testPlanQuotaEnv(t)
	ctx := context.Background()

	cost := decimal.RequireFromString("0.1")
	const n = 1000
	var view UsageView
	for i := 0; i < n; i++ {
		var err error
		view, err = svc.ConsumeQuota(ctx, up, plan, cost)
		if err != nil {
			t.Fatalf("consume %d: %v", i, err)
		}
	}

	want := decimal.RequireFromString("100")
	if !view.Used5h.Equal(want) {
		t.Errorf("used_5h = %s, want exactly %s", view.Used5h, want)
	}
	if !view.UsedWeekly.Equal(want) || !view.UsedMonthly.Equal(want) {
		t.Errorf("weekly=%s monthly=%s, want %s for both", view.UsedWeekly, view.UsedMonthly, want)
	}

	peek, err := svc.PeekQuota(ctx, up)
	if err != nil {
		t.Fatalf("peek: %v", err)
	}
	if !peek.Used5h.Equal(want) {
		t.Errorf("peek disagrees with consume: %s vs %s", peek.Used5h, view.Used5h)
	}
}

// Costs carrying more precision than the ledger holds are the norm, not the
// exception (CalcCost divides by 1000 per component). They must be counted at
// numeric(20,8), never dropped.
func TestPlanQuotaCountsOverPreciseCosts(t *testing.T) {
	svc, _, up, plan := testPlanQuotaEnv(t)
	ctx := context.Background()

	// 9 decimals: exactly what a 6-decimal unit price becomes after /1000.
	view, err := svc.ConsumeQuota(ctx, up, plan, decimal.RequireFromString("0.000105375"))
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if !view.Used5h.Equal(decimal.RequireFromString("0.00010538")) {
		t.Errorf("used_5h = %s, want 0.00010538 (quantized, not dropped)", view.Used5h)
	}
}

// Deliberate behavior change from the INCRBYFLOAT era, pinned here so it is
// not mistaken for a bug: a cost below the ledger's own resolution rounds to
// zero and does not advance the counter. It previously accumulated.
//
// This is the consistent choice — such a request does not move the balance
// either, because numeric(20,8) rounds it away on store — but it does mean a
// user issuing only sub-1e-8 requests burns no quota. Reachable in principle
// (a 1-token call on a cheap model under a 0.03 rate multiplier lands near
// 4.5e-9); the same request bills nothing, so the two agree.
func TestPlanQuotaIgnoresCostsBelowLedgerResolution(t *testing.T) {
	svc, _, up, plan := testPlanQuotaEnv(t)
	ctx := context.Background()

	view, err := svc.ConsumeQuota(ctx, up, plan, decimal.RequireFromString("0.000000004"))
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if !view.Used5h.IsZero() {
		t.Errorf("used_5h = %s, want 0", view.Used5h)
	}
}

// Reservation settlement may be retried after a process dies between Redis
// consumption and a durable completion flag. The same settlement key must
// advance all windows exactly once.
func TestPlanQuotaConsumeOnceIsIdempotent(t *testing.T) {
	svc, rdb, up, plan := testPlanQuotaEnv(t)
	ctx := context.Background()
	key := uuid.NewString()
	t.Cleanup(func() {
		_ = rdb.Del(context.Background(), quotaOnceKey(up.ID, key)).Err()
	})

	view, consumed, err := svc.ConsumeQuotaOnce(ctx, up, plan, decimal.RequireFromString("7.5"), key)
	if err != nil {
		t.Fatalf("first consume: %v", err)
	}
	if !consumed {
		t.Fatal("first consume did not report ownership")
	}
	if !view.Used5h.Equal(decimal.RequireFromString("7.5")) {
		t.Fatalf("first used_5h = %s, want 7.5", view.Used5h)
	}

	view, consumed, err = svc.ConsumeQuotaOnce(ctx, up, plan, decimal.RequireFromString("99"), key)
	if err != nil {
		t.Fatalf("retry consume: %v", err)
	}
	if consumed {
		t.Fatal("retry consumed the quota again")
	}
	if !view.Used5h.Equal(decimal.RequireFromString("7.5")) ||
		!view.UsedWeekly.Equal(decimal.RequireFromString("7.5")) ||
		!view.UsedMonthly.Equal(decimal.RequireFromString("7.5")) {
		t.Fatalf("retry changed counters: %+v", view)
	}
}

// Keys written by the INCRBYFLOAT era must keep their accumulated value across
// the switch. Losing them would reset a live window and hand the user a fresh
// allowance mid-period.
func TestPlanQuotaMigratesLegacyFloatKeys(t *testing.T) {
	svc, rdb, up, plan := testPlanQuotaEnv(t)
	ctx := context.Background()
	keys := quotaKeys(up, time.Now())

	// Seed all three windows the way INCRBYFLOAT left them.
	for _, k := range keys {
		if err := rdb.Set(ctx, k, "12.5", time.Hour).Err(); err != nil {
			t.Fatalf("seed %s: %v", k, err)
		}
	}

	// A read before any write must still see the legacy value.
	peek, err := svc.PeekQuota(ctx, up)
	if err != nil {
		t.Fatalf("peek: %v", err)
	}
	if !peek.Used5h.Equal(decimal.RequireFromString("12.5")) {
		t.Errorf("legacy key read as %s, want 12.5", peek.Used5h)
	}

	view, err := svc.ConsumeQuota(ctx, up, plan, decimal.RequireFromString("0.5"))
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if !view.Used5h.Equal(decimal.RequireFromString("13")) {
		t.Errorf("used_5h = %s, want 13 (12.5 carried over + 0.5)", view.Used5h)
	}

	// The key must now be fixed-point, so subsequent INCRBYs do not error.
	raw, err := rdb.Get(ctx, keys[0]).Result()
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if raw != "1300000000" {
		t.Errorf("key holds %q, want the fixed-point form 1300000000", raw)
	}

	if _, err := svc.ConsumeQuota(ctx, up, plan, decimal.RequireFromString("0.25")); err != nil {
		t.Fatalf("consume after migration: %v", err)
	}
	peek, err = svc.PeekQuota(ctx, up)
	if err != nil {
		t.Fatalf("peek after migration: %v", err)
	}
	if !peek.Used5h.Equal(decimal.RequireFromString("13.25")) {
		t.Errorf("used_5h = %s, want 13.25", peek.Used5h)
	}
}

// Scientific notation is the shape INCRBYFLOAT produced for very small sums,
// and it must survive the rewrite rather than being read as zero.
func TestPlanQuotaMigratesScientificNotationKeys(t *testing.T) {
	svc, rdb, up, plan := testPlanQuotaEnv(t)
	ctx := context.Background()
	keys := quotaKeys(up, time.Now())

	for _, k := range keys {
		if err := rdb.Set(ctx, k, "1.5e-05", time.Hour).Err(); err != nil {
			t.Fatalf("seed %s: %v", k, err)
		}
	}
	view, err := svc.ConsumeQuota(ctx, up, plan, decimal.RequireFromString("0.00001"))
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if !view.Used5h.Equal(decimal.RequireFromString("0.000025")) {
		t.Errorf("used_5h = %s, want 0.000025", view.Used5h)
	}
}

// Windows are counted from the plan's start_at: a plan bought on the 28th
// gets a full week and a full 30-day month, not whatever is left of the
// calendar week or month.
func TestPlanQuotaWindowsCountFromStart(t *testing.T) {
	start := time.Date(2026, 9, 28, 15, 20, 0, 0, time.UTC)
	up := &ent.UserPlan{ID: uuid.New(), StartAt: start}

	cases := []struct {
		name   string
		now    time.Time
		keys   [3]string
		resets [3]time.Time
	}{
		{"at purchase", start,
			[3]string{"5h:0", "weekly:0", "monthly:0"},
			[3]time.Time{start.Add(5 * time.Hour), start.AddDate(0, 0, 7), start.AddDate(0, 0, 30)}},
		{"past the calendar month end", time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC),
			[3]string{"5h:16", "weekly:0", "monthly:0"},
			[3]time.Time{start.Add(85 * time.Hour), start.AddDate(0, 0, 7), start.AddDate(0, 0, 30)}},
		{"week boundary is inclusive", start.AddDate(0, 0, 7),
			[3]string{"5h:33", "weekly:1", "monthly:0"},
			[3]time.Time{start.Add(170 * time.Hour), start.AddDate(0, 0, 14), start.AddDate(0, 0, 30)}},
		{"second month", start.AddDate(0, 0, 31),
			[3]string{"5h:148", "weekly:4", "monthly:1"},
			[3]time.Time{start.Add(745 * time.Hour), start.AddDate(0, 0, 35), start.AddDate(0, 0, 60)}},
	}
	for _, c := range cases {
		w := QuotaWindowsAt(up, c.now)
		for i := range w {
			if want := "qta:" + up.ID.String() + ":" + c.keys[i]; w[i].Key != want {
				t.Errorf("%s: window %d key = %s, want %s", c.name, i, w[i].Key, want)
			}
			if !w[i].End.Equal(c.resets[i]) {
				t.Errorf("%s: window %d resets at %s, want %s", c.name, i, w[i].End, c.resets[i])
			}
			if c.now.Before(w[i].Start) || !c.now.Before(w[i].End) {
				t.Errorf("%s: window %d [%s, %s) does not contain now", c.name, i, w[i].Start, w[i].End)
			}
		}
	}
}
