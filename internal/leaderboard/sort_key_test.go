package leaderboard_test

import (
	"context"
	"fmt"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/typemore/typemore-server/internal/leaderboard"
)

// The packing has to reproduce the ordering it replaced, exactly.
//
// sort_key = (score << 32) - epoch(achieved_at) claims to be
// (score DESC, achieved_at ASC) in one descending integer. That is an argument;
// this is the check. It plants entries whose scores and timestamps collide on
// purpose — the interesting cases are all ties — and asserts that ordering by
// (sort_key DESC, user_id ASC) is row-for-row the legacy
// (score DESC, achieved_at ASC, user_id ASC).
//
// It runs in SQL against the real column rather than against a Go
// reimplementation of the expression, because a Go copy of the packing that
// agreed with a Go copy of the ordering would prove nothing about the column
// the index is built on.
func TestSortKeyOrderingEqualsTheLegacyOrdering(t *testing.T) {
	b := newBoard(t)
	ctx := context.Background()
	bucket := bucket15s(t)

	// A deliberately cramped space: 12 scores over 40 timestamps for 400 rows
	// means many players share a score, and many share a score AND a second.
	// Ties are where a packed key can silently disagree with the spec.
	rng := rand.New(rand.NewPCG(20260727, 3))
	const rows = 400
	for i := range rows {
		u := b.user(fmt.Sprintf("p%03d", i), true)
		b.addRun(runSpec{
			user:       u,
			score:      int64(rng.IntN(12) + 1),
			achievedAt: minutesAgo(rng.IntN(40)),
		})
	}

	var disagreements int64
	require.NoError(t, b.pool.QueryRow(ctx, `
		WITH legacy AS (
		    SELECT user_id,
		           row_number() OVER (ORDER BY score DESC, achieved_at ASC, user_id ASC) AS n
		    FROM leaderboard_ranked WHERE bucket_key = $1),
		     packed AS (
		    SELECT user_id,
		           row_number() OVER (ORDER BY sort_key DESC, achieved_at ASC, user_id ASC) AS n
		    FROM leaderboard_ranked WHERE bucket_key = $1)
		SELECT count(*) FROM legacy JOIN packed USING (user_id) WHERE legacy.n <> packed.n`,
		bucket.Key()).Scan(&disagreements))
	assert.Zero(t, disagreements,
		"the packed ordering disagrees with (score DESC, achieved_at ASC, user_id ASC) on %d of %d rows",
		disagreements, rows)

	// And the reason achieved_at is in that ORDER BY, pinned.
	//
	// The packing floors the epoch to whole seconds, so it CANNOT order two
	// runs that share a score and a second — the schema's "ties are broken by
	// who got there first" is finer-grained than the key. Ordering by
	// (sort_key DESC, user_id) alone therefore silently re-ranks those players,
	// and this asserts it does, so that dropping achieved_at from
	// leaderboard_sort_idx fails here instead of quietly moving people.
	var withoutAchievedAt int64
	require.NoError(t, b.pool.QueryRow(ctx, `
		WITH legacy AS (
		    SELECT user_id,
		           row_number() OVER (ORDER BY score DESC, achieved_at ASC, user_id ASC) AS n
		    FROM leaderboard_ranked WHERE bucket_key = $1),
		     packed AS (
		    SELECT user_id,
		           row_number() OVER (ORDER BY sort_key DESC, user_id ASC) AS n
		    FROM leaderboard_ranked WHERE bucket_key = $1)
		SELECT count(*) FROM legacy JOIN packed USING (user_id) WHERE legacy.n <> packed.n`,
		bucket.Key()).Scan(&withoutAchievedAt))
	assert.Positivef(t, withoutAchievedAt,
		"this fixture has no same-second ties, so it cannot prove achieved_at is load-bearing")
	t.Logf("without achieved_at in the ordering, %d of %d rows would move", withoutAchievedAt, rows)

	// The premise: the fixture actually contains the ties that make this test
	// capable of failing. Without them it would be asserting over distinct
	// scores, where any monotone packing passes.
	var tiedScores, tiedSortKeys int64
	require.NoError(t, b.pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM (SELECT score FROM leaderboard_ranked WHERE bucket_key = $1
		                              GROUP BY score HAVING count(*) > 1) s),
		       (SELECT count(*) FROM (SELECT sort_key FROM leaderboard_ranked WHERE bucket_key = $1
		                              GROUP BY sort_key HAVING count(*) > 1) k)`,
		bucket.Key()).Scan(&tiedScores, &tiedSortKeys))
	require.Positive(t, tiedScores, "no two entries share a score: the tiebreak is untested")
	require.Positive(t, tiedSortKeys,
		"no two entries share a sort_key: the user_id tiebreak inside a tie is untested")
	t.Logf("%d rows, %d shared scores, %d shared sort_keys", rows, tiedScores, tiedSortKeys)
}

// Walking the board with real cursors must visit every row exactly once, in the
// ranking order — which is the property keyset pagination is FOR, and the one
// the old OR-of-conjunctions happened to satisfy while doing it in O(depth).
//
// The continuation is now two clauses (a start condition plus a tie filter),
// and getting the boundary wrong there loses or repeats exactly the rows that
// share a sort_key with a cursor. So this walks a board dense with such ties.
func TestKeysetWalkVisitsEveryRowExactlyOnce(t *testing.T) {
	b := newBoard(t)
	ctx := context.Background()
	bucket := bucket15s(t)

	rng := rand.New(rand.NewPCG(20260727, 11))
	const rows = 250
	for i := range rows {
		b.addRun(runSpec{
			user:       b.user(fmt.Sprintf("w%03d", i), true),
			score:      int64(rng.IntN(8) + 1),
			achievedAt: minutesAgo(rng.IntN(20)),
		})
	}

	// Ranking order, straight from the database, as the answer to compare to.
	want := make([]uuid.UUID, 0, rows)
	got, err := b.pool.Query(ctx, `
		SELECT user_id FROM leaderboard_ranked WHERE bucket_key = $1
		ORDER BY score DESC, achieved_at ASC, user_id ASC`, bucket.Key())
	require.NoError(t, err)
	for got.Next() {
		var id uuid.UUID
		require.NoError(t, got.Scan(&id))
		want = append(want, id)
	}
	require.NoError(t, got.Err())
	require.Len(t, want, rows)

	// The same board, walked the way a client walks it: 7 at a time so page
	// boundaries land inside tie groups rather than politely between them.
	var walked []uuid.UUID
	var cursor *leaderboard.Cursor
	for page := 0; ; page++ {
		require.Less(t, page, rows, "the walk did not terminate")
		entries, err := b.store.Page(ctx, bucket, cursor, 7)
		require.NoError(t, err)
		if len(entries) == 0 {
			break
		}
		for _, e := range entries {
			walked = append(walked, e.UserID)
		}
		last := entries[len(entries)-1]
		cursor = &leaderboard.Cursor{Score: last.Score, AchievedAt: last.AchievedAt, UserID: last.UserID}
	}

	assert.Equal(t, want, walked,
		"the keyset walk is not the ranking: %d rows walked against %d in the board",
		len(walked), len(want))
}

// RankAbove is the count on the other side of the same boundary, so it has to
// agree with the walk row for row: the entry at index i must rank i+1.
func TestRankAboveAgreesWithThePageOrder(t *testing.T) {
	b := newBoard(t)
	ctx := context.Background()
	bucket := bucket15s(t)

	rng := rand.New(rand.NewPCG(20260727, 5))
	const rows = 120
	for i := range rows {
		b.addRun(runSpec{
			user:       b.user(fmt.Sprintf("r%03d", i), true),
			score:      int64(rng.IntN(6) + 1),
			achievedAt: minutesAgo(rng.IntN(10)),
		})
	}

	entries, err := b.store.Page(ctx, bucket, nil, rows)
	require.NoError(t, err)
	require.Len(t, entries, rows)

	for i, e := range entries {
		above, err := b.store.RankAbove(ctx, bucket, leaderboard.Cursor{
			Score: e.Score, AchievedAt: e.AchievedAt, UserID: e.UserID,
		})
		require.NoError(t, err)
		require.Equalf(t, int64(i), above,
			"the row at position %d reports %d entries above it", i, above)

		full, err := b.store.EntryFor(ctx, bucket, e.UserID)
		require.NoError(t, err)
		assert.Equalf(t, int64(i+1), full.Rank, "/me rank for the row at position %d", i)
	}
}

// The packing is only sound while the score stays inside the low bits it was
// given. A score at 2^31 would shift into the sign bit and rank a player LAST
// instead of first — silently, on a board that still looks healthy. So the
// bound is a CHECK, and this is the proof it fires at write time rather than
// being a comment in a migration.
func TestAScoreTooLargeToPackIsRefusedAtWriteTime(t *testing.T) {
	b := newBoard(t)
	ctx := context.Background()

	const packCap = int64(1) << 24
	user := b.user("whale", true)

	// One under the cap is fine, and packs to something still comfortably
	// inside bigint.
	var packed int64
	require.NoError(t, b.pool.QueryRow(ctx,
		`SELECT leaderboard_sort_key($1::bigint, now())`, packCap-1).Scan(&packed))
	assert.Positive(t, packed, "a score just under the cap must not pack to a negative key")

	_, err := b.pool.Exec(ctx, `
		INSERT INTO leaderboard_entries
		    (bucket_key, user_id, run_id, score, wpm, raw, acc, grade, mods, achieved_at)
		VALUES ($1, $2, $3, $4, 100, 100, 1, 'S', '{}'::jsonb, now())`,
		bucket15s(t).Key(), user, uuid.New(), packCap)
	require.Error(t, err, "a score at the cap was accepted; the packing can overflow")
	assert.Contains(t, err.Error(), "leaderboard_entries_score_packable",
		"the refusal must name the constraint, so the failure explains itself")
}

// The generated column is not something a writer supplies, and it must not be
// possible to supply one: a hand-written sort_key that disagreed with its own
// row would put a player in the wrong place with no way to notice.
func TestSortKeyCannotBeWritten(t *testing.T) {
	b := newBoard(t)
	user := b.user("meddler", true)

	_, err := b.pool.Exec(context.Background(), `
		INSERT INTO leaderboard_entries
		    (bucket_key, user_id, run_id, score, wpm, raw, acc, grade, mods, achieved_at, sort_key)
		VALUES ($1, $2, $3, 10, 100, 100, 1, 'S', '{}'::jsonb, now(), 999)`,
		bucket15s(t).Key(), user, uuid.New())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sort_key")
}

// The packing must survive a timestamp far enough out to matter. The epoch
// occupies the low 32 bits, so it stays a pure suffix until 2106; before then a
// later run must simply sort below an earlier one at the same score.
func TestPackingHoldsForDistantTimestamps(t *testing.T) {
	b := newBoard(t)
	ctx := context.Background()

	for _, at := range []time.Time{
		time.Date(1970, 1, 1, 0, 0, 1, 0, time.UTC),
		time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC),
		time.Date(2105, 12, 31, 23, 59, 59, 0, time.UTC),
	} {
		var earlier, later int64
		require.NoError(t, b.pool.QueryRow(ctx,
			`SELECT leaderboard_sort_key(1000, $1::timestamptz),
			        leaderboard_sort_key(1000, $1::timestamptz + interval '1 second')`,
			at).Scan(&earlier, &later))
		assert.Greaterf(t, earlier, later,
			"at %s a later run does not sort below an earlier one at the same score", at)

		var higher int64
		require.NoError(t, b.pool.QueryRow(ctx,
			`SELECT leaderboard_sort_key(1001, $1::timestamptz)`, at).Scan(&higher))
		assert.Greaterf(t, higher, earlier,
			"at %s a higher score does not outrank a lower one", at)
	}
}
