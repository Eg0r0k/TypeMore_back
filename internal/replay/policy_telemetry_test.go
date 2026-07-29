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
// window that lost focus).
//
// The ARITHMETIC half of that promise — that every telemetry code carries a
// weights entry and that entry is exactly zero — is a property of the policy and
// lives with it, behind the build tag. What stays here is the half that is a
// property of the WORKER and holds for any judge that does not weight telemetry:
// however many unpaired releases a log carries, the run it belongs to comes back
// with the same numbers and the same verdict, and the flag is still recorded.

// unpairedKeyupCode is the core's flag code, written out as the literal the core
// emits: this test is about what the WORKER does with it, so it must not borrow
// a constant from a policy that may not be compiled in.
const unpairedKeyupCode = "unpaired-keyup"

// A run with N unpaired key releases replays to the SAME numbers as the run
// without them, scores the same suspicion, and is accepted with the flag kept.
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
	assert.NotContains(t, flagCodes(cleanDoc.Flags), unpairedKeyupCode,
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
			assert.Contains(t, flagCodes(doc.Flags), unpairedKeyupCode,
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
