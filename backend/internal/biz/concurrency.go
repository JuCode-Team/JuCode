package biz

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/jucode/jucode/internal/data"
	"github.com/jucode/jucode/internal/data/ent"
	entgroup "github.com/jucode/jucode/internal/data/ent/modelgroup"
	entusergroup "github.com/jucode/jucode/internal/data/ent/usergroup"
)

// ErrConcurrencyLimit is returned by Acquire when the caller is already at
// or beyond their applicable in-flight slot cap. Gateway maps this to 429.
var ErrConcurrencyLimit = errors.New("concurrency limit reached")

// concurrencyAcquireLuaSrc / concurrencyReleaseLuaSrc are the canonical
// sources of the slot counter scripts. Duplicate copies live at
// internal/data/redis/concurrency.lua for ops review and MUST stay in sync
// (same convention as quotaLuaSrc above).
//
// Both were previously plain INCR/EXPIRE/DECR command sequences. They are Lua
// now for two reasons:
//
//  1. INCR and EXPIRE were not atomic — an INCR that succeeded while the
//     EXPIRE failed left a counter with no TTL at all.
//  2. release() used to DECR unconditionally. When the counter key expired
//     while a request was still in flight (the TTL used to be a flat 5
//     minutes, shorter than stream_total_timeout_seconds — default 600s),
//     the DECR ran against a missing key and Redis created it at -1. That
//     permanently granted the user an extra slot, and it accumulated.
const concurrencyAcquireLuaSrc = `
local limit = tonumber(ARGV[1])
local ttl = tonumber(ARGV[2])

local n = redis.call('INCR', KEYS[1])
if n < 1 then
  -- Counter was negative: legacy damage from the release-on-expired-key bug.
  -- Self-heal to a correct single holder rather than carrying the deficit.
  redis.call('SET', KEYS[1], 1)
  n = 1
end
redis.call('EXPIRE', KEYS[1], ttl)

if n > limit then
  redis.call('DECR', KEYS[1])
  return 0
end
return n
`

const concurrencyReleaseLuaSrc = `
local v = redis.call('GET', KEYS[1])
if not v then
  -- Key already expired (request outlived the TTL). Nothing to give back;
  -- decrementing here is exactly what used to mint the -1 counters.
  return 0
end
local n = tonumber(v)
if n == nil or n <= 0 then
  redis.call('DEL', KEYS[1])
  return 0
end
n = redis.call('DECR', KEYS[1])
if n <= 0 then
  -- Last holder left: drop the key instead of parking a 0 with a long TTL.
  redis.call('DEL', KEYS[1])
end
return n
`

var (
	concurrencyAcquireLua = redis.NewScript(concurrencyAcquireLuaSrc)
	concurrencyReleaseLua = redis.NewScript(concurrencyReleaseLuaSrc)
)

// Counter TTL bounds. The TTL is a backstop for releases that never run
// (process killed mid-request), so it must outlive the longest request the
// gateway will itself allow — otherwise the counter vanishes underneath a
// live request. See counterTTL.
const (
	concurTTLGrace = 5 * time.Minute
	concurTTLMin   = 5 * time.Minute
	// SettingsSvc clamps stream_total_timeout_seconds to 3600 (settings.go),
	// so 3600s + grace is the real ceiling.
	concurTTLMax = 65 * time.Minute
)

// ConcurrencyGate enforces per-user in-flight request caps. Limits live on
// the user's UserGroup (for balance_only billing) or the active Plan (for
// plan_only billing). Counters are stored in Redis and mutated through the
// Lua scripts above; the TTL on each counter is derived from the streaming
// wall-clock cap so a long stream can never outlive its own slot.
type ConcurrencyGate struct {
	d        *data.Data
	plans    *PlanSvc
	settings *SettingsSvc
}

func NewConcurrencyGate(d *data.Data, plans *PlanSvc, settings *SettingsSvc) *ConcurrencyGate {
	return &ConcurrencyGate{d: d, plans: plans, settings: settings}
}

// counterTTL sizes the slot-counter TTL off stream_total_timeout_seconds so
// that a stream running to its wall-clock cap still finds its counter alive
// at release time. When the total timeout is disabled (0 = unbounded stream)
// we assume the worst case and use the ceiling.
func (g *ConcurrencyGate) counterTTL(ctx context.Context) time.Duration {
	ttl := concurTTLMax
	if g.settings != nil {
		if st, err := g.settings.GetSite(ctx); err == nil && st != nil && st.StreamTotalTimeoutSeconds > 0 {
			ttl = time.Duration(st.StreamTotalTimeoutSeconds)*time.Second + concurTTLGrace
		}
	}
	if ttl < concurTTLMin {
		ttl = concurTTLMin
	}
	if ttl > concurTTLMax {
		ttl = concurTTLMax
	}
	return ttl
}

// noopRelease is returned whenever no limit applies (or we fail-open on
// Redis errors), so callers can `defer release()` unconditionally without
// nil-checking.
func noopRelease() {}

func (g *ConcurrencyGate) AcquireWithPlanState(ctx context.Context, u *ent.User, group *ent.ModelGroup, state *PlanChargeState) (release func(), err error) {
	if u == nil || group == nil {
		return noopRelease, nil
	}

	var key string
	var limit int

	// Per-user override wins when set. Key includes "ovr" so the
	// counter stays separate from the group/plan counters even if
	// admin removes the override later — avoids stale counts blocking
	// the user after a config change.
	if u.ConcurrencyLimitOverride > 0 {
		limit = u.ConcurrencyLimitOverride
		key = fmt.Sprintf("concur:u:%s:ovr", u.ID)
	} else {
		switch group.BillingSource {
		case entgroup.BillingSourceBalanceOnly:
			ug, err := g.userGroupFor(ctx, u)
			if err != nil {
				return noopRelease, nil // fail-open on lookup error
			}
			if ug == nil || ug.ConcurrencyLimit <= 0 {
				return noopRelease, nil
			}
			limit = ug.ConcurrencyLimit
			key = fmt.Sprintf("concur:u:%s:bal", u.ID)

		case entgroup.BillingSourcePlanOnly:
			if state == nil || state.UserPlan == nil || state.Plan == nil {
				up, plan, perr := g.plans.ActiveUserPlan(ctx, u.ID)
				if perr != nil || plan == nil || up == nil {
					return noopRelease, nil
				}
				state = &PlanChargeState{UserPlan: up, Plan: plan}
			}
			up := state.UserPlan
			plan := state.Plan
			if plan.ConcurrencyLimit <= 0 {
				return noopRelease, nil
			}
			limit = plan.ConcurrencyLimit
			key = fmt.Sprintf("concur:u:%s:plan:%s", u.ID, up.PlanID)

		default:
			return noopRelease, nil
		}
	}

	ttl := int(g.counterTTL(ctx).Seconds())
	n, rerr := concurrencyAcquireLua.Run(ctx, g.d.Redis, []string{key}, limit, ttl).Int64()
	if rerr != nil {
		return noopRelease, nil // fail-open
	}
	if n == 0 {
		// Script already rolled the counter back; the slot was never held.
		return noopRelease, ErrConcurrencyLimit
	}
	// Release runs on a background ctx so request cancellation can't skip
	// giving the slot back.
	return func() {
		concurrencyReleaseLua.Run(context.Background(), g.d.Redis, []string{key})
	}, nil
}

func (g *ConcurrencyGate) userGroupFor(ctx context.Context, u *ent.User) (*ent.UserGroup, error) {
	return userGroupForUser(ctx, g.d, u)
}

// userGroupForUser resolves the UserGroup whose limits govern this user:
// their explicit user_group_id if set and still exists, otherwise the
// row marked is_default=true (if any). nil-nil means "no applicable
// group" — caller should treat as unlimited.
//
// Shared by ConcurrencyGate and RateLimitGate so a user's concurrency cap and
// rate caps are always read off the same tier.
func userGroupForUser(ctx context.Context, d *data.Data, u *ent.User) (*ent.UserGroup, error) {
	if u.UserGroupID != nil {
		ug, err := d.Ent.UserGroup.Get(ctx, *u.UserGroupID)
		if err == nil {
			return ug, nil
		}
		if !ent.IsNotFound(err) {
			return nil, err
		}
		// dangling user_group_id → fall through to default
	}
	ug, err := d.Ent.UserGroup.Query().
		Where(entusergroup.IsDefaultEQ(true)).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return ug, nil
}
