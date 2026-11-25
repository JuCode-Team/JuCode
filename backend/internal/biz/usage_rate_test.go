package biz

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/jucode/jucode/internal/data"
)

// The rate counters are Redis state and the estimate blends two buckets, so
// this needs a real Redis:
//
//	docker run -d --rm -p 6399:6379 --name jucode-redis-test redis:7-alpine
//	JUCODE_TEST_REDIS_ADDR=127.0.0.1:6399 go test ./internal/biz -run UsageRate
func testRateStats(t *testing.T) (*BillingSvc, *StatsSvc, *redis.Client) {
	t.Helper()
	addr := os.Getenv("JUCODE_TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("JUCODE_TEST_REDIS_ADDR not set; skipping Redis-backed rate tests")
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("redis ping: %v", err)
	}
	t.Cleanup(func() { _ = rdb.Close() })
	d := data.NewWith(nil, rdb, nil)
	return &BillingSvc{d: d}, NewStatsSvc(d), rdb
}

// seedRate writes counters directly into a chosen minute bucket, which is the
// only way to exercise the previous-minute half of the estimate without
// waiting out a real minute boundary.
func seedRate(t *testing.T, rdb *redis.Client, scope string, minute int64, reqs, tokens int64) {
	t.Helper()
	ctx := context.Background()
	key := usageRateKey(scope, minute)
	if err := rdb.HIncrBy(ctx, key, fieldRateRequests, reqs).Err(); err != nil {
		t.Fatalf("seed reqs: %v", err)
	}
	if err := rdb.HIncrBy(ctx, key, fieldRateTokens, tokens).Err(); err != nil {
		t.Fatalf("seed tokens: %v", err)
	}
	rdb.Expire(ctx, key, usageRateTTL)
	t.Cleanup(func() { rdb.Del(context.Background(), key) })
}

// Counters must be written for every row, not only for users who happen to
// have caps configured — the usage screen shows a rate for everyone.
func TestUsageRateCountsEveryRequest(t *testing.T) {
	billing, stats, rdb := testRateStats(t)
	ctx := context.Background()
	userID := uuid.New()
	t.Cleanup(func() {
		rdb.Del(ctx, usageRateKey(usageScopeUser(userID), usageRateMinute(time.Now())))
	})

	const n = 7
	for i := 0; i < n; i++ {
		in := sampleUsageInput(userID, uuid.NewString(), nil)
		in.TokensIn, in.TokensOut = 10, 5
		in.CachedTokensIn, in.CacheCreationTokensIn = 20, 30
		billing.incrUsageBucketsAsync(ctx, in)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r, err := stats.UserRate(ctx, userID)
		if err != nil {
			t.Fatalf("user rate: %v", err)
		}
		if r.RPM >= n {
			if r.TPM != n*5 {
				t.Fatalf("TPM: got %d, want %d output tokens only", r.TPM, n*5)
			}
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("counters never reflected the writes")
}

func TestUsageRateTokensCountsOnlyOutput(t *testing.T) {
	in := WriteUsageLogInput{
		TokensIn:              10,
		TokensOut:             5,
		CachedTokensIn:        20,
		CacheCreationTokensIn: 30,
	}
	if got := usageRateTokens(in); got != 5 {
		t.Fatalf("TPM tokens = %d, want 5 output tokens only", got)
	}
}

// Unlike the personal spend chart, a user's rate must include org-billed rows:
// the per-user caps count them, so a rate that excluded them would disagree
// with the limit displayed beside it.
func TestUsageRateIncludesOrgBilledRows(t *testing.T) {
	billing, stats, rdb := testRateStats(t)
	ctx := context.Background()
	userID := uuid.New()
	orgID := uuid.New()
	t.Cleanup(func() {
		rdb.Del(ctx, usageRateKey(usageScopeUser(userID), usageRateMinute(time.Now())))
	})

	in := sampleUsageInput(userID, uuid.NewString(), nil)
	in.OrganizationID = &orgID
	billing.incrUsageBucketsAsync(ctx, in)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if r, _ := stats.UserRate(ctx, userID); r.RPM >= 1 {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("org-billed row did not count toward the user's rate")
}

// The estimate blends the previous minute by how much of it is still inside
// the trailing 60s. Reading only the current bucket would make a busy account
// display 0 RPM for a moment after every minute boundary.
func TestUsageRateBlendsPreviousMinute(t *testing.T) {
	_, stats, rdb := testRateStats(t)
	ctx := context.Background()
	scope := usageScopeUser(uuid.New())
	now := time.Now().UTC()
	minute := usageRateMinute(now)

	// Nothing this minute, 600 requests last minute.
	seedRate(t, rdb, scope, minute-1, 600, 6000)

	r, err := stats.rateForScope(ctx, scope)
	if err != nil {
		t.Fatalf("rate: %v", err)
	}
	elapsed := float64(now.Unix()%60) / 60.0
	want := int64(600*(1-elapsed) + 0.5)
	// One unit of slack: the handler re-reads the clock after the seed.
	if diff := r.RPM - want; diff > 11 || diff < -11 {
		t.Fatalf("RPM = %d, want ~%d (elapsed %.2f of the minute)", r.RPM, want, elapsed)
	}
	if r.RPM == 0 {
		t.Fatal("previous minute ignored entirely; the display would sawtooth to zero every 60s")
	}
}

func TestUsageRateEmptyScopeIsZeroNotError(t *testing.T) {
	_, stats, _ := testRateStats(t)
	r, err := stats.rateForScope(context.Background(), usageScopeUser(uuid.New()))
	if err != nil {
		t.Fatalf("unexpected error for an idle scope: %v", err)
	}
	if r.RPM != 0 || r.TPM != 0 {
		t.Fatalf("idle scope reported %+v", r)
	}
}
