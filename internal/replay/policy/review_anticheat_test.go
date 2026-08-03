//go:build anticheat

package policy

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The review policy's own tests. They compile only under -tags anticheat,
// alongside the thing they test — so the file moves with the policy if it ever
// leaves this repo for a private module.
//
// What is NOT here: anything about hard verdicts. Those do not depend on this
// package at all, and the proof of that lives in the open repo
// (TestHardVerdictsDoNotDependOnTheJudge), which is where it is useful.

func mustPolicy(t *testing.T, cfg Config) Judge {
	t.Helper()
	j, err := Provide(cfg)
	require.NoError(t, err)
	return j
}

// --- the regression the policy exists to fix ----------------------------------

// Under "any flag ⇒ flagged", ordinary key rollover — one or two intervals under
// 15 ms in a hundred — sent real runs to review. A single weak flag must not.
func TestASingleWeakFlagDoesNotReachReview(t *testing.T) {
	p := mustPolicy(t, Config{})
	d := p.Judge([]Flag{{Code: FlagMinInterval, Score: 0.02}}, RunMeta{DurationSec: 30})

	assert.False(t, d.NeedsReview, "one rollover interval reached the review threshold")
	assert.Positive(t, d.Suspicion, "the flag should still contribute something")
	assert.Less(t, d.Suspicion, d.Threshold)
	assert.Empty(t, d.Reasons)
}

// AFK is a bad run, not a suspicious one: idle time costs the player their own
// score and buys them nothing, so it must not fill the review queue.
func TestAfkFlagsDoNotReachReview(t *testing.T) {
	p := mustPolicy(t, Config{})
	// The worst AFK case the real data produced, taken to its maximum.
	worst := []Flag{{Code: FlagAfkHeavy, Score: 1}, {Code: FlagTrailingAfk, Score: 1}}
	d := p.Judge(worst, RunMeta{DurationSec: 120})

	assert.False(t, d.NeedsReview, "a maximally idle run must not reach review on idleness alone")
	assert.Empty(t, d.Reasons)
	// Non-zero on purpose: a run that is both idle AND otherwise suspicious
	// should still tip a little further.
	assert.Positive(t, d.Suspicion)
	t.Logf("maximally idle run scores %.4f against a %.2f threshold", d.Suspicion, d.Threshold)
}

// Telemetry is COLLECTED, not judged. Every code derived from log v2's keyboard
// telemetry carries a weights entry — so it is never reported as unknown — and
// that entry is exactly zero.
func TestTelemetryFlagsAreCollectedNotJudged(t *testing.T) {
	p := mustPolicy(t, Config{})
	desc, ok := Describe(p)
	require.True(t, ok)
	require.NotEmpty(t, TelemetryOnlyFlags)

	for _, code := range TelemetryOnlyFlags {
		w, known := desc.Weights[code]
		assert.True(t, known, "telemetry flag %q has no weights entry: it would surface as unknown", code)
		assert.Zero(t, w, "telemetry flag %q carries a weight — scored telemetry heuristics are a later phase", code)
	}
}

// The arithmetic, at any count and any severity: telemetry flags do not move
// suspicion by a single bit and take part in no shape rule.
func TestTelemetryFlagsMoveSuspicionByNothing(t *testing.T) {
	p := mustPolicy(t, Config{})
	// A baseline of real, weighted flags — the point is that telemetry does not
	// perturb an existing suspicion either, not just that it sums to zero alone.
	base := []Flag{{Code: FlagMinInterval, Score: 0.5}, {Code: FlagSuperhumanBurst, Score: 0.5}}
	want := p.Judge(base, RunMeta{}).Suspicion

	for _, n := range []int{1, 2, 10, 1000} {
		for _, code := range TelemetryOnlyFlags {
			flags := append([]Flag{}, base...)
			for range n {
				flags = append(flags, Flag{Code: code, Score: 1})
			}
			d := p.Judge(flags, RunMeta{})
			assert.Equal(t, want, d.Suspicion, "%d × %s moved suspicion", n, code)
			assert.Empty(t, d.UnknownFlags, "%s must be a known code, not an unknown one", code)
			assert.Empty(t, d.Reasons, "%s took part in a combination rule", code)
		}
	}
}

// --- canary flags -------------------------------------------------------------

// The direct canary is zero-variance class: an invisible codepoint that exists
// only in the RENDERED text reached the log, and no keyboard, layout or IME
// produces one. A single occurrence must reach review on its own.
func TestCanaryGraphemeReachesReviewAlone(t *testing.T) {
	p := mustPolicy(t, Config{})
	// The core emits score 1 whatever the count — one is the whole signal.
	d := p.Judge([]Flag{{Code: FlagCanaryGrapheme, Score: 1}}, RunMeta{DurationSec: 30})

	assert.True(t, d.NeedsReview, "a rendered-only codepoint in the log must reach review")
	assert.GreaterOrEqual(t, d.Suspicion, d.Threshold)
	assert.Empty(t, d.Reasons, "this is a magnitude, not a shape rule")
}

// The positional canary is evidence, not proof: the core refuses to raise it
// below three hits, and the weight is set so the minimum severity stays under
// the threshold while a saturated one needs one more weak signal to cross.
func TestCanaryCommitScalesWithSeverity(t *testing.T) {
	p := mustPolicy(t, Config{})

	minimum := p.Judge([]Flag{{Code: FlagCanaryCommit, Score: 0.25}}, RunMeta{DurationSec: 30})
	assert.False(t, minimum.NeedsReview, "three positional hits alone must not reach review")
	assert.InDelta(t, 0.15, minimum.Suspicion, 1e-12)

	saturated := p.Judge([]Flag{{Code: FlagCanaryCommit, Score: 1}}, RunMeta{DurationSec: 30})
	assert.False(t, saturated.NeedsReview, "even saturated, the positional flag is not proof by itself")
	assert.InDelta(t, 0.60, saturated.Suspicion, 1e-12)

	// …but it is most of the way there: one ordinary weak signal tips it.
	together := p.Judge([]Flag{
		{Code: FlagCanaryCommit, Score: 1},
		{Code: FlagSuperhumanBurst, Score: 0.5},
	}, RunMeta{DurationSec: 30})
	assert.True(t, together.NeedsReview)
}

// No combination rule was added for the canaries: a shape rule fires whatever
// the weights say, which is exactly the wrong property for a flag whose weight
// is a starting point awaiting calibration on a real armed population.
func TestCanaryFlagsTakePartInNoShapeRule(t *testing.T) {
	p := mustPolicy(t, Config{})
	d := p.Judge([]Flag{
		{Code: FlagCanaryGrapheme, Score: 1},
		{Code: FlagCanaryCommit, Score: 1},
	}, RunMeta{DurationSec: 300})

	assert.Empty(t, d.Reasons)
	assert.Empty(t, d.UnknownFlags, "both codes must be known to the table")
}

// --- suspicion arithmetic -----------------------------------------------------

func TestSuspicionIsWeightedSeverity(t *testing.T) {
	p := mustPolicy(t, Config{})
	d := p.Judge([]Flag{
		{Code: FlagMinInterval, Score: 0.5},     // 0.30 × 0.5 = 0.15
		{Code: FlagSuperhumanBurst, Score: 0.5}, // 0.80 × 0.5 = 0.40
	}, RunMeta{})
	assert.InDelta(t, 0.55, d.Suspicion, 1e-12)
	assert.Zero(t, p.Judge(nil, RunMeta{}).Suspicion)
}

// A flag the weights table has never heard of contributes nothing AND is
// reported: a bundle that grows a new signal must not silently disarm review.
func TestUnknownFlagCodeIsReportedNotGuessed(t *testing.T) {
	p := mustPolicy(t, Config{})
	d := p.Judge([]Flag{{Code: "telepathy", Score: 1}, {Code: FlagPaste, Score: 0.5}}, RunMeta{})

	assert.InDelta(t, 0.4, d.Suspicion, 1e-12, "the unknown code must contribute nothing")
	assert.Equal(t, []string{"telepathy"}, d.UnknownFlags)
}

// Every code the core can emit needs a weight, or the table has a hole.
func TestWeightsTableCoversEveryCoreFlag(t *testing.T) {
	emitted := []string{
		FlagMultiGraphemeInsert, FlagPaste, FlagMinInterval, FlagUniformIntervals,
		FlagZeroVariance, FlagSuperhumanBurst, FlagAfkHeavy, FlagTrailingAfk,
		FlagUnpairedKeyup, FlagCanaryGrapheme, FlagCanaryCommit,
	}
	desc, ok := Describe(mustPolicy(t, Config{}))
	require.True(t, ok)
	for _, code := range emitted {
		_, known := desc.Weights[code]
		assert.True(t, known, "no weight for core flag %q", code)
	}
	assert.Len(t, desc.Weights, len(emitted), "the weights table has entries the core never emits")
}

// --- combination rules --------------------------------------------------------

// A shape outranks a magnitude: the rule fires even with every weight zeroed, so
// no amount of tuning can hide a pattern only a machine produces.
func TestBotCadenceFiresEvenWithZeroWeights(t *testing.T) {
	spec := ""
	for code := range defaultFlagWeights {
		if spec != "" {
			spec += ","
		}
		spec += code + "=0"
	}
	p := mustPolicy(t, Config{FlagWeights: spec})

	d := p.Judge([]Flag{
		{Code: FlagUniformIntervals, Score: 1},
		{Code: FlagZeroVariance, Score: 1},
	}, RunMeta{DurationSec: 30})

	assert.True(t, d.NeedsReview)
	assert.Equal(t, []string{ruleBotCadence}, d.Reasons)
	assert.Zero(t, d.Suspicion, "weights were zeroed; only the shape rule should be left")
}

func TestBotCadenceNeedsBothFlags(t *testing.T) {
	p := mustPolicy(t, Config{})
	assert.Empty(t, p.Judge([]Flag{{Code: FlagUniformIntervals, Score: 1}}, RunMeta{}).Reasons)
	assert.Empty(t, p.Judge([]Flag{{Code: FlagZeroVariance, Score: 1}}, RunMeta{}).Reasons)
	assert.Equal(t, []string{ruleBotCadence}, p.Judge([]Flag{
		{Code: FlagUniformIntervals, Score: 1},
		{Code: FlagZeroVariance, Score: 1},
	}, RunMeta{}).Reasons)
}

func TestSustainedSuperhumanNeedsTheDurationFloor(t *testing.T) {
	p := mustPolicy(t, Config{})
	burst := []Flag{{Code: FlagSuperhumanBurst, Score: 0.6}}

	assert.Empty(t, p.Judge(burst, RunMeta{DurationSec: defaultSustainedBurstSec - 0.1}).Reasons,
		"a brief flurry is not evidence")
	assert.Equal(t, []string{ruleSustainedSuperhuman},
		p.Judge(burst, RunMeta{DurationSec: defaultSustainedBurstSec}).Reasons)
	// A missing duration makes the rule abstain rather than fire on a guess.
	assert.Empty(t, p.Judge(burst, RunMeta{}).Reasons)
}

// The rule has no accuracy condition, and until the v4 core it had one anyway.
//
// `sustained_superhuman` cannot fire unless FlagSuperhumanBurst is present, and
// that flag used to be gated on `metrics.accuracy === 1` — a STRICT equality —
// so a single mistyped character removed the flag and disabled the combination
// rule along with it. That is how a 336.2 wpm run held for a full minute at
// 99.6 % accuracy was accepted at suspicion 0.0074 while a 282 wpm run of ten
// seconds at 100 % was flagged.
//
// The fix is in the core, not here. This test is what keeps it that way: the
// rule fires on the FLAG and the DURATION and on nothing else, whatever severity
// the core attached — including the reduced severity an imperfect run now
// carries, which is the exact case the old gate erased.
func TestSustainedSuperhumanHasNoAccuracyGate(t *testing.T) {
	p := mustPolicy(t, Config{})

	// The severity the v4 core produces for 336.2 wpm at 99.6 % over 60 s:
	// min(1, 336.2/500) × f(0.996) ≈ 0.664. Nothing about it is "flawless", and
	// the rule must not care.
	imperfect := []Flag{{Code: FlagSuperhumanBurst, Score: 0.664}}
	assert.Equal(t, []string{ruleSustainedSuperhuman},
		p.Judge(imperfect, RunMeta{DurationSec: 60}).Reasons,
		"a run does not stop being sustained because it contains a typo")

	// And the rule is severity-blind at the floor as well as the ceiling: the
	// accuracy curve bottoms out at 0.25, so the weakest possible instance of
	// the flag still names the shape.
	floor := []Flag{{Code: FlagSuperhumanBurst, Score: 0.25}}
	assert.Equal(t, []string{ruleSustainedSuperhuman},
		p.Judge(floor, RunMeta{DurationSec: 60}).Reasons)

	// And firing is where it stops. The rule is REPORTED and it contributes its
	// weight, but it does not route the run to review on its own — see
	// forcesReview. At weight 0.80 a floor-severity instance is 0.2 of
	// suspicion against a threshold of 1.0, so this run stays accepted with the
	// shape recorded against it.
	//
	// That is deliberate and it is the difference between this rule and
	// bot_cadence. Speed is a continuum with real people at the top of it, and
	// `flagged` is not `accepted` — a forced review takes the run off the
	// leaderboard. A rule that fires on speed must therefore need corroboration
	// before it costs somebody a result.
	decision := p.Judge(floor, RunMeta{DurationSec: 60})
	assert.Less(t, decision.Suspicion, decision.Threshold)
	assert.False(t, decision.NeedsReview,
		"speed alone must not route a run to review — it has to reach the threshold")
	assert.Equal(t, []string{ruleSustainedSuperhuman}, decision.Reasons,
		"...but the shape is still recorded, or the decision is not explainable")
}

// The other half of the same rule, and the one that must NOT have changed:
// bot_cadence still routes on its own.
//
// Identical keystroke intervals to the millisecond across a whole run is a
// clock, not a fast human. There is no honest run to protect from it, so it
// keeps the bypass that superhuman-burst gave up — and a change that removed
// both would have quietly turned the shape rules into decoration.
func TestBotCadenceStillRoutesOnItsOwn(t *testing.T) {
	p := mustPolicy(t, Config{})
	// Severities low enough that the weighted sum is nowhere near the threshold.
	cadence := []Flag{
		{Code: FlagUniformIntervals, Score: 0.05},
		{Code: FlagZeroVariance, Score: 0.05},
	}
	d := p.Judge(cadence, RunMeta{DurationSec: 60})
	assert.Less(t, d.Suspicion, d.Threshold, "the fixture must not reach the threshold by weight")
	assert.True(t, d.NeedsReview, "bot_cadence must still route on the shape alone")
	assert.Equal(t, []string{ruleBotCadence}, d.Reasons)
}

// v4 is the first policy version whose rule set did not change. It moved because
// the CORE's flags changed underneath it — `superhuman-burst` lost its accuracy
// gate and gained a duration-dependent ceiling — and `revalidate` keys on the
// version to walk stored runs forward. A release that fixes a detector and
// leaves the version alone judges the whole history by the broken one.
func TestPolicyVersionTracksTheCoreDetectorChange(t *testing.T) {
	col, err := ParseVersion(mustPolicy(t, Config{}).Version())
	require.NoError(t, err)
	assert.EqualValues(t, 4, col,
		"bump this together with docs/REPLAY.md and a revalidate pass, never on its own")
}

// --- configuration -------------------------------------------------------------

func TestWeightOverridesApply(t *testing.T) {
	p := mustPolicy(t, Config{FlagWeights: "min-interval=0.9, paste=0.1"})
	desc, ok := Describe(p)
	require.True(t, ok)

	assert.InDelta(t, 0.9, desc.Weights[FlagMinInterval], 1e-12)
	assert.InDelta(t, 0.1, desc.Weights[FlagPaste], 1e-12)
	assert.InDelta(t, defaultFlagWeights[FlagZeroVariance], desc.Weights[FlagZeroVariance], 1e-12,
		"an untouched code keeps its calibrated weight")
}

func TestEmptyOverridesKeepTheDefaults(t *testing.T) {
	desc, ok := Describe(mustPolicy(t, Config{}))
	require.True(t, ok)
	assert.Equal(t, defaultFlagWeights, desc.Weights)
	assert.InDelta(t, defaultReviewThreshold, desc.Threshold, 1e-12)
}

// A typo in the tuning knob must stop the process, not quietly leave a check at
// its default.
func TestBadOverridesAreStartupErrors(t *testing.T) {
	for _, spec := range []string{
		"min-interval",            // no '='
		"telepathy=0.5",           // unknown code
		"min-interval=very",       // unparseable
		"min-interval=-1",         // negative
		"paste=0.5,telepathy=0.1", // one good, one bad
	} {
		_, err := Provide(Config{FlagWeights: spec})
		assert.Error(t, err, "override %q was accepted", spec)
	}
}

// The threshold governs review, or the knob is decorative.
func TestThresholdGovernsReview(t *testing.T) {
	weak := []Flag{{Code: FlagMinInterval, Score: 0.02}}
	assert.False(t, mustPolicy(t, Config{}).Judge(weak, RunMeta{}).NeedsReview)

	strict := mustPolicy(t, Config{ReviewThreshold: 0.001})
	d := strict.Judge(weak, RunMeta{})
	assert.True(t, d.NeedsReview)
	assert.Empty(t, d.Reasons, "this is a magnitude, not a shape")
}

// --- versioning ----------------------------------------------------------------

// The version is storable and is not the one a Noop reports — a run judged by
// this policy must be tellable from one judged by nothing.
func TestPolicyVersionIsStorableAndNotNone(t *testing.T) {
	p := mustPolicy(t, Config{})
	assert.NotEqual(t, VersionNone, p.Version())

	col, err := ParseVersion(p.Version())
	require.NoError(t, err)
	assert.Positive(t, col)
	assert.NotEqual(t, ColumnNone, col)
}

// Tuning does NOT move the version. The weights are env-overridable, and a
// version that shifted with them would make `revalidate` re-judge the whole
// table every time an operator nudged a knob. The recorded threshold is what
// explains a verdict on a tuned instance — which is exactly why the audit
// document carries it.
func TestOverridesDoNotMoveTheVersion(t *testing.T) {
	assert.Equal(t,
		mustPolicy(t, Config{}).Version(),
		mustPolicy(t, Config{FlagWeights: "min-interval=0.9", ReviewThreshold: 0.5}).Version())
}
