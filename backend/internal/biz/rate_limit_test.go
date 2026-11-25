package biz

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/jucode/jucode/internal/data"
	"github.com/jucode/jucode/internal/data/ent"
	entgroup "github.com/jucode/jucode/internal/data/ent/modelgroup"
)

// The counters are Redis state and the phase semantics only exist in how the
// three calls interleave, so these need a real Redis:
//
//	docker run -d --rm -p 6399:6379 --name jucode-redis-test redis:7-alpine
//	JUCODE_TEST_REDIS_ADDR=127.0.0.1:6399 go test ./internal/biz -run RateLimit
func testRateLimitGate(t *testing.T, mode RateLimitMode) *RateLimitGate {
	t.Helper()
	addr := os.Getenv("JUCODE_TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("JUCODE_TEST_REDIS_ADDR not set; skipping Redis-backed rate limit tests")
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("redis ping: %v", err)
	}
	t.Cleanup(func() { _ = rdb.Close() })
	g := NewRateLimitGate(data.NewWith(nil, rdb, nil), nil, mode,
		slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
	// Pin the window so a test cannot straddle a 60s boundary and flake.
	fixed := time.Now()
	g.nowFn = func() time.Time { return fixed }
	return g
}

// rateLimitUser exercises the override branch, the only one needing no DB.
func rateLimitUser(rpm, tpm int) *ent.User {
	return &ent.User{ID: uuid.New(), RpmLimitOverride: rpm, TpmLimitOverride: tpm}
}

func rateLimitGroup() *ent.ModelGroup {
	return &ent.ModelGroup{ID: uuid.New(), BillingSource: entgroup.BillingSourceBalanceOnly}
}

func TestRateLimitRPMRejectsOnlyAfterCommits(t *testing.T) {
	g := testRateLimitGate(t, RateLimitEnforce)
	ctx := context.Background()
	u := rateLimitUser(2, 0)
	grp := rateLimitGroup()

	// Peeking alone consumes nothing: without commits the cap never trips,
	// which is what keeps upstream failures free.
	for i := 0; i < 5; i++ {
		if _, err := g.Peek(ctx, u, grp, nil); err != nil {
			t.Fatalf("peek %d rejected before any commit: %v", i, err)
		}
	}

	t1, err := g.Peek(ctx, u, grp, nil)
	if err != nil {
		t.Fatalf("peek 1: %v", err)
	}
	t1.Commit(ctx)
	t2, err := g.Peek(ctx, u, grp, nil)
	if err != nil {
		t.Fatalf("peek 2: %v", err)
	}
	t2.Commit(ctx)

	// Two committed, cap is two: the third must be refused.
	if _, err := g.Peek(ctx, u, grp, nil); err != ErrRPMLimit {
		t.Fatalf("third peek: got %v, want ErrRPMLimit", err)
	}
}

// TPM is checked against history, not forecast: the request that pushes the
// window over the cap is allowed to finish (its token count was unknowable at
// peek time) and only the next one is refused.
func TestRateLimitTPMIsHistoricalNotForwardLooking(t *testing.T) {
	g := testRateLimitGate(t, RateLimitEnforce)
	ctx := context.Background()
	u := rateLimitUser(0, 1000)
	grp := rateLimitGroup()

	ticket, err := g.Peek(ctx, u, grp, nil)
	if err != nil {
		t.Fatalf("first peek: %v", err)
	}
	// One request blows way past the cap.
	ticket.Settle(ctx, 5000)

	if _, err := g.Peek(ctx, u, grp, nil); err != ErrTPMLimit {
		t.Fatalf("peek after overshoot: got %v, want ErrTPMLimit", err)
	}
}

// Shadow mode must maintain the counters (so limits can be tuned against real
// traffic) while never returning a rejection.
func TestRateLimitShadowModeNeverRejects(t *testing.T) {
	g := testRateLimitGate(t, RateLimitShadow)
	ctx := context.Background()
	u := rateLimitUser(1, 0)
	grp := rateLimitGroup()

	ticket, err := g.Peek(ctx, u, grp, nil)
	if err != nil {
		t.Fatalf("peek: %v", err)
	}
	ticket.Commit(ctx)

	// Enforce mode would refuse this one.
	if _, err := g.Peek(ctx, u, grp, nil); err != nil {
		t.Fatalf("shadow mode rejected: %v", err)
	}

	// ...and the counter really was maintained, which is the point of shadow.
	enforcing := testRateLimitGate(t, RateLimitEnforce)
	enforcing.nowFn = g.nowFn
	if _, err := enforcing.Peek(ctx, u, grp, nil); err != ErrRPMLimit {
		t.Fatalf("shadow mode did not maintain counters: enforce peek gave %v", err)
	}
}

func TestRateLimitOffModeIsInert(t *testing.T) {
	g := testRateLimitGate(t, RateLimitOff)
	ctx := context.Background()
	u := rateLimitUser(1, 1)
	grp := rateLimitGroup()

	ticket, err := g.Peek(ctx, u, grp, nil)
	if err != nil || ticket != nil {
		t.Fatalf("off mode: ticket=%v err=%v; want nil/nil", ticket, err)
	}
	// Nil-safety: the gateway calls these unconditionally.
	ticket.Commit(ctx)
	ticket.Settle(ctx, 100)
}

// Zero means unlimited, matching concurrency_limit: a scope with both caps at
// zero must never reject and must consume nothing.
func TestRateLimitZeroMeansUnlimited(t *testing.T) {
	if (rateLimitScope{}).active() {
		t.Fatal("an all-zero scope reports active; every unconfigured user would be rate limited")
	}
	if !(rateLimitScope{rpmLimit: 1}).active() || !(rateLimitScope{tpmLimit: 1}).active() {
		t.Fatal("a scope with one cap set must be active")
	}

	g := testRateLimitGate(t, RateLimitEnforce)
	ctx := context.Background()
	// An override user with both caps zero does not take the override branch,
	// so this exercises the "no applicable scope" path end to end.
	u := rateLimitUser(0, 0)
	grp := &ent.ModelGroup{ID: uuid.New()} // no billing source -> no scope

	for i := 0; i < 50; i++ {
		ticket, err := g.Peek(ctx, u, grp, nil)
		if err != nil {
			t.Fatalf("peek %d: %v", i, err)
		}
		ticket.Commit(ctx)
		ticket.Settle(ctx, 10_000)
	}
}

// The gate is wired through a setter and may be built without a database.
// It must degrade to "no caps" rather than panic on the request path.
func TestRateLimitWithoutDatabaseDegradesInsteadOfPanicking(t *testing.T) {
	g := testRateLimitGate(t, RateLimitEnforce)
	ctx := context.Background()
	u := &ent.User{ID: uuid.New()} // no override -> needs the DB
	grp := rateLimitGroup()        // balance_only -> user_group lookup

	ticket, err := g.Peek(ctx, u, grp, nil)
	if err != nil {
		t.Fatalf("peek: %v", err)
	}
	ticket.Commit(ctx)
	ticket.Settle(ctx, 100)
}

// Counters are keyed by tier so that moving a user between tiers cannot carry
// the previous tier's usage across.
func TestRateLimitCountersAreScopedPerTier(t *testing.T) {
	g := testRateLimitGate(t, RateLimitEnforce)
	ctx := context.Background()
	u := rateLimitUser(1, 0)
	grp := rateLimitGroup()

	ticket, err := g.Peek(ctx, u, grp, nil)
	if err != nil {
		t.Fatalf("peek: %v", err)
	}
	ticket.Commit(ctx)
	if _, err := g.Peek(ctx, u, grp, nil); err != ErrRPMLimit {
		t.Fatalf("override tier should be exhausted: %v", err)
	}

	// Same user, no override any more: a different tier, hence a different
	// counter, hence not exhausted. (Limit comes from the user_group here, so
	// resolveScope needs a DB; assert on the key prefix instead.)
	scope, ok := g.resolveScope(ctx, u, grp, nil)
	if !ok {
		t.Fatal("override scope did not resolve")
	}
	if scope.prefix != "u:"+u.ID.String()+":ovr" {
		t.Fatalf("override prefix = %q; a tier change would reuse the wrong counter", scope.prefix)
	}
}
