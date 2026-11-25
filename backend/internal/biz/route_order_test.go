package biz

import (
	"testing"

	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/data/ent"
	entapikey "github.com/jucode/jucode/internal/data/ent/apikey"
	entschema "github.com/jucode/jucode/internal/data/ent/schema"
)

func routeGroup(name string, rate float64) *ent.ModelGroup {
	return &ent.ModelGroup{ID: uuid.New(), Name: name, RateMultiplier: rate}
}

func names(groups []*ent.ModelGroup) []string {
	out := make([]string, len(groups))
	for i, group := range groups {
		out[i] = group.Name
	}
	return out
}

func fptr(v float64) *float64 { return &v }

func TestOrderAutoGroupsPriceUsesCacheHit(t *testing.T) {
	cheap := routeGroup("cheap", 1)
	warm := routeGroup("warm", 2)
	key := &ent.ApiKey{AutoRoute: true, AutoRouteMode: entapikey.AutoRouteModePrice}
	pref := routePref{
		HasInput: true, Input: 10, CacheRead: 0, Output: 0,
		Signals: map[uuid.UUID]GroupRouteSignal{
			warm.ID: {Hit: fptr(0.9)},
		},
	}
	got := names(orderAutoGroups([]*ent.ModelGroup{cheap, warm}, key, pref))
	if len(got) != 2 || got[0] != "warm" {
		t.Fatalf("cache-warm group should lead, got %v", got)
	}
}

func TestOrderAutoGroupsPriceCeilingKeepsPin(t *testing.T) {
	low := routeGroup("low", 1)
	high := routeGroup("high", 4)
	key := &ent.ApiKey{
		AutoRoute: true, AutoRouteMode: entapikey.AutoRouteModePrice, AutoRouteMaxRate: 2,
		ModelPins: []entschema.ModelPin{{Model: "luna", GroupID: high.ID}},
	}
	got := names(orderAutoGroups([]*ent.ModelGroup{low, high}, key, routePref{ModelName: "luna", HasInput: true, Input: 1}))
	if len(got) != 2 || got[0] != "high" || got[1] != "low" {
		t.Fatalf("pin above the ceiling should stay first, got %v", got)
	}
	without := names(orderAutoGroups([]*ent.ModelGroup{low, high}, key, routePref{ModelName: "astra", HasInput: true, Input: 1}))
	if len(without) != 1 || without[0] != "low" {
		t.Fatalf("unpinned model should drop the group above the ceiling, got %v", without)
	}
}

func TestOrderAutoGroupsStablePutsUnknownInTheMiddle(t *testing.T) {
	low := routeGroup("low", 1)
	mid := routeGroup("mid", 1)
	high := routeGroup("high", 1)
	key := &ent.ApiKey{AutoRoute: true, AutoRouteMode: entapikey.AutoRouteModeStable}
	pref := routePref{Signals: map[uuid.UUID]GroupRouteSignal{
		low.ID:  {Success: fptr(0.5)},
		high.ID: {Success: fptr(0.99)},
	}}
	got := names(orderAutoGroups([]*ent.ModelGroup{low, mid, high}, key, pref))
	if len(got) != 3 || got[0] != "high" || got[1] != "mid" || got[2] != "low" {
		t.Fatalf("unknown sample should sit between the known rates, got %v", got)
	}
}

func TestOrderAutoGroupsFastUsesTTFTAndTPS(t *testing.T) {
	slow := routeGroup("slow", 1)
	fast := routeGroup("fast", 1)
	key := &ent.ApiKey{AutoRoute: true, AutoRouteMode: entapikey.AutoRouteModeFast}
	pref := routePref{Signals: map[uuid.UUID]GroupRouteSignal{
		slow.ID: {TTFTMs: fptr(800), TPS: fptr(50)},
		fast.ID: {TTFTMs: fptr(200), TPS: fptr(50)},
	}}
	got := names(orderAutoGroups([]*ent.ModelGroup{slow, fast}, key, pref))
	if len(got) != 2 || got[0] != "fast" {
		t.Fatalf("lower TTFT at the same TPS should lead, got %v", got)
	}

	start := routeGroup("start", 1)
	decode := routeGroup("decode", 1)
	partial := routeGroup("partial", 1)
	pref = routePref{Signals: map[uuid.UUID]GroupRouteSignal{
		start.ID:   {TTFTMs: fptr(100), TPS: fptr(20)},
		decode.ID:  {TTFTMs: fptr(300), TPS: fptr(200)},
		partial.ID: {TTFTMs: fptr(150)},
	}}
	got = names(orderAutoGroups([]*ent.ModelGroup{start, partial, decode}, key, pref))
	if len(got) != 3 || got[0] != "decode" {
		t.Fatalf("faster decode should beat a quicker first token, got %v", got)
	}
}

func TestOrderAutoGroupsPinKeepsItsOwnGroup(t *testing.T) {
	pro := routeGroup("pro", 1)
	azure := routeGroup("azure", 1)
	key := &ent.ApiKey{
		AutoRoute: true,
		ModelPins: []entschema.ModelPin{{Model: "luna", GroupID: pro.ID}},
	}
	got := names(orderAutoGroups([]*ent.ModelGroup{pro, azure}, key, routePref{ModelName: "luna", Sticky: azure.ID}))
	if len(got) != 2 || got[0] != "pro" {
		t.Fatalf("a pinned model should stay on its own group, got %v", got)
	}
}

func TestOrderAutoGroupsSessionIgnoresFasterPeer(t *testing.T) {
	slow := routeGroup("slow", 1)
	fast := routeGroup("fast", 1)
	key := &ent.ApiKey{AutoRoute: true, AutoRouteMode: entapikey.AutoRouteModeFast}
	pref := routePref{
		Sticky: slow.ID,
		Signals: map[uuid.UUID]GroupRouteSignal{
			slow.ID: {TTFTMs: fptr(800), TPS: fptr(20)},
			fast.ID: {TTFTMs: fptr(100), TPS: fptr(200)},
		},
	}
	got := names(orderAutoGroups([]*ent.ModelGroup{fast, slow}, key, pref))
	if len(got) != 2 || got[0] != "slow" {
		t.Fatalf("session group should stay ahead of a faster peer, got %v", got)
	}
}

func TestOrderAutoGroupsModelStickyStaysAfterMove(t *testing.T) {
	pro := routeGroup("pro", 1)
	azure := routeGroup("azure", 1)
	key := &ent.ApiKey{
		AutoRoute: true,
		ModelPins: []entschema.ModelPin{{Model: "astra", GroupID: pro.ID}},
	}
	pref := routePref{ModelName: "astra", Sticky: pro.ID, ModelSticky: azure.ID}
	got := names(orderAutoGroups([]*ent.ModelGroup{pro, azure}, key, pref))
	if len(got) != 2 || got[0] != "azure" {
		t.Fatalf("model that left the session should stay on its group, got %v", got)
	}
}

func TestNextConversationAssignment(t *testing.T) {
	session := uuid.New()
	other := uuid.New()
	moved := uuid.New()
	if got := nextConversationAssignment(conversationAssignment{}, other, false); got.Session != other || got.Model != uuid.Nil {
		t.Fatalf("first success should assign the session, got %+v", got)
	}
	if got := nextConversationAssignment(conversationAssignment{Session: session}, session, false); got.Session != session || got.Model != uuid.Nil {
		t.Fatalf("same group should refresh the session, got %+v", got)
	}
	if got := nextConversationAssignment(conversationAssignment{Session: session}, other, false); got.Session != session || got.Model != other {
		t.Fatalf("a model outside the session group should keep its own group, got %+v", got)
	}
	if got := nextConversationAssignment(conversationAssignment{Session: session}, moved, true); got.Session != moved || got.Model != uuid.Nil {
		t.Fatalf("a serious failure should move the session, got %+v", got)
	}
	if got := nextConversationAssignment(conversationAssignment{Session: session, Model: other}, moved, true); got.Session != session || got.Model != moved {
		t.Fatalf("a serious failure of a model's own group should not move the session, got %+v", got)
	}
}

func TestSelectGroupsWithoutAutoRouteKeepsNameOrder(t *testing.T) {
	b := routeGroup("b", 0.2)
	a := routeGroup("a", 3)
	key := &ent.ApiKey{AutoRoute: false}
	got, err := selectGroups([]*ent.ModelGroup{b, a}, key, routePref{})
	if err != nil || len(got) != 1 || got[0].Name != "a" {
		t.Fatalf("auto route off should keep the first name, got %v err %v", names(got), err)
	}
}
