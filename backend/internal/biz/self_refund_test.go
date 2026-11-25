package biz

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/jucode/jucode/internal/data/ent"
	entbalancetx "github.com/jucode/jucode/internal/data/ent/balancetx"
	entplan "github.com/jucode/jucode/internal/data/ent/plan"
	entsiteorder "github.com/jucode/jucode/internal/data/ent/siteorder"
	entsiterefund "github.com/jucode/jucode/internal/data/ent/siterefund"
	entuserplan "github.com/jucode/jucode/internal/data/ent/userplan"
)

// fakeRefunds answers every refund with outcome and records the calls.
type fakeRefunds struct {
	outcome RefundOutcome
	err     error
	query   RefundOutcome
	calls   []RefundRequest
}

func (f *fakeRefunds) Refund(_ context.Context, in RefundRequest) (RefundOutcome, error) {
	f.calls = append(f.calls, in)
	return f.outcome, f.err
}

func (f *fakeRefunds) QueryRefund(context.Context, string, string) (RefundOutcome, error) {
	return f.query, nil
}

type refundFixture struct {
	t       *testing.T
	finance *FinanceSvc
	client  *ent.Client
	user    *ent.User
	balance decimal.Decimal
	n       int
}

func newRefundFixture(t *testing.T) *refundFixture {
	finance, _, _, settings, client := couponTestEnv(t)
	st, err := settings.GetSite(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	st.SelfRefundEnabled = true
	settings.storeSite(st)
	return &refundFixture{t: t, finance: finance, client: client, user: newInviteTestUser(t, client)}
}

func (f *refundFixture) tx(kind entbalancetx.Kind, amount, note string) {
	f.t.Helper()
	f.n++
	f.balance = f.balance.Add(decimal.RequireFromString(amount))
	ctx := context.Background()
	if _, err := f.client.BalanceTx.Create().
		SetUserID(f.user.ID).SetKind(kind).SetAmount(decimal.RequireFromString(amount)).
		SetBalanceAfter(f.balance).SetNote(note).SetCreatedAt(time.Now().Add(time.Duration(f.n-1000) * time.Minute)).
		Save(ctx); err != nil {
		f.t.Fatal(err)
	}
	if err := f.client.User.UpdateOneID(f.user.ID).SetBalance(f.balance).Exec(ctx); err != nil {
		f.t.Fatal(err)
	}
}

// order records a paid balance order and its top-up.
func (f *refundFixture) order(channel, amount string, daysAgo int) *ent.SiteOrder {
	f.t.Helper()
	paid := time.Now().AddDate(0, 0, -daysAgo)
	o, err := f.client.SiteOrder.Create().
		SetOrderNo("SO" + uuid.NewString()[:8]).SetUserID(f.user.ID).
		SetKind(entsiteorder.KindBalance).SetStatus(entsiteorder.StatusPaid).
		SetAmount(decimal.RequireFromString(amount)).SetCreditAmount(decimal.RequireFromString(amount)).
		SetPaymentChannel(channel).SetPaidAt(paid).
		Save(context.Background())
	if err != nil {
		f.t.Fatal(err)
	}
	f.tx(entbalancetx.KindTopup, amount, siteOrderTopupNote+o.OrderNo)
	return o
}

func (f *refundFixture) reload() decimal.Decimal {
	f.t.Helper()
	u, err := f.client.User.Get(context.Background(), f.user.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	f.balance = u.Balance
	return u.Balance
}

func eq(t *testing.T, what string, got decimal.Decimal, want string) {
	t.Helper()
	if !got.Equal(decimal.RequireFromString(want)) {
		t.Errorf("%s = %s, want %s", what, got, want)
	}
}

func TestSelfRefundGoesBackThroughAlipayNewestFirst(t *testing.T) {
	f := newRefundFixture(t)
	ctx := context.Background()
	old := f.order("alipay", "100", 30)
	recent := f.order("alipay", "50", 2)
	f.order("manual", "30", 1)
	f.tx(entbalancetx.KindAdjust, "20", "")
	f.tx(entbalancetx.KindInviteReward, "5", "")
	f.tx(entbalancetx.KindDeduct, "-45", "") // drains gift 5, manual 20, then 20 paid
	// balance 160: paid 160 (180 - 20 spent), nothing else left

	gw := &fakeRefunds{outcome: RefundOutcome{Refunded: true}}
	f.finance.SetRefundGateway("alipay", gw)
	res, err := f.finance.ExecuteSelfRefund(ctx, f.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(gw.calls) != 2 || gw.calls[0].OrderNo != recent.OrderNo || gw.calls[1].OrderNo != old.OrderNo {
		t.Fatalf("alipay calls = %+v, want recent then old", gw.calls)
	}
	if gw.calls[0].AmountCents != 5000 || gw.calls[1].AmountCents != 10000 {
		t.Errorf("amounts = %d, %d", gw.calls[0].AmountCents, gw.calls[1].AmountCents)
	}
	eq(t, "balance after", f.reload(), "10")
	eq(t, "unsupported", res.Unsupported, "10") // the manually paid money that is left
	n, _ := f.client.SiteRefund.Query().Where(entsiterefund.StatusEQ(entsiterefund.StatusApproved), entsiterefund.UserIDEQ(f.user.ID)).Count(ctx)
	if n != 2 {
		t.Errorf("approved refunds = %d, want 2", n)
	}
	got, _ := f.client.SiteOrder.Get(ctx, recent.ID)
	if got.Status != entsiteorder.StatusRefunded {
		t.Errorf("recent order status = %s", got.Status)
	}

	// A second run finds nothing more to send to Alipay.
	gw.calls = nil
	if _, err := f.finance.ExecuteSelfRefund(ctx, f.user.ID); err != nil {
		t.Fatal(err)
	}
	if len(gw.calls) != 0 {
		t.Errorf("second run called alipay %d times", len(gw.calls))
	}
}

func TestSelfRefundRefusedByAlipayPutsBalanceBack(t *testing.T) {
	f := newRefundFixture(t)
	ctx := context.Background()
	f.order("alipay", "40", 3)
	f.finance.SetRefundGateway("alipay", &fakeRefunds{outcome: RefundOutcome{Rejected: true, Detail: "交易已超过退款期限"}})
	res, err := f.finance.ExecuteSelfRefund(ctx, f.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, "balance", f.reload(), "40")
	eq(t, "unsupported", res.Unsupported, "40")
	split, err := f.finance.BalanceSplitFor(ctx, &ent.User{ID: f.user.ID, Balance: f.balance})
	if err != nil {
		t.Fatal(err)
	}
	eq(t, "refundable after reversal", split.Refundable, "40")
}

func TestSelfRefundUnknownOutcomeKeepsBalanceReserved(t *testing.T) {
	f := newRefundFixture(t)
	ctx := context.Background()
	f.order("alipay", "40", 3)
	gw := &fakeRefunds{err: errors.New("timeout")}
	f.finance.SetRefundGateway("alipay", gw)
	if _, err := f.finance.ExecuteSelfRefund(ctx, f.user.ID); err != nil {
		t.Fatal(err)
	}
	eq(t, "balance while unknown", f.reload(), "0")

	// Still unknown: the next run refuses to plan anything new.
	if _, err := f.finance.ExecuteSelfRefund(ctx, f.user.ID); !errors.Is(err, ErrSelfRefundUnresolved) {
		t.Fatalf("err = %v, want ErrSelfRefundUnresolved", err)
	}
	// Alipay confirms it: the run books it and moves on.
	gw.query = RefundOutcome{Refunded: true}
	gw.calls = nil
	if _, err := f.finance.ExecuteSelfRefund(ctx, f.user.ID); err != nil {
		t.Fatal(err)
	}
	if len(gw.calls) != 0 {
		t.Errorf("refunded twice: %+v", gw.calls)
	}
	eq(t, "balance", f.reload(), "0")
	r, err := f.client.SiteRefund.Query().Where(entsiterefund.UserIDEQ(f.user.ID)).Only(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != entsiterefund.StatusApproved {
		t.Errorf("refund status = %s", r.Status)
	}
	// Staff cannot approve or reject it by hand.
	if _, err := f.finance.RejectRefund(ctx, r.ID, f.user.ID, ""); err == nil {
		t.Error("reject of a settled self-service refund succeeded")
	}
}

func TestSelfRefundNeverRefundsSpentMoney(t *testing.T) {
	f := newRefundFixture(t)
	ctx := context.Background()
	o := f.order("alipay", "40", 3)
	f.finance.SetRefundGateway("alipay", &fakeRefunds{outcome: RefundOutcome{Refunded: true}})
	plan, err := f.finance.PlanSelfRefund(ctx, f.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The user spends 15 between planning and the refund.
	f.tx(entbalancetx.KindDeduct, "-15", "")
	keep := plan.balance.Sub(plan.Refundable)
	if _, err := f.finance.reserveSelfRefund(ctx, plan.Orders[0], keep); !errors.Is(err, errRefundBalanceGone) {
		t.Fatalf("err = %v, want errRefundBalanceGone", err)
	}
	// A fresh run refunds only what is left.
	gw := &fakeRefunds{outcome: RefundOutcome{Refunded: true}}
	f.finance.SetRefundGateway("alipay", gw)
	if _, err := f.finance.ExecuteSelfRefund(ctx, f.user.ID); err != nil {
		t.Fatal(err)
	}
	if len(gw.calls) != 1 || gw.calls[0].OrderNo != o.OrderNo || gw.calls[0].AmountCents != 2500 {
		t.Fatalf("calls = %+v, want one refund of 25.00", gw.calls)
	}
	eq(t, "balance", f.reload(), "0")
}

func TestSelfRefundDisabled(t *testing.T) {
	finance, _, _, _, client := couponTestEnv(t)
	finance.SetRefundGateway("alipay", &fakeRefunds{})
	u := newInviteTestUser(t, client)
	if _, err := finance.ExecuteSelfRefund(context.Background(), u.ID); !errors.Is(err, ErrSelfRefundDisabled) {
		t.Fatalf("err = %v, want ErrSelfRefundDisabled", err)
	}
}

func (f *refundFixture) request(o *ent.SiteOrder, amount string) *ent.SiteRefund {
	f.t.Helper()
	a := decimal.RequireFromString(amount)
	r, err := f.finance.CreateRefund(context.Background(), CreateRefundInput{OrderID: o.ID, Amount: &a, Reason: "test"})
	if err != nil {
		f.t.Fatal(err)
	}
	return r
}

func TestApproveRefundOnAlipayOrderGoesBackThroughAlipay(t *testing.T) {
	f := newRefundFixture(t)
	ctx := context.Background()
	o := f.order("alipay", "30", 3)
	gw := &fakeRefunds{outcome: RefundOutcome{Refunded: true}}
	f.finance.SetRefundGateway("alipay", gw)
	r := f.request(o, "30")
	got, err := f.finance.ApproveRefund(ctx, r.ID, f.user.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != entsiterefund.StatusApproved {
		t.Errorf("status = %s", got.Status)
	}
	if len(gw.calls) != 1 || gw.calls[0].OrderNo != o.OrderNo || gw.calls[0].AmountCents != 3000 || gw.calls[0].RequestNo != requestNo(r.ID) {
		t.Fatalf("calls = %+v", gw.calls)
	}
	eq(t, "balance", f.reload(), "0")
}

func TestApproveRefundRefusesSpentMoney(t *testing.T) {
	f := newRefundFixture(t)
	ctx := context.Background()
	o := f.order("alipay", "30", 3)
	f.tx(entbalancetx.KindInviteReward, "50", "")
	f.tx(entbalancetx.KindDeduct, "-60", "") // gift 50 gone, 10 of the 30 paid gone
	gw := &fakeRefunds{outcome: RefundOutcome{Refunded: true}}
	f.finance.SetRefundGateway("alipay", gw)
	r := f.request(o, "30")
	if _, err := f.finance.ApproveRefund(ctx, r.ID, f.user.ID, ""); !errors.Is(err, errRefundBalanceGone) {
		t.Fatalf("err = %v, want errRefundBalanceGone", err)
	}
	if len(gw.calls) != 0 {
		t.Errorf("alipay called for spent money: %+v", gw.calls)
	}
	eq(t, "balance untouched", f.reload(), "20")
}

func TestApproveRefundUnknownThenConfirmed(t *testing.T) {
	f := newRefundFixture(t)
	ctx := context.Background()
	o := f.order("alipay", "30", 3)
	gw := &fakeRefunds{err: errors.New("timeout")}
	f.finance.SetRefundGateway("alipay", gw)
	r := f.request(o, "30")
	if _, err := f.finance.ApproveRefund(ctx, r.ID, f.user.ID, ""); !errors.Is(err, ErrRefundOutcomeUnknown) {
		t.Fatalf("err = %v, want ErrRefundOutcomeUnknown", err)
	}
	eq(t, "balance reserved", f.reload(), "0")
	if _, err := f.finance.RejectRefund(ctx, r.ID, f.user.ID, ""); !errors.Is(err, ErrRefundManaged) {
		t.Fatalf("reject err = %v, want ErrRefundManaged", err)
	}
	gw.query = RefundOutcome{Refunded: true}
	gw.calls = nil
	got, err := f.finance.ApproveRefund(ctx, r.ID, f.user.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != entsiterefund.StatusApproved || len(gw.calls) != 0 {
		t.Errorf("status %s, refund calls %d", got.Status, len(gw.calls))
	}
	eq(t, "balance", f.reload(), "0")
}

func TestApproveRefundRejectedByAlipay(t *testing.T) {
	f := newRefundFixture(t)
	ctx := context.Background()
	o := f.order("alipay", "30", 3)
	f.finance.SetRefundGateway("alipay", &fakeRefunds{outcome: RefundOutcome{Rejected: true, Detail: "交易状态不合法"}})
	r := f.request(o, "30")
	if _, err := f.finance.ApproveRefund(ctx, r.ID, f.user.ID, ""); err == nil {
		t.Fatal("want an error")
	}
	eq(t, "balance back", f.reload(), "30")
	got, _ := f.client.SiteRefund.Get(ctx, r.ID)
	if got.Status != entsiterefund.StatusRejected {
		t.Errorf("status = %s", got.Status)
	}
}

func TestApproveRefundOnOtherChannelIsBookedOnly(t *testing.T) {
	f := newRefundFixture(t)
	ctx := context.Background()
	o := f.order("manual", "30", 3)
	gw := &fakeRefunds{outcome: RefundOutcome{Refunded: true}}
	f.finance.SetRefundGateway("alipay", gw)
	r := f.request(o, "30")
	if _, err := f.finance.ApproveRefund(ctx, r.ID, f.user.ID, ""); err != nil {
		t.Fatal(err)
	}
	if len(gw.calls) != 0 {
		t.Errorf("alipay called for a manually paid order")
	}
	eq(t, "balance", f.reload(), "0")
}

func TestRefundDefaultsToWhatIsRefundable(t *testing.T) {
	f := newRefundFixture(t)
	ctx := context.Background()
	o := f.order("manual", "30", 3)
	f.tx(entbalancetx.KindDeduct, "-12", "")
	ref, err := f.finance.OrderRefundable(ctx, o.ID)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, "order refundable", ref.Cash, "18")
	r, err := f.finance.CreateRefund(ctx, CreateRefundInput{OrderID: o.ID, Reason: "test"})
	if err != nil {
		t.Fatal(err)
	}
	eq(t, "default amount", r.Amount, "18")

	// An amount typed by hand above that is refused at approval.
	if _, err := f.finance.RejectRefund(ctx, r.ID, f.user.ID, ""); err != nil {
		t.Fatal(err)
	}
	r2 := f.request(o, "30")
	if _, err := f.finance.ApproveRefund(ctx, r2.ID, f.user.ID, ""); !errors.Is(err, errRefundBalanceGone) {
		t.Fatalf("err = %v, want errRefundBalanceGone", err)
	}
	eq(t, "balance untouched", f.reload(), "18")

	f.tx(entbalancetx.KindDeduct, "-18", "")
	if _, err := f.finance.RejectRefund(ctx, r2.ID, f.user.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := f.finance.CreateRefund(ctx, CreateRefundInput{OrderID: o.ID, Reason: "test"}); !errors.Is(err, ErrNothingRefundable) {
		t.Fatalf("err = %v, want ErrNothingRefundable", err)
	}
}

// subscription records a paid Coding Plan that started 10 days ago and
// runs 30 days.
func (f *refundFixture) subscription(channel, amount string) (*ent.SiteOrder, *ent.UserPlan) {
	f.t.Helper()
	ctx := context.Background()
	p, err := f.client.Plan.Create().
		SetName("Coding " + uuid.NewString()[:6]).
		SetPrice(decimal.RequireFromString(amount)).
		SetType(entplan.TypeQuotaSubscription).
		SetPeriodDays(30).
		Save(ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	day := 24 * time.Hour
	start := time.Now().Add(-10 * day)
	up, err := f.client.UserPlan.Create().
		SetUserID(f.user.ID).SetPlanID(p.ID).
		SetStartAt(start).SetExpireAt(start.Add(30 * day)).
		Save(ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	o, err := f.client.SiteOrder.Create().
		SetOrderNo("SO" + uuid.NewString()[:8]).SetUserID(f.user.ID).
		SetKind(entsiteorder.KindPlan).SetStatus(entsiteorder.StatusPaid).
		SetAmount(decimal.RequireFromString(amount)).
		SetPlanID(p.ID).SetUserPlanID(up.ID).
		SetPaymentChannel(channel).SetPaidAt(start).
		Save(ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	return o, up
}

func TestSelfRefundsCodingPlanForTimeLeft(t *testing.T) {
	f := newRefundFixture(t)
	ctx := context.Background()
	ali, aliPlan := f.subscription("alipay", "30")
	other, otherPlan := f.subscription("manual", "60")
	gw := &fakeRefunds{outcome: RefundOutcome{Refunded: true}}
	f.finance.SetRefundGateway("alipay", gw)

	plan, err := f.finance.PlanSelfRefund(ctx, f.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Plans) != 2 {
		t.Fatalf("plans = %+v", plan.Plans)
	}
	for _, p := range plan.Plans {
		// 20 of 30 days left; rounding down may shave a cent.
		want := decimal.RequireFromString("20")
		if p.OrderID == other.ID {
			want = decimal.RequireFromString("40")
		}
		if d := want.Sub(p.Cash); d.IsNegative() || d.GreaterThan(decimal.New(1, -2)) {
			t.Errorf("%s cash = %s, want about %s", p.OrderNo, p.Cash, want)
		}
	}

	res, err := f.finance.ExecuteSelfRefund(ctx, f.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(gw.calls) != 1 || gw.calls[0].OrderNo != ali.OrderNo {
		t.Fatalf("alipay calls = %+v", gw.calls)
	}
	if len(res.RefundedPlans) != 1 {
		t.Fatalf("refunded plans = %d, want 1", len(res.RefundedPlans))
	}
	up, _ := f.client.UserPlan.Get(ctx, aliPlan.ID)
	if up.Status != entuserplan.StatusCanceled {
		t.Errorf("alipay plan status = %s, want canceled", up.Status)
	}
	// The manually paid plan has no refund gateway: it is left alone for staff.
	up, _ = f.client.UserPlan.Get(ctx, otherPlan.ID)
	if up.Status != entuserplan.StatusActive {
		t.Errorf("manually paid plan status = %s, want active", up.Status)
	}
	if n, _ := f.client.SiteRefund.Query().Where(entsiterefund.OrderIDEQ(other.ID)).Count(ctx); n != 0 {
		t.Errorf("manual refunds = %d, want 0", n)
	}

	// Nothing is planned twice; the manually paid plan stays listed, not automatic.
	again, err := f.finance.PlanSelfRefund(ctx, f.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Plans) != 1 || again.Plans[0].OrderID != other.ID || again.Plans[0].Auto {
		t.Errorf("plans after refund = %+v", again.Plans)
	}
}

func TestSelfRefundTakesOverPendingRequests(t *testing.T) {
	f := newRefundFixture(t)
	ctx := context.Background()
	o := f.order("alipay", "30", 3)
	req := f.request(o, "30") // a request from the old ticket flow, never approved
	gw := &fakeRefunds{outcome: RefundOutcome{Refunded: true}}
	f.finance.SetRefundGateway("alipay", gw)
	if _, err := f.finance.ExecuteSelfRefund(ctx, f.user.ID); err != nil {
		t.Fatal(err)
	}
	if len(gw.calls) != 1 || gw.calls[0].AmountCents != 3000 {
		t.Fatalf("calls = %+v", gw.calls)
	}
	old, _ := f.client.SiteRefund.Get(ctx, req.ID)
	if old.Status != entsiterefund.StatusCanceled {
		t.Errorf("old request status = %s, want canceled", old.Status)
	}
	eq(t, "balance", f.reload(), "0")
}

// A Coding Plan refund Alipay has been asked to pay, with no answer yet, is
// in flight: a self-service refund must neither cancel it nor send another
// refund for the same plan, and staff cannot reject it by hand.
func TestCodingPlanRefundInFlightIsNotRefundedAgain(t *testing.T) {
	f := newRefundFixture(t)
	ctx := context.Background()
	o, _ := f.subscription("alipay", "30")
	gw := &fakeRefunds{} // neither refunded nor rejected: outcome unknown
	f.finance.SetRefundGateway("alipay", gw)
	r := f.request(o, "20") // a refund ticket staff approves
	if _, err := f.finance.ApproveRefund(ctx, r.ID, f.user.ID, ""); !errors.Is(err, ErrRefundOutcomeUnknown) {
		t.Fatalf("approve err = %v, want ErrRefundOutcomeUnknown", err)
	}
	if len(gw.calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(gw.calls))
	}
	got, _ := f.client.SiteRefund.Get(ctx, r.ID)
	if got.Status != entsiterefund.StatusPending || !sentToGateway(got) {
		t.Fatalf("refund = %+v; want pending and sent", got)
	}

	if _, err := f.finance.ExecuteSelfRefund(ctx, f.user.ID); err != nil {
		t.Fatal(err)
	}
	if len(gw.calls) != 1 {
		t.Errorf("self-service refund sent %d more refunds", len(gw.calls)-1)
	}
	again, _ := f.client.SiteRefund.Get(ctx, r.ID)
	if again.Status != entsiterefund.StatusPending {
		t.Errorf("in-flight refund status = %s, want pending", again.Status)
	}
	if _, err := f.finance.RejectRefund(ctx, r.ID, f.user.ID, ""); !errors.Is(err, ErrRefundManaged) {
		t.Errorf("reject err = %v, want ErrRefundManaged", err)
	}
}
