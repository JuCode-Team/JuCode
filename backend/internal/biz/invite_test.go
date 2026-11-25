package biz

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"slices"
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
	entagententry "github.com/jucode/jucode/internal/data/ent/agentsettlemententry"
	entbinding "github.com/jucode/jucode/internal/data/ent/invitebinding"
	entrebate "github.com/jucode/jucode/internal/data/ent/inviterebate"
	entreward "github.com/jucode/jucode/internal/data/ent/invitereward"
	entsiteorder "github.com/jucode/jucode/internal/data/ent/siteorder"
	entsiterefund "github.com/jucode/jucode/internal/data/ent/siterefund"
	entuser "github.com/jucode/jucode/internal/data/ent/user"
)

// Postgres-backed like the usage-writer tests: the engine's idempotency
// guarantees live in unique constraints and FOR UPDATE locks that sqlite
// cannot reproduce. Same opt-in:
//
//	JUCODE_TEST_PG_DSN='postgres://postgres:test@127.0.0.1:5499/jucode_test?sslmode=disable' \
//	  go test ./internal/biz -run Invite
func testInviteEnv(t *testing.T) (*InviteSvc, *SettingsSvc, *ent.Client) {
	t.Helper()
	dsn := os.Getenv("JUCODE_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("JUCODE_TEST_PG_DSN not set; skipping Postgres-backed invite tests")
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
	return NewInviteSvc(d, settings), settings, client
}

// setInviteProgram swaps the program in the settings cache directly —
// the tests exercise the engine, not the KV round-trip (which
// TestInviteProgramParseRoundTrip covers separately).
func setInviteProgram(t *testing.T, settings *SettingsSvc, p InviteProgramSettings) {
	t.Helper()
	st, err := settings.GetSite(context.Background())
	if err != nil {
		t.Fatalf("get site: %v", err)
	}
	st.InviteProgram = &p
	settings.storeSite(st)
}

// permissiveProgram: signup rewards on with every risk gate that would
// trip on freshly-created test fixtures disabled. Individual tests
// re-enable the gate under test.
func permissiveProgram() InviteProgramSettings {
	p := DefaultInviteProgram()
	p.Enabled = true
	p.SignupRewardEnabled = true
	p.SignupRewardAmount = decimal.RequireFromString("5")
	p.SignupCondition = InviteConditionNone
	p.InviterMinAgeHours = 0
	p.RequireInviterEmailVerified = false
	p.SameIPDailyCap = 0
	p.SignupDailyCountCapPerInviter = 0
	return p
}

func newInviteTestUser(t *testing.T, client *ent.Client) *ent.User {
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

func inviterWithCode(t *testing.T, svc *InviteSvc, client *ent.Client) (*ent.User, string) {
	t.Helper()
	u := newInviteTestUser(t, client)
	code, err := svc.EnsureInviteCode(context.Background(), u.ID)
	if err != nil {
		t.Fatalf("ensure code: %v", err)
	}
	return u, code
}

func rewardOf(t *testing.T, client *ent.Client, inviteeID uuid.UUID) *ent.InviteReward {
	t.Helper()
	r, err := client.InviteReward.Query().Where(entreward.InviteeUserIDEQ(inviteeID)).Only(context.Background())
	if err != nil {
		t.Fatalf("load reward: %v", err)
	}
	return r
}

func balanceOf(t *testing.T, client *ent.Client, id uuid.UUID) decimal.Decimal {
	t.Helper()
	u, err := client.User.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("load user: %v", err)
	}
	return u.Balance
}

func addTopup(t *testing.T, client *ent.Client, userID uuid.UUID, amount string) {
	t.Helper()
	amt := decimal.RequireFromString(amount)
	if _, err := client.SiteOrder.Create().SetOrderNo("T" + uuid.NewString()).
		SetUserID(userID).SetKind(entsiteorder.KindBalance).SetAmount(amt).
		SetStatus(entsiteorder.StatusPaid).SetPaidAt(time.Now()).Save(context.Background()); err != nil {
		t.Fatalf("paid order: %v", err)
	}
	if err := client.BalanceTx.Create().
		SetUserID(userID).SetKind("topup").
		SetAmount(amt).SetBalanceAfter(amt).
		SetCurrency(CurrencyCNY).
		Exec(context.Background()); err != nil {
		t.Fatalf("topup tx: %v", err)
	}
}

// attributeInviteTestUserToAgent uses the production agent-code redemption
// path so tests cover both sides of the mutex: the agent sale still lands,
// while invite payouts use that immutable sale attribution as their gate.
func attributeInviteTestUserToAgent(t *testing.T, settings *SettingsSvc, client *ent.Client, userID uuid.UUID) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	agent := newInviteTestUser(t, client)
	d := data.NewWith(client, nil, nil)
	agents := NewAgentSvc(d, settings)
	redeem := NewRedeemSvc(d, nil, settings)
	redeem.SetAgentSvc(agents)
	amount := decimal.RequireFromString("1")
	codes, err := redeem.GenerateBatch(ctx, GenerateInput{
		Kind:      "balance",
		Amount:    &amount,
		Count:     1,
		MaxUses:   1,
		CreatedBy: agent.ID,
		AgentID:   &agent.ID,
	})
	if err != nil {
		t.Fatalf("generate agent code: %v", err)
	}
	if _, err := redeem.Redeem(ctx, userID, codes[0].Code, "203.0.113.88"); err != nil {
		t.Fatalf("redeem agent code: %v", err)
	}
	n, err := client.AgentSettlementEntry.Query().
		Where(entagententry.AgentIDEQ(agent.ID), entagententry.UserIDEQ(userID)).
		Count(ctx)
	if err != nil {
		t.Fatalf("query agent sale: %v", err)
	}
	if n != 1 {
		t.Fatalf("agent sale entries = %d, want 1", n)
	}
	return agent.ID
}

// --- binding ---

func TestInviteBindOnRegister(t *testing.T) {
	svc, settings, client := testInviteEnv(t)
	ctx := context.Background()
	setInviteProgram(t, settings, permissiveProgram())
	inviter, code := inviterWithCode(t, svc, client)

	invitee := newInviteTestUser(t, client)
	if err := svc.BindOnRegister(ctx, invitee.ID, code, "203.0.113.9", "ua"); err != nil {
		t.Fatalf("bind: %v", err)
	}
	b, err := client.InviteBinding.Query().Where(entbinding.InviteeUserIDEQ(invitee.ID)).Only(ctx)
	if err != nil {
		t.Fatalf("binding not created: %v", err)
	}
	if b.InviterUserID != inviter.ID || b.InviteCodeUsed != code || b.RegisterIP != "203.0.113.9" {
		t.Fatalf("binding fields wrong: %+v", b)
	}
	u, _ := client.User.Get(ctx, invitee.ID)
	if u.InvitedByUserID == nil || *u.InvitedByUserID != inviter.ID {
		t.Fatal("invited_by_user_id not set")
	}

	// A code that reaches here unchecked (caller skipped ResolveInviter,
	// or the inviter row vanished in between) still must not error or bind.
	loner := newInviteTestUser(t, client)
	for _, bad := range []string{"", "  ", "short", "ZZZZZZZZ", "has 0O1I"} {
		if err := svc.BindOnRegister(ctx, loner.ID, bad, "203.0.113.9", "ua"); err != nil {
			t.Fatalf("code %q must not error: %v", bad, err)
		}
	}
	if n, _ := client.InviteBinding.Query().Where(entbinding.InviteeUserIDEQ(loner.ID)).Count(ctx); n != 0 {
		t.Fatal("invalid code must not bind")
	}
}

func TestInviteCannotBindTwice(t *testing.T) {
	svc, settings, client := testInviteEnv(t)
	ctx := context.Background()
	setInviteProgram(t, settings, permissiveProgram())
	inviterA, codeA := inviterWithCode(t, svc, client)
	_, codeB := inviterWithCode(t, svc, client)

	invitee := newInviteTestUser(t, client)
	if err := svc.BindOnRegister(ctx, invitee.ID, codeA, "", ""); err != nil {
		t.Fatalf("first bind: %v", err)
	}
	if err := svc.BindOnRegister(ctx, invitee.ID, codeB, "", ""); err != nil {
		t.Fatalf("second bind must be a silent no-op: %v", err)
	}
	bindings, _ := client.InviteBinding.Query().Where(entbinding.InviteeUserIDEQ(invitee.ID)).All(ctx)
	if len(bindings) != 1 || bindings[0].InviterUserID != inviterA.ID {
		t.Fatalf("expected exactly the first binding, got %d", len(bindings))
	}
}

func TestInviteAgentAttributionBeforeCodeSkipsAllInvitePayouts(t *testing.T) {
	svc, settings, client := testInviteEnv(t)
	ctx := context.Background()
	p := permissiveProgram()
	p.TopupRebateEnabled = true
	p.TopupRebateRatePct = decimal.RequireFromString("10")
	setInviteProgram(t, settings, p)
	inviter, code := inviterWithCode(t, svc, client)
	invitee := newInviteTestUser(t, client)

	attributeInviteTestUserToAgent(t, settings, client, invitee.ID)
	if err := svc.BindOnRegister(ctx, invitee.ID, code, "", ""); err != nil {
		t.Fatalf("bind after agent attribution: %v", err)
	}
	if n, _ := client.InviteBinding.Query().Where(entbinding.InviteeUserIDEQ(invitee.ID)).Count(ctx); n != 1 {
		t.Fatalf("invite binding audit trail count = %d, want 1", n)
	}
	if n, _ := client.InviteReward.Query().Where(entreward.InviteeUserIDEQ(invitee.ID)).Count(ctx); n != 0 {
		t.Fatalf("agent-attributed invitee got %d signup reward rows, want 0", n)
	}
	if err := svc.ProcessTopupRebate(ctx, invitee.ID, uuid.New(), decimal.RequireFromString("100")); err != nil {
		t.Fatalf("process rebate: %v", err)
	}
	if n, _ := client.InviteRebate.Query().Where(entrebate.InviteeUserIDEQ(invitee.ID)).Count(ctx); n != 0 {
		t.Fatalf("agent-attributed invitee got %d rebate rows, want 0", n)
	}
	if got := balanceOf(t, client, inviter.ID); !got.IsZero() {
		t.Fatalf("inviter balance = %s, want 0", got)
	}
}

func TestInviteWithoutAgentAttributionKeepsOriginalPayouts(t *testing.T) {
	svc, settings, client := testInviteEnv(t)
	ctx := context.Background()
	p := permissiveProgram()
	p.TopupRebateEnabled = true
	p.TopupRebateRatePct = decimal.RequireFromString("10")
	setInviteProgram(t, settings, p)
	inviter, code := inviterWithCode(t, svc, client)
	invitee := newInviteTestUser(t, client)

	if err := svc.BindOnRegister(ctx, invitee.ID, code, "", ""); err != nil {
		t.Fatal(err)
	}
	if r := rewardOf(t, client, invitee.ID); r.Status != entreward.StatusPaid {
		t.Fatalf("signup reward status = %s, want paid", r.Status)
	}
	if err := svc.ProcessTopupRebate(ctx, invitee.ID, uuid.New(), decimal.RequireFromString("100")); err != nil {
		t.Fatal(err)
	}
	if got := balanceOf(t, client, inviter.ID); !got.Equal(decimal.RequireFromString("15")) {
		t.Fatalf("original signup + rebate payout = %s, want 15", got)
	}
}

func TestInviteBindingBeforeAgentAttributionStopsLaterPayouts(t *testing.T) {
	svc, settings, client := testInviteEnv(t)
	ctx := context.Background()
	p := permissiveProgram()
	p.SignupCondition = InviteConditionEmailVerified
	p.TopupRebateEnabled = true
	p.TopupRebateRatePct = decimal.RequireFromString("10")
	p.ReviewAmountThreshold = decimal.RequireFromString("1")
	setInviteProgram(t, settings, p)
	inviter, code := inviterWithCode(t, svc, client)
	invitee := newInviteTestUser(t, client)

	if err := svc.BindOnRegister(ctx, invitee.ID, code, "", ""); err != nil {
		t.Fatal(err)
	}
	reward := rewardOf(t, client, invitee.ID)
	if reward.Status != entreward.StatusPending {
		t.Fatalf("setup reward status = %s, want pending", reward.Status)
	}
	if err := svc.ProcessTopupRebate(ctx, invitee.ID, uuid.New(), decimal.RequireFromString("100")); err != nil {
		t.Fatal(err)
	}
	rebates, err := client.InviteRebate.Query().Where(entrebate.InviteeUserIDEQ(invitee.ID)).All(ctx)
	if err != nil || len(rebates) != 1 || rebates[0].Status != entrebate.StatusHeld {
		t.Fatalf("setup held rebate = %+v, err = %v", rebates, err)
	}

	attributeInviteTestUserToAgent(t, settings, client, invitee.ID)
	if err := client.User.UpdateOneID(invitee.ID).SetEmailVerified(true).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if err := svc.EvaluateSignup(ctx, invitee.ID); err != nil {
		t.Fatalf("evaluate after agent attribution: %v", err)
	}
	if got := rewardOf(t, client, invitee.ID); got.Status != entreward.StatusPending {
		t.Fatalf("agent mutex changed pending reward to %s", got.Status)
	}
	if _, err := svc.PayReward(ctx, reward.ID, "manual attempt"); !errors.Is(err, ErrInviteInvalidState) {
		t.Fatalf("manual reward pay error = %v, want ErrInviteInvalidState", err)
	}
	if _, err := svc.PayRebate(ctx, rebates[0].ID, "manual attempt"); !errors.Is(err, ErrInviteInvalidState) {
		t.Fatalf("manual rebate pay error = %v, want ErrInviteInvalidState", err)
	}
	if err := svc.ProcessTopupRebate(ctx, invitee.ID, uuid.New(), decimal.RequireFromString("100")); err != nil {
		t.Fatal(err)
	}
	if n, _ := client.InviteRebate.Query().Where(entrebate.InviteeUserIDEQ(invitee.ID)).Count(ctx); n != 1 {
		t.Fatalf("later agent attribution changed rebate row count to %d, want 1 existing row", n)
	}
	if got := balanceOf(t, client, inviter.ID); !got.IsZero() {
		t.Fatalf("inviter balance after later agent attribution = %s, want 0", got)
	}
}

func TestInvitePaidBeforeAgentAttributionIsNotClawedBack(t *testing.T) {
	svc, settings, client := testInviteEnv(t)
	ctx := context.Background()
	p := permissiveProgram()
	p.TopupRebateEnabled = true
	p.TopupRebateRatePct = decimal.RequireFromString("10")
	setInviteProgram(t, settings, p)
	inviter, code := inviterWithCode(t, svc, client)
	invitee := newInviteTestUser(t, client)

	if err := svc.BindOnRegister(ctx, invitee.ID, code, "", ""); err != nil {
		t.Fatal(err)
	}
	if got := balanceOf(t, client, inviter.ID); !got.Equal(decimal.RequireFromString("5")) {
		t.Fatalf("pre-attribution signup payout = %s, want 5", got)
	}
	attributeInviteTestUserToAgent(t, settings, client, invitee.ID)
	if err := svc.ProcessTopupRebate(ctx, invitee.ID, uuid.New(), decimal.RequireFromString("100")); err != nil {
		t.Fatal(err)
	}
	if got := balanceOf(t, client, inviter.ID); !got.Equal(decimal.RequireFromString("5")) {
		t.Fatalf("paid reward was clawed back or new rebate paid: balance = %s, want 5", got)
	}
	if r := rewardOf(t, client, invitee.ID); r.Status != entreward.StatusPaid {
		t.Fatalf("paid reward status changed to %s", r.Status)
	}
}

// Full registration path: a valid code binds, an invalid code still
// registers (and neither blocks the account creation).
func TestInviteRegisterWithCode(t *testing.T) {
	svc, settings, client := testInviteEnv(t)
	ctx := context.Background()
	setInviteProgram(t, settings, permissiveProgram())
	inviter, code := inviterWithCode(t, svc, client)

	acc := NewAccount(data.NewWith(client, nil, nil), nil, settings)
	acc.SetInvites(svc)

	res, err := acc.Register(ctx, RegisterInput{
		Email: uuid.NewString() + "@test.invalid", Password: "password123",
		InviteCode: code, IP: "203.0.113.77", UserAgent: "test-ua",
	})
	if err != nil {
		t.Fatalf("register with valid code: %v", err)
	}
	b, err := client.InviteBinding.Query().Where(entbinding.InviteeUserIDEQ(res.User.ID)).Only(ctx)
	if err != nil {
		t.Fatalf("binding not created: %v", err)
	}
	if b.InviterUserID != inviter.ID || b.RegisterIP != "203.0.113.77" {
		t.Fatalf("binding fields wrong: %+v", b)
	}
	// The new user got their own invite code at registration.
	u, _ := client.User.Get(ctx, res.User.ID)
	if u.InviteCode == nil || *u.InviteCode == "" {
		t.Fatal("registrant should get an invite code lazily")
	}

	// Neither an unknown nor a malformed code may reach the database: the
	// registration is refused so the registrant can correct the field.
	// Letting it through would strand the attribution for good, since a
	// binding is only ever written at registration.
	unknownEmail := uuid.NewString() + "@test.invalid"
	if _, err := acc.Register(ctx, RegisterInput{
		Email: unknownEmail, Password: "password123", InviteCode: "ZZZZZZZZ",
	}); !errors.Is(err, ErrInviteCodeUnknown) {
		t.Fatalf("unknown code must fail the registration, got %v", err)
	}

	malformedEmail := uuid.NewString() + "@test.invalid"
	if _, err := acc.Register(ctx, RegisterInput{
		Email: malformedEmail, Password: "password123", InviteCode: "NOT-A-CODE",
	}); !errors.Is(err, ErrInviteCodeMalformed) {
		t.Fatalf("malformed code must fail the registration, got %v", err)
	}

	for _, email := range []string{unknownEmail, malformedEmail} {
		if n, _ := client.User.Query().Where(entuser.EmailEQ(email)).Count(ctx); n != 0 {
			t.Fatalf("refused registration must not create %s", email)
		}
	}

	// Leaving the field empty stays the escape hatch: no code, no check.
	if _, err := acc.Register(ctx, RegisterInput{
		Email: uuid.NewString() + "@test.invalid", Password: "password123",
	}); err != nil {
		t.Fatalf("register without a code: %v", err)
	}
}

func TestInviteResolveInviter(t *testing.T) {
	svc, _, client := testInviteEnv(t)
	ctx := context.Background()
	inviter, code := inviterWithCode(t, svc, client)

	got, err := svc.ResolveInviter(ctx, "  "+code+"  ")
	if err != nil {
		t.Fatalf("valid code rejected: %v", err)
	}
	if got.ID != inviter.ID {
		t.Fatalf("resolved %s, want %s", got.ID, inviter.ID)
	}

	for _, bad := range []string{"", "   ", "short", "has 0O1I"} {
		if _, err := svc.ResolveInviter(ctx, bad); !errors.Is(err, ErrInviteCodeMalformed) {
			t.Fatalf("code %q: want ErrInviteCodeMalformed, got %v", bad, err)
		}
	}
	if _, err := svc.ResolveInviter(ctx, "ZZZZZZZZ"); !errors.Is(err, ErrInviteCodeUnknown) {
		t.Fatalf("unknown code: want ErrInviteCodeUnknown, got %v", err)
	}
}

// --- qualification conditions ---

func TestInviteConditionNoneAutoPays(t *testing.T) {
	svc, settings, client := testInviteEnv(t)
	ctx := context.Background()
	setInviteProgram(t, settings, permissiveProgram())
	inviter, code := inviterWithCode(t, svc, client)

	invitee := newInviteTestUser(t, client)
	if err := svc.BindOnRegister(ctx, invitee.ID, code, "", ""); err != nil {
		t.Fatalf("bind: %v", err)
	}
	r := rewardOf(t, client, invitee.ID)
	if r.Status != entreward.StatusPaid {
		t.Fatalf("want paid, got %s (flags %v)", r.Status, r.RiskFlags)
	}
	if got := balanceOf(t, client, inviter.ID); !got.Equal(decimal.RequireFromString("5")) {
		t.Fatalf("inviter balance = %s, want 5", got)
	}
	// Idempotency: re-evaluating (and re-paying) an already-paid reward
	// must not double-credit.
	if err := svc.EvaluateSignup(ctx, invitee.ID); err != nil {
		t.Fatalf("re-eval: %v", err)
	}
	if _, err := svc.PayReward(ctx, r.ID, ""); err != nil {
		t.Fatalf("re-pay: %v", err)
	}
	if got := balanceOf(t, client, inviter.ID); !got.Equal(decimal.RequireFromString("5")) {
		t.Fatalf("double credit: balance = %s", got)
	}
}

func TestInviteConditionEmailVerified(t *testing.T) {
	svc, settings, client := testInviteEnv(t)
	ctx := context.Background()
	p := permissiveProgram()
	p.SignupCondition = InviteConditionEmailVerified
	setInviteProgram(t, settings, p)
	inviter, code := inviterWithCode(t, svc, client)

	invitee := newInviteTestUser(t, client)
	if err := svc.BindOnRegister(ctx, invitee.ID, code, "", ""); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if r := rewardOf(t, client, invitee.ID); r.Status != entreward.StatusPending {
		t.Fatalf("want pending before verification, got %s", r.Status)
	}
	if err := client.User.UpdateOneID(invitee.ID).SetEmailVerified(true).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if err := svc.EvaluateSignup(ctx, invitee.ID); err != nil {
		t.Fatalf("eval: %v", err)
	}
	if r := rewardOf(t, client, invitee.ID); r.Status != entreward.StatusPaid {
		t.Fatalf("want paid after verification, got %s", r.Status)
	}
	if got := balanceOf(t, client, inviter.ID); !got.Equal(decimal.RequireFromString("5")) {
		t.Fatalf("inviter balance = %s, want 5", got)
	}
}

func TestInviteConditionTopup(t *testing.T) {
	svc, settings, client := testInviteEnv(t)
	ctx := context.Background()

	t.Run("any_topup", func(t *testing.T) {
		p := permissiveProgram()
		p.SignupCondition = InviteConditionAnyTopup
		setInviteProgram(t, settings, p)
		_, code := inviterWithCode(t, svc, client)
		invitee := newInviteTestUser(t, client)
		if err := svc.BindOnRegister(ctx, invitee.ID, code, "", ""); err != nil {
			t.Fatal(err)
		}
		if r := rewardOf(t, client, invitee.ID); r.Status != entreward.StatusPending {
			t.Fatalf("want pending before topup, got %s", r.Status)
		}
		if err := client.BalanceTx.Create().
			SetUserID(invitee.ID).SetKind("gift").
			SetAmount(decimal.RequireFromString("100")).
			SetBalanceAfter(decimal.RequireFromString("100")).
			SetCurrency(CurrencyCNY).
			Exec(ctx); err != nil {
			t.Fatalf("gift tx: %v", err)
		}
		if err := svc.EvaluateSignup(ctx, invitee.ID); err != nil {
			t.Fatal(err)
		}
		if r := rewardOf(t, client, invitee.ID); r.Status != entreward.StatusPending {
			t.Fatalf("plan gift must not satisfy any_topup, got %s", r.Status)
		}
		addTopup(t, client, invitee.ID, "0.01")
		if err := svc.EvaluateSignup(ctx, invitee.ID); err != nil {
			t.Fatal(err)
		}
		if r := rewardOf(t, client, invitee.ID); r.Status != entreward.StatusPaid {
			t.Fatalf("want paid after any topup, got %s", r.Status)
		}
	})

	t.Run("min_topup", func(t *testing.T) {
		p := permissiveProgram()
		p.SignupCondition = InviteConditionMinTopup
		p.SignupMinTopupAmount = decimal.RequireFromString("50")
		setInviteProgram(t, settings, p)
		_, code := inviterWithCode(t, svc, client)
		invitee := newInviteTestUser(t, client)
		if err := svc.BindOnRegister(ctx, invitee.ID, code, "", ""); err != nil {
			t.Fatal(err)
		}
		addTopup(t, client, invitee.ID, "30")
		if err := svc.EvaluateSignup(ctx, invitee.ID); err != nil {
			t.Fatal(err)
		}
		if r := rewardOf(t, client, invitee.ID); r.Status != entreward.StatusPending {
			t.Fatalf("30 < 50 must stay pending, got %s", r.Status)
		}
		addTopup(t, client, invitee.ID, "25") // cumulative 55 >= 50
		if err := svc.EvaluateSignup(ctx, invitee.ID); err != nil {
			t.Fatal(err)
		}
		if r := rewardOf(t, client, invitee.ID); r.Status != entreward.StatusPaid {
			t.Fatalf("55 >= 50 must pay, got %s", r.Status)
		}
	})

	t.Run("min_topup_zero", func(t *testing.T) {
		p := permissiveProgram()
		p.SignupCondition = InviteConditionMinTopup
		p.SignupMinTopupAmount = decimal.Zero
		setInviteProgram(t, settings, p)
		inviter, code := inviterWithCode(t, svc, client)
		invitee := newInviteTestUser(t, client)
		if err := svc.BindOnRegister(ctx, invitee.ID, code, "", ""); err != nil {
			t.Fatal(err)
		}
		if r := rewardOf(t, client, invitee.ID); r.Status != entreward.StatusPaid {
			t.Fatalf("0 >= zero threshold must pay, got %s", r.Status)
		}
		if got := balanceOf(t, client, inviter.ID); !got.Equal(decimal.RequireFromString("5")) {
			t.Fatalf("inviter balance = %s, want 5", got)
		}
	})
}

func TestInvitePaidPlanThresholdAndProgress(t *testing.T) {
	svc, settings, client := testInviteEnv(t)
	ctx := context.Background()
	p := permissiveProgram()
	p.SignupCondition = InviteConditionMinTopup
	p.SignupMinTopupAmount = decimal.NewFromInt(50)
	p.RequireInviterEmailVerified = true
	p.AutoHoldOnRisk = true
	p.TopupRebateEnabled = true
	p.TopupRebateRatePct = decimal.NewFromInt(10)
	setInviteProgram(t, settings, p)
	inviter, code := inviterWithCode(t, svc, client)
	invitee := newInviteTestUser(t, client)
	if err := svc.BindOnRegister(ctx, invitee.ID, code, "", ""); err != nil {
		t.Fatal(err)
	}
	finance := NewFinanceSvc(svc.d, nil)
	finance.SetInvites(svc)
	plan, err := client.Plan.Create().SetName("paid-plan-" + uuid.NewString()).
		SetPrice(decimal.NewFromInt(49)).SetInitialBalance(decimal.NewFromInt(100)).
		SetPeriodDays(30).Save(ctx)
	if err != nil {
		t.Fatal(err)
	}
	order, err := finance.CreateOrder(ctx, CreateOrderInput{
		UserID: invitee.ID, Kind: "plan", PlanID: &plan.ID, Amount: plan.Price, Currency: CurrencyCNY,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := finance.MarkOrderPaid(ctx, order.ID); err != nil {
		t.Fatal(err)
	}
	checkView := func(paid string, status entreward.Status, held string) {
		t.Helper()
		view, err := svc.MyInvite(ctx, inviter.ID, 20, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(view.Invitees) != 1 {
			t.Fatalf("invitees = %d", len(view.Invitees))
		}
		i := view.Invitees[0]
		if !i.SignupPaidAmount.Equal(decimal.RequireFromString(paid)) ||
			!i.SignupRequiredAmount.Equal(decimal.NewFromInt(50)) ||
			i.SignupRewardStatus != string(status) ||
			!i.RebateHeld.Equal(decimal.RequireFromString(held)) ||
			!view.InviterEmailVerificationRequired {
			t.Fatalf("unexpected view: %+v", i)
		}
	}
	// Gift face value 100 must not replace the plan's cash price 49.
	checkView("49", entreward.StatusPending, "0")
	if err := svc.EvaluateSignup(ctx, invitee.ID); err != nil {
		t.Fatal(err)
	}
	// The reward already existed before the second purchase: its stored
	// min_topup snapshot must use the paid-plan basis without a migration.
	balanceOrder, err := finance.CreateOrder(ctx, CreateOrderInput{
		UserID: invitee.ID, Kind: "balance", Amount: decimal.NewFromInt(1), Currency: CurrencyCNY,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := finance.MarkOrderPaid(ctx, balanceOrder.ID); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		r := rewardOf(t, client, invitee.ID)
		n, err := client.InviteRebate.Query().Where(entrebate.SourceOrderIDEQ(balanceOrder.ID)).Count(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if r.Status == entreward.StatusHeld && n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("payment hook did not evaluate the paid-plan threshold")
		}
		time.Sleep(20 * time.Millisecond)
	}
	checkView("50", entreward.StatusHeld, "0.1")
	view, err := svc.MyInvite(ctx, inviter.ID, 20, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !view.Invitees[0].SignupEmailVerificationRequired || !view.Invitees[0].RebateEmailVerificationRequired {
		t.Fatal("missing inviter email verification reasons")
	}
	if got := view.Invitees[0]; !slices.Equal(got.SignupRewardHeldReasons, []string{"inviter_email_unverified"}) ||
		!slices.Equal(got.RebateHeldReasons, []string{"inviter_email_unverified"}) {
		t.Fatalf("held reasons = %v / %v", got.SignupRewardHeldReasons, got.RebateHeldReasons)
	}
	refund, err := client.SiteRefund.Create().SetOrderID(order.ID).SetUserID(invitee.ID).
		SetAmount(decimal.NewFromInt(49)).Save(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := finance.ApproveRefund(ctx, refund.ID, inviter.ID, ""); err != nil {
		t.Fatal(err)
	}
	checkView("1", entreward.StatusPending, "0.1")
	// A plan payment itself must trigger qualification, without another
	// balance topup or a dashboard read re-evaluating the reward.
	order, err = finance.CreateOrder(ctx, CreateOrderInput{
		UserID: invitee.ID, Kind: "plan", PlanID: &plan.ID, Amount: plan.Price, Currency: CurrencyCNY,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := finance.MarkOrderPaid(ctx, order.ID); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for rewardOf(t, client, invitee.ID).Status != entreward.StatusHeld {
		if time.Now().After(deadline) {
			t.Fatal("plan payment did not trigger qualification")
		}
		time.Sleep(20 * time.Millisecond)
	}
	checkView("50", entreward.StatusHeld, "0.1")
}

func TestInvitePaidTotalUsesCashAndApprovedRefunds(t *testing.T) {
	svc, _, client := testInviteEnv(t)
	ctx := context.Background()
	u := newInviteTestUser(t, client)
	order, err := client.SiteOrder.Create().SetOrderNo("T" + uuid.NewString()).SetUserID(u.ID).
		SetKind("balance").SetAmount(decimal.NewFromInt(40)).SetCreditAmount(decimal.NewFromInt(100)).
		SetStatus("partially_refunded").Save(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{"approved", "pending", "rejected"} {
		if _, err := client.SiteRefund.Create().SetOrderID(order.ID).SetUserID(u.ID).
			SetAmount(decimal.NewFromInt(10)).SetStatus(entsiterefund.Status(status)).Save(ctx); err != nil {
			t.Fatal(err)
		}
	}
	for _, status := range []string{"pending", "canceled", "refunded"} {
		if _, err := client.SiteOrder.Create().SetOrderNo("T" + uuid.NewString()).SetUserID(u.ID).
			SetKind("plan").SetAmount(decimal.NewFromInt(100)).SetStatus(entsiteorder.Status(status)).Save(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := client.SiteOrder.Create().SetOrderNo("T" + uuid.NewString()).SetUserID(u.ID).
		SetKind("plan").SetAmount(decimal.NewFromInt(100)).SetStatus("paid").
		SetPromotionPromoterUserID(uuid.New()).Save(ctx); err != nil {
		t.Fatal(err)
	}
	total, err := svc.inviteePaidTotal(ctx, u.ID)
	if err != nil || !total.Equal(decimal.NewFromInt(30)) {
		t.Fatalf("paid total = %s, err = %v, want 30", total, err)
	}
}

func TestInviteAlreadyPaidLegacyRewardKeepsOriginalBasis(t *testing.T) {
	svc, settings, client := testInviteEnv(t)
	ctx := context.Background()
	p := permissiveProgram()
	p.SignupCondition = InviteConditionMinTopup
	p.SignupMinTopupAmount = decimal.NewFromInt(50)
	setInviteProgram(t, settings, p)
	_, code := inviterWithCode(t, svc, client)
	u := newInviteTestUser(t, client)
	if err := svc.BindOnRegister(ctx, u.ID, code, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := client.BalanceTx.Create().SetUserID(u.ID).SetKind("topup").
		SetAmount(decimal.NewFromInt(50)).SetBalanceAfter(decimal.NewFromInt(50)).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	r := rewardOf(t, client, u.ID)
	if _, err := client.InviteReward.UpdateOneID(r.ID).SetStatus(entreward.StatusPaid).
		SetConditionSnapshot(`{"condition":"min_topup","min_topup_amount":"50"}`).Save(ctx); err != nil {
		t.Fatal(err)
	}
	svc.ReconcileRefund(ctx, u.ID, uuid.New(), decimal.Zero)
	if r := rewardOf(t, client, u.ID); r.Status != entreward.StatusPaid {
		t.Fatalf("an unrelated plan refund must not revoke a legacy reward: %s", r.Status)
	}
}

// --- risk holds ---

func TestInviteSameIPHold(t *testing.T) {
	svc, settings, client := testInviteEnv(t)
	ctx := context.Background()
	p := permissiveProgram()
	p.SameIPDailyCap = 1
	p.AutoHoldOnRisk = true // holding is opt-in since 2026-09-03
	setInviteProgram(t, settings, p)
	_, code := inviterWithCode(t, svc, client)

	first := newInviteTestUser(t, client)
	if err := svc.BindOnRegister(ctx, first.ID, code, "198.51.100.7", ""); err != nil {
		t.Fatal(err)
	}
	if r := rewardOf(t, client, first.ID); r.Status != entreward.StatusPaid {
		t.Fatalf("first same-IP signup should pay, got %s (flags %v)", r.Status, r.RiskFlags)
	}
	second := newInviteTestUser(t, client)
	if err := svc.BindOnRegister(ctx, second.ID, code, "198.51.100.7", ""); err != nil {
		t.Fatal(err)
	}
	r := rewardOf(t, client, second.ID)
	if r.Status != entreward.StatusHeld {
		t.Fatalf("overflow same-IP signup should hold, got %s", r.Status)
	}
	if !containsFlag(r.RiskFlags, riskSameIPDailyCap) {
		t.Fatalf("missing %s flag: %v", riskSameIPDailyCap, r.RiskFlags)
	}
}

func TestInviteDailyCountCapHold(t *testing.T) {
	svc, settings, client := testInviteEnv(t)
	ctx := context.Background()
	p := permissiveProgram()
	p.SignupDailyCountCapPerInviter = 1
	p.AutoHoldOnRisk = true // holding is opt-in since 2026-09-03
	setInviteProgram(t, settings, p)
	inviter, code := inviterWithCode(t, svc, client)

	first := newInviteTestUser(t, client)
	if err := svc.BindOnRegister(ctx, first.ID, code, "203.0.113.1", ""); err != nil {
		t.Fatal(err)
	}
	if r := rewardOf(t, client, first.ID); r.Status != entreward.StatusPaid {
		t.Fatalf("first signup of the day should pay, got %s (flags %v)", r.Status, r.RiskFlags)
	}
	second := newInviteTestUser(t, client)
	if err := svc.BindOnRegister(ctx, second.ID, code, "203.0.113.2", ""); err != nil {
		t.Fatal(err)
	}
	r := rewardOf(t, client, second.ID)
	if r.Status != entreward.StatusHeld {
		t.Fatalf("over-cap signup should hold, got %s", r.Status)
	}
	if !containsFlag(r.RiskFlags, riskDailyCountCap) {
		t.Fatalf("missing %s flag: %v", riskDailyCountCap, r.RiskFlags)
	}
	if got := balanceOf(t, client, inviter.ID); !got.Equal(decimal.RequireFromString("5")) {
		t.Fatalf("only the first payout may credit: balance = %s", got)
	}
}

func TestInviteDailyCountCapConcurrent(t *testing.T) {
	svc, settings, client := testInviteEnv(t)
	ctx := context.Background()
	p := permissiveProgram()
	p.SignupCondition = InviteConditionEmailVerified
	p.SignupDailyCountCapPerInviter = 1
	p.AutoHoldOnRisk = true // holding is opt-in since 2026-09-03
	setInviteProgram(t, settings, p)
	inviter, code := inviterWithCode(t, svc, client)

	const count = 12
	invitees := make([]*ent.User, 0, count)
	for i := 0; i < count; i++ {
		invitee := newInviteTestUser(t, client)
		if err := svc.BindOnRegister(ctx, invitee.ID, code, "", ""); err != nil {
			t.Fatal(err)
		}
		if err := client.User.UpdateOneID(invitee.ID).SetEmailVerified(true).Exec(ctx); err != nil {
			t.Fatal(err)
		}
		invitees = append(invitees, invitee)
	}

	start := make(chan struct{})
	errs := make(chan error, count)
	var wg sync.WaitGroup
	for _, invitee := range invitees {
		wg.Add(1)
		go func(id uuid.UUID) {
			defer wg.Done()
			<-start
			errs <- svc.EvaluateSignup(ctx, id)
		}(invitee.ID)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent evaluate: %v", err)
		}
	}

	rewards, err := client.InviteReward.Query().
		Where(entreward.InviterUserIDEQ(inviter.ID)).
		All(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var paid, held int
	for _, reward := range rewards {
		switch reward.Status {
		case entreward.StatusPaid:
			paid++
		case entreward.StatusHeld:
			held++
		}
	}
	if paid != 1 || held != count-1 {
		t.Fatalf("concurrent cap: paid=%d held=%d, want 1/%d", paid, held, count-1)
	}
	if got := balanceOf(t, client, inviter.ID); !got.Equal(decimal.RequireFromString("5")) {
		t.Fatalf("concurrent cap credited %s, want 5", got)
	}
}

// With holding off (the default), a risk flag is still recorded on the
// payout row — it just does not stop the payout.
func TestInviteRiskFlagsRecordedWithoutHolding(t *testing.T) {
	svc, settings, client := testInviteEnv(t)
	ctx := context.Background()
	p := permissiveProgram()
	p.RequireInviterEmailVerified = true // test inviters are unverified
	if p.AutoHoldOnRisk {
		t.Fatal("AutoHoldOnRisk must default to false")
	}
	setInviteProgram(t, settings, p)
	inviter, code := inviterWithCode(t, svc, client)
	invitee := newInviteTestUser(t, client)
	if err := svc.BindOnRegister(ctx, invitee.ID, code, "", ""); err != nil {
		t.Fatal(err)
	}
	r := rewardOf(t, client, invitee.ID)
	if r.Status != entreward.StatusPaid {
		t.Fatalf("want paid, got %s", r.Status)
	}
	if !containsFlag(r.RiskFlags, riskInviterEmailUnverified) {
		t.Fatalf("risk flag must still be recorded, got %v", r.RiskFlags)
	}
	if got := balanceOf(t, client, inviter.ID); !got.Equal(decimal.RequireFromString("5")) {
		t.Fatalf("balance = %s, want 5", got)
	}
}

func TestInviteInviterRiskHold(t *testing.T) {
	svc, settings, client := testInviteEnv(t)
	ctx := context.Background()
	p := permissiveProgram()
	p.RequireInviterEmailVerified = true // test inviters are unverified
	p.AutoHoldOnRisk = true              // holding is opt-in since 2026-09-03
	setInviteProgram(t, settings, p)
	_, code := inviterWithCode(t, svc, client)

	invitee := newInviteTestUser(t, client)
	if err := svc.BindOnRegister(ctx, invitee.ID, code, "", ""); err != nil {
		t.Fatal(err)
	}
	r := rewardOf(t, client, invitee.ID)
	if r.Status != entreward.StatusHeld || !containsFlag(r.RiskFlags, riskInviterEmailUnverified) {
		t.Fatalf("want held/%s, got %s %v", riskInviterEmailUnverified, r.Status, r.RiskFlags)
	}
}

// --- manual-review amount threshold ---

// ReviewAmountThreshold is a money control, not a fraud heuristic: a single
// payout at/over the threshold holds even with AutoHoldOnRisk off, the
// amount is never reduced, and a human pay releases it.
func TestInviteReviewAmountThreshold(t *testing.T) {
	svc, settings, client := testInviteEnv(t)
	ctx := context.Background()

	t.Run("zero_threshold_pays", func(t *testing.T) {
		p := permissiveProgram() // ReviewAmountThreshold defaults to 0
		setInviteProgram(t, settings, p)
		_, code := inviterWithCode(t, svc, client)
		invitee := newInviteTestUser(t, client)
		if err := svc.BindOnRegister(ctx, invitee.ID, code, "", ""); err != nil {
			t.Fatal(err)
		}
		if r := rewardOf(t, client, invitee.ID); r.Status != entreward.StatusPaid {
			t.Fatalf("zero threshold must not hold, got %s (flags %v)", r.Status, r.RiskFlags)
		}
	})

	t.Run("at_threshold_holds_even_without_auto_hold", func(t *testing.T) {
		p := permissiveProgram() // reward amount 5
		p.AutoHoldOnRisk = false
		p.ReviewAmountThreshold = decimal.RequireFromString("5") // >= is inclusive
		setInviteProgram(t, settings, p)
		inviter, code := inviterWithCode(t, svc, client)
		invitee := newInviteTestUser(t, client)
		if err := svc.BindOnRegister(ctx, invitee.ID, code, "", ""); err != nil {
			t.Fatal(err)
		}
		r := rewardOf(t, client, invitee.ID)
		if r.Status != entreward.StatusHeld || !containsFlag(r.RiskFlags, riskAmountOverThreshold) {
			t.Fatalf("want held/%s, got %s %v", riskAmountOverThreshold, r.Status, r.RiskFlags)
		}
		if !r.Amount.Equal(decimal.RequireFromString("5")) {
			t.Fatalf("threshold is not a cap: amount = %s, want 5", r.Amount)
		}
		if got := balanceOf(t, client, inviter.ID); !got.IsZero() {
			t.Fatalf("held reward must not credit: balance = %s", got)
		}
		// Re-evaluation keeps it held; only a human pay releases it.
		if err := svc.EvaluateSignup(ctx, invitee.ID); err != nil {
			t.Fatal(err)
		}
		if r = rewardOf(t, client, invitee.ID); r.Status != entreward.StatusHeld {
			t.Fatalf("re-eval must keep held, got %s", r.Status)
		}
		if _, err := svc.PayReward(ctx, r.ID, "manually reviewed"); err != nil {
			t.Fatal(err)
		}
		if got := balanceOf(t, client, inviter.ID); !got.Equal(decimal.RequireFromString("5")) {
			t.Fatalf("manual pay must credit the full amount: balance = %s", got)
		}
	})

	t.Run("below_threshold_follows_existing_risk_rules", func(t *testing.T) {
		// Below the threshold with a risk flag and AutoHoldOnRisk on:
		// the existing hold path still applies, no amount flag.
		p := permissiveProgram()
		p.ReviewAmountThreshold = decimal.RequireFromString("10") // reward 5 < 10
		p.RequireInviterEmailVerified = true                      // test inviters are unverified
		p.AutoHoldOnRisk = true
		setInviteProgram(t, settings, p)
		_, code := inviterWithCode(t, svc, client)
		invitee := newInviteTestUser(t, client)
		if err := svc.BindOnRegister(ctx, invitee.ID, code, "", ""); err != nil {
			t.Fatal(err)
		}
		r := rewardOf(t, client, invitee.ID)
		if r.Status != entreward.StatusHeld || !containsFlag(r.RiskFlags, riskInviterEmailUnverified) {
			t.Fatalf("want held/%s, got %s %v", riskInviterEmailUnverified, r.Status, r.RiskFlags)
		}
		if containsFlag(r.RiskFlags, riskAmountOverThreshold) {
			t.Fatalf("below threshold must not flag %s: %v", riskAmountOverThreshold, r.RiskFlags)
		}

		// Same risk flag with AutoHoldOnRisk off: still pays below the
		// threshold — the threshold does not change the heuristics.
		p.AutoHoldOnRisk = false
		setInviteProgram(t, settings, p)
		_, code2 := inviterWithCode(t, svc, client)
		invitee2 := newInviteTestUser(t, client)
		if err := svc.BindOnRegister(ctx, invitee2.ID, code2, "", ""); err != nil {
			t.Fatal(err)
		}
		if r := rewardOf(t, client, invitee2.ID); r.Status != entreward.StatusPaid {
			t.Fatalf("below threshold with auto-hold off must pay, got %s (flags %v)", r.Status, r.RiskFlags)
		}
	})
}

func TestInviteRebateReviewAmountThreshold(t *testing.T) {
	svc, settings, client := testInviteEnv(t)
	ctx := context.Background()
	p := rebateProgram() // 10% rate
	p.AutoHoldOnRisk = false
	p.ReviewAmountThreshold = decimal.RequireFromString("10")
	setInviteProgram(t, settings, p)
	inviter, code := inviterWithCode(t, svc, client)
	invitee := newInviteTestUser(t, client)
	if err := svc.BindOnRegister(ctx, invitee.ID, code, "", ""); err != nil {
		t.Fatal(err)
	}

	// 10% of 100 = 10 >= threshold 10 → held, full amount, no auto-pay.
	orderID := uuid.New()
	if err := svc.ProcessTopupRebate(ctx, invitee.ID, orderID, decimal.RequireFromString("100")); err != nil {
		t.Fatal(err)
	}
	row, err := client.InviteRebate.Query().Where(entrebate.SourceOrderIDEQ(orderID)).Only(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != entrebate.StatusHeld || !containsFlag(row.RiskFlags, riskAmountOverThreshold) {
		t.Fatalf("want held/%s, got %s %v", riskAmountOverThreshold, row.Status, row.RiskFlags)
	}
	if !row.RebateAmount.Equal(decimal.RequireFromString("10")) {
		t.Fatalf("threshold is not a cap: rebate = %s, want 10", row.RebateAmount)
	}
	if got := balanceOf(t, client, inviter.ID); !got.IsZero() {
		t.Fatalf("held rebate must not credit: balance = %s", got)
	}
	// A human pay releases the full amount.
	if _, err := svc.PayRebate(ctx, row.ID, "manually reviewed"); err != nil {
		t.Fatal(err)
	}
	if got := balanceOf(t, client, inviter.ID); !got.Equal(decimal.RequireFromString("10")) {
		t.Fatalf("manual pay must credit the full amount: balance = %s", got)
	}

	// 10% of 50 = 5 < threshold 10 → auto-pays as before.
	if err := svc.ProcessTopupRebate(ctx, invitee.ID, uuid.New(), decimal.RequireFromString("50")); err != nil {
		t.Fatal(err)
	}
	if got := balanceOf(t, client, inviter.ID); !got.Equal(decimal.RequireFromString("15")) {
		t.Fatalf("below-threshold rebate must auto-pay: balance = %s, want 15", got)
	}
}

// --- disabled program ---

func TestInviteDisabledProgramDoesNotPay(t *testing.T) {
	svc, settings, client := testInviteEnv(t)
	ctx := context.Background()

	// Disabled at bind time: binding exists, but no reward row is created —
	// enabling later is not retroactive.
	setInviteProgram(t, settings, DefaultInviteProgram())
	_, code := inviterWithCode(t, svc, client)
	invitee := newInviteTestUser(t, client)
	if err := svc.BindOnRegister(ctx, invitee.ID, code, "", ""); err != nil {
		t.Fatal(err)
	}
	if n, _ := client.InviteBinding.Query().Where(entbinding.InviteeUserIDEQ(invitee.ID)).Count(ctx); n != 1 {
		t.Fatal("binding must be created even while the program is off")
	}
	if n, _ := client.InviteReward.Query().Where(entreward.InviteeUserIDEQ(invitee.ID)).Count(ctx); n != 0 {
		t.Fatal("no reward row while the program is off")
	}
	setInviteProgram(t, settings, permissiveProgram())
	if err := svc.EvaluateSignup(ctx, invitee.ID); err != nil {
		t.Fatal(err)
	}
	if n, _ := client.InviteReward.Query().Where(entreward.InviteeUserIDEQ(invitee.ID)).Count(ctx); n != 0 {
		t.Fatal("enabling the program must not retroactively create rewards")
	}

	// Disabled after the reward exists: evaluation freezes, nothing pays.
	setInviteProgram(t, settings, permissiveProgram())
	inviter2, code2 := inviterWithCode(t, svc, client)
	p := permissiveProgram()
	p.SignupCondition = InviteConditionEmailVerified
	setInviteProgram(t, settings, p)
	invitee2 := newInviteTestUser(t, client)
	if err := svc.BindOnRegister(ctx, invitee2.ID, code2, "", ""); err != nil {
		t.Fatal(err)
	}
	off := p
	off.Enabled = false
	setInviteProgram(t, settings, off)
	_ = client.User.UpdateOneID(invitee2.ID).SetEmailVerified(true).Exec(ctx)
	if err := svc.EvaluateSignup(ctx, invitee2.ID); err != nil {
		t.Fatal(err)
	}
	if r := rewardOf(t, client, invitee2.ID); r.Status != entreward.StatusPending {
		t.Fatalf("disabled program must leave the reward pending, got %s", r.Status)
	}
	if got := balanceOf(t, client, inviter2.ID); !got.IsZero() {
		t.Fatalf("disabled program paid out: %s", got)
	}
}

// --- topup rebate ---

func rebateProgram() InviteProgramSettings {
	p := permissiveProgram()
	p.SignupRewardEnabled = false
	p.TopupRebateEnabled = true
	p.TopupRebateRatePct = decimal.RequireFromString("10")
	return p
}

func TestInviteRebateIdempotent(t *testing.T) {
	svc, settings, client := testInviteEnv(t)
	ctx := context.Background()
	setInviteProgram(t, settings, rebateProgram())
	inviter, code := inviterWithCode(t, svc, client)
	invitee := newInviteTestUser(t, client)
	if err := svc.BindOnRegister(ctx, invitee.ID, code, "", ""); err != nil {
		t.Fatal(err)
	}

	orderID := uuid.New()
	spend := decimal.RequireFromString("100")
	for i := 0; i < 3; i++ { // duplicate deliveries must be no-ops
		if err := svc.ProcessTopupRebate(ctx, invitee.ID, orderID, spend); err != nil {
			t.Fatalf("process %d: %v", i, err)
		}
	}
	rows, err := client.InviteRebate.Query().Where(entrebate.SourceOrderIDEQ(orderID)).All(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("want exactly 1 rebate row, got %d", len(rows))
	}
	if rows[0].Status != entrebate.StatusPaid || !rows[0].RebateAmount.Equal(decimal.RequireFromString("10")) {
		t.Fatalf("want paid 10 (10%% of 100), got %s %s", rows[0].Status, rows[0].RebateAmount)
	}
	if got := balanceOf(t, client, inviter.ID); !got.Equal(decimal.RequireFromString("10")) {
		t.Fatalf("inviter balance = %s, want 10", got)
	}
}

func TestInviteRebateSkipsUnboundAndDisabled(t *testing.T) {
	svc, settings, client := testInviteEnv(t)
	ctx := context.Background()
	setInviteProgram(t, settings, rebateProgram())

	// No binding → no rebate row.
	stranger := newInviteTestUser(t, client)
	if err := svc.ProcessTopupRebate(ctx, stranger.ID, uuid.New(), decimal.RequireFromString("100")); err != nil {
		t.Fatal(err)
	}
	if n, _ := client.InviteRebate.Query().Where(entrebate.InviteeUserIDEQ(stranger.ID)).Count(ctx); n != 0 {
		t.Fatal("unbound user must not rebate")
	}

	// Disabled program → no rebate row even for a bound invitee.
	_, code := inviterWithCode(t, svc, client)
	invitee := newInviteTestUser(t, client)
	if err := svc.BindOnRegister(ctx, invitee.ID, code, "", ""); err != nil {
		t.Fatal(err)
	}
	setInviteProgram(t, settings, DefaultInviteProgram())
	if err := svc.ProcessTopupRebate(ctx, invitee.ID, uuid.New(), decimal.RequireFromString("100")); err != nil {
		t.Fatal(err)
	}
	if n, _ := client.InviteRebate.Query().Where(entrebate.InviteeUserIDEQ(invitee.ID)).Count(ctx); n != 0 {
		t.Fatal("disabled program must not rebate")
	}
}

func TestInviteRebateSkipsExclusiveGroupInvitee(t *testing.T) {
	svc, settings, client := testInviteEnv(t)
	ctx := context.Background()
	setInviteProgram(t, settings, rebateProgram())

	// An exclusive-group user may still invite and earn from ordinary invitees.
	inviter, code := inviterWithCode(t, svc, client)
	grantExclusiveGroup(t, client, inviter.ID)
	ordinary := newInviteTestUser(t, client)
	if err := svc.BindOnRegister(ctx, ordinary.ID, code, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := svc.ProcessTopupRebate(ctx, ordinary.ID, uuid.New(), decimal.RequireFromString("100")); err != nil {
		t.Fatal(err)
	}
	if n, _ := client.InviteRebate.Query().Where(entrebate.InviteeUserIDEQ(ordinary.ID)).Count(ctx); n != 1 {
		t.Fatalf("ordinary invitee rebates = %d, want 1", n)
	}

	// An exclusive-group invitee's topups pay their inviter nothing.
	vip := newInviteTestUser(t, client)
	if err := svc.BindOnRegister(ctx, vip.ID, code, "", ""); err != nil {
		t.Fatal(err)
	}
	grantExclusiveGroup(t, client, vip.ID)
	if err := svc.ProcessTopupRebate(ctx, vip.ID, uuid.New(), decimal.RequireFromString("100")); err != nil {
		t.Fatal(err)
	}
	if n, _ := client.InviteRebate.Query().Where(entrebate.InviteeUserIDEQ(vip.ID)).Count(ctx); n != 0 {
		t.Fatalf("exclusive invitee rebates = %d, want 0", n)
	}
}

func TestInviteRebateCapPerInvitee(t *testing.T) {
	svc, settings, client := testInviteEnv(t)
	ctx := context.Background()
	p := rebateProgram()
	p.TopupRebateCapPerInvitee = decimal.RequireFromString("15")
	setInviteProgram(t, settings, p)
	inviter, code := inviterWithCode(t, svc, client)
	invitee := newInviteTestUser(t, client)
	if err := svc.BindOnRegister(ctx, invitee.ID, code, "", ""); err != nil {
		t.Fatal(err)
	}

	spend := decimal.RequireFromString("100") // 10% → 10 per charge
	if err := svc.ProcessTopupRebate(ctx, invitee.ID, uuid.New(), spend); err != nil {
		t.Fatal(err)
	}
	if err := svc.ProcessTopupRebate(ctx, invitee.ID, uuid.New(), spend); err != nil {
		t.Fatal(err) // remaining 5 → clamped
	}
	if err := svc.ProcessTopupRebate(ctx, invitee.ID, uuid.New(), spend); err != nil {
		t.Fatal(err) // remaining 0 → skipped
	}
	rows, _ := client.InviteRebate.Query().Where(entrebate.InviteeUserIDEQ(invitee.ID)).All(ctx)
	if len(rows) != 2 {
		t.Fatalf("want 2 rebate rows (third over cap), got %d", len(rows))
	}
	if got := balanceOf(t, client, inviter.ID); !got.Equal(decimal.RequireFromString("15")) {
		t.Fatalf("total rebates must clamp to the 15 cap, got %s", got)
	}
}

// The per-invitee lifetime cap is enforced under the inviter row lock, so
// a burst of concurrent orders for the same invitee must still commit
// exactly the cap and no more.
func TestInviteTopupRebateCapConcurrent(t *testing.T) {
	svc, settings, client := testInviteEnv(t)
	ctx := context.Background()
	p := rebateProgram()
	p.TopupRebateCapPerInvitee = decimal.RequireFromString("15")
	setInviteProgram(t, settings, p)
	inviter, code := inviterWithCode(t, svc, client)
	invitee := newInviteTestUser(t, client)
	if err := svc.BindOnRegister(ctx, invitee.ID, code, "", ""); err != nil {
		t.Fatal(err)
	}

	const count = 12
	start := make(chan struct{})
	errs := make(chan error, count)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- svc.ProcessTopupRebate(ctx, invitee.ID, uuid.New(), decimal.RequireFromString("100"))
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent rebate: %v", err)
		}
	}

	rows, err := client.InviteRebate.Query().
		Where(entrebate.InviterUserIDEQ(inviter.ID)).
		All(ctx)
	if err != nil {
		t.Fatal(err)
	}
	total := decimal.Zero
	for _, row := range rows {
		total = total.Add(row.RebateAmount)
	}
	if !total.Equal(decimal.RequireFromString("15")) {
		t.Fatalf("committed rebates = %s, want cap 15", total)
	}
	if got := balanceOf(t, client, inviter.ID); !got.Equal(decimal.RequireFromString("15")) {
		t.Fatalf("concurrent rebates credited %s, want 15", got)
	}
}

// The rebate is truncated to whole cents, never rounded up: a 10% rebate on
// 0.55 is 0.055, which must pay 0.05 rather than 0.06.
func TestInviteTopupRebateTruncatesToCents(t *testing.T) {
	got := topupRebateAmount(decimal.RequireFromString("0.55"), decimal.RequireFromString("10"))
	if !got.Equal(decimal.RequireFromString("0.05")) {
		t.Fatalf("topupRebateAmount(0.55, 10%%) = %s, want 0.05", got)
	}
	// Below one cent there is nothing to pay at all.
	if got := topupRebateAmount(decimal.RequireFromString("0.05"), decimal.RequireFromString("10")); !got.IsZero() {
		t.Fatalf("sub-cent rebate = %s, want 0", got)
	}
}

// --- refund reconciliation ---

// addRefundTx records an approved cash refund against the test order.
func addRefundTx(t *testing.T, client *ent.Client, userID uuid.UUID, amount string) {
	t.Helper()
	ctx := context.Background()
	orders, err := client.SiteOrder.Query().Where(entsiteorder.UserIDEQ(userID)).All(ctx)
	if err != nil || len(orders) == 0 {
		t.Fatalf("refund order: %v", err)
	}
	if _, err := client.SiteRefund.Create().SetOrderID(orders[0].ID).SetUserID(userID).
		SetAmount(decimal.RequireFromString(amount)).SetStatus("approved").Save(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestInviteRefundClawsBackPaidRebate(t *testing.T) {
	svc, settings, client := testInviteEnv(t)
	ctx := context.Background()
	setInviteProgram(t, settings, rebateProgram())
	inviter, code := inviterWithCode(t, svc, client)
	invitee := newInviteTestUser(t, client)
	if err := svc.BindOnRegister(ctx, invitee.ID, code, "", ""); err != nil {
		t.Fatal(err)
	}
	orderID := uuid.New()
	if err := svc.ProcessTopupRebate(ctx, invitee.ID, orderID, decimal.RequireFromString("100")); err != nil {
		t.Fatal(err)
	}
	if got := balanceOf(t, client, inviter.ID); !got.Equal(decimal.RequireFromString("10")) {
		t.Fatalf("inviter balance after rebate = %s, want 10", got)
	}

	// Partial refund of 40 on a 100 order leaves 60 earning: 6 of the 10
	// stays, 4 comes back.
	svc.ReconcileRefund(ctx, invitee.ID, orderID, decimal.RequireFromString("40"))
	if got := balanceOf(t, client, inviter.ID); !got.Equal(decimal.RequireFromString("6")) {
		t.Fatalf("balance after partial refund = %s, want 6", got)
	}
	// Replaying the SAME cumulative total must not debit twice.
	svc.ReconcileRefund(ctx, invitee.ID, orderID, decimal.RequireFromString("40"))
	if got := balanceOf(t, client, inviter.ID); !got.Equal(decimal.RequireFromString("6")) {
		t.Fatalf("replayed reconcile double-charged: %s, want 6", got)
	}
	// A second refund brings the cumulative total to 100 — everything goes.
	svc.ReconcileRefund(ctx, invitee.ID, orderID, decimal.RequireFromString("100"))
	if got := balanceOf(t, client, inviter.ID); !got.IsZero() {
		t.Fatalf("balance after full refund = %s, want 0", got)
	}
	row, err := client.InviteRebate.Query().Where(entrebate.SourceOrderIDEQ(orderID)).Only(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != entrebate.StatusPaid {
		t.Fatalf("clawback must not rewrite payout history, status = %s", row.Status)
	}
	if !row.RebateAmount.Equal(decimal.RequireFromString("10")) ||
		!row.ClawedBackAmount.Equal(decimal.RequireFromString("10")) {
		t.Fatalf("want rebate 10 clawed back 10, got %s / %s", row.RebateAmount, row.ClawedBackAmount)
	}
}

// A clawback may push the inviter negative: they were credited cash that
// has since been returned, and the debt is real.
func TestInviteRefundClawbackMayGoNegative(t *testing.T) {
	svc, settings, client := testInviteEnv(t)
	ctx := context.Background()
	setInviteProgram(t, settings, rebateProgram())
	inviter, code := inviterWithCode(t, svc, client)
	invitee := newInviteTestUser(t, client)
	if err := svc.BindOnRegister(ctx, invitee.ID, code, "", ""); err != nil {
		t.Fatal(err)
	}
	orderID := uuid.New()
	if err := svc.ProcessTopupRebate(ctx, invitee.ID, orderID, decimal.RequireFromString("100")); err != nil {
		t.Fatal(err)
	}
	// The inviter spends the rebate before the refund lands.
	if err := client.User.UpdateOneID(inviter.ID).SetBalance(decimal.Zero).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	svc.ReconcileRefund(ctx, invitee.ID, orderID, decimal.RequireFromString("100"))
	if got := balanceOf(t, client, inviter.ID); !got.Equal(decimal.RequireFromString("-10")) {
		t.Fatalf("balance after clawback = %s, want -10", got)
	}
}

// A held rebate that was never credited is shrunk (and voided when it hits
// zero) rather than debited.
func TestInviteRefundVoidsUnpaidRebate(t *testing.T) {
	svc, settings, client := testInviteEnv(t)
	ctx := context.Background()
	p := rebateProgram()
	p.ReviewAmountThreshold = decimal.RequireFromString("1") // force a hold
	setInviteProgram(t, settings, p)
	inviter, code := inviterWithCode(t, svc, client)
	invitee := newInviteTestUser(t, client)
	if err := svc.BindOnRegister(ctx, invitee.ID, code, "", ""); err != nil {
		t.Fatal(err)
	}
	orderID := uuid.New()
	if err := svc.ProcessTopupRebate(ctx, invitee.ID, orderID, decimal.RequireFromString("100")); err != nil {
		t.Fatal(err)
	}
	row, err := client.InviteRebate.Query().Where(entrebate.SourceOrderIDEQ(orderID)).Only(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != entrebate.StatusHeld {
		t.Fatalf("setup: want held, got %s", row.Status)
	}
	svc.ReconcileRefund(ctx, invitee.ID, orderID, decimal.RequireFromString("100"))
	row, err = client.InviteRebate.Query().Where(entrebate.SourceOrderIDEQ(orderID)).Only(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// rebate_amount stays the payout snapshot; the whole of it is recorded
	// as clawed back, leaving nothing payable.
	if row.Status != entrebate.StatusVoided ||
		!row.RebateAmount.Equal(decimal.RequireFromString("10")) ||
		!row.ClawedBackAmount.Equal(decimal.RequireFromString("10")) {
		t.Fatalf("want voided/10/10, got %s/%s/%s", row.Status, row.RebateAmount, row.ClawedBackAmount)
	}
	if _, err := svc.PayRebate(ctx, row.ID, "manual"); !errors.Is(err, ErrInviteInvalidState) {
		t.Fatalf("a fully clawed-back rebate must not be payable, got %v", err)
	}
	if got := balanceOf(t, client, inviter.ID); !got.IsZero() {
		t.Fatalf("an unpaid rebate must not move money, balance = %s", got)
	}
}

// Topping up to clear min_topup, collecting the reward, then refunding must
// not leave the reward paid.
func TestInviteRefundVoidsPaidSignupReward(t *testing.T) {
	svc, settings, client := testInviteEnv(t)
	ctx := context.Background()
	p := permissiveProgram()
	p.SignupCondition = InviteConditionMinTopup
	p.SignupMinTopupAmount = decimal.RequireFromString("50")
	setInviteProgram(t, settings, p)
	inviter, code := inviterWithCode(t, svc, client)
	invitee := newInviteTestUser(t, client)
	if err := svc.BindOnRegister(ctx, invitee.ID, code, "", ""); err != nil {
		t.Fatal(err)
	}
	addTopup(t, client, invitee.ID, "60")
	if err := svc.EvaluateSignup(ctx, invitee.ID); err != nil {
		t.Fatal(err)
	}
	if r := rewardOf(t, client, invitee.ID); r.Status != entreward.StatusPaid {
		t.Fatalf("setup: want paid reward, got %s", r.Status)
	}
	if got := balanceOf(t, client, inviter.ID); !got.Equal(decimal.RequireFromString("5")) {
		t.Fatalf("setup: inviter balance = %s, want 5", got)
	}

	addRefundTx(t, client, invitee.ID, "60")
	svc.ReconcileRefund(ctx, invitee.ID, uuid.New(), decimal.RequireFromString("60"))
	if r := rewardOf(t, client, invitee.ID); r.Status != entreward.StatusVoided {
		t.Fatalf("reward after refund = %s, want voided", r.Status)
	}
	if got := balanceOf(t, client, inviter.ID); !got.IsZero() {
		t.Fatalf("reward was not clawed back, balance = %s", got)
	}
	// Idempotent: a second reconcile finds nothing left to reverse.
	svc.ReconcileRefund(ctx, invitee.ID, uuid.New(), decimal.RequireFromString("60"))
	if got := balanceOf(t, client, inviter.ID); !got.IsZero() {
		t.Fatalf("second reconcile double-charged: %s", got)
	}
}

// A partial refund that still leaves the invitee above the threshold keeps
// the reward, and conditions that a refund cannot affect are never touched.
func TestInviteRefundKeepsStillQualifiedReward(t *testing.T) {
	svc, settings, client := testInviteEnv(t)
	ctx := context.Background()
	p := permissiveProgram()
	p.SignupCondition = InviteConditionMinTopup
	p.SignupMinTopupAmount = decimal.RequireFromString("50")
	setInviteProgram(t, settings, p)
	inviter, code := inviterWithCode(t, svc, client)
	invitee := newInviteTestUser(t, client)
	if err := svc.BindOnRegister(ctx, invitee.ID, code, "", ""); err != nil {
		t.Fatal(err)
	}
	addTopup(t, client, invitee.ID, "100")
	if err := svc.EvaluateSignup(ctx, invitee.ID); err != nil {
		t.Fatal(err)
	}
	addRefundTx(t, client, invitee.ID, "20") // net 80, still >= 50
	svc.ReconcileRefund(ctx, invitee.ID, uuid.New(), decimal.RequireFromString("20"))
	if r := rewardOf(t, client, invitee.ID); r.Status != entreward.StatusPaid {
		t.Fatalf("still-qualified reward was reversed: %s", r.Status)
	}
	if got := balanceOf(t, client, inviter.ID); !got.Equal(decimal.RequireFromString("5")) {
		t.Fatalf("balance = %s, want 5", got)
	}
}

// inviteePaidTotal is what min_topup reads; it must net approved refunds out and
// must never report a negative total.
func TestInviteeTopupTotalNetsRefunds(t *testing.T) {
	svc, _, client := testInviteEnv(t)
	ctx := context.Background()
	u := newInviteTestUser(t, client)
	addTopup(t, client, u.ID, "100")
	addRefundTx(t, client, u.ID, "30")
	got, err := svc.inviteePaidTotal(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(decimal.RequireFromString("70")) {
		t.Fatalf("net topup = %s, want 70", got)
	}
	addRefundTx(t, client, u.ID, "200") // over-refunded: clamp at zero
	got, err = svc.inviteePaidTotal(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.IsZero() {
		t.Fatalf("over-refunded total = %s, want 0", got)
	}
}

// Regression: successive partial refunds must reconcile against the
// ORIGINAL rebate, not against an already-shrunk one. The old code rewrote
// rebate_amount on unpaid rows, so the second refund compounded and short-
// changed the inviter.
func TestInviteRefundPartialRefundsDoNotCompound(t *testing.T) {
	svc, settings, client := testInviteEnv(t)
	ctx := context.Background()
	p := rebateProgram()                                     // 10%
	p.ReviewAmountThreshold = decimal.RequireFromString("1") // force a hold
	setInviteProgram(t, settings, p)
	inviter, code := inviterWithCode(t, svc, client)
	invitee := newInviteTestUser(t, client)
	if err := svc.BindOnRegister(ctx, invitee.ID, code, "", ""); err != nil {
		t.Fatal(err)
	}
	orderID := uuid.New()
	if err := svc.ProcessTopupRebate(ctx, invitee.ID, orderID, decimal.RequireFromString("100")); err != nil {
		t.Fatal(err)
	}

	// Two partial refunds, cumulative 30 then 60. Entitled is 10% of what
	// survives, so 7 then 4 — never 10*70/100 then 7*40/100.
	svc.ReconcileRefund(ctx, invitee.ID, orderID, decimal.RequireFromString("30"))
	svc.ReconcileRefund(ctx, invitee.ID, orderID, decimal.RequireFromString("60"))

	row, err := client.InviteRebate.Query().Where(entrebate.SourceOrderIDEQ(orderID)).Only(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !row.RebateAmount.Equal(decimal.RequireFromString("10")) {
		t.Fatalf("rebate_amount must stay the payout snapshot, got %s", row.RebateAmount)
	}
	if !row.ClawedBackAmount.Equal(decimal.RequireFromString("6")) {
		t.Fatalf("clawed back = %s, want 6 (10 - 10%% of the surviving 40)", row.ClawedBackAmount)
	}
	// Paying a partially-refunded hold credits only what survives.
	if _, err := svc.PayRebate(ctx, row.ID, "manually reviewed"); err != nil {
		t.Fatal(err)
	}
	if got := balanceOf(t, client, inviter.ID); !got.Equal(decimal.RequireFromString("4")) {
		t.Fatalf("paid %s, want 4", got)
	}
}

// Regression: a rebate clamped by the per-invitee cap is not rate% of its
// source amount, so scaling it by the refunded fraction would claw back
// money the cap says the inviter still earned.
func TestInviteRefundOnCapClampedRebate(t *testing.T) {
	svc, settings, client := testInviteEnv(t)
	ctx := context.Background()
	p := rebateProgram() // 10%
	p.TopupRebateCapPerInvitee = decimal.RequireFromString("15")
	setInviteProgram(t, settings, p)
	inviter, code := inviterWithCode(t, svc, client)
	invitee := newInviteTestUser(t, client)
	if err := svc.BindOnRegister(ctx, invitee.ID, code, "", ""); err != nil {
		t.Fatal(err)
	}
	orderA, orderB := uuid.New(), uuid.New()
	if err := svc.ProcessTopupRebate(ctx, invitee.ID, orderA, decimal.RequireFromString("100")); err != nil {
		t.Fatal(err) // 10
	}
	if err := svc.ProcessTopupRebate(ctx, invitee.ID, orderB, decimal.RequireFromString("100")); err != nil {
		t.Fatal(err) // clamped to the remaining 5
	}
	if got := balanceOf(t, client, inviter.ID); !got.Equal(decimal.RequireFromString("15")) {
		t.Fatalf("setup: balance = %s, want the 15 cap", got)
	}

	// Refunding half of B still leaves 150 topped up — more than enough to
	// earn the whole 15 cap, so nothing comes back.
	svc.ReconcileRefund(ctx, invitee.ID, orderB, decimal.RequireFromString("50"))
	if got := balanceOf(t, client, inviter.ID); !got.Equal(decimal.RequireFromString("15")) {
		t.Fatalf("clamped rebate over-clawed: balance = %s, want 15", got)
	}
}

// Retired consume rows are historical: they must not consume a cap that
// was configured for the topup program.
func TestInviteTopupCapIgnoresRetiredConsumeRows(t *testing.T) {
	svc, settings, client := testInviteEnv(t)
	ctx := context.Background()
	p := rebateProgram()
	p.TopupRebateCapPerInvitee = decimal.RequireFromString("20")
	setInviteProgram(t, settings, p)
	inviter, code := inviterWithCode(t, svc, client)
	invitee := newInviteTestUser(t, client)
	if err := svc.BindOnRegister(ctx, invitee.ID, code, "", ""); err != nil {
		t.Fatal(err)
	}
	// A legacy consume rebate worth more than the whole new cap.
	if err := client.InviteRebate.Create().
		SetInviterUserID(inviter.ID).
		SetInviteeUserID(invitee.ID).
		SetSourceKind(entrebate.SourceKindConsume).
		SetSourceUsageLogID(uuid.New()).
		SetSourceAmount(decimal.RequireFromString("300")).
		SetRatePctSnapshot(decimal.RequireFromString("10")).
		SetRebateAmount(decimal.RequireFromString("30")).
		SetCurrency(CurrencyCNY).
		SetStatus(entrebate.StatusPaid).
		Exec(ctx); err != nil {
		t.Fatal(err)
	}

	if err := svc.ProcessTopupRebate(ctx, invitee.ID, uuid.New(), decimal.RequireFromString("1000")); err != nil {
		t.Fatal(err)
	}
	if got := balanceOf(t, client, inviter.ID); !got.Equal(decimal.RequireFromString("20")) {
		t.Fatalf("legacy consume rows ate the topup cap: balance = %s, want 20", got)
	}
}

// A reward that is qualified/held when the refund lands was never credited,
// so there is nothing to debit — but it must stop being payable, because
// PayReward does not re-check the condition.
func TestInviteRefundResetsHeldRewardToPending(t *testing.T) {
	svc, settings, client := testInviteEnv(t)
	ctx := context.Background()
	p := permissiveProgram()
	p.SignupCondition = InviteConditionMinTopup
	p.SignupMinTopupAmount = decimal.RequireFromString("50")
	p.RequireInviterEmailVerified = true // test inviters are unverified
	p.AutoHoldOnRisk = true
	setInviteProgram(t, settings, p)
	inviter, code := inviterWithCode(t, svc, client)
	invitee := newInviteTestUser(t, client)
	if err := svc.BindOnRegister(ctx, invitee.ID, code, "", ""); err != nil {
		t.Fatal(err)
	}
	addTopup(t, client, invitee.ID, "60")
	if err := svc.EvaluateSignup(ctx, invitee.ID); err != nil {
		t.Fatal(err)
	}
	if r := rewardOf(t, client, invitee.ID); r.Status != entreward.StatusHeld {
		t.Fatalf("setup: want held, got %s", r.Status)
	}

	addRefundTx(t, client, invitee.ID, "60")
	svc.ReconcileRefund(ctx, invitee.ID, uuid.New(), decimal.RequireFromString("60"))
	r := rewardOf(t, client, invitee.ID)
	if r.Status != entreward.StatusPending {
		t.Fatalf("held reward after refund = %s, want pending", r.Status)
	}
	if got := balanceOf(t, client, inviter.ID); !got.IsZero() {
		t.Fatalf("nothing was credited, so nothing may be debited: %s", got)
	}
	// And it must not be payable behind the operator's back.
	if _, err := svc.PayReward(ctx, r.ID, "manual"); err != nil {
		t.Fatalf("pending reward stays manually payable by design: %v", err)
	}
}

// --- admin actions ---

func TestInviteAdminRejectAndVoid(t *testing.T) {
	svc, settings, client := testInviteEnv(t)
	ctx := context.Background()
	p := permissiveProgram()
	p.RequireInviterEmailVerified = true // forces held so there's something to moderate
	p.AutoHoldOnRisk = true
	p.TopupRebateEnabled = true
	p.TopupRebateRatePct = decimal.RequireFromString("10")
	setInviteProgram(t, settings, p)
	_, code := inviterWithCode(t, svc, client)

	// Reject a held reward; a second reject is an invalid state.
	rejected := newInviteTestUser(t, client)
	if err := svc.BindOnRegister(ctx, rejected.ID, code, "", ""); err != nil {
		t.Fatal(err)
	}
	r := rewardOf(t, client, rejected.ID)
	if r.Status != entreward.StatusHeld {
		t.Fatalf("setup: want held, got %s", r.Status)
	}
	if _, err := svc.RejectReward(ctx, r.ID, "farm suspicion"); err != nil {
		t.Fatal(err)
	}
	if r = rewardOf(t, client, rejected.ID); r.Status != entreward.StatusRejected || r.Note != "farm suspicion" {
		t.Fatalf("want rejected + note, got %s %q", r.Status, r.Note)
	}
	// Re-rejecting is idempotent (mirrors PayReward on paid); paying a
	// rejected reward is the invalid transition.
	if _, err := svc.RejectReward(ctx, r.ID, ""); err != nil {
		t.Fatalf("re-reject should be idempotent: %v", err)
	}
	if _, err := svc.PayReward(ctx, r.ID, ""); err == nil {
		t.Fatal("paying a rejected reward must fail")
	}

	// Voiding a binding voids its unpaid reward and held rebates, and
	// blocks future rebates. Paying the voided reward must fail.
	voided := newInviteTestUser(t, client)
	if err := svc.BindOnRegister(ctx, voided.ID, code, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := svc.ProcessTopupRebate(ctx, voided.ID, uuid.New(), decimal.RequireFromString("100")); err != nil {
		t.Fatal(err)
	}
	b, _ := client.InviteBinding.Query().Where(entbinding.InviteeUserIDEQ(voided.ID)).Only(ctx)
	if _, err := svc.VoidBinding(ctx, b.ID, "void note"); err != nil {
		t.Fatal(err)
	}
	if r := rewardOf(t, client, voided.ID); r.Status != entreward.StatusVoided {
		t.Fatalf("reward after void: want voided, got %s", r.Status)
	}
	rebates, _ := client.InviteRebate.Query().Where(entrebate.InviteeUserIDEQ(voided.ID)).All(ctx)
	if len(rebates) != 1 || rebates[0].Status != entrebate.StatusVoided {
		t.Fatalf("held rebate must void with the binding: %+v", rebates)
	}
	if _, err := svc.PayReward(ctx, rewardOf(t, client, voided.ID).ID, ""); err == nil {
		t.Fatal("paying a voided reward must fail")
	}
	if err := svc.ProcessTopupRebate(ctx, voided.ID, uuid.New(), decimal.RequireFromString("100")); err != nil {
		t.Fatal(err)
	}
	if n, _ := client.InviteRebate.Query().Where(entrebate.InviteeUserIDEQ(voided.ID)).Count(ctx); n != 1 {
		t.Fatal("voided binding must not accrue new rebates")
	}
}

func TestInviteAdminPayHeldReward(t *testing.T) {
	svc, settings, client := testInviteEnv(t)
	ctx := context.Background()
	p := permissiveProgram()
	p.RequireInviterEmailVerified = true
	p.AutoHoldOnRisk = true
	setInviteProgram(t, settings, p)
	inviter, code := inviterWithCode(t, svc, client)

	invitee := newInviteTestUser(t, client)
	if err := svc.BindOnRegister(ctx, invitee.ID, code, "", ""); err != nil {
		t.Fatal(err)
	}
	r := rewardOf(t, client, invitee.ID)
	if r.Status != entreward.StatusHeld {
		t.Fatalf("setup: want held, got %s", r.Status)
	}
	paid, err := svc.PayReward(ctx, r.ID, "manually reviewed")
	if err != nil {
		t.Fatal(err)
	}
	if paid.Status != entreward.StatusPaid || paid.PaidBalanceTxID == nil {
		t.Fatalf("want paid with tx id, got %+v", paid)
	}
	if got := balanceOf(t, client, inviter.ID); !got.Equal(decimal.RequireFromString("5")) {
		t.Fatalf("inviter balance = %s, want 5", got)
	}
}

// --- settings ---

func TestNormalizeInviteProgramValidation(t *testing.T) {
	ok := permissiveProgram()
	if _, err := NormalizeInviteProgram(ok); err != nil {
		t.Fatalf("valid program rejected: %v", err)
	}
	bad := []func(p *InviteProgramSettings){
		func(p *InviteProgramSettings) { p.SignupCondition = "bogus" },
		func(p *InviteProgramSettings) { p.SignupRewardAmount = decimal.NewFromInt(-1) },
		func(p *InviteProgramSettings) { p.SignupMinTopupAmount = decimal.NewFromInt(-1) },
		func(p *InviteProgramSettings) { p.TopupRebateRatePct = decimal.NewFromInt(-1) },
		func(p *InviteProgramSettings) { p.TopupRebateRatePct = decimal.NewFromInt(101) },
		func(p *InviteProgramSettings) { p.TopupRebateWindowDays = -1 },
		func(p *InviteProgramSettings) { p.TopupRebateCapPerInvitee = decimal.NewFromInt(-1) },
		func(p *InviteProgramSettings) { p.SignupDailyCountCapPerInviter = -1 },
		func(p *InviteProgramSettings) { p.SignupDailyAmountCapPerInviter = decimal.NewFromInt(-1) },
		func(p *InviteProgramSettings) { p.InviterMinAgeHours = -1 },
		func(p *InviteProgramSettings) { p.SameIPDailyCap = -1 },
		func(p *InviteProgramSettings) { p.ReviewAmountThreshold = decimal.NewFromInt(-1) },
	}
	for i, mutate := range bad {
		p := permissiveProgram()
		mutate(&p)
		if _, err := NormalizeInviteProgram(p); err == nil {
			t.Errorf("case %d: invalid program accepted", i)
		}
	}
	// Empty condition defaults rather than erroring.
	p := permissiveProgram()
	p.SignupCondition = ""
	out, err := NormalizeInviteProgram(p)
	if err != nil || out.SignupCondition != InviteConditionEmailVerified {
		t.Fatalf("empty condition should default: %v %q", err, out.SignupCondition)
	}
}

func TestInviteProgramPartialJSONKeepsRiskDefaults(t *testing.T) {
	got := parseInviteProgram(`{"enabled":true}`)
	want := DefaultInviteProgram()
	if !got.Enabled {
		t.Fatal("explicit enabled value was not applied")
	}
	if got.SignupDailyCountCapPerInviter != want.SignupDailyCountCapPerInviter ||
		got.InviterMinAgeHours != want.InviterMinAgeHours ||
		got.SameIPDailyCap != want.SameIPDailyCap ||
		got.AutoHoldOnRisk != want.AutoHoldOnRisk ||
		got.RequireInviterEmailVerified != want.RequireInviterEmailVerified {
		t.Fatalf("partial JSON dropped risk defaults: got %+v want defaults %+v", got, want)
	}
}

// The public projection is what /v1/public/site-info and /v1/me/invite
// expose; it must never grow a risk/cap field by accident.
func TestInviteProgramPublicRedaction(t *testing.T) {
	raw, err := json.Marshal(DefaultInviteProgram().Public())
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"enabled", "signup_reward_enabled", "signup_reward_amount",
		"signup_condition", "signup_min_topup_amount",
		"topup_rebate_enabled", "topup_rebate_rate_pct",
		"topup_rebate_window_days", "risk_review", "email_verification_hold",
	}
	if len(m) != len(want) {
		t.Fatalf("public payload has %d keys, want %d: %v", len(m), len(want), m)
	}
	for _, k := range want {
		if _, ok := m[k]; !ok {
			t.Errorf("missing public key %q", k)
		}
	}
}

func TestReleaseEmailHolds(t *testing.T) {
	svc, settings, client := testInviteEnv(t)
	ctx := context.Background()
	p := permissiveProgram()
	p.RequireInviterEmailVerified = true
	p.AutoHoldOnRisk = true
	p.TopupRebateEnabled = true
	p.TopupRebateRatePct = decimal.NewFromInt(10)
	setInviteProgram(t, settings, p)

	held := func(inviterCode string) *ent.User {
		t.Helper()
		invitee := newInviteTestUser(t, client)
		if err := svc.BindOnRegister(ctx, invitee.ID, inviterCode, "", ""); err != nil {
			t.Fatal(err)
		}
		if err := svc.EvaluateSignup(ctx, invitee.ID); err != nil {
			t.Fatal(err)
		}
		if err := svc.ProcessTopupRebate(ctx, invitee.ID, uuid.New(), decimal.NewFromInt(20)); err != nil {
			t.Fatal(err)
		}
		return invitee
	}
	verify := func(id uuid.UUID) {
		t.Helper()
		if _, err := client.User.UpdateOneID(id).SetEmailVerified(true).Save(ctx); err != nil {
			t.Fatal(err)
		}
	}
	rebateOf := func(inviteeID uuid.UUID) *ent.InviteRebate {
		t.Helper()
		r, err := client.InviteRebate.Query().Where(entrebate.InviteeUserIDEQ(inviteeID)).Only(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}

	inviter, code := inviterWithCode(t, svc, client)
	invitee := held(code)
	if r := rewardOf(t, client, invitee.ID); r.Status != entreward.StatusHeld || !EmailHoldOnly(r.RiskFlags) {
		t.Fatalf("reward = %s %v, want email hold", r.Status, r.RiskFlags)
	}
	if r := rebateOf(invitee.ID); r.Status != entrebate.StatusHeld || !EmailHoldOnly(r.RiskFlags) {
		t.Fatalf("rebate = %s %v, want email hold", r.Status, r.RiskFlags)
	}
	// Still unverified: nothing moves.
	if err := svc.ReleaseEmailHolds(ctx, inviter.ID); err != nil {
		t.Fatal(err)
	}
	if r := rebateOf(invitee.ID); r.Status != entrebate.StatusHeld {
		t.Fatalf("released before verification: %s", r.Status)
	}
	verify(inviter.ID)
	if err := svc.ReleaseEmailHolds(ctx, inviter.ID); err != nil {
		t.Fatal(err)
	}
	if r := rewardOf(t, client, invitee.ID); r.Status != entreward.StatusPaid {
		t.Fatalf("reward = %s, want paid", r.Status)
	}
	if r := rebateOf(invitee.ID); r.Status != entrebate.StatusPaid {
		t.Fatalf("rebate = %s, want paid", r.Status)
	}
	if got := balanceOf(t, client, inviter.ID); !got.Equal(decimal.NewFromInt(7)) {
		t.Fatalf("inviter balance = %s, want 7", got)
	}

	// A hold that still applies after verification stays, with its flags.
	frozen, code := inviterWithCode(t, svc, client)
	other := held(code)
	if _, err := client.User.UpdateOneID(frozen.ID).SetEmailVerified(true).
		SetFrozenUntil(time.Now().Add(time.Hour)).Save(ctx); err != nil {
		t.Fatal(err)
	}
	if err := svc.ReleaseEmailHolds(ctx, frozen.ID); err != nil {
		t.Fatal(err)
	}
	if r := rebateOf(other.ID); r.Status != entrebate.StatusHeld || !slices.Equal(r.RiskFlags, []string{riskInviterFrozen}) {
		t.Fatalf("frozen inviter rebate = %s %v", r.Status, r.RiskFlags)
	}
}

func TestHeldReasons(t *testing.T) {
	got := heldReasons(nil, []string{riskIPMatchesInviter, riskPayerMatchesInviter, riskInviterFrozen, "unknown"})
	got = heldReasons(got, []string{riskInviterNotActive, riskAmountOverThreshold})
	want := []string{"self_referral_suspected", "inviter_account_restricted", "amount_review"}
	if !slices.Equal(got, want) {
		t.Fatalf("heldReasons = %v, want %v", got, want)
	}
}

func TestInviteCodeNormalization(t *testing.T) {
	if got := NormalizeInviteCode("  ab3k7q2m "); got != "AB3K7Q2M" {
		t.Fatalf("normalize = %q", got)
	}
	for _, bad := range []string{"", "SHORT", "ABCDEFGH1", "AB3K7Q0M", "AB3K7QIM", "AB3K7QOM"} {
		if got := NormalizeInviteCode(bad); got != "" {
			t.Errorf("code %q should normalize to empty, got %q", bad, got)
		}
	}
}

func containsFlag(flags []string, want string) bool {
	for _, f := range flags {
		if f == want {
			return true
		}
	}
	return false
}

// --- self-referral evidence ---

func paidOrderWithPayer(t *testing.T, client *ent.Client, userID uuid.UUID, payer string) *ent.SiteOrder {
	t.Helper()
	o, err := client.SiteOrder.Create().
		SetOrderNo("T" + uuid.NewString()).
		SetUserID(userID).
		SetKind(entsiteorder.KindBalance).
		SetAmount(decimal.RequireFromString("100")).
		SetCurrency(CurrencyCNY).
		SetStatus(entsiteorder.StatusPaid).
		SetPaymentChannel("alipay").
		SetPaymentPayerOpenid(payer).
		Save(context.Background())
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	return o
}

// An organic inviter (never invited themselves) used to have no IP on
// record, so an invitee signing up from the inviter's own network passed.
func TestInviteSelfReferralInviterLoginIP(t *testing.T) {
	svc, settings, client := testInviteEnv(t)
	ctx := context.Background()
	p := permissiveProgram()
	p.AutoHoldOnRisk = true
	setInviteProgram(t, settings, p)
	inviter, code := inviterWithCode(t, svc, client)
	if err := client.User.UpdateOneID(inviter.ID).SetLastLoginIP("198.51.100.20").Exec(ctx); err != nil {
		t.Fatal(err)
	}

	invitee := newInviteTestUser(t, client)
	if err := svc.BindOnRegister(ctx, invitee.ID, code, "198.51.100.20", ""); err != nil {
		t.Fatal(err)
	}
	r := rewardOf(t, client, invitee.ID)
	if r.Status != entreward.StatusHeld || !containsFlag(r.RiskFlags, riskIPMatchesInviter) {
		t.Fatalf("want held with %s, got %s %v", riskIPMatchesInviter, r.Status, r.RiskFlags)
	}

	// The inviter moving networks must not clear the evidence: re-evaluation
	// (fired by any later top-up) keeps the reward held.
	if err := client.User.UpdateOneID(inviter.ID).SetLastLoginIP("192.0.2.1").Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if err := svc.EvaluateSignup(ctx, invitee.ID); err != nil {
		t.Fatal(err)
	}
	if r := rewardOf(t, client, invitee.ID); r.Status != entreward.StatusHeld || !containsFlag(r.RiskFlags, riskIPMatchesInviter) {
		t.Fatalf("re-evaluation released the hold: %s %v", r.Status, r.RiskFlags)
	}

	other := newInviteTestUser(t, client)
	if err := svc.BindOnRegister(ctx, other.ID, code, "203.0.113.9", ""); err != nil {
		t.Fatal(err)
	}
	if r := rewardOf(t, client, other.ID); r.Status != entreward.StatusPaid {
		t.Fatalf("different IP should pay, got %s %v", r.Status, r.RiskFlags)
	}
}

func TestInviteRebateSamePayerHeld(t *testing.T) {
	svc, settings, client := testInviteEnv(t)
	ctx := context.Background()
	p := rebateProgram()
	p.AutoHoldOnRisk = true
	setInviteProgram(t, settings, p)
	inviter, code := inviterWithCode(t, svc, client)
	paidOrderWithPayer(t, client, inviter.ID, "2088000000000001")
	paidOrderWithPayer(t, client, inviter.ID, "159****5620")

	same := newInviteTestUser(t, client)
	if err := svc.BindOnRegister(ctx, same.ID, code, "", ""); err != nil {
		t.Fatal(err)
	}
	o := paidOrderWithPayer(t, client, same.ID, "2088000000000001")
	if err := svc.ProcessTopupRebate(ctx, same.ID, o.ID, o.Amount); err != nil {
		t.Fatal(err)
	}
	r, err := client.InviteRebate.Query().Where(entrebate.SourceOrderIDEQ(o.ID)).Only(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != entrebate.StatusHeld || !containsFlag(r.RiskFlags, riskPayerMatchesInviter) {
		t.Fatalf("want held with %s, got %s %v", riskPayerMatchesInviter, r.Status, r.RiskFlags)
	}

	// A masked Alipay login name is shared by many people: not evidence.
	masked := newInviteTestUser(t, client)
	if err := svc.BindOnRegister(ctx, masked.ID, code, "", ""); err != nil {
		t.Fatal(err)
	}
	o = paidOrderWithPayer(t, client, masked.ID, "159****5620")
	if err := svc.ProcessTopupRebate(ctx, masked.ID, o.ID, o.Amount); err != nil {
		t.Fatal(err)
	}
	r, err = client.InviteRebate.Query().Where(entrebate.SourceOrderIDEQ(o.ID)).Only(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != entrebate.StatusPaid {
		t.Fatalf("masked payer should pay, got %s %v", r.Status, r.RiskFlags)
	}
}

func TestInviteCouponConflict(t *testing.T) {
	svc, settings, client := testInviteEnv(t)
	ctx := context.Background()
	p := rebateProgram()
	p.SignupRewardEnabled = true
	p.SignupCondition = InviteConditionMinTopup
	p.SignupMinTopupAmount = decimal.RequireFromString("50")
	p.TopupRebateWindowDays = 7
	setInviteProgram(t, settings, p)
	_, code := inviterWithCode(t, svc, client)

	unbound := newInviteTestUser(t, client)
	if got, err := svc.CouponConflict(ctx, unbound.ID); err != nil || got != (InviteCouponConflict{}) {
		t.Fatalf("unbound: %+v %v", got, err)
	}

	invitee := newInviteTestUser(t, client)
	if err := svc.BindOnRegister(ctx, invitee.ID, code, "", ""); err != nil {
		t.Fatal(err)
	}
	got, err := svc.CouponConflict(ctx, invitee.ID)
	if err != nil || !got.TopupRebate || !got.SignupReward {
		t.Fatalf("fresh invitee: %+v %v", got, err)
	}

	// Past the rebate window only the pending signup reward is at stake.
	if _, err := svc.d.DB().ExecContext(ctx,
		`UPDATE invite_bindings SET created_at = $1 WHERE invitee_user_id = $2`,
		time.Now().AddDate(0, 0, -8), invitee.ID); err != nil {
		t.Fatal(err)
	}
	got, err = svc.CouponConflict(ctx, invitee.ID)
	if err != nil || got.TopupRebate || !got.SignupReward {
		t.Fatalf("window expired: %+v %v", got, err)
	}

	attributeInviteTestUserToAgent(t, settings, client, invitee.ID)
	if got, err := svc.CouponConflict(ctx, invitee.ID); err != nil || got != (InviteCouponConflict{}) {
		t.Fatalf("agent-attributed: %+v %v", got, err)
	}
}
