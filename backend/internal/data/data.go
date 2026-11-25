// Package data wires the Ent client and Redis client into a single Data
// container that handlers and biz code depend on. Keeping the dependency
// graph here (instead of in main) makes wiring testable.
package data

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/redis/go-redis/v9"

	"github.com/jucode/jucode/internal/conf"
	"github.com/jucode/jucode/internal/data/ent"
)

type Data struct {
	Ent   *ent.Client
	Redis *redis.Client

	rawDB *sql.DB
}

// DB returns the underlying *sql.DB so callers (e.g. aggregation queries
// that don't fit Ent cleanly) can run raw SQL. Read-only convention —
// mutation should still flow through Ent for hook coverage.
func (d *Data) DB() *sql.DB { return d.rawDB }

// NewWith wraps already-open clients. Intended for tests that bring their own
// Postgres/Redis (the raw *sql.DB is otherwise unreachable from outside this
// package, and the stats aggregation reads it via DB()). Production wiring
// goes through New.
func NewWith(entClient *ent.Client, rdb *redis.Client, rawDB *sql.DB) *Data {
	return &Data{Ent: entClient, Redis: rdb, rawDB: rawDB}
}

// New opens Postgres + Redis, returns a wired Data. Caller must call Close.
func New(ctx context.Context, cfg *conf.Config) (*Data, error) {
	db, err := sql.Open("pgx", cfg.Database.DSN)
	if err != nil {
		return nil, fmt.Errorf("postgres open: %w", err)
	}
	db.SetMaxOpenConns(cfg.Database.MaxOpenConns)
	db.SetMaxIdleConns(cfg.Database.MaxIdleConns)
	db.SetConnMaxLifetime(cfg.Database.ConnMaxLifetime)

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("postgres ping: %w", err)
	}

	drv := entsql.OpenDB(dialect.Postgres, db)
	client := ent.NewClient(ent.Driver(drv))

	rdb := redis.NewClient(&redis.Options{
		Addr:     cfg.Redis.Addr,
		Password: cfg.Redis.Password,
		DB:       cfg.Redis.DB,
	})
	if err := rdb.Ping(pingCtx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("redis ping: %w", err)
	}

	return &Data{Ent: client, Redis: rdb, rawDB: db}, nil
}

func (d *Data) Close() {
	if d.Ent != nil {
		_ = d.Ent.Close()
	}
	if d.Redis != nil {
		_ = d.Redis.Close()
	}
	if d.rawDB != nil {
		_ = d.rawDB.Close()
	}
}
