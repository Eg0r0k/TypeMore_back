package leaderboard_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/dop251/goja"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Two things the leaderboard derives in SQL are also stated by the TS core:
// the letter grade of an accuracy, and which flags count as mods. Neither is
// simulation logic — the grade is a five-branch presentation mapping and the mod
// list is field selection — but both would be wrong QUIETLY if the core moved.
//
// So they are fenced here against the real vendored bundle: the same artifact
// internal/replay embeds, evaluated in goja, asked directly. A threshold change
// in shared/core/score.ts turns these red instead of skewing every board.

// loadCore evaluates the vendored core bundle in a bare goja runtime and returns
// its exports object.
func loadCore(t *testing.T) (*goja.Runtime, *goja.Object) {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("..", "replay", "corejs", "core.bundle.js"))
	require.NoError(t, err, "vendored core bundle missing — see `make core-bundle`")

	rt := goja.New()
	_, err = rt.RunScript("core.bundle.js", string(src))
	require.NoError(t, err)

	exports, ok := rt.Get("TypeMoreCore").(*goja.Object)
	require.True(t, ok, "core bundle did not define TypeMoreCore")
	return rt, exports
}

func TestGradeMatchesTheCore(t *testing.T) {
	rt, exports := loadCore(t)
	gradeOf, ok := goja.AssertFunction(exports.Get("gradeOf"))
	require.True(t, ok, "core bundle exports no gradeOf")

	b := newBoard(t)
	ctx := context.Background()

	// The boundaries and the space either side of each of them — an off-by-one
	// in a threshold shows up at exactly these accuracies and nowhere else.
	accuracies := []float64{
		0, 0.5, 0.8999, 0.9, 0.90001, 0.9499, 0.95, 0.95001,
		0.9799, 0.98, 0.98001, 0.9999, 1,
	}

	for _, acc := range accuracies {
		v, err := gradeOf(goja.Undefined(), rt.ToValue(acc))
		require.NoError(t, err)
		want := v.String()

		var got string
		require.NoError(t, b.pool.QueryRow(ctx, `SELECT run_grade($1::numeric)`, acc).Scan(&got))

		assert.Equal(t, want, got,
			"run_grade(%v) disagrees with the core's gradeOf — one of them moved", acc)
	}
}

// run_mods lifts the mod-bearing fields out of a run's setup. It must not fall
// behind the core's table: a mod the core knows about and the projection drops
// is a leaderboard row that under-reports how the score was earned.
func TestModsProjectionCoversEveryCoreMod(t *testing.T) {
	_, exports := loadCore(t)
	multipliers, ok := exports.Get("MOD_MULTIPLIERS").(*goja.Object)
	require.True(t, ok, "core bundle exports no MOD_MULTIPLIERS")

	// The core names two difficulty mods and a family of speed floors; the
	// projection carries the SOURCE fields they are derived from instead of the
	// derived booleans, so those map rather than match by name.
	derived := map[string]string{
		"expert": "difficulty",
		"master": "difficulty",
	}

	b := newBoard(t)
	var projected map[string]any
	require.NoError(t, b.pool.QueryRow(context.Background(),
		`SELECT run_mods($1::jsonb)`, defaultSetup).Scan(&projected))

	for _, id := range multipliers.Keys() {
		field := id
		if mapped, ok := derived[id]; ok {
			field = mapped
		}
		assert.Contains(t, projected, field,
			"the core knows mod %q but run_mods projects no %q field", id, field)
	}

	// The speed floor is a number, not a flag, and has no MOD_MULTIPLIERS key.
	assert.Contains(t, projected, "minWpm", "the minSpeed floor must be projected")
}

// The projected mods are what a board row renders as badges, so their shape is a
// contract, not an implementation detail.
func TestModsProjectionShape(t *testing.T) {
	b := newBoard(t)
	ctx := context.Background()

	modded := `{
	  "config":      {"mode":"words","durationMs":60000,"maxExtraChars":20,"difficulty":"expert","nospace":true,"minWpm":80},
	  "generation":  {"mode":"words","length":50,"punctuation":true,"numbers":true,"randomCase":true,"reverse":false},
	  "declaration": {"blind":true,"fading":false,"flashlight":false}
	}`

	var got map[string]any
	require.NoError(t, b.pool.QueryRow(ctx, `SELECT run_mods($1::jsonb)`, modded).Scan(&got))
	assert.Equal(t, map[string]any{
		"punctuation": true, "numbers": true, "randomCase": true, "reverse": false,
		"nospace": true, "difficulty": "expert", "minWpm": float64(80),
		"blind": true, "fading": false, "flashlight": false,
	}, got)

	// A setup missing a section must still project a complete, false-by-default
	// object rather than nulls a client has to guard against.
	require.NoError(t, b.pool.QueryRow(ctx, `SELECT run_mods('{}'::jsonb)`).Scan(&got))
	assert.Equal(t, map[string]any{
		"punctuation": false, "numbers": false, "randomCase": false, "reverse": false,
		"nospace": false, "difficulty": "normal", "minWpm": float64(0),
		"blind": false, "fading": false, "flashlight": false,
	}, got)
}

// The grade on an entry is derived from the SERVER's accuracy at projection
// time, so it has to be right on the row itself, not just in the function.
func TestEntryCarriesTheGradeOfItsAccuracy(t *testing.T) {
	cases := []struct {
		acc   float64
		grade string
	}{
		{1, "SS"}, {0.99, "S"}, {0.96, "A"}, {0.91, "B"}, {0.7, "C"},
	}

	for _, tc := range cases {
		t.Run(tc.grade, func(t *testing.T) {
			b := newBoard(t)
			user := b.user("racer", true)
			b.addRun(runSpec{user: user, score: 1000, acc: tc.acc})

			entry, ok := b.storedEntry(bucket15s(t), user)
			require.True(t, ok)
			assert.Equal(t, tc.grade, entry.Grade)
		})
	}
}
