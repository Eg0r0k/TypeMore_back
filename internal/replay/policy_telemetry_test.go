package replay

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Log v2's telemetry is a STRUCTURAL layer. The pairing flags it raises are
// collected, stored and visible to moderation, and they are worth nothing at
// all: there is no calibration data for v2, and an unpaired keyup is something
// honest players produce routinely (alt-tab holding a key, a layout switch, a
// window that lost focus). These tests are the contract that keeps a weight off
// them until that changes deliberately.

// Every telemetry code carries a weight entry — so it is not reported as an
// unknown flag — and that entry is exactly zero.
func TestTelemetryFlagsAreCollectedNotJudged(t *testing.T) {
	p := DefaultPolicy()
	require.NotEmpty(t, TelemetryOnlyFlags)

	for _, code := range TelemetryOnlyFlags {
		w, known := p.Weights[code]
		assert.True(t, known, "telemetry flag %q has no weights entry: it would surface as unknown", code)
		assert.Zero(t, w, "telemetry flag %q carries a weight — scored telemetry heuristics are a later phase", code)
	}
}

// The arithmetic, at any count and any severity: telemetry flags do not move
// suspicion by a single bit, and they do not participate in a shape rule.
func TestTelemetryFlagsMoveSuspicionByNothing(t *testing.T) {
	p := DefaultPolicy()

	// A baseline of real, weighted flags — the point is that the telemetry
	// flags do not perturb an existing suspicion either, not just that they sum
	// to zero on their own.
	base := []Flag{
		{Code: FlagMinInterval, Score: 0.5},
		{Code: FlagSuperhumanBurst, Score: 0.5},
	}
	want := p.Suspicion(base)

	for _, n := range []int{1, 2, 10, 1000} {
		for _, code := range TelemetryOnlyFlags {
			flags := append([]Flag{}, base...)
			for range n {
				flags = append(flags, Flag{Code: code, Score: 1.0})
			}
			assert.Equal(t, want, p.Suspicion(flags),
				"%d × %s moved suspicion", n, code)
			assert.Empty(t, p.UnknownFlagCodes(flags),
				"%s must be a known code, not an unknown one", code)
			assert.Empty(t, p.Combinations(flags, nil),
				"%s took part in a combination rule", code)
		}
	}
}

// The end-to-end shape of the same promise, on a real log the core actually
// judges: a run with N unpaired key releases replays to the SAME numbers as the
// run without them, scores zero suspicion, and is accepted with the flag kept.
//
// The vector is the log-v2 telemetry vector, which is clean; the releases are
// injected here so the count can be varied. Telemetry events are no-ops in the
// reducer, so nothing about the score or the metrics may move — if this test
// ever sees them move, the no-op property broke, which is a bigger finding than
// the flag weight.
func TestUnpairedKeyupsAreAcceptedAtAnyCount(t *testing.T) {
	v := firstVector(t, "words-telemetry-v2")

	clean := judgeOne(t, v.pendingRun(t))
	require.Equal(t, StatusAccepted, clean.Status, "validation: %s", clean.Validation)
	cleanDoc := audit(t, clean)
	require.NotNil(t, cleanDoc.Policy)
	assert.NotContains(t, flagCodes(cleanDoc.Flags), FlagUnpairedKeyup,
		"the baseline vector already has unpaired releases; it cannot isolate them")

	for _, n := range []int{1, 5, 50} {
		t.Run(fmt.Sprintf("%d_unpaired", n), func(t *testing.T) {
			run := v.pendingRun(t)
			run.Log = gzipJSON(t, withUnpairedKeyups(t, v.Payload.Log, n))

			d := judgeOne(t, run)
			require.Equal(t, StatusAccepted, d.Status, "validation: %s", d.Validation)

			doc := audit(t, d)
			assert.Equal(t, verdictValid, doc.Verdict)
			assert.Empty(t, doc.Reason)
			assert.Contains(t, flagCodes(doc.Flags), FlagUnpairedKeyup,
				"the flag must still be raised and stored: it is telemetry, not silence")

			require.NotNil(t, doc.Policy)
			assert.Equal(t, cleanDoc.Policy.Suspicion, doc.Policy.Suspicion,
				"%d unpaired releases moved suspicion", n)
			assert.Empty(t, doc.Policy.Rules)
			assert.Empty(t, doc.Policy.UnknownFlags)

			// The releases are inert for the reducer too, so the verdict rests
			// on identical numbers.
			assert.JSONEq(t, string(clean.ServerScore), string(d.ServerScore))
			assert.JSONEq(t, string(clean.ServerMetrics), string(d.ServerMetrics))
		})
	}
}

// withUnpairedKeyups returns the log with n key releases that were never pressed
// spliced in at the front, and every seq renumbered to stay contiguous — the
// log's own structural invariant, which the core enforces before it looks at
// anything else.
//
// The front is where an honest one comes from: a key held across the start of
// the log is released into a reducer that never saw it go down.
func withUnpairedKeyups(t *testing.T, raw json.RawMessage, n int) json.RawMessage {
	t.Helper()

	var log struct {
		Version int               `json:"version"`
		Events  []json.RawMessage `json:"events"`
	}
	require.NoError(t, json.Unmarshal(raw, &log))
	require.Equal(t, 2, log.Version, "unpaired releases only exist in log v2")

	// Distinct codes so each release is its own unpaired key rather than one
	// key released n times — the alt-tab-with-a-chord shape.
	codes := []string{"ShiftLeft", "ControlLeft", "AltLeft", "MetaLeft", "ShiftRight"}
	injected := make([]json.RawMessage, 0, n+len(log.Events))
	for i := range n {
		injected = append(injected, json.RawMessage(fmt.Sprintf(
			`{"kind":"up","seq":0,"t":0,"code":%q}`, codes[i%len(codes)])))
	}
	injected = append(injected, log.Events...)

	for i := range injected {
		var ev map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(injected[i], &ev))
		ev["seq"] = json.RawMessage(fmt.Sprintf("%d", i+1))
		out, err := json.Marshal(ev)
		require.NoError(t, err)
		injected[i] = out
	}

	out, err := json.Marshal(struct {
		Version int               `json:"version"`
		Events  []json.RawMessage `json:"events"`
	}{Version: log.Version, Events: injected})
	require.NoError(t, err)
	return out
}
