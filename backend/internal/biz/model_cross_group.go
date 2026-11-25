// model_cross_group.go — candidate selection for cross-group retry
// (product rule D8=B).
//
// The gateway still routes every request to its preferred group first.
// Only when EVERY provider of that group failed BEFORE a single byte was
// written to the client may the request move to another group serving the
// same model. Candidates are constrained to:
//
//   - the SAME billing_source as the group the request was admitted
//     against — a plan-quota request must never silently start spending
//     wallet balance, and vice versa;
//   - keys that opted into multi-group routing (api_keys.auto_route=true).
//     A key pinned to one deterministic group keeps today's single-group
//     behavior; auto_route is also where the rate ceiling lives, so it is
//     the only key shape whose cost exposure is well-defined;
//   - a rate_multiplier within the key's auto_route_max_rate ceiling.
//     0 = uncapped, mirroring selectGroups — the retry path never applies
//     a stricter or looser cap than first-choice routing does;
//   - the key's allowed_group_ids restriction (enforced by selectGroups).
//
// Image, video and async generation traffic never reaches this path: the
// gateway excludes it before asking for fallbacks (see llm/cross_group.go),
// and the /v1/images and /v1/videos handlers do not fail over across
// groups at all.
package biz

import (
	"context"

	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/data/ent"
	entapikey "github.com/jucode/jucode/internal/data/ent/apikey"
)

// CrossGroupFallbackGroups filters candidate groups down to the ones a
// failed request on `chosen` may retry on, preserving the candidates'
// order (selectGroups already sorts auto-route candidates cheapest-first).
// Pure function so the product policy is testable without a database.
func CrossGroupFallbackGroups(candidates []*ent.ModelGroup, chosen *ent.ModelGroup, apiKey *ent.ApiKey) []*ent.ModelGroup {
	if chosen == nil || apiKey == nil || !apiKey.AutoRoute {
		return nil
	}
	out := make([]*ent.ModelGroup, 0, len(candidates))
	for _, g := range candidates {
		if g == nil || g.ID == chosen.ID {
			continue
		}
		if g.BillingSource != chosen.BillingSource {
			continue
		}
		if apiKey.AutoRouteMode != entapikey.AutoRouteModeStable && apiKey.AutoRouteMode != entapikey.AutoRouteModeFast && apiKey.AutoRouteMaxRate > 0 && g.RateMultiplier > apiKey.AutoRouteMaxRate {
			continue
		}
		out = append(out, g)
	}
	return out
}

// CrossGroupFallbackRoutes resolves the ordered fallback routes for a
// request whose preferred group (route.Group) has been exhausted. It is
// called only on the failure path, so the extra candidate/provider queries
// never touch the request hot path. Every error degrades to "no fallback"
// — cross-group retry is an optimization on top of an already-failed
// request and must never turn that failure into a different one.
func (s *ModelSvc) CrossGroupFallbackRoutes(ctx context.Context, route *Route, user *ent.User, apiKey *ent.ApiKey, protocols ...RequestProtocol) []*Route {
	if route == nil || route.Model == nil || route.Group == nil {
		return nil
	}
	if apiKey == nil || !apiKey.AutoRoute {
		return nil
	}
	// An explicit X-JuCode-Group pin narrows routing to exactly one group;
	// retrying elsewhere would override the caller's own choice.
	if _, pinned := groupOverrideFrom(ctx); pinned {
		return nil
	}
	protocol := NormalizeRequestProtocol(protocols...)
	groups, err := s.modelGroups(ctx, route.Model.ID)
	if err != nil {
		return nil
	}
	accessible, err := s.filterGroupsForUser(ctx, user, groups)
	if err != nil {
		return nil
	}
	candidates, err := selectGroups(accessible, apiKey, routePrefFrom(route.Model, s.routeSignals(ctx, route.Model.Name, groupIDsOf(accessible)), uuid.Nil, uuid.Nil))
	if err != nil {
		return nil
	}
	eligible := CrossGroupFallbackGroups(candidates, route.Group, apiKey)
	var stats resolveRejectStats
	out := make([]*Route, 0, len(eligible))
	for _, g := range eligible {
		providers, err := s.groupRouteAccounts(ctx, route.Model, g, &stats, protocol)
		if err != nil || len(providers) == 0 {
			continue
		}
		orderRouteAccounts(providers)
		out = append(out, &Route{Model: route.Model, Group: g, Providers: providers})
	}
	return out
}
