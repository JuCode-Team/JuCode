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
	entagententry "github.com/jucode/jucode/internal/data/ent/agentsettlemententry"
	entrebate "github.com/jucode/jucode/internal/data/ent/inviterebate"
	entplan "github.com/jucode/jucode/internal/data/ent/plan"
	entsiteorder "github.com/jucode/jucode/internal/data/ent/siteorder"
)

func TestNormalizeCouponCode(t *testing.T) {
	got, err := normalizeCouponCode(" c-partner-01 ")
	if err != nil {
		t.Fatal(err)
	}
	if got != "C-PARTNER-01" {
		t.Fatalf("normalized code = %q, want C-PARTNER-01", got)
	}
	for _, code := range []string{"PARTNER", "C-A", "C-BAD_01", "C-BAD-", "P-OLD-CODE"} {
		if _, err := normalizeCouponCode(code); !errors.Is(err, ErrCouponCodeMalformed) {
			t.Errorf("normalizeCouponCode(%q) error = %v, want ErrCouponCodeMalformed", code, err)
		}
	}
}

func TestCouponRefundAdjustmentsConvergeToFullRebate(t *testing.T) {
	rate := decimal.NewFromInt(10)
	original := decimal.NewFromInt(9)
	reversed := decimal.Zero
	for _, cumulativeRefund := range []decimal.Decimal{
		decimal.RequireFromString("33.33"),
		decimal.RequireFromString("66.66"),
		decimal.RequireFromString("90.00"),
	} {
		reversed = reversed.Add(couponRefundAdjustment(cumulativeRefund, rate, original, reversed))
	}
	if !reversed.Equal(original) {
		t.Fatalf("reversed = %s, want %s", reversed, original)
	}
}

// couponTestEnv wires finance, agents and invites against one Postgres
// client, the way main does.
func couponTestEnv(t *testing.T) (*FinanceSvc, *AgentSvc, *InviteSvc, *SettingsSvc, *ent.Client) {
	t.Helper()
	invites, settings, client := testInviteEnv(t)
	d := data.NewWith(client, nil, invites.d.DB())
	finance := NewFinanceSvc(d, settings)
	agents := NewAgentSvc(d, settings)
	finance.SetAgents(agents)
	finance.SetInvites(invites)
	setMaxIncentive(t, settings, 30)
	return finance, agents, invites, settings, client
}

func setMaxIncentive(t *testing.T, settings *SettingsSvc, pct float64) {
	t.Helper()
	st, err := settings.GetSite(context.Background())
	if err != nil {
		t.Fatalf("get site: %v", err)
	}
	st.MaxIncentivePct = pct
	settings.storeSite(st)
}

// newCouponAgent creates an agent at the full default budget with a coupon
// giving discount percent off.
func newCouponAgent(t *testing.T, agents *AgentSvc, client *ent.Client, discount int64) (*ent.User, string) {
	t.Helper()
	ctx := context.Background()
	agent := newInviteTestUser(t, client)
	p, err := agents.CreateCoupon(ctx, agent.ID, "C-"+uuid.NewString()[:8])
	if err != nil {
		t.Fatalf("create coupon: %v", err)
	}
	if _, err := agents.SetCouponDiscount(ctx, agent.ID, decimal.NewFromInt(discount)); err != nil {
		t.Fatalf("set discount: %v", err)
	}
	return agent, *p.CouponCode
}

func couponEntry(t *testing.T, client *ent.Client, orderID uuid.UUID, kind entagententry.Kind) *ent.AgentSettlementEntry {
	t.Helper()
	e, err := client.AgentSettlementEntry.Query().
		Where(entagententry.OrderIDEQ(orderID), entagententry.KindEQ(kind)).
		Only(context.Background())
	if err != nil {
		t.Fatalf("load %s entry: %v", kind, err)
	}
	return e
}

func TestCouponOrderRebateAndRefundLedger(t *testing.T) {
	finance, agents, _, _, client := couponTestEnv(t)
	ctx := context.Background()
	agent, code := newCouponAgent(t, agents, client, 10)
	buyer := newInviteTestUser(t, client)

	if _, err := agents.QuoteCoupon(ctx, agent.ID, code); !errors.Is(err, ErrCouponSelfUse) {
		t.Fatalf("self quote error = %v, want ErrCouponSelfUse", err)
	}
	terms, err := agents.QuoteCoupon(ctx, buyer.ID, " "+code+" ")
	if err != nil {
		t.Fatalf("quote: %v", err)
	}
	if !terms.DiscountPct.Equal(decimal.NewFromInt(10)) || !terms.RebatePct.Equal(decimal.NewFromInt(20)) {
		t.Fatalf("terms = %s/%s, want 10/20", terms.DiscountPct, terms.RebatePct)
	}

	order, err := finance.CreateOrder(ctx, CreateOrderInput{
		UserID: buyer.ID, Kind: string(entsiteorder.KindBalance),
		Amount: decimal.NewFromInt(100), Currency: CurrencyCNY, CouponCode: code,
	})
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	if !order.Amount.Equal(decimal.NewFromInt(90)) || !order.CreditAmount.Equal(decimal.NewFromInt(100)) {
		t.Fatalf("order amount/credit = %s/%s, want 90/100", order.Amount, order.CreditAmount)
	}
	if _, err := finance.MarkOrderPaid(ctx, order.ID); err != nil {
		t.Fatalf("pay: %v", err)
	}
	// 20% of the 90 paid: the platform owes the agent 18.
	rebate := couponEntry(t, client, order.ID, entagententry.KindCouponRebate)
	if !rebate.PlatformPayable.Equal(decimal.NewFromInt(-18)) || rebate.AgentID != agent.ID {
		t.Fatalf("rebate payable = %s agent %s, want -18 for %s", rebate.PlatformPayable, rebate.AgentID, agent.ID)
	}

	refundAmount := decimal.NewFromInt(90)
	refund, err := finance.CreateRefund(ctx, CreateRefundInput{OrderID: order.ID, Amount: &refundAmount, Reason: "test"})
	if err != nil {
		t.Fatalf("create refund: %v", err)
	}
	if _, err := finance.ApproveRefund(ctx, refund.ID, agent.ID, "test"); err != nil {
		t.Fatalf("approve refund: %v", err)
	}
	reversal := couponEntry(t, client, order.ID, entagententry.KindCouponRefund)
	if !reversal.PlatformPayable.Equal(decimal.NewFromInt(18)) {
		t.Fatalf("reversal payable = %s, want 18", reversal.PlatformPayable)
	}

	plan, err := client.Plan.Create().
		SetName("coupon-plan-" + uuid.NewString()).
		SetPrice(decimal.NewFromInt(200)).
		SetType(entplan.TypeQuotaSubscription).
		SetPeriodDays(30).
		Save(ctx)
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	planOrder, err := finance.CreateOrder(ctx, CreateOrderInput{
		UserID: buyer.ID, Kind: string(entsiteorder.KindPlan), PlanID: &plan.ID,
		Amount: plan.Price, Currency: CurrencyCNY, CouponCode: code,
	})
	if err != nil {
		t.Fatalf("create plan order: %v", err)
	}
	if _, err := finance.MarkOrderPaid(ctx, planOrder.ID); err != nil {
		t.Fatalf("pay plan order: %v", err)
	}
	if got := couponEntry(t, client, planOrder.ID, entagententry.KindCouponRebate).PlatformPayable; !got.Equal(decimal.NewFromInt(-36)) {
		t.Fatalf("plan rebate payable = %s, want -36", got)
	}

	sum, err := agents.PendingSummary(ctx, agent.ID)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if !sum.TotalPayable.Equal(decimal.NewFromInt(-36)) || !sum.CouponRebates.Equal(decimal.NewFromInt(36)) {
		t.Fatalf("summary total/rebates = %s/%s, want -36/36", sum.TotalPayable, sum.CouponRebates)
	}
	if buyers, err := agents.CouponBuyerCount(ctx, agent.ID); err != nil || buyers != 1 {
		t.Fatalf("buyers = %d, err = %v, want 1", buyers, err)
	}
}

// One order pays one referrer: a coupon order skips the inviter's topup
// rebate and does not count toward the signup-reward topup total.
func TestCouponOrderExcludesInviteRebate(t *testing.T) {
	finance, agents, invites, settings, client := couponTestEnv(t)
	ctx := context.Background()
	p := permissiveProgram()
	p.TopupRebateEnabled = true
	p.TopupRebateRatePct = decimal.NewFromInt(10)
	setInviteProgram(t, settings, p)

	_, code := newCouponAgent(t, agents, client, 5)
	_, inviteCode := inviterWithCode(t, invites, client)
	buyer := newInviteTestUser(t, client)
	if err := invites.BindOnRegister(ctx, buyer.ID, inviteCode, "", ""); err != nil {
		t.Fatalf("bind: %v", err)
	}

	pay := func(coupon string) *ent.SiteOrder {
		t.Helper()
		o, err := finance.CreateOrder(ctx, CreateOrderInput{
			UserID: buyer.ID, Kind: string(entsiteorder.KindBalance),
			Amount: decimal.NewFromInt(100), Currency: CurrencyCNY, CouponCode: coupon,
		})
		if err != nil {
			t.Fatalf("create order: %v", err)
		}
		if _, err := finance.MarkOrderPaid(ctx, o.ID); err != nil {
			t.Fatalf("pay: %v", err)
		}
		return o
	}
	couponOrder := pay(code)
	plainOrder := pay("")

	deadline := time.Now().Add(10 * time.Second)
	for {
		n, err := client.InviteRebate.Query().Where(entrebate.SourceOrderIDEQ(plainOrder.ID)).Count(ctx)
		if err != nil {
			t.Fatalf("query rebate: %v", err)
		}
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("plain order never produced an invite rebate")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if n, err := client.InviteRebate.Query().Where(entrebate.SourceOrderIDEQ(couponOrder.ID)).Count(ctx); err != nil || n != 0 {
		t.Fatalf("coupon order invite rebates = %d, err = %v, want 0", n, err)
	}
	total, err := invites.inviteePaidTotal(ctx, buyer.ID)
	if err != nil {
		t.Fatalf("topup total: %v", err)
	}
	if !total.Equal(decimal.NewFromInt(100)) {
		t.Fatalf("topup total = %s, want 100 (plain order only)", total)
	}
}

func TestIncentiveCapBoundsAgentBudget(t *testing.T) {
	_, agents, _, settings, client := couponTestEnv(t)
	ctx := context.Background()
	agent, code := newCouponAgent(t, agents, client, 25)
	buyer := newInviteTestUser(t, client)

	if _, err := agents.UpdateSharePct(ctx, agent.ID, decimal.NewFromInt(60)); err == nil {
		t.Fatal("share below 100 - max incentive was accepted")
	}
	if _, err := agents.SetCouponDiscount(ctx, agent.ID, decimal.NewFromInt(31)); err == nil {
		t.Fatal("discount above budget was accepted")
	}

	// Lowering the site cap shrinks existing budgets without touching rows.
	setMaxIncentive(t, settings, 20)
	terms, err := agents.QuoteCoupon(ctx, buyer.ID, code)
	if err != nil {
		t.Fatalf("quote: %v", err)
	}
	if !terms.DiscountPct.Equal(decimal.NewFromInt(20)) || !terms.RebatePct.IsZero() {
		t.Fatalf("capped terms = %s/%s, want 20/0", terms.DiscountPct, terms.RebatePct)
	}
	p, err := agents.EnsureProfile(ctx, agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	share, err := agents.EffectiveShare(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if !share.Equal(decimal.NewFromInt(80)) {
		t.Fatalf("effective share = %s, want 80", share)
	}

	// Raising the share shrinks the budget and pulls the discount down with it.
	p, err = agents.UpdateSharePct(ctx, agent.ID, decimal.NewFromInt(90))
	if err != nil {
		t.Fatalf("update share: %v", err)
	}
	if !p.CouponDiscountPct.Equal(decimal.NewFromInt(10)) {
		t.Fatalf("discount after share raise = %s, want 10", p.CouponDiscountPct)
	}
}

func TestUpdateSiteRejectsIncentiveOverCap(t *testing.T) {
	_, settings, _ := testInviteEnv(t)
	ctx := context.Background()
	st, err := settings.GetSite(ctx)
	if err != nil {
		t.Fatal(err)
	}
	in := *st
	in.MaxIncentivePct = 0
	if err := settings.UpdateSite(ctx, in); !errors.Is(err, ErrInvalidSiteSettings) {
		t.Fatalf("max 0 error = %v, want ErrInvalidSiteSettings", err)
	}
	p := permissiveProgram()
	p.TopupRebateRatePct = decimal.NewFromInt(40)
	in.MaxIncentivePct = 30
	in.InviteProgram = &p
	if err := settings.UpdateSite(ctx, in); !errors.Is(err, ErrInvalidSiteSettings) {
		t.Fatalf("invite rate over cap error = %v, want ErrInvalidSiteSettings", err)
	}
}

// grantExclusiveGroup creates an enabled exclusive group and grants userID.
func grantExclusiveGroup(t *testing.T, client *ent.Client, userID uuid.UUID) *ent.ModelGroup {
	t.Helper()
	ctx := context.Background()
	group := client.ModelGroup.Create().
		SetName("vip-" + uuid.NewString()[:8]).
		SetBillingSource("balance_only").
		SetRateMultiplier(0.2).
		SetExclusive(true).
		SaveX(ctx)
	client.ModelGroupUserGrant.Create().SetGroupID(group.ID).SetUserID(userID).ExecX(ctx)
	return group
}

func TestCouponRejectsExclusiveGroupBuyer(t *testing.T) {
	finance, agents, _, _, client := couponTestEnv(t)
	ctx := context.Background()
	_, code := newCouponAgent(t, agents, client, 10)

	direct := newInviteTestUser(t, client)
	group := grantExclusiveGroup(t, client, direct.ID)
	if _, err := agents.QuoteCoupon(ctx, direct.ID, code); !errors.Is(err, ErrCouponExclusiveBuyer) {
		t.Fatalf("direct grant quote error = %v, want ErrCouponExclusiveBuyer", err)
	}
	if _, err := finance.CreateOrder(ctx, CreateOrderInput{
		UserID: direct.ID, Kind: string(entsiteorder.KindBalance),
		Amount: decimal.NewFromInt(100), Currency: CurrencyCNY, CouponCode: code,
	}); !errors.Is(err, ErrCouponExclusiveBuyer) {
		t.Fatalf("direct grant order error = %v, want ErrCouponExclusiveBuyer", err)
	}

	ug := client.UserGroup.Create().SetName("ug-" + uuid.NewString()[:8]).SaveX(ctx)
	member := newInviteTestUser(t, client)
	client.User.UpdateOneID(member.ID).SetUserGroupID(ug.ID).ExecX(ctx)
	client.ModelGroupUserGroupGrant.Create().SetGroupID(group.ID).SetUserGroupID(ug.ID).ExecX(ctx)
	if _, err := agents.QuoteCoupon(ctx, member.ID, code); !errors.Is(err, ErrCouponExclusiveBuyer) {
		t.Fatalf("user group grant quote error = %v, want ErrCouponExclusiveBuyer", err)
	}

	// A disabled exclusive group no longer gives the low rate, so it no
	// longer blocks the coupon.
	client.ModelGroup.UpdateOneID(group.ID).SetEnabled(false).ExecX(ctx)
	if _, err := agents.QuoteCoupon(ctx, direct.ID, code); err != nil {
		t.Fatalf("quote after group disabled: %v", err)
	}
}
