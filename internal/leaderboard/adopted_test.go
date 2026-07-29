package leaderboard_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/typemore/typemore-server/internal/leaderboard"
)

// Seeded repeats (migration 00017).
//
// The rule is about where a run's TEXT came from and about nothing else. A run
// played with a pace caret or a ghost on screen is an ordinary run — it holds a
// board slot, a personal best and (when it exists) a rating point, and the
// outcome of the "race" changes none of that. A run that ADOPTED another run's
// setup is the exception, because its text was knowable before the first
// keystroke.
//
// These tests state that as the same run twice: identical in every column, told
// apart only by `setup.adoptedFromRunId`. If the exclusion ever drifted onto
// something else — a mode, a flag, the presence of an opponent — the pair below
// would stop disagreeing.

// The pair. Same player, same board, same score, same everything except the one
// field, and only one of them takes the slot.
func TestOnlyTheAdoptedTwinIsRankedNowhere(t *testing.T) {
	b := newBoard(t)
	board := bucket15s(t)
	user := b.user("racer", true)

	fresh := b.addRun(runSpec{user: user, score: 1000, achievedAt: minutesAgo(30)})

	entry, ok := b.storedEntry(board, user)
	require.True(t, ok, "a freshly generated run ranks")
	assert.Equal(t, fresh, entry.RunID)

	// The adopted twin scores HIGHER. If eligibility were being decided by score
	// alone it would take the slot; it must not even be a candidate.
	adopted := b.addRun(runSpec{
		user: user, score: 999_999, achievedAt: minutesAgo(20), adoptedFrom: fresh,
	})

	entry, ok = b.storedEntry(board, user)
	require.True(t, ok)
	assert.Equal(t, fresh, entry.RunID,
		"a seeded repeat must not displace the run it copied — nor any other")
	assert.EqualValues(t, 1000, entry.Score)

	t.Run("and it is ranked in no other board either", func(t *testing.T) {
		for _, e := range b.storedEntries() {
			assert.NotEqual(t, adopted, e.RunID,
				"a seeded repeat holds no slot anywhere, on any board shape")
		}
	})

	t.Run("the run itself is stored and accepted — saved is not counted", func(t *testing.T) {
		var status string
		require.NoError(t, b.pool.QueryRow(context.Background(),
			`SELECT status FROM runs WHERE id = $1`, adopted).Scan(&status))
		assert.Equal(t, "accepted", status,
			"a seeded repeat is judged and kept; it simply does not compete")
	})
}

// The same statement on a QUOTE board, because the two board shapes reach
// leaderboard_entries through one WHERE clause and a rule that held for only one
// of them would be a rule in the wrong place.
func TestAnAdoptedQuoteRunIsRankedNowhereEither(t *testing.T) {
	b := newBoard(t)
	quoteID := b.quote("english", "everyone types the same bytes", "Anonymous")
	board := quoteBucket(t, quoteID)
	user := b.user("racer", true)

	honest := b.addRun(runSpec{user: user, quote: quoteID, score: 1200, achievedAt: minutesAgo(30)})
	b.addRun(runSpec{
		user: user, quote: quoteID, score: 9000, achievedAt: minutesAgo(10), adoptedFrom: honest,
	})

	entry, ok := b.storedEntry(board, user)
	require.True(t, ok)
	assert.Equal(t, honest, entry.RunID)
	assert.Len(t, b.storedEntries(), 1, "one slot, held by the run that was not copied")
}

// Demotion's mirror image: if the ONLY eligible run is demoted, the seeded
// repeat beside it must not be promoted into the empty slot. "Ranked nowhere"
// has to survive the cell being recomputed for another reason.
func TestASeededRepeatIsNotPromotedWhenTheSlotEmpties(t *testing.T) {
	b := newBoard(t)
	board := bucket15s(t)
	user := b.user("racer", true)

	fresh := b.addRun(runSpec{user: user, score: 1000, achievedAt: minutesAgo(30)})
	b.addRun(runSpec{user: user, score: 5000, achievedAt: minutesAgo(20), adoptedFrom: fresh})

	b.judge(fresh, "flagged")

	_, ok := b.storedEntry(board, user)
	assert.False(t, ok, "the cell empties rather than falling back to the copy")
	assert.Empty(t, b.storedEntries())
}

// The rebuild is what proves a board is derived from Postgres rather than from
// the order things happened in. A seeded repeat is exactly the kind of row a
// rebuild could quietly readmit, because the rebuild enumerates coordinates
// instead of replaying the verdicts that produced them.
func TestRebuildDoesNotReadmitSeededRepeats(t *testing.T) {
	b := newBoard(t)
	ctx := context.Background()
	user := b.user("racer", true)

	fresh := b.addRun(runSpec{user: user, score: 1000, achievedAt: minutesAgo(30)})
	b.addRun(runSpec{user: user, score: 8000, achievedAt: minutesAgo(20), adoptedFrom: fresh})

	before := b.storedEntries()
	require.Len(t, before, 1)

	stats, err := b.store.Rebuild(ctx)
	require.NoError(t, err)
	assert.Equal(t, before, b.storedEntries())
	assert.Equal(t, stats.Before, stats.After, "a healthy rebuild reports UNCHANGED")
}

// A malformed marker must not reach the uuid cast: run_adopted_from is evaluated
// inside the replay worker's verdict transaction, so `'nope'::uuid` would abort
// the batch that wrote it. It resolves to NULL — the run counts, which is the
// same answer as "no marker at all" and the one that cannot lose data. Ingestion
// refuses the document up front (422 invalid_adopted_from), so this can only be
// reached by a row written around the API.
func TestAMalformedAdoptedMarkerDoesNotReachTheCast(t *testing.T) {
	b := newBoard(t)
	user := b.user("racer", true)

	const malformed = `{
	  "adoptedFromRunId": "not-a-uuid",
	  "config":      {"mode":"time","durationMs":15000,"maxExtraChars":20,"difficulty":"normal","nospace":false,"minWpm":0},
	  "generation":  {"mode":"time","length":0,"punctuation":false,"numbers":false,"randomCase":false,"reverse":false},
	  "declaration": {"blind":false,"fading":false,"flashlight":false}
	}`
	id := b.addRun(runSpec{user: user, setup: malformed, score: 1000})

	entry, ok := b.storedEntry(bucket15s(t), user)
	require.True(t, ok, "an unreadable marker means no marker, not a lost run")
	assert.Equal(t, id, entry.RunID)
}

// Per-quote boards and SUPERSEDE (docs/QUOTES.md, docs/LEADERBOARDS.md).
//
// A published quote is never edited. Correcting one inserts a NEW ROW with a NEW
// id and retires the old one (`quotes_revision_idx` is (lang, upstream_id,
// text_hash), and InsertQuoteRevision mints `gen_random_uuid()`), so (id, hash)
// is one-to-one for the life of the corpus.
//
// Two consequences, and the second is the one worth pinning:
//
//  1. A board keyed on the quote id is therefore ALREADY keyed per version. A
//     run that has been ranked stays ranked, against the exact bytes it was
//     played on, forever — retiring a revision touches neither its text nor its
//     board.
//  2. A correction does not RESET the old board, it FORKS: the new revision is a
//     different id, hence a different, empty board, and players who draw the
//     quote after the correction rank there. The two populations never merge.
func TestSupersedingAQuoteLeavesItsBoardAloneAndOpensANewOne(t *testing.T) {
	b := newBoard(t)
	ctx := context.Background()

	const lang = "english"
	original := b.quote(lang, "the qiuck brown fox", "Aesop")
	originalBoard := quoteBucket(t, original)
	user := b.user("racer", true)

	runID := b.addRun(runSpec{user: user, quote: original, score: 1500, achievedAt: minutesAgo(30)})
	entry, ok := b.storedEntry(originalBoard, user)
	require.True(t, ok)
	assert.Equal(t, runID, entry.RunID)

	// The typo is corrected the only way the registry allows: a new revision
	// beside the old one, and the old one retired.
	corrected := b.quote(lang, "the quick brown fox", "Aesop")
	_, err := b.pool.Exec(ctx, `UPDATE quotes SET superseded = true WHERE id = $1`, original)
	require.NoError(t, err)

	t.Run("the run keeps its slot on the board it was played on", func(t *testing.T) {
		entry, ok := b.storedEntry(originalBoard, user)
		require.True(t, ok, "retiring a revision must not evict the runs recorded against it")
		assert.Equal(t, runID, entry.RunID)

		rows, err := b.store.Page(ctx, originalBoard, nil, 10)
		require.NoError(t, err)
		require.Len(t, rows, 1, "the board is still readable")
		assert.Equal(t, runID, rows[0].RunID)
	})

	t.Run("and still resolves the bytes it was played on", func(t *testing.T) {
		var text string
		var superseded bool
		require.NoError(t, b.pool.QueryRow(ctx,
			`SELECT text, superseded FROM quotes WHERE id = $1`, original).Scan(&text, &superseded))
		assert.True(t, superseded)
		assert.Equal(t, "the qiuck brown fox", text,
			"a retired revision keeps its text: every run on it must replay forever")
	})

	t.Run("the correction is a DIFFERENT board, not the same one reset", func(t *testing.T) {
		assert.NotEqual(t, original, corrected,
			"a corrected quote is a new id — that is what makes the board per-version")

		rows, err := b.store.Page(ctx, quoteBucket(t, corrected), nil, 10)
		require.NoError(t, err)
		assert.Empty(t, rows, "nobody has played the corrected text yet")
	})

	t.Run("a rebuild reproduces the retired board exactly", func(t *testing.T) {
		before := b.storedEntries()
		stats, err := b.store.Rebuild(ctx)
		require.NoError(t, err)
		assert.Equal(t, before, b.storedEntries())
		assert.Equal(t, stats.Before, stats.After)
	})
}

// A run whose quote was retired is still ranked when it is judged LATER — the
// case a re-import creates: the corpus moves while a run sits pending. The
// projection resolves the id through `quotes`, and `superseded` is not part of
// that join, which is what this asserts rather than assumes.
func TestARunJudgedAfterItsQuoteWasRetiredStillRanks(t *testing.T) {
	b := newBoard(t)
	ctx := context.Background()

	quoteID := b.quote("english", "a corpus that moved under a pending run", "Anonymous")
	user := b.user("racer", true)

	runID := b.addRun(runSpec{user: user, quote: quoteID, score: 700, status: "pending"})
	_, err := b.pool.Exec(ctx, `UPDATE quotes SET superseded = true WHERE id = $1`, quoteID)
	require.NoError(t, err)

	b.judge(runID, "accepted")

	entry, ok := b.storedEntry(quoteBucket(t, quoteID), user)
	require.True(t, ok, "a retired revision is still a board")
	assert.Equal(t, runID, entry.RunID)
	assert.Equal(t, "Anonymous", entry.QuoteSource)
}

// Keyset pagination on a QUOTE bucket, driven through the SAME assertions the
// language-board pagination test makes. The task for this phase asks for the
// existing pagination test to be run against a quote bucket rather than trusted
// to generalise; ordering, the counted rank and the continuation are all
// bucket-shape agnostic only if the sort_key index treats a "quote:<uuid>" key
// exactly like a "time:15000:en:seeded" one.
func TestQuoteBucketPagesExactlyLikeALanguageBucket(t *testing.T) {
	b := newBoard(t)
	ctx := context.Background()

	quoteID := b.quote("english", "paging is not a property of the board shape", "Anonymous")
	board := quoteBucket(t, quoteID)

	// Distinct scores AND a colliding pair on the same score, so both the
	// sort_key descent and the achieved_at tiebreak inside one key are exercised.
	type planted struct {
		user  uuid.UUID
		score int64
	}
	rows := []planted{{score: 5000}, {score: 4000}, {score: 4000}, {score: 3000}, {score: 2000}}
	for i := range rows {
		rows[i].user = b.user(quoteUser(i), true)
		b.addRun(runSpec{
			user: rows[i].user, quote: quoteID, score: rows[i].score,
			achievedAt: minutesAgo(60 - i),
		})
	}

	first, err := b.store.Page(ctx, board, nil, 2)
	require.NoError(t, err)
	require.Len(t, first, 2)
	assert.Equal(t, rows[0].user, first[0].UserID)
	assert.Equal(t, rows[1].user, first[1].UserID, "the earlier of the tied pair comes first")

	cursor := leaderboard.Cursor{
		Score: first[1].Score, AchievedAt: first[1].AchievedAt, UserID: first[1].UserID,
	}
	above, err := b.store.RankAbove(ctx, board, cursor)
	require.NoError(t, err)
	assert.EqualValues(t, 1, above, "the rank is counted, not carried in the token")

	next, err := b.store.Page(ctx, board, &cursor, 2)
	require.NoError(t, err)
	require.Len(t, next, 2)
	assert.Equal(t, rows[2].user, next[0].UserID, "the tied twin resumes after the cursor, once")
	assert.Equal(t, rows[3].user, next[1].UserID)

	// Walking the whole board through the cursor must visit every row exactly
	// once — the property a filter-shaped continuation quietly breaks.
	seen := map[uuid.UUID]int{}
	var cur *leaderboard.Cursor
	for {
		page, err := b.store.Page(ctx, board, cur, 2)
		require.NoError(t, err)
		if len(page) == 0 {
			break
		}
		for _, row := range page {
			seen[row.UserID]++
		}
		last := page[len(page)-1]
		cur = &leaderboard.Cursor{Score: last.Score, AchievedAt: last.AchievedAt, UserID: last.UserID}
	}
	require.Len(t, seen, len(rows))
	for user, times := range seen {
		assert.Equal(t, 1, times, "user %s appeared %d times in one walk", user, times)
	}
}

func quoteUser(i int) string {
	return string(rune('a'+i)) + "-quote-pager"
}
