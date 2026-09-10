// Package db owns the PostgreSQL connection pool, schema migrations, and all
// SQL queries used by the broker. Queries are grouped by domain across the
// db_*.go files; every one runs through the single pgxpool here.
package db

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/artemavrin/broker/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

// DB wraps the pgx connection pool and exposes the broker's data access
// methods.
type DB struct {
	pool *pgxpool.Pool
	url  string
}

// Connect opens a pooled connection to the database at url and verifies it
// with a ping.
func Connect(ctx context.Context, url string) (*DB, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return &DB{pool: pool, url: url}, nil
}

// Pool returns the underlying pgx pool for callers that need direct access.
func (db *DB) Pool() *pgxpool.Pool { return db.pool }

// URL returns the database connection string (used by the listener, which
// needs its own dedicated connection outside the pool).
func (db *DB) URL() string { return db.url }

// Close drains the pool.
func (db *DB) Close() { db.pool.Close() }

// Migrate applies the schema migrations using a temporary database/sql
// connection backed by the pgx stdlib driver.
//
// An empty dir uses the migrations embedded in the binary, which is what a
// deployed broker does — the release artefact then needs no files beside it.
// A non-empty dir reads them from disk instead, for development and tests that
// run against a working copy.
func (db *DB) Migrate(ctx context.Context, dir string) error {
	sqlDB := stdlib.OpenDBFromPool(db.pool)
	defer sqlDB.Close()
	return runGoose(ctx, sqlDB, dir)
}

func runGoose(ctx context.Context, sqlDB *sql.DB, dir string) error {
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("postgres"); err != nil {
		return err
	}
	if dir == "" {
		// goose keeps the base FS in a package-level variable, so it is set for
		// this call only and restored afterwards.
		goose.SetBaseFS(migrations.FS)
		defer goose.SetBaseFS(nil)
		dir = "."
	}
	return goose.UpContext(ctx, sqlDB, dir)
}
