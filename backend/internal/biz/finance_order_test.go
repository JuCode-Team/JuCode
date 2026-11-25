package biz

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/shopspring/decimal"

	"github.com/jucode/jucode/internal/data"
	"github.com/jucode/jucode/internal/data/ent"
	entbalancetx "github.com/jucode/jucode/internal/data/ent/balancetx"
	entplan "github.com/jucode/jucode/internal/data/ent/plan"
	entsiteorder "github.com/jucode/jucode/internal/data/ent/siteorder"
	entuser "github.com/jucode/jucode/internal/data/ent/user"
	entuserplan "github.com/jucode/jucode/internal/data/ent/userplan"
)

func testFinanceEnv(t *testing.T) (*FinanceSvc, *ent.Client, *sql.DB) {
	t.Helper()
	dsn := os.Getenv("JUCODE_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("JUCODE_TEST_PG_DSN not set; skipping Postgres-backed finance tests")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	client := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	if err := client.Schema.Create(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	d := data.NewWith(client, nil, db)
	return NewFinanceSvc(d, nil), client, db
}

func newFinanceTestUser(t *testing.T, client *ent.Client) *ent.User {
	t.Helper()
	u, err := client.User.Create().
		SetEmail(uuid.NewString() + "@test.invalid").
		SetPasswordHash("x").
		SetStatus(entuser.StatusActive).
		Save(context.Background())
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	return u
}

func TestNormalizeOrderAmountToCents(t *testing.T) {
	got, err := normalizeOrderAmount(decimal.RequireFromString("1.00499999"))
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(decimal.RequireFromString("1.00")) {
		t.Fatalf("normalized amount = %s, want 1.00", got)
	}
	cents, err := (&FinanceSvc{}).paymentAmountCents(got)
	if err != nil {
		t.Fatal(err)
	}
	if cents != 100 {
		t.Fatalf("payment cents = %d, want 100", cents)
	}
}

func TestMarkOrderPaidCreditsCanonicalCents(t *testing.T) {
	svc, client, _ := testFinanceEnv(t)
	ctx := context.Background()
	u := newFinanceTestUser(t, client)

	o, err := svc.CreateOrder(ctx, CreateOrderInput{
		UserID:   u.ID,
		Kind:     string(entsiteorder.KindBalance),
		Amount:   decimal.RequireFromString("1.00499999"),
		Currency: CurrencyCNY,
	})
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	if !o.Amount.Equal(decimal.RequireFromString("1.00")) {
		t.Fatalf("stored amount = %s, want 1.00", o.Amount)
	}

	o, err = svc.MarkOrderPaid(ctx, o.ID)
	if err != nil {
		t.Fatalf("mark paid: %v", err)
	}
	if o.PaymentAmountCents != 100 || !o.Amount.Equal(decimal.RequireFromString("1.00")) {
		t.Fatalf("paid order amount = %s (%d cents), want 1.00 (100 cents)", o.Amount, o.PaymentAmountCents)
	}
	u, err = client.User.Get(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !u.Balance.Equal(decimal.RequireFromString("1.00")) {
		t.Fatalf("balance = %s, want 1.00", u.Balance)
	}
	topups, err := client.BalanceTx.Query().
		Where(
			entbalancetx.UserIDEQ(u.ID),
			entbalancetx.KindEQ(entbalancetx.KindTopup),
		).
		All(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(topups) != 1 || !topups[0].Amount.Equal(decimal.RequireFromString("1.00")) {
		t.Fatalf("topup rows = %+v, want one 1.00 row", topups)
	}
}

func TestApprovePlanRefundRevokesRemainingGift(t *testing.T) {
	svc, client, _ := testFinanceEnv(t)
	ctx := context.Background()
	u := newFinanceTestUser(t, client)
	plan, err := client.Plan.Create().
		SetName("gift-refund-" + uuid.NewString()).
		SetPrice(decimal.RequireFromString("10")).
		SetType(entplan.TypePayAsYouGo).
		SetInitialBalance(decimal.RequireFromString("100")).
		SetPeriodDays(30).
		Save(ctx)
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	o, err := svc.CreateOrder(ctx, CreateOrderInput{
		UserID:   u.ID,
		Kind:     string(entsiteorder.KindPlan),
		PlanID:   &plan.ID,
		Amount:   plan.Price,
		Currency: CurrencyCNY,
	})
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	o, err = svc.MarkOrderPaid(ctx, o.ID)
	if err != nil {
		t.Fatalf("mark paid: %v", err)
	}
	if o.UserPlanID == nil {
		t.Fatal("paid plan order has no user_plan_id")
	}

	// Spend 40 from the pooled balance. Gift-first ledger replay must leave
	// exactly 60 for the refund approval to revoke.
	tx, err := client.Tx(ctx)
	if err != nil {
		t.Fatal(err)
	}
	locked, err := tx.User.Query().Where(entuser.IDEQ(u.ID)).ForUpdate().Only(ctx)
	if err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	spend := decimal.RequireFromString("40")
	afterSpend := locked.Balance.Sub(spend)
	if err := tx.User.UpdateOneID(u.ID).SetBalance(afterSpend).Exec(ctx); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.BalanceTx.Create().
		SetUserID(u.ID).
		SetKind(entbalancetx.KindDeduct).
		SetAmount(spend.Neg()).
		SetBalanceAfter(afterSpend).
		SetCurrency(CurrencyCNY).
		SetNote("test spend").
		Exec(ctx); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	// 60 of the 100 credited is unspent, so at most 6 of the 10 paid may go
	// back; refunding more would refund money already spent.
	refundAmount := decimal.RequireFromString("6")
	refund, err := svc.CreateRefund(ctx, CreateRefundInput{
		OrderID: o.ID,
		Amount:  &refundAmount,
		Reason:  "test",
	})
	if err != nil {
		t.Fatalf("create refund: %v", err)
	}
	refund, err = svc.ApproveRefund(ctx, refund.ID, u.ID, "approved in test")
	if err != nil {
		t.Fatalf("approve refund: %v", err)
	}
	if !refund.Amount.Equal(refundAmount) {
		t.Fatalf("refund amount = %s, want paid amount %s", refund.Amount, refundAmount)
	}

	u, err = client.User.Get(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !u.Balance.IsZero() {
		t.Fatalf("balance after gift revocation = %s, want 0", u.Balance)
	}
	up, err := client.UserPlan.Get(ctx, *o.UserPlanID)
	if err != nil {
		t.Fatal(err)
	}
	if up.Status != entuserplan.StatusCanceled {
		t.Fatalf("user plan status = %s, want canceled", up.Status)
	}

	checkTx, err := client.Tx(ctx)
	if err != nil {
		t.Fatal(err)
	}
	remaining, err := remainingPlanGift(ctx, checkTx, u.ID, *o.UserPlanID)
	_ = checkTx.Rollback()
	if err != nil {
		t.Fatal(err)
	}
	if !remaining.IsZero() {
		t.Fatalf("remaining plan gift = %s, want 0", remaining)
	}
	gifts, err := client.BalanceTx.Query().
		Where(
			entbalancetx.UserIDEQ(u.ID),
			entbalancetx.KindEQ(entbalancetx.KindGift),
			entbalancetx.RelatedUserPlanIDEQ(*o.UserPlanID),
		).
		All(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(gifts) != 2 {
		t.Fatalf("gift ledger rows = %d, want grant and revocation", len(gifts))
	}
	revoked := decimal.Zero
	for _, gift := range gifts {
		if gift.Amount.Sign() < 0 {
			revoked = revoked.Add(gift.Amount.Abs())
		}
	}
	if !revoked.Equal(decimal.RequireFromString("60")) {
		t.Fatalf("revoked gift = %s, want 60", revoked)
	}
}

func TestPlanGiftReplayDoesNotTreatCashRefundAsSpend(t *testing.T) {
	svc, client, _ := testFinanceEnv(t)
	ctx := context.Background()
	u := newFinanceTestUser(t, client)

	// A separate cash topup is refunded after the plan gift lands. That
	// withdrawal must not consume the gift lot or leave cash behind when the
	// plan itself is subsequently refunded.
	cash := decimal.RequireFromString("50")
	u, err := client.User.UpdateOneID(u.ID).SetBalance(cash).Save(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.BalanceTx.Create().
		SetUserID(u.ID).
		SetKind(entbalancetx.KindTopup).
		SetAmount(cash).
		SetBalanceAfter(cash).
		SetCurrency(CurrencyCNY).
		Exec(ctx); err != nil {
		t.Fatal(err)
	}
	plan, err := client.Plan.Create().
		SetName("cash-refund-before-gift-refund-" + uuid.NewString()).
		SetPrice(decimal.RequireFromString("10")).
		SetType(entplan.TypePayAsYouGo).
		SetInitialBalance(decimal.RequireFromString("100")).
		Save(ctx)
	if err != nil {
		t.Fatal(err)
	}
	o, err := svc.CreateOrder(ctx, CreateOrderInput{
		UserID: u.ID, Kind: string(entsiteorder.KindPlan), PlanID: &plan.ID,
		Amount: plan.Price, Currency: CurrencyCNY,
	})
	if err != nil {
		t.Fatal(err)
	}
	o, err = svc.MarkOrderPaid(ctx, o.ID)
	if err != nil {
		t.Fatal(err)
	}

	u, err = client.User.Get(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	afterCashRefund := u.Balance.Sub(cash)
	if err := client.User.UpdateOneID(u.ID).SetBalance(afterCashRefund).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if err := client.BalanceTx.Create().
		SetUserID(u.ID).
		SetKind(entbalancetx.KindRefund).
		SetAmount(cash.Neg()).
		SetBalanceAfter(afterCashRefund).
		SetCurrency(CurrencyCNY).
		Exec(ctx); err != nil {
		t.Fatal(err)
	}

	refundAmount := o.Amount
	refund, err := svc.CreateRefund(ctx, CreateRefundInput{OrderID: o.ID, Amount: &refundAmount})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ApproveRefund(ctx, refund.ID, u.ID, "approved in test"); err != nil {
		t.Fatal(err)
	}
	u, err = client.User.Get(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !u.Balance.IsZero() {
		t.Fatalf("balance after cash and plan refunds = %s, want 0", u.Balance)
	}
}

func TestCancelOrderCannotOverwriteConcurrentPaid(t *testing.T) {
	svc, client, db := testFinanceEnv(t)
	ctx := context.Background()
	u := newFinanceTestUser(t, client)
	o, err := svc.CreateOrder(ctx, CreateOrderInput{
		UserID:   u.ID,
		Kind:     string(entsiteorder.KindBalance),
		Amount:   decimal.RequireFromString("10"),
		Currency: CurrencyCNY,
	})
	if err != nil {
		t.Fatalf("create order: %v", err)
	}

	// Hold the user lock so MarkOrderPaid keeps the order lock while blocked.
	// CancelOrder then queues behind that order lock instead of reading stale
	// pending state and overwriting the committed paid state.
	blocker, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Rollback() }()
	if _, err := blocker.ExecContext(ctx, `SELECT id FROM users WHERE id = $1 FOR UPDATE`, u.ID); err != nil {
		t.Fatal(err)
	}

	paidErr := make(chan error, 1)
	go func() {
		_, err := svc.MarkOrderPaid(ctx, o.ID)
		paidErr <- err
	}()
	waitForPostgresLockWaiters(t, db, 1)

	cancelErr := make(chan error, 1)
	go func() {
		_, err := svc.CancelOrder(ctx, o.ID)
		cancelErr <- err
	}()
	waitForPostgresLockWaiters(t, db, 2)

	if err := blocker.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-paidErr; err != nil {
		t.Fatalf("mark paid: %v", err)
	}
	if err := <-cancelErr; !errors.Is(err, ErrOrderInvalidState) {
		t.Fatalf("cancel error = %v, want ErrOrderInvalidState", err)
	}
	o, err = client.SiteOrder.Get(ctx, o.ID)
	if err != nil {
		t.Fatal(err)
	}
	if o.Status != entsiteorder.StatusPaid {
		t.Fatalf("final order status = %s, want paid", o.Status)
	}
}

func waitForPostgresLockWaiters(t *testing.T, db *sql.DB, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var got int
		err := db.QueryRow(`
			SELECT COUNT(*)
			FROM pg_stat_activity
			WHERE datname = current_database()
			  AND pid <> pg_backend_pid()
			  AND state = 'active'
			  AND wait_event_type = 'Lock'`).Scan(&got)
		if err != nil {
			t.Fatalf("query lock waiters: %v", err)
		}
		if got >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d PostgreSQL lock waiters", want)
}
