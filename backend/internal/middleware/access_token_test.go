package middleware

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/data"
	"github.com/jucode/jucode/internal/data/ent"
	entuser "github.com/jucode/jucode/internal/data/ent/user"
	jwtpkg "github.com/jucode/jucode/internal/pkg/jwt"
)

// stepUpAlwaysSatisfied models the most permissive step-up state: the owner
// just entered a code in the browser (or has no two-factor at all).
type stepUpAlwaysSatisfied struct{}

func (stepUpAlwaysSatisfied) IsTOTPEnabled(context.Context, uuid.UUID) (bool, error) {
	return true, nil
}
func (stepUpAlwaysSatisfied) StepUpSatisfied(context.Context, uuid.UUID) bool       { return true }
func (stepUpAlwaysSatisfied) StepUpVerify(context.Context, uuid.UUID, string) error { return nil }

// Needs Postgres:
//
//	JUCODE_TEST_PG_DSN='postgres://postgres:test@127.0.0.1:5499/jucode_test?sslmode=disable' \
//	  go test ./internal/middleware -run AccessToken
func TestAccessTokenAuth(t *testing.T) {
	dsn := os.Getenv("JUCODE_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("JUCODE_TEST_PG_DSN not set")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	client := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	ctx := context.Background()
	if err := client.Schema.Create(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	tokens := biz.NewAccessTokenSvc(data.NewWith(client, nil, db))

	admin, err := client.User.Create().SetEmail(uuid.NewString() + "@test.invalid").SetPasswordHash("x").
		SetStatus(entuser.StatusActive).SetRole(entuser.RoleAdmin).Save(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	_, token, err := tokens.Create(ctx, admin.ID, "agent", nil)
	if err != nil {
		t.Fatalf("create token: %v", err)
	}

	signer := jwtpkg.NewSigner("test-secret-that-is-long-enough-for-the-signer-32b", time.Hour, 24*time.Hour)
	session, _, _, err := signer.IssueAccess(admin.ID, "admin")
	if err != nil {
		t.Fatalf("issue session: %v", err)
	}
	active := func(ctx context.Context, id uuid.UUID) (bool, error) {
		u, err := client.User.Get(ctx, id)
		return err == nil && u.Status == entuser.StatusActive, nil
	}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	ok := func(c *gin.Context) { c.Status(http.StatusNoContent) }
	g := r.Group("/v1", JWT(signer, tokens, active))
	g.GET("/admin/ping", AdminOnly(), ok)
	g.POST("/me/change-password", SessionOnly(), ok)
	g.POST("/admin/topup", RequireTOTPStepUp(stepUpAlwaysSatisfied{}), ok)

	call := func(method, path, bearer string) int {
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set("Authorization", "Bearer "+bearer)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code
	}
	expect := func(name string, got, want int) {
		t.Helper()
		if got != want {
			t.Errorf("%s: got %d, want %d", name, got, want)
		}
	}

	expect("token acts with the owner's role", call("GET", "/v1/admin/ping", token), 204)
	expect("unknown token", call("GET", "/v1/admin/ping", "jcp-not-a-real-token"), 401)
	expect("token on session-only route", call("POST", "/v1/me/change-password", token), 403)
	expect("session on session-only route", call("POST", "/v1/me/change-password", session), 204)
	expect("token never passes step-up", call("POST", "/v1/admin/topup", token), 403)
	expect("session still passes step-up", call("POST", "/v1/admin/topup", session), 204)

	// A demotion applies to the next token request, not after the JWT TTL.
	client.User.UpdateOneID(admin.ID).SetRole(entuser.RoleUser).ExecX(ctx)
	expect("token after demotion", call("GET", "/v1/admin/ping", token), 403)
	client.User.UpdateOneID(admin.ID).SetRole(entuser.RoleAdmin).ExecX(ctx)

	client.User.UpdateOneID(admin.ID).SetStatus(entuser.StatusBanned).ExecX(ctx)
	expect("token of a banned user", call("GET", "/v1/admin/ping", token), 403)
	client.User.UpdateOneID(admin.ID).SetStatus(entuser.StatusActive).ExecX(ctx)

	// last_used_at is written off the request path.
	var stored []*ent.PersonalAccessToken
	for i := 0; i < 50; i++ {
		if stored, err = tokens.List(ctx, admin.ID); err == nil && len(stored) == 1 && stored[0].LastUsedAt != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(stored) != 1 || stored[0].LastUsedAt == nil {
		t.Fatalf("list after use = %v, %v; want one token with last_used_at", stored, err)
	}
	if err := tokens.Revoke(ctx, admin.ID, stored[0].ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	expect("revoked token", call("GET", "/v1/admin/ping", token), 401)

	past := time.Now().Add(-time.Minute)
	_, expired, err := tokens.Create(ctx, admin.ID, "old", &past)
	if err != nil {
		t.Fatalf("create expired: %v", err)
	}
	expect("expired token", call("GET", "/v1/admin/ping", expired), 401)

	other, err := client.User.Create().SetEmail(uuid.NewString() + "@test.invalid").SetPasswordHash("x").
		SetStatus(entuser.StatusActive).Save(ctx)
	if err != nil {
		t.Fatalf("create other user: %v", err)
	}
	_, live, _ := tokens.Create(ctx, admin.ID, "live", nil)
	liveRows, _ := tokens.List(ctx, admin.ID)
	if err := tokens.Revoke(ctx, other.ID, liveRows[0].ID); err != biz.ErrAccessTokenNotFound {
		t.Errorf("revoking someone else's token: err = %v, want not found", err)
	}
	expect("token survives a foreign revoke attempt", call("GET", "/v1/admin/ping", live), 204)
}
