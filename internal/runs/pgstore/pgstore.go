// Package pgstore is the PostgreSQL implementation of the runs domain's Store
// interface, backed by the sqlc-generated runsdb queries. It converts generated
// rows into runs domain types so nothing outside this package depends on the
// generated code.
package pgstore

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/typemore/typemore-server/internal/runs"
	"github.com/typemore/typemore-server/internal/runs/runsdb"
)

// Store implements runs.Store against Postgres.
type Store struct {
	pool *pgxpool.Pool
	q    *runsdb.Queries
}

// Compile-time check that Store satisfies the consumer interface.
var _ runs.Store = (*Store)(nil)

// New builds a Store from a pgx pool.
func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool, q: runsdb.New(pool)}
}

// mapErr translates pgx's no-rows sentinel into the domain's ErrNotFound,
// leaving everything else as-is.
func mapErr(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return runs.ErrNotFound
	}
	return err
}

// CreateRun inserts a run and returns its server-assigned id, status, and
// creation time.
func (s *Store) CreateRun(ctx context.Context, p runs.CreateRunParams) (runs.Run, error) {
	row, err := s.q.CreateRun(ctx, runsdb.CreateRunParams{
		UserID:                  p.UserID,
		Mode:                    p.Mode,
		DurationMs:              p.DurationMs,
		WordCount:               p.WordCount,
		Lang:                    p.Lang,
		Seed:                    p.Seed,
		DictHash:                p.DictHash,
		Setup:                   p.Setup,
		ClientMetrics:           p.ClientMetrics,
		ClientScore:             p.ClientScore,
		ScoreVersion:            p.ScoreVersion,
		Log:                     p.Log,
		LogBytes:                p.LogBytes,
		RestartsSinceLastSubmit: p.RestartsSinceLastSubmit,
	})
	if err != nil {
		return runs.Run{}, err
	}
	return runs.Run{ID: row.ID, Status: row.Status, CreatedAt: row.CreatedAt}, nil
}

// ListRuns returns up to limit of userID's runs newest-first, continuing after
// the given keyset cursor when non-nil.
func (s *Store) ListRuns(ctx context.Context, userID uuid.UUID, after *runs.Cursor, limit int32) ([]runs.Summary, error) {
	if after == nil {
		rows, err := s.q.ListRunsFirst(ctx, runsdb.ListRunsFirstParams{UserID: userID, Limit: limit})
		if err != nil {
			return nil, err
		}
		out := make([]runs.Summary, len(rows))
		for i := range rows {
			out[i] = firstRowToSummary(rows[i])
		}
		return out, nil
	}
	rows, err := s.q.ListRunsAfter(ctx, runsdb.ListRunsAfterParams{
		UserID:    userID,
		CreatedAt: after.CreatedAt,
		ID:        after.ID,
		Limit:     limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]runs.Summary, len(rows))
	for i := range rows {
		out[i] = afterRowToSummary(rows[i])
	}
	return out, nil
}

// Run returns one run summary owned by userID.
func (s *Store) Run(ctx context.Context, id, userID uuid.UUID) (runs.Summary, error) {
	row, err := s.q.GetRun(ctx, runsdb.GetRunParams{ID: id, UserID: userID})
	if err != nil {
		return runs.Summary{}, mapErr(err)
	}
	return getRowToSummary(row), nil
}

// RunLog returns the gzip log blob for one run owned by userID.
func (s *Store) RunLog(ctx context.Context, id, userID uuid.UUID) ([]byte, error) {
	log, err := s.q.GetRunLog(ctx, runsdb.GetRunLogParams{ID: id, UserID: userID})
	if err != nil {
		return nil, mapErr(err)
	}
	return log, nil
}

// PublicReplay returns one accepted run's playback metadata. Every access rule
// lives in the query's WHERE clause, so "not accepted", "owner banned" and
// "does not exist" all arrive here as the same no-rows error.
func (s *Store) PublicReplay(ctx context.Context, id uuid.UUID) (runs.PublicReplay, error) {
	row, err := s.q.GetPublicReplay(ctx, id)
	if err != nil {
		return runs.PublicReplay{}, mapErr(err)
	}
	return runs.PublicReplay{
		RunID:         id,
		DisplayName:   row.DisplayName,
		Mode:          row.Mode,
		DurationMs:    row.DurationMs,
		WordCount:     row.WordCount,
		Lang:          row.Lang,
		Seed:          row.Seed,
		DictHash:      row.DictHash,
		Setup:         row.Setup,
		ServerMetrics: row.ServerMetrics,
		ServerScore:   row.ServerScore,
		Grade:         row.Grade,
		AchievedAt:    row.CreatedAt,
	}, nil
}

// PublicReplayLog returns the stored gzip event log of one accepted run. The
// blob is handed back untouched — no gunzip, no copy beyond the driver's own
// read — because the handler writes exactly these bytes to the socket.
func (s *Store) PublicReplayLog(ctx context.Context, id uuid.UUID) ([]byte, error) {
	log, err := s.q.GetPublicReplayLog(ctx, id)
	if err != nil {
		return nil, mapErr(err)
	}
	return log, nil
}

// --- row conversions ---
//
// The three summary-shaped queries emit distinct-but-identical generated row
// types, so each gets a tiny converter into the shared runs.Summary.

func firstRowToSummary(r runsdb.ListRunsFirstRow) runs.Summary {
	out := runs.Summary{
		ID:                      r.ID,
		Mode:                    r.Mode,
		DurationMs:              r.DurationMs,
		WordCount:               r.WordCount,
		Lang:                    r.Lang,
		Seed:                    r.Seed,
		DictHash:                r.DictHash,
		Setup:                   r.Setup,
		ClientMetrics:           r.ClientMetrics,
		ClientScore:             r.ClientScore,
		ScoreVersion:            r.ScoreVersion,
		Status:                  r.Status,
		ServerMetrics:           r.ServerMetrics,
		ServerScore:             r.ServerScore,
		Validation:              r.Validation,
		ValidatedAt:             r.ValidatedAt,
		LogBytes:                r.LogBytes,
		RestartsSinceLastSubmit: r.RestartsSinceLastSubmit,
		CreatedAt:               r.CreatedAt,
	}
	applyDerived(&out, r.Derived)
	return out
}

func afterRowToSummary(r runsdb.ListRunsAfterRow) runs.Summary {
	out := runs.Summary{
		ID:                      r.ID,
		Mode:                    r.Mode,
		DurationMs:              r.DurationMs,
		WordCount:               r.WordCount,
		Lang:                    r.Lang,
		Seed:                    r.Seed,
		DictHash:                r.DictHash,
		Setup:                   r.Setup,
		ClientMetrics:           r.ClientMetrics,
		ClientScore:             r.ClientScore,
		ScoreVersion:            r.ScoreVersion,
		Status:                  r.Status,
		ServerMetrics:           r.ServerMetrics,
		ServerScore:             r.ServerScore,
		Validation:              r.Validation,
		ValidatedAt:             r.ValidatedAt,
		LogBytes:                r.LogBytes,
		RestartsSinceLastSubmit: r.RestartsSinceLastSubmit,
		CreatedAt:               r.CreatedAt,
	}
	applyDerived(&out, r.Derived)
	return out
}

func getRowToSummary(r runsdb.GetRunRow) runs.Summary {
	out := runs.Summary{
		ID:                      r.ID,
		Mode:                    r.Mode,
		DurationMs:              r.DurationMs,
		WordCount:               r.WordCount,
		Lang:                    r.Lang,
		Seed:                    r.Seed,
		DictHash:                r.DictHash,
		Setup:                   r.Setup,
		ClientMetrics:           r.ClientMetrics,
		ClientScore:             r.ClientScore,
		ScoreVersion:            r.ScoreVersion,
		Status:                  r.Status,
		ServerMetrics:           r.ServerMetrics,
		ServerScore:             r.ServerScore,
		Validation:              r.Validation,
		ValidatedAt:             r.ValidatedAt,
		LogBytes:                r.LogBytes,
		RestartsSinceLastSubmit: r.RestartsSinceLastSubmit,
		CreatedAt:               r.CreatedAt,
	}
	applyDerived(&out, r.Derived)
	return out
}

// applyDerived unpacks the SQL-side `derived` cells document onto the flat
// summary fields. A decode failure leaves the summary usable without its
// derived cells rather than failing the whole page — the raw documents are
// still on the row.
func applyDerived(out *runs.Summary, raw json.RawMessage) {
	if len(raw) == 0 {
		return
	}
	var cells runs.DerivedCells
	if err := json.Unmarshal(raw, &cells); err != nil {
		return
	}
	out.Grade = cells.Grade
	out.Consistency = cells.Consistency
	out.Chars = cells.Chars
	out.QuoteID = cells.QuoteID
	out.Mods = cells.Mods
}
