package leaderboard_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/typemore/typemore-server/internal/leaderboard"
)

// The catalogue's count and a board's rows are read through DIFFERENT views —
// ListLeaderboardBuckets counts leaderboard_ranked, the pages select
// leaderboard_rows, and CountLeaderboardAbove (which is where a rank comes from)
// counts leaderboard_ranked again. The split was a plan fix (00011): the count
// does not need the users join, so it does not pay for it.
//
// That leaves three relations that must agree about who is visible, and nothing
// checks that they do. TestBucketCountsFollowTheBanPredicate proves the count
// alone follows the ban filter, with one player; it cannot see a disagreement
// BETWEEN the count and the ranking, because with one player every wrong answer
// is also zero or one.
//
// The disagreement is not academic — it is the number the reader sees. The
// percentile on the pinned self row is rank ÷ entries, with the rank from the
// ranked view and the denominator from the count. If the two ever drift, the
// last player on a board is told they are Top 104%, and nobody on either side
// of the split is obviously at fault.
func assertCountsAgreeWithTheRanking(t *testing.T, b *board, bucket leaderboard.Bucket, when string) {
	t.Helper()
	ctx := context.Background()

	// Walk the whole board through the paging path, three rows at a time, so the
	// walk crosses page seams rather than reading one big page the cursor code
	// never touches.
	var walked []leaderboard.Entry
	var after *leaderboard.Cursor
	for {
		page, err := b.store.Page(ctx, bucket, after, 3)
		require.NoError(t, err, when)
		if len(page) == 0 {
			break
		}
		walked = append(walked, page...)
		last := page[len(page)-1]
		after = &leaderboard.Cursor{Score: last.Score, AchievedAt: last.AchievedAt, UserID: last.UserID}
		require.LessOrEqual(t, len(walked), 200, "%s: the walk is not terminating", when)
	}

	buckets, err := b.store.Buckets(ctx)
	require.NoError(t, err, when)

	var counted int64
	var listed bool
	for _, bc := range buckets {
		if bc.Bucket.Key() == bucket.Key() {
			counted, listed = bc.Entries, true
		}
	}
	if len(walked) == 0 {
		assert.False(t, listed, "%s: a board with no visible rows must not be listed at all", when)
		return
	}
	require.True(t, listed, "%s: a board with %d visible rows must be listed", when, len(walked))

	assert.EqualValues(t, len(walked), counted,
		"%s: the catalogue count must equal the number of rows the board actually pages out",
		when)

	// And the rank each row reports must be its position in that same walk — the
	// third relation. This is what makes the percentile's numerator and
	// denominator commensurable: the last row's rank has to BE the count, or
	// "Top 100%" is not the bottom of the board.
	for i := range walked {
		got, err := b.store.EntryFor(ctx, bucket, walked[i].UserID)
		require.NoError(t, err, when)
		assert.EqualValues(t, i+1, got.Rank,
			"%s: row %d of the walk reports rank %d", when, i+1, got.Rank)
	}
	last, err := b.store.EntryFor(ctx, bucket, walked[len(walked)-1].UserID)
	require.NoError(t, err, when)
	assert.EqualValues(t, counted, last.Rank,
		"%s: the last place's rank must equal the count, or the percentile cannot reach exactly 100%%",
		when)
}

// The count, the ranking and the ranks stay one number through every event that
// can change who is on a board: a new result, a ban, an unban, and a
// revalidation that takes a run's acceptance away.
func TestCatalogueCountAgreesWithTheRankingThroughEveryEvent(t *testing.T) {
	b := newBoard(t)
	bucket := bucket15s(t)

	// Six players, with deliberate score collisions so the ranking has ties to
	// order and the count has to agree about them too.
	type player struct {
		id  uuid.UUID
		run uuid.UUID
	}
	players := make([]player, 0, 6)
	scores := []int64{1500, 1200, 1200, 900, 900, 400}
	for i, score := range scores {
		id := b.user(string(rune('a'+i))+"player", true)
		run := b.addRun(runSpec{user: id, score: score, achievedAt: minutesAgo(60 - i)})
		players = append(players, player{id: id, run: run})
	}
	assertCountsAgreeWithTheRanking(t, b, bucket, "after the initial population")

	// A new personal best: the row moves up, the count does not move at all.
	b.addRun(runSpec{user: players[5].id, score: 1400, achievedAt: minutesAgo(5)})
	assertCountsAgreeWithTheRanking(t, b, bucket, "after a new personal best")

	// A brand-new player: one more row, one more count.
	newcomer := b.user("newcomer", true)
	b.addRun(runSpec{user: newcomer, score: 1300, achievedAt: minutesAgo(4)})
	assertCountsAgreeWithTheRanking(t, b, bucket, "after a newcomer")

	// A ban in the MIDDLE of the ranking: every rank below it shifts up by one
	// and the count drops by one. Both halves have to move together.
	b.ban(players[2].id, nil)
	assertCountsAgreeWithTheRanking(t, b, bucket, "with a mid-board player banned")

	// Banning rank 1 as well: the top of the board is the position most likely
	// to be special-cased somewhere.
	b.ban(players[0].id, nil)
	assertCountsAgreeWithTheRanking(t, b, bucket, "with rank 1 banned too")

	b.unban(players[0].id)
	b.unban(players[2].id)
	assertCountsAgreeWithTheRanking(t, b, bucket, "after both bans lift")

	// Revalidation takes an acceptance away. Unlike a ban this DELETES the row
	// (the player has no other run here), so the count and the ranking must both
	// forget it rather than one of them keeping a ghost.
	b.judge(players[1].run, "rejected")
	assertCountsAgreeWithTheRanking(t, b, bucket, "after a revalidation rejected a run")

	// And back: re-accepting restores the row on both sides without a rebuild.
	b.judge(players[1].run, "accepted")
	assertCountsAgreeWithTheRanking(t, b, bucket, "after the run was re-accepted")
}
