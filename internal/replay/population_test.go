package replay

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"sort"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// THE REAL POPULATION, replayed through the current bundle.
//
// Every other test in this package builds its own log. This one does not: it
// replays the runs a production database actually holds, and it exists because
// the detector this release repairs cannot be judged on synthetic logs. A
// hand-built run proves the rule fires; only a real population proves it fires
// on the right runs and on nothing else.
//
// The fixture is an export of 2026-08-03 — 138 runs, six accounts — with one
// column added: `label`, `honest` or `cheat`. One account (`mental1sm`) is a
// confirmed cheat; the rest are ordinary players. That labelling is a human
// judgement and it is the only thing here that is not measured.
//
// THE INVARIANT THIS DEFENDS. `superhuman-burst` is the one flag with a ZERO
// false-positive rate on this population — it has never fired on an honest run.
// The v4 change makes it fire far more readily (accuracy stopped being a gate,
// and the ceiling falls with the run's duration), so the thing most worth
// checking is not that it now catches the cheat — that is easy — but that it
// STILL catches nobody else. A detector that flags real players is worse than
// the hole it closed.
//
// ADDING RUNS IS A FILE EDIT, NOT A CODE EDIT. Append rows to the CSV with a
// label and the harness picks them up. That is the whole point of the shape: the
// next export can be dropped in by anyone, and the assertions below are about
// the labels rather than about row counts.
const populationFixture = "testdata/population/runs-2026-08-03.csv"

type populationRun struct {
	label        string
	user         string
	status       string
	seed         int64
	dictHash     string
	scoreVersion int16
	setup        json.RawMessage
	log          json.RawMessage
	// The metrics the server recorded at the time, for reporting: this test does
	// not assert them (that is what the golden vectors are for), it prints them
	// so a failure says WHICH run at WHAT speed.
	wpm      float64
	accuracy float64
	duration float64
}

func gunzipHex(t *testing.T, s string) json.RawMessage {
	t.Helper()
	raw, err := hex.DecodeString(s)
	require.NoError(t, err)
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	require.NoError(t, err)
	defer func() { _ = zr.Close() }()
	out, err := io.ReadAll(zr)
	require.NoError(t, err)
	return json.RawMessage(out)
}

// loadPopulation reads the fixture and reports what it could NOT use, loudly.
// A harness that silently drops rows it cannot resolve reads as "the whole
// population is clean" when it means "the part I looked at is".
func loadPopulation(t *testing.T) (runs []populationRun, skipped map[string]int) {
	t.Helper()
	f, err := os.Open(populationFixture)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()

	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	records, err := r.ReadAll()
	require.NoError(t, err)
	require.Greater(t, len(records), 1, "fixture has no rows")

	index := map[string]int{}
	for i, name := range records[0] {
		index[name] = i
	}
	get := func(row []string, name string) string {
		i, ok := index[name]
		if !ok || i >= len(row) {
			return ""
		}
		return row[i]
	}

	skipped = map[string]int{}
	num := func(raw json.RawMessage, field string) float64 {
		var m map[string]any
		if json.Unmarshal(raw, &m) != nil {
			return 0
		}
		v, _ := m[field].(float64)
		return v
	}

	for _, row := range records[1:] {
		setup := get(row, "setup")
		// A quote run needs its text resolved from the quote registry, which
		// lives in another domain package this one deliberately cannot import.
		// All seven in this export belong to one honest account and none is
		// anywhere near the speed ceiling, so the invariant below is unaffected
		// — but the count is reported rather than swallowed.
		if bytes.Contains([]byte(setup), []byte("quoteHash")) {
			skipped["quote run (text not resolvable from this package)"]++
			continue
		}
		seed, err := strconv.ParseInt(get(row, "seed"), 10, 64)
		if err != nil {
			skipped["unparseable seed"]++
			continue
		}
		sv, err := strconv.ParseInt(get(row, "score_version"), 10, 16)
		if err != nil {
			skipped["unparseable score_version"]++
			continue
		}
		metrics := json.RawMessage(get(row, "server_metrics"))
		runs = append(runs, populationRun{
			label:        get(row, "label"),
			user:         get(row, "user"),
			status:       get(row, "status"),
			seed:         seed,
			dictHash:     get(row, "dict_hash"),
			scoreVersion: int16(sv),
			setup:        json.RawMessage(setup),
			log:          gunzipHex(t, get(row, "log")),
			wpm:          num(metrics, "wpm"),
			accuracy:     num(metrics, "accuracy"),
			duration:     num(metrics, "durationSec"),
		})
	}
	return runs, skipped
}

func TestSuperhumanBurstFiresOnTheCheatAndOnNobodyElse(t *testing.T) {
	if testing.Short() {
		t.Skip("replays a 138-run population through the bundle")
	}
	core, reg := sharedDicts(t)
	ctx := context.Background()

	runs, skipped := loadPopulation(t)
	require.NotEmpty(t, runs)

	type outcome struct {
		run    populationRun
		burst  bool
		score  float64
		reason string
	}
	var results []outcome
	unresolved := 0

	for _, run := range runs {
		body, ok := reg.Body(run.dictHash)
		if !ok {
			// A dictionary that has since been rotated out. Counted, not hidden.
			unresolved++
			continue
		}
		res, err := core.Replay(ctx, Input{
			Seed:         run.seed,
			DictHash:     run.dictHash,
			DictBody:     body,
			Setup:        run.setup,
			Log:          run.log,
			ScoreVersion: run.scoreVersion,
			// The canary epoch was unset for every run in this export, which is
			// what production judged them under. Arming them here would score
			// coincidental early commits as evidence.
			CanariesArmed: false,
		})
		require.NoError(t, err, "%s run replay failed", run.user)

		out := outcome{run: run, reason: res.Reason}
		for _, f := range res.Flags {
			if f.Code == "superhuman-burst" {
				out.burst = true
				out.score = f.Score
			}
		}
		results = append(results, out)
	}

	if unresolved > 0 {
		t.Logf("SKIPPED %d run(s): dictionary no longer in the registry", unresolved)
	}
	for reason, n := range skipped {
		t.Logf("SKIPPED %d run(s): %s", n, reason)
	}
	t.Logf("replayed %d of %d fixture rows", len(results), len(runs)+sumOf(skipped))

	// The expected x actual matrix, printed whatever happens — the number this
	// test is really about is the top-right cell, and it should be zero.
	var honestFlagged, honestClean, cheatFlagged, cheatClean int
	for _, o := range results {
		switch {
		case o.run.label == "honest" && o.burst:
			honestFlagged++
		case o.run.label == "honest":
			honestClean++
		case o.burst:
			cheatFlagged++
		default:
			cheatClean++
		}
	}
	t.Logf("superhuman-burst    flagged   clean")
	t.Logf("  honest            %7d %7d", honestFlagged, honestClean)
	t.Logf("  cheat             %7d %7d", cheatFlagged, cheatClean)

	// (1) THE INVARIANT. Not one honest run, at any speed, may raise it.
	for _, o := range results {
		if o.run.label != "honest" {
			continue
		}
		assert.False(t, o.burst,
			"honest run by %s flagged superhuman-burst: %.1f wpm, acc %.3f, %.0fs (severity %.4f)",
			o.run.user, o.run.wpm, o.run.accuracy, o.run.duration, o.score)
	}

	// (2) And the four runs the investigation named must all raise it — the two
	// that always did, and the two that a single mistyped character hid.
	//
	// Identified by their recorded speed rather than by a row number, so the
	// fixture can be re-exported or appended to without renumbering anything.
	mustFlag := []float64{373.4, 336.2, 282.0, 212.4}
	for _, wpm := range mustFlag {
		found := false
		for _, o := range results {
			if o.run.label != "cheat" || absDiff(o.run.wpm, wpm) > 0.05 {
				continue
			}
			found = true
			assert.True(t, o.burst,
				"the %.1f wpm run must raise superhuman-burst (acc %.3f, %.0fs)",
				o.run.wpm, o.run.accuracy, o.run.duration)
			assert.Greater(t, o.score, 0.0, "a raised flag with zero severity is not a flag")
		}
		assert.True(t, found, "no cheat run at %.1f wpm in the fixture", wpm)
	}

	// (3) The runs below the ceiling must NOT raise it, cheat or not — the
	// detector judges speed, not the account.
	mustNotFlag := []float64{195.2, 163.2, 160.8, 158.4, 106.8, 60.0, 20.4}
	for _, wpm := range mustNotFlag {
		for _, o := range results {
			if absDiff(o.run.wpm, wpm) > 0.05 {
				continue
			}
			assert.False(t, o.burst,
				"the %.1f wpm run (%.0fs) must not raise superhuman-burst", o.run.wpm, o.run.duration)
		}
	}
}

// The flag's severity has to move with the speed, or "flagged" carries no
// information beyond a boolean and the weights table has nothing to weigh.
func TestSuperhumanBurstSeverityOrdersTheCheatRuns(t *testing.T) {
	if testing.Short() {
		t.Skip("replays a 138-run population through the bundle")
	}
	core, reg := sharedDicts(t)
	ctx := context.Background()
	runs, _ := loadPopulation(t)

	type pair struct {
		wpm, severity float64
	}
	var flagged []pair
	for _, run := range runs {
		if run.label != "cheat" {
			continue
		}
		body, ok := reg.Body(run.dictHash)
		if !ok {
			continue
		}
		res, err := core.Replay(ctx, Input{
			Seed: run.seed, DictHash: run.dictHash, DictBody: body,
			Setup: run.setup, Log: run.log, ScoreVersion: run.scoreVersion,
		})
		require.NoError(t, err)
		for _, f := range res.Flags {
			if f.Code == "superhuman-burst" {
				flagged = append(flagged, pair{run.wpm, f.Score})
			}
		}
	}
	require.Len(t, flagged, 4, "four runs are expected above their ceiling")

	sort.Slice(flagged, func(i, j int) bool { return flagged[i].wpm < flagged[j].wpm })
	for i := 1; i < len(flagged); i++ {
		assert.Greater(t, flagged[i].severity, flagged[i-1].severity,
			"%.1f wpm scored no higher than %.1f wpm", flagged[i].wpm, flagged[i-1].wpm)
	}
	// The two that flagged under the OLD rule keep the severity they had: the
	// scale is a fixed 500 wpm and their accuracy is 1, so the revalidate pass
	// moves the runs that were being missed and leaves these two alone.
	for _, p := range flagged {
		if absDiff(p.wpm, 373.4) < 0.05 {
			assert.InDelta(t, 0.7468, p.severity, 0.0005)
		}
		if absDiff(p.wpm, 282.0) < 0.05 {
			assert.InDelta(t, 0.5640, p.severity, 0.0005)
		}
	}
}

func absDiff(a, b float64) float64 {
	if a > b {
		return a - b
	}
	return b - a
}

func sumOf(m map[string]int) int {
	total := 0
	for _, v := range m {
		total += v
	}
	return total
}
