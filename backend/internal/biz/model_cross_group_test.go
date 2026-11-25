package biz

import (
	"testing"

	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/data/ent"
	entgroup "github.com/jucode/jucode/internal/data/ent/modelgroup"
)

func crossGroupTestGroup(name string, source entgroup.BillingSource, rate float64) *ent.ModelGroup {
	return &ent.ModelGroup{ID: uuid.New(), Name: name, BillingSource: source, RateMultiplier: rate}
}

// 同 source 可切：a fallback group with the same billing_source (and no
// rate-cap violation) is eligible; the chosen group itself never is.
func TestCrossGroupFallbackGroupsSameBillingSource(t *testing.T) {
	chosen := crossGroupTestGroup("a", entgroup.BillingSourceBalanceOnly, 1.0)
	same := crossGroupTestGroup("b", entgroup.BillingSourceBalanceOnly, 2.0)
	key := &ent.ApiKey{AutoRoute: true}

	got := CrossGroupFallbackGroups([]*ent.ModelGroup{chosen, same}, chosen, key)
	if len(got) != 1 || got[0].ID != same.ID {
		t.Fatalf("fallbacks = %v, want exactly the same-source sibling", got)
	}
}

// 不同 source 不可切：a plan-only group must never absorb a balance-only
// request's retry (and vice versa).
func TestCrossGroupFallbackGroupsRejectsOtherBillingSource(t *testing.T) {
	chosen := crossGroupTestGroup("a", entgroup.BillingSourceBalanceOnly, 1.0)
	other := crossGroupTestGroup("b", entgroup.BillingSourcePlanOnly, 1.0)
	key := &ent.ApiKey{AutoRoute: true}

	if got := CrossGroupFallbackGroups([]*ent.ModelGroup{chosen, other}, chosen, key); len(got) != 0 {
		t.Fatalf("cross-billing-source fallback allowed: %v", got)
	}
}

// 超倍率不可切：groups above the key's auto_route_max_rate are excluded;
// max rate 0 keeps the existing "no cap" semantics of selectGroups.
func TestCrossGroupFallbackGroupsRespectsRateCap(t *testing.T) {
	chosen := crossGroupTestGroup("a", entgroup.BillingSourceBalanceOnly, 1.0)
	within := crossGroupTestGroup("b", entgroup.BillingSourceBalanceOnly, 1.5)
	over := crossGroupTestGroup("c", entgroup.BillingSourceBalanceOnly, 3.0)
	candidates := []*ent.ModelGroup{chosen, within, over}

	capped := &ent.ApiKey{AutoRoute: true, AutoRouteMaxRate: 2.0}
	got := CrossGroupFallbackGroups(candidates, chosen, capped)
	if len(got) != 1 || got[0].ID != within.ID {
		t.Fatalf("capped fallbacks = %v, want only the within-cap group", got)
	}

	uncapped := &ent.ApiKey{AutoRoute: true, AutoRouteMaxRate: 0}
	if got := CrossGroupFallbackGroups(candidates, chosen, uncapped); len(got) != 2 {
		t.Fatalf("uncapped fallbacks = %d groups, want 2 (0 = no cap, mirroring selectGroups)", len(got))
	}
}

// Cross-group retry is an auto-route feature: a key pinned to deterministic
// single-group routing (or no key at all) never fails over across groups.
func TestCrossGroupFallbackGroupsRequiresAutoRouteKey(t *testing.T) {
	chosen := crossGroupTestGroup("a", entgroup.BillingSourceBalanceOnly, 1.0)
	sibling := crossGroupTestGroup("b", entgroup.BillingSourceBalanceOnly, 1.0)
	candidates := []*ent.ModelGroup{chosen, sibling}

	if got := CrossGroupFallbackGroups(candidates, chosen, nil); got != nil {
		t.Fatalf("nil key got fallbacks: %v", got)
	}
	if got := CrossGroupFallbackGroups(candidates, chosen, &ent.ApiKey{AutoRoute: false}); got != nil {
		t.Fatalf("non-auto-route key got fallbacks: %v", got)
	}
}

// Candidate order is preserved (selectGroups sorts cheapest-first, so the
// retry tries the cheapest eligible sibling first).
func TestCrossGroupFallbackGroupsKeepsOrder(t *testing.T) {
	chosen := crossGroupTestGroup("a", entgroup.BillingSourceBalanceOnly, 1.0)
	cheap := crossGroupTestGroup("b", entgroup.BillingSourceBalanceOnly, 1.2)
	pricey := crossGroupTestGroup("c", entgroup.BillingSourceBalanceOnly, 1.8)
	key := &ent.ApiKey{AutoRoute: true}

	got := CrossGroupFallbackGroups([]*ent.ModelGroup{chosen, cheap, pricey}, chosen, key)
	if len(got) != 2 || got[0].ID != cheap.ID || got[1].ID != pricey.ID {
		t.Fatalf("fallback order mangled: %v", got)
	}
}
