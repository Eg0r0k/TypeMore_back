// Package policytest is the contract every Judge must satisfy, plus a
// deterministic fake to test against.
//
// It exists because the real policy is behind a build tag and may one day be
// behind a private module. Without a contract expressed in the open, "does this
// implementation still behave like a judge" would only be answerable inside the
// build that has one — and the open repo's own tests would have to either
// depend on closed code or assert nothing.
//
// It imports testing, like net/http/httptest. Nothing outside a test may import
// this package, and nothing in the server binary does.
package policytest

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/typemore/typemore-server/internal/replay/policy"
)

// RunContract asserts the properties EVERY judge must have, whatever it decides.
// They are about shape and self-consistency, never about a particular weight —
// a contract that pinned numbers would just be the closed policy written twice.
//
// newJudge returns a fresh judge; it is called repeatedly so a judge that
// accumulated state between calls is caught.
func RunContract(t *testing.T, name string, newJudge func() policy.Judge) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		t.Run("version is storable", func(t *testing.T) {
			v := newJudge().Version()
			require.NotEmpty(t, v, "a judge must identify its rule set")
			col, err := policy.ParseVersion(v)
			require.NoError(t, err, "Version() %q cannot be stored on a run", v)
			if v == policy.VersionNone {
				assert.Equal(t, policy.ColumnNone, col)
			} else {
				assert.Positive(t, col, "a judging policy needs a positive version")
			}
		})

		t.Run("version is stable", func(t *testing.T) {
			j := newJudge()
			first := j.Version()
			for range 3 {
				assert.Equal(t, first, j.Version(), "Version() must not change under a running worker")
			}
			assert.Equal(t, first, newJudge().Version(),
				"two judges built the same way must report the same version")
		})

		t.Run("no flags is no suspicion and no review", func(t *testing.T) {
			for _, flags := range [][]policy.Flag{nil, {}} {
				d := newJudge().Judge(flags, policy.RunMeta{DurationSec: 60})
				assert.Zero(t, d.Suspicion, "a run with nothing against it scored something")
				assert.False(t, d.NeedsReview, "a run with nothing against it was sent to review")
				assert.Empty(t, d.Reasons)
			}
		})

		t.Run("decisions are deterministic", func(t *testing.T) {
			flags := sampleFlags()
			meta := policy.RunMeta{DurationSec: 42.5, ScoreVersion: 2}
			j := newJudge()
			want := j.Judge(flags, meta)
			for range 5 {
				assert.Equal(t, want, j.Judge(flags, meta),
					"the same run judged twice got two answers")
			}
			assert.Equal(t, want, newJudge().Judge(flags, meta),
				"two judges built the same way disagreed about the same run")
		})

		t.Run("the input is not mutated", func(t *testing.T) {
			flags := sampleFlags()
			before := append([]policy.Flag(nil), flags...)
			newJudge().Judge(flags, policy.RunMeta{DurationSec: 30})
			assert.Equal(t, before, flags, "a judge rewrote the flags it was given")
		})

		t.Run("suspicion is a real number and never negative", func(t *testing.T) {
			for _, flags := range flagSets() {
				d := newJudge().Judge(flags, policy.RunMeta{DurationSec: 30})
				assert.False(t, math.IsNaN(d.Suspicion), "suspicion is NaN for %v", codes(flags))
				assert.False(t, math.IsInf(d.Suspicion, 0), "suspicion is infinite for %v", codes(flags))
				assert.GreaterOrEqual(t, d.Suspicion, 0.0,
					"negative suspicion for %v: a flag cannot argue a run is MORE honest", codes(flags))
			}
		})

		t.Run("suspicion does not decrease when a flag is added", func(t *testing.T) {
			// Monotonicity is the property that makes suspicion mean anything.
			// A policy where adding evidence could lower the total would be
			// gameable by raising a harmless flag on purpose.
			j := newJudge()
			base := j.Judge(nil, policy.RunMeta{DurationSec: 30}).Suspicion
			var flags []policy.Flag
			for _, f := range sampleFlags() {
				flags = append(flags, f)
				got := j.Judge(flags, policy.RunMeta{DurationSec: 30}).Suspicion
				assert.GreaterOrEqual(t, got, base, "adding %q lowered suspicion", f.Code)
				base = got
			}
		})

		t.Run("severity zero is worth nothing", func(t *testing.T) {
			j := newJudge()
			var zeroed []policy.Flag
			for _, f := range sampleFlags() {
				zeroed = append(zeroed, policy.Flag{Code: f.Code, Score: 0})
			}
			assert.Zero(t, j.Judge(zeroed, policy.RunMeta{DurationSec: 30}).Suspicion,
				"flags at severity zero produced suspicion; the core says nothing happened")
		})

		t.Run("an unknown code is reported, never guessed at", func(t *testing.T) {
			j := newJudge()
			meta := policy.RunMeta{DurationSec: 30}
			known := sampleFlags()
			withAlien := append(append([]policy.Flag(nil), known...),
				policy.Flag{Code: "telepathy", Score: 1})

			base, alien := j.Judge(known, meta), j.Judge(withAlien, meta)
			assert.Equal(t, base.Suspicion, alien.Suspicion,
				"a code the policy has never heard of moved the suspicion")
			if len(alien.UnknownFlags) > 0 {
				assert.Contains(t, alien.UnknownFlags, "telepathy",
					"the unknown code was neither weighted nor reported")
			}
		})

		t.Run("reasons are stable and reproducible", func(t *testing.T) {
			// Whatever fires, it fires the same way twice and carries no
			// duplicates — the ids land in an audit document a human reads.
			j := newJudge()
			flags, meta := sampleFlags(), policy.RunMeta{DurationSec: 600}
			first := j.Judge(flags, meta).Reasons
			assert.Equal(t, first, j.Judge(flags, meta).Reasons)
			seen := map[string]bool{}
			for _, r := range first {
				assert.NotEmpty(t, r, "an empty reason id")
				assert.False(t, seen[r], "reason %q reported twice", r)
				seen[r] = true
			}
		})

		t.Run("a reason means review", func(t *testing.T) {
			// A shape is a routing decision, so a judge that names one and then
			// declines to review is telling the audit trail something it does
			// not act on.
			for _, meta := range []policy.RunMeta{{DurationSec: 1}, {DurationSec: 600}} {
				for _, flags := range flagSets() {
					d := newJudge().Judge(flags, meta)
					if len(d.Reasons) > 0 {
						assert.True(t, d.NeedsReview,
							"reasons %v fired but the run was not routed to review", d.Reasons)
					}
				}
			}
		})

		t.Run("a judge that does not judge does not review", func(t *testing.T) {
			j := newJudge()
			if !policy.IsNoop(j) {
				t.Skip("this judge judges")
			}
			for _, flags := range flagSets() {
				d := j.Judge(flags, policy.RunMeta{DurationSec: 600})
				assert.Zero(t, d.Suspicion)
				assert.False(t, d.NeedsReview, "a version-%q judge sent a run to review", policy.VersionNone)
				assert.Empty(t, d.Reasons)
			}
		})
	})
}

// sampleFlags is a spread of codes and severities: real codes the core emits,
// at severities from negligible to maximal.
func sampleFlags() []policy.Flag {
	return []policy.Flag{
		{Code: "min-interval", Score: 0.02, Detail: "1/82 intervals < 15ms"},
		{Code: "superhuman-burst", Score: 0.55},
		{Code: "uniform-intervals", Score: 1},
		{Code: "zero-variance", Score: 1},
		{Code: "afk-heavy", Score: 0.93},
		{Code: "unpaired-keyup", Score: 1},
	}
}

// flagSets is a handful of shapes to run a property over: nothing, one weak
// flag, the bot-shaped pair, and everything at once.
func flagSets() [][]policy.Flag {
	all := sampleFlags()
	return [][]policy.Flag{
		nil,
		{{Code: "min-interval", Score: 0.02}},
		{{Code: "uniform-intervals", Score: 1}, {Code: "zero-variance", Score: 1}},
		{{Code: "superhuman-burst", Score: 1}},
		{{Code: "unpaired-keyup", Score: 1}},
		all,
	}
}

func codes(flags []policy.Flag) []string {
	out := make([]string, 0, len(flags))
	for _, f := range flags {
		out = append(out, f.Code)
	}
	return out
}
