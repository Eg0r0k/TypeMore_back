package leaderboard_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/typemore/typemore-server/internal/leaderboard"
)

type bucketsBody struct {
	Buckets []struct {
		Bucket     string `json:"bucket"`
		Mode       string `json:"mode"`
		DurationMs *int32 `json:"durationMs"`
		WordCount  *int32 `json:"wordCount"`
		Lang       string `json:"lang"`
		TextSource string `json:"textSource"`
		Entries    int64  `json:"entries"`
	} `json:"buckets"`
}

type pageBody struct {
	Bucket  string `json:"bucket"`
	Entries []struct {
		Rank        int64           `json:"rank"`
		UserID      uuid.UUID       `json:"userId"`
		DisplayName string          `json:"displayName"`
		Score       int64           `json:"score"`
		WPM         float64         `json:"wpm"`
		Raw         float64         `json:"raw"`
		Acc         float64         `json:"acc"`
		Grade       string          `json:"grade"`
		Mods        json.RawMessage `json:"mods"`
		RunID       uuid.UUID       `json:"runId"`
		AchievedAt  string          `json:"achievedAt"`
	} `json:"entries"`
	NextCursor string `json:"nextCursor"`
}

// The index is what a client renders as "pick a board". Each entry has to carry
// enough to label itself without the client re-deriving the key format.
func TestBucketsIndex(t *testing.T) {
	b := newBoard(t)
	user := b.user("racer", true)
	b.addRun(runSpec{user: user, score: 1000, durationMs: new(int32(15000))})
	b.addRun(runSpec{user: user, score: 2000, mode: leaderboard.ModeWords, wordCount: new(int32(50))})

	resp := b.get("/api/v1/leaderboards")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body := decodeInto[bucketsBody](t, resp)
	require.Len(t, body.Buckets, 2)

	// Sorted by key, so "time:..." follows "words:..." alphabetically.
	timeBucket, wordBucket := body.Buckets[0], body.Buckets[1]
	assert.Equal(t, "time:15000:en:seeded", timeBucket.Bucket)
	assert.Equal(t, "time", timeBucket.Mode)
	require.NotNil(t, timeBucket.DurationMs)
	assert.EqualValues(t, 15000, *timeBucket.DurationMs)
	assert.Nil(t, timeBucket.WordCount, "a time bucket must not report a word count")
	assert.Equal(t, "en", timeBucket.Lang)
	assert.Equal(t, "seeded", timeBucket.TextSource)
	assert.EqualValues(t, 1, timeBucket.Entries)

	assert.Equal(t, "words:50:en:seeded", wordBucket.Bucket)
	require.NotNil(t, wordBucket.WordCount)
	assert.EqualValues(t, 50, *wordBucket.WordCount)
	assert.Nil(t, wordBucket.DurationMs)
}

func TestBucketsIndexIsEmptyNotNull(t *testing.T) {
	b := newBoard(t)
	resp := b.get("/api/v1/leaderboards")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.JSONEq(t, `{"buckets":[]}`, string(readBody(t, resp)),
		"an empty index must be [] so a client can render it without a null check")
}

func TestPageIsRankedAndComplete(t *testing.T) {
	b := newBoard(t)
	first := b.user("first", true)
	second := b.user("second", true)

	runID := b.addRun(runSpec{
		user: first, score: 2000, wpm: 120.5, raw: 130.25, acc: 0.99,
		achievedAt: minutesAgo(30),
	})
	b.addRun(runSpec{user: second, score: 1000, achievedAt: minutesAgo(20)})

	resp := b.get("/api/v1/leaderboards/time:15000:en:seeded")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body := decodeInto[pageBody](t, resp)

	assert.Equal(t, "time:15000:en:seeded", body.Bucket)
	require.Len(t, body.Entries, 2)
	assert.Empty(t, body.NextCursor, "the last page must carry no cursor")

	top := body.Entries[0]
	assert.EqualValues(t, 1, top.Rank)
	assert.Equal(t, first, top.UserID)
	assert.Equal(t, "first", top.DisplayName)
	assert.EqualValues(t, 2000, top.Score)
	assert.InDelta(t, 120.5, top.WPM, 1e-9)
	assert.InDelta(t, 130.25, top.Raw, 1e-9)
	assert.InDelta(t, 0.99, top.Acc, 1e-9)
	assert.Equal(t, "S", top.Grade)
	assert.Equal(t, runID, top.RunID)
	assert.NotEmpty(t, top.AchievedAt)
	assert.JSONEq(t, `{
		"punctuation": false, "numbers": false, "randomCase": false, "reverse": false,
		"nospace": false, "difficulty": "normal", "minWpm": 0,
		"blind": false, "fading": false, "flashlight": false}`, string(top.Mods))

	assert.EqualValues(t, 2, body.Entries[1].Rank)
	assert.Equal(t, second, body.Entries[1].UserID)
}

// Paging over the HTTP surface: the cursor round-trips and the ranks continue
// across the boundary rather than restarting at 1.
func TestPageCursorContinuesTheRanking(t *testing.T) {
	b := newBoard(t)
	for i := range 5 {
		u := b.user(fmt.Sprintf("player%02d", i), true)
		b.addRun(runSpec{user: u, score: int64(1000 - i), achievedAt: minutesAgo(30 - i)})
	}

	resp := b.get("/api/v1/leaderboards/time:15000:en:seeded?limit=2")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	page1 := decodeInto[pageBody](t, resp)
	require.Len(t, page1.Entries, 2)
	require.NotEmpty(t, page1.NextCursor)
	assert.EqualValues(t, []int64{1, 2}, []int64{page1.Entries[0].Rank, page1.Entries[1].Rank})

	resp = b.get("/api/v1/leaderboards/time:15000:en:seeded?limit=2&cursor=" + page1.NextCursor)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	page2 := decodeInto[pageBody](t, resp)
	require.Len(t, page2.Entries, 2)
	assert.EqualValues(t, []int64{3, 4}, []int64{page2.Entries[0].Rank, page2.Entries[1].Rank})
	assert.NotEqual(t, page1.Entries[1].UserID, page2.Entries[0].UserID)

	resp = b.get("/api/v1/leaderboards/time:15000:en:seeded?limit=2&cursor=" + page2.NextCursor)
	page3 := decodeInto[pageBody](t, resp)
	require.Len(t, page3.Entries, 1)
	assert.EqualValues(t, 5, page3.Entries[0].Rank)
	assert.Empty(t, page3.NextCursor)
}

func TestPageLimitIsClamped(t *testing.T) {
	b := newBoard(t)
	for i := range 3 {
		u := b.user(fmt.Sprintf("player%02d", i), true)
		b.addRun(runSpec{user: u, score: int64(100 + i)})
	}

	for _, q := range []string{"?limit=0", "?limit=-5", "?limit=abc", "?limit=1000", ""} {
		resp := b.get("/api/v1/leaderboards/time:15000:en:seeded" + q)
		require.Equal(t, http.StatusOK, resp.StatusCode, "limit %q", q)
		body := decodeInto[pageBody](t, resp)
		assert.Len(t, body.Entries, 3, "limit %q must fall back inside [1,100]", q)
	}
}

func TestPageRejectsJunk(t *testing.T) {
	b := newBoard(t)

	t.Run("unparseable bucket is 404", func(t *testing.T) {
		resp := b.get("/api/v1/leaderboards/not-a-bucket")
		assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	})

	t.Run("well-formed but empty bucket is an empty page", func(t *testing.T) {
		resp := b.get("/api/v1/leaderboards/words:100:ru:seeded")
		require.Equal(t, http.StatusOK, resp.StatusCode)
		body := decodeInto[pageBody](t, resp)
		assert.Empty(t, body.Entries)
	})

	t.Run("bad cursor is 400", func(t *testing.T) {
		resp := b.get("/api/v1/leaderboards/time:15000:en:seeded?cursor=!!!")
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})
}

// The boards are public. That is a deliberate product decision (a board nobody
// can read without an account is a board nobody links to), so it gets a test.
func TestBoardsAreReadableWithoutASession(t *testing.T) {
	b := newBoard(t)
	user := b.user("racer", true)
	b.addRun(runSpec{user: user, score: 1000})
	b.asUser = uuid.Nil

	assert.Equal(t, http.StatusOK, b.get("/api/v1/leaderboards").StatusCode)
	assert.Equal(t, http.StatusOK, b.get("/api/v1/leaderboards/time:15000:en:seeded").StatusCode)
}

func TestMeEndpoint(t *testing.T) {
	b := newBoard(t)
	ahead := b.user("ahead", true)
	me := b.user("mine", true)
	b.addRun(runSpec{user: ahead, score: 2000, achievedAt: minutesAgo(30)})
	runID := b.addRun(runSpec{user: me, score: 1000, achievedAt: minutesAgo(20)})

	t.Run("anonymous is 401", func(t *testing.T) {
		b.asUser = uuid.Nil
		resp := b.get("/api/v1/leaderboards/time:15000:en:seeded/me")
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	})

	t.Run("own rank and entry", func(t *testing.T) {
		b.asUser = me
		resp := b.get("/api/v1/leaderboards/time:15000:en:seeded/me")
		require.Equal(t, http.StatusOK, resp.StatusCode)

		body := decodeInto[struct {
			Bucket string `json:"bucket"`
			Entry  struct {
				Rank   int64     `json:"rank"`
				UserID uuid.UUID `json:"userId"`
				RunID  uuid.UUID `json:"runId"`
				Score  int64     `json:"score"`
			} `json:"entry"`
		}](t, resp)

		assert.Equal(t, "time:15000:en:seeded", body.Bucket)
		assert.EqualValues(t, 2, body.Entry.Rank)
		assert.Equal(t, me, body.Entry.UserID)
		assert.Equal(t, runID, body.Entry.RunID)
		assert.EqualValues(t, 1000, body.Entry.Score)
	})

	t.Run("no entry in that bucket is 204", func(t *testing.T) {
		b.asUser = me
		resp := b.get("/api/v1/leaderboards/words:100:en:seeded/me")
		assert.Equal(t, http.StatusNoContent, resp.StatusCode)
	})

	t.Run("banned players see 204, not their hidden slot", func(t *testing.T) {
		b.ban(me, nil)
		b.asUser = me
		resp := b.get("/api/v1/leaderboards/time:15000:en:seeded/me")
		assert.Equal(t, http.StatusNoContent, resp.StatusCode)
	})

	t.Run("unknown bucket is 404 even with a session", func(t *testing.T) {
		b.asUser = me
		resp := b.get("/api/v1/leaderboards/nonsense/me")
		assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	})
}
