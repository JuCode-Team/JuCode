package biz

import (
	"context"
	"database/sql"
	"os"
	"slices"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/data"
	"github.com/jucode/jucode/internal/data/ent"
	entgroup "github.com/jucode/jucode/internal/data/ent/modelgroup"
)

func TestVisibleGroupIDs(t *testing.T) {
	a, retired := uuid.New(), uuid.New()
	live := map[uuid.UUID]struct{}{a: {}}
	if got := VisibleGroupIDs([]uuid.UUID{retired, a}, live); !slices.Equal(got, []uuid.UUID{a}) {
		t.Fatalf("visible = %v, want [a]", got)
	}
	if got := VisibleGroupIDs([]uuid.UUID{retired}, live); got == nil || len(got) != 0 {
		t.Fatalf("all-retired = %#v, want empty non-nil slice", got)
	}
}

// Postgres-backed, same opt-in as the invite tests:
//
//	JUCODE_TEST_PG_DSN='postgres://...' go test ./internal/biz -run KeyGroups
func testKeyGroupsEnv(t *testing.T) (*ApiKeySvc, *ent.Client) {
	t.Helper()
	dsn := os.Getenv("JUCODE_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("JUCODE_TEST_PG_DSN not set; skipping Postgres-backed key group tests")
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
	return NewApiKeySvc(data.NewWith(client, nil, db)), client
}

// A restricted key must never become unrestricted because its groups went
// away: unbind, prune, and saving the empty visible list all keep the ids.
func TestKeyGroupsNeverWidenPG(t *testing.T) {
	keys, client := testKeyGroupsEnv(t)
	ctx := context.Background()
	user := client.User.Create().SetEmail(uuid.NewString() + "@example.com").SetPasswordHash("x").SaveX(ctx)
	newGroup := func() uuid.UUID {
		return client.ModelGroup.Create().SetName("kg-" + uuid.NewString()).
			SetBillingSource(entgroup.BillingSourceBalanceOnly).SaveX(ctx).ID
	}
	disable := func(id uuid.UUID) { client.ModelGroup.UpdateOneID(id).SetEnabled(false).ExecX(ctx) }
	a, b := newGroup(), newGroup()
	scope := ApiKeyScope{UserID: user.ID}
	newKey := func(ids ...uuid.UUID) *ent.ApiKey {
		res, err := keys.Create(ctx, user.ID, CreateInput{Name: "k", AllowedGroupIDs: ids, BillingScope: "personal"})
		if err != nil {
			t.Fatalf("create key: %v", err)
		}
		return res.Record
	}
	stored := func(k *ent.ApiKey) []uuid.UUID { return client.ApiKey.GetX(ctx, k.ID).AllowedGroupIds }

	shared, sole := newKey(a, b), newKey(b)
	disable(b)
	if err := keys.UnbindGroup(ctx, b); err != nil {
		t.Fatalf("unbind: %v", err)
	}
	if got := stored(shared); !slices.Equal(got, []uuid.UUID{a}) {
		t.Fatalf("shared key after unbind = %v, want [a]", got)
	}
	if got := stored(sole); !slices.Equal(got, []uuid.UUID{b}) {
		t.Fatalf("sole key after unbind = %v, want [b] kept", got)
	}

	// Listing prunes, but not down to an empty list.
	pruned := newKey(a)
	disable(a)
	if _, err := keys.List(ctx, user.ID); err != nil {
		t.Fatalf("list: %v", err)
	}
	if got := stored(pruned); !slices.Equal(got, []uuid.UUID{a}) {
		t.Fatalf("pruned key = %v, want [a] kept", got)
	}

	// The client saw [] for sole; saving that back keeps the restriction.
	empty := []uuid.UUID{}
	if _, err := keys.UpdateScoped(ctx, scope, sole.ID, UpdateInput{AllowedGroupIDs: &empty}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if got := stored(sole); !slices.Equal(got, []uuid.UUID{b}) {
		t.Fatalf("sole key after empty save = %v, want [b] kept", got)
	}

	// A key that was never restricted can still be saved as unrestricted.
	open := newKey()
	if _, err := keys.UpdateScoped(ctx, scope, open.ID, UpdateInput{AllowedGroupIDs: &empty}); err != nil {
		t.Fatalf("update open: %v", err)
	}
	if got := stored(open); len(got) != 0 {
		t.Fatalf("open key = %v, want []", got)
	}
}
