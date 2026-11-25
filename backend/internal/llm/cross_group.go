// cross_group.go — cross-group retry for the text gateways (product rule
// D8=B). See docs/cross-group-retry.md and biz/model_cross_group.go for the
// candidate policy; this file owns the request-side mechanics:
//
//   - the preferred group is still chosen before the request runs; failover
//     is considered only after EVERY provider of the current group failed
//     with nothing written to the client;
//   - the pre-charge hold is re-priced for the new group: a cheaper or
//     equal estimate keeps the existing hold (settle refunds the
//     difference as always), a more expensive one returns the old hold and
//     takes a fresh, larger one — and when that reserve is rejected the
//     whole failover stops ("top up or stop"). At most ONE monetary hold
//     is live at any moment, so a request can never be double-charged;
//   - image / video / async-generation traffic never fails over across
//     groups (the /v1/images and /v1/videos handlers have no cross-group
//     path at all; chat-carried image generation is excluded here).
//
// The concurrency slot and the rate-limit ticket taken for the preferred
// group are deliberately kept across the switch: both scopes key off the
// user + the group's billing_source (see biz/concurrency.go and
// biz/rate_limit.go), and cross-group retry never leaves the billing
// source, so the original slot/ticket remain the correct ones.
package llm

import (
	"context"
	"log/slog"
	"time"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/data/ent"
)

// crossGroupRetryPermitted is the pure product-policy gate: only API keys
// that opted into multi-group routing may fail over across groups, and
// media-style requests (image generation via chat, video, async tasks)
// never do.
func crossGroupRetryPermitted(apiKey *ent.ApiKey, media bool) bool {
	return apiKey != nil && apiKey.AutoRoute && !media
}

// groupFailover carries the state of one request's cross-group retry.
// Fallback candidates are loaded lazily — the first time the retry loop
// actually runs out of providers — so the request hot path pays nothing.
// crossGroupHoldScope prefixes the fresh idempotency key taken on a top-up
// re-reserve. It is deliberately not a per-caller knob: biz's last-request
// serialization counts only llm:/ws: holds, so a gateway that picked its own
// prefix here would hide its hold from that check and let a second request
// overdraw the same balance beside it.
const crossGroupHoldScope = "llm"

// maxGroupSwitches bounds how many times one request may change group. The
// per-group attempt budget (llm.max_attempts) resets on every switch, so the
// worst case a request can cost upstream is max_attempts × (1 + this), and
// the worst case it can cost in latency is bounded by the stream wall-clock
// timeout on top of that. Two switches means at most three groups are tried:
// enough to get past a cheap group that is wholly parked plus one more, and
// small enough that a systemic outage does not turn every request into a
// long walk down the whole price ladder.
const maxGroupSwitches = 2

type groupFailover struct {
	g         *Gateway
	user      *ent.User
	apiKey    *ent.ApiKey
	protocol  biz.RequestProtocol
	requestID string
	estimate  biz.CostEstimateInput
	media     bool

	loaded  bool
	stopped bool
	pending []*biz.Route
	// switches counts completed group moves (observability).
	switches int
}

func newGroupFailover(g *Gateway, user *ent.User, apiKey *ent.ApiKey, protocol biz.RequestProtocol,
	requestID string, estimate biz.CostEstimateInput, media bool,
) *groupFailover {
	return &groupFailover{
		g: g, user: user, apiKey: apiKey, protocol: protocol,
		requestID: requestID, estimate: estimate, media: media,
	}
}

func (f *groupFailover) load(ctx context.Context, route *biz.Route) {
	if f.loaded {
		return
	}
	f.loaded = true
	f.pending = f.g.models.CrossGroupFallbackRoutes(ctx, route, f.user, f.apiKey, f.protocol)
}

// available reports whether at least one untried fallback group remains.
// Used by the retry loop's "is there anywhere left to go" conditions; it
// does NOT consume a candidate.
func (f *groupFailover) available(ctx context.Context, route *biz.Route) bool {
	if f == nil || f.stopped || !crossGroupRetryPermitted(f.apiKey, f.media) {
		return false
	}
	f.load(ctx, route)
	return len(f.pending) > 0
}

// next moves the request to the next eligible fallback group: access is
// re-checked, the billing precheck re-run, and the hold re-priced for the
// new group (see rehold). Returns the new route + plan state, or (nil, nil)
// when failover is not possible — because bytes already reached the client,
// the group-switch cap is reached, no candidate passes the checks, or a
// required hold top-up was rejected.
func (f *groupFailover) next(ctx context.Context, route *biz.Route, written bool, lc *logContext) (*biz.Route, *biz.PlanChargeState) {
	if written || f.switches >= maxGroupSwitches || !f.available(ctx, route) {
		return nil, nil
	}
	for len(f.pending) > 0 {
		cand := f.pending[0]
		f.pending = f.pending[1:]
		if err := f.g.models.CheckAccess(ctx, f.user, f.apiKey, cand.Group); err != nil {
			continue
		}
		state, err := f.g.billing.ResolvePlanState(ctx, f.user, f.apiKey, cand.Group)
		if err != nil {
			continue
		}
		if !f.rehold(ctx, cand, state, lc) {
			// Candidates are ordered cheapest-first: if this group's hold
			// cannot be funded, a pricier one cannot either. Stop for good.
			f.stopped = true
			return nil, nil
		}
		cand.Providers = f.g.partitionCoolingProviders(ctx, cand.Providers)
		f.switches++
		slog.Info("gateway.cross_group_retry",
			"request_id", f.requestID,
			"from_group", route.Group.Name,
			"to_group", cand.Group.Name,
			"switches", f.switches,
		)
		return cand, state
	}
	return nil, nil
}

// rehold re-prices the pre-charge hold for the fallback group.
//
//	new estimate <= held amount → keep the existing hold. Settle already
//	    refunds hold-minus-actual, so a cheaper group just refunds more.
//	new estimate  > held amount → release the old hold, then reserve the
//	    full new estimate under a fresh server-owned key. If that reserve
//	    is rejected (balance/limits) the failover stops — the request is
//	    NOT allowed to run against a group its hold does not cover.
//
// The release-then-reserve order guarantees at most one live monetary hold
// per request, which is what makes double-charging impossible: settle and
// release always act on lc.Reservation, and that always names the single
// hold currently backing the request. A crash between the two steps leaks
// nothing — the old hold was already returned and no new one exists.
func (f *groupFailover) rehold(ctx context.Context, cand *biz.Route, state *biz.PlanChargeState, lc *logContext) bool {
	est := f.g.billing.EstimateMaxCost(ctx, cand.Model, cand.Group, f.estimate)
	cur := lc.Reservation
	if cur != nil && cur.Monetary() && est.LessThanOrEqual(cur.Amount) {
		return true
	}
	if cur != nil && cur.Monetary() {
		f.g.releaseReservation(ctx, cur)
		lc.Reservation = nil
	}
	// Plan-quota pseudo-holds carry no money; re-reserving swaps in the
	// freshly resolved plan state for the new group's settle.
	res, err := f.g.billing.Reserve(ctx, biz.ReserveInput{
		User: f.user, ApiKey: f.apiKey, Group: cand.Group, PlanState: state,
		Key: newBillingHoldKey(crossGroupHoldScope), Amount: est, TTL: biz.ReserveTTLRequest,
		Note: "cross-group re-reserve " + f.requestID,
	})
	if err != nil {
		slog.Warn("gateway.cross_group_rehold_failed",
			"request_id", f.requestID,
			"to_group", cand.Group.Name,
			"err", err.Error(),
		)
		return false
	}
	lc.Reservation = res
	return true
}

// partitionCoolingProviders reorders a fallback group's account chain the same
// way the primary group's chain is filtered at the top of the retry loop:
// accounts that cannot serve right now are dropped while at least one active
// account exists, otherwise the parked set is used as-is.
func (g *Gateway) partitionCoolingProviders(ctx context.Context, providers []biz.RouteAccount) []biz.RouteAccount {
	if len(providers) <= 1 {
		return providers
	}
	now := time.Now()
	active := make([]biz.RouteAccount, 0, len(providers))
	parked := make([]biz.RouteAccount, 0, len(providers))
	for _, rp := range providers {
		if rp.Account != nil && biz.AccountSchedulable(rp.Account, "", now) {
			active = append(active, rp)
		} else {
			parked = append(parked, rp)
		}
	}
	if len(active) > 0 {
		return active
	}
	if len(parked) > 0 {
		return parked
	}
	return providers
}
