package middleware

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"

	"github.com/jucode/jucode/internal/data/ent"
)

// RateLimit returns a Gin middleware enforcing a fixed-window request
// count per (scope, key). Window is 60s by default.
//
// Two common compositions:
//   - IPRateLimit: scope="ip", key = client IP
//   - UserRateLimit: scope="user", key = user_id (requires JWT middleware to run first)
type RateLimitConfig struct {
	Scope          string // included in the Redis key namespace
	WindowSeconds  int    // size of the fixed window
	MaxRequests    int    // requests permitted per window
	KeyFromContext func(c *gin.Context) string
}

func RateLimit(rdb *redis.Client, cfg RateLimitConfig) gin.HandlerFunc {
	if cfg.WindowSeconds <= 0 {
		cfg.WindowSeconds = 60
	}
	return func(c *gin.Context) {
		key := cfg.KeyFromContext(c)
		if key == "" {
			c.Next()
			return
		}
		bucket := time.Now().Unix() / int64(cfg.WindowSeconds)
		redisKey := fmt.Sprintf("rl:%s:%s:%d", cfg.Scope, key, bucket)
		ctx, cancel := context.WithTimeout(c.Request.Context(), 200*time.Millisecond)
		defer cancel()

		count, err := rdb.Incr(ctx, redisKey).Result()
		if err != nil {
			// Fail open: rate-limiter is best-effort, never block real traffic
			// on Redis hiccups.
			c.Next()
			return
		}
		if count == 1 {
			_ = rdb.Expire(ctx, redisKey, time.Duration(cfg.WindowSeconds)*time.Second).Err()
		}
		c.Header("X-RateLimit-Limit", strconv.Itoa(cfg.MaxRequests))
		c.Header("X-RateLimit-Remaining", strconv.FormatInt(int64(cfg.MaxRequests)-count, 10))
		if count > int64(cfg.MaxRequests) {
			slog.Warn("rate_limit_exceeded",
				"scope", cfg.Scope,
				"key", key,
				"count", count,
				"max", cfg.MaxRequests,
				"ip", c.ClientIP(),
				"path", c.FullPath(),
				"req_id", c.GetString(CtxRequestID),
			)
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
				"error": "rate limit exceeded",
				"scope": cfg.Scope,
			})
			return
		}
		c.Next()
	}
}

// IPRateLimit limits by client IP. Suitable for public endpoints (auth).
// Uses c.ClientIP(), which honors X-Forwarded-For only from trusted
// proxies, so the key is the real client rather than the Caddy peer.
func IPRateLimit(rdb *redis.Client, max int, windowSec int) gin.HandlerFunc {
	return RateLimit(rdb, RateLimitConfig{
		Scope:         "ip",
		WindowSeconds: windowSec,
		MaxRequests:   max,
		KeyFromContext: func(c *gin.Context) string {
			return c.ClientIP()
		},
	})
}

// UserRateLimit limits by authenticated user — apikey OR jwt user. Skips
// (no limit) if neither is in context, so unauthenticated requests pass
// through to a downstream IPRateLimit if any.
func UserRateLimit(rdb *redis.Client, max int, windowSec int) gin.HandlerFunc {
	return UserScopedRateLimit(rdb, "user", max, windowSec)
}

// UserScopedRateLimit is UserRateLimit counting in its own scope, so one
// family of endpoints neither uses up nor is used up by the user's others.
func UserScopedRateLimit(rdb *redis.Client, scope string, max int, windowSec int) gin.HandlerFunc {
	return RateLimit(rdb, RateLimitConfig{
		Scope:          scope,
		WindowSeconds:  windowSec,
		MaxRequests:    max,
		KeyFromContext: userKey,
	})
}

// IPScopedRateLimit is IPRateLimit counting in its own scope.
func IPScopedRateLimit(rdb *redis.Client, scope string, max int, windowSec int) gin.HandlerFunc {
	return RateLimit(rdb, RateLimitConfig{
		Scope:         scope,
		WindowSeconds: windowSec,
		MaxRequests:   max,
		KeyFromContext: func(c *gin.Context) string {
			return c.ClientIP()
		},
	})
}

func userKey(c *gin.Context) string {
	if u, ok := UserFrom(c); ok && u != nil {
		return u.ID.String()
	}
	if id, ok := UserIDFrom(c); ok {
		return id.String()
	}
	return ""
}

// BodyLimit caps a request body at max bytes; reading past it fails, so a
// JSON bind answers 400 instead of buffering an oversized upload.
func BodyLimit(max int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, max)
		c.Next()
	}
}

// Silence unused-import warning for ent.User indirect references.
var _ = ent.User{}
