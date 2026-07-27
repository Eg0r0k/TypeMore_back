package leaderboard_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A ban hides entries; it never deletes them. That is the whole reason the
// filter is on the READ side, and it is what makes an unban instant.
//
// This walks the full cycle on one board and asserts the entry disappears and
// comes back — with the run untouched throughout, and with no rebuild anywhere
// in the test. If anything ever moves the filter to write time, the second half
// of this fails.
func TestBoardEntriesHideAndRestoreWithNoRebuild(t *testing.T) {
	b := newBoard(t)
	ctx := context.Background()
	bucket := bucket15s(t)

	banned := b.user("mallory", true)
	honest := b.user("ada", true)
	runID := b.addRun(runSpec{user: banned, score: 900})
	b.addRun(runSpec{user: honest, score: 100})

	visible := func() []string {
		rows, err := b.store.Page(ctx, bucket, nil, 50)
		require.NoError(t, err)
		names := make([]string, len(rows))
		for i := range rows {
			names[i] = rows[i].DisplayName
		}
		return names
	}

	require.Equal(t, []string{"mallory", "ada"}, visible())

	b.ban(banned, nil)
	assert.Equal(t, []string{"ada"}, visible(), "a banned player is still on the board")

	// The entry is still THERE. Nothing was projected away, which is why the
	// unban below needs no recomputation.
	entry, ok := b.storedEntry(bucket, banned)
	require.True(t, ok, "the ban deleted the entry instead of hiding it")
	assert.Equal(t, runID, entry.RunID)

	b.unban(banned)
	assert.Equal(t, []string{"mallory", "ada"}, visible(),
		"the unban did not restore the entry; something is filtering at write time")
}

// The same cycle, driven by EXPIRY rather than by a revocation, and with no
// janitor involved: the ban simply lapses and the entry reappears.
func TestBoardEntriesRestoreWhenATemporaryBanLapses(t *testing.T) {
	b := newBoard(t)
	ctx := context.Background()
	bucket := bucket15s(t)

	banned := b.user("mallory", true)
	b.addRun(runSpec{user: banned, score: 900})

	count := func() int {
		rows, err := b.store.Page(ctx, bucket, nil, 50)
		require.NoError(t, err)
		return len(rows)
	}
	require.Equal(t, 1, count())

	soon := time.Now().Add(1200 * time.Millisecond)
	b.ban(banned, &soon)
	require.Zero(t, count(), "a temporary ban is not hiding the entry")

	require.Eventually(t, func() bool { return count() == 1 }, 5*time.Second, 100*time.Millisecond,
		"the entry did not come back when the ban lapsed; expiry is not being evaluated at read time")
}

// A ban that was revoked stops hiding immediately, even though its row is still
// in the table. This is the case a `DELETE`-based unban would pass by accident
// and a `revoked_at`-based one has to get right.
func TestARevokedBanStopsHidingImmediately(t *testing.T) {
	b := newBoard(t)
	ctx := context.Background()
	bucket := bucket15s(t)

	banned := b.user("mallory", true)
	b.addRun(runSpec{user: banned, score: 900})

	b.ban(banned, nil)
	rows, err := b.store.Page(ctx, bucket, nil, 50)
	require.NoError(t, err)
	require.Empty(t, rows)

	_, err = b.pool.Exec(ctx, `UPDATE bans SET revoked_at = now() WHERE user_id = $1`, banned)
	require.NoError(t, err)

	rows, err = b.store.Page(ctx, bucket, nil, 50)
	require.NoError(t, err)
	require.Len(t, rows, 1, "a revoked ban is still hiding the entry")

	// And the row survived the revocation, so the history is intact.
	var kept int
	require.NoError(t, b.pool.QueryRow(ctx,
		`SELECT count(*) FROM bans WHERE user_id = $1`, banned).Scan(&kept))
	assert.Equal(t, 1, kept, "revoking deleted the ban instead of recording it")
}

// The board index is filtered by the same predicate: a bucket whose only player
// is banned reports zero, not one, and comes back when the ban lifts.
func TestBucketCountsFollowTheBanPredicate(t *testing.T) {
	b := newBoard(t)
	ctx := context.Background()

	banned := b.user("mallory", true)
	b.addRun(runSpec{user: banned, score: 900})

	buckets, err := b.store.Buckets(ctx)
	require.NoError(t, err)
	require.Len(t, buckets, 1)
	require.Equal(t, int64(1), buckets[0].Entries)

	b.ban(banned, nil)
	buckets, err = b.store.Buckets(ctx)
	require.NoError(t, err)
	assert.Empty(t, buckets, "a bucket whose only player is banned must not be listed")

	b.unban(banned)
	buckets, err = b.store.Buckets(ctx)
	require.NoError(t, err)
	require.Len(t, buckets, 1)
	assert.Equal(t, int64(1), buckets[0].Entries)
}
