// Package pgstore is the PostgreSQL implementation of the quote domain's Store
// interface, backed by the sqlc-generated quotedb queries. It also owns the
// write side — the import that publishes a language's corpus — so nothing
// outside this package depends on the generated code or on how a revision is
// retired.
package pgstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/typemore/typemore-server/internal/quote"
	"github.com/typemore/typemore-server/internal/quote/quotedb"
)

// Store implements quote.Store against Postgres.
type Store struct {
	pool *pgxpool.Pool
	q    *quotedb.Queries
}

// Compile-time check that Store satisfies the consumer interface.
var _ quote.Store = (*Store)(nil)

// New builds a Store from a pgx pool.
func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool, q: quotedb.New(pool)}
}

// --- read side ---

// Page returns up to limit published quotes' metadata in browse order.
func (s *Store) Page(ctx context.Context, f quote.Filter, after *quote.Cursor, limit int32) ([]quote.Meta, error) {
	params := quotedb.ListQuotesParams{RowLimit: limit}
	if f.Lang != "" {
		params.Lang = &f.Lang
	}
	if f.Group != nil {
		params.LenGroup = new(int16(*f.Group))
	}
	if after != nil {
		params.AfterCursor = true
		params.AfterLang = after.Lang
		params.AfterGroup = int16(after.Group)
		params.AfterID = after.ID
	}

	rows, err := s.q.ListQuotes(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("quote/pgstore: list: %w", err)
	}
	out := make([]quote.Meta, len(rows))
	for i := range rows {
		out[i] = quote.Meta{
			ID: rows[i].ID, Lang: rows[i].Lang, UpstreamID: rows[i].UpstreamID,
			Source: rows[i].Source, Length: rows[i].Length,
			LenGroup: quote.LenGroup(rows[i].LenGroup), TextHash: rows[i].TextHash,
			CreatedAt: rows[i].CreatedAt,
		}
	}
	return out, nil
}

// Random draws one published quote, text included.
func (s *Store) Random(ctx context.Context, f quote.Filter) (quote.Quote, error) {
	params := quotedb.PickRandomQuoteParams{}
	if f.Lang != "" {
		params.Lang = &f.Lang
	}
	if f.Group != nil {
		params.LenGroup = new(int16(*f.Group))
	}

	row, err := s.q.PickRandomQuote(ctx, params)
	if errors.Is(err, pgx.ErrNoRows) {
		return quote.Quote{}, quote.ErrNotFound
	}
	if err != nil {
		return quote.Quote{}, fmt.Errorf("quote/pgstore: random: %w", err)
	}
	return randomRowToQuote(row), nil
}

// ByID returns one quote, superseded and withdrawn revisions included.
func (s *Store) ByID(ctx context.Context, id uuid.UUID) (quote.Quote, error) {
	row, err := s.q.GetQuote(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return quote.Quote{}, quote.ErrNotFound
	}
	if err != nil {
		return quote.Quote{}, fmt.Errorf("quote/pgstore: get: %w", err)
	}
	return quote.Quote{
		Meta: quote.Meta{
			ID: row.ID, Lang: row.Lang, UpstreamID: row.UpstreamID, Source: row.Source,
			Length: row.Length, LenGroup: quote.LenGroup(row.LenGroup),
			TextHash: row.TextHash, Superseded: row.Superseded,
			Withdrawn: row.Withdrawn, CreatedAt: row.CreatedAt,
		},
		Text: row.Text,
	}, nil
}

// WithdrawnIDs lists the quotes a moderator has taken out of circulation. A set
// rather than a slice because its one caller asks membership questions of it,
// once per bucket in the board index.
func (s *Store) WithdrawnIDs(ctx context.Context) (map[uuid.UUID]struct{}, error) {
	ids, err := s.q.ListWithdrawnQuoteIDs(ctx)
	if err != nil {
		return nil, fmt.Errorf("quote/pgstore: withdrawn ids: %w", err)
	}
	out := make(map[uuid.UUID]struct{}, len(ids))
	for _, id := range ids {
		out[id] = struct{}{}
	}
	return out, nil
}

// randomRowToQuote maps a drawn row. Withdrawn is left false rather than read:
// the draw's own predicate excludes withdrawn rows, so there is no true to
// carry — and a field set from a column the query does not select would be a
// lie waiting for someone to widen the query.
func randomRowToQuote(r quotedb.PickRandomQuoteRow) quote.Quote {
	return quote.Quote{
		Meta: quote.Meta{
			ID: r.ID, Lang: r.Lang, UpstreamID: r.UpstreamID, Source: r.Source,
			Length: r.Length, LenGroup: quote.LenGroup(r.LenGroup),
			TextHash: r.TextHash, Superseded: r.Superseded, CreatedAt: r.CreatedAt,
		},
		Text: r.Text,
	}
}

// --- write side: import ---

// Import publishes one language's corpus, in ONE transaction: either the whole
// language moves or none of it does, so a failure halfway cannot leave a corpus
// where some quotes are the new revision and some are the old.
//
// Immutability is the rule the whole method exists to keep (docs/QUOTES.md).
// Published text is NEVER edited in place — exactly as a published dict_hash is
// never edited (docs/DICTIONARIES.md) — because a stored run regenerates its
// text from the row it was played on, and rewriting that row silently changes
// what an old score was scored against. So for each (lang, upstream_id):
//
//	nothing published there        -> INSERT                       (Inserted)
//	the published bytes match      -> nothing at all               (Unchanged)
//	the bytes differ               -> INSERT beside it and retire
//	                                  the previous revision(s)     (Superseded)
//
// It is driven per quote rather than as one bulk statement on purpose: the
// three outcomes have to be COUNTED, and a COPY that reports "2286 rows" tells
// an operator nothing about whether a re-import quietly replaced the corpus.
func (s *Store) Import(ctx context.Context, lang string, quotes []quote.Incoming) (quote.ImportStats, error) {
	var stats quote.ImportStats

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return stats, fmt.Errorf("quote/pgstore: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	for i := range quotes {
		outcome, err := publish(ctx, q, lang, &quotes[i])
		if err != nil {
			return quote.ImportStats{}, err
		}
		switch outcome {
		case outcomeInserted:
			stats.Inserted++
		case outcomeSuperseded:
			stats.Superseded++
		case outcomeUnchanged:
			stats.Unchanged++
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return quote.ImportStats{}, fmt.Errorf("quote/pgstore: commit %s: %w", lang, err)
	}
	return stats, nil
}

// Count returns how many quote rows exist, retired revisions included. The
// import command prints it so an operator can see the registry's real size next
// to a pass that reported "unchanged".
func (s *Store) Count(ctx context.Context) (int64, error) {
	n, err := s.q.CountQuotes(ctx)
	if err != nil {
		return 0, fmt.Errorf("quote/pgstore: count: %w", err)
	}
	return n, nil
}

type outcome int

const (
	outcomeInserted outcome = iota
	outcomeSuperseded
	outcomeUnchanged
)

// publish brings one (lang, upstream_id) in line with the incoming bytes and
// reports which of the three things it did.
func publish(ctx context.Context, q *quotedb.Queries, lang string, in *quote.Incoming) (outcome, error) {
	rev, err := q.FindQuoteRevision(ctx, quotedb.FindQuoteRevisionParams{
		Lang: lang, UpstreamID: in.UpstreamID, TextHash: in.TextHash,
	})
	switch {
	case err == nil:
		// These exact bytes are already stored. Nothing is rewritten; at most
		// this revision is made current again, which happens only when upstream
		// reverted to a text it had previously replaced.
		restored, err := q.RepublishQuoteRevision(ctx, rev.ID)
		if err != nil {
			return 0, fmt.Errorf("quote/pgstore: republish %s#%d: %w", lang, in.UpstreamID, err)
		}
		retired, err := supersedeOthers(ctx, q, lang, in.UpstreamID, rev.ID)
		if err != nil {
			return 0, err
		}
		if restored > 0 || retired > 0 {
			return outcomeSuperseded, nil
		}
		return outcomeUnchanged, nil

	case errors.Is(err, pgx.ErrNoRows):
		id, err := q.InsertQuoteRevision(ctx, quotedb.InsertQuoteRevisionParams{
			Lang: lang, UpstreamID: in.UpstreamID, Text: in.Text, Source: in.Source,
			Length: in.Length, LenGroup: int16(in.LenGroup), TextHash: in.TextHash,
		})
		if err != nil {
			return 0, fmt.Errorf("quote/pgstore: insert %s#%d: %w", lang, in.UpstreamID, err)
		}
		retired, err := supersedeOthers(ctx, q, lang, in.UpstreamID, id)
		if err != nil {
			return 0, err
		}
		if retired > 0 {
			return outcomeSuperseded, nil
		}
		return outcomeInserted, nil

	default:
		return 0, fmt.Errorf("quote/pgstore: find revision %s#%d: %w", lang, in.UpstreamID, err)
	}
}

func supersedeOthers(ctx context.Context, q *quotedb.Queries, lang string, upstreamID int32, keep uuid.UUID) (int64, error) {
	n, err := q.SupersedeOtherRevisions(ctx, quotedb.SupersedeOtherRevisionsParams{
		Lang: lang, UpstreamID: upstreamID, KeepID: keep,
	})
	if err != nil {
		return 0, fmt.Errorf("quote/pgstore: supersede %s#%d: %w", lang, upstreamID, err)
	}
	return n, nil
}

// --- write side: moderation ---
//
// The admin surface's half of this store. It is on the same *Store as the
// reads (one pool, one adapter) but behind its own interface at the consumer
// end (quote.ModerationStore), so the public service cannot reach it by
// holding the value it already has.

// Withdraw takes a quote out of circulation, keeping any EARLIER withdrawal
// intact: the first moderator's decision is the record, and the boolean tells
// the handler whether this call is what changed the world.
func (s *Store) Withdraw(ctx context.Context, id, actor uuid.UUID, reason string) (quote.Withdrawal, bool, error) {
	row, err := s.q.WithdrawQuote(ctx, quotedb.WithdrawQuoteParams{
		ID: id, Actor: actor, Reason: reason,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return quote.Withdrawal{}, false, quote.ErrNotFound
	}
	if err != nil {
		return quote.Withdrawal{}, false, fmt.Errorf("quote/pgstore: withdraw: %w", err)
	}
	return toWithdrawal(row.WithdrawnAt, row.WithdrawnBy, row.WithdrawnReason), row.NewlyWithdrawn, nil
}

// Restore puts a withdrawn quote back into circulation, reporting whether it
// changed anything. A quote that was never withdrawn is not an error: the
// caller asked for a state that already holds, which is the same idempotency
// the unban route answers with.
func (s *Store) Restore(ctx context.Context, id uuid.UUID) (bool, error) {
	n, err := s.q.RestoreQuote(ctx, id)
	if err != nil {
		return false, fmt.Errorf("quote/pgstore: restore: %w", err)
	}
	return n > 0, nil
}

// Moderated returns one quote with its withdrawal record.
func (s *Store) Moderated(ctx context.Context, id uuid.UUID) (quote.Quote, quote.Withdrawal, error) {
	row, err := s.q.GetQuoteWithdrawal(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return quote.Quote{}, quote.Withdrawal{}, quote.ErrNotFound
	}
	if err != nil {
		return quote.Quote{}, quote.Withdrawal{}, fmt.Errorf("quote/pgstore: moderated: %w", err)
	}
	q := quote.Quote{
		Meta: quote.Meta{
			ID: row.ID, Lang: row.Lang, UpstreamID: row.UpstreamID, Source: row.Source,
			Length: row.Length, LenGroup: quote.LenGroup(row.LenGroup),
			Superseded: row.Superseded, Withdrawn: row.WithdrawnAt != nil,
			CreatedAt: row.CreatedAt,
		},
		Text: row.Text,
	}
	return q, toWithdrawal(row.WithdrawnAt, row.WithdrawnBy, row.WithdrawnReason), nil
}

// toWithdrawal folds the three nullable columns into the record. They move
// together by CHECK (00025) apart from the actor, which is ON DELETE SET NULL
// and so may be absent from a withdrawal that definitely happened.
func toWithdrawal(at *time.Time, by *uuid.UUID, reason *string) quote.Withdrawal {
	if at == nil {
		return quote.Withdrawal{}
	}
	w := quote.Withdrawal{At: *at, By: by}
	if reason != nil {
		w.Reason = *reason
	}
	return w
}
