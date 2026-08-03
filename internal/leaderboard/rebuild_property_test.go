package leaderboard_test

import (
	"context"
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/typemore/typemore-server/internal/leaderboard"
)

// "A rebuild reproduces the incremental table" is the property the whole
// projection design rests on, and TestRebuildReproducesIncrementalMaintenance
// checks it on ONE history: a single fixed seed, one population, one eligibility
// policy. That is a sample, not a property. The two paths share the maintenance
// statement, so what a second sample can actually catch is the half they do NOT
// share — the cell ENUMERATION — and enumeration bugs are shape-dependent by
// nature: they need a particular collision of quote and language coordinates, or
// a player whose only eligible run sits behind the email gate, before they show.
//
// So this sweeps histories instead of asserting one. Each iteration draws its own
// population, its own mix of shapes, its own churn, and — the axis the fixed-seed
// test cannot vary at all — its own eligibility policy, since requireVerifiedEmail
// is a WHERE clause in both the enumeration and the recompute and the two have to
// agree about it.
func TestRebuildReproducesIncrementalMaintenanceAcrossRandomHistories(t *testing.T) {
	if testing.Short() {
		t.Skip("property sweep: several histories, each a full rebuild")
	}

	const histories = 12
	for h := range histories {
		t.Run(fmt.Sprintf("history%02d", h), func(t *testing.T) {
			// Seeded from the iteration index alone: a failure names the history
			// that produced it, and re-running that one subtest reproduces it
			// exactly. A clock-seeded sweep would find the same bugs and be
			// unable to tell anyone which draw found them.
			rng := rand.New(rand.NewPCG(uint64(h)+1, uint64(h)*2+1))

			// The gate alternates rather than being drawn, so the sweep is
			// guaranteed to cover both settings instead of merely likely to.
			requireVerified := h%2 == 1
			b := newBoard(t, func(o *boardOpts) { o.requireVerifiedEmail = requireVerified })

			users := make([]uuid.UUID, 0, 8)
			for i := range 2 + rng.IntN(6) {
				// Verified-ness is drawn, not alternated: with the gate on, a
				// cell whose only runs belong to unverified players must be
				// absent from BOTH paths, and that case only exists if the
				// population is genuinely mixed.
				users = append(users, b.user(fmt.Sprintf("h%02dp%02d", h, i), rng.IntN(3) != 0))
			}

			shapes := []runSpec{
				{mode: leaderboard.ModeTime, durationMs: new(int32(15000)), lang: "en"},
				{mode: leaderboard.ModeTime, durationMs: new(int32(30000)), lang: "en"},
				{mode: leaderboard.ModeTime, durationMs: new(int32(60000)), lang: "ru"},
				{mode: leaderboard.ModeWords, wordCount: new(int32(25)), lang: "en"},
				{mode: leaderboard.ModeWords, wordCount: new(int32(50)), lang: "german"},
				{mode: leaderboard.ModeWords, wordCount: new(int32(100)), lang: "code_css"},
				// Ranked nowhere, on either path: an unranked size, a legal-but-
				// unranked word count, and a run naming a quote that is not in
				// the registry. They must survive every iteration without ever
				// producing an entry.
				{mode: leaderboard.ModeTime, durationMs: new(int32(600_000)), lang: "en"},
				{mode: leaderboard.ModeWords, wordCount: new(int32(7)), lang: "en"},
				{quote: uuid.New(), lang: "en"},
			}
			// One or two quotes, so some histories exercise the coordinate
			// COLLISION (two enumerated tuples naming one quote board) and some
			// do not.
			for q := range 1 + rng.IntN(2) {
				id := b.quote(fmt.Sprintf("lang%d", q), fmt.Sprintf("fixed map %d %d", h, q), "Author")
				shapes = append(shapes,
					// The same quote reached at two different play shapes. This is
					// the one place the per-verdict path and the rebuild spell a
					// cell differently, and it is what makes the dedup in
					// Rebuild load-bearing.
					runSpec{quote: id, lang: "english"},
					runSpec{quote: id, lang: "russian"},
				)
			}

			statuses := []string{"accepted", "flagged", "rejected", "pending"}

			runs := 40 + rng.IntN(80)
			planted := make([]uuid.UUID, 0, runs)
			for i := range runs {
				spec := shapes[rng.IntN(len(shapes))]
				spec.user = users[rng.IntN(len(users))]
				// Scores collide on purpose: equal scores are what make the
				// ORDER BY's tiebreak decide which run holds a cell, and a
				// tiebreak the two paths disagree on is exactly the drift this
				// is looking for.
				spec.score = int64(rng.IntN(50) * 10)
				spec.acc = 0.85 + float64(rng.IntN(16))/100
				spec.wpm = 40 + float64(rng.IntN(120))
				spec.raw = spec.wpm + 1
				spec.achievedAt = minutesAgo(runs - i)
				spec.status = statuses[rng.IntN(len(statuses))]
				// Some runs are adopted twins: stored, listed, ranked nowhere.
				if len(planted) > 0 && rng.IntN(8) == 0 {
					spec.adoptedFrom = planted[rng.IntN(len(planted))]
				}
				planted = append(planted, b.addRun(spec))
			}

			// Churn: re-judge a slice of the population, which is what a
			// revalidation pass does to a live board.
			for range runs / 3 {
				b.judge(planted[rng.IntN(len(planted))], statuses[rng.IntN(len(statuses))])
			}

			incremental := b.storedEntries()

			stats, err := b.store.Rebuild(context.Background())
			require.NoError(t, err)
			rebuilt := b.storedEntries()

			assert.Equal(t, incremental, rebuilt,
				"seed %d (requireVerifiedEmail=%v): a rebuild must reproduce the "+
					"incrementally maintained table exactly", h, requireVerified)
			assert.EqualValues(t, len(incremental), stats.Before)
			assert.EqualValues(t, len(rebuilt), stats.After)
			assert.Equal(t, len(rebuilt), stats.Cells,
				"every enumerated cell must produce exactly one entry — a mismatch "+
					"means the quote-coordinate dedup let a board be recomputed twice")

			// Idempotence, per history: the second rebuild is the cheapest place
			// for an enumeration that depends on the table it is rebuilding to
			// give itself away.
			_, err = b.store.Rebuild(context.Background())
			require.NoError(t, err)
			assert.Equal(t, rebuilt, b.storedEntries(), "a rebuild must be idempotent")
		})
	}
}

// The fourth demotion path, and the only one with no test: the run behind a slot
// is DELETED rather than re-judged.
//
// leaderboard_entries.run_id is ON DELETE CASCADE, so the row disappears without
// anything recomputing the cell — which means the player's next-best accepted run
// is NOT promoted into the slot it should now hold. Every other way a slot can
// empty (re-judged, flagged, rejected, un-adopted) goes through
// RecomputeLeaderboardCell and promotes correctly; this one does not, because
// nothing calls it.
//
// This test pins the behaviour rather than asserting the ideal, and it is the
// evidence for the report rather than a bug to fix: NOTHING deletes a run today.
// There is no DELETE FROM runs anywhere in the codebase — logs are immutable and
// kept indefinitely — and the only reachable cascade is deleting the USER, which
// takes their entries with them through a second ON DELETE CASCADE and leaves
// nothing to promote. If a run-deletion path is ever added (a GDPR erasure of one
// result, a moderator dropping a single run), THIS is the test that has to change
// with it, and the fix is a ProjectRun on the owner's cell after the delete.
func TestDeletingTheRunBehindASlotDoesNotPromoteTheRunnerUp(t *testing.T) {
	b := newBoard(t)
	ctx := context.Background()
	bucket := bucket15s(t)

	user := b.user("racer", true)
	best := b.addRun(runSpec{user: user, score: 1500, achievedAt: minutesAgo(30)})
	runnerUp := b.addRun(runSpec{user: user, score: 900, achievedAt: minutesAgo(20)})

	entry, ok := b.storedEntry(bucket, user)
	require.True(t, ok)
	require.Equal(t, best, entry.RunID, "the better run holds the slot")

	_, err := b.pool.Exec(ctx, `DELETE FROM runs WHERE id = $1`, best)
	require.NoError(t, err)

	// The cascade removed the row. The runner-up is still accepted and still
	// eligible — it is simply never asked about, because a cascade is not a
	// projection.
	_, ok = b.storedEntry(bucket, user)
	assert.False(t, ok,
		"the cascade drops the slot; if this now holds an entry, a delete path "+
			"grew a projection and the comment above is out of date")

	// And the proof that the slot was OWED to the runner-up: the rebuild, which
	// is the repair tool, hands it straight over.
	_, err = b.store.Rebuild(ctx)
	require.NoError(t, err)

	repaired, ok := b.storedEntry(bucket, user)
	require.True(t, ok, "a rebuild must promote the run the cascade skipped")
	assert.Equal(t, runnerUp, repaired.RunID)
	assert.EqualValues(t, 900, repaired.Score)
}
