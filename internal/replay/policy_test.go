package replay

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- the policy this phase exists to fix -------------------------------------

// The regression that motivated the whole policy: under "any flag ⇒ flagged",
// ordinary key rollover — one or two intervals under 15 ms in a hundred — sent
// real runs to review. It must not any more, AND the flag must survive so
// moderation can still see it.
func TestSingleWeakFlagIsAcceptedWithTheFlagKept(t *testing.T) {
	v := firstVector(t, "words-one-fast-interval")
	d := judgeOne(t, v.pendingRun(t))

	require.Equal(t, StatusAccepted, d.Status, "validation: %s", d.Validation)

	doc := audit(t, d)
	assert.Equal(t, []string{FlagMinInterval}, flagCodes(doc.Flags), "the flag must still be recorded")
	assert.Empty(t, doc.Reason)

	require.NotNil(t, doc.Policy)
	assert.Less(t, doc.Policy.Suspicion, DefaultReviewThreshold,
		"one rollover interval reached the review threshold")
	assert.Positive(t, doc.Policy.Suspicion, "the flag should still contribute something")
	assert.Empty(t, doc.Policy.Rules)

	t.Logf("suspicion %.6f (threshold %.2f), flags %v",
		doc.Policy.Suspicion, doc.Policy.Threshold, flagCodes(doc.Flags))
}

// The other half of the contract: a log no hand produces still goes to review,
// and it does so on the SHAPE (uniform + zero variance together), not because
// any single severity crossed a line.
func TestBotCadenceIsFlagged(t *testing.T) {
	v := firstVector(t, "words-bot-cadence")
	d := judgeOne(t, v.pendingRun(t))

	require.Equal(t, StatusFlagged, d.Status)

	doc := audit(t, d)
	assert.Equal(t, verdictValid, doc.Verdict, "the log is structurally fine; the cadence is not")
	assert.Equal(t, ReasonBotPattern, doc.Reason)
	require.NotNil(t, doc.Policy)
	assert.Contains(t, doc.Policy.Rules, RuleBotCadence)
	assert.Subset(t, flagCodes(doc.Flags), []string{FlagUniformIntervals, FlagZeroVariance})

	// The server's numbers are still recorded — a flagged run is reviewable,
	// not discarded.
	assert.NotEmpty(t, d.ServerScore)
	assert.NotEmpty(t, d.ServerMetrics)

	t.Logf("suspicion %.6f, rules %v, flags %v",
		doc.Policy.Suspicion, doc.Policy.Rules, flagCodes(doc.Flags))
}

// The combination rule is a shape, so it must fire even if every weight is
// zeroed — no amount of tuning should be able to hide it.
func TestBotCadenceFiresEvenWithZeroWeights(t *testing.T) {
	p := DefaultPolicy()
	for code := range p.Weights {
		p.Weights[code] = 0
	}
	d := judgeOneWithPolicy(t, firstVector(t, "words-bot-cadence").pendingRun(t), p)

	require.Equal(t, StatusFlagged, d.Status)
	doc := audit(t, d)
	assert.Equal(t, ReasonBotPattern, doc.Reason)
	assert.Zero(t, doc.Policy.Suspicion, "weights were zeroed; only the shape rule should be left")
}

// AFK is a bad run, not a suspicious one: idle time costs the player their own
// score and buys them nothing, so it must not fill the review queue.
func TestAfkFlagsDoNotReachReview(t *testing.T) {
	p := DefaultPolicy()

	// The worst AFK case the real data produced: afk-heavy at 0.935 alongside a
	// trailing tail at 0.685 (see the calibration output in docs/REPLAY.md).
	worst := []Flag{
		{Code: FlagAfkHeavy, Score: 1.0},
		{Code: FlagTrailingAfk, Score: 1.0},
	}
	suspicion := p.Suspicion(worst)
	assert.Less(t, suspicion, p.ReviewThreshold,
		"a maximally idle run must not reach review on idleness alone")
	assert.Empty(t, p.Combinations(worst, nil))

	// Non-zero on purpose: a run that is both idle AND otherwise suspicious
	// should still tip a little further.
	assert.Positive(t, suspicion)
	t.Logf("maximally idle run scores %.4f against a %.2f threshold", suspicion, p.ReviewThreshold)
}

// --- suspicion arithmetic -----------------------------------------------------

func TestSuspicionIsWeightedSeverity(t *testing.T) {
	p := DefaultPolicy()
	flags := []Flag{
		{Code: FlagMinInterval, Score: 0.5},     // 0.30 × 0.5 = 0.15
		{Code: FlagSuperhumanBurst, Score: 0.5}, // 0.80 × 0.5 = 0.40
	}
	assert.InDelta(t, 0.55, p.Suspicion(flags), 1e-12)
	assert.Zero(t, p.Suspicion(nil))
}

// A flag the weights table has never heard of contributes nothing AND is
// reported: a bundle that grows a new signal must not silently disarm review.
func TestUnknownFlagCodeIsReportedNotGuessed(t *testing.T) {
	p := DefaultPolicy()
	flags := []Flag{{Code: "telepathy", Score: 1}, {Code: FlagPaste, Score: 0.5}}

	assert.InDelta(t, 0.4, p.Suspicion(flags), 1e-12, "the unknown code must contribute nothing")
	assert.Equal(t, []string{"telepathy"}, p.UnknownFlagCodes(flags))
}

// Every code the core can emit needs a weight, or the table has a hole.
func TestWeightsTableCoversEveryCoreFlag(t *testing.T) {
	emitted := []string{
		FlagMultiGraphemeInsert, FlagPaste, FlagMinInterval, FlagUniformIntervals,
		FlagZeroVariance, FlagSuperhumanBurst, FlagAfkHeavy, FlagTrailingAfk,
		FlagUnpairedKeyup, // log v2 telemetry pairing sanity
	}
	p := DefaultPolicy()
	for _, code := range emitted {
		_, ok := p.Weights[code]
		assert.True(t, ok, "no weight for core flag %q", code)
	}
	assert.Len(t, p.Weights, len(emitted), "the weights table has entries the core never emits")
}

// --- combination rules --------------------------------------------------------

func TestSustainedSuperhumanNeedsTheDurationFloor(t *testing.T) {
	p := DefaultPolicy()
	burst := []Flag{{Code: FlagSuperhumanBurst, Score: 0.6}}

	short := json.RawMessage(fmt.Sprintf(`{"durationSec":%v}`, p.SustainedBurstSec-0.1))
	assert.Empty(t, p.Combinations(burst, short), "a brief flurry is not evidence")

	long := json.RawMessage(fmt.Sprintf(`{"durationSec":%v}`, p.SustainedBurstSec))
	assert.Equal(t, []string{RuleSustainedSuperhuman}, p.Combinations(burst, long))

	// Unreadable metrics make the rule abstain rather than fire on a guess.
	assert.Empty(t, p.Combinations(burst, nil))
	assert.Empty(t, p.Combinations(burst, json.RawMessage(`not json`)))
}

func TestBotCadenceNeedsBothFlags(t *testing.T) {
	p := DefaultPolicy()
	assert.Empty(t, p.Combinations([]Flag{{Code: FlagUniformIntervals, Score: 1}}, nil))
	assert.Empty(t, p.Combinations([]Flag{{Code: FlagZeroVariance, Score: 1}}, nil))
	assert.Equal(t, []string{RuleBotCadence}, p.Combinations([]Flag{
		{Code: FlagUniformIntervals, Score: 1},
		{Code: FlagZeroVariance, Score: 1},
	}, nil))
}

// --- configuration -------------------------------------------------------------

func TestWeightOverridesApply(t *testing.T) {
	p, err := DefaultPolicy().WithOverrides("min-interval=0.9, paste=0.1", 2.5, 30)
	require.NoError(t, err)

	assert.InDelta(t, 0.9, p.Weights[FlagMinInterval], 1e-12)
	assert.InDelta(t, 0.1, p.Weights[FlagPaste], 1e-12)
	assert.InDelta(t, DefaultFlagWeights[FlagZeroVariance], p.Weights[FlagZeroVariance], 1e-12,
		"unlisted codes keep their default")
	assert.InDelta(t, 2.5, p.ReviewThreshold, 1e-12)
	assert.InDelta(t, 30.0, p.SustainedBurstSec, 1e-12)

	// The default policy is untouched: overrides copy, never mutate.
	assert.InDelta(t, DefaultFlagWeights[FlagMinInterval], DefaultPolicy().Weights[FlagMinInterval], 1e-12)
}

func TestEmptyOverridesKeepTheDefaults(t *testing.T) {
	p, err := DefaultPolicy().WithOverrides("", 0, 0)
	require.NoError(t, err)
	assert.Equal(t, DefaultPolicy(), p)
}

// A typo in a tuning knob must fail loudly at startup. The alternative — falling
// back to a default — is a check that looks configured and is not.
func TestBadOverridesAreStartupErrors(t *testing.T) {
	for _, spec := range []string{
		"min-intervall=0.5", // typo in the code
		"min-interval",      // no value
		"min-interval=huge", // not a number
		"min-interval=-1",   // negative
	} {
		_, err := DefaultPolicy().WithOverrides(spec, 0, 0)
		assert.Error(t, err, "override %q should be rejected", spec)
	}
}

// --- policy versioning ---------------------------------------------------------

// Every decision carries the rule set that produced it, whatever the outcome —
// including the ones the core never got to judge.
func TestPolicyVersionIsRecordedOnEveryDecision(t *testing.T) {
	clean := firstVector(t, "words-clean")

	unknownDict := clean.pendingRun(t)
	unknownDict.DictHash = "deadbeef"

	badVersion := clean.pendingRun(t)
	badVersion.ScoreVersion = 99

	for name, run := range map[string]PendingRun{
		"accepted":     clean.pendingRun(t),
		"unknown_dict": unknownDict,
		"replay_error": badVersion,
	} {
		t.Run(name, func(t *testing.T) {
			d := judgeOne(t, run)
			assert.Equal(t, CurrentPolicyVersion, d.PolicyVersion)
			assert.Equal(t, bundleSHA, d.BundleSHA)
		})
	}
}

// Lowering the threshold has to change the outcome, or the knob is decorative.
func TestThresholdGovernsReview(t *testing.T) {
	v := firstVector(t, "words-one-fast-interval")

	accepted := judgeOne(t, v.pendingRun(t))
	require.Equal(t, StatusAccepted, accepted.Status)
	suspicion := audit(t, accepted).Policy.Suspicion
	require.Positive(t, suspicion)

	strict, err := DefaultPolicy().WithOverrides("", suspicion/2, 0)
	require.NoError(t, err)
	flagged := judgeOneWithPolicy(t, v.pendingRun(t), strict)

	require.Equal(t, StatusFlagged, flagged.Status)
	assert.Equal(t, ReasonSuspicionThreshold, audit(t, flagged).Reason)
}

// --- the hard checks are untouched --------------------------------------------

// The policy only governs PLAUSIBILITY. Every hard signal — an edited log, a
// faked number, an unreplayable run — must still be caught exactly as before,
// and none of them may depend on suspicion. This matrix is the guard that a
// future weight change cannot quietly downgrade one of them; the logged
// suspicion values are the evidence that they are not reaching their verdict
// through the flag score.
func TestTamperedFixturesStayCaughtUnderThePolicy(t *testing.T) {
	tests := []struct {
		name       string
		tamper     func(t *testing.T, v vector, run *PendingRun) PendingRun
		wantStatus string
		wantReason string // "" = assert only that it is non-empty
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
				run.ClientScore = withNumber(t, v.Payload.ClientScore, "total", func(v float64) float64 { return v * 10 })
				return *run
			},
			wantStatus: StatusFlagged,
			wantReason: ReasonScoreMismatch,
		},
		{
			name: "nudged client metric",
			tamper: func(t *testing.T, v vector, run *PendingRun) PendingRun {
				run.ClientMetrics = withNumber(t, v.Payload.ClientMetrics, "wpm", func(v float64) float64 { return v + 1e-6 })
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

	clean := firstVector(t, "words-clean")
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			run := clean.pendingRun(t)
			run = tc.tamper(t, clean, &run)

			d := judgeOne(t, run)
			require.Equal(t, tc.wantStatus, d.Status, "validation: %s", d.Validation)
			assert.Equal(t, CurrentPolicyVersion, d.PolicyVersion)

			doc := audit(t, d)
			if tc.wantReason != "" {
				assert.Equal(t, tc.wantReason, doc.Reason)
			} else {
				assert.NotEmpty(t, doc.Reason)
			}

			// The decisive point: none of these got there on suspicion. Each is
			// a hard signal, and each would still be caught with the review
			// threshold turned off entirely.
			suspicion := 0.0
			if doc.Policy != nil {
				suspicion = doc.Policy.Suspicion
				assert.Less(t, suspicion, DefaultReviewThreshold,
					"this fixture must be caught by a hard check, not by suspicion")
			}
			t.Logf("%-26s -> %-8s %-20s suspicion %.6f", tc.name, d.Status, doc.Reason, suspicion)

			lax, err := DefaultPolicy().WithOverrides("", 1e9, 1e9)
			require.NoError(t, err)
			relaxed := judgeOneWithPolicy(t, run, lax)
			assert.Equal(t, tc.wantStatus, relaxed.Status,
				"an unreachable threshold must not change a hard verdict")
		})
	}
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
