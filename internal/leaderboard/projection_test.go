package leaderboard_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/typemore/typemore-server/internal/leaderboard"
)

// A board holds a player's BEST run, so a worse one must leave it alone — and
// leave it alone physically, not just numerically: the entry keeps naming the
// run that earned it.
func TestOnlyABetterRunTakesTheSlot(t *testing.T) {
	b := newBoard(t)
	bucket := bucket15s(t)
	user := b.user("racer", true)

	first := b.addRun(runSpec{user: user, score: 1000, achievedAt: minutesAgo(30)})

	entry, ok := b.storedEntry(bucket, user)
	require.True(t, ok, "the first accepted run must take the empty slot")
	assert.Equal(t, first, entry.RunID)
	assert.EqualValues(t, 1000, entry.Score)

	// Worse: ignored.
	worse := b.addRun(runSpec{user: user, score: 500, achievedAt: minutesAgo(20)})
	entry, ok = b.storedEntry(bucket, user)
	require.True(t, ok)
	assert.Equal(t, first, entry.RunID, "a worse run must not displace the best one")
	assert.EqualValues(t, 1000, entry.Score)
	assert.NotEqual(t, worse, entry.RunID)

	// Equal: also ignored — the earlier achievement wins a tie, and the
	// incumbent is earlier by definition.
	b.addRun(runSpec{user: user, score: 1000, achievedAt: minutesAgo(10)})
	entry, ok = b.storedEntry(bucket, user)
	require.True(t, ok)
	assert.Equal(t, first, entry.RunID, "an equal score must not steal the slot from the earlier run")

	// Better: takes over.
	best := b.addRun(runSpec{user: user, score: 1500, achievedAt: minutesAgo(5)})
	entry, ok = b.storedEntry(bucket, user)
	require.True(t, ok)
	assert.Equal(t, best, entry.RunID)
	assert.EqualValues(t, 1500, entry.Score)

	// One slot per player per bucket, throughout.
	assert.Len(t, b.storedEntries(), 1)
}

// The case that rots: a run is demoted AFTER it took the slot. The board must
// fall back to the player's next best accepted run rather than keeping a score
// the runs table no longer stands behind.
func TestDemotionPromotesTheNextBestRun(t *testing.T) {
	for _, status := range []string{"flagged", "rejected", "pending"} {
		t.Run("demoted to "+status, func(t *testing.T) {
			b := newBoard(t)
			bucket := bucket15s(t)
			user := b.user("racer", true)

			second := b.addRun(runSpec{user: user, score: 1000, achievedAt: minutesAgo(30)})
			top := b.addRun(runSpec{user: user, score: 1500, achievedAt: minutesAgo(20)})

			entry, ok := b.storedEntry(bucket, user)
			require.True(t, ok)
			require.Equal(t, top, entry.RunID)

			b.judge(top, status)

			entry, ok = b.storedEntry(bucket, user)
			require.True(t, ok, "demoting the best run must not empty a slot the player still deserves")
			assert.Equal(t, second, entry.RunID)
			assert.EqualValues(t, 1000, entry.Score)
		})
	}
}

// The other half of the same case: when the demoted run was the ONLY thing
// holding the slot, the slot goes away entirely — the board must not keep a
// row pointing at a run nobody may see.
func TestDemotingTheOnlyEntryClearsTheCell(t *testing.T) {
	b := newBoard(t)
	bucket := bucket15s(t)
	user := b.user("racer", true)

	only := b.addRun(runSpec{user: user, score: 1000})
	require.Len(t, b.storedEntries(), 1)

	b.judge(only, "flagged")

	_, ok := b.storedEntry(bucket, user)
	assert.False(t, ok, "the cell must be cleared, not left pointing at a flagged run")
	assert.Empty(t, b.storedEntries())

	// And the bucket disappears from the index rather than reporting zero.
	buckets, err := b.store.Buckets(context.Background())
	require.NoError(t, err)
	assert.Empty(t, buckets)
}

// Demotion is not a one-way door: a moderator (or a revalidation under a fixed
// policy) putting a run back must restore its slot without a rebuild.
func TestRepromotionRestoresTheSlot(t *testing.T) {
	b := newBoard(t)
	bucket := bucket15s(t)
	user := b.user("racer", true)

	run := b.addRun(runSpec{user: user, score: 1000})
	b.judge(run, "flagged")
	require.Empty(t, b.storedEntries())

	b.judge(run, "accepted")

	entry, ok := b.storedEntry(bucket, user)
	require.True(t, ok)
	assert.Equal(t, run, entry.RunID)
}

// Demoting a run that was NOT the player's best must leave the board untouched.
// Recomputing the cell makes this free, but "free" is exactly the kind of thing
// that stops being true silently.
func TestDemotingANonBestRunChangesNothing(t *testing.T) {
	b := newBoard(t)
	bucket := bucket15s(t)
	user := b.user("racer", true)

	worse := b.addRun(runSpec{user: user, score: 500, achievedAt: minutesAgo(30)})
	best := b.addRun(runSpec{user: user, score: 1500, achievedAt: minutesAgo(20)})

	before, ok := b.storedEntry(bucket, user)
	require.True(t, ok)

	b.judge(worse, "rejected")

	after, ok := b.storedEntry(bucket, user)
	require.True(t, ok)
	assert.Equal(t, before, after)
	assert.Equal(t, best, after.RunID)
}

// Buckets are separate boards. A run in one must never disturb another, and a
// player can hold a slot in each.
func TestBucketsAreIndependent(t *testing.T) {
	b := newBoard(t)
	user := b.user("racer", true)

	b.addRun(runSpec{user: user, score: 1000, durationMs: new(int32(15000))})
	b.addRun(runSpec{user: user, score: 2000, durationMs: new(int32(30000))})
	b.addRun(runSpec{user: user, score: 3000, mode: leaderboard.ModeWords, wordCount: new(int32(50))})
	b.addRun(runSpec{user: user, score: 4000, lang: "ru"})

	entries := b.storedEntries()
	keys := make([]string, len(entries))
	for i := range entries {
		keys[i] = entries[i].BucketKey
	}
	assert.ElementsMatch(t, []string{
		"time:15000:en:seeded",
		"time:30000:en:seeded",
		"words:50:en:seeded",
		"time:15000:ru:seeded",
	}, keys)
}

// The point of the weighted review policy: a run that raised a weak plausibility
// flag is ACCEPTED, and an accepted run ranks. If flags disqualified, policy v1
// would have bought nothing.
func TestAcceptedRunWithFlagsStillRanks(t *testing.T) {
	b := newBoard(t)
	bucket := bucket15s(t)
	user := b.user("racer", true)

	run := b.addRun(runSpec{user: user, score: 1200, flags: []string{"min-interval"}})

	entry, ok := b.storedEntry(bucket, user)
	require.True(t, ok, "an accepted run with flags must still rank (docs/REPLAY.md, Review policy)")
	assert.Equal(t, run, entry.RunID)
}

// Eligibility, as a table. Everything here is decided by the
// leaderboard_eligible_runs view, so this is the executable form of the
// eligibility table in docs/LEADERBOARDS.md.
func TestEligibility(t *testing.T) {
	quoteSetup := `{
	  "config":      {"mode":"time","durationMs":15000,"maxExtraChars":20,"difficulty":"normal","nospace":false,"minWpm":0},
	  "generation":  {"mode":"time","length":0,"punctuation":false,"numbers":false,"randomCase":false,"reverse":false,
	                  "textSource":{"kind":"quote","quoteId":"q1"}},
	  "declaration": {"blind":false,"fading":false,"flashlight":false}
	}`

	cases := []struct {
		name     string
		spec     runSpec
		eligible bool
		why      string
	}{
		{"accepted seeded 15s", runSpec{}, true, ""},
		{"accepted seeded 25 words", runSpec{mode: leaderboard.ModeWords, wordCount: new(int32(25))}, true, ""},
		{"accepted with flags", runSpec{flags: []string{"afk-heavy"}}, true, ""},
		{"pending", runSpec{status: "pending"}, false, "an unjudged run has no server numbers to rank"},
		{"flagged", runSpec{status: "flagged"}, false, "a run under review is not a result"},
		{"rejected", runSpec{status: "rejected"}, false, "an invalid log is not a result"},
		{"quote text", runSpec{setup: quoteSetup}, false, "quotes rank per quote, never globally (SCORING_CONCEPT §6)"},
		{"unranked duration", runSpec{durationMs: new(int32(600_000))}, false, "10min is out of ranked (SCORING_CONCEPT §4)"},
		{"unranked word count", runSpec{mode: leaderboard.ModeWords, wordCount: new(int32(10))}, false, "not a ranked word count"},
		{"odd duration", runSpec{durationMs: new(int32(17_000))}, false, "not a ranked duration"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := newBoard(t)
			spec := tc.spec
			spec.user = b.user("racer", true)
			spec.score = 1000
			b.addRun(spec)

			if tc.eligible {
				assert.Len(t, b.storedEntries(), 1)
				return
			}
			assert.Empty(t, b.storedEntries(), tc.why)
		})
	}
}

// The verified-email gate is deployment policy, so it is checked where the
// projection happens rather than in the schema. With it on, an unverified
// account's runs are accepted, kept, and simply do not rank.
func TestVerifiedEmailGate(t *testing.T) {
	t.Run("on: unverified accounts do not rank", func(t *testing.T) {
		b := newBoard(t, func(o *boardOpts) { o.requireVerifiedEmail = true })
		unverified := b.user("drifter", false)
		verified := b.user("regular", true)

		b.addRun(runSpec{user: unverified, score: 9999})
		b.addRun(runSpec{user: verified, score: 100})

		entries := b.storedEntries()
		require.Len(t, entries, 1)
		assert.Equal(t, verified, entries[0].UserID)
	})

	t.Run("off: anyone with an accepted run ranks", func(t *testing.T) {
		b := newBoard(t, func(o *boardOpts) { o.requireVerifiedEmail = false })
		unverified := b.user("drifter", false)
		b.addRun(runSpec{user: unverified, score: 9999})
		assert.Len(t, b.storedEntries(), 1)
	})

	// Flipping the gate does not retroactively rewrite history — a rebuild does.
	// That is the documented contract, and it is what makes the setting cheap.
	t.Run("flipping it off takes effect on a rebuild", func(t *testing.T) {
		b := newBoard(t, func(o *boardOpts) { o.requireVerifiedEmail = true })
		unverified := b.user("drifter", false)
		b.addRun(runSpec{user: unverified, score: 9999})
		require.Empty(t, b.storedEntries())

		open := newStoreOn(t, b, false)
		_, err := open.Rebuild(context.Background())
		require.NoError(t, err)
		assert.Len(t, b.storedEntries(), 1)
	})
}
