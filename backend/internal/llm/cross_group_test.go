package llm

// Pure-logic tests for cross-group retry (product rule D8=B). The
// database-backed halves — candidate loading, hold re-pricing, and the full
// gateway failover — live in cross_group_pg_test.go behind the same
// JUCODE_TEST_PG_DSN opt-in the other Postgres tests use.

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/data/ent"
	entgroup "github.com/jucode/jucode/internal/data/ent/modelgroup"
)

// 媒体不切 / key gating: image-generation traffic and keys that did not opt
// into auto-route never fail over across groups.
func TestCrossGroupRetryPermitted(t *testing.T) {
	autoKey := &ent.ApiKey{AutoRoute: true}
	cases := []struct {
		name   string
		apiKey *ent.ApiKey
		media  bool
		want   bool
	}{
		{"auto-route text request", autoKey, false, true},
		{"media request", autoKey, true, false},
		{"nil key", nil, false, false},
		{"non-auto-route key", &ent.ApiKey{AutoRoute: false}, false, false},
	}
	for _, c := range cases {
		if got := crossGroupRetryPermitted(c.apiKey, c.media); got != c.want {
			t.Errorf("%s: permitted = %v, want %v", c.name, got, c.want)
		}
	}
}

// 已写下游字节后不可切：once anything reached the client, next() refuses to
// move the request even when eligible fallback groups remain.
func TestGroupFailoverNextRefusesAfterBytesWritten(t *testing.T) {
	route := &biz.Route{
		Group: &ent.ModelGroup{ID: uuid.New(), Name: "a", BillingSource: entgroup.BillingSourceBalanceOnly},
	}
	fallback := &biz.Route{
		Group: &ent.ModelGroup{ID: uuid.New(), Name: "b", BillingSource: entgroup.BillingSourceBalanceOnly},
	}
	f := &groupFailover{
		apiKey:  &ent.ApiKey{AutoRoute: true},
		loaded:  true,
		pending: []*biz.Route{fallback},
	}
	lc := logContext{}
	if next, _ := f.next(context.Background(), route, true /*written*/, &lc); next != nil {
		t.Fatalf("failover moved to %s after bytes were written", next.Group.Name)
	}
	if len(f.pending) != 1 {
		t.Fatal("written-guard consumed a fallback candidate")
	}
}

// The media flag disables the whole failover object even when candidates
// were already loaded (defense in depth around the pure gate above).
func TestGroupFailoverAvailableRefusesMedia(t *testing.T) {
	fallback := &biz.Route{
		Group: &ent.ModelGroup{ID: uuid.New(), Name: "b", BillingSource: entgroup.BillingSourceBalanceOnly},
	}
	f := &groupFailover{
		apiKey:  &ent.ApiKey{AutoRoute: true},
		media:   true,
		loaded:  true,
		pending: []*biz.Route{fallback},
	}
	if f.available(context.Background(), nil) {
		t.Fatal("media request reported an available cross-group fallback")
	}
}

// A stopped failover (a hold top-up was rejected) stays stopped.
func TestGroupFailoverStoppedStaysStopped(t *testing.T) {
	f := &groupFailover{
		apiKey:  &ent.ApiKey{AutoRoute: true},
		loaded:  true,
		stopped: true,
		pending: []*biz.Route{{Group: &ent.ModelGroup{ID: uuid.New()}}},
	}
	if f.available(context.Background(), nil) {
		t.Fatal("stopped failover reported availability")
	}
	lc := logContext{}
	if next, _ := f.next(context.Background(), nil, false, &lc); next != nil {
		t.Fatal("stopped failover still returned a route")
	}
}

// The group-switch cap bounds one request's total cost: the attempt budget
// resets on every switch, so without this a request could walk the whole
// price ladder. Checked before candidates are consulted, so a capped
// failover leaves its pending list untouched.
func TestGroupFailoverStopsAtSwitchCap(t *testing.T) {
	route := &biz.Route{
		Group: &ent.ModelGroup{ID: uuid.New(), Name: "a", BillingSource: entgroup.BillingSourceBalanceOnly},
	}
	fallback := &biz.Route{
		Group: &ent.ModelGroup{ID: uuid.New(), Name: "b", BillingSource: entgroup.BillingSourceBalanceOnly},
	}
	f := &groupFailover{
		apiKey:   &ent.ApiKey{AutoRoute: true},
		loaded:   true,
		pending:  []*biz.Route{fallback},
		switches: maxGroupSwitches,
	}
	lc := logContext{}
	if next, _ := f.next(context.Background(), route, false, &lc); next != nil {
		t.Fatalf("failover moved to %s past the switch cap", next.Group.Name)
	}
	if len(f.pending) != 1 {
		t.Fatal("switch cap consumed a fallback candidate")
	}
}
