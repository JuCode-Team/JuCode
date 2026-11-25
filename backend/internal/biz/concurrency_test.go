package biz

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/jucode/jucode/internal/data"
	"github.com/jucode/jucode/internal/data/ent"
)

// The concurrency slot counters are Lua, so their semantics cannot be checked
// by compiling — they need a real Redis. Point JUCODE_TEST_REDIS_ADDR at a
// throwaway instance to run these:
//
//	docker run -d --rm -p 6399:6379 --name jucode-redis-test redis:7-alpine
//	JUCODE_TEST_REDIS_ADDR=127.0.0.1:6399 go test ./internal/biz -run Concurrency
func testGate(t *testing.T) (*ConcurrencyGate, *redis.Client) {
	t.Helper()
	addr := os.Getenv("JUCODE_TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("JUCODE_TEST_REDIS_ADDR not set; skipping Redis-backed concurrency tests")
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("redis ping: %v", err)
	}
	// settings is nil on purpose: counterTTL then falls back to concurTTLMax,
	// which is the branch that matters for "TTL must outlive the request".
	return NewConcurrencyGate(&data.Data{Redis: rdb}, nil, nil), rdb
}

// overrideUser exercises the ConcurrencyLimitOverride branch, the only one
// that needs no Ent client.
func overrideUser(limit int) *ent.User {
	return &ent.User{ID: uuid.New(), ConcurrencyLimitOverride: limit}
}

// anyGroup is a non-nil group; its BillingSource is never consulted because
// ConcurrencyLimitOverride short-circuits ahead of the billing-source switch.
func anyGroup() *ent.ModelGroup { return &ent.ModelGroup{ID: uuid.New()} }

func TestConcurrencyAcquireRejectsAtCap(t *testing.T) {
	g, rdb := testGate(t)
	ctx := context.Background()
	u := overrideUser(2)
	key := "concur:u:" + u.ID.String() + ":ovr"
	t.Cleanup(func() { rdb.Del(ctx, key) })

	r1, err := g.AcquireWithPlanState(ctx, u, anyGroup(), nil)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	r2, err := g.AcquireWithPlanState(ctx, u, anyGroup(), nil)
	if err != nil {
		t.Fatalf("second acquire: %v", err)
	}

	if _, err := g.AcquireWithPlanState(ctx, u, anyGroup(), nil); err != ErrConcurrencyLimit {
		t.Fatalf("third acquire: want ErrConcurrencyLimit, got %v", err)
	}
	// The rejected attempt must not leave the counter inflated.
	if n, _ := rdb.Get(ctx, key).Int64(); n != 2 {
		t.Fatalf("counter after rejection: want 2, got %d", n)
	}

	r1()
	r2()
	// Last release drops the key rather than parking a 0 with a long TTL.
	if exists, _ := rdb.Exists(ctx, key).Result(); exists != 0 {
		n, _ := rdb.Get(ctx, key).Int64()
		t.Fatalf("counter after all releases: want key gone, got %d", n)
	}
}

// This is the regression guard for the bug this change fixes: a request whose
// counter key expired mid-flight used to DECR a missing key, and Redis would
// create it at -1 — permanently granting that user an extra slot.
func TestConcurrencyReleaseOnExpiredKeyDoesNotGoNegative(t *testing.T) {
	g, rdb := testGate(t)
	ctx := context.Background()
	u := overrideUser(1)
	key := "concur:u:" + u.ID.String() + ":ovr"
	t.Cleanup(func() { rdb.Del(ctx, key) })

	release, err := g.AcquireWithPlanState(ctx, u, anyGroup(), nil)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	// Simulate the counter TTL lapsing while the request is still in flight.
	rdb.Del(ctx, key)
	release()

	if exists, _ := rdb.Exists(ctx, key).Result(); exists != 0 {
		n, _ := rdb.Get(ctx, key).Int64()
		t.Fatalf("release on expired key resurrected the counter at %d; want no key", n)
	}
}

// A counter already damaged by the old code must heal instead of carrying the
// deficit forever.
func TestConcurrencyAcquireHealsNegativeCounter(t *testing.T) {
	g, rdb := testGate(t)
	ctx := context.Background()
	u := overrideUser(1)
	key := "concur:u:" + u.ID.String() + ":ovr"
	t.Cleanup(func() { rdb.Del(ctx, key) })

	rdb.Set(ctx, key, -3, time.Hour) // legacy damage

	if _, err := g.AcquireWithPlanState(ctx, u, anyGroup(), nil); err != nil {
		t.Fatalf("acquire on damaged counter: %v", err)
	}
	if n, _ := rdb.Get(ctx, key).Int64(); n != 1 {
		t.Fatalf("counter after heal: want 1, got %d", n)
	}
	// With the deficit gone, the cap of 1 must bite again immediately.
	if _, err := g.AcquireWithPlanState(ctx, u, anyGroup(), nil); err != ErrConcurrencyLimit {
		t.Fatalf("acquire past healed cap: want ErrConcurrencyLimit, got %v", err)
	}
}

// The TTL must outlive the longest request the gateway itself permits,
// otherwise the counter vanishes underneath a live stream.
func TestConcurrencyCounterTTLOutlivesStreamCap(t *testing.T) {
	g, rdb := testGate(t)
	ctx := context.Background()
	u := overrideUser(1)
	key := "concur:u:" + u.ID.String() + ":ovr"
	t.Cleanup(func() { rdb.Del(ctx, key) })

	release, err := g.AcquireWithPlanState(ctx, u, anyGroup(), nil)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer release()

	ttl, err := rdb.TTL(ctx, key).Result()
	if err != nil {
		t.Fatalf("ttl: %v", err)
	}
	if ttl <= 5*time.Minute {
		t.Fatalf("counter TTL %v is not longer than the old flat 5m; a long stream can still outlive it", ttl)
	}
}
