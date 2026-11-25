package auth

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/redis/go-redis/v9"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/data"
	"github.com/jucode/jucode/internal/data/ent"
	entevt "github.com/jucode/jucode/internal/data/ent/emailverificationtoken"
	entuser "github.com/jucode/jucode/internal/data/ent/user"
)

// Postgres-backed like the invite engine's own tests — the code lookup rides
// the unique index on users.invite_code, and these cases drive the real gin
// routes rather than the handler functions, so the status codes and the
// machine-readable `code` field the register form keys off are covered too.
// Same opt-in as the rest of the Postgres-backed suite:
//
//	JUCODE_TEST_PG_DSN='postgres://postgres:test@127.0.0.1:5499/jucode_test?sslmode=disable' \
//	  go test ./internal/api/auth
func testAuthEnv(t *testing.T) (*ent.Client, *biz.InviteSvc, *gin.Engine) {
	t.Helper()
	dsn := os.Getenv("JUCODE_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("JUCODE_TEST_PG_DSN not set; skipping Postgres-backed auth tests")
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
	settings := biz.NewSettingsSvc(d)
	invites := biz.NewInviteSvc(d, settings)
	acc := biz.NewAccount(d, nil, settings)
	acc.SetInvites(invites)

	h := New(acc)
	h.SetInvites(invites)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	h.Register(r.Group("/v1/auth"))
	return client, invites, r
}

func inviterWithCode(t *testing.T, client *ent.Client, invites *biz.InviteSvc) string {
	t.Helper()
	u, err := client.User.Create().
		SetEmail(uuid.NewString() + "@test.invalid").
		SetPasswordHash("x").
		SetStatus(entuser.StatusActive).
		Save(context.Background())
	if err != nil {
		t.Fatalf("create inviter: %v", err)
	}
	code, err := invites.EnsureInviteCode(context.Background(), u.ID)
	if err != nil {
		t.Fatalf("ensure code: %v", err)
	}
	return code
}

type checkResp struct {
	Valid  bool   `json:"valid"`
	Reason string `json:"reason"`
}

func TestCheckInviteCodeRoute(t *testing.T) {
	client, invites, r := testAuthEnv(t)
	code := inviterWithCode(t, client, invites)

	cases := []struct {
		name       string
		code       string
		wantValid  bool
		wantReason string
	}{
		{name: "live code", code: code, wantValid: true},
		{name: "wrong length", code: "WB82WQ", wantReason: "malformed"},
		{name: "confusable characters", code: "WB82WQ0I", wantReason: "malformed"},
		{name: "well formed but unowned", code: "ZZZZZZZZ", wantReason: "unknown"},
		{name: "empty", code: "", wantReason: "malformed"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/auth/invite-code/check?code="+tc.code, nil))

			// Always 200: an unusable code is an answer, not a failed request,
			// and the register form calls this on every blur.
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
			}
			var got checkResp
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode: %v (%s)", err, w.Body.String())
			}
			if got.Valid != tc.wantValid || got.Reason != tc.wantReason {
				t.Fatalf("got %+v, want valid=%v reason=%q", got, tc.wantValid, tc.wantReason)
			}
		})
	}
}

func TestRegisterRejectsUnusableInviteCode(t *testing.T) {
	client, invites, r := testAuthEnv(t)
	inviterWithCode(t, client, invites)

	cases := []struct {
		name     string
		code     string
		wantCode string
	}{
		{name: "malformed", code: "short", wantCode: "invite_code_malformed"},
		{name: "unknown", code: "ZZZZZZZZ", wantCode: "invite_code_unknown"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			email := uuid.NewString() + "@test.invalid"
			body, _ := json.Marshal(map[string]string{
				"email": email, "password": "password123", "invite_code": tc.code,
			})
			req := httptest.NewRequest(http.MethodPost, "/v1/auth/register", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
			}
			var got struct {
				Error string `json:"error"`
				Code  string `json:"code"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode: %v (%s)", err, w.Body.String())
			}
			if got.Code != tc.wantCode {
				t.Fatalf("code = %q, want %q (body %s)", got.Code, tc.wantCode, w.Body.String())
			}

			// The whole point of refusing: no account, so no stranded binding
			// that support would have to unwind later.
			n, err := client.User.Query().Where(entuser.EmailEQ(email)).Count(context.Background())
			if err != nil {
				t.Fatalf("count users: %v", err)
			}
			if n != 0 {
				t.Fatal("refused registration must not create the account")
			}
		})
	}
}

func TestVerifyEmailUsesStoredKind(t *testing.T) {
	client, _, _ := testAuthEnv(t)
	ctx := context.Background()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	d := data.NewWith(client, rdb, nil)
	settings := biz.NewSettingsSvc(d)
	account := biz.NewAccount(d, nil, settings)
	account.SetVerification(biz.NewVerificationSvc(d, settings, biz.NewEmailSvc(settings, nil)))
	gin.SetMode(gin.TestMode)
	r := gin.New()
	New(account).Register(r.Group("/v1/auth"))
	for _, tc := range []struct {
		name, tokenKind, bodyKind string
		wantStatus                int
	}{
		{"old frontend guesses register", "change_email", "register", http.StatusOK},
		{"old mail omits kind", "change_email", "", http.StatusOK},
		{"current email verification", "register", "", http.StatusOK},
		{"reset token is rejected", "reset_password", "register", http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := client.User.Create().SetEmail(uuid.NewString() + "@test.invalid").SetPasswordHash("x").SaveX(ctx)
			newEmail := uuid.NewString() + "@test.invalid"
			pendingEmail := newEmail
			if tc.tokenKind == "register" {
				pendingEmail = u.Email
			}
			token := client.EmailVerificationToken.Create().SetUserID(u.ID).SetKind(entevt.Kind(tc.tokenKind)).
				SetCode(uuid.NewString()).SetPendingEmail(pendingEmail).SetExpiresAt(time.Now().Add(time.Hour)).SaveX(ctx)
			body := map[string]string{"code": token.Code}
			if tc.bodyKind != "" {
				body["kind"] = tc.bodyKind
			}
			encoded, _ := json.Marshal(body)
			request := func() *httptest.ResponseRecorder {
				req := httptest.NewRequest(http.MethodPost, "/v1/auth/verify-email", bytes.NewReader(encoded))
				req.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()
				r.ServeHTTP(w, req)
				return w
			}
			w := request()
			if w.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d: %s", w.Code, tc.wantStatus, w.Body)
			}
			updated := client.User.GetX(ctx, u.ID)
			if tc.wantStatus != http.StatusOK {
				if updated.EmailVerified || client.EmailVerificationToken.GetX(ctx, token.ID).UsedAt != nil {
					t.Fatal("reset token verified the email or was consumed")
				}
				return
			}
			if !strings.Contains(w.Body.String(), `"kind":"`+tc.tokenKind+`"`) || !updated.EmailVerified {
				t.Fatalf("wrong verification result: %s", w.Body)
			}
			wantEmail := u.Email
			if tc.tokenKind == "change_email" {
				wantEmail = newEmail
			}
			if updated.Email != wantEmail {
				t.Fatalf("email = %s, want %s", updated.Email, wantEmail)
			}
			if replay := request(); replay.Code != http.StatusBadRequest || !strings.Contains(replay.Body.String(), "already used") {
				t.Fatalf("replayed token was not refused: %d %s", replay.Code, replay.Body)
			}
		})
	}
}
