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

// The projection is only trustworthy if it can be thrown away. A rebuild after
// an arbitrary history of promotions and demotions must land on exactly the
// table the incremental path produced — same runs, same scores, same
// achievement times, same grades, same mods.
//
// The two paths share the maintenance statement by construction, so this is
// less "do two implementations agree" than "does the cell ENUMERATION cover
// everything the incremental path touched" — which is the part that can be
// wrong, and is precisely what silently loses a player's slot.
func TestRebuildReproducesIncrementalMaintenance(t *testing.T) {
	b := newBoard(t)
	ctx := context.Background()

	// A deterministic pseudo-random history: fixed seed, so a failure is
	// reproducible rather than a story about a flake.
	rng := rand.New(rand.NewPCG(0x7ea15e, 0xb042d))

	users := make([]uuid.UUID, 0, 6)
	for i := range 6 {
		users = append(users, b.user(fmt.Sprintf("player%02d", i), i%3 != 0))
	}
	shapes := []runSpec{
		{mode: leaderboard.ModeTime, durationMs: new(int32(15000)), lang: "en"},
		{mode: leaderboard.ModeTime, durationMs: new(int32(30000)), lang: "en"},
		{mode: leaderboard.ModeTime, durationMs: new(int32(60000)), lang: "ru"},
		{mode: leaderboard.ModeWords, wordCount: new(int32(25)), lang: "en"},
		{mode: leaderboard.ModeWords, wordCount: new(int32(50)), lang: "german"},
		// Deliberately unrankable: 10min and an odd word count must survive the
		// whole exercise without ever producing an entry, on either path.
		{mode: leaderboard.ModeTime, durationMs: new(int32(600_000)), lang: "en"},
		{mode: leaderboard.ModeWords, wordCount: new(int32(7)), lang: "en"},
	}
	statuses := []string{"accepted", "flagged", "rejected", "pending"}

	planted := make([]uuid.UUID, 0, 120)
	for i := range 120 {
		spec := shapes[rng.IntN(len(shapes))]
		spec.user = users[rng.IntN(len(users))]
		spec.score = int64(rng.IntN(3000))
		spec.acc = 0.85 + float64(rng.IntN(16))/100 // spans every grade boundary
		spec.wpm = 40 + float64(rng.IntN(120))
		spec.raw = spec.wpm + 1
		spec.achievedAt = minutesAgo(200 - i)
		planted = append(planted, b.addRun(spec))
	}

	// Then churn: re-judge a third of them at random, which is what a
	// revalidation pass or a moderator queue does to a live board.
	for range 40 {
		runID := planted[rng.IntN(len(planted))]
		b.judge(runID, statuses[rng.IntN(len(statuses))])
	}

	incremental := b.storedEntries()
	require.NotEmpty(t, incremental, "the exercise must actually populate boards")

	stats, err := b.store.Rebuild(ctx)
	require.NoError(t, err)

	rebuilt := b.storedEntries()
	assert.Equal(t, incremental, rebuilt,
		"a rebuild must reproduce the incrementally maintained table exactly")
	assert.EqualValues(t, len(incremental), stats.Before)
	assert.EqualValues(t, len(rebuilt), stats.After)
	assert.Equal(t, len(rebuilt), stats.Cells,
		"every enumerated cell must have produced exactly one entry")

	// And it is idempotent: running it again changes nothing.
	_, err = b.store.Rebuild(ctx)
	require.NoError(t, err)
	assert.Equal(t, rebuilt, b.storedEntries())
}

// A rebuild is also the repair tool, so it has to fix a table that drifted.
// Corrupting the entries directly is the only way to prove it repairs rather
// than merely re-deriving what was already right.
func TestRebuildRepairsADriftedTable(t *testing.T) {
	b := newBoard(t)
	ctx := context.Background()

	user := b.user("racer", true)
	b.addRun(runSpec{user: user, score: 1500, achievedAt: minutesAgo(30)})
	runnerUp := b.addRun(runSpec{user: user, score: 900, achievedAt: minutesAgo(20)})
	correct := b.storedEntries()
	require.Len(t, correct, 1)

	// Simulate drift two ways at once: a wrong score on the real cell, and a
	// phantom entry in a bucket the player never played. (The phantom names the
	// runner-up run — leaderboard_entries_run_idx already makes it impossible
	// for one run to hold two slots, which is that index earning its keep.)
	_, err := b.pool.Exec(ctx, `UPDATE leaderboard_entries SET score = 999999`)
	require.NoError(t, err)
	_, err = b.pool.Exec(ctx, `
		INSERT INTO leaderboard_entries
			(bucket_key, user_id, run_id, score, wpm, raw, acc, grade, mods, achieved_at)
		VALUES ('words:100:en:seeded', $1, $2, 4242, 1, 1, 1, 'SS', '{}'::jsonb, now())`,
		user, runnerUp)
	require.NoError(t, err)

	stats, err := b.store.Rebuild(ctx)
	require.NoError(t, err)

	assert.Equal(t, correct, b.storedEntries(), "a rebuild must repair a drifted table")
	assert.EqualValues(t, 2, stats.Before)
	assert.EqualValues(t, 1, stats.After)
}

// A rebuild of an empty database is a no-op, not an error — the first deploy
// runs it before anyone has played.
func TestRebuildOnAnEmptyDatabase(t *testing.T) {
	b := newBoard(t)
	stats, err := b.store.Rebuild(context.Background())
	require.NoError(t, err)
	assert.Zero(t, stats.Cells)
	assert.Zero(t, stats.After)
	assert.Empty(t, b.storedEntries())
}
