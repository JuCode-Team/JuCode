package biz

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/data/ent"
	entgroup "github.com/jucode/jucode/internal/data/ent/modelgroup"
	entaccount "github.com/jucode/jucode/internal/data/ent/upstreamaccount"
)

func TestAccountSupportsRequestProtocol(t *testing.T) {
	msgs := []string{NativeProtocolMessages}
	chat := []string{NativeProtocolChat}
	both := []string{NativeProtocolChat, NativeProtocolMessages}
	responses := []string{NativeProtocolResponses}

	tests := []struct {
		name         string
		native       []string
		allowConvert bool
		conversionOn bool
		protocol     RequestProtocol
		want         bool
	}{
		// Native matches: the request is forwarded unchanged.
		{name: "messages account serves messages", native: msgs, protocol: RequestProtocolAnthropic, want: true},
		{name: "chat account serves chat", native: chat, protocol: RequestProtocolOpenAI, want: true},
		{name: "responses account serves responses", native: responses, protocol: RequestProtocolResponses, want: true},
		{name: "responses account serves the openai family", native: responses, protocol: RequestProtocolOpenAI, want: true},
		{name: "dual account serves both", native: both, protocol: RequestProtocolAnthropic, want: true},
		{name: "dual account serves chat", native: both, protocol: RequestProtocolOpenAI, want: true},
		{name: "responses account serves the family", native: responses, protocol: RequestProtocolOpenAI, want: true},
		{name: "messages account rejects the openai family", native: msgs, protocol: RequestProtocolOpenAI, want: false},
		{name: "messages account plus opt-in and switch serves the family", native: msgs, allowConvert: true, conversionOn: true, protocol: RequestProtocolOpenAI, want: true},

		// Not native: only conversion makes it a candidate, and only when the
		// account opted in AND the site switch is on.
		{name: "messages account rejects chat", native: msgs, protocol: RequestProtocolOpenAI, want: false},
		{name: "chat account rejects messages", native: chat, protocol: RequestProtocolAnthropic, want: false},
		{name: "messages account rejects responses", native: msgs, protocol: RequestProtocolResponses, want: false},
		{name: "opt-in alone is not enough", native: msgs, allowConvert: true, protocol: RequestProtocolOpenAI, want: false},
		{name: "site switch alone is not enough", native: msgs, conversionOn: true, protocol: RequestProtocolOpenAI, want: false},
		{name: "opt-in plus switch converts chat", native: msgs, allowConvert: true, conversionOn: true, protocol: RequestProtocolOpenAI, want: true},
		{name: "opt-in plus switch converts responses", native: msgs, allowConvert: true, conversionOn: true, protocol: RequestProtocolResponses, want: true},
		{name: "conversion covers the other direction", native: chat, allowConvert: true, conversionOn: true, protocol: RequestProtocolAnthropic, want: true},

		// The router's "any" keeps every account in play.
		{name: "any keeps a messages account", native: msgs, protocol: RequestProtocolAny, want: true},
		{name: "any keeps an undeclared account", protocol: RequestProtocolAny, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			acct := &ent.UpstreamAccount{
				NativeProtocols: tt.native,
				AllowConversion: tt.allowConvert,
			}
			if got := AccountSupportsRequestProtocol(acct, tt.protocol, tt.conversionOn); got != tt.want {
				t.Fatalf("native=%v allow_conversion=%v site_switch=%v protocol=%s got=%v want=%v",
					tt.native, tt.allowConvert, tt.conversionOn, tt.protocol, got, tt.want)
			}
		})
	}
}

// TestNormalizeNativeProtocols pins the stored vocabulary: the admin boundary
// canonicalizes what an operator typed, and rejects dialects the gateway cannot
// serve.
func TestNormalizeNativeProtocols(t *testing.T) {
	got, err := NormalizeNativeProtocols([]string{" Messages ", "chat_completions", "chat_completions"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 || got[0] != NativeProtocolMessages || got[1] != NativeProtocolChat {
		t.Fatalf("normalize = %v, want [messages chat_completions]", got)
	}
	if _, err := NormalizeNativeProtocols([]string{"grpc"}); err == nil {
		t.Fatal("an unknown dialect must be rejected at the admin boundary")
	}
	if out, err := NormalizeNativeProtocols(nil); err != nil || len(out) != 0 {
		t.Fatalf("empty declaration should stay empty, got %v err=%v", out, err)
	}
}

func probeFor(providerCount []int, schedulable []bool) routeGroupProbe {
	return routeGroupProbe{
		providers: func(i int) []RouteAccount {
			return make([]RouteAccount, providerCount[i])
		},
		schedulable: func(i int) bool { return schedulable[i] },
	}
}

func balanceGroups(names ...string) []*ent.ModelGroup {
	out := make([]*ent.ModelGroup, 0, len(names))
	for _, n := range names {
		out = append(out, &ent.ModelGroup{Name: n, BillingSource: entgroup.BillingSourceBalanceOnly})
	}
	return out
}

// The bug this guards: a group whose every account was parked by an upstream
// 429 (rate_limit_reset_at in the future) still won auto-route on price,
// because the group-level check only read the Redis cooldown key and never
// the account's own health state. With s.d nil the Redis half is skipped, so
// this exercises exactly the half that was missing.
func TestAnyProviderSchedulableRejectsRateLimitedAccounts(t *testing.T) {
	future := time.Now().Add(30 * time.Minute)
	past := time.Now().Add(-time.Minute)
	svc := &ModelSvc{}

	parked := []RouteAccount{{Account: &ent.UpstreamAccount{
		Status: entaccount.StatusActive, Schedulable: true, RateLimitResetAt: &future,
	}}}
	if svc.anyProviderSchedulable(context.Background(), parked, "gpt-5") {
		t.Fatal("account inside its 429 window must not keep the group eligible")
	}

	recovered := []RouteAccount{{Account: &ent.UpstreamAccount{
		Status: entaccount.StatusActive, Schedulable: true, RateLimitResetAt: &past,
	}}}
	if !svc.anyProviderSchedulable(context.Background(), recovered, "gpt-5") {
		t.Fatal("account past its 429 window must keep the group eligible")
	}

	mixed := []RouteAccount{
		{Account: &ent.UpstreamAccount{Status: entaccount.StatusActive, Schedulable: true, RateLimitResetAt: &future}},
		{Account: &ent.UpstreamAccount{Status: entaccount.StatusActive, Schedulable: true}},
	}
	if !svc.anyProviderSchedulable(context.Background(), mixed, "gpt-5") {
		t.Fatal("one healthy account is enough to keep the group eligible")
	}
}

// The bug this guards: gpt-plus's only channel was in runtime cooldown, but
// Resolve returned gpt-plus anyway because it only checked that providers
// existed. Every request died there instead of trying gpt-pro.
func TestStayOnHeldGroupIgnoresCooling(t *testing.T) {
	groups := balanceGroups("held", "other")
	groups[0].ID = uuid.New()
	groups[1].ID = uuid.New()
	idx, cooling, ok := stayOnHeldGroup(groups, groups[0].ID, probeFor([]int{1, 1}, []bool{false, true}))
	if !ok || idx != 0 || !cooling {
		t.Fatalf("held cooling group should stay, idx=%d cooling=%v ok=%v", idx, cooling, ok)
	}
	if _, _, ok := stayOnHeldGroup(groups, groups[0].ID, probeFor([]int{0, 1}, []bool{false, true})); ok {
		t.Fatal("held group with no providers should release the hold")
	}
}

func TestChooseRouteGroupFallsThroughWhenCheapestIsCooling(t *testing.T) {
	groups := balanceGroups("gpt-plus", "gpt-pro")
	var stats resolveRejectStats
	idx, cooling := chooseRouteGroup(groups, probeFor([]int{1, 1}, []bool{false, true}), &stats)
	if idx != 1 || cooling {
		t.Fatalf("idx=%d cooling=%v, want idx=1 (gpt-pro) and not a cooling fallback", idx, cooling)
	}
	if stats.groupsFullyCooling != 1 {
		t.Fatalf("groupsFullyCooling = %d, want 1", stats.groupsFullyCooling)
	}
}

func TestChooseRouteGroupPrefersCheapestWhenUsable(t *testing.T) {
	groups := balanceGroups("gpt-plus", "gpt-pro")
	var stats resolveRejectStats
	idx, cooling := chooseRouteGroup(groups, probeFor([]int{1, 1}, []bool{true, true}), &stats)
	if idx != 0 || cooling {
		t.Fatalf("idx=%d cooling=%v, want the cheapest group unchanged", idx, cooling)
	}
}

// All cooling must not turn into "no provider available" — that would convert
// a transient upstream blip into a hard failure for the caller.
func TestChooseRouteGroupKeepsCheapestWhenEveryCandidateIsCooling(t *testing.T) {
	groups := balanceGroups("gpt-plus", "gpt-pro")
	var stats resolveRejectStats
	idx, cooling := chooseRouteGroup(groups, probeFor([]int{1, 1}, []bool{false, false}), &stats)
	if idx != 0 || !cooling {
		t.Fatalf("idx=%d cooling=%v, want idx=0 flagged as a cooling fallback", idx, cooling)
	}
}

// A group with no configured providers is not a fall-back target, so an
// all-cooling chain must not land on it.
func TestChooseRouteGroupSkipsUnconfiguredGroups(t *testing.T) {
	groups := balanceGroups("empty", "configured")
	var stats resolveRejectStats
	idx, cooling := chooseRouteGroup(groups, probeFor([]int{0, 1}, []bool{true, false}), &stats)
	if idx != 1 || !cooling {
		t.Fatalf("idx=%d cooling=%v, want the configured group as cooling fallback", idx, cooling)
	}
	idx, _ = chooseRouteGroup(balanceGroups("empty"), probeFor([]int{0}, []bool{true}), &stats)
	if idx != -1 {
		t.Fatalf("idx=%d, want -1 when nothing is configured", idx)
	}
}

// Fall-through must not change how the request is paid for: a plan-only group
// never spills into a balance-only one, even when it is entirely cooling.
func TestChooseRouteGroupDoesNotCrossBillingSource(t *testing.T) {
	groups := []*ent.ModelGroup{
		{Name: "plan", BillingSource: entgroup.BillingSourcePlanOnly},
		{Name: "balance", BillingSource: entgroup.BillingSourceBalanceOnly},
	}
	var stats resolveRejectStats
	idx, cooling := chooseRouteGroup(groups, probeFor([]int{1, 1}, []bool{false, true}), &stats)
	if idx != 0 || !cooling {
		t.Fatalf("idx=%d cooling=%v, want to stay on the plan-only group", idx, cooling)
	}
	if stats.groupsWithOtherBilling != 1 {
		t.Fatalf("groupsWithOtherBilling = %d, want 1", stats.groupsWithOtherBilling)
	}
}

// The billing anchor is the first *usable* group. Anchoring on candidates[0]
// unconditionally would let an unconfigured group at the head of the list
// veto every group that bills differently, turning a working setup into
// "no available provider".
func TestChooseRouteGroupAnchorsBillingOnFirstUsableGroup(t *testing.T) {
	groups := []*ent.ModelGroup{
		{Name: "plan-empty", BillingSource: entgroup.BillingSourcePlanOnly},
		{Name: "balance", BillingSource: entgroup.BillingSourceBalanceOnly},
	}
	var stats resolveRejectStats
	idx, cooling := chooseRouteGroup(groups, probeFor([]int{0, 1}, []bool{true, true}), &stats)
	if idx != 1 || cooling {
		t.Fatalf("idx=%d cooling=%v, want the configured balance group", idx, cooling)
	}
	if stats.groupsWithOtherBilling != 0 {
		t.Fatalf("groupsWithOtherBilling = %d, want 0 — nothing was anchored yet", stats.groupsWithOtherBilling)
	}
}

// Weights are scaled by the health factor (healthy/unknown ×10) before the
// weighted shuffle, so pick thresholds here are in the scaled space. With a
// nil Provider the factor is the healthy/unknown default (×10).

func TestOrderRouteAccountsKeepsPriorityTiers(t *testing.T) {
	providers := []RouteAccount{
		{UpstreamModel: "high", Priority: 20, Weight: 100},
		{UpstreamModel: "low-a", Priority: 10, Weight: 100},
		{UpstreamModel: "low-b", Priority: 10, Weight: 100},
	}
	// Tier {low-a,low-b}: effective weights 1000 each, total 2000; a pick
	// of 1000 lands in low-b's [1000,2000) slot.
	picks := []int{1000}
	orderRouteAccountsWithPicker(providers, func(n int) int {
		if len(picks) == 0 {
			t.Fatalf("unexpected pick with n=%d", n)
		}
		v := picks[0]
		picks = picks[1:]
		return v
	})

	if providers[0].UpstreamModel != "low-b" || providers[1].UpstreamModel != "low-a" || providers[2].UpstreamModel != "high" {
		t.Fatalf("unexpected provider order: %s %s %s", providers[0].UpstreamModel, providers[1].UpstreamModel, providers[2].UpstreamModel)
	}
}

func TestWeightedShuffleRouteAccountsUsesWeights(t *testing.T) {
	providers := []RouteAccount{
		{UpstreamModel: "a", Priority: 100, Weight: 1},
		{UpstreamModel: "b", Priority: 100, Weight: 3},
		{UpstreamModel: "c", Priority: 100, Weight: 1},
	}
	// Effective weights ×10: a=10,b=30,c=10. i=0 total=50, pick 10 → b
	// ([10,40)); i=1 over {a,c} total=20, pick 10 → c ([10,20)).
	picks := []int{10, 10}
	weightedShuffleRouteAccounts(providers, func(n int) int {
		if len(picks) == 0 {
			t.Fatalf("unexpected pick with n=%d", n)
		}
		v := picks[0]
		picks = picks[1:]
		return v
	})

	if providers[0].UpstreamModel != "b" || providers[1].UpstreamModel != "c" || providers[2].UpstreamModel != "a" {
		t.Fatalf("unexpected weighted order: %s %s %s", providers[0].UpstreamModel, providers[1].UpstreamModel, providers[2].UpstreamModel)
	}
}

// TestHealthDeprioritizesWithinTier verifies an unhealthy provider gets a
// reduced effective weight (factor 1 vs healthy 10) but is never excluded.
func TestHealthDeprioritizesWithinTier(t *testing.T) {
	healthy := &ent.UpstreamAccount{Health: entaccount.HealthHealthy}
	unhealthy := &ent.UpstreamAccount{Health: entaccount.HealthUnhealthy}

	rpHealthy := RouteAccount{Account: healthy, UpstreamModel: "h", Priority: 1, Weight: 10}
	rpUnhealthy := RouteAccount{Account: unhealthy, UpstreamModel: "u", Priority: 1, Weight: 10}

	if w := effectiveRouteWeight(rpHealthy); w != 100 {
		t.Fatalf("healthy effective weight = %d, want 100", w)
	}
	if w := effectiveRouteWeight(rpUnhealthy); w != 10 {
		t.Fatalf("unhealthy effective weight = %d, want 10 (reduced, not zero)", w)
	}

	// Total 110; a pick of 100 falls in the unhealthy [100,110) slot, proving
	// it can still be chosen (never starved).
	providers := []RouteAccount{rpHealthy, rpUnhealthy}
	weightedShuffleRouteAccounts(providers, func(int) int { return 100 })
	if providers[0].UpstreamModel != "u" {
		t.Fatalf("unhealthy should still be reachable; got first=%s", providers[0].UpstreamModel)
	}
}

// TestAccountUpstreamProtocolForModel covers the per-model pin: the upstream is
// spoken to in the pinned protocol, eligibility still respects conversion
// gating, and a pin naming a protocol the endpoint does not speak makes the
// pair unusable.
func TestAccountUpstreamProtocolForModel(t *testing.T) {
	both := []string{NativeProtocolChat, NativeProtocolResponses}
	msgs := []string{NativeProtocolMessages}

	tests := []struct {
		name         string
		native       []string
		pin          string
		allowConvert bool
		conversionOn bool
		protocol     RequestProtocol
		wantProto    string
		wantOK       bool
	}{
		{name: "empty pin defers to account rules", native: both, protocol: RequestProtocolAnthropic, allowConvert: true, conversionOn: true, wantProto: NativeProtocolChat, wantOK: true},
		{name: "native pin serves its own protocol", native: both, pin: NativeProtocolResponses, protocol: RequestProtocolResponses, wantProto: NativeProtocolResponses, wantOK: true},
		{name: "pin overrides the client dialect when conversion is on", native: both, pin: NativeProtocolResponses, protocol: RequestProtocolCompatible, allowConvert: true, conversionOn: true, wantProto: NativeProtocolResponses, wantOK: true},
		{name: "pin override blocked without conversion", native: both, pin: NativeProtocolResponses, protocol: RequestProtocolCompatible, wantOK: false},
		{name: "pin wins for the openai family", native: both, pin: NativeProtocolResponses, protocol: RequestProtocolOpenAI, wantProto: NativeProtocolResponses, wantOK: true},
		{name: "any client speaks the pin", native: both, pin: NativeProtocolChat, protocol: RequestProtocolAny, wantProto: NativeProtocolChat, wantOK: true},
		{name: "pin outside native set is unusable", native: msgs, pin: NativeProtocolResponses, protocol: RequestProtocolResponses, allowConvert: true, conversionOn: true, wantOK: false},
		{name: "unparsable pin is unusable", native: both, pin: "grpc", protocol: RequestProtocolCompatible, allowConvert: true, conversionOn: true, wantOK: false},
		{name: "messages pin on a chat request needs conversion", native: msgs, pin: NativeProtocolMessages, protocol: RequestProtocolCompatible, allowConvert: true, conversionOn: true, wantProto: NativeProtocolMessages, wantOK: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			acct := &ent.UpstreamAccount{
				NativeProtocols: tt.native,
				AllowConversion: tt.allowConvert,
			}
			got, ok := AccountUpstreamProtocolForModel(acct, tt.pin, tt.protocol, tt.conversionOn)
			if ok != tt.wantOK {
				t.Fatalf("native=%v pin=%q protocol=%s ok=%v want=%v", tt.native, tt.pin, tt.protocol, ok, tt.wantOK)
			}
			if ok && got != tt.wantProto {
				t.Fatalf("native=%v pin=%q protocol=%s proto=%s want=%s", tt.native, tt.pin, tt.protocol, got, tt.wantProto)
			}
		})
	}
}
