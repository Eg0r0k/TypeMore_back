package quote_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/typemore/typemore-server/internal/quote"
	"github.com/typemore/typemore-server/internal/quote/corpus"
)

// Running the import twice must be a no-op the second time — not "converges to
// the same state", but writes nothing at all. created_at is the witness: an
// upsert that rewrote every row would leave the counts identical and still have
// churned the table.
func TestImportIsIdempotent(t *testing.T) {
	r := newRegistry(t)
	rows := r.incoming(
		spec{upstreamID: 1, length: 40, group: quote.LenShort},
		spec{upstreamID: 2, length: 150, group: quote.LenMedium},
		spec{upstreamID: 3, length: 400, group: quote.LenLong},
	)

	first := r.importLang("german", rows)
	assert.Equal(t, quote.ImportStats{Inserted: 3}, first)
	before := r.storedQuotes()
	require.Len(t, before, 3)

	second := r.importLang("german", rows)
	assert.Equal(t, quote.ImportStats{Unchanged: 3}, second,
		"a second pass over unchanged corpora must report every quote unchanged")

	after := r.storedQuotes()
	require.Len(t, after, 3, "a re-import must not add rows")
	for i := range after {
		assert.Equal(t, before[i].ID, after[i].ID)
		assert.Equal(t, before[i].TextHash, after[i].TextHash)
		assert.False(t, after[i].Superseded)
		assert.True(t, before[i].CreatedAt.Equal(after[i].CreatedAt),
			"quote %d was rewritten: created_at moved from %s to %s",
			after[i].UpstreamID, before[i].CreatedAt, after[i].CreatedAt)
	}
}

// The supersede path. Published text is never edited in place, so a corrected
// upstream quote lands as a NEW row and the old one is retired — and BOTH stay
// resolvable, because runs played on the old bytes have to keep replaying.
func TestEditedTextIsPublishedBesideTheOldRevision(t *testing.T) {
	r := newRegistry(t)
	original := r.incoming(
		spec{upstreamID: 7, length: 40, group: quote.LenShort},
		spec{upstreamID: 8, length: 40, group: quote.LenShort},
	)
	require.Equal(t, quote.ImportStats{Inserted: 2}, r.importLang("german", original))

	old := r.storedQuotes()
	require.Len(t, old, 2)
	oldSeven := old[0]
	require.EqualValues(t, 7, oldSeven.UpstreamID)

	// Upstream fixed a typo in #7 and left #8 alone.
	edited := r.incoming(
		spec{upstreamID: 7, text: "a corrected quote, materially different bytes"},
		spec{upstreamID: 8, length: 40, group: quote.LenShort},
	)
	stats := r.importLang("german", edited)
	assert.Equal(t, quote.ImportStats{Superseded: 1, Unchanged: 1}, stats)

	rows := r.storedQuotes()
	require.Len(t, rows, 3, "the edit must ADD a row, never replace one")

	var retired, current storedQuote
	for _, row := range rows {
		if row.UpstreamID != 7 {
			continue
		}
		if row.Superseded {
			retired = row
		} else {
			current = row
		}
	}
	require.NotEqual(t, uuid.Nil, retired.ID, "the previous revision must still exist")
	require.NotEqual(t, uuid.Nil, current.ID, "the new revision must be published")

	assert.Equal(t, oldSeven.ID, retired.ID, "the retired row is the ORIGINAL row")
	assert.Equal(t, oldSeven.Text, retired.Text,
		"a retired quote's bytes must be untouched: every run played on it "+
			"regenerates its text from exactly these")
	assert.Equal(t, oldSeven.TextHash, retired.TextHash)
	assert.Equal(t, "a corrected quote, materially different bytes", current.Text)
	assert.NotEqual(t, retired.TextHash, current.TextHash)

	// Both are still fetchable by id. This is the whole point of the doctrine.
	for _, id := range []struct {
		id   string
		want bool
	}{{retired.ID.String(), true}, {current.ID.String(), false}} {
		resp := r.get("/api/v1/quotes/" + id.id)
		require.Equal(t, 200, resp.StatusCode, "quote %s must stay fetchable", id.id)
		body := decodeInto[quoteBody](t, resp)
		assert.Equal(t, id.want, body.Superseded)
		assert.NotEmpty(t, body.Text)
	}

	// A third pass over the edited corpus changes nothing again.
	assert.Equal(t, quote.ImportStats{Unchanged: 2}, r.importLang("german", edited))
	assert.Len(t, r.storedQuotes(), 3)
}

// The stored len_group must come from the corpus's OWN thresholds, all the way
// through the import and into Postgres. chinese is the witness: its 30/80/200
// boundaries put quotes in `medium` that a global 100/300/600 table would file
// as `short`, so this fails outright on an importer with one threshold table.
func TestImportedGroupsFollowEachCorpusThresholds(t *testing.T) {
	r := newRegistry(t)
	manifest, err := corpus.ReadManifest()
	require.NoError(t, err)

	var chinese corpus.Language
	for _, lang := range manifest.Languages {
		if lang.Lang == "chinese" {
			chinese = lang
		}
	}
	require.NotEmpty(t, chinese.File, "the manifest must still carry chinese")

	incoming, err := corpus.Load(r.core, chinese)
	require.NoError(t, err)
	stats := r.importLang(chinese.Lang, incoming)
	require.Equal(t, chinese.Quotes, stats.Inserted)

	// Every stored row agrees with the file's own thresholds...
	nonShort := 0
	for _, row := range r.storedQuotes() {
		want := -1
		for i, g := range chinese.Groups {
			if int(row.Length) >= g[0] && int(row.Length) <= g[1] {
				want = i
				break
			}
		}
		require.NotEqual(t, -1, want, "chinese #%d length %d fits no group", row.UpstreamID, row.Length)
		require.EqualValues(t, want, row.LenGroup,
			"chinese #%d (length %d) with thresholds %v", row.UpstreamID, row.Length, chinese.Groups)
		if row.LenGroup != int16(quote.LenShort) {
			nonShort++
		}
	}

	// ...and at least one of them is past chinese's own short boundary while
	// still inside the common table's. Without this the test would pass on an
	// importer that filed the whole corpus as `short`.
	require.Positive(t, nonShort,
		"chinese must contribute at least one non-short quote under its own "+
			"thresholds, or it cannot witness a global threshold table")
}

// `length` is what the API pages on and what the thresholds are expressed in.
// The schema enforces the tie with a CHECK; this asserts it over real imported
// data so a future importer change cannot quietly disable it (a CHECK dropped
// in a migration would let this fail loudly rather than never run).
func TestStoredLengthMatchesStoredText(t *testing.T) {
	r := newRegistry(t)
	manifest, err := corpus.ReadManifest()
	require.NoError(t, err)

	for _, lang := range manifest.Languages {
		if lang.Lang != "code_css" && lang.Lang != "chinese" {
			continue
		}
		incoming, err := corpus.Load(r.core, lang)
		require.NoError(t, err)
		r.importLang(lang.Lang, incoming)
	}

	var mismatched int
	require.NoError(t, r.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM quotes WHERE length <> char_length(text)`).Scan(&mismatched))
	assert.Zero(t, mismatched)

	rows := r.storedQuotes()
	require.NotEmpty(t, rows)
	for _, row := range rows {
		assert.EqualValues(t, len([]rune(row.Text)), row.Length, "%s #%d", row.Lang, row.UpstreamID)
	}
}

// text_hash must be the core's digest of the text, byte for byte. If anyone
// ever adds a Go FNV-1a — or a second goja binding — this is what catches the
// day the two disagree.
func TestTextHashIsTheCoresDigest(t *testing.T) {
	r := newRegistry(t)
	r.importLang("german", r.incoming(
		spec{upstreamID: 1, length: 40, group: quote.LenShort},
		spec{upstreamID: 2, length: 250, group: quote.LenMedium},
	))

	rows := r.storedQuotes()
	require.Len(t, rows, 2)
	for _, row := range rows {
		want, err := r.core.DictVersion([]string{row.Text})
		require.NoError(t, err)
		assert.Equal(t, want, row.TextHash,
			"the stored hash must be core.DictVersion([]string{text})")
	}
}

// Each language is published in its own transaction, so a corpus that fails
// halfway leaves the previous revision intact rather than a half-swapped one.
func TestAFailedLanguageLeavesTheCorpusAlone(t *testing.T) {
	r := newRegistry(t)
	require.Equal(t, quote.ImportStats{Inserted: 2}, r.importLang("german", r.incoming(
		spec{upstreamID: 1, length: 40, group: quote.LenShort},
		spec{upstreamID: 2, length: 40, group: quote.LenShort},
	)))

	// A group the schema refuses. The rows before it are already written inside
	// the transaction; the commit must never happen.
	broken := r.incoming(
		spec{upstreamID: 1, text: "an edit that would supersede the first quote"},
		spec{upstreamID: 3, length: 40, group: quote.LenShort},
	)
	broken[1].LenGroup = quote.LenGroup(9)

	_, err := r.store.Import(context.Background(), "german", broken)
	require.Error(t, err)

	rows := r.storedQuotes()
	assert.Len(t, rows, 2, "the failed pass must not have published anything")
	for _, row := range rows {
		assert.False(t, row.Superseded, "nothing may be retired by a pass that failed")
	}
}
