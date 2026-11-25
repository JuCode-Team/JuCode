package biz

import (
	"context"
	"database/sql"
	"os"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"

	"github.com/jucode/jucode/internal/data"
	"github.com/jucode/jucode/internal/data/ent"
)

// Postgres-backed, same opt-in as the key group tests:
//
//	JUCODE_TEST_PG_DSN='postgres://...' go test ./internal/biz -run PickerHidden
func TestModelPickerHiddenPG(t *testing.T) {
	dsn := os.Getenv("JUCODE_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("JUCODE_TEST_PG_DSN not set; skipping Postgres-backed model tests")
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
	svc := NewModelSvc(data.NewWith(client, nil, db))

	create := func(hidden bool) *ent.Model {
		m, err := svc.CreateModel(ctx, CreateModelInput{
			Name: "ph-" + uuid.NewString(), BillingMode: "per_request", PickerHidden: hidden,
		})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		return m
	}
	if create(false).PickerHidden {
		t.Fatal("default create is hidden")
	}
	m := create(true)
	if !m.PickerHidden {
		t.Fatal("create with picker_hidden lost the flag")
	}

	// An update that does not mention the flag leaves it alone.
	desc := "kept"
	m, err = svc.UpdateModel(ctx, m.ID, UpdateModelInput{Description: &desc})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if !m.PickerHidden {
		t.Fatal("unrelated update cleared picker_hidden")
	}
	off := false
	m, err = svc.UpdateModel(ctx, m.ID, UpdateModelInput{PickerHidden: &off})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if m.PickerHidden || !m.Enabled {
		t.Fatalf("after clearing: picker_hidden=%v enabled=%v", m.PickerHidden, m.Enabled)
	}
}
