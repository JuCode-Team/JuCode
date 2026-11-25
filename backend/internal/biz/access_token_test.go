package biz

import (
	"context"
	"database/sql"
	"os"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/jucode/jucode/internal/data"
	"github.com/jucode/jucode/internal/data/ent"
	entuser "github.com/jucode/jucode/internal/data/ent/user"
)

// Changing or resetting a password (and changing email) calls RevokeAll; a
// token that survived it would keep a compromised account open.
//
//	JUCODE_TEST_PG_DSN='postgres://postgres:test@127.0.0.1:5499/jucode_test?sslmode=disable' \
//	JUCODE_TEST_REDIS_ADDR=127.0.0.1:6399 go test ./internal/biz -run RevokeAllRevokesAccessTokens
func TestRevokeAllRevokesAccessTokens(t *testing.T) {
	dsn, addr := os.Getenv("JUCODE_TEST_PG_DSN"), os.Getenv("JUCODE_TEST_REDIS_ADDR")
	if dsn == "" || addr == "" {
		t.Skip("JUCODE_TEST_PG_DSN or JUCODE_TEST_REDIS_ADDR not set")
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
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	t.Cleanup(func() { _ = client.Close(); _ = rdb.Close() })
	d := data.NewWith(client, rdb, db)
	tokens := NewAccessTokenSvc(d)

	u, err := client.User.Create().SetEmail(uuid.NewString() + "@test.invalid").SetPasswordHash("x").SetStatus(entuser.StatusActive).Save(ctx)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	_, raw, err := tokens.Create(ctx, u.ID, "agent", nil)
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	if err := NewAccount(d, nil, nil).RevokeAll(ctx, u.ID); err != nil {
		t.Fatalf("revoke all: %v", err)
	}
	if _, _, err := tokens.Resolve(ctx, raw); err != ErrAccessTokenInvalid {
		t.Fatalf("token after RevokeAll: err = %v, want invalid", err)
	}
}
