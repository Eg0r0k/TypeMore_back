// Package pgstore is the PostgreSQL implementation of the leaderboard domain's
// Store interface, backed by the sqlc-generated leaderboarddb queries. It also
// owns the write side of the projection — the hook the replay worker calls
// inside its own transaction, and the offline rebuild — so nothing outside this
// package depends on the generated code or on how a cell is maintained.
package pgstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/typemore/typemore-server/internal/leaderboard"
	"github.com/typemore/typemore-server/internal/leaderboard/leaderboarddb"
)

// Store implements leaderboard.Store against Postgres.
type Store struct {
	pool *pgxpool.Pool
	q    *leaderboarddb.Queries
	// requireVerifiedEmail gates board eligibility on the player holding a
	// verified email identity. It is deployment policy, not schema, so it
	// travels with every projection query rather than living in the view.
	requireVerifiedEmail bool
}

// Compile-time check that Store satisfies the consumer interface.
var _ leaderboard.Store = (*Store)(nil)

// New builds a Store from a pgx pool. requireVerifiedEmail is the eligibility
// gate described in docs/LEADERBOARDS.md; changing it takes effect on existing
// runs only after `make rebuild-leaderboards`.
func New(pool *pgxpool.Pool, requireVerifiedEmail bool) *Store {
	return &Store{pool: pool, q: leaderboarddb.New(pool), requireVerifiedEmail: requireVerifiedEmail}
}

// --- write side: projection ---

// ProjectRun brings the leaderboard cell that runID belongs to back in line with
// the runs table, INSIDE the caller's transaction.
//
// It is called by the replay worker for every decision it writes, in the same
// transaction as the status update, which is what makes "accepted" and "on the
// board" a single atomic fact. It does not care WHICH way the status moved:
// promotion, demotion and a re-judged run that did not change all resolve to the
// same question — who holds this cell now? — so the case teams forget (a run
// losing its accepted status while it was the player's best) is not a separate
// code path that can be missing.
//
// A run that has vanished (deleted account) leaves nothing to recompute; the
// entry went with it via ON DELETE CASCADE.
func (s *Store) ProjectRun(ctx context.Context, tx pgx.Tx, runID uuid.UUID) error {
	q := s.q.WithTx(tx)

	row, err := q.RunBucketCell(ctx, runID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("leaderboard/pgstore: resolve cell for run %s: %w", runID, err)
	}

	c := cell{
		userID: row.UserID, quoteID: row.QuoteID,
		mode: row.Mode, durationMs: row.DurationMs, wordCount: row.WordCount,
		lang: row.Lang, textSourceKind: row.TextSourceKind,
	}
	// A run whose shape cannot name a bucket (an unranked mode, a quote id that
	// resolves to nothing) can never have held a slot, so there is nothing to
	// recompute for it.
	bucket, err := c.bucket()
	if err != nil {
		return nil //nolint:nilerr // not an error: unrankable runs simply have no cell
	}

	return s.recompute(ctx, q, bucket, c)
}

// cell is one (player, board) coordinate tuple, in the spelling both the
// per-verdict lookup and the rebuild's enumeration produce. quoteID is the
// discriminator: when it is set the run was played on a quote, and the language
// coordinates describe the run without naming its board.
type cell struct {
	userID                uuid.UUID
	quoteID               *uuid.UUID
	mode                  string
	durationMs, wordCount *int32
	lang, textSourceKind  string
}

// bucket names the board this cell belongs to. A quote run's board is its quote
// and nothing else (SCORING_CONCEPT §6): the mode, size and language it was
// played at describe the run, not a ranking it competes in, so they are dropped
// here rather than carried into a key.
func (c cell) bucket() (leaderboard.Bucket, error) {
	if c.quoteID != nil {
		return leaderboard.NewQuoteBucket(*c.quoteID)
	}
	return leaderboard.NewBucket(c.mode, c.durationMs, c.wordCount, c.lang, c.textSourceKind)
}

// recompute is the single write statement, shared by the projection and the
// rebuild. Both paths reaching the table through here is what makes the rebuilt
// board equal to the incrementally maintained one by construction rather than
// by testing two implementations against each other.
//
// The coordinates are passed as the cell reported them, NOT as the bucket
// re-derived them: which of them the statement actually matches on is SQL's
// decision, and for a quote cell it matches on the quote alone. Go choosing the
// wrong cell is therefore a recompute that finds nothing, never a run filed
// into a board the eligible view would not have put it in.
func (s *Store) recompute(
	ctx context.Context, q *leaderboarddb.Queries,
	bucket leaderboard.Bucket, c cell,
) error {
	err := q.RecomputeLeaderboardCell(ctx, leaderboarddb.RecomputeLeaderboardCellParams{
		BucketKey:            bucket.Key(),
		UserID:               c.userID,
		QuoteID:              c.quoteID,
		Mode:                 c.mode,
		DurationMs:           c.durationMs,
		WordCount:            c.wordCount,
		Lang:                 c.lang,
		TextSourceKind:       c.textSourceKind,
		RequireVerifiedEmail: s.requireVerifiedEmail,
	})
	if err != nil {
		return fmt.Errorf("leaderboard/pgstore: recompute %s for %s: %w", bucket.Key(), c.userID, err)
	}
	return nil
}

// RebuildStats is what one rebuild did.
type RebuildStats struct {
	// Before / After are the entry counts on either side of the rebuild. They
	// differ only when the incremental projection had drifted — which is the
	// whole reason to be able to run this.
	Before int64
	After  int64
	// Cells is how many (player, bucket) cells had an eligible run.
	Cells int
}

// Rebuild recomputes the entire table from accepted runs, in ONE transaction:
// truncate, then replay every cell through the same statement the worker uses.
//
// Deliberately NOT a bulk INSERT..SELECT. Formatting a bucket key has exactly
// one implementation and it is in Go, so the rebuild walks cells rather than
// teaching SQL the format a second time — and reusing the maintenance statement
// means a rebuild cannot disagree with incremental maintenance about who owns a
// slot. TRUNCATE is transactional, so a failure leaves the old board intact.
func (s *Store) Rebuild(ctx context.Context) (RebuildStats, error) {
	var stats RebuildStats

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return stats, fmt.Errorf("leaderboard/pgstore: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	// The enumerate's DISTINCT digests every eligible run — ~50MB of on-disk
	// sort volume at 1M runs, which is ~2× that as an in-memory sort, far past
	// the default 4MB work_mem — since 00019's verdict join tipped its plan
	// from a hash aggregate into a sort (the zone-4 plan check asserts the
	// no-spill contract). SET LOCAL is scoped to this transaction, and the
	// rebuild is a singleton maintenance operation — one session, one grant,
	// released at commit — so this is a bounded allowance, not a global knob.
	if _, err := tx.Exec(ctx, `SET LOCAL work_mem = '256MB'`); err != nil {
		return stats, fmt.Errorf("leaderboard/pgstore: set work_mem: %w", err)
	}

	if stats.Before, err = q.CountLeaderboardEntries(ctx); err != nil {
		return stats, fmt.Errorf("leaderboard/pgstore: count before: %w", err)
	}
	cells, err := q.EnumerateLeaderboardCells(ctx, s.requireVerifiedEmail)
	if err != nil {
		return stats, fmt.Errorf("leaderboard/pgstore: enumerate cells: %w", err)
	}
	if err := q.ClearLeaderboard(ctx); err != nil {
		return stats, fmt.Errorf("leaderboard/pgstore: clear: %w", err)
	}

	// One recompute per BOARD, not per enumerated coordinate tuple. The two are
	// the same thing for a language board, but a quote board ignores the mode,
	// size and language its runs were played at, so two tuples can name it — and
	// recomputing the same cell twice would double-count it in stats.Cells while
	// producing exactly one entry. The bucket key is the identity here for the
	// same reason it is everywhere else: it is what the table is keyed by.
	seen := make(map[string]struct{}, len(cells))
	for i := range cells {
		c := cell{
			userID: cells[i].UserID, quoteID: cells[i].QuoteID,
			mode: cells[i].Mode, durationMs: cells[i].DurationMs,
			wordCount: cells[i].WordCount, lang: cells[i].Lang,
			textSourceKind: cells[i].TextSourceKind,
		}
		bucket, err := c.bucket()
		if err != nil {
			// Unreachable through the eligible view, which only admits ranked
			// shapes and resolvable quotes — but a view change must not
			// silently drop rows.
			return stats, fmt.Errorf("leaderboard/pgstore: eligible run in an unrankable cell: %w", err)
		}
		slot := bucket.Key() + "|" + c.userID.String()
		if _, dup := seen[slot]; dup {
			continue
		}
		seen[slot] = struct{}{}
		if err := s.recompute(ctx, q, bucket, c); err != nil {
			return stats, err
		}
	}
	stats.Cells = len(seen)

	if stats.After, err = q.CountLeaderboardEntries(ctx); err != nil {
		return stats, fmt.Errorf("leaderboard/pgstore: count after: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return stats, fmt.Errorf("leaderboard/pgstore: commit: %w", err)
	}
	return stats, nil
}

// --- read side ---

// Buckets lists every bucket holding at least one visible entry.
func (s *Store) Buckets(ctx context.Context) ([]leaderboard.BucketCount, error) {
	rows, err := s.q.ListLeaderboardBuckets(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]leaderboard.BucketCount, 0, len(rows))
	for i := range rows {
		bucket, err := leaderboard.ParseBucketKey(rows[i].BucketKey)
		if err != nil {
			// A stored key that no longer parses means the format changed
			// without a migration. Surfacing it beats serving a board whose
			// identity nobody can reconstruct.
			return nil, fmt.Errorf("leaderboard/pgstore: stored bucket key %q: %w", rows[i].BucketKey, err)
		}
		out = append(out, leaderboard.BucketCount{Bucket: bucket, Entries: rows[i].Entries})
	}
	return out, nil
}

// Page returns up to limit entries of a bucket's ranking, continuing after the
// keyset position when non-nil.
func (s *Store) Page(ctx context.Context, b leaderboard.Bucket, after *leaderboard.Cursor, limit int32) ([]leaderboard.Entry, error) {
	if after == nil {
		rows, err := s.q.ListLeaderboardPageFirst(ctx, leaderboarddb.ListLeaderboardPageFirstParams{
			BucketKey: b.Key(), RowLimit: limit,
		})
		if err != nil {
			return nil, err
		}
		out := make([]leaderboard.Entry, len(rows))
		for i := range rows {
			out[i] = firstRowToEntry(rows[i])
		}
		return out, nil
	}
	rows, err := s.q.ListLeaderboardPageAfter(ctx, leaderboarddb.ListLeaderboardPageAfterParams{
		BucketKey:  b.Key(),
		Score:      after.Score,
		AchievedAt: after.AchievedAt,
		UserID:     after.UserID,
		RowLimit:   limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]leaderboard.Entry, len(rows))
	for i := range rows {
		out[i] = afterRowToEntry(rows[i])
	}
	return out, nil
}

// PageBefore returns up to limit entries strictly outranking the position, in
// ranking order. SQL hands them back nearest-first (that is what makes LIMIT
// take the position's neighbours instead of rank 1's), so the slice is
// reversed here once rather than by every caller.
func (s *Store) PageBefore(ctx context.Context, b leaderboard.Bucket, before leaderboard.Cursor, limit int32) ([]leaderboard.Entry, error) {
	rows, err := s.q.ListLeaderboardPageBefore(ctx, leaderboarddb.ListLeaderboardPageBeforeParams{
		BucketKey:  b.Key(),
		Score:      before.Score,
		AchievedAt: before.AchievedAt,
		UserID:     before.UserID,
		RowLimit:   limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]leaderboard.Entry, len(rows))
	for i := range rows {
		out[len(rows)-1-i] = beforeRowToEntry(rows[i])
	}
	return out, nil
}

// RankAbove counts the visible entries that outrank a position.
func (s *Store) RankAbove(ctx context.Context, b leaderboard.Bucket, at leaderboard.Cursor) (int64, error) {
	return s.q.CountLeaderboardAbove(ctx, leaderboarddb.CountLeaderboardAboveParams{
		BucketKey:  b.Key(),
		Score:      at.Score,
		AchievedAt: at.AchievedAt,
		UserID:     at.UserID,
	})
}

// EntryFor returns one player's entry with its rank, or leaderboard.ErrNoEntry.
func (s *Store) EntryFor(ctx context.Context, b leaderboard.Bucket, userID uuid.UUID) (leaderboard.Entry, error) {
	row, err := s.q.GetLeaderboardEntry(ctx, leaderboarddb.GetLeaderboardEntryParams{
		BucketKey: b.Key(), UserID: userID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return leaderboard.Entry{}, leaderboard.ErrNoEntry
	}
	if err != nil {
		return leaderboard.Entry{}, err
	}
	entry := getRowToEntry(row)
	above, err := s.RankAbove(ctx, b, leaderboard.Cursor{
		Score: entry.Score, AchievedAt: entry.AchievedAt, UserID: entry.UserID,
	})
	if err != nil {
		return leaderboard.Entry{}, err
	}
	entry.Rank = above + 1
	return entry, nil
}

// --- row conversions ---
//
// The three ranked-row queries emit distinct-but-identical generated types, so
// each gets a tiny converter into the shared leaderboard.Entry.

func firstRowToEntry(r leaderboarddb.ListLeaderboardPageFirstRow) leaderboard.Entry {
	return leaderboard.Entry{
		UserID: r.UserID, DisplayName: r.DisplayName, RunID: r.RunID,
		Score: r.Score, WPM: r.Wpm, Raw: r.Raw, Acc: r.Acc, Grade: r.Grade,
		Mods: r.Mods, AchievedAt: r.AchievedAt, Source: text(r.QuoteSource),
	}
}

func afterRowToEntry(r leaderboarddb.ListLeaderboardPageAfterRow) leaderboard.Entry {
	return leaderboard.Entry{
		UserID: r.UserID, DisplayName: r.DisplayName, RunID: r.RunID,
		Score: r.Score, WPM: r.Wpm, Raw: r.Raw, Acc: r.Acc, Grade: r.Grade,
		Mods: r.Mods, AchievedAt: r.AchievedAt, Source: text(r.QuoteSource),
	}
}

func beforeRowToEntry(r leaderboarddb.ListLeaderboardPageBeforeRow) leaderboard.Entry {
	return leaderboard.Entry{
		UserID: r.UserID, DisplayName: r.DisplayName, RunID: r.RunID,
		Score: r.Score, WPM: r.Wpm, Raw: r.Raw, Acc: r.Acc, Grade: r.Grade,
		Mods: r.Mods, AchievedAt: r.AchievedAt, Source: text(r.QuoteSource),
	}
}

func getRowToEntry(r leaderboarddb.GetLeaderboardEntryRow) leaderboard.Entry {
	return leaderboard.Entry{
		UserID: r.UserID, DisplayName: r.DisplayName, RunID: r.RunID,
		Score: r.Score, WPM: r.Wpm, Raw: r.Raw, Acc: r.Acc, Grade: r.Grade,
		Mods: r.Mods, AchievedAt: r.AchievedAt, Source: text(r.QuoteSource),
	}
}

// text flattens a nullable column into the empty string. A language board has
// no quote and therefore no attribution, and "absent" is exactly what the API
// omits it as.
func text(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
