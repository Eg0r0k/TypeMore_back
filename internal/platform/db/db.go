// Package db builds and owns the PostgreSQL connection pool. It is platform
// infrastructure: it knows nothing about auth, runs, or any other domain — those
// packages receive a *pgxpool.Pool and run their own queries against it.
package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	pgxuuid "github.com/vgarvardt/pgx-google-uuid/v5"
)

// NewPool creates a pgx connection pool from a DSN, verifies connectivity with a
// ping, and registers the google/uuid codec on every connection so domain code
// can use uuid.UUID directly instead of pgtype.UUID.
//
// The pool is safe for concurrent use and must be Closed by the caller on
// shutdown. A failed ping is returned as an error (with the pool already closed)
// so the process fails fast at startup rather than on the first query.
func NewPool(ctx context.Context, dsn string, maxConns int32) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse database dsn: %w", err)
	}
	if maxConns > 0 {
		cfg.MaxConns = maxConns
	}
	// AfterConnect runs once per physical connection; registering the uuid codec
	// here makes uuid.UUID scan/encode work everywhere in the pool.
	cfg.AfterConnect = func(_ context.Context, conn *pgx.Conn) error {
		pgxuuid.Register(conn.TypeMap())
		return nil
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create database pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return pool, nil
}
