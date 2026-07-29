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
		FlagUnpairedKeyup,
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
