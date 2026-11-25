package biz

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/shopspring/decimal"

	"github.com/jucode/jucode/internal/data"
	"github.com/jucode/jucode/internal/data/ent"
	entapikey "github.com/jucode/jucode/internal/data/ent/apikey"
	entbalancetx "github.com/jucode/jucode/internal/data/ent/balancetx"
	entbillinghold "github.com/jucode/jucode/internal/data/ent/billinghold"
	entgentask "github.com/jucode/jucode/internal/data/ent/generationtask"
	entgroup "github.com/jucode/jucode/internal/data/ent/modelgroup"
	entorgmember "github.com/jucode/jucode/internal/data/ent/organizationmember"
	entuser "github.com/jucode/jucode/internal/data/ent/user"
)

// Postgres-backed like the usage-writer and invite tests: the reserve/settle
// engine's guarantees live in conditional UPDATEs, unique constraints and FOR
// UPDATE row locks that sqlite cannot reproduce. Same opt-in:
//
//	JUCODE_TEST_PG_DSN='postgres://postgres:test@127.0.0.1:5499/jucode_test?sslmode=disable' \
//	  go test ./internal/biz -run Reserve
func testBillingReserveEnv(t *testing.T) (*BillingSvc, *ent.Client) {
	t.Helper()
	dsn := os.Getenv("JUCODE_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("JUCODE_TEST_PG_DSN not set; skipping Postgres-backed billing reserve tests")
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
	settings := NewSettingsSvc(d)
	return NewBillingSvc(d, NewPlanSvc(d, settings), settings), client
}

func reserveTestUser(t *testing.T, client *ent.Client, balance string) *ent.User {
	t.Helper()
	u, err := client.User.Create().
		SetEmail(uuid.NewString() + "@test.invalid").
		SetPasswordHash("x").
		SetStatus(entuser.StatusActive).
		SetBalance(decimal.RequireFromString(balance)).
		Save(context.Background())
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	return u
}

func reserveTestGroup(t *testing.T, client *ent.Client) *ent.ModelGroup {
	t.Helper()
	g, err := client.ModelGroup.Create().
		SetName("grp-" + uuid.NewString()).
		SetBillingSource(entgroup.BillingSourceBalanceOnly).
		Save(context.Background())
	if err != nil {
		t.Fatalf("create group: %v", err)
	}
	return g
}

func llmHoldKey() string   { return "llm:" + uuid.NewString() }
func videoHoldKey() string { return "video:" + uuid.NewString() }

func userBalance(t *testing.T, client *ent.Client, id uuid.UUID) decimal.Decimal {
	t.Helper()
	u, err := client.User.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("load user: %v", err)
	}
	return u.Balance
}

func orgBalance(t *testing.T, client *ent.Client, id uuid.UUID) decimal.Decimal {
	t.Helper()
	o, err := client.Organization.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("load org: %v", err)
	}
	return o.Balance
}

func requireDecimalEq(t *testing.T, got decimal.Decimal, want string, label string) {
	t.Helper()
	if !got.Equal(decimal.RequireFromString(want)) {
		t.Fatalf("%s = %s, want %s", label, got, want)
	}
}

// 并发透支被拒: many simultaneous reserves against one balance — only the
// ones the balance can fully cover may pass, and the balance must never go
// negative. This is the conditional-UPDATE guarantee under real concurrency.
func TestReserveConcurrentOverdraftRejected(t *testing.T) {
	svc, client := testBillingReserveEnv(t)
	ctx := context.Background()
	user := reserveTestUser(t, client, "10")
	group := reserveTestGroup(t, client)

	const attempts = 12
	amount := decimal.RequireFromString("3")
	var wg sync.WaitGroup
	results := make([]error, attempts)
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := svc.Reserve(ctx, ReserveInput{
				User: user, Group: group,
				Key: llmHoldKey(), Amount: amount, TTL: time.Hour,
			})
			results[i] = err
		}(i)
	}
	wg.Wait()

	ok, rejected := 0, 0
	for _, err := range results {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrInsufficientBalance):
			rejected++
		default:
			t.Fatalf("unexpected reserve error: %v", err)
		}
	}
	if ok != 3 { // 10 / 3 = 3 full holds, the 4th would overdraft
		t.Fatalf("successful reserves = %d, want 3 (rejected %d)", ok, rejected)
	}
	bal := userBalance(t, client, user.ID)
	if bal.IsNegative() {
		t.Fatalf("balance went negative: %s", bal)
	}
	requireDecimalEq(t, bal, "1", "balance after concurrent reserves")
}

func TestReserveLastRequestHoldsRemaining(t *testing.T) {
	svc, client := testBillingReserveEnv(t)
	ctx := context.Background()
	user := reserveTestUser(t, client, "0.20")
	group := reserveTestGroup(t, client)

	r, err := svc.Reserve(ctx, ReserveInput{
		User: user, Group: group, Key: llmHoldKey(),
		Amount: decimal.RequireFromString("2.00"), TTL: time.Hour,
	})
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	requireDecimalEq(t, r.Amount, "0.20", "last-request hold")
	requireDecimalEq(t, userBalance(t, client, user.ID), "0", "balance frozen")
}

func TestReserveLowBalanceSmallEstimateStillLastRequest(t *testing.T) {
	svc, client := testBillingReserveEnv(t)
	ctx := context.Background()
	user := reserveTestUser(t, client, "0.20")
	group := reserveTestGroup(t, client)

	r, err := svc.Reserve(ctx, ReserveInput{
		User: user, Group: group, Key: llmHoldKey(),
		Amount: decimal.RequireFromString("0.05"), TTL: time.Hour,
	})
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	requireDecimalEq(t, r.Amount, "0.20", "low-water hold all remaining")
	if _, err := svc.Reserve(ctx, ReserveInput{
		User: user, Group: group, Key: llmHoldKey(),
		Amount: decimal.RequireFromString("0.05"), TTL: time.Hour,
	}); !errors.Is(err, ErrInsufficientBalance) {
		t.Fatalf("second low-water reserve err = %v, want ErrInsufficientBalance", err)
	}
}

func TestReserveLastRequestConcurrentOnlyOne(t *testing.T) {
	svc, client := testBillingReserveEnv(t)
	ctx := context.Background()
	user := reserveTestUser(t, client, "0.20")
	group := reserveTestGroup(t, client)
	amount := decimal.RequireFromString("2.00")

	const attempts = 8
	var wg sync.WaitGroup
	results := make([]error, attempts)
	holds := make([]*Reservation, attempts)
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r, err := svc.Reserve(ctx, ReserveInput{
				User: user, Group: group, Key: llmHoldKey(),
				Amount: amount, TTL: time.Hour,
			})
			results[i], holds[i] = err, r
		}(i)
	}
	wg.Wait()
	ok, rejected := 0, 0
	for i, err := range results {
		switch {
		case err == nil:
			ok++
			requireDecimalEq(t, holds[i].Amount, "0.20", "winner hold")
		case errors.Is(err, ErrInsufficientBalance):
			rejected++
		default:
			t.Fatalf("unexpected reserve error: %v", err)
		}
	}
	if ok != 1 || rejected != attempts-1 {
		t.Fatalf("ok=%d rejected=%d, want 1 / %d", ok, rejected, attempts-1)
	}
	requireDecimalEq(t, userBalance(t, client, user.ID), "0", "balance after last-request race")
}

func TestReserveInteractiveHoldBlocksLastRequest(t *testing.T) {
	svc, client := testBillingReserveEnv(t)
	ctx := context.Background()
	user := reserveTestUser(t, client, "1.00")
	group := reserveTestGroup(t, client)

	if _, err := svc.Reserve(ctx, ReserveInput{
		User: user, Group: group, Key: llmHoldKey(),
		Amount: decimal.RequireFromString("0.60"), TTL: time.Hour,
	}); err != nil {
		t.Fatalf("first reserve: %v", err)
	}
	if _, err := svc.Reserve(ctx, ReserveInput{
		User: user, Group: group, Key: llmHoldKey(),
		Amount: decimal.RequireFromString("0.50"), TTL: time.Hour,
	}); !errors.Is(err, ErrInsufficientBalance) {
		t.Fatalf("last-request with live llm hold err = %v, want ErrInsufficientBalance", err)
	}
	requireDecimalEq(t, userBalance(t, client, user.ID), "0.40", "second reserve must not freeze")
}

func TestReserveVideoHoldDoesNotBlockLastRequest(t *testing.T) {
	svc, client := testBillingReserveEnv(t)
	ctx := context.Background()
	user := reserveTestUser(t, client, "0.30")
	group := reserveTestGroup(t, client)

	if _, err := svc.Reserve(ctx, ReserveInput{
		User: user, Group: group, Key: videoHoldKey(),
		Amount: decimal.RequireFromString("0.10"), TTL: time.Hour,
	}); err != nil {
		t.Fatalf("video reserve: %v", err)
	}
	r, err := svc.Reserve(ctx, ReserveInput{
		User: user, Group: group, Key: llmHoldKey(),
		Amount: decimal.RequireFromString("2.00"), TTL: time.Hour,
	})
	if err != nil {
		t.Fatalf("chat last-request beside video: %v", err)
	}
	requireDecimalEq(t, r.Amount, "0.20", "chat froze remaining")
	requireDecimalEq(t, userBalance(t, client, user.ID), "0", "all remaining frozen")
}

func TestReserveLastRequestIdempotentRetry(t *testing.T) {
	svc, client := testBillingReserveEnv(t)
	ctx := context.Background()
	user := reserveTestUser(t, client, "0.20")
	group := reserveTestGroup(t, client)
	key := llmHoldKey()
	est := decimal.RequireFromString("2.00")

	r1, err := svc.Reserve(ctx, ReserveInput{
		User: user, Group: group, Key: key, Amount: est, TTL: time.Hour,
	})
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	r2, err := svc.Reserve(ctx, ReserveInput{
		User: user, Group: group, Key: key, Amount: est, TTL: time.Hour,
	})
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if r1.HoldID != r2.HoldID {
		t.Fatalf("retry minted a second hold")
	}
	requireDecimalEq(t, r2.Amount, "0.20", "retry returns reduced hold")
	requireDecimalEq(t, userBalance(t, client, user.ID), "0", "no second freeze")
}

func TestReserveNegativeBalanceRejected(t *testing.T) {
	svc, client := testBillingReserveEnv(t)
	ctx := context.Background()
	user := reserveTestUser(t, client, "-0.50")
	group := reserveTestGroup(t, client)
	if _, err := svc.Reserve(ctx, ReserveInput{
		User: user, Group: group, Key: llmHoldKey(),
		Amount: decimal.RequireFromString("0.10"), TTL: time.Hour,
	}); !errors.Is(err, ErrInsufficientBalance) {
		t.Fatalf("err = %v, want ErrInsufficientBalance", err)
	}
}

// 预留后结算: settle below the hold refunds the difference and writes exactly
// one deduct ledger row; a retried settle is a no-op reporting the recorded
// amount.
func TestReserveSettleRefundsDifference(t *testing.T) {
	svc, client := testBillingReserveEnv(t)
	ctx := context.Background()
	user := reserveTestUser(t, client, "10")
	group := reserveTestGroup(t, client)

	r, err := svc.Reserve(ctx, ReserveInput{
		User: user, Group: group, Key: uuid.NewString(),
		Amount: decimal.RequireFromString("5"), TTL: time.Hour,
	})
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	requireDecimalEq(t, userBalance(t, client, user.ID), "5", "balance while held")

	res, err := svc.SettleReservation(ctx, r,
		decimal.RequireFromString("2"), decimal.RequireFromString("2"), SettleOptions{Note: "t"})
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	requireDecimalEq(t, res.CostFinal, "2", "settled cost")
	requireDecimalEq(t, userBalance(t, client, user.ID), "8", "balance after settle")

	n, err := client.BalanceTx.Query().Where(entbalancetx.UserIDEQ(user.ID)).Count(ctx)
	if err != nil || n != 1 {
		t.Fatalf("deduct rows = %d (err %v), want 1", n, err)
	}
	h, err := client.BillingHold.Query().Where(entbillinghold.RequestKeyEQ(r.Key)).Only(ctx)
	if err != nil || h.Status != entbillinghold.StatusSettled {
		t.Fatalf("hold status = %v (err %v), want settled", h.Status, err)
	}

	// Idempotent retry: no money moves, the recorded amount comes back.
	res2, err := svc.SettleReservation(ctx, r,
		decimal.RequireFromString("2"), decimal.RequireFromString("2"), SettleOptions{Note: "t"})
	if err != nil {
		t.Fatalf("settle retry: %v", err)
	}
	requireDecimalEq(t, res2.CostFinal, "2", "retried settle cost")
	requireDecimalEq(t, userBalance(t, client, user.ID), "8", "balance after retried settle")
}

// Actual above the hold: charge the full usage even if that overdrafts.
func TestSettleAboveHoldGoesNegative(t *testing.T) {
	svc, client := testBillingReserveEnv(t)
	ctx := context.Background()
	user := reserveTestUser(t, client, "6")
	group := reserveTestGroup(t, client)

	r, err := svc.Reserve(ctx, ReserveInput{
		User: user, Group: group, Key: llmHoldKey(),
		Amount: decimal.RequireFromString("5"), TTL: time.Hour,
	})
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	res, err := svc.SettleReservation(ctx, r,
		decimal.RequireFromString("8"), decimal.RequireFromString("8"), SettleOptions{Note: "t"})
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	requireDecimalEq(t, res.CostFinal, "8", "full actual")
	requireDecimalEq(t, userBalance(t, client, user.ID), "-2", "overdraft")
}

func TestSettleLastRequestActualGoesNegative(t *testing.T) {
	svc, client := testBillingReserveEnv(t)
	ctx := context.Background()
	user := reserveTestUser(t, client, "0.20")
	group := reserveTestGroup(t, client)

	r, err := svc.Reserve(ctx, ReserveInput{
		User: user, Group: group, Key: llmHoldKey(),
		Amount: decimal.RequireFromString("2.00"), TTL: time.Hour,
	})
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	res, err := svc.SettleReservation(ctx, r,
		decimal.RequireFromString("2.00"), decimal.RequireFromString("2.00"), SettleOptions{Note: "t"})
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	requireDecimalEq(t, res.CostFinal, "2.00", "full actual")
	requireDecimalEq(t, userBalance(t, client, user.ID), "-1.80", "0.20 - 2.00")
	n, err := client.BalanceTx.Query().Where(entbalancetx.UserIDEQ(user.ID)).Count(ctx)
	if err != nil || n != 1 {
		t.Fatalf("deduct rows = %d (err %v), want 1", n, err)
	}
}

func TestSettleCapAtHoldLastRequestStaysNonNegative(t *testing.T) {
	svc, client := testBillingReserveEnv(t)
	ctx := context.Background()
	user := reserveTestUser(t, client, "0.20")
	group := reserveTestGroup(t, client)

	r, err := svc.Reserve(ctx, ReserveInput{
		User: user, Group: group, Key: llmHoldKey(),
		Amount: decimal.RequireFromString("2.00"), TTL: time.Hour,
	})
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	res, err := svc.SettleReservation(ctx, r,
		decimal.RequireFromString("2.00"), decimal.RequireFromString("2.00"),
		SettleOptions{Note: "cancel", CapAtHold: true})
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	requireDecimalEq(t, res.CostFinal, "0.20", "capped at last-request hold")
	requireDecimalEq(t, userBalance(t, client, user.ID), "0", "reconstructed usage must not overdraft")
}

func TestOrganizationSettleLastRequestGoesNegative(t *testing.T) {
	svc, client := testBillingReserveEnv(t)
	ctx := context.Background()
	member := reserveTestUser(t, client, "0")
	group := reserveTestGroup(t, client)
	org, key := reserveTestOrg(t, client, member, "0.20", "0", "0", "0")

	r, err := svc.Reserve(ctx, ReserveInput{
		User: member, ApiKey: key, Group: group, Key: llmHoldKey(),
		Amount: decimal.RequireFromString("2.00"), TTL: time.Hour,
	})
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	res, err := svc.SettleReservation(ctx, r,
		decimal.RequireFromString("2.00"), decimal.RequireFromString("2.00"), SettleOptions{Note: "t"})
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	requireDecimalEq(t, res.CostFinal, "2.00", "org full actual")
	requireDecimalEq(t, orgBalance(t, client, org.ID), "-1.80", "org overdraft")
}

// 流取消仍结算: a client-cancel settle reconstructs usage, so the charge is
// capped at the hold — never above what the balance was told it might lose.
func TestSettleCapAtHoldForEstimatedUsage(t *testing.T) {
	svc, client := testBillingReserveEnv(t)
	ctx := context.Background()
	user := reserveTestUser(t, client, "10")
	group := reserveTestGroup(t, client)

	r, err := svc.Reserve(ctx, ReserveInput{
		User: user, Group: group, Key: uuid.NewString(),
		Amount: decimal.RequireFromString("5"), TTL: time.Hour,
	})
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	res, err := svc.SettleReservation(ctx, r,
		decimal.RequireFromString("9"), decimal.RequireFromString("9"),
		SettleOptions{Note: "cancel", CapAtHold: true})
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	requireDecimalEq(t, res.CostFinal, "5", "capped charge")
	requireDecimalEq(t, userBalance(t, client, user.ID), "5", "balance after capped settle")
}

// Release returns the whole hold; releasing twice is a no-op; settling a hold
// that was already released (reaper raced an in-flight request) still
// collects the actual cost from the live balance.
func TestReleaseThenSettleFallsBackToDirectCharge(t *testing.T) {
	svc, client := testBillingReserveEnv(t)
	ctx := context.Background()
	user := reserveTestUser(t, client, "10")
	group := reserveTestGroup(t, client)

	r, err := svc.Reserve(ctx, ReserveInput{
		User: user, Group: group, Key: uuid.NewString(),
		Amount: decimal.RequireFromString("4"), TTL: time.Hour,
	})
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := svc.ReleaseReservation(ctx, r); err != nil {
		t.Fatalf("release: %v", err)
	}
	requireDecimalEq(t, userBalance(t, client, user.ID), "10", "balance after release")
	if err := svc.ReleaseReservation(ctx, r); err != nil {
		t.Fatalf("double release: %v", err)
	}
	requireDecimalEq(t, userBalance(t, client, user.ID), "10", "balance after double release")

	res, err := svc.SettleReservation(ctx, r,
		decimal.RequireFromString("2"), decimal.RequireFromString("2"), SettleOptions{Note: "late"})
	if err != nil {
		t.Fatalf("settle after release: %v", err)
	}
	requireDecimalEq(t, res.CostFinal, "2", "post-release charge")
	requireDecimalEq(t, userBalance(t, client, user.ID), "8", "balance after post-release settle")

	// Retrying the released-hold fallback must report the recorded result,
	// not debit the live balance for a second time.
	res, err = svc.SettleReservation(ctx, r,
		decimal.RequireFromString("2"), decimal.RequireFromString("2"), SettleOptions{Note: "late retry"})
	if err != nil {
		t.Fatalf("retry settle after release: %v", err)
	}
	requireDecimalEq(t, res.CostFinal, "2", "retried post-release charge")
	requireDecimalEq(t, userBalance(t, client, user.ID), "8", "balance after post-release retry")
	n, err := client.BalanceTx.Query().Where(entbalancetx.UserIDEQ(user.ID)).Count(ctx)
	if err != nil || n != 1 {
		t.Fatalf("deduct rows after retry = %d (err %v), want 1", n, err)
	}
}

// The idempotency key: a duplicate Reserve hands back the live hold without
// freezing twice, and refuses once the hold has finished.
func TestReserveIdempotencyKey(t *testing.T) {
	svc, client := testBillingReserveEnv(t)
	ctx := context.Background()
	user := reserveTestUser(t, client, "10")
	group := reserveTestGroup(t, client)
	key := uuid.NewString()

	r1, err := svc.Reserve(ctx, ReserveInput{
		User: user, Group: group, Key: key,
		Amount: decimal.RequireFromString("4"), TTL: time.Hour,
	})
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	other := reserveTestUser(t, client, "10")
	if _, err := svc.Reserve(ctx, ReserveInput{
		User: other, Group: group, Key: key,
		Amount: decimal.RequireFromString("4"), TTL: time.Hour,
	}); !errors.Is(err, ErrHoldIdentityConflict) {
		t.Fatalf("cross-user duplicate err = %v, want ErrHoldIdentityConflict", err)
	}
	requireDecimalEq(t, userBalance(t, client, other.ID), "10", "colliding user balance")
	if _, err := svc.Reserve(ctx, ReserveInput{
		User: user, Group: group, Key: key,
		Amount: decimal.RequireFromString("3"), TTL: time.Hour,
	}); !errors.Is(err, ErrHoldIdentityConflict) {
		t.Fatalf("different-amount duplicate err = %v, want ErrHoldIdentityConflict", err)
	}
	r2, err := svc.Reserve(ctx, ReserveInput{
		User: user, Group: group, Key: key,
		Amount: decimal.RequireFromString("4"), TTL: time.Hour,
	})
	if err != nil {
		t.Fatalf("duplicate reserve: %v", err)
	}
	if r1.HoldID != r2.HoldID {
		t.Fatalf("duplicate reserve created a second hold: %s vs %s", r1.HoldID, r2.HoldID)
	}
	requireDecimalEq(t, userBalance(t, client, user.ID), "6", "balance frozen once")

	if _, err := svc.SettleReservation(ctx, r1,
		decimal.RequireFromString("1"), decimal.RequireFromString("1"), SettleOptions{}); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if _, err := svc.Reserve(ctx, ReserveInput{
		User: user, Group: group, Key: key,
		Amount: decimal.RequireFromString("4"), TTL: time.Hour,
	}); !errors.Is(err, ErrHoldFinished) {
		t.Fatalf("reserve on finished key err = %v, want ErrHoldFinished", err)
	}
}

func reserveTestOrg(t *testing.T, client *ent.Client, member *ent.User, balance, monthlyLimit, memberMonthly, memberTotal string) (*ent.Organization, *ent.ApiKey) {
	t.Helper()
	ctx := context.Background()
	owner := reserveTestUser(t, client, "0")
	org, err := client.Organization.Create().
		SetName("org-" + uuid.NewString()).
		SetOwnerUserID(owner.ID).
		SetBalance(decimal.RequireFromString(balance)).
		SetMonthlySpendLimit(decimal.RequireFromString(monthlyLimit)).
		Save(ctx)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if _, err := client.OrganizationMember.Create().
		SetOrganizationID(org.ID).
		SetUserID(member.ID).
		SetRole(entorgmember.RoleMember).
		SetMonthlySpendLimit(decimal.RequireFromString(memberMonthly)).
		SetTotalSpendLimit(decimal.RequireFromString(memberTotal)).
		Save(ctx); err != nil {
		t.Fatalf("create member: %v", err)
	}
	key, err := client.ApiKey.Create().
		SetUserID(member.ID).
		SetPrefix("sk-test").
		SetHash(uuid.NewString()).
		SetBillingScope(entapikey.BillingScopeOrganization).
		SetOrganizationID(org.ID).
		Save(ctx)
	if err != nil {
		t.Fatalf("create api key: %v", err)
	}
	return org, key
}

// 组织限额拒绝在输出前: the limits are occupied by the ESTIMATE at reserve
// time — settled spend plus still-held amounts plus the new hold must fit —
// so an over-limit request is rejected before any output exists.
func TestOrganizationReserveOccupiesLimitBeforeOutput(t *testing.T) {
	svc, client := testBillingReserveEnv(t)
	ctx := context.Background()
	member := reserveTestUser(t, client, "0")
	group := reserveTestGroup(t, client)
	org, key := reserveTestOrg(t, client, member, "100", "10", "0", "0")

	// First estimate fits under the monthly limit of 10.
	r1, err := svc.Reserve(ctx, ReserveInput{
		User: member, ApiKey: key, Group: group, Key: uuid.NewString(),
		Amount: decimal.RequireFromString("6"), TTL: time.Hour,
	})
	if err != nil {
		t.Fatalf("first org reserve: %v", err)
	}
	requireDecimalEq(t, orgBalance(t, client, org.ID), "94", "org balance while held")

	// Second estimate would take held total to 12 > 10 → rejected while the
	// balance easily covers it, and BEFORE anything ran.
	if _, err := svc.Reserve(ctx, ReserveInput{
		User: member, ApiKey: key, Group: group, Key: uuid.NewString(),
		Amount: decimal.RequireFromString("6"), TTL: time.Hour,
	}); !errors.Is(err, ErrOrganizationLimit) {
		t.Fatalf("over-limit reserve err = %v, want ErrOrganizationLimit", err)
	}
	requireDecimalEq(t, orgBalance(t, client, org.ID), "94", "org balance unchanged by rejected reserve")

	// Settling the first at its actual cost frees the estimate's remainder.
	res, err := svc.SettleReservation(ctx, r1,
		decimal.RequireFromString("3"), decimal.RequireFromString("3"), SettleOptions{Note: "t"})
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	requireDecimalEq(t, res.CostFinal, "3", "org settled cost")
	requireDecimalEq(t, orgBalance(t, client, org.ID), "97", "org balance after settle")

	// Settled spend 3 + new hold 6 = 9 ≤ 10 → allowed again.
	if _, err := svc.Reserve(ctx, ReserveInput{
		User: member, ApiKey: key, Group: group, Key: uuid.NewString(),
		Amount: decimal.RequireFromString("6"), TTL: time.Hour,
	}); err != nil {
		t.Fatalf("reserve after settle: %v", err)
	}
}

func TestOrganizationReserveMemberLimitAndBalance(t *testing.T) {
	svc, client := testBillingReserveEnv(t)
	ctx := context.Background()
	group := reserveTestGroup(t, client)

	// Member monthly cap of 5 rejects a 6-point estimate.
	member := reserveTestUser(t, client, "0")
	_, key := reserveTestOrg(t, client, member, "100", "0", "5", "0")
	if _, err := svc.Reserve(ctx, ReserveInput{
		User: member, ApiKey: key, Group: group, Key: uuid.NewString(),
		Amount: decimal.RequireFromString("6"), TTL: time.Hour,
	}); !errors.Is(err, ErrMemberLimit) {
		t.Fatalf("member-limit reserve err = %v, want ErrMemberLimit", err)
	}

	// Last-request: estimate above org cash still freezes remaining.
	member2 := reserveTestUser(t, client, "0")
	org2, key2 := reserveTestOrg(t, client, member2, "5", "0", "0", "0")
	r, err := svc.Reserve(ctx, ReserveInput{
		User: member2, ApiKey: key2, Group: group, Key: llmHoldKey(),
		Amount: decimal.RequireFromString("6"), TTL: time.Hour,
	})
	if err != nil {
		t.Fatalf("org last-request: %v", err)
	}
	requireDecimalEq(t, r.Amount, "5", "org last-request hold")
	requireDecimalEq(t, orgBalance(t, client, org2.ID), "0", "org frozen")
}

func TestOrganizationReserveLastRequestHoldsRemaining(t *testing.T) {
	svc, client := testBillingReserveEnv(t)
	ctx := context.Background()
	member := reserveTestUser(t, client, "0")
	group := reserveTestGroup(t, client)
	org, key := reserveTestOrg(t, client, member, "0.20", "0", "0", "0")

	r, err := svc.Reserve(ctx, ReserveInput{
		User: member, ApiKey: key, Group: group, Key: llmHoldKey(),
		Amount: decimal.RequireFromString("2.00"), TTL: time.Hour,
	})
	if err != nil {
		t.Fatalf("org last-request: %v", err)
	}
	requireDecimalEq(t, r.Amount, "0.20", "org last-request hold")
	requireDecimalEq(t, orgBalance(t, client, org.ID), "0", "org frozen")
}

func TestOrganizationReserveLastRequestConcurrentOnlyOne(t *testing.T) {
	svc, client := testBillingReserveEnv(t)
	ctx := context.Background()
	member := reserveTestUser(t, client, "0")
	group := reserveTestGroup(t, client)
	org, key := reserveTestOrg(t, client, member, "0.20", "0", "0", "0")
	amount := decimal.RequireFromString("2.00")

	const attempts = 8
	var wg sync.WaitGroup
	results := make([]error, attempts)
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := svc.Reserve(ctx, ReserveInput{
				User: member, ApiKey: key, Group: group, Key: llmHoldKey(),
				Amount: amount, TTL: time.Hour,
			})
			results[i] = err
		}(i)
	}
	wg.Wait()
	ok := 0
	for _, err := range results {
		if err == nil {
			ok++
			continue
		}
		if !errors.Is(err, ErrInsufficientBalance) {
			t.Fatalf("unexpected err: %v", err)
		}
	}
	if ok != 1 {
		t.Fatalf("successful org last-requests = %d, want 1", ok)
	}
	requireDecimalEq(t, orgBalance(t, client, org.ID), "0", "org after race")
}

func TestOrganizationReserveNegativeBalanceRejected(t *testing.T) {
	svc, client := testBillingReserveEnv(t)
	ctx := context.Background()
	member := reserveTestUser(t, client, "0")
	group := reserveTestGroup(t, client)
	_, key := reserveTestOrg(t, client, member, "-0.50", "0", "0", "0")
	if _, err := svc.Reserve(ctx, ReserveInput{
		User: member, ApiKey: key, Group: group, Key: llmHoldKey(),
		Amount: decimal.RequireFromString("0.10"), TTL: time.Hour,
	}); !errors.Is(err, ErrInsufficientBalance) {
		t.Fatalf("err = %v, want ErrInsufficientBalance", err)
	}
}

// WS 第二轮余额不足: with turn 1's estimate still frozen, turn 2's reserve is
// rejected; once turn 1 settles at its (smaller) actual cost the next turn
// fits again. This is exactly the per-turn sequence the realtime meter runs.
func TestReserveSecondTurnInsufficientBalance(t *testing.T) {
	svc, client := testBillingReserveEnv(t)
	ctx := context.Background()
	user := reserveTestUser(t, client, "10")
	group := reserveTestGroup(t, client)
	sessionKey := uuid.NewString()

	turn1, err := svc.Reserve(ctx, ReserveInput{
		User: user, Group: group, Key: "llm:" + sessionKey + ":turn1",
		Amount: decimal.RequireFromString("6"), TTL: time.Hour,
	})
	if err != nil {
		t.Fatalf("turn 1 reserve: %v", err)
	}
	if _, err := svc.Reserve(ctx, ReserveInput{
		User: user, Group: group, Key: "llm:" + sessionKey + ":turn2",
		Amount: decimal.RequireFromString("6"), TTL: time.Hour,
	}); !errors.Is(err, ErrInsufficientBalance) {
		t.Fatalf("turn 2 reserve err = %v, want ErrInsufficientBalance", err)
	}
	if _, err := svc.SettleReservation(ctx, turn1,
		decimal.RequireFromString("1"), decimal.RequireFromString("1"), SettleOptions{}); err != nil {
		t.Fatalf("turn 1 settle: %v", err)
	}
	requireDecimalEq(t, userBalance(t, client, user.ID), "9", "balance after turn 1")
	if _, err := svc.Reserve(ctx, ReserveInput{
		User: user, Group: group, Key: "llm:" + sessionKey + ":turn3",
		Amount: decimal.RequireFromString("6"), TTL: time.Hour,
	}); err != nil {
		t.Fatalf("turn 3 reserve: %v", err)
	}
}

// 视频 Charge 失败不标 billed / 原子性: the charge and the billed flag are one
// transaction. A settle that loses the billed-flag race rolls the whole
// charge back; a settle that wins flips the flag in the same commit.
func TestVideoSettleChargeAndBilledAreAtomic(t *testing.T) {
	svc, client := testBillingReserveEnv(t)
	ctx := context.Background()
	user := reserveTestUser(t, client, "10")
	group := reserveTestGroup(t, client)

	newTask := func(billed bool) *ent.GenerationTask {
		task, err := client.GenerationTask.Create().
			SetUserID(user.ID).
			SetRequestID(uuid.NewString()).
			SetModel("video-test").
			SetStatus(entgentask.StatusRunning).
			SetBilled(billed).
			Save(ctx)
		if err != nil {
			t.Fatalf("create task: %v", err)
		}
		return task
	}

	// Winning settle: charge lands AND billed flips, atomically.
	task := newTask(false)
	r, err := svc.Reserve(ctx, ReserveInput{
		User: user, Group: group, Key: uuid.NewString(),
		Amount: decimal.RequireFromString("5"), TTL: time.Hour,
	})
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	res, err := svc.SettleReservation(ctx, r,
		decimal.RequireFromString("2"), decimal.RequireFromString("2"),
		SettleOptions{Note: "video", TaskID: &task.ID})
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	requireDecimalEq(t, res.CostFinal, "2", "video settled cost")
	requireDecimalEq(t, userBalance(t, client, user.ID), "8", "balance after video settle")
	got, _ := client.GenerationTask.Get(ctx, task.ID)
	if !got.Billed {
		t.Fatal("task not marked billed after settle")
	}
	if got.CostFinal == nil {
		t.Fatal("task cost_final not recorded")
	}
	requireDecimalEq(t, *got.CostFinal, "2", "task cost_final")

	// Losing settle: the billed flag was already taken → the WHOLE settle
	// rolls back. No charge, hold still held (so it can be released/retried),
	// balance untouched.
	taken := newTask(true)
	r2, err := svc.Reserve(ctx, ReserveInput{
		User: user, Group: group, Key: uuid.NewString(),
		Amount: decimal.RequireFromString("5"), TTL: time.Hour,
	})
	if err != nil {
		t.Fatalf("reserve 2: %v", err)
	}
	if _, err := svc.SettleReservation(ctx, r2,
		decimal.RequireFromString("2"), decimal.RequireFromString("2"),
		SettleOptions{Note: "video", TaskID: &taken.ID}); !errors.Is(err, ErrTaskAlreadyBilled) {
		t.Fatalf("settle on billed task err = %v, want ErrTaskAlreadyBilled", err)
	}
	h, err := client.BillingHold.Query().Where(entbillinghold.RequestKeyEQ(r2.Key)).Only(ctx)
	if err != nil || h.Status != entbillinghold.StatusHeld {
		t.Fatalf("hold status after rolled-back settle = %v (err %v), want held", h.Status, err)
	}
	requireDecimalEq(t, userBalance(t, client, user.ID), "3", "balance: only the r2 hold moved, no charge")
	if err := svc.ReleaseReservation(ctx, r2); err != nil {
		t.Fatalf("release: %v", err)
	}
	requireDecimalEq(t, userBalance(t, client, user.ID), "8", "balance after releasing rolled-back hold")
}

// Crash safety: a hold whose owner died is returned by the reaper once its
// TTL passes.
func TestReleaseExpiredHoldsReturnsFunds(t *testing.T) {
	svc, client := testBillingReserveEnv(t)
	ctx := context.Background()
	user := reserveTestUser(t, client, "10")
	group := reserveTestGroup(t, client)

	r, err := svc.Reserve(ctx, ReserveInput{
		User: user, Group: group, Key: uuid.NewString(),
		Amount: decimal.RequireFromString("4"), TTL: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if err := svc.ReleaseExpiredHolds(ctx, 100); err != nil {
		t.Fatalf("reap: %v", err)
	}
	requireDecimalEq(t, userBalance(t, client, user.ID), "10", "balance after reap")
	h, err := client.BillingHold.Query().Where(entbillinghold.RequestKeyEQ(r.Key)).Only(ctx)
	if err != nil || h.Status != entbillinghold.StatusReleased {
		t.Fatalf("hold status after reap = %v (err %v), want released", h.Status, err)
	}
}

// Direct (non-reserved) charges take the full final and may overdraft —
// same cash rule as Settle without CapAtHold.
func TestDirectChargeGoesNegative(t *testing.T) {
	svc, client := testBillingReserveEnv(t)
	ctx := context.Background()
	user := reserveTestUser(t, client, "3")

	res, err := svc.chargeBalance(ctx, user.ID, nil, "balance",
		decimal.RequireFromString("5"), decimal.RequireFromString("5"), "t")
	if err != nil {
		t.Fatalf("direct charge: %v", err)
	}
	requireDecimalEq(t, res.CostFinal, "5", "full actual")
	requireDecimalEq(t, userBalance(t, client, user.ID), "-2", "direct overdraft")
}
