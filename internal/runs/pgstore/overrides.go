package pgstore

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/typemore/typemore-server/internal/runs"
	"github.com/typemore/typemore-server/internal/runs/runsdb"
	"github.com/typemore/typemore-server/internal/runstatus"
)

// Projector is notified, inside the transaction that changed a run's status,
// that the run's projection may have moved.
//
// Declared HERE, at the consumer, exactly as `internal/replay/pgstore` declares
// its own copy — neither domain imports the other, and the composition root
// decides whether a deployment projects at all. The interface is identical on
// purpose: the leaderboard adapter satisfies both, and "accepted" and "on the
// board" stay one atomic fact whether the status was decided by the worker or by
// a person.
//
// A nil Projector means the deployment does not project, and an override then
// changes the status and nothing else — the same contract the replay queue has.
type Projector interface {
	ProjectRun(ctx context.Context, tx pgx.Tx, runID uuid.UUID) error
}

// WithProjector attaches the projection seam. Returns the store so wiring reads
// as one expression in the composition root.
func (s *Store) WithProjector(p Projector) *Store {
	s.projector = p
	return s
}

// OverrideRunStatus moves one run's status by hand and records who did it.
//
// Everything happens in ONE transaction, and the membership of that transaction
// is the whole design:
//
//   - the run is locked and re-read, so two operators acting at once serialise
//     rather than both reading the pre-move status and writing contradictory
//     audit rows;
//   - the status is written;
//   - the audit row is appended;
//   - the leaderboard projection is brought back in line.
//
// A rollback therefore takes all four with it. The alternative — status now,
// projection later — is a window in which a reinstated run is `accepted` and
// absent from its board, which is the state a moderator would be reinstating it
// out of.
//
// What it does NOT touch: the verdict, the server's numbers, the client's. An
// override disagrees with what the evidence was taken to MEAN. Rewriting the
// evidence would destroy the only record of why the run was flagged in the first
// place, and the next person to look would find a decision with no case behind it.
func (s *Store) OverrideRunStatus(ctx context.Context, p runs.OverrideParams) (runs.StatusOverride, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return runs.StatusOverride{}, fmt.Errorf("runs: begin override: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	current, err := q.RunStatusForOverride(ctx, p.RunID)
	if errors.Is(err, pgx.ErrNoRows) {
		return runs.StatusOverride{}, runs.ErrNotFound
	}
	if err != nil {
		return runs.StatusOverride{}, fmt.Errorf("runs: read run for override: %w", err)
	}

	// A pending run has no verdict to disagree with. Refused here as well as by
	// the table's CHECK, so the caller gets a reason rather than a constraint.
	if current.Status == runstatus.Pending {
		return runs.StatusOverride{}, runs.ErrRunNotJudged
	}
	if current.Status == p.ToStatus {
		return runs.StatusOverride{}, runs.ErrStatusUnchanged
	}

	if err := q.SetRunStatus(ctx, runsdb.SetRunStatusParams{
		ID:     p.RunID,
		Status: p.ToStatus,
	}); err != nil {
		return runs.StatusOverride{}, fmt.Errorf("runs: set status: %w", err)
	}

	row, err := q.InsertRunStatusOverride(ctx, runsdb.InsertRunStatusOverrideParams{
		RunID:      p.RunID,
		FromStatus: current.Status,
		ToStatus:   p.ToStatus,
		Reason:     p.Reason,
		DecidedBy:  p.DecidedBy,
	})
	if err != nil {
		return runs.StatusOverride{}, fmt.Errorf("runs: record override: %w", err)
	}

	// Inside the same transaction, for the same reason the worker projects
	// inside its verdict transaction: a run's status and its board membership
	// are one fact or they are two facts that can disagree.
	if s.projector != nil {
		if err := s.projector.ProjectRun(ctx, tx, p.RunID); err != nil {
			return runs.StatusOverride{}, fmt.Errorf("runs: project override: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return runs.StatusOverride{}, fmt.Errorf("runs: commit override: %w", err)
	}
	return runs.StatusOverride{
		ID:         row.ID,
		RunID:      row.RunID,
		FromStatus: row.FromStatus,
		ToStatus:   row.ToStatus,
		Reason:     row.Reason,
		DecidedBy:  row.DecidedBy,
		DecidedAt:  row.DecidedAt,
	}, nil
}

// RunStatusOverrides is one run's decision history, newest first.
func (s *Store) RunStatusOverrides(ctx context.Context, runID uuid.UUID) ([]runs.StatusOverride, error) {
	rows, err := s.q.ListRunStatusOverrides(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("runs: list overrides: %w", err)
	}
	out := make([]runs.StatusOverride, 0, len(rows))
	for i := range rows {
		out = append(out, runs.StatusOverride{
			ID:            rows[i].ID,
			RunID:         rows[i].RunID,
			FromStatus:    rows[i].FromStatus,
			ToStatus:      rows[i].ToStatus,
			Reason:        rows[i].Reason,
			DecidedBy:     rows[i].DecidedBy,
			DecidedByName: rows[i].DecidedByName,
			DecidedAt:     rows[i].DecidedAt,
		})
	}
	return out, nil
}

// RunsForReview lists judged runs at or above a suspicion floor, ordered by
// sortKey ('suspicion' | 'date' | 'player'), one page at a time. The second
// return is the pre-LIMIT total, carried by the query itself.
func (s *Store) RunsForReview(ctx context.Context, minSuspicion float64, sortKey string, limit, offset int32) ([]runs.ReviewRow, int64, error) {
	var floor pgtype.Numeric
	if err := floor.Scan(strconv.FormatFloat(minSuspicion, 'f', -1, 64)); err != nil {
		return nil, 0, fmt.Errorf("runs: review floor: %w", err)
	}
	rows, err := s.q.ListRunsForReview(ctx, runsdb.ListRunsForReviewParams{
		MinSuspicion: floor,
		SortKey:      sortKey,
		RowLimit:     limit,
		RowOffset:    offset,
	})
	if err != nil {
		return nil, 0, fmt.Errorf("runs: review queue: %w", err)
	}
	var total int64
	if len(rows) > 0 {
		total = rows[0].Total
	}
	out := make([]runs.ReviewRow, 0, len(rows))
	for i := range rows {
		row := runs.ReviewRow{
			ID:         rows[i].ID,
			Status:     rows[i].Status,
			Mode:       rows[i].Mode,
			Overridden: rows[i].AlreadyOverridden,
			Metrics:    rows[i].ServerMetrics,
			Validation: rows[i].Validation,
			CreatedAt:  rows[i].CreatedAt,
		}
		row.UserID = rows[i].UserID
		if rows[i].DisplayName != nil {
			row.DisplayName = *rows[i].DisplayName
		}
		row.Lang = rows[i].Lang
		if rows[i].Suspicion.Valid {
			if f, err := rows[i].Suspicion.Float64Value(); err == nil {
				row.Suspicion = f.Float64
			}
		}
		out = append(out, row)
	}
	return out, total, nil
}
