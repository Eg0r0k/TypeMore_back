package perf

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// SetPlannerCosts pins the fixture database to production planner constants.
//
// Stock Postgres ships random_page_cost = 4 — a spinning-disk model in which
// one random (index) page visit costs four sequential ones. Every deployment
// target of this project is SSD/NVMe storage, where the community convention
// is ~1.1. The difference is not cosmetic: the load fixtures compress a
// production selectivity ratio (one player against millions) into a few
// hundred thousand rows, which parks several pinned plans near the
// seq-vs-index flip point — and there the disk model, not the query, decides
// the plan. The pinned property is "index-driven under production constants",
// so the fixture must plan under them.
//
// ALTER DATABASE (not SET) so every session the pool opens afterwards
// inherits the setting; call this BEFORE the pool is created.
func SetPlannerCosts(ctx context.Context, dsn string) error {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return fmt.Errorf("perf: connect for planner costs: %w", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	// The database name comes from the DSN (the leaderboard fixture creates
	// its own), so the target is whatever this connection landed in.
	if _, err := conn.Exec(ctx, `
		DO $$ BEGIN
			EXECUTE format('ALTER DATABASE %I SET random_page_cost = 1.1', current_database());
		END $$`); err != nil {
		return fmt.Errorf("perf: set planner costs: %w", err)
	}
	return nil
}
