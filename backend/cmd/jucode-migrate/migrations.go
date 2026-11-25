package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"

	"github.com/jucode/jucode/internal/region/control"
)

// migrationStep is one idempotent statement batch applied after Ent's
// Schema.Create. Every statement must be safe to re-run (IF NOT EXISTS /
// guarded DO blocks).
type migrationStep struct {
	name string
	sql  string
}

// postEntSteps lays down what Ent's declarative schema cannot express: the
// raw-SQL regional ledger tables (owned by internal/region/control, created
// whether or not the experimental region feature is enabled) and a partial
// unique index over an expression.
var postEntSteps = []migrationStep{
	{name: "regional_ledger", sql: control.SchemaSQL},
	{
		// One live console-owned (system) key per user and billing scope.
		name: "api_keys_one_live_system",
		sql: `
CREATE UNIQUE INDEX IF NOT EXISTS api_keys_one_live_system
ON api_keys (user_id, (COALESCE(organization_id, '00000000-0000-0000-0000-000000000000'::uuid)))
WHERE system AND revoked_at IS NULL
`,
	},
}

func applySteps(ctx context.Context, db *sql.DB, steps []migrationStep) error {
	for _, step := range steps {
		if _, err := db.ExecContext(ctx, step.sql); err != nil {
			return fmt.Errorf("migration %s: %w", step.name, err)
		}
		slog.Debug("migration applied", "step", step.name)
	}
	return nil
}
