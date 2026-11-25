package user

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/data"
	"github.com/jucode/jucode/internal/data/ent"
	"github.com/jucode/jucode/internal/middleware"
)

func TestSendEmailVerificationRequiresUser(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	(&Handler{}).Mount(r.Group("/v1/me"))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/me/send-email-verification", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body)
	}
}

func TestSendEmailVerificationUsesSessionUser(t *testing.T) {
	dsn := os.Getenv("JUCODE_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("JUCODE_TEST_PG_DSN not set")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	client := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	t.Cleanup(func() { _ = client.Close() })
	ctx := context.Background()
	if err := client.Schema.Create(ctx); err != nil {
		t.Fatal(err)
	}
	d := data.NewWith(client, nil, db)
	account := biz.NewAccount(d, nil, biz.NewSettingsSvc(d))
	u := client.User.Create().SetEmail(uuid.NewString() + "@test.invalid").SetPasswordHash("x").SaveX(ctx)
	other := client.User.Create().SetEmail(uuid.NewString() + "@test.invalid").SetPasswordHash("x").SetEmailVerified(true).SaveX(ctx)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	g := r.Group("/v1/me", func(c *gin.Context) { c.Set(middleware.CtxUserID, u.ID) })
	(&Handler{users: client, account: account}).Mount(g)
	request := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		// Neither an address nor a user ID in the body selects the recipient.
		r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/me/send-email-verification", strings.NewReader(`{"email":"`+other.Email+`","user_id":"`+other.ID.String()+`"}`)))
		return w
	}
	if w := request(); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("unverified current user with SMTP disabled: %d %s", w.Code, w.Body)
	}
	client.User.UpdateOneID(u.ID).SetEmailVerified(true).ExecX(ctx)
	if w := request(); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "email_already_verified") {
		t.Fatalf("verified current user: %d %s", w.Code, w.Body)
	}
}
