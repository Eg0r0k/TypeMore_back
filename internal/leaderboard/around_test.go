package leaderboard_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The `?around=me` window and the `?before=` continuation — the two halves of
// "jump to my row, then keep scrolling in either direction".
//
// The board seeded here is deliberately DENSE and ORDERED: user i scores
// 1000*i, so rank r belongs to the player named after N-r+1 and every
// assertion about ranks can be read off the fixture. The seam walks assert
// set equality against the whole board, because "no duplicate, no missing"
// across window+continuation joins is the entire contract.

// aroundBody is pageBody plus the upward continuation token.
type aroundBody struct {
	Bucket  string `json:"bucket"`
	Entries []struct {
		Rank        int64     `json:"rank"`
		UserID      uuid.UUID `json:"userId"`
		DisplayName string    `json:"displayName"`
		Score       int64     `json:"score"`
	} `json:"entries"`
	PrevCursor string `json:"prevCursor"`
	NextCursor string `json:"nextCursor"`
}

// seedLadder plants n players scoring 1000, 2000, … n*1000 and returns their
// ids INDEXED BY RANK: ids[0] is rank 1 (the top score), ids[n-1] the last.
func seedLadder(b *board, n int) []uuid.UUID {
	ids := make([]uuid.UUID, n)
	for i := 1; i <= n; i++ {
		u := b.user(fmt.Sprintf("player-%d", i), true)
		b.addRun(runSpec{user: u, score: int64(1000 * i), achievedAt: minutesAgo(i)})
		ids[n-i] = u
	}
	return ids
}

func ranksOf(body aroundBody) []int64 {
	out := make([]int64, len(body.Entries))
	for i, e := range body.Entries {
		out[i] = e.Rank
	}
	return out
}

func TestAroundMeRequiresASession(t *testing.T) {
	b := newBoard(t)
	seedLadder(b, 3)

	resp := b.get("/api/v1/leaderboards/time:15000:en:seeded?around=me")
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestAroundMeWithNoSlotIsA204(t *testing.T) {
	b := newBoard(t)
	seedLadder(b, 3)
	// Signed in, never played this board — the same answer /me gives, because
	// it is the same question with neighbours attached.
	b.asUser = b.user("spectator", true)

	resp := b.get("/api/v1/leaderboards/time:15000:en:seeded?around=me")
	assert.Equal(t, http.StatusNoContent, resp.StatusCode)
}

func TestAroundMeCentresTheWindowOnTheCaller(t *testing.T) {
	b := newBoard(t)
	ids := seedLadder(b, 9)
	b.asUser = ids[4] // rank 5 of 9

	resp := b.get("/api/v1/leaderboards/time:15000:en:seeded?around=me&limit=5")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body := decodeInto[aroundBody](t, resp)

	assert.Equal(t, []int64{3, 4, 5, 6, 7}, ranksOf(body))
	assert.Equal(t, b.asUser, body.Entries[2].UserID, "the caller sits in the middle")
	assert.NotEmpty(t, body.PrevCursor, "rank 3 is not the top — rows continue above")
	assert.NotEmpty(t, body.NextCursor, "rank 7 is not the bottom — rows continue below")
}

func TestAroundMeAtTheTopSpendsTheWindowBelow(t *testing.T) {
	b := newBoard(t)
	ids := seedLadder(b, 9)
	b.asUser = ids[0] // rank 1

	resp := b.get("/api/v1/leaderboards/time:15000:en:seeded?around=me&limit=5")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body := decodeInto[aroundBody](t, resp)

	// Nothing exists above rank 1; the window is still 5 rows, all below.
	assert.Equal(t, []int64{1, 2, 3, 4, 5}, ranksOf(body))
	assert.Equal(t, b.asUser, body.Entries[0].UserID)
	assert.Empty(t, body.PrevCursor, "there is nothing above rank 1")
	assert.NotEmpty(t, body.NextCursor)
}

func TestAroundMeAtTheBottomSpendsTheWindowAbove(t *testing.T) {
	b := newBoard(t)
	ids := seedLadder(b, 9)
	b.asUser = ids[8] // rank 9, the last

	resp := b.get("/api/v1/leaderboards/time:15000:en:seeded?around=me&limit=5")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body := decodeInto[aroundBody](t, resp)

	// Nothing exists below the last rank; the spare capacity goes above.
	assert.Equal(t, []int64{5, 6, 7, 8, 9}, ranksOf(body))
	assert.Equal(t, b.asUser, body.Entries[4].UserID)
	assert.NotEmpty(t, body.PrevCursor)
	assert.Empty(t, body.NextCursor, "there is nothing below the last rank")
}

func TestAroundMeOnATinyBoardIsTheWholeBoard(t *testing.T) {
	b := newBoard(t)
	ids := seedLadder(b, 3)
	b.asUser = ids[1]

	resp := b.get("/api/v1/leaderboards/time:15000:en:seeded?around=me&limit=50")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body := decodeInto[aroundBody](t, resp)

	assert.Equal(t, []int64{1, 2, 3}, ranksOf(body))
	assert.Empty(t, body.PrevCursor)
	assert.Empty(t, body.NextCursor)
}

// The seam contract: the window plus its two continuations reproduce the whole
// board with no duplicate and no missing row. This is what the client's merged
// feed stands on.
func TestAroundWindowAndContinuationsTileTheBoard(t *testing.T) {
	b := newBoard(t)
	ids := seedLadder(b, 12)
	b.asUser = ids[6] // rank 7 of 12

	resp := b.get("/api/v1/leaderboards/time:15000:en:seeded?around=me&limit=4")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	window := decodeInto[aroundBody](t, resp)
	require.NotEmpty(t, window.PrevCursor)
	require.NotEmpty(t, window.NextCursor)

	seen := map[uuid.UUID]int64{}
	record := func(body aroundBody) {
		for _, e := range body.Entries {
			_, dup := seen[e.UserID]
			require.False(t, dup, "user %s served twice, at ranks %d and %d",
				e.DisplayName, seen[e.UserID], e.Rank)
			seen[e.UserID] = e.Rank
		}
	}
	record(window)

	// Walk UP from the window with ?before= until the top.
	prev := window.PrevCursor
	for prev != "" {
		resp := b.get("/api/v1/leaderboards/time:15000:en:seeded?before=" + prev + "&limit=2")
		require.Equal(t, http.StatusOK, resp.StatusCode)
		page := decodeInto[aroundBody](t, resp)
		require.NotEmpty(t, page.Entries, "a prevCursor must never lead to an empty page")
		record(page)
		prev = page.PrevCursor
	}

	// Walk DOWN from the window with ?cursor= until the bottom.
	next := window.NextCursor
	for next != "" {
		resp := b.get("/api/v1/leaderboards/time:15000:en:seeded?cursor=" + next + "&limit=3")
		require.Equal(t, http.StatusOK, resp.StatusCode)
		page := decodeInto[aroundBody](t, resp)
		record(page)
		next = page.NextCursor
	}

	// Every player exactly once, at exactly their rank.
	require.Len(t, seen, 12)
	for i, id := range ids {
		assert.EqualValues(t, i+1, seen[id], "player at seeded rank %d", i+1)
	}
}

// The same tiling when EVERY row ties on score and second: the user_id
// tiebreak is all that orders them, in both scan directions.
func TestAroundTilingSurvivesTotalTies(t *testing.T) {
	b := newBoard(t)
	at := minutesAgo(30)
	var users []uuid.UUID
	for i := 0; i < 7; i++ {
		u := b.user(fmt.Sprintf("tied-%d", i), true)
		b.addRun(runSpec{user: u, score: 5000, achievedAt: at})
		users = append(users, u)
	}
	b.asUser = users[3]

	resp := b.get("/api/v1/leaderboards/time:15000:en:seeded?around=me&limit=3")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	window := decodeInto[aroundBody](t, resp)
	require.Len(t, window.Entries, 3)

	seen := map[uuid.UUID]int64{}
	for _, e := range window.Entries {
		seen[e.UserID] = e.Rank
	}
	prev, next := window.PrevCursor, window.NextCursor
	for prev != "" {
		page := decodeInto[aroundBody](t,
			b.get("/api/v1/leaderboards/time:15000:en:seeded?before="+prev+"&limit=2"))
		for _, e := range page.Entries {
			_, dup := seen[e.UserID]
			require.False(t, dup, "duplicate row across the upward seam")
			seen[e.UserID] = e.Rank
		}
		prev = page.PrevCursor
	}
	for next != "" {
		page := decodeInto[aroundBody](t,
			b.get("/api/v1/leaderboards/time:15000:en:seeded?cursor="+next+"&limit=2"))
		for _, e := range page.Entries {
			_, dup := seen[e.UserID]
			require.False(t, dup, "duplicate row across the downward seam")
			seen[e.UserID] = e.Rank
		}
		next = page.NextCursor
	}

	require.Len(t, seen, 7, "every tied player exactly once")
	ranks := map[int64]bool{}
	for _, r := range seen {
		require.False(t, ranks[r], "two players share rank %d", r)
		ranks[r] = true
	}
}

func TestAroundBeforeAndCursorAreMutuallyExclusive(t *testing.T) {
	b := newBoard(t)
	ids := seedLadder(b, 3)
	b.asUser = ids[0]

	resp := b.get("/api/v1/leaderboards/time:15000:en:seeded?around=me&cursor=abc")
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)

	resp = b.get("/api/v1/leaderboards/time:15000:en:seeded?before=abc&cursor=abc")
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestAroundAnchorsOnlyMe(t *testing.T) {
	b := newBoard(t)
	ids := seedLadder(b, 3)
	b.asUser = ids[0]

	resp := b.get("/api/v1/leaderboards/time:15000:en:seeded?around=somebody")
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestBeforeARankOneCursorIsAnEmptyPage(t *testing.T) {
	b := newBoard(t)
	ids := seedLadder(b, 3)
	b.asUser = ids[0]

	window := decodeInto[aroundBody](t,
		b.get("/api/v1/leaderboards/time:15000:en:seeded?around=me&limit=1"))
	require.Len(t, window.Entries, 1)
	require.EqualValues(t, 1, window.Entries[0].Rank)

	// A client should never ask (no prevCursor was offered), but the answer to
	// "what outranks the rank-1 position" is an empty page, not an error.
	next := window.NextCursor
	require.NotEmpty(t, next, "a lone row above a longer board still continues down")
	page := decodeInto[aroundBody](t,
		b.get("/api/v1/leaderboards/time:15000:en:seeded?before="+next+"&limit=5"))
	require.Empty(t, page.Entries)
	require.Empty(t, page.PrevCursor)
}

// A banned player is invisible to the window exactly as to every other read:
// the ranks a caller sees around them close over the gap.
func TestAroundWindowHonoursBans(t *testing.T) {
	b := newBoard(t)
	ids := seedLadder(b, 5)
	b.asUser = ids[3]  // rank 4
	b.ban(ids[2], nil) // rank 3 vanishes

	resp := b.get("/api/v1/leaderboards/time:15000:en:seeded?around=me&limit=3")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body := decodeInto[aroundBody](t, resp)

	// With the rank-3 player hidden, the caller IS rank 3 of 4 visible.
	require.Len(t, body.Entries, 3)
	assert.Equal(t, b.asUser, body.Entries[1].UserID)
	assert.EqualValues(t, 3, body.Entries[1].Rank)
	for _, e := range body.Entries {
		assert.NotEqual(t, ids[2], e.UserID, "a banned player must not appear in a window")
	}
}
