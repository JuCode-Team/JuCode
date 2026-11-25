package llm

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/data/ent"
	entaccount "github.com/jucode/jucode/internal/data/ent/upstreamaccount"
)

func TestPromoteStickyWithinTopPriorityTier(t *testing.T) {
	a := uuid.New()
	b := uuid.New()
	c := uuid.New()
	providers := []biz.RouteAccount{
		{Account: &ent.UpstreamAccount{ID: a, Name: "a"}, Priority: 10},
		{Account: &ent.UpstreamAccount{ID: b, Name: "b"}, Priority: 10},
		{Account: &ent.UpstreamAccount{ID: c, Name: "c"}, Priority: 100},
	}

	got := promoteSticky(providers, b)
	if got[0].Account.ID != b || got[1].Account.ID != a || got[2].Account.ID != c {
		t.Fatalf("unexpected order: %s %s %s", got[0].Account.ID, got[1].Account.ID, got[2].Account.ID)
	}
}

func TestPromoteStickyDoesNotLiftFallbackTier(t *testing.T) {
	a := uuid.New()
	b := uuid.New()
	providers := []biz.RouteAccount{
		{Account: &ent.UpstreamAccount{ID: a, Name: "primary"}, Priority: 10},
		{Account: &ent.UpstreamAccount{ID: b, Name: "fallback"}, Priority: 100},
	}

	got := promoteSticky(providers, b)
	if got[0].Account.ID != a || got[1].Account.ID != b {
		t.Fatalf("fallback sticky crossed priority tier: %s %s", got[0].Account.ID, got[1].Account.ID)
	}
}

func TestAssignedFirstKeepsCoolingAccount(t *testing.T) {
	future := time.Now().Add(time.Minute)
	cool := uuid.New()
	other := uuid.New()
	providers := []biz.RouteAccount{
		{Account: &ent.UpstreamAccount{ID: other, Status: entaccount.StatusActive, Schedulable: true}},
		{Account: &ent.UpstreamAccount{ID: cool, Status: entaccount.StatusActive, Schedulable: true, RateLimitResetAt: &future}},
	}
	got := assignedFirst(providers, cool, "m", time.Now())
	if len(got) != 2 || got[0].Account.ID != cool || got[1].Account.ID != other {
		t.Fatalf("cooling assigned account should stay first")
	}
	dead := []biz.RouteAccount{{Account: &ent.UpstreamAccount{ID: cool, Status: entaccount.StatusDisabled, Schedulable: false}}}
	if assignedFirst(dead, cool, "m", time.Now()) != nil {
		t.Fatal("disabled account should not keep the assignment")
	}
}
