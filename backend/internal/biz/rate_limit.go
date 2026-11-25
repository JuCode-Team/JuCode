package biz

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/jucode/jucode/internal/data"
	"github.com/jucode/jucode/internal/data/ent"
	entgroup "github.com/jucode/jucode/internal/data/ent/modelgroup"
)

// ErrRPMLimit / ErrTPMLimit are returned by RateLimitGate.Peek. The gateway
// maps both to 429 with a Retry-After.
var (
	ErrRPMLimit = errors.New("request rate limit reached")
	ErrTPMLimit = errors.New("token rate limit reached")
)

// RateLimitMode selects how far the gate goes.
//
// Shadow exists because these are the first rate caps on the generation
// endpoints: limits configured against guesswork will reject real traffic.
// In shadow the counters are maintained and violations are logged, so the
// numbers can be tuned against production before anything gets a 429.
type RateLimitMode string

const (
	RateLimitOff     RateLimitMode = "off"
	RateLimitShadow  RateLimitMode = "shadow"
	RateLimitEnforce RateLimitMode = "enforce"
)

func ParseRateLimitMode(s string) RateLimitMode {
	switch RateLimitMode(s) {
	case RateLimitShadow:
		return RateLimitShadow
	case RateLimitEnforce:
		return RateLimitEnforce
	default:
		return RateLimitOff
	}
}

// rateLimitWindow is the counter window. Fixed (tumbling), matching the
// existing middleware/ratelimit.go convention rather than inventing a second
// one. The cost is the classic fixed-window edge burst: up to 2x the limit can
// pass across a boundary. Acceptable here because these caps exist to stop
// runaway clients, not to shape traffic precisely.
const rateLimitWindow = 60 * time.Second

// Counters outlive their window by one period so a settle that lands just
// after the boundary still finds the key it is topping up.
const rateLimitTTL = 2 * rateLimitWindow

// commitLuaSrc / settleLuaSrc mirror internal/data/redis/rate_limit.lua.
// Both are Lua for the same reason concurrency.go is: INCR and EXPIRE as
// separate commands can leave a counter with no TTL if the second one is lost.
const rateLimitCommitLuaSrc = `
local n = redis.call('INCRBY', KEYS[1], tonumber(ARGV[1]))
redis.call('EXPIRE', KEYS[1], tonumber(ARGV[2]))
return n
`

var rateLimitCommitLua = redis.NewScript(rateLimitCommitLuaSrc)

// RateLimitGate enforces per-user request/token rate caps using the
// peek -> commit -> settle protocol.
//
//	peek    before dispatch    read-only check, can reject
//	commit  after the upstream accepted the request, +1 request
//	settle  after the response, += output tokens
//
// Commit deliberately runs after the upstream accepted, not at peek time:
// upstream 5xx and connection failures then cost the caller nothing, which is
// the behavior anyone reading "requests per minute" expects.
//
// The two dimensions are checked with different semantics, because the token
// count of a request is unknowable until it is over:
//
//   - RPM is forward-looking: reject when cur+1 would exceed the cap.
//   - TPM is historical: reject only when the window is ALREADY over budget.
//     The request that blows past the cap is allowed to finish; the next one
//     is refused.
//
// There is no TPM pre-deduction and therefore no rollback to get wrong on the
// error paths — settle only ever adds output tokens from completed responses.
type RateLimitGate struct {
	d      *data.Data
	plans  *PlanSvc
	mode   RateLimitMode
	logger *slog.Logger
	nowFn  func() time.Time // swappable in tests
}

// rateLimitScope is the resolved limit set plus the key prefix it applies to.
// The prefix encodes which tier the limits came from so that moving a user
// between tiers does not inherit the previous tier's counters.
type rateLimitScope struct {
	prefix   string
	rpmLimit int
	tpmLimit int
}

func (s rateLimitScope) active() bool { return s.rpmLimit > 0 || s.tpmLimit > 0 }

func NewRateLimitGate(d *data.Data, plans *PlanSvc, mode RateLimitMode, logger *slog.Logger) *RateLimitGate {
	if logger == nil {
		logger = slog.Default()
	}
	return &RateLimitGate{
		d:      d,
		plans:  plans,
		mode:   mode,
		logger: logger.With("svc", "rate_limit"),
		nowFn:  time.Now,
	}
}

func (g *RateLimitGate) Mode() RateLimitMode {
	if g == nil {
		return RateLimitOff
	}
	return g.mode
}

func (g *RateLimitGate) enabled() bool {
	return g != nil && g.mode != RateLimitOff && g.d != nil && g.d.Redis != nil
}

func (g *RateLimitGate) window() int64 {
	return g.nowFn().Unix() / int64(rateLimitWindow.Seconds())
}

func rateLimitKey(prefix, dim string, window int64) string {
	return fmt.Sprintf("rl:%s:%s:%d", dim, prefix, window)
}

// resolveScope mirrors ConcurrencyGate's priority chain exactly, so a user's
// rate caps and concurrency cap always come from the same tier.
func (g *RateLimitGate) resolveScope(ctx context.Context, u *ent.User, group *ent.ModelGroup, state *PlanChargeState) (rateLimitScope, bool) {
	if u == nil || group == nil {
		return rateLimitScope{}, false
	}
	if u.RpmLimitOverride > 0 || u.TpmLimitOverride > 0 {
		return rateLimitScope{
			prefix:   "u:" + u.ID.String() + ":ovr",
			rpmLimit: u.RpmLimitOverride,
			tpmLimit: u.TpmLimitOverride,
		}, true
	}
	// Everything below reads the database. The gate is wired via a setter and
	// can legitimately be constructed without one (tests, and any future
	// caller that only wants the override tier), so degrade to "no caps"
	// instead of dereferencing a nil client.
	if g.d == nil || g.d.Ent == nil {
		return rateLimitScope{}, false
	}
	switch group.BillingSource {
	case entgroup.BillingSourceBalanceOnly:
		// NOTE: this is a second read of the same user_group row that
		// ConcurrencyGate.AcquireWithPlanState just did. Both are indexed
		// point reads on a tiny table, so it is left duplicated rather than
		// threading a shared resolution through the gateway; revisit if the
		// row ever grows or the query shows up in latency_detail.
		ug, err := userGroupForUser(ctx, g.d, u)
		if err != nil || ug == nil {
			return rateLimitScope{}, false
		}
		return rateLimitScope{
			prefix:   "u:" + u.ID.String() + ":bal",
			rpmLimit: ug.RpmLimit,
			tpmLimit: ug.TpmLimit,
		}, true
	case entgroup.BillingSourcePlanOnly:
		if state == nil || state.UserPlan == nil || state.Plan == nil {
			up, plan, err := g.plans.ActiveUserPlan(ctx, u.ID)
			if err != nil || up == nil || plan == nil {
				return rateLimitScope{}, false
			}
			state = &PlanChargeState{UserPlan: up, Plan: plan}
		}
		return rateLimitScope{
			prefix:   "u:" + u.ID.String() + ":plan:" + state.UserPlan.PlanID.String(),
			rpmLimit: state.Plan.RpmLimit,
			tpmLimit: state.Plan.TpmLimit,
		}, true
	default:
		return rateLimitScope{}, false
	}
}

// EffectiveLimitsForUser reports the caps a user is currently subject to, for
// display next to their live rate. 0 = unlimited.
//
// Approximation, and deliberately so: the enforced cap depends on the billing
// source of whichever model group a request routes to, which is not known
// outside a request. This resolves the same priority chain with the group step
// collapsed — override, else active plan, else user group. For the common case
// (a user on one billing source) it is exact; for a user spanning both it
// reports the plan's caps.
func (g *RateLimitGate) EffectiveLimitsForUser(ctx context.Context, u *ent.User) (rpm, tpm int) {
	if u == nil {
		return 0, 0
	}
	if u.RpmLimitOverride > 0 || u.TpmLimitOverride > 0 {
		return u.RpmLimitOverride, u.TpmLimitOverride
	}
	if g == nil || g.d == nil || g.d.Ent == nil {
		return 0, 0
	}
	if g.plans != nil {
		if up, plan, err := g.plans.ActiveUserPlan(ctx, u.ID); err == nil && up != nil && plan != nil {
			if plan.RpmLimit > 0 || plan.TpmLimit > 0 {
				return plan.RpmLimit, plan.TpmLimit
			}
		}
	}
	if ug, err := userGroupForUser(ctx, g.d, u); err == nil && ug != nil {
		return ug.RpmLimit, ug.TpmLimit
	}
	return 0, 0
}

// RateLimitTicket carries the resolved scope from peek through to settle so
// the later phases neither re-resolve limits nor re-hit the database.
type RateLimitTicket struct {
	scope rateLimitScope
	gate  *RateLimitGate
}

// Peek checks the caps without consuming anything. A nil error means the
// request may proceed; the returned ticket must be passed to Commit/Settle.
//
// Redis failures fail open, matching every other gate in this package
// (concurrency.go, middleware/ratelimit.go, the quota precheck): a Redis blip
// must not 429 all traffic.
func (g *RateLimitGate) Peek(ctx context.Context, u *ent.User, group *ent.ModelGroup, state *PlanChargeState) (*RateLimitTicket, error) {
	if !g.enabled() {
		return nil, nil
	}
	scope, ok := g.resolveScope(ctx, u, group, state)
	if !ok || !scope.active() {
		return nil, nil
	}
	ticket := &RateLimitTicket{scope: scope, gate: g}

	w := g.window()
	res, err := g.d.Redis.MGet(ctx,
		rateLimitKey(scope.prefix, "rpm", w),
		rateLimitKey(scope.prefix, "tpm", w),
	).Result()
	if err != nil {
		return ticket, nil // fail open
	}
	rpmUsed := redisCounterValue(res, 0)
	tpmUsed := redisCounterValue(res, 1)

	var violation error
	switch {
	// Forward-looking: this request would be the one to exceed the cap.
	case scope.rpmLimit > 0 && rpmUsed+1 > int64(scope.rpmLimit):
		violation = ErrRPMLimit
	// Historical: only refuse once the window is already over budget, since
	// this request's token count cannot be known yet.
	case scope.tpmLimit > 0 && tpmUsed >= int64(scope.tpmLimit):
		violation = ErrTPMLimit
	}
	if violation == nil {
		return ticket, nil
	}

	if g.mode == RateLimitShadow {
		g.logger.Info("rate limit would reject (shadow mode)",
			"user_id", u.ID, "limit", violation.Error(),
			"rpm_used", rpmUsed, "rpm_limit", scope.rpmLimit,
			"tpm_used", tpmUsed, "tpm_limit", scope.tpmLimit)
		return ticket, nil
	}
	return ticket, violation
}

// Commit records one accepted request. Call it once the upstream has taken
// the request, so failures to even reach a provider stay free.
func (t *RateLimitTicket) Commit(ctx context.Context) {
	if t == nil || t.gate == nil || t.scope.rpmLimit <= 0 {
		return
	}
	g := t.gate
	key := rateLimitKey(t.scope.prefix, "rpm", g.window())
	_ = rateLimitCommitLua.Run(ctx, g.d.Redis, []string{key}, 1, int(rateLimitTTL.Seconds())).Err()
}

// Settle adds the output tokens produced by a finished request.
//
// Written to the window current at settle time rather than the one the request
// commit landed in: a long stream would otherwise top up a window that has
// already expired, and the tokens would vanish. Attributing them to "now" keeps
// them inside a live window, which is what the cap is about.
func (t *RateLimitTicket) Settle(ctx context.Context, outputTokens int64) {
	if t == nil || t.gate == nil || t.scope.tpmLimit <= 0 || outputTokens <= 0 {
		return
	}
	g := t.gate
	key := rateLimitKey(t.scope.prefix, "tpm", g.window())
	_ = rateLimitCommitLua.Run(ctx, g.d.Redis, []string{key}, outputTokens, int(rateLimitTTL.Seconds())).Err()
}

func redisCounterValue(vals []any, i int) int64 {
	if i >= len(vals) || vals[i] == nil {
		return 0
	}
	s, ok := vals[i].(string)
	if !ok {
		return 0
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return n
}
