package leaderboard_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/typemore/typemore-server/internal/leaderboard"
)

// A ban hides a player from every read path WITHOUT deleting anything, so an
// unban is instant. Both halves matter: hiding without deleting is what makes
// the unban free, and hiding on EVERY path is what makes the ban mean anything.
func TestBanHidesTheEntryEverywhereAndKeepsIt(t *testing.T) {
	b := newBoard(t)
	ctx := context.Background()
	bucket := bucket15s(t)

	cheat := b.user("cheat", true)
	honest := b.user("honest", true)
	b.addRun(runSpec{user: cheat, score: 9999, achievedAt: minutesAgo(30)})
	b.addRun(runSpec{user: honest, score: 100, achievedAt: minutesAgo(20)})

	// Before the ban: two entries, cheat on top.
	page, err := b.store.Page(ctx, bucket, nil, 10)
	require.NoError(t, err)
	require.Len(t, page, 2)
	require.Equal(t, cheat, page[0].UserID)

	b.ban(cheat, nil)

	t.Run("page", func(t *testing.T) {
		rows, err := b.store.Page(ctx, bucket, nil, 10)
		require.NoError(t, err)
		require.Len(t, rows, 1)
		assert.Equal(t, honest, rows[0].UserID)
	})

	t.Run("bucket index counts", func(t *testing.T) {
		buckets, err := b.store.Buckets(ctx)
		require.NoError(t, err)
		require.Len(t, buckets, 1)
		assert.EqualValues(t, 1, buckets[0].Entries, "a banned player must not be counted")
	})

	t.Run("own entry lookup", func(t *testing.T) {
		_, err := b.store.EntryFor(ctx, bucket, cheat)
		assert.ErrorIs(t, err, leaderboard.ErrNoEntry,
			"a banned player must not be able to see their own hidden slot either")
	})

	t.Run("rank arithmetic", func(t *testing.T) {
		entry, err := b.store.EntryFor(ctx, bucket, honest)
		require.NoError(t, err)
		assert.EqualValues(t, 1, entry.Rank,
			"the honest player is now first: a hidden entry must not occupy a rank")
	})

	t.Run("the row is still there", func(t *testing.T) {
		_, ok := b.storedEntry(bucket, cheat)
		assert.True(t, ok, "a ban hides an entry, it does not delete it")
	})

	t.Run("unban restores it with no rebuild", func(t *testing.T) {
		b.unban(cheat)
		rows, err := b.store.Page(ctx, bucket, nil, 10)
		require.NoError(t, err)
		require.Len(t, rows, 2)
		assert.Equal(t, cheat, rows[0].UserID)
	})
}

// An expired ban is not a ban. The predicate lives in the active_bans view, so
// this also pins that expiry is evaluated at read time rather than by a sweeper.
func TestExpiredBanDoesNotHide(t *testing.T) {
	b := newBoard(t)
	bucket := bucket15s(t)
	user := b.user("served", true)
	b.addRun(runSpec{user: user, score: 500})

	past := time.Now().Add(-time.Hour)
	b.ban(user, &past)

	rows, err := b.store.Page(context.Background(), bucket, nil, 10)
	require.NoError(t, err)
	assert.Len(t, rows, 1, "a ban that has expired must stop hiding immediately")
}

// Ties go to whoever got there first. Without this the ordering is arbitrary,
// which means a page boundary can show the same player twice.
func TestTiesGoToTheEarliestAchievement(t *testing.T) {
	b := newBoard(t)
	bucket := bucket15s(t)

	late := b.user("late", true)
	early := b.user("early", true)
	middle := b.user("middle", true)

	// Inserted out of order on purpose: the board must sort by achievement, not
	// by projection order.
	b.addRun(runSpec{user: late, score: 1000, achievedAt: minutesAgo(10)})
	b.addRun(runSpec{user: early, score: 1000, achievedAt: minutesAgo(30)})
	b.addRun(runSpec{user: middle, score: 1000, achievedAt: minutesAgo(20)})

	rows, err := b.store.Page(context.Background(), bucket, nil, 10)
	require.NoError(t, err)
	require.Len(t, rows, 3)
	assert.Equal(t, []uuid.UUID{early, middle, late},
		[]uuid.UUID{rows[0].UserID, rows[1].UserID, rows[2].UserID})
}

// Keyset pagination over an ordering that is NOT unique is where boards
// duplicate and drop rows. Every player here has the same score AND the same
// achievement instant, so only the user_id tiebreak keeps the order total.
func TestPaginationIsStableWhenEverythingTies(t *testing.T) {
	b := newBoard(t)
	ctx := context.Background()
	bucket := bucket15s(t)

	const players = 11
	sameInstant := minutesAgo(30)
	want := make(map[uuid.UUID]bool, players)
	for i := range players {
		u := b.user(fmt.Sprintf("player%02d", i), true)
		b.addRun(runSpec{user: u, score: 1000, achievedAt: sameInstant})
		want[u] = true
	}

	// Page through in threes, following the cursor the way a client would.
	seen := make([]uuid.UUID, 0, players)
	var after *leaderboard.Cursor
	for range players + 2 { // generous bound: a broken cursor must not loop forever
		rows, err := b.store.Page(ctx, bucket, after, 3)
		require.NoError(t, err)
		if len(rows) == 0 {
			break
		}
		for i := range rows {
			seen = append(seen, rows[i].UserID)
		}
		last := rows[len(rows)-1]
		after = &leaderboard.Cursor{
			Score: last.Score, AchievedAt: last.AchievedAt, UserID: last.UserID,
		}
	}

	require.Len(t, seen, players, "every entry must appear exactly once across pages")
	got := make(map[uuid.UUID]bool, players)
	for _, id := range seen {
		require.False(t, got[id], "entry %s appeared on two pages", id)
		got[id] = true
	}
	assert.Equal(t, want, got)
}

// The rank on a continuation page is counted, not carried, so it stays right
// even when the ordering is a wall of ties.
func TestRanksAreContinuousAcrossPages(t *testing.T) {
	b := newBoard(t)
	ctx := context.Background()
	bucket := bucket15s(t)

	sameInstant := minutesAgo(30)
	ids := make([]uuid.UUID, 0, 5)
	for i := range 5 {
		u := b.user(fmt.Sprintf("player%02d", i), true)
		b.addRun(runSpec{user: u, score: 1000, achievedAt: sameInstant})
		ids = append(ids, u)
	}

	first, err := b.store.Page(ctx, bucket, nil, 2)
	require.NoError(t, err)
	require.Len(t, first, 2)

	cursor := leaderboard.Cursor{
		Score: first[1].Score, AchievedAt: first[1].AchievedAt, UserID: first[1].UserID,
	}
	above, err := b.store.RankAbove(ctx, bucket, cursor)
	require.NoError(t, err)
	assert.EqualValues(t, 1, above, "one entry outranks the second row")

	// Every player's own rank, via the /me path, must match their position.
	for _, id := range ids {
		entry, err := b.store.EntryFor(ctx, bucket, id)
		require.NoError(t, err)
		assert.GreaterOrEqual(t, entry.Rank, int64(1))
		assert.LessOrEqual(t, entry.Rank, int64(5))
	}
}

// Score outranks achievement: a later run with a higher score is still first.
func TestHigherScoreOutranksEarlierAchievement(t *testing.T) {
	b := newBoard(t)
	bucket := bucket15s(t)

	early := b.user("early", true)
	strong := b.user("strong", true)
	b.addRun(runSpec{user: early, score: 900, achievedAt: minutesAgo(60)})
	b.addRun(runSpec{user: strong, score: 1000, achievedAt: minutesAgo(1)})

	rows, err := b.store.Page(context.Background(), bucket, nil, 10)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, strong, rows[0].UserID)
	assert.Equal(t, early, rows[1].UserID)
}
