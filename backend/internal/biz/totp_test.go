package biz

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/redis/go-redis/v9"

	"github.com/jucode/jucode/internal/data"
	"github.com/jucode/jucode/internal/data/ent"
	entuserrole "github.com/jucode/jucode/internal/data/ent/userrole"
	"github.com/jucode/jucode/internal/pkg/jwt"
	"github.com/jucode/jucode/internal/pkg/pwd"
	"github.com/jucode/jucode/internal/pkg/totp"
)

// Postgres + Redis backed, same opt-in pattern as the invite tests:
//
//	JUCODE_TEST_PG_DSN='postgres://postgres:test@127.0.0.1:5432/jucode_test?sslmode=disable' \
//	JUCODE_TEST_REDIS_ADDR='127.0.0.1:6379' \
//	  go test ./internal/biz -run TOTP
func testTOTPEnv(t *testing.T) (*Account, *ent.Client, *redis.Client) {
	t.Helper()
	dsn := os.Getenv("JUCODE_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("JUCODE_TEST_PG_DSN not set; skipping Postgres-backed TOTP tests")
	}
	addr := os.Getenv("JUCODE_TEST_REDIS_ADDR")
	if addr == "" {
		addr = "127.0.0.1:6379"
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
	rdb := redis.NewClient(&redis.Options{Addr: addr, DB: 9})
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		t.Skipf("redis not reachable at %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = rdb.Close() })
	d := data.NewWith(client, rdb, db)
	settings := NewSettingsSvc(d)
	signer := jwt.NewSigner("test-secret-test-secret-test-secret!", time.Hour, 24*time.Hour)
	return NewAccount(d, signer, settings), client, rdb
}

func createTOTPUser(t *testing.T, client *ent.Client, role string) *ent.User {
	t.Helper()
	hash, err := pwd.Hash("password123")
	if err != nil {
		t.Fatal(err)
	}
	email := fmt.Sprintf("totp-%s@test.local", uuid.NewString()[:13])
	q := client.User.Create().SetEmail(email).SetPasswordHash(hash)
	if role == "admin" {
		q = q.SetRole("admin")
	}
	u, err := q.Save(context.Background())
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	return u
}

// codeFor returns a valid code for a NOT-yet-used time step so tests
// don't collide with the replay guard from an earlier verification.
func codeFor(t *testing.T, secret string, offsetSteps int) string {
	t.Helper()
	c, err := totp.Code(secret, time.Now().Add(time.Duration(offsetSteps)*totp.Period*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestTOTPSetupConfirmLogin(t *testing.T) {
	acc, client, _ := testTOTPEnv(t)
	ctx := context.Background()
	u := createTOTPUser(t, client, "admin")

	// Wrong confirm code: enable must not flip.
	secret, uri, err := acc.BeginTOTPSetup(ctx, u.ID)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	if secret == "" || uri == "" {
		t.Fatal("empty secret or uri")
	}
	if _, err := acc.ConfirmTOTPSetup(ctx, u.ID, "000000"); !errors.Is(err, ErrTOTPInvalidCode) {
		t.Fatalf("confirm with wrong code: got %v, want ErrTOTPInvalidCode", err)
	}
	if got, _ := acc.IsTOTPEnabled(ctx, u.ID); got {
		t.Fatal("totp enabled after failed confirm")
	}

	// Correct confirm: recovery codes returned exactly once.
	codes, err := acc.ConfirmTOTPSetup(ctx, u.ID, codeFor(t, secret, 0))
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if len(codes) != totp.RecoveryCodeCount {
		t.Fatalf("got %d recovery codes, want %d", len(codes), totp.RecoveryCodeCount)
	}
	if _, _, err := acc.BeginTOTPSetup(ctx, u.ID); !errors.Is(err, ErrTOTPAlreadyEnabled) {
		t.Fatalf("re-setup after enable: got %v, want ErrTOTPAlreadyEnabled", err)
	}

	// Login now returns an MFA ticket, no tokens.
	lr, err := acc.Login(ctx, u.Email, "password123")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if lr.Tokens != nil || lr.MFAToken == "" {
		t.Fatalf("expected MFA ticket without tokens, got tokens=%v ticket=%q", lr.Tokens != nil, lr.MFAToken)
	}

	// Wrong code burns an attempt but keeps the ticket alive.
	if _, _, err := acc.CompleteTOTPLogin(ctx, lr.MFAToken, "000001", ""); !errors.Is(err, ErrTOTPInvalidCode) {
		t.Fatalf("wrong code: got %v, want ErrTOTPInvalidCode", err)
	}
	// Next time step avoids the replay guard from the confirm above.
	_, pair, err := acc.CompleteTOTPLogin(ctx, lr.MFAToken, codeFor(t, secret, 1), "")
	if err != nil {
		t.Fatalf("complete login: %v", err)
	}
	if pair == nil || pair.Access == "" {
		t.Fatal("no tokens after successful second step")
	}
	// Ticket is single-use.
	if _, _, err := acc.CompleteTOTPLogin(ctx, lr.MFAToken, codeFor(t, secret, -1), ""); !errors.Is(err, ErrMFATicketInvalid) {
		t.Fatalf("ticket reuse: got %v, want ErrMFATicketInvalid", err)
	}
}

func TestTOTPReplayGuard(t *testing.T) {
	acc, client, _ := testTOTPEnv(t)
	ctx := context.Background()
	u := createTOTPUser(t, client, "user")
	secret, _, err := acc.BeginTOTPSetup(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acc.ConfirmTOTPSetup(ctx, u.ID, codeFor(t, secret, 0)); err != nil {
		t.Fatal(err)
	}
	fresh, err := client.User.Get(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	code := codeFor(t, secret, 1)
	if err := acc.VerifyTOTP(ctx, fresh, code); err != nil {
		t.Fatalf("first use: %v", err)
	}
	if err := acc.VerifyTOTP(ctx, fresh, code); !errors.Is(err, ErrTOTPInvalidCode) {
		t.Fatalf("replay: got %v, want ErrTOTPInvalidCode", err)
	}
}

func TestRecoveryCodeOneTime(t *testing.T) {
	acc, client, _ := testTOTPEnv(t)
	ctx := context.Background()
	u := createTOTPUser(t, client, "user")
	secret, _, err := acc.BeginTOTPSetup(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	codes, err := acc.ConfirmTOTPSetup(ctx, u.ID, codeFor(t, secret, 0))
	if err != nil {
		t.Fatal(err)
	}

	// First login via recovery code succeeds…
	lr, err := acc.Login(ctx, u.Email, "password123")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := acc.CompleteTOTPLogin(ctx, lr.MFAToken, "", codes[0]); err != nil {
		t.Fatalf("recovery login: %v", err)
	}
	fresh, _ := client.User.Get(ctx, u.ID)
	if len(fresh.TotpRecoveryCodes) != totp.RecoveryCodeCount-1 {
		t.Fatalf("recovery codes left = %d, want %d", len(fresh.TotpRecoveryCodes), totp.RecoveryCodeCount-1)
	}

	// …the same code a second time does not.
	lr2, err := acc.Login(ctx, u.Email, "password123")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := acc.CompleteTOTPLogin(ctx, lr2.MFAToken, "", codes[0]); !errors.Is(err, ErrTOTPInvalidCode) {
		t.Fatalf("reused recovery code: got %v, want ErrTOTPInvalidCode", err)
	}
	// A different unused code still works.
	if _, _, err := acc.CompleteTOTPLogin(ctx, lr2.MFAToken, "", codes[1]); err != nil {
		t.Fatalf("second recovery code: %v", err)
	}
}

func TestTOTPSetupRequiredNudge(t *testing.T) {
	acc, client, _ := testTOTPEnv(t)
	ctx := context.Background()

	// Admin without TOTP: login succeeds (migration grace) but carries
	// the setup-required nudge.
	admin := createTOTPUser(t, client, "admin")
	lr, err := acc.Login(ctx, admin.Email, "password123")
	if err != nil {
		t.Fatal(err)
	}
	if lr.Tokens == nil {
		t.Fatal("admin without totp must still get tokens (no lockout)")
	}
	if !lr.TOTPSetupRequired {
		t.Error("admin without totp should be nudged to bind")
	}

	// cs functional role gets the same nudge.
	csUser := createTOTPUser(t, client, "user")
	if _, err := client.UserRole.Create().SetUserID(csUser.ID).SetRole(entuserrole.RoleCs).Save(ctx); err != nil {
		t.Fatal(err)
	}
	lr2, err := acc.Login(ctx, csUser.Email, "password123")
	if err != nil {
		t.Fatal(err)
	}
	if !lr2.TOTPSetupRequired {
		t.Error("cs role without totp should be nudged to bind")
	}

	// Plain user: no nudge.
	plain := createTOTPUser(t, client, "user")
	lr3, err := acc.Login(ctx, plain.Email, "password123")
	if err != nil {
		t.Fatal(err)
	}
	if lr3.TOTPSetupRequired {
		t.Error("plain user should not be nudged")
	}
}

func TestDisableTOTP(t *testing.T) {
	acc, client, _ := testTOTPEnv(t)
	ctx := context.Background()
	u := createTOTPUser(t, client, "user")
	secret, _, err := acc.BeginTOTPSetup(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	codes, err := acc.ConfirmTOTPSetup(ctx, u.ID, codeFor(t, secret, 0))
	if err != nil {
		t.Fatal(err)
	}
	if err := acc.DisableTOTP(ctx, u.ID, "wrong-password", codeFor(t, secret, 1)); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("wrong password: got %v, want ErrInvalidCredential", err)
	}
	if err := acc.DisableTOTP(ctx, u.ID, "password123", "000000"); !errors.Is(err, ErrTOTPInvalidCode) {
		t.Fatalf("wrong code: got %v, want ErrTOTPInvalidCode", err)
	}
	// Recovery code works for disable (lost-phone path).
	if err := acc.DisableTOTP(ctx, u.ID, "password123", codes[0]); err != nil {
		t.Fatalf("disable: %v", err)
	}
	fresh, _ := client.User.Get(ctx, u.ID)
	if fresh.TotpEnabled || len(fresh.TotpRecoveryCodes) != 0 {
		t.Fatal("totp state not cleared after disable")
	}
	// Login is single-step again.
	lr, err := acc.Login(ctx, u.Email, "password123")
	if err != nil {
		t.Fatal(err)
	}
	if lr.Tokens == nil || lr.MFAToken != "" {
		t.Fatal("expected plain login after disable")
	}
}

func TestStepUpVerify(t *testing.T) {
	acc, client, _ := testTOTPEnv(t)
	ctx := context.Background()
	u := createTOTPUser(t, client, "admin")
	secret, _, err := acc.BeginTOTPSetup(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acc.ConfirmTOTPSetup(ctx, u.ID, codeFor(t, secret, 0)); err != nil {
		t.Fatal(err)
	}
	if acc.StepUpSatisfied(ctx, u.ID) {
		t.Fatal("step-up satisfied before any verification")
	}
	if err := acc.StepUpVerify(ctx, u.ID, "000000"); !errors.Is(err, ErrTOTPInvalidCode) {
		t.Fatalf("wrong step-up code: got %v, want ErrTOTPInvalidCode", err)
	}
	if err := acc.StepUpVerify(ctx, u.ID, codeFor(t, secret, 1)); err != nil {
		t.Fatalf("step-up verify: %v", err)
	}
	if !acc.StepUpSatisfied(ctx, u.ID) {
		t.Fatal("step-up window not opened after successful verify")
	}
}
