// Package cache wraps the project's Redis client with a small typed
// GetOrCompute helper. It exists because every public/admin read-only
// endpoint that wants caching needs the same boilerplate:
//
//   - look up Redis
//   - on miss, call the underlying biz/svc method
//   - marshal/unmarshal JSON
//   - collapse concurrent misses for the same key (single-flight)
//   - fail open on Redis errors (the upstream system must keep working
//     when Redis hiccups)
//
// We cache *typed values*, not JSON HTTP responses. Handlers still
// project/shape the value into a gin.H{} on the way out — caching
// pre-projection means one cache key can serve multiple endpoints
// (e.g. /public/budget and the admin budget view share the same value
// with different field selections at the edge).
package cache

import (
	"context"
	"encoding/json"
	"time"

	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/singleflight"
)

// Cache wraps the Redis client + an in-process single-flight group so a
// stampede on a freshly-expired key only runs the underlying fn once per
// process.
type Cache struct {
	rdb *redis.Client
	g   singleflight.Group
}

func New(rdb *redis.Client) *Cache {
	return &Cache{rdb: rdb}
}

// opTimeout caps a single Redis call. Cache lookups are best-effort —
// when Redis is slow we'd rather miss and recompute than hold the request
// hostage.
const opTimeout = 200 * time.Millisecond

// GetOrCompute returns the cached value at key when present, otherwise
// calls fn and caches its result for ttl. The bool return is true on
// cache HIT, false on MISS (useful for the X-Cache response header).
//
// Failure modes:
//   - c == nil: skip cache entirely, just call fn (lets tests omit Redis)
//   - Redis GET error / unmarshal error: treat as miss, call fn, try to
//     write back; do NOT bubble the Redis error to the caller
//   - Redis SET error: ignored — fn already produced a usable value
//   - fn error: returned as-is, nothing cached
//
// Concurrency: the first goroutine through populates Redis. Other
// goroutines hitting the same key while fn runs join via singleflight
// and share the result (avoids stampede on cold/expired keys).
func GetOrCompute[T any](ctx context.Context, c *Cache, key string, ttl time.Duration, fn func(context.Context) (T, error)) (T, bool, error) {
	var zero T
	if c != nil {
		rctx, cancel := context.WithTimeout(ctx, opTimeout)
		raw, err := c.rdb.Get(rctx, key).Bytes()
		cancel()
		if err == nil {
			var v T
			if uerr := json.Unmarshal(raw, &v); uerr == nil {
				return v, true, nil
			}
			// Malformed cache entry: drop it so the next caller does a
			// clean reload.
			_ = c.rdb.Del(ctx, key).Err()
		}
		// Other Redis errors (network, timeout) fall through to fn(),
		// do not bubble up.
	}

	// Coalesce concurrent misses for the same key.
	if c == nil {
		v, err := fn(ctx)
		return v, false, err
	}
	v, err, _ := c.g.Do(key, func() (any, error) {
		out, ferr := fn(ctx)
		if ferr != nil {
			return out, ferr
		}
		if b, mErr := json.Marshal(out); mErr == nil {
			sctx, cancel := context.WithTimeout(ctx, opTimeout)
			_ = c.rdb.Set(sctx, key, b, ttl).Err()
			cancel()
		}
		return out, nil
	})
	if err != nil {
		return zero, false, err
	}
	return v.(T), false, nil
}

// Invalidate deletes one or more cache keys. Safe to call with a nil
// receiver and with no keys (no-op). Errors are swallowed: invalidation
// is best-effort; on Redis failure the TTL will eventually expire the
// stale entry anyway.
func (c *Cache) Invalidate(ctx context.Context, keys ...string) {
	if c == nil || len(keys) == 0 {
		return
	}
	rctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	_ = c.rdb.Del(rctx, keys...).Err()
}
