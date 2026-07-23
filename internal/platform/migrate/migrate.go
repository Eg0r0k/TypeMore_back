// Package migrate applies the embedded goose SQL migrations. It is kept separate
// from package db so the server binary (which needs only the pool) does not pull
// in goose; the migrate command and the integration tests import this package.
package migrate

import (
	"context"
	"database/sql"
	"fmt"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
	"github.com/pressly/goose/v3"

	"github.com/typemore/typemore-server/db/migrations"
)

// dialect is PostgreSQL; goose needs to know it to track applied migrations.
const dialect = "postgres"

// open dials the database via the pgx stdlib driver for goose (goose uses
// database/sql, not pgxpool).
func open(dsn string) (*sql.DB, error) {
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	return sqlDB, nil
}

// Run executes a goose command ("up", "down", "status", "version", "reset", …)
// against the database at dsn using the embedded migration files. It is the
// single entry point shared by the migrate command and tests.
func Run(ctx context.Context, dsn, command string, args ...string) error {
	sqlDB, err := open(dsn)
	if err != nil {
		return err
	}
	defer func() { _ = sqlDB.Close() }()

	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect(dialect); err != nil {
		return fmt.Errorf("set goose dialect: %w", err)
	}
	if err := goose.RunContext(ctx, command, sqlDB, ".", args...); err != nil {
		return fmt.Errorf("goose %s: %w", command, err)
	}
	return nil
}

// Up applies all pending migrations. It is the common case (startup / tests).
func Up(ctx context.Context, dsn string) error {
	return Run(ctx, dsn, "up")
}
