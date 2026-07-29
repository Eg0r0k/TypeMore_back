package replay_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/typemore/typemore-server/internal/replay"
	replaypg "github.com/typemore/typemore-server/internal/replay/pgstore"
)

// Ingest accepts log v1 and log v2 forever — the version is decided per run by
// the client's capability detection, so both stay live indefinitely and a real
// queue holds them side by side. That acceptance was pinned at the door
// (runs_test.go) and nowhere after it: the only thing that had ever replayed a
// v2 log out of Postgres was a perf gate that nobody runs.
//
// This is the population test. Both versions interleaved in one queue, drained
// by one worker, every verdict checked against the vector's own recorded
// expectation, every score exact and every metric inside 1e-9.

// mixedFixture is one run in the population: which vector it is built from,
// which log actually goes into the database, and what the log is meant to be.
type mixedFixture struct {
	vector string
	// logVersion is the wire version the stored log declares. It is asserted
	// against the bytes, not trusted — a fixture that silently stopped being v2
	// would turn this whole test back into the v1-only coverage it replaces.
	logVersion int
	telemetry  bool
	// rewrite optionally edits the vector's log before it is stored.
	rewrite func(t *testing.T, log json.RawMessage) json.RawMessage
	why     string
}

// mixedPopulation is deliberately INTERLEAVED, not grouped by version: the
// worker claims runs in batches, and a queue that only ever sees a version
// change between batches is not the queue this exists to cover.
var mixedPopulation = []mixedFixture{
	{vector: "words-clean", logVersion: 1,
		why: "the baseline v1 run"},
	{vector: "words-telemetry-v2", logVersion: 2, telemetry: true,
		why: "v2 carrying down/up telemetry"},
	{vector: "time-clean", logVersion: 1,
		why: "v1, time mode — the other dimension field"},
	{vector: "words-telemetry-unpaired-keyup", logVersion: 2, telemetry: true,
		why: "v2 whose telemetry raises a flag that must not change the verdict"},
	{vector: "words-typos-v1", logVersion: 1,
		why: "v1 on the OLD score formula (scoreVersion 1) — the oldest thing in the queue"},
	{vector: "words-telemetry-stripped-v1", logVersion: 1,
		why: "the capability flip: this device reported v1, and its v2 twin is in this same population with identical numbers"},
	{vector: "words-one-fast-interval", logVersion: 1,
		why: "v1 accepted WITH a flag — the false-positive boundary"},
	{vector: "words-telemetry-stripped-v1", logVersion: 2, rewrite: declareLogVersion(2),
		why: "v2 with NO telemetry: the same device after capability detection flipped, before it ever emits a down/up. v2's telemetry is optional, so this is a legal log and must judge identically to the v1 twin above"},
	{vector: "words-bot-cadence", logVersion: 1,
		why: "v1 that must come out FLAGGED — a population where everything is accepted proves nothing about the verdict path"},
	{vector: "words-mods", logVersion: 1,
		why: "v1 with text mods"},
	{vector: "words-telemetry-v2", logVersion: 2, rewrite: stripTelemetry,
		why: "the v2 telemetry vector with its down/up events removed in place: same wire version, same keystrokes, no telemetry"},
	{vector: "words-rejected-backspace", logVersion: 1,
		why: "v1 with a seq hole the reducer rejected"},
	{vector: "words-consistency-chars", logVersion: 1,
		why: "v1 pinning the server-only metrics"},
	{vector: "words-nospace-space-presses", logVersion: 1,
		why: "v1 under the nospace commit guard"},
}

// TestWorkerDrainsAMixedV1V2Population is the integration test for the mixed
// population: fourteen runs of both wire versions interleaved in one queue,
// drained by one worker in batches smaller than the population, each judged to
// the verdict its vector records.
func TestWorkerDrainsAMixedV1V2Population(t *testing.T) {
	pool := newPool(t)
	user := seedUser(t, pool)

	require.GreaterOrEqual(t, len(mixedPopulation), 10,
		"a mixed-population test needs a population")

	type seeded struct {
		mixedFixture
		id     uuid.UUID
		vector vectorFile
		log    json.RawMessage
	}

	runs := make([]seeded, 0, len(mixedPopulation))
	for i, f := range mixedPopulation {
		v := loadVector(t, f.vector)
		log := v.Payload.Log
		if f.rewrite != nil {
			log = f.rewrite(t, log)
		}

		// The fixture's claim about the log is checked against the bytes that
		// are about to be stored, before anything is judged.
		assert.Equal(t, f.logVersion, logVersionOf(t, log),
			"fixture %d (%s): stored log is not the version the fixture claims", i, f.vector)
		assert.Equal(t, f.telemetry, hasTelemetry(t, log),
			"fixture %d (%s): stored log's telemetry is not what the fixture claims", i, f.vector)

		runs = append(runs, seeded{
			mixedFixture: f,
			id:           insertPendingLog(t, pool, user, v, log),
			vector:       v,
			log:          log,
		})
	}

	// The population must actually be mixed, or this test is the v1-only
	// coverage it was written to replace.
	var v1, v2, v2Telemetry, v2Bare int
	for _, r := range runs {
		switch {
		case r.logVersion == 1:
			v1++
		case r.telemetry:
			v2, v2Telemetry = v2+1, v2Telemetry+1
		default:
			v2, v2Bare = v2+1, v2Bare+1
		}
	}
	require.Positive(t, v1, "no v1 runs in the mixed population")
	require.Positive(t, v2Telemetry, "no v2-with-telemetry runs in the mixed population")
	require.Positive(t, v2Bare, "no v2-without-telemetry runs in the mixed population")
	t.Logf("population: %d × v1, %d × v2 (%d with telemetry, %d without)", v1, v2, v2Telemetry, v2Bare)

	// Batches deliberately smaller than the population: the worker has to come
	// back for more, and every batch is a fresh mix of both versions.
	const batchSize = 4
	w := newTestWorker(t, replaypg.New(pool, nil), replay.WorkerConfig{BatchSize: batchSize})
	core, err := replay.NewCore(replay.DefaultReplayTimeout)
	require.NoError(t, err)

	drained, passes := 0, 0
	for {
		n, err := w.RunBatch(context.Background(), core, discardLogger())
		require.NoError(t, err)
		if n == 0 {
			break
		}
		drained, passes = drained+n, passes+1
		require.Less(t, passes, len(runs)+2, "the worker is not draining the queue")
	}
	assert.Equal(t, len(runs), drained, "the worker did not drain every run")
	assert.Greater(t, passes, 1, "the whole population fit in one batch; it is not testing the drain")

	var pending int
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT count(*) FROM runs WHERE status = 'pending'`).Scan(&pending))
	assert.Zero(t, pending, "runs left pending after the queue drained")

	for i, r := range runs {
		t.Run(fmt.Sprintf("%02d_%s_v%d", i, r.vector.Name, r.logVersion), func(t *testing.T) {
			t.Log(r.why)
			row := fetchRun(t, pool, r.id)

			require.Equal(t, r.vector.Expect.Status, row.Status, "validation: %s", row.Validation)

			var doc struct {
				Verdict string        `json:"verdict"`
				Reason  string        `json:"reason"`
				Flags   []replay.Flag `json:"flags"`
			}
			require.NoError(t, json.Unmarshal(row.Validation, &doc))
			assert.Equal(t, r.vector.Expect.Verdict, doc.Verdict)
			assert.Equal(t, sortedStrings(r.vector.Expect.Flags), codesOf(doc.Flags),
				"the flags the core raised are not the ones the vector recorded")

			// Every judged run records what judged it, whatever its log version.
			require.NotNil(t, row.BundleSha)
			assert.Equal(t, replay.BundleSHA(), *row.BundleSha)
			require.NotNil(t, row.PolicyVersion)
			assert.Equal(t, fakePolicyColumn(t), *row.PolicyVersion)
			require.NotNil(t, row.ValidatedAt)
			assert.Zero(t, row.Attempts, "a clean replay must not burn an attempt")
			assert.Nil(t, row.LastError)

			// The score is an integer out of a single Math.round: it is compared
			// EXACTLY, because a tolerance there would hide the drift this
			// worker exists to detect.
			assert.Equal(t, jsonNumber(t, r.vector.Payload.ClientScore, "total"),
				jsonNumber(t, row.ServerScore, "total"), "score total")

			// The metrics are doubles that made a JSON round trip on both
			// sides, so they get the worker's own 1e-9.
			for _, m := range []struct{ client, server string }{
				{"wpm", "wpm"}, {"raw", "raw"}, {"acc", "accuracy"},
			} {
				assert.InDelta(t,
					jsonNumber(t, r.vector.Payload.ClientMetrics, m.client),
					jsonNumber(t, row.ServerMetrics, m.server),
					1e-9, "metric %s", m.client)
			}
		})
	}

	// The capability flip, stated as an equality: one device's keystrokes judged
	// under v1, under v2-without-telemetry and under v2-with-telemetry produce
	// the same numbers. The wire version is a container, not an input to the
	// arithmetic.
	t.Run("the_wire_version_moves_no_number", func(t *testing.T) {
		byShape := map[string][]byte{}
		for _, r := range runs {
			row := fetchRun(t, pool, r.id)
			key := fmt.Sprintf("%s/v%d/telemetry=%v", r.vector.Name, r.logVersion, r.telemetry)
			byShape[key] = row.ServerScore
			t.Logf("%-56s score %s", key, string(row.ServerScore))
		}

		twins := [][2]string{
			// The same six words, submitted three ways.
			{"words-telemetry-stripped-v1/v1/telemetry=false", "words-telemetry-stripped-v1/v2/telemetry=false"},
			{"words-telemetry-v2/v2/telemetry=true", "words-telemetry-v2/v2/telemetry=false"},
		}
		for _, pair := range twins {
			a, b := byShape[pair[0]], byShape[pair[1]]
			require.NotEmpty(t, a, pair[0])
			require.NotEmpty(t, b, pair[1])
			assert.JSONEq(t, string(a), string(b),
				"%s and %s disagree: the wire version moved a number", pair[0], pair[1])
		}
	})
}

// --- log surgery -------------------------------------------------------------

// declareLogVersion re-declares the log's wire version without touching its
// events — a v1 log becoming a legal v2 log that happens to carry no telemetry.
func declareLogVersion(version int) func(*testing.T, json.RawMessage) json.RawMessage {
	return func(t *testing.T, log json.RawMessage) json.RawMessage {
		t.Helper()
		return withLogFields(t, log, version, nil)
	}
}

// stripTelemetry removes every down/up event from a v2 log and renumbers what
// is left, keeping the log's contiguous-seq invariant. The wire version stays 2:
// telemetry is optional in v2, and this is the shape a v2-capable client
// produces before the first key it observes.
func stripTelemetry(t *testing.T, log json.RawMessage) json.RawMessage {
	t.Helper()
	version, events := decodeLog(t, log)

	kept := make([]json.RawMessage, 0, len(events))
	for _, ev := range events {
		if !isTelemetryEvent(t, ev) {
			kept = append(kept, ev)
		}
	}
	require.Less(t, len(kept), len(events), "nothing was stripped: this log carries no telemetry")

	for i := range kept {
		var fields map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(kept[i], &fields))
		fields["seq"] = json.RawMessage(fmt.Sprintf("%d", i+1))
		out, err := json.Marshal(fields)
		require.NoError(t, err)
		kept[i] = out
	}
	return withLogFields(t, log, version, kept)
}

// --- log inspection ----------------------------------------------------------

func decodeLog(t *testing.T, log json.RawMessage) (int, []json.RawMessage) {
	t.Helper()
	var doc struct {
		Version int               `json:"version"`
		Events  []json.RawMessage `json:"events"`
	}
	require.NoError(t, json.Unmarshal(log, &doc))
	return doc.Version, doc.Events
}

// withLogFields re-encodes a log with a chosen wire version, keeping its events
// unless replacements are supplied.
func withLogFields(t *testing.T, log json.RawMessage, version int, events []json.RawMessage) json.RawMessage {
	t.Helper()
	if events == nil {
		_, events = decodeLog(t, log)
	}
	out, err := json.Marshal(struct {
		Version int               `json:"version"`
		Events  []json.RawMessage `json:"events"`
	}{Version: version, Events: events})
	require.NoError(t, err)
	return out
}

func logVersionOf(t *testing.T, log json.RawMessage) int {
	t.Helper()
	version, _ := decodeLog(t, log)
	return version
}

func hasTelemetry(t *testing.T, log json.RawMessage) bool {
	t.Helper()
	_, events := decodeLog(t, log)
	for _, ev := range events {
		if isTelemetryEvent(t, ev) {
			return true
		}
	}
	return false
}

// isTelemetryEvent reports whether an event is one of log v2's keystroke
// observations — the kinds the reducer treats as no-ops.
func isTelemetryEvent(t *testing.T, ev json.RawMessage) bool {
	t.Helper()
	var kind struct {
		Kind string `json:"kind"`
	}
	require.NoError(t, json.Unmarshal(ev, &kind))
	return kind.Kind == "down" || kind.Kind == "up"
}

// --- small helpers -----------------------------------------------------------

func jsonNumber(t *testing.T, raw []byte, field string) float64 {
	t.Helper()
	var doc map[string]any
	require.NoError(t, json.Unmarshal(raw, &doc))
	v, ok := doc[field]
	require.True(t, ok, "no %q in %s", field, string(raw))
	n, ok := v.(float64)
	require.True(t, ok, "%q is not a number in %s", field, string(raw))
	require.False(t, math.IsNaN(n), "%q is NaN", field)
	return n
}

func codesOf(flags []replay.Flag) []string {
	out := make([]string, 0, len(flags))
	for _, f := range flags {
		out = append(out, f.Code)
	}
	sort.Strings(out)
	return out
}

func sortedStrings(in []string) []string {
	out := append([]string{}, in...)
	sort.Strings(out)
	if len(out) == 0 {
		return []string{}
	}
	return out
}
