package replay

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/typemore/typemore-server/internal/replay/policy"
	"github.com/typemore/typemore-server/internal/replay/policy/policytest"
)

// This is what the open repo keeps after the review policy moved behind an
// interface: the proof that a HARD verdict is not the policy's to make.
//
// Everything the product needs to work stays here and stays open — regenerating
// the words, folding the log through the same core the browser ran, recomputing
// the score and the metrics with exact equality, and refusing what is
// structurally wrong. An instance built without a policy still does all of it.
// The tests below are what makes that a fact rather than an intention.

// judges spans the whole space an implementation can occupy: one that judges
// nothing, one that judges normally, one that would review every run it sees,
// and one that would review none. If a verdict is the same across all four, it
// did not come from a judge.
func judges() []struct {
	name  string
	judge policy.Judge
} {
	return []struct {
		name  string
		judge policy.Judge
	}{
		{"noop", policy.Noop{}},
		{"fake", policytest.NewFake()},
		{"reviews everything", policytest.NewFakeWithThreshold(0)},
		{"reviews nothing", policytest.NewFakeWithThreshold(1e9)},
	}
}

// TestHardVerdictsDoNotDependOnTheJudge is the tamper matrix, re-aimed.
//
// It used to prove that the review policy's weights could not downgrade a hard
// check; it now proves the stronger thing the interface needs to be true — that
// the hard checks do not consult a judge AT ALL. An edited log, a faked score, a
// nudged metric, an unplayable dictionary: each is caught identically whether
// the instance runs a policy, runs none, or runs one tuned to absurdity in
// either direction.
//
// That is the property that makes the policy removable. Without it, a
// self-hosted instance would not merely be unprotected against cheating — it
// would be WRONG about runs, which is a different and much worse thing.
func TestHardVerdictsDoNotDependOnTheJudge(t *testing.T) {
	clean := firstVector(t, "words-clean")

	tests := []struct {
		name       string
		tamper     func(t *testing.T, v vector, run *PendingRun) PendingRun
		wantStatus string
		wantReason string
	}{
		{
			name: "edited event (seq hole)",
			tamper: func(t *testing.T, v vector, run *PendingRun) PendingRun {
				var log struct {
					Version int               `json:"version"`
					Events  []json.RawMessage `json:"events"`
				}
				require.NoError(t, json.Unmarshal(v.Payload.Log, &log))
				log.Events = append(log.Events[:5], log.Events[6:]...)
				edited, err := json.Marshal(log)
				require.NoError(t, err)
				run.Log = gzipJSON(t, edited)
				return *run
			},
			wantStatus: StatusRejected,
		},
		{
			name: "inflated client score",
			tamper: func(t *testing.T, v vector, run *PendingRun) PendingRun {
				run.ClientScore = withNumber(t, v.Payload.ClientScore, "total",
					func(v float64) float64 { return v * 10 })
				return *run
			},
			wantStatus: StatusFlagged,
			wantReason: ReasonScoreMismatch,
		},
		{
			name: "nudged client metric",
			tamper: func(t *testing.T, v vector, run *PendingRun) PendingRun {
				run.ClientMetrics = withNumber(t, v.Payload.ClientMetrics, "wpm",
					func(v float64) float64 { return v + 1e-6 })
				return *run
			},
			wantStatus: StatusFlagged,
			wantReason: ReasonMetricMismatch,
		},
		{
			name: "unpublished dictionary",
			tamper: func(_ *testing.T, _ vector, run *PendingRun) PendingRun {
				run.DictHash = "deadbeef"
				return *run
			},
			wantStatus: StatusFlagged,
			wantReason: ReasonUnknownDict,
		},
		{
			name: "unroutable score version",
			tamper: func(_ *testing.T, _ vector, run *PendingRun) PendingRun {
				run.ScoreVersion = 99
				return *run
			},
			wantStatus: StatusFlagged,
			wantReason: ReasonReplayError,
		},
		{
			name: "malformed setup",
			tamper: func(_ *testing.T, _ vector, run *PendingRun) PendingRun {
				run.Setup = json.RawMessage(`{"config":null,"generation":null,"declaration":{}}`)
				return *run
			},
			wantStatus: StatusFlagged,
			wantReason: ReasonReplayError,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for _, j := range judges() {
				t.Run(j.name, func(t *testing.T) {
					run := clean.pendingRun(t)
					run = tc.tamper(t, clean, &run)

					d := judgeOneWithPolicy(t, run, j.judge)
					require.Equal(t, tc.wantStatus, d.Status, "validation: %s", d.Validation)

					doc := audit(t, d)
					if tc.wantReason != "" {
						assert.Equal(t, tc.wantReason, doc.Reason)
					} else {
						assert.NotEmpty(t, doc.Reason)
					}
					// The reason is the hard one, never a policy routing code:
					// a run refused for its structure must not be recorded as
					// merely suspicious.
					assert.NotEqual(t, ReasonSuspicionThreshold, doc.Reason)
					assert.NotEqual(t, ReasonBotPattern, doc.Reason)
				})
			}
		})
	}
}

// The clean run is the other half: a run nothing is wrong with is ACCEPTED under
// every judge except one that has been told to review everything. A matrix that
// only showed refusals surviving would also pass if the worker refused
// everything.
func TestACleanRunIsAcceptedWhateverJudges(t *testing.T) {
	clean := firstVector(t, "words-clean")

	for _, j := range judges() {
		t.Run(j.name, func(t *testing.T) {
			d := judgeOneWithPolicy(t, clean.pendingRun(t), j.judge)
			require.Equal(t, StatusAccepted, d.Status, "validation: %s", d.Validation)

			doc := audit(t, d)
			assert.Equal(t, verdictValid, doc.Verdict)
			assert.Empty(t, doc.Reason)
			// The numbers are the server's own and are recorded regardless.
			assert.NotEmpty(t, d.ServerScore)
			assert.NotEmpty(t, d.ServerMetrics)
			assert.NotEmpty(t, d.BundleSHA)
		})
	}
}

// --- what a judge DOES decide -------------------------------------------------

// The judge owns routing, and the two reason codes stay distinct: a run routed
// because a SHAPE fired is recorded differently from one routed because a
// magnitude crossed a line. Both come from the same fake, so this asserts the
// worker's wiring rather than any real rule.
func TestTheJudgeOwnsReviewRouting(t *testing.T) {
	t.Run("a shape routes as a bot pattern", func(t *testing.T) {
		// The fake fires its one shape rule on uniform+zero-variance, which is
		// what this vector raises.
		d := judgeOne(t, firstVector(t, "words-bot-cadence").pendingRun(t))
		require.Equal(t, StatusFlagged, d.Status, "validation: %s", d.Validation)

		doc := audit(t, d)
		assert.Equal(t, verdictValid, doc.Verdict, "the log is structurally fine; the cadence is not")
		assert.Equal(t, ReasonBotPattern, doc.Reason)
		require.NotNil(t, doc.Policy)
		assert.Equal(t, []string{policytest.FakeShapeRule}, doc.Policy.Rules)
		// A flagged run is reviewable, not discarded.
		assert.NotEmpty(t, d.ServerScore)
		assert.NotEmpty(t, d.ServerMetrics)
	})

	t.Run("a magnitude routes as a threshold", func(t *testing.T) {
		// No shape rule can fire on a single weak flag, so a review here can
		// only have come from the threshold.
		d := judgeOneWithPolicy(t, firstVector(t, "words-one-fast-interval").pendingRun(t),
			policytest.NewFakeWithThreshold(1e-9))
		require.Equal(t, StatusFlagged, d.Status, "validation: %s", d.Validation)

		doc := audit(t, d)
		assert.Equal(t, ReasonSuspicionThreshold, doc.Reason)
		assert.Empty(t, doc.Policy.Rules)
	})

	t.Run("a weak flag below the line is accepted and kept", func(t *testing.T) {
		// The regression the policy existed to fix, restated against the seam:
		// ordinary key rollover must not reach review, AND the flag must survive
		// so moderation can still see it.
		d := judgeOne(t, firstVector(t, "words-one-fast-interval").pendingRun(t))
		require.Equal(t, StatusAccepted, d.Status, "validation: %s", d.Validation)

		doc := audit(t, d)
		assert.Equal(t, []string{"min-interval"}, flagCodes(doc.Flags), "the flag must still be recorded")
		assert.Empty(t, doc.Reason)
		require.NotNil(t, doc.Policy)
		assert.Positive(t, doc.Policy.Suspicion, "the flag should still contribute something")
		assert.Less(t, doc.Policy.Suspicion, doc.Policy.Threshold)
	})
}

// --- the instance with no policy ----------------------------------------------

// A run judged without a policy has to stay TELLABLE from one judged with a
// policy, forever. That is the whole reason a version reaches the database on
// every decision: an instance that later turns anti-cheat on must be able to
// find the runs that were never judged, and `revalidate` finds them by their
// NULL policy_version.
//
// So a Noop verdict writes no policy block and no version — not a block full of
// zeroes, which would read as "a policy looked and found nothing".
func TestAnUnjudgedRunIsMarkedAsUnjudged(t *testing.T) {
	clean := firstVector(t, "words-clean")

	noop := judgeOneWithPolicy(t, clean.pendingRun(t), policy.Noop{})
	require.Equal(t, StatusAccepted, noop.Status)
	assert.Equal(t, policy.ColumnNone, noop.PolicyVersion,
		"a run judged by no policy must not claim one")
	assert.Nil(t, audit(t, noop).Policy,
		"a judge that judged nothing wrote an audit block saying it found nothing")

	// …and the same run under a judge IS marked, with that judge's version.
	judged := judgeOne(t, clean.pendingRun(t))
	assert.Equal(t, fakePolicyColumn(t), judged.PolicyVersion)
	require.NotNil(t, audit(t, judged).Policy)
	assert.NotEqual(t, noop.PolicyVersion, judged.PolicyVersion,
		"an open verdict and a judged verdict are indistinguishable in the database")

	// The bundle is recorded either way: the code that produced the NUMBERS is
	// not the policy's business, and revalidate keys on it too.
	assert.Equal(t, bundleSHA, noop.BundleSHA)
	assert.Equal(t, bundleSHA, judged.BundleSHA)
}

// The worker refuses to be built around a judge whose version cannot be stored.
// Failing here means failing at startup; the alternative is discovering it one
// run at a time, in a column that is supposed to be the record of who judged
// what.
func TestADeciderRefusesAnUnstorableVersion(t *testing.T) {
	for _, version := range []string{"", "v2", "1.5", "0", "-3", "40000", "none-ish"} {
		_, err := NewDecider(brokenJudge(version))
		assert.Error(t, err, "version %q was accepted", version)
	}
	for _, version := range []string{policy.VersionNone, "1", "2", "32767"} {
		_, err := NewDecider(brokenJudge(version))
		assert.NoError(t, err, "version %q was refused", version)
	}
	// A nil judge is the open default, not a crash.
	d, err := NewDecider(nil)
	require.NoError(t, err)
	assert.True(t, policy.IsNoop(d.Judge()))
}

// brokenJudge is a judge that exists only to report a version.
type brokenJudge string

func (brokenJudge) Judge([]policy.Flag, policy.RunMeta) policy.Decision {
	return policy.Decision{}
}
func (j brokenJudge) Version() string { return string(j) }

// --- the contract every judge must pass ---------------------------------------

// The open implementations, against the contract. The real policy runs the same
// set from behind its build tag, so "is this still a judge" has one answer that
// does not depend on which build is being asked.
func TestOpenJudgesSatisfyTheContract(t *testing.T) {
	policytest.RunContract(t, "noop", func() policy.Judge { return policy.Noop{} })
	policytest.RunContract(t, "fake", func() policy.Judge { return policytest.NewFake() })
	policytest.RunContract(t, "fake/reviews-everything",
		func() policy.Judge { return policytest.NewFakeWithThreshold(0) })
	policytest.RunContract(t, "fake/reviews-nothing",
		func() policy.Judge { return policytest.NewFakeWithThreshold(1e9) })
}

// withNumber rewrites one numeric field of a JSON object.
func withNumber(t *testing.T, raw json.RawMessage, field string, f func(float64) float64) json.RawMessage {
	t.Helper()
	var obj map[string]any
	require.NoError(t, json.Unmarshal(raw, &obj))
	cur, ok := obj[field].(float64)
	require.True(t, ok, "field %q is not a number", field)
	obj[field] = f(cur)
	out, err := json.Marshal(obj)
	require.NoError(t, err)
	return out
}
