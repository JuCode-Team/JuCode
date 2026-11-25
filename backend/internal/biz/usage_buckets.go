package biz

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	entusagelog "github.com/jucode/jucode/internal/data/ent/usagelog"
)

// Rolling hour-bucket counters for the dashboard's hot query.
//
// The admin overview and the user usage panel both poll a 24h/hour time series
// every 2 seconds. Served from Postgres that is a full scan of 24h of
// usage_logs per cache miss, on the largest table in the database, with no
// (model|status|provider, created_at) index to help it. Served from these
// counters it is one pipelined round trip of ~25 HGETALLs.
//
// Granularity is one hour, not one minute, because that is exactly the
// resolution the API already exposes (StatsFilter.Bucket is "hour" | "day").
// Minute buckets would be 1440 keys to read back for the same answer.
//
// This mirrors the pattern provider_health.go established for error rates —
// the comment there records that it replaced a per-request full table scan,
// which is the same trade being made here.
const (
	// 24 full hours + the current partial hour + one hour of slack so the
	// oldest bucket cannot expire out from under a read that is in flight.
	usageBucketCount = 26
	usageBucketTTL   = time.Duration(usageBucketCount+1) * time.Hour
)

// Hash fields. Short names because they repeat once per bucket per axis.
const (
	fieldRequests      = "req"
	fieldErrors        = "err"
	fieldTokensIn      = "tin"
	fieldTokensOut     = "tout"
	fieldCachedIn      = "tcin"
	fieldCacheCreateIn = "tccin"
	// Audio rides its own counters for the same reason the cache ones do:
	// tin/tout are the text-only remainder since the audio pricing split, so a
	// series that omitted these would under-report a voice workload by however
	// much of it was audio — which is nearly all of it.
	fieldAudioIn   = "tain"
	fieldAudioOut  = "taout"
	fieldCostFixed = "cost"
)

// Cost is accumulated with moneyToFixed/moneyFromFixed (currency.go) — scaled
// int64 added via HINCRBY, at the ledger's own numeric(20,8) precision.
//
// Until 2026-08 this file quantized nothing and instead refused any cost
// carrying more than 8 decimals, dropping the increment whole. That was a
// systematic under-count of every cost series, always in the same direction:
// CalcCost divides by 1000 per component, so a 6-decimal unit price reaches 9
// decimals before the group rate multiplier adds its own. Measured against the
// SQL sum it was losing 3-6% of spend per hour in production.

// usageBucketHour is the UTC hour index a timestamp falls in.
func usageBucketHour(t time.Time) int64 { return t.UTC().Unix() / 3600 }

// usageBucketKey builds the counter key. scope is "all" for the unfiltered
// admin series, or "u:<user_id>" for one user's personal series.
func usageBucketKey(scope string, hour int64) string {
	return fmt.Sprintf("ustat:%s:%d", scope, hour)
}

const usageScopeAll = "all"

// usageBucketSinceKey records the first hour the writer ever populated.
//
// Without it the ring cannot answer "was there no traffic that hour, or was I
// not running yet?" — both look like a missing key. A window reaching back
// before the ring warmed up would silently render a chart with its first hours
// chopped off, under-reporting totals on a billing-adjacent dashboard. Reads
// therefore refuse any window starting before this mark and fall back to SQL.
//
// Deliberately has no TTL: it is one key, and it must outlive the buckets it
// guards. If Redis is flushed it simply re-arms at the next write, which is the
// correct behavior — the ring really did start over.
const usageBucketSinceKey = "ustat:since"

func usageScopeUser(userID uuid.UUID) string { return "u:" + userID.String() }

// incrUsageBucketsAsync bumps the counters for one persisted row.
//
// Called off the request path, after the row is known to be going to the DB.
// Redis failures are swallowed: these counters are an accelerator, and every
// read path can fall back to the table they are derived from.
func (s *BillingSvc) incrUsageBucketsAsync(ctx context.Context, in WriteUsageLogInput) {
	if s.d == nil || s.d.Redis == nil {
		return
	}
	scopes := usageScopesFor(in)
	// Bucketed by wall-clock now, matching the SQL path which groups on
	// created_at (stamped by ent at insert). Using StartedAt would drift from
	// it by the whole duration of a long stream.
	hour := usageBucketHour(time.Now())
	costFixed, costOK := moneyToFixed(in.CostFinal)

	go func() {
		bumpCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()

		pipe := s.d.Redis.Pipeline()
		for _, scope := range scopes {
			key := usageBucketKey(scope, hour)
			pipe.HIncrBy(bumpCtx, key, fieldRequests, 1)
			if in.Status != entusagelog.StatusOk {
				pipe.HIncrBy(bumpCtx, key, fieldErrors, 1)
			}
			pipe.HIncrBy(bumpCtx, key, fieldTokensIn, int64(in.TokensIn))
			pipe.HIncrBy(bumpCtx, key, fieldTokensOut, int64(in.TokensOut))
			pipe.HIncrBy(bumpCtx, key, fieldCachedIn, int64(in.CachedTokensIn))
			pipe.HIncrBy(bumpCtx, key, fieldCacheCreateIn, int64(in.CacheCreationTokensIn))
			pipe.HIncrBy(bumpCtx, key, fieldAudioIn, int64(in.AudioTokensIn))
			pipe.HIncrBy(bumpCtx, key, fieldAudioOut, int64(in.AudioTokensOut))
			if costOK && costFixed != 0 {
				pipe.HIncrBy(bumpCtx, key, fieldCostFixed, costFixed)
			}
			// Refreshed on every write rather than only on creation: an
			// INCR that lands while the EXPIRE is lost would otherwise
			// leave a key with no TTL at all.
			pipe.Expire(bumpCtx, key, usageBucketTTL)
		}
		// SETNX: only the very first write wins, so this marks when the ring
		// began rather than when it was last touched.
		//
		// hour+1, not hour: the ring almost never starts on an hour boundary,
		// so the bucket for `hour` is missing whatever traffic happened before
		// the process came up. The first hour it can answer completely is the
		// next one, and reporting a partial hour as complete is exactly the
		// silent under-count this marker exists to prevent.
		pipe.SetNX(bumpCtx, usageBucketSinceKey, hour+1, 0)
		// Live RPM/TPM counters ride along in the same pipeline — see
		// usage_rate.go for why they are a separate namespace.
		addUsageRateToPipeline(bumpCtx, pipe, in, time.Now())
		_, _ = pipe.Exec(bumpCtx)
	}()
}

// redisMapCmd pairs a pipelined HGETALL with the hour it belongs to, so the
// results can be reassembled in order after Exec.
type redisMapCmd struct {
	hour int64
	cmd  *redis.MapStringStringCmd
}

// usageScopesFor lists the counter scopes a row contributes to.
//
// The personal scope deliberately excludes organization-billed rows: the
// personal chart filters on `organization_id IS NULL` (StatsFilter.PersonalOnly)
// because org spend never touched the personal balance. Mirroring that
// exclusion here is what lets the two paths agree.
func usageScopesFor(in WriteUsageLogInput) []string {
	scopes := []string{usageScopeAll}
	if in.OrganizationID == nil && in.UserID != uuid.Nil {
		scopes = append(scopes, usageScopeUser(in.UserID))
	}
	return scopes
}

// bucketSeriesScope decides whether a filter can be answered from the
// counters, and which scope holds the answer.
//
// Deliberately narrow. Any filter the counters do not shard by (model, status,
// group, organization) falls back to SQL rather than being approximated.
func bucketSeriesScope(f StatsFilter) (string, bool) {
	if f.Model != "" || f.Status != "" || f.GroupID != nil || f.OrganizationID != nil {
		return "", false
	}
	switch {
	case f.UserID == nil && !f.PersonalOnly:
		return usageScopeAll, true
	case f.UserID != nil && f.PersonalOnly:
		return usageScopeUser(*f.UserID), true
	default:
		// user-without-PersonalOnly and org-scoped views have no counter.
		return "", false
	}
}

// bucketSeriesEligible reports whether [Start, End) can be reconstructed from
// the ring exactly — not approximately.
//
// Three conditions, each of which would otherwise silently change the answer:
//
//   - hourly buckets only; "day" is a different grouping entirely.
//   - Start must land on an hour boundary. The SQL path filters rows by
//     `created_at >= Start` and only then groups them into hours, so a Start
//     of 10:30 yields a 10:00 bucket holding just the second half of the hour.
//     The ring cannot express that, it holds the whole hour.
//   - End must be ~now. The ring only reaches back usageBucketCount hours, and
//     a historical window would read mostly-expired keys and report zeros.
func bucketSeriesEligible(f StatsFilter, now time.Time) bool {
	if f.Bucket != "hour" {
		return false
	}
	if !f.Start.UTC().Truncate(time.Hour).Equal(f.Start.UTC()) {
		return false
	}
	if f.End.Before(now.Add(-2 * time.Minute)) {
		return false
	}
	if !f.End.After(f.Start) {
		return false
	}
	// Bound by bucket COUNT, not by elapsed duration. The dashboard's 24h
	// range starts at the top of the hour 24h ago, so it spans 25 hour
	// buckets (the first partial one through the current partial one) and
	// `now - Start` is somewhere between 24h and 25h. A 24h duration limit
	// here would reject the exact query this whole path exists to serve.
	// One bucket is left as slack so a read never lands on the edge of the
	// oldest key's TTL.
	if usageBucketSpan(f) > usageBucketCount-1 {
		return false
	}
	return true
}

// usageBucketSpan is how many hour buckets [Start, End] touches, inclusive.
func usageBucketSpan(f StatsFilter) int {
	return int(usageBucketHour(f.End)-usageBucketHour(f.Start)) + 1
}

// aggregateFromBuckets reads the hour series for [Start, End). The bool
// reports whether the counters could serve this filter at all.
func (s *StatsSvc) aggregateFromBuckets(ctx context.Context, f StatsFilter) ([]StatsBucket, bool, error) {
	if s.d == nil || s.d.Redis == nil {
		return nil, false, nil
	}
	now := time.Now()
	if !bucketSeriesEligible(f, now) {
		return nil, false, nil
	}
	scope, ok := bucketSeriesScope(f)
	if !ok {
		return nil, false, nil
	}

	startHour := usageBucketHour(f.Start)
	endHour := usageBucketHour(f.End)
	n := usageBucketSpan(f) // bounded by bucketSeriesEligible

	// Refuse windows that reach back before the ring started collecting.
	// Serving them would silently drop the leading hours, which reads as a
	// dip in traffic rather than as missing data.
	since, err := s.d.Redis.Get(ctx, usageBucketSinceKey).Int64()
	if err != nil || startHour < since {
		return nil, false, nil
	}

	pipe := s.d.Redis.Pipeline()
	cmds := make([]*redisMapCmd, 0, n)
	for h := startHour; h <= endHour; h++ {
		cmds = append(cmds, &redisMapCmd{hour: h, cmd: pipe.HGetAll(ctx, usageBucketKey(scope, h))})
	}
	if _, err := pipe.Exec(ctx); err != nil {
		// redis.Nil is not returned by HGETALL (missing key is an empty map),
		// so anything here is a real failure — let the caller use SQL.
		return nil, false, err
	}

	out := make([]StatsBucket, 0, n)
	for _, c := range cmds {
		m, err := c.cmd.Result()
		if err != nil || len(m) == 0 {
			// Missing bucket = no traffic that hour. The SQL path omits empty
			// buckets too (it does not zero-fill), so skipping matches.
			continue
		}
		b := StatsBucket{
			Time:                  time.Unix(c.hour*3600, 0).UTC(),
			Requests:              int(bucketInt(m, fieldRequests)),
			Errors:                int(bucketInt(m, fieldErrors)),
			TokensIn:              bucketInt(m, fieldTokensIn),
			TokensOut:             bucketInt(m, fieldTokensOut),
			CachedTokensIn:        bucketInt(m, fieldCachedIn),
			CacheCreationTokensIn: bucketInt(m, fieldCacheCreateIn),
			AudioTokensIn:         bucketInt(m, fieldAudioIn),
			AudioTokensOut:        bucketInt(m, fieldAudioOut),
			CostFinalSum:          moneyFromFixed(bucketInt(m, fieldCostFixed)).String(),
		}
		if b.Requests == 0 {
			continue
		}
		out = append(out, b)
	}
	return out, true, nil
}

func bucketInt(m map[string]string, field string) int64 {
	v, ok := m[field]
	if !ok {
		return 0
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0
	}
	return n
}
