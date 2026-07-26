package leaderboard_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/typemore/typemore-server/internal/leaderboard"
)

// Per-quote boards (SCORING_CONCEPT §6). A quote is a fixed map: everyone types
// the same bytes, so a quote score means something next to other scores on that
// quote and nothing at all next to a seeded one.
//
// The policy is one sentence — RANKED WITHIN THE QUOTE, UNRANKED GLOBALLY — and
// the tests below are its two halves plus the reasons it is not merely a
// preference: a finite corpus is memorisable, quotes run 8 to 100+ words so they
// do not fit the ranked sizes, and cherry-picking easy quotes would turn a
// global rating into a search for easy maps.

// A quote run lands on its own board, ranked, with the quote's attribution on
// the row.
func TestQuoteRunLandsOnItsQuoteBoard(t *testing.T) {
	b := newBoard(t)
	ctx := context.Background()

	quoteID := b.quote("english", "the quick brown fox", "Aesop")
	board := quoteBucket(t, quoteID)

	fastest := b.user("fastest", true)
	slower := b.user("slower", true)
	topRun := b.addRun(runSpec{user: fastest, quote: quoteID, score: 2000, achievedAt: minutesAgo(30)})
	b.addRun(runSpec{user: slower, quote: quoteID, score: 900, achievedAt: minutesAgo(20)})

	rows, err := b.store.Page(ctx, board, nil, 10)
	require.NoError(t, err)
	require.Len(t, rows, 2)

	assert.Equal(t, fastest, rows[0].UserID)
	assert.Equal(t, topRun, rows[0].RunID)
	assert.EqualValues(t, 2000, rows[0].Score)
	assert.Equal(t, slower, rows[1].UserID)

	t.Run("the row carries the quote's source", func(t *testing.T) {
		assert.Equal(t, "Aesop", rows[0].Source,
			"a quote is someone's words; the attribution is not optional")
		assert.Equal(t, "Aesop", rows[1].Source)
	})

	t.Run("rank comes back through the /me path too", func(t *testing.T) {
		entry, err := b.store.EntryFor(ctx, board, slower)
		require.NoError(t, err)
		assert.EqualValues(t, 2, entry.Rank)
		assert.Equal(t, "Aesop", entry.Source)
	})
}

// The hard rule, asserted over the WHOLE catalogue rather than over the one
// language board the run would most plausibly have landed in. "It is not in
// time:15000:en:seeded" would pass just as well if the run had quietly filed
// itself under words:50 — this asserts there is no such row anywhere.
func TestQuoteRunIsRankedNowhereButItsQuote(t *testing.T) {
	b := newBoard(t)
	ctx := context.Background()

	quoteID := b.quote("english", "a finite corpus is memorisable", "Anonymous")
	user := b.user("racer", true)
	b.addRun(runSpec{user: user, quote: quoteID, score: 5000})

	// Every row in the table, not every row in one bucket.
	entries := b.storedEntries()
	require.Len(t, entries, 1, "one run, one slot")
	assert.Equal(t, "quote:"+quoteID.String(), entries[0].BucketKey)

	// And the same statement through the public catalogue, which is what a
	// client would page.
	buckets, err := b.store.Buckets(ctx)
	require.NoError(t, err)
	require.Len(t, buckets, 1)
	assert.True(t, buckets[0].Bucket.IsQuote())
	assert.Equal(t, quoteID, buckets[0].Bucket.QuoteID)

	t.Run("a seeded run beside it still ranks normally", func(t *testing.T) {
		// The exclusion has to be about the QUOTE, not about the player: the
		// same account's seeded run must be entirely unaffected.
		b.addRun(runSpec{user: user, score: 100})

		keys := make([]string, 0, 2)
		for _, e := range b.storedEntries() {
			keys = append(keys, e.BucketKey)
		}
		assert.ElementsMatch(t, []string{"quote:" + quoteID.String(), "time:15000:en:seeded"}, keys)
	})
}

// The other direction, and it is not symmetric by accident: a quote board must
// not fill up with seeded runs that happen to belong to the same player.
func TestSeededRunIsRankedInNoQuoteBoard(t *testing.T) {
	b := newBoard(t)

	quoteID := b.quote("english", "seeded text is effectively infinite", "Anonymous")
	user := b.user("racer", true)

	b.addRun(runSpec{user: user, score: 4000})
	b.addRun(runSpec{user: user, score: 3000, mode: leaderboard.ModeWords, wordCount: new(int32(50))})

	_, ok := b.storedEntry(quoteBucket(t, quoteID), user)
	assert.False(t, ok, "a seeded run has no quote and must not hold a quote slot")

	for _, e := range b.storedEntries() {
		assert.NotEqual(t, "quote:"+quoteID.String(), e.BucketKey)
	}
}

// Two quotes are two boards, and a player's run on one must not shadow the
// other. This is the "finite corpus" property in practice: boards multiply with
// the corpus rather than merging into one.
func TestEachQuoteIsItsOwnBoard(t *testing.T) {
	b := newBoard(t)

	first := b.quote("english", "one text", "Author One")
	second := b.quote("english", "another text", "Author Two")
	user := b.user("racer", true)

	b.addRun(runSpec{user: user, quote: first, score: 1000})
	b.addRun(runSpec{user: user, quote: second, score: 2000})

	entries := b.storedEntries()
	keys := make([]string, len(entries))
	sources := make([]string, len(entries))
	for i := range entries {
		keys[i] = entries[i].BucketKey
		sources[i] = entries[i].QuoteSource
	}
	assert.ElementsMatch(t, []string{"quote:" + first.String(), "quote:" + second.String()}, keys)
	assert.ElementsMatch(t, []string{"Author One", "Author Two"}, sources,
		"each board must carry ITS quote's attribution, not whichever was projected last")
}

// A quote board is a board: best-run-wins, demotion, re-promotion. The
// projection is one statement for both shapes, so this is really asserting that
// the quote cell reaches that statement at all.
func TestQuoteBoardKeepsOnlyThePlayersBest(t *testing.T) {
	b := newBoard(t)

	quoteID := b.quote("english", "muscle memory farms a short text", "Anonymous")
	board := quoteBucket(t, quoteID)
	user := b.user("racer", true)

	best := b.addRun(runSpec{user: user, quote: quoteID, score: 1500, achievedAt: minutesAgo(30)})
	b.addRun(runSpec{user: user, quote: quoteID, score: 900, achievedAt: minutesAgo(20)})

	entry, ok := b.storedEntry(board, user)
	require.True(t, ok)
	assert.Equal(t, best, entry.RunID)
	assert.Len(t, b.storedEntries(), 1, "one slot per player per quote")

	t.Run("demoting the best run promotes the next one", func(t *testing.T) {
		b.judge(best, "flagged")
		entry, ok := b.storedEntry(board, user)
		require.True(t, ok, "the next best run takes the slot")
		assert.EqualValues(t, 900, entry.Score)
		assert.Equal(t, "Anonymous", entry.QuoteSource,
			"the promoted row must carry the attribution too")
	})
}

// Ordering and paging are the same rule as everywhere else — score DESC,
// achieved_at ASC — and the rank on a continuation page is COUNTED, not carried
// in the cursor. A rank baked into a token was true when the token was minted.
func TestQuoteBoardOrderingAndPaging(t *testing.T) {
	b := newBoard(t)
	ctx := context.Background()

	quoteID := b.quote("english", "everyone types the same bytes", "Anonymous")
	board := quoteBucket(t, quoteID)

	// Same score for everyone, so only the achievement time separates them, and
	// planted out of order so the board cannot be sorting by projection order.
	type player struct {
		id      uuid.UUID
		minutes int
	}
	players := []player{{minutes: 10}, {minutes: 40}, {minutes: 20}, {minutes: 50}, {minutes: 30}}
	for i := range players {
		players[i].id = b.user(fmt.Sprintf("player%02d", i), true)
		b.addRun(runSpec{
			user: players[i].id, quote: quoteID, score: 1000,
			achievedAt: minutesAgo(players[i].minutes),
		})
	}

	first, err := b.store.Page(ctx, board, nil, 2)
	require.NoError(t, err)
	require.Len(t, first, 2)
	assert.Equal(t, players[3].id, first[0].UserID, "earliest achievement wins the tie")
	assert.Equal(t, players[1].id, first[1].UserID)

	cursor := leaderboard.Cursor{
		Score: first[1].Score, AchievedAt: first[1].AchievedAt, UserID: first[1].UserID,
	}
	above, err := b.store.RankAbove(ctx, board, cursor)
	require.NoError(t, err)
	assert.EqualValues(t, 1, above, "the rank of the next page's first row is counted, not carried")

	next, err := b.store.Page(ctx, board, &cursor, 2)
	require.NoError(t, err)
	require.Len(t, next, 2)
	assert.Equal(t, players[4].id, next[0].UserID)
	assert.Equal(t, players[2].id, next[1].UserID)
}

// The rebuild is what proves a board is derived from Postgres and not a second
// source of truth, and a quote board has to satisfy it too — including the part
// that can actually be wrong, the cell ENUMERATION: a quote cell is enumerated
// from coordinates the per-verdict path never sees.
func TestRebuildReproducesQuoteBoards(t *testing.T) {
	b := newBoard(t)
	ctx := context.Background()

	quotes := []uuid.UUID{
		b.quote("english", "first quote text", "Author One"),
		b.quote("russian", "second quote text", "Author Two"),
	}
	users := []uuid.UUID{b.user("alpha", true), b.user("beta", true), b.user("gamma", false)}

	for i, u := range users {
		for j, q := range quotes {
			b.addRun(runSpec{user: u, quote: q, score: int64(1000 + 100*i + j), achievedAt: minutesAgo(60 - i*5 - j)})
		}
		// Seeded runs in the mix, so the rebuild has to keep both kinds of cell
		// apart rather than merely reproducing a uniform table.
		b.addRun(runSpec{user: u, score: int64(500 + i), achievedAt: minutesAgo(10 - i)})
	}

	incremental := b.storedEntries()
	require.Len(t, incremental, len(users)*(len(quotes)+1))

	stats, err := b.store.Rebuild(ctx)
	require.NoError(t, err)

	assert.Equal(t, incremental, b.storedEntries(),
		"a rebuild must reproduce the quote boards exactly, attribution included")
	assert.Equal(t, stats.Before, stats.After,
		"a healthy rebuild reports UNCHANGED: that is the proof the board is derived from Postgres alone")
	assert.Equal(t, len(incremental), stats.Cells,
		"one enumerated cell per board, even though a quote run also carries a mode and a language")
	assert.EqualValues(t, len(incremental), stats.After)
}

// Bans and the verified-email gate live outside leaderboard_eligible_runs — one
// at read time, one as a per-query policy — so a new board shape is exactly the
// place they could quietly stop applying.
func TestQuoteBoardsHonourBansAndTheEmailGate(t *testing.T) {
	b := newBoard(t, func(o *boardOpts) { o.requireVerifiedEmail = true })
	ctx := context.Background()

	quoteID := b.quote("english", "a board must not leak who is banned", "Anonymous")
	board := quoteBucket(t, quoteID)

	cheat := b.user("cheat", true)
	drifter := b.user("drifter", false)
	honest := b.user("honest", true)

	b.addRun(runSpec{user: cheat, quote: quoteID, score: 9999, achievedAt: minutesAgo(30)})
	b.addRun(runSpec{user: drifter, quote: quoteID, score: 8888, achievedAt: minutesAgo(25)})
	b.addRun(runSpec{user: honest, quote: quoteID, score: 100, achievedAt: minutesAgo(20)})

	t.Run("an unverified account never takes a quote slot", func(t *testing.T) {
		_, ok := b.storedEntry(board, drifter)
		assert.False(t, ok)
	})

	b.ban(cheat, nil)

	rows, err := b.store.Page(ctx, board, nil, 10)
	require.NoError(t, err)
	require.Len(t, rows, 1, "a banned player must be hidden on a quote board too")
	assert.Equal(t, honest, rows[0].UserID)

	_, err = b.store.EntryFor(ctx, board, cheat)
	assert.ErrorIs(t, err, leaderboard.ErrNoEntry)

	_, kept := b.storedEntry(board, cheat)
	assert.True(t, kept, "a ban hides an entry, it does not delete it")
}

// The HTTP surface: a quote board is reachable by its key, renders its quote id
// instead of a mode it does not have, and answers 404 for a key that names no
// board.
func TestQuoteBoardOverHTTP(t *testing.T) {
	b := newBoard(t)

	quoteID := b.quote("english", "the osu beatmap analogue", "Marcus Aurelius")
	user := b.user("racer", true)
	runID := b.addRun(runSpec{user: user, quote: quoteID, score: 1234})

	t.Run("the page", func(t *testing.T) {
		resp := b.get("/api/v1/leaderboards/quote:" + quoteID.String())
		require.Equal(t, http.StatusOK, resp.StatusCode)
		body := decodeInto[pageBody](t, resp)

		assert.Equal(t, "quote:"+quoteID.String(), body.Bucket)
		require.Len(t, body.Entries, 1)
		assert.EqualValues(t, 1, body.Entries[0].Rank)
		assert.Equal(t, runID, body.Entries[0].RunID)
		assert.Equal(t, "Marcus Aurelius", body.Entries[0].Source)
	})

	t.Run("the index labels it by quote, not by mode", func(t *testing.T) {
		resp := b.get("/api/v1/leaderboards")
		require.Equal(t, http.StatusOK, resp.StatusCode)
		body := decodeInto[bucketsBody](t, resp)
		require.Len(t, body.Buckets, 1)

		got := body.Buckets[0]
		assert.Equal(t, "quote:"+quoteID.String(), got.Bucket)
		require.NotNil(t, got.QuoteID)
		assert.Equal(t, quoteID, *got.QuoteID)
		assert.Empty(t, got.Mode, "a quote board has no mode dimension")
		assert.Empty(t, got.Lang, "a quote board has no language dimension")
		assert.Empty(t, got.TextSource, "a quote board is not a text-source flavour")
		assert.Nil(t, got.DurationMs)
		assert.Nil(t, got.WordCount)
		assert.EqualValues(t, 1, got.Entries)
	})

	t.Run("a seeded row carries no source", func(t *testing.T) {
		b.addRun(runSpec{user: user, score: 10})
		resp := b.get("/api/v1/leaderboards/time:15000:en:seeded")
		require.Equal(t, http.StatusOK, resp.StatusCode)
		body := decodeInto[pageBody](t, resp)
		require.Len(t, body.Entries, 1)
		assert.Empty(t, body.Entries[0].Source,
			"there is no quote to attribute, so the field is absent rather than empty")
	})

	t.Run("an unparseable quote key is 404", func(t *testing.T) {
		for _, key := range []string{
			"quote:not-a-uuid",
			"quote:",
			"quote:1f5f1f2c6f0f4d5a9f0a3f2a1b0c9d8e",
			"words:50:en:quote",
		} {
			resp := b.get("/api/v1/leaderboards/" + key)
			assert.Equal(t, http.StatusNotFound, resp.StatusCode, "key %q", key)
		}
	})

	t.Run("a well-formed quote key nobody has played is an empty page", func(t *testing.T) {
		resp := b.get("/api/v1/leaderboards/quote:" + uuid.New().String())
		require.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Empty(t, decodeInto[pageBody](t, resp).Entries)
	})
}

// A run that claims a quote it cannot name is ranked NOWHERE — not on a quote
// board (there is no quote), and not on a language board either (its text did
// not come from the seed). Failing closed is the only safe direction: the
// alternative is a phantom board, or a quote score in the global ranking.
func TestRunNamingAnUnresolvableQuoteRanksNowhere(t *testing.T) {
	b := newBoard(t)
	user := b.user("racer", true)

	// Well-formed id, no such quote.
	b.addRun(runSpec{user: user, quote: uuid.New(), score: 1000})
	assert.Empty(t, b.storedEntries(), "a quote board on a quote that does not exist is not a board")

	// A quoteId that is not a uuid at all. run_quote_id must answer NULL rather
	// than reach the cast: this view is evaluated inside the replay worker's
	// verdict transaction, and `'q1'::uuid` would abort the batch that wrote it.
	const malformed = `{
	  "config":      {"mode":"quote","maxExtraChars":20,"difficulty":"normal","nospace":false,"minWpm":0},
	  "generation":  {"mode":"quote","length":0,"punctuation":false,"numbers":false,"randomCase":false,"reverse":false,
	                  "textSource":{"kind":"quote","quoteId":"q1","quoteHash":"deadbeef"}},
	  "declaration": {"blind":false,"fading":false,"flashlight":false}
	}`
	b.addRun(runSpec{user: user, quote: uuid.New(), setup: malformed, score: 2000})
	assert.Empty(t, b.storedEntries(), "an unparseable quote id must not reach a uuid cast")
}
