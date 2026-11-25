// Command jucode-migrate applies the Ent schema to the configured Postgres
// database.
//
// Order of operations:
//  1. client.Schema.Create — every table, column and index Ent declares.
//  2. postEntSteps — the raw-SQL tables and indexes Ent cannot express (see
//     migrations.go).
//  3. MigrateLegacyGroupACL — folds legacy model-group ACL lists into grants
//     (a no-op on a fresh database).
package main

import (
	"context"
	"database/sql"
	"flag"
	"log/slog"
	"os"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/jucode/jucode/internal/biz"
	"github.com/jucode/jucode/internal/conf"
	"github.com/jucode/jucode/internal/data/ent"
)

func main() {
	var cfgPath string
	flag.StringVar(&cfgPath, "config", "configs/config.yaml", "path to config yaml")
	flag.Parse()

	cfg, err := conf.Load(cfgPath)
	if err != nil {
		slog.Error("config load failed", "err", err)
		os.Exit(2)
	}

	db, err := sql.Open("pgx", cfg.Database.DSN)
	if err != nil {
		slog.Error("postgres open failed", "err", err)
		os.Exit(1)
	}
	defer db.Close()

	drv := entsql.OpenDB(dialect.Postgres, db)
	client := ent.NewClient(ent.Driver(drv))
	defer client.Close()

	ctx := context.Background()
	if err := client.Schema.Create(ctx); err != nil {
		slog.Error("schema create failed", "err", err)
		os.Exit(1)
	}
	if err := applySteps(ctx, db, postEntSteps); err != nil {
		slog.Error("post-ent migration failed", "err", err)
		os.Exit(1)
	}
	if err := biz.MigrateLegacyGroupACL(ctx, client); err != nil {
		slog.Error("legacy group ACL migrate failed", "err", err)
		os.Exit(1)
	}
	slog.Info("schema applied")
}
