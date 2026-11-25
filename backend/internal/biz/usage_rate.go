package biz

import (
	"context"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// Live RPM / TPM observation counters.
//
// These are deliberately independent of RateLimitGate. The gate only maintains
// counters for users who actually have caps configured (and only when
// llm.rate_limit_mode is not "off"), whereas the usage screens have to show a
// rate for everyone, all the time. So the observation counters are written
// unconditionally on every persisted row, in the same pipeline as the hour
// buckets.
//
// They also count something slightly different from the hour buckets, which is
// why they get their own key namespace rather than another field on the same
// hash:
//
//	ustat:*  per-user scope EXCLUDES organization-billed rows, because the
//	         personal spend chart filters on `organization_id IS NULL`.
//	urate:*  per-user scope INCLUDES them, because RateLimitGate counts every
//	         request a user makes regardless of who pays. A displayed rate that
//	         disagreed with the cap that will eventually reject you would be
//	         worse than no display at all.
const (
	// Two minutes of buckets are read for the sliding estimate; keep one
	// spare so the older one cannot expire mid-read.
	usageRateTTL = 3 * time.Minute

	fieldRateRequests = "req"
	fieldRateTokens   = "tok"
)

func usageRateKey(scope string, minute int64) string {
	return "urate:" + scope + ":" + strconv.FormatInt(minute, 10)
}

func usageRateMinute(t time.Time) int64 { return t.UTC().Unix() / 60 }

// usageRateScopesFor lists the rate scopes a row contributes to: the platform
// total (admin view) and the acting user (their own view).
func usageRateScopesFor(in WriteUsageLogInput) []string {
	scopes := []string{usageScopeAll}
	if in.UserID != uuid.Nil {
		scopes = append(scopes, usageScopeUser(in.UserID))
	}
	return scopes
}

// addUsageRateToPipeline queues the counter bumps. Shares the caller's
// pipeline so observing a rate costs no extra round trip.
func addUsageRateToPipeline(ctx context.Context, pipe redis.Pipeliner, in WriteUsageLogInput, now time.Time) {
	minute := usageRateMinute(now)
	// TPM is output throughput. Prompt input (including cache reads and cache
	// writes) does not consume the TPM budget and must not inflate the live
	// rate shown beside that budget.
	tokens := usageRateTokens(in)
	for _, scope := range usageRateScopesFor(in) {
		key := usageRateKey(scope, minute)
		pipe.HIncrBy(ctx, key, fieldRateRequests, 1)
		if tokens > 0 {
			pipe.HIncrBy(ctx, key, fieldRateTokens, tokens)
		}
		pipe.Expire(ctx, key, usageRateTTL)
	}
}

// usageRateTokens must match what RateLimitTicket.Settle charges the TPM cap,
// or the displayed rate stops predicting the rejection. Audio output counts on
// both sides: it is output the model produced, just in another modality, and it
// sits outside TokensOut since the audio pricing split.
func usageRateTokens(in WriteUsageLogInput) int64 {
	return int64(in.TokensOut) + int64(in.AudioTokensOut)
}

// UsageRate is a point-in-time view of throughput over the trailing 60s.
type UsageRate struct {
	RPM int64 `json:"rpm"`
	TPM int64 `json:"tpm"`
	// Limits currently in effect for this scope. 0 = unlimited. Always 0 for
	// the platform-wide view, which has no cap of its own.
	RPMLimit int `json:"rpm_limit"`
	TPMLimit int `json:"tpm_limit"`
}

// PlatformRate returns the whole platform's current throughput.
func (s *StatsSvc) PlatformRate(ctx context.Context) (UsageRate, error) {
	return s.rateForScope(ctx, usageScopeAll)
}

// UserRate returns one user's current throughput, counting every request they
// made — including organization-billed ones, matching what the caps count.
func (s *StatsSvc) UserRate(ctx context.Context, userID uuid.UUID) (UsageRate, error) {
	return s.rateForScope(ctx, usageScopeUser(userID))
}

// rateForScope estimates a trailing-60s rate from two one-minute buckets.
//
// Reporting the current bucket alone would sawtooth: it resets to zero every
// minute, so a busy account would read 0 RPM right after the boundary. Instead
// the previous bucket is blended in by how much of the current minute is left,
// which is the standard sliding-window approximation and moves smoothly as the
// page polls.
func (s *StatsSvc) rateForScope(ctx context.Context, scope string) (UsageRate, error) {
	if s.d == nil || s.d.Redis == nil {
		return UsageRate{}, nil
	}
	now := time.Now().UTC()
	minute := usageRateMinute(now)

	pipe := s.d.Redis.Pipeline()
	cur := pipe.HGetAll(ctx, usageRateKey(scope, minute))
	prev := pipe.HGetAll(ctx, usageRateKey(scope, minute-1))
	if _, err := pipe.Exec(ctx); err != nil {
		return UsageRate{}, err
	}

	curMap, _ := cur.Result()
	prevMap, _ := prev.Result()

	// Fraction of the previous minute still inside the trailing 60s window.
	elapsed := float64(now.Unix()%60) / 60.0
	carry := 1.0 - elapsed

	blend := func(field string) int64 {
		c := float64(bucketInt(curMap, field))
		p := float64(bucketInt(prevMap, field))
		v := c + p*carry
		if v < 0 {
			return 0
		}
		return int64(v + 0.5)
	}
	return UsageRate{RPM: blend(fieldRateRequests), TPM: blend(fieldRateTokens)}, nil
}
