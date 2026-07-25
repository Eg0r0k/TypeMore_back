// Package pgstore is the PostgreSQL implementation of the replay domain's Queue
// interface, backed by the sqlc-generated replaydb queries. It converts
// generated rows into replay domain types so nothing outside this package
// depends on the generated code.
package pgstore

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/typemore/typemore-server/internal/replay"
	"github.com/typemore/typemore-server/internal/replay/replaydb"
)

// Queue implements replay.Queue against Postgres.
type Queue struct {
	pool *pgxpool.Pool
	q    *replaydb.Queries
}

// Compile-time check that Queue satisfies the consumer interface.
var _ replay.Queue = (*Queue)(nil)

// New builds a Queue from a pgx pool.
func New(pool *pgxpool.Pool) *Queue {
	return &Queue{pool: pool, q: replaydb.New(pool)}
}

// ProcessBatch is the whole queue protocol in one transaction: claim pending
// rows with FOR UPDATE SKIP LOCKED, decide each, write the verdicts, commit.
//
// The row locks are held for the length of the batch on purpose. It is the
// cheapest correct design for one job type: there is no 'processing' status to
// reconcile, a crashed or killed worker rolls back to exactly the state it
// started from, and a concurrent worker's SKIP LOCKED walks straight past the
// rows this one holds. The cost is transaction duration, which is bounded by
// batchSize × the per-run interrupt budget. See docs/REPLAY.md.
func (q *Queue) ProcessBatch(ctx context.Context, limit int32, decide func(context.Context, replay.PendingRun) replay.Decision) (int, error) {
	tx, err := q.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("replay/pgstore: begin: %w", err)
	}
	// Rollback is a no-op after a successful commit; on any early return it is
	// what returns the claimed rows to the queue.
	defer func() { _ = tx.Rollback(ctx) }()

	qtx := q.q.WithTx(tx)
	rows, err := qtx.ClaimPendingRuns(ctx, limit)
	if err != nil {
		return 0, fmt.Errorf("replay/pgstore: claim: %w", err)
	}
	if len(rows) == 0 {
		// Nothing to do — commit (releasing the snapshot) rather than leaving an
		// idle transaction behind on an empty queue.
		if err := tx.Commit(ctx); err != nil {
			return 0, fmt.Errorf("replay/pgstore: commit: %w", err)
		}
		return 0, nil
	}

	for i := range rows {
		row := &rows[i]
		d := decide(ctx, toPendingRun(row))
		if err := qtx.ApplyReplayDecision(ctx, toDecisionParams(row.ID, d)); err != nil {
			return 0, fmt.Errorf("replay/pgstore: apply decision for run %s: %w", row.ID, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("replay/pgstore: commit: %w", err)
	}
	return len(rows), nil
}

func toPendingRun(r *replaydb.ClaimPendingRunsRow) replay.PendingRun {
	return replay.PendingRun{
		ID:            r.ID,
		Seed:          r.Seed,
		DictHash:      r.DictHash,
		ScoreVersion:  r.ScoreVersion,
		Setup:         r.Setup,
		ClientMetrics: r.ClientMetrics,
		ClientScore:   r.ClientScore,
		Log:           r.Log,
		Attempts:      r.Attempts,
	}
}

func toDecisionParams(id uuid.UUID, d replay.Decision) replaydb.ApplyReplayDecisionParams {
	var bundle *string
	if d.BundleSHA != "" {
		bundle = &d.BundleSHA
	}
	return replaydb.ApplyReplayDecisionParams{
		ID:            id,
		Status:        d.Status,
		ServerMetrics: d.ServerMetrics,
		ServerScore:   d.ServerScore,
		Validation:    d.Validation,
		BundleSha:     bundle,
		Attempts:      d.Attempts,
		LastError:     d.LastError,
	}
}
