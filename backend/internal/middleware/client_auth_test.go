package middleware

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/data"
	"github.com/jucode/jucode/internal/data/ent"
	entuser "github.com/jucode/jucode/internal/data/ent/user"
)

// The open API reads the caller through UserIDFrom; an API key must set it
// like an OAuth device token does, or /v1/open/* answers 401 "no user".
//
//	JUCODE_TEST_PG_DSN='postgres://...' go test ./internal/middleware -run ClientAuthApiKey
func TestClientAuthApiKeySetsUserID(t *testing.T) {
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
	keys := biz.NewApiKeySvc(data.NewWith(client, nil, db))

	u := client.User.Create().SetEmail(uuid.NewString() + "@test.invalid").SetPasswordHash("x").
		SetStatus(entuser.StatusActive).SaveX(ctx)
	created, err := keys.Create(ctx, u.ID, biz.CreateInput{Name: "open-api"})
	if err != nil {
		t.Fatalf("create key: %v", err)
	}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/v1/open/balance", ClientAuth(keys, nil, nil), func(c *gin.Context) {
		id, ok := UserIDFrom(c)
		if !ok || id != u.ID {
			c.Status(http.StatusUnauthorized)
			return
		}
		c.Status(http.StatusNoContent)
	})
	req := httptest.NewRequest("GET", "/v1/open/balance", nil)
	req.Header.Set("Authorization", "Bearer "+created.Secret)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("api key caller: got %d, want 204", w.Code)
	}
}
