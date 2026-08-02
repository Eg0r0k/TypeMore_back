package quote_test

// Quote withdrawal (docs/REPORTS.md, "The action a report points at").
//
// The load-bearing test in this file is TestWithdrawalSurvivesAReimport. The
// whole reason `withdrawn_at` is a new column rather than a reuse of
// `superseded` is that the importer OWNS superseded and clears it — so a
// moderator's decision spelled that way would be undone by the next
// `make import-quotes`, silently, weeks later. If that test ever goes green
// against a reused column, the design has been quietly reverted.

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/typemore/typemore-server/internal/quote"
)

// withdraw takes a quote out of circulation through the store, as the admin
// surface does.
func (r *registry) withdraw(id uuid.UUID, reason string) (quote.Withdrawal, bool) {
	r.t.Helper()
	rec, changed, err := r.store.Withdraw(context.Background(), id, r.moderator(), reason)
	require.NoError(r.t, err)
	return rec, changed
}

// moderator returns an account id to attribute withdrawals to, created lazily —
// withdrawn_by is a real foreign key.
func (r *registry) moderator() uuid.UUID {
	r.t.Helper()
	var id uuid.UUID
	require.NoError(r.t, r.pool.QueryRow(context.Background(),
		`INSERT INTO users (display_name) VALUES ('quotemod')
		 ON CONFLICT (display_name) DO UPDATE SET display_name = excluded.display_name
		 RETURNING id`).Scan(&id))
	return id
}

// A withdrawn quote leaves DISCOVERY — browsing and the random draw — and stays
// resolvable by id forever, because runs played on it must keep replaying.
func TestWithdrawnQuoteLeavesDiscoveryButStaysResolvable(t *testing.T) {
	r := newRegistry(t)
	r.importLang("english", r.incoming(
		spec{upstreamID: 1, length: 40, group: quote.LenShort},
		spec{upstreamID: 2, length: 40, group: quote.LenShort},
	))
	stored := r.storedQuotes()
	require.Len(t, stored, 2)
	victim := stored[0].ID

	rec, changed := r.withdraw(victim, "offensive")
	assert.True(t, changed)
	assert.Equal(t, "offensive", rec.Reason)
	assert.False(t, rec.At.IsZero())

	// Browsing no longer offers it.
	page, err := r.store.Page(context.Background(), quote.Filter{Lang: "english"}, nil, 50)
	require.NoError(t, err)
	require.Len(t, page, 1, "the withdrawn quote is gone from the index")
	assert.NotEqual(t, victim, page[0].ID)

	// Neither does the draw — repeatedly, since it is random.
	for i := 0; i < 20; i++ {
		drawn, err := r.store.Random(context.Background(), quote.Filter{Lang: "english"})
		require.NoError(t, err)
		require.NotEqual(t, victim, drawn.ID, "a withdrawn quote must never be drawn")
	}

	// But it resolves by id, with its bytes intact and its state announced.
	got, err := r.store.ByID(context.Background(), victim)
	require.NoError(t, err)
	assert.Equal(t, stored[0].Text, got.Text, "the bytes a run was played on cannot change")
	assert.True(t, got.Withdrawn)
}

// THE test this column exists for. The importer clears `superseded` when
// upstream's current bytes match a stored revision (RepublishQuoteRevision); if
// withdrawal shared that column, a re-import would put a quote a moderator
// removed straight back into rotation.
func TestWithdrawalSurvivesAReimport(t *testing.T) {
	r := newRegistry(t)
	rows := r.incoming(spec{upstreamID: 1, length: 40, group: quote.LenShort})
	r.importLang("english", rows)
	victim := r.storedQuotes()[0].ID

	r.withdraw(victim, "offensive")

	// The same corpus, imported again — the unchanged path, which is exactly
	// the one that touches `superseded`.
	stats := r.importLang("english", rows)
	assert.Equal(t, quote.ImportStats{Unchanged: 1}, stats)

	got, err := r.store.ByID(context.Background(), victim)
	require.NoError(t, err)
	assert.True(t, got.Withdrawn,
		"a re-import must not undo a moderator's decision")
	page, err := r.store.Page(context.Background(), quote.Filter{Lang: "english"}, nil, 50)
	require.NoError(t, err)
	assert.Empty(t, page, "and it must not come back into circulation")
}

// Withdrawing twice keeps the FIRST decision: its timestamp, its actor and its
// reason are the record, and the second call says it changed nothing.
func TestWithdrawIsIdempotentAndKeepsTheFirstRecord(t *testing.T) {
	r := newRegistry(t)
	r.importLang("english", r.incoming(spec{upstreamID: 1, length: 40, group: quote.LenShort}))
	id := r.storedQuotes()[0].ID

	first, changed := r.withdraw(id, "offensive")
	require.True(t, changed)

	second, changedAgain := r.withdraw(id, "a different reason entirely")
	assert.False(t, changedAgain, "nothing moved")
	assert.Equal(t, first.At, second.At, "the original timestamp stands")
	assert.Equal(t, "offensive", second.Reason, "and so does the original reason")
}

// Restoring is idempotent in the same way, and puts the quote back where a
// player can meet it.
func TestRestorePutsAQuoteBack(t *testing.T) {
	r := newRegistry(t)
	r.importLang("english", r.incoming(spec{upstreamID: 1, length: 40, group: quote.LenShort}))
	id := r.storedQuotes()[0].ID

	r.withdraw(id, "mistaken")
	changed, err := r.store.Restore(context.Background(), id)
	require.NoError(t, err)
	assert.True(t, changed)

	page, err := r.store.Page(context.Background(), quote.Filter{Lang: "english"}, nil, 50)
	require.NoError(t, err)
	assert.Len(t, page, 1, "back in circulation")

	// A quote that was never withdrawn is not an error to restore: the caller
	// asked for a state that already holds.
	changed, err = r.store.Restore(context.Background(), id)
	require.NoError(t, err)
	assert.False(t, changed)
}

// WithdrawnIDs is what the board index reads. It must be exactly the withdrawn
// set — empty when nothing is, which is the normal case it has to be cheap in.
func TestWithdrawnIDsReportsTheWithdrawnSet(t *testing.T) {
	r := newRegistry(t)
	r.importLang("english", r.incoming(
		spec{upstreamID: 1, length: 40, group: quote.LenShort},
		spec{upstreamID: 2, length: 40, group: quote.LenShort},
	))
	ids, err := r.store.WithdrawnIDs(context.Background())
	require.NoError(t, err)
	assert.Empty(t, ids)

	victim := r.storedQuotes()[0].ID
	r.withdraw(victim, "offensive")

	ids, err = r.store.WithdrawnIDs(context.Background())
	require.NoError(t, err)
	require.Len(t, ids, 1)
	_, ok := ids[victim]
	assert.True(t, ok)
}

// The withdrawal record is all-or-nothing: a timestamp without a reason is a
// decision nobody can review, and the database refuses it.
func TestSchemaRefusesAWithdrawalWithoutAReason(t *testing.T) {
	r := newRegistry(t)
	r.importLang("english", r.incoming(spec{upstreamID: 1, length: 40, group: quote.LenShort}))
	id := r.storedQuotes()[0].ID

	_, err := r.pool.Exec(context.Background(),
		`UPDATE quotes SET withdrawn_at = now() WHERE id = $1`, id)
	assert.Error(t, err, "withdrawn_at and withdrawn_reason move together")
}
