// Package pgstore is the PostgreSQL implementation of the replay domain's Queue
// interface, backed by the sqlc-generated replaydb queries. It converts
// generated rows into replay domain types so nothing outside this package
// depends on the generated code.
package pgstore

import (
	"context"
	"encoding/json"
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
	return q.inTx(ctx, decide, func(qtx *replaydb.Queries) ([]replay.PendingRun, error) {
		rows, err := qtx.ClaimPendingRuns(ctx, limit)
		if err != nil {
			return nil, err
		}
		out := make([]replay.PendingRun, len(rows))
		for i := range rows {
			out[i] = toPendingRun(rows[i].ID, rows[i].Seed, rows[i].DictHash, rows[i].ScoreVersion,
				rows[i].Setup, rows[i].ClientMetrics, rows[i].ClientScore, rows[i].Log, rows[i].Attempts)
		}
		return out, nil
	})
}

// ProcessStalePolicyBatch re-judges runs whose policy_version is behind the
// current one, with the same locking discipline as the queue. Idempotent by
// construction: applying the decision sets policy_version, so the row stops
// matching the claim and a second pass finds nothing.
func (q *Queue) ProcessStalePolicyBatch(ctx context.Context, policyVersion int16, limit int32, decide func(context.Context, replay.PendingRun) replay.Decision) (int, error) {
	return q.inTx(ctx, decide, func(qtx *replaydb.Queries) ([]replay.PendingRun, error) {
		rows, err := qtx.ClaimStalePolicyRuns(ctx, replaydb.ClaimStalePolicyRunsParams{
			PolicyVersion: &policyVersion,
			RowLimit:      limit,
		})
		if err != nil {
			return nil, err
		}
		out := make([]replay.PendingRun, len(rows))
		for i := range rows {
			out[i] = toPendingRun(rows[i].ID, rows[i].Seed, rows[i].DictHash, rows[i].ScoreVersion,
				rows[i].Setup, rows[i].ClientMetrics, rows[i].ClientScore, rows[i].Log, rows[i].Attempts)
		}
		return out, nil
	})
}

// inTx is the shared unit of work behind both batch operations: one
// transaction, one claim, one decision per row, one commit.
func (q *Queue) inTx(
	ctx context.Context,
	decide func(context.Context, replay.PendingRun) replay.Decision,
	claim func(*replaydb.Queries) ([]replay.PendingRun, error),
) (int, error) {
	tx, err := q.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("replay/pgstore: begin: %w", err)
	}
	// Rollback is a no-op after a successful commit; on any early return it is
	// what returns the claimed rows to the queue.
	defer func() { _ = tx.Rollback(ctx) }()

	qtx := q.q.WithTx(tx)
	runs, err := claim(qtx)
	if err != nil {
		return 0, fmt.Errorf("replay/pgstore: claim: %w", err)
	}
	for i := range runs {
		d := decide(ctx, runs[i])
		if err := qtx.ApplyReplayDecision(ctx, toDecisionParams(runs[i].ID, d)); err != nil {
			return 0, fmt.Errorf("replay/pgstore: apply decision for run %s: %w", runs[i].ID, err)
		}
	}
	// Commit even on an empty claim: releasing the snapshot beats leaving an
	// idle transaction behind on an empty queue.
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("replay/pgstore: commit: %w", err)
	}
	return len(runs), nil
}

// toPendingRun builds the domain row from the columns both claim queries
// select. They are distinct generated types with identical shapes, so the
// converter takes the fields rather than one of the two structs.
func toPendingRun(
	id uuid.UUID, seed int64, dictHash string, scoreVersion int16,
	setup, clientMetrics, clientScore json.RawMessage, log []byte, attempts int16,
) replay.PendingRun {
	return replay.PendingRun{
		ID:            id,
		Seed:          seed,
		DictHash:      dictHash,
		ScoreVersion:  scoreVersion,
		Setup:         setup,
		ClientMetrics: clientMetrics,
		ClientScore:   clientScore,
		Log:           log,
		Attempts:      attempts,
	}
}

func toDecisionParams(id uuid.UUID, d replay.Decision) replaydb.ApplyReplayDecisionParams {
	var bundle *string
	if d.BundleSHA != "" {
		bundle = &d.BundleSHA
	}
	var policy *int16
	if d.PolicyVersion > 0 {
		policy = &d.PolicyVersion
	}
	return replaydb.ApplyReplayDecisionParams{
		ID:            id,
		Status:        d.Status,
		ServerMetrics: d.ServerMetrics,
		ServerScore:   d.ServerScore,
		Validation:    d.Validation,
		BundleSha:     bundle,
		PolicyVersion: policy,
		Attempts:      d.Attempts,
		LastError:     d.LastError,
	}
}

// ListForCalibration reads judged runs without locking or writing anything —
// the input to `replayctl calibrate`. Deliberately NOT part of the replay.Queue
// interface: the worker has no business reading rows it will not judge.
func (q *Queue) ListForCalibration(ctx context.Context, limit int32) ([]replay.CalibrationRun, error) {
	rows, err := q.q.ListRunsForCalibration(ctx, limit)
	if err != nil {
		return nil, fmt.Errorf("replay/pgstore: list for calibration: %w", err)
	}
	out := make([]replay.CalibrationRun, len(rows))
	for i := range rows {
		out[i] = replay.CalibrationRun{
			PendingRun: toPendingRun(rows[i].ID, rows[i].Seed, rows[i].DictHash, rows[i].ScoreVersion,
				rows[i].Setup, rows[i].ClientMetrics, rows[i].ClientScore, rows[i].Log, rows[i].Attempts),
			Status:        rows[i].Status,
			PolicyVersion: rows[i].PolicyVersion,
			CreatedAt:     rows[i].CreatedAt,
		}
	}
	return out, nil
}
