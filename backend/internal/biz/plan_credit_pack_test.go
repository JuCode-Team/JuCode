package biz

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/jucode/jucode/internal/data"
	"github.com/jucode/jucode/internal/data/ent"
	entgroup "github.com/jucode/jucode/internal/data/ent/modelgroup"
	entplan "github.com/jucode/jucode/internal/data/ent/plan"
)

// A credit pack ("额度套餐", plan type pay_as_you_go) only grants balance. These
// tests pin that it never acts as a subscription: it must not displace an
// active subscription, and it cannot back a key restricted to plan-only groups.
// Postgres-backed, same opt-in as billing_reserve_test.go (JUCODE_TEST_PG_DSN).

func creditPackTestPlan(t *testing.T, client *ent.Client, typ entplan.Type) *ent.Plan {
	t.Helper()
	p, err := client.Plan.Create().
		SetName("plan-" + uuid.NewString()).
		SetPrice(decimal.RequireFromString("10")).
		SetType(typ).
		Save(context.Background())
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	return p
}

func creditPackTestUserPlan(t *testing.T, client *ent.Client, userID uuid.UUID, p *ent.Plan, start time.Time) *ent.UserPlan {
	t.Helper()
	up, err := client.UserPlan.Create().
		SetUserID(userID).
		SetPlanID(p.ID).
		SetStartAt(start).
		SetExpireAt(start.Add(30 * 24 * time.Hour)).
		Save(context.Background())
	if err != nil {
		t.Fatalf("create user plan: %v", err)
	}
	return up
}

func creditPackTestGroup(t *testing.T, client *ent.Client, source entgroup.BillingSource) *ent.ModelGroup {
	t.Helper()
	g, err := client.ModelGroup.Create().
		SetName("grp-" + uuid.NewString()).
		SetBillingSource(source).
		Save(context.Background())
	if err != nil {
		t.Fatalf("create group: %v", err)
	}
	return g
}

func TestActiveUserPlanPrefersSubscriptionOverCreditPack(t *testing.T) {
	_, client := testBillingReserveEnv(t)
	ctx := context.Background()
	now := time.Now()
	sub := creditPackTestPlan(t, client, entplan.TypeQuotaSubscription)
	pack := creditPackTestPlan(t, client, entplan.TypePayAsYouGo)

	// Subscriber who bought a credit pack one day later: the subscription
	// must stay the active plan.
	both := reserveTestUser(t, client, "0")
	subUP := creditPackTestUserPlan(t, client, both.ID, sub, now.Add(-48*time.Hour))
	creditPackTestUserPlan(t, client, both.ID, pack, now.Add(-24*time.Hour))
	up, p, err := activeUserPlan(ctx, client, both.ID)
	if err != nil || up.ID != subUP.ID || !IsSubscriptionPlan(p) {
		t.Fatalf("active plan = %v / %v (err %v), want the subscription", up, p, err)
	}

	// A credit pack alone is still reported (usage page, spend limit).
	packOnly := reserveTestUser(t, client, "0")
	packUP := creditPackTestUserPlan(t, client, packOnly.ID, pack, now.Add(-time.Hour))
	up, p, err = activeUserPlan(ctx, client, packOnly.ID)
	if err != nil || up.ID != packUP.ID || IsSubscriptionPlan(p) {
		t.Fatalf("active plan = %v / %v (err %v), want the credit pack", up, p, err)
	}

	if _, _, err := activeUserPlan(ctx, client, reserveTestUser(t, client, "0").ID); !errors.Is(err, ErrNoActivePlan) {
		t.Fatalf("no plan: err = %v, want ErrNoActivePlan", err)
	}
}

func TestApiKeyPlanGroupsNeedSubscription(t *testing.T) {
	_, client := testBillingReserveEnv(t)
	ctx := context.Background()
	svc := NewApiKeySvc(data.NewWith(client, nil, nil))
	now := time.Now()
	planGroup := creditPackTestGroup(t, client, entgroup.BillingSourcePlanOnly)
	otherPlanGroup := creditPackTestGroup(t, client, entgroup.BillingSourcePlanOnly)
	balanceGroup := creditPackTestGroup(t, client, entgroup.BillingSourceBalanceOnly)

	packUser := reserveTestUser(t, client, "0")
	creditPackTestUserPlan(t, client, packUser.ID, creditPackTestPlan(t, client, entplan.TypePayAsYouGo), now.Add(-time.Hour))
	subUser := reserveTestUser(t, client, "0")
	creditPackTestUserPlan(t, client, subUser.ID, creditPackTestPlan(t, client, entplan.TypeQuotaSubscription), now.Add(-time.Hour))
	noPlanUser := reserveTestUser(t, client, "0")

	create := func(u *ent.User, groups ...uuid.UUID) (*CreateResult, error) {
		return svc.Create(ctx, u.ID, CreateInput{Name: "k", AllowedGroupIDs: groups, BillingScope: "personal"})
	}
	if _, err := create(packUser, planGroup.ID); !errors.Is(err, ErrPlanGroupsNeedSubscription) {
		t.Fatalf("credit pack + plan group: err = %v, want ErrPlanGroupsNeedSubscription", err)
	}
	if _, err := create(noPlanUser, planGroup.ID); !errors.Is(err, ErrPlanGroupsNeedSubscription) {
		t.Fatalf("no plan + plan group: err = %v, want ErrPlanGroupsNeedSubscription", err)
	}
	if _, err := create(packUser, balanceGroup.ID); err != nil {
		t.Fatalf("credit pack + balance group: %v", err)
	}
	if _, err := create(subUser, planGroup.ID); err != nil {
		t.Fatalf("subscription + plan group: %v", err)
	}

	// A key that already carries a plan group (made before this rule) stays
	// editable; only newly added plan groups are checked.
	legacy, err := client.ApiKey.Create().
		SetUserID(packUser.ID).
		SetName("legacy").
		SetHash(uuid.NewString()).
		SetPrefix("sk-test").
		SetAllowedGroupIds([]uuid.UUID{planGroup.ID}).
		Save(ctx)
	if err != nil {
		t.Fatalf("create legacy key: %v", err)
	}
	scope := ApiKeyScope{UserID: packUser.ID}
	keep := []uuid.UUID{planGroup.ID, balanceGroup.ID}
	if _, err := svc.UpdateScoped(ctx, scope, legacy.ID, UpdateInput{AllowedGroupIDs: &keep}); err != nil {
		t.Fatalf("keeping an existing plan group: %v", err)
	}
	add := []uuid.UUID{planGroup.ID, otherPlanGroup.ID}
	if _, err := svc.UpdateScoped(ctx, scope, legacy.ID, UpdateInput{AllowedGroupIDs: &add}); !errors.Is(err, ErrPlanGroupsNeedSubscription) {
		t.Fatalf("adding a plan group: err = %v, want ErrPlanGroupsNeedSubscription", err)
	}
	if _, err := svc.Update(ctx, packUser.ID, legacy.ID, UpdateInput{AllowedGroupIDs: &add}); !errors.Is(err, ErrPlanGroupsNeedSubscription) {
		t.Fatalf("admin path adding a plan group: err = %v, want ErrPlanGroupsNeedSubscription", err)
	}
}
