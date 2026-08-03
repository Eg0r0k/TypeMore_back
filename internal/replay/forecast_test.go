package replay

import (
	"context"
	"encoding/json"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// THE REVALIDATE FORECAST, measured instead of predicted.
//
// `metric_mismatch` is not a comparison between two computations of the same
// thing. It is the server's FRESH numbers against numbers the CLIENT froze at
// submission time and that can never be recomputed (`compareMetrics`: wpm, raw
// and acc, at a tolerance of 1e-9). So the moment a metric formula changes in
// the core, every stored run whose value moves starts disagreeing with its own
// record — permanently, and through no fault of the run.
//
// That is not a hypothesis. It is where the 41 `metric_mismatch` runs in the
// 2026-08-03 export came from: the net-WPM rule changed under a client that had
// already shipped, and history was left behind it. A release that changes
// another metric formula does the same thing again, to a different set of runs.
//
// This test is the instrument that says how many, BEFORE the deploy rather than
// after. It replays the labelled population through the vendored bundle and
// counts the runs whose recomputed metrics no longer match what they carry.
//
// It deliberately asserts NOTHING about that count — the number is a property of
// the release, not a bug, and pinning it would mean editing this file every time
// a formula legitimately moves. What it DOES assert is the shape of the
// divergence: this release changed the net-WPM credit for the word in hand and
// nothing else, so `raw` and `accuracy` must not have moved for a single run. A
// release that claims to touch one formula and moves three is the failure worth
// catching automatically.
func TestRevalidateForecast(t *testing.T) {
	if testing.Short() {
		t.Skip("replays the population through the bundle")
	}
	core, reg := sharedDicts(t)
	ctx := context.Background()
	runs, _ := loadPopulation(t)

	var replayed, wpmDiffers, rawDiffers, accDiffers int
	var worst float64
	perUser := map[string]int{}

	for _, run := range runs {
		body, ok := reg.Body(run.dictHash)
		if !ok {
			continue
		}
		res, err := core.Replay(ctx, Input{
			Seed: run.seed, DictHash: run.dictHash, DictBody: body,
			Setup: run.setup, Log: run.log, ScoreVersion: run.scoreVersion,
		})
		require.NoError(t, err)
		if res.Metrics == nil {
			continue
		}
		replayed++

		var server struct {
			Wpm      float64 `json:"wpm"`
			Raw      float64 `json:"raw"`
			Accuracy float64 `json:"accuracy"`
		}
		require.NoError(t, json.Unmarshal(res.Metrics, &server))

		// The worker's own comparison, field for field.
		if math.Abs(server.Wpm-run.clientWpm) > metricTolerance {
			wpmDiffers++
			perUser[run.user]++
			if d := math.Abs(server.Wpm - run.clientWpm); d > worst {
				worst = d
			}
		}
		if math.Abs(server.Raw-run.clientRaw) > metricTolerance {
			rawDiffers++
		}
		if math.Abs(server.Accuracy-run.clientAcc) > metricTolerance {
			accDiffers++
		}
	}

	t.Logf("REVALIDATE FORECAST over %d replayable runs of the 2026-08-03 population:", replayed)
	t.Logf("  wpm disagrees with the stored client value : %d  (worst |delta| = %.3f wpm)", wpmDiffers, worst)
	t.Logf("  raw disagrees                              : %d", rawDiffers)
	t.Logf("  accuracy disagrees                         : %d", accDiffers)
	t.Logf("  => expect this many runs to come back flagged metric_mismatch: %d", wpmDiffers)
	for user, n := range perUser {
		t.Logf("       %-16s %d", user, n)
	}

	// The shape assertion. This release moved the NET WPM credit for the word
	// still being typed; `raw` counts characters produced and `accuracy` counts
	// keystrokes, and neither has anything to do with that credit.
	assert.Zero(t, rawDiffers, "raw moved — this release was not supposed to touch it")
	assert.Zero(t, accDiffers, "accuracy moved — this release was not supposed to touch it")
}
