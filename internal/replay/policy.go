package replay

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// CurrentPolicyVersion identifies the rule set below. Bump it whenever the
// weights, the threshold, or the combination rules change in a way that would
// re-judge an already-judged run, then run `make revalidate` to walk stored runs
// forward. It is stored on every decision beside bundle_sha: the bundle says
// which code produced the numbers, the policy says which rules turned them into
// a status, and the two move independently.
const CurrentPolicyVersion int16 = 1

// Flag codes emitted by the core's validateLog (shared/core/validate.ts). Listed
// here so the weights table below is exhaustive by construction — an unknown
// code is a bundle change the server has not been taught about yet.
const (
	FlagMultiGraphemeInsert = "multi-grapheme-insert"
	FlagPaste               = "paste"
	FlagMinInterval         = "min-interval"
	FlagUniformIntervals    = "uniform-intervals"
	FlagZeroVariance        = "zero-variance"
	FlagSuperhumanBurst     = "superhuman-burst"
	FlagAfkHeavy            = "afk-heavy"
	FlagTrailingAfk         = "trailing-afk"
)

// DefaultFlagWeights is THE weights table: how much one unit of a flag's
// severity counts toward sending a run to review. Suspicion is
// Σ weight[code] × score, and the core already emits `score` in [0, 1], so a
// weight is "what a maximally severe instance of this flag is worth".
//
// Calibrated against the flags real runs actually raise (`make calibrate`):
//
//   - zero-variance (1.00) — every keystroke interval identical to the
//     millisecond. Humans do not do this; a script does. Worth review alone.
//   - uniform-intervals (0.90) — ≥90% of intervals within ±2 ms of the mean.
//     Same signal, slightly softer, because a metronome-steady typist is rare
//     but not impossible.
//   - superhuman-burst (0.80) — above the WPM ceiling at flawless accuracy.
//     Strong, but its severity is a ratio against 2× the ceiling, so a genuine
//     short burst lands well under the threshold; sustained speed is caught by
//     the combination rule instead.
//   - paste (0.80) — text that arrived without being typed. Unambiguous, but
//     the severity is pastes/events, so one paste in a long log stays small.
//   - multi-grapheme-insert (0.50) — more than one grapheme per event. Usually
//     an IME or a mobile keyboard, occasionally automation.
//   - min-interval (0.30) — intervals under 15 ms. THE false-positive
//     generator: ordinary key rollover produces one or two of these in a
//     hundred, which is a severity around 0.02 and must never reach review on
//     its own. Only a log where most intervals are impossible moves the needle,
//     and such a log trips superhuman-burst as well.
//   - afk-heavy / trailing-afk (0.02) — see the note below: idle is a bad run,
//     not a suspicious one.
//
// # Why AFK is worth almost nothing
//
// An idle or abandoned run is already punished by its own score: the duration
// keeps running, WPM collapses, and the result is a bad result. There is no
// advantage to be gained by going AFK, so it is not an anti-cheat signal — it is
// a shape of play. Flagging it buries the moderation queue in people who walked
// away from their keyboard while the genuinely suspicious runs sit underneath.
// The flags are still recorded on every run (they are useful for product
// questions like "how many runs get abandoned"), they just do not route to
// review. The weight is deliberately non-zero so a run that is BOTH mostly idle
// and otherwise suspicious still tips a little.
var DefaultFlagWeights = map[string]float64{
	FlagZeroVariance:        1.00,
	FlagUniformIntervals:    0.90,
	FlagSuperhumanBurst:     0.80,
	FlagPaste:               0.80,
	FlagMultiGraphemeInsert: 0.50,
	FlagMinInterval:         0.30,
	FlagAfkHeavy:            0.02,
	FlagTrailingAfk:         0.02,
}

// Policy defaults.
const (
	// DefaultReviewThreshold is the suspicion at which a run goes to review.
	// 1.0 means "one maximally severe strong flag, or a believable combination
	// of weaker ones". Every flag distribution seen on real runs sits two orders
	// of magnitude below it; the bot-shaped fixtures sit comfortably above.
	DefaultReviewThreshold = 1.0
	// DefaultSustainedBurstSec is the duration floor for the sustained-burst
	// combination rule. Under it, superhuman speed is a flurry — a few words
	// typed from muscle memory, or a short run where the clock barely moved.
	// Over it, it is a claim that has to be reviewed.
	DefaultSustainedBurstSec = 10.0
)

// Combination rule ids. A rule is a shape that is suspicious even when no single
// flag is severe enough to reach the threshold on its own.
const (
	// RuleBotCadence: uniform intervals AND zero variance together. Machine
	// timing — a human hand cannot hold an identical interval, and the two flags
	// reinforce rather than repeat each other (uniformity is a ratio, variance
	// is absolute).
	RuleBotCadence = "bot_cadence"
	// RuleSustainedSuperhuman: superhuman burst held past the duration floor.
	// A two-second flurry above the WPM ceiling is rollover or a short sample;
	// ten seconds of it is not.
	RuleSustainedSuperhuman = "sustained_superhuman"
)

// Policy is the flag-scoring rule set: what each flag is worth, how much
// suspicion sends a run to review, and which shapes bypass the threshold.
// Immutable once built — the worker shares one across goroutines.
type Policy struct {
	Version int16
	// Weights maps a flag code to its contribution per unit of severity. A code
	// missing from the map contributes nothing (and is reported by
	// UnknownFlagCodes so a bundle change cannot silently disarm a check).
	Weights map[string]float64
	// ReviewThreshold is the suspicion at or above which a run is flagged.
	ReviewThreshold float64
	// SustainedBurstSec is the duration floor for RuleSustainedSuperhuman.
	SustainedBurstSec float64
}

// DefaultPolicy is the calibrated rule set documented above.
func DefaultPolicy() Policy {
	return Policy{
		Version:           CurrentPolicyVersion,
		Weights:           maps.Clone(DefaultFlagWeights),
		ReviewThreshold:   DefaultReviewThreshold,
		SustainedBurstSec: DefaultSustainedBurstSec,
	}
}

// WithOverrides returns a copy of p with per-code weight overrides applied, from
// a "code=weight,code=weight" string (TYPEMORE_REPLAY_FLAG_WEIGHTS). An unknown
// code or an unparseable weight is an error: a typo in the tuning knob must fail
// at startup, not quietly leave a check at its default.
func (p Policy) WithOverrides(spec string, threshold, sustainedBurstSec float64) (Policy, error) {
	out := p
	out.Weights = maps.Clone(p.Weights)
	if threshold > 0 {
		out.ReviewThreshold = threshold
	}
	if sustainedBurstSec > 0 {
		out.SustainedBurstSec = sustainedBurstSec
	}

	for part := range strings.SplitSeq(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		code, raw, ok := strings.Cut(part, "=")
		if !ok {
			return Policy{}, fmt.Errorf("replay: flag weight %q is not code=weight", part)
		}
		code = strings.TrimSpace(code)
		if _, known := out.Weights[code]; !known {
			return Policy{}, fmt.Errorf("replay: unknown flag code %q (known: %s)",
				code, strings.Join(slices.Sorted(maps.Keys(out.Weights)), ", "))
		}
		w, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
		if err != nil {
			return Policy{}, fmt.Errorf("replay: flag weight for %q: %w", code, err)
		}
		if w < 0 {
			return Policy{}, fmt.Errorf("replay: flag weight for %q is negative", code)
		}
		out.Weights[code] = w
	}
	return out, nil
}

// Suspicion is Σ weight × severity over the run's flags. Codes the policy does
// not know contribute nothing — they are surfaced separately rather than
// guessed at.
func (p Policy) Suspicion(flags []Flag) float64 {
	var total float64
	for _, f := range flags {
		total += p.Weights[f.Code] * f.Score
	}
	return total
}

// UnknownFlagCodes lists flag codes the policy has no weight for, sorted. A
// non-empty result means the bundle emits a signal the policy ignores, which is
// a review-the-weights event, not something to paper over.
func (p Policy) UnknownFlagCodes(flags []Flag) []string {
	var out []string
	for _, f := range flags {
		if _, ok := p.Weights[f.Code]; !ok {
			out = append(out, f.Code)
		}
	}
	sort.Strings(out)
	return slices.Compact(out)
}

// Combinations returns the ids of the bot-shaped rules that fire for this run,
// in a stable order. These bypass the threshold: they are shapes, not
// magnitudes, so no amount of weight tuning should be able to hide them.
func (p Policy) Combinations(flags []Flag, metrics json.RawMessage) []string {
	present := make(map[string]struct{}, len(flags))
	for _, f := range flags {
		present[f.Code] = struct{}{}
	}

	var fired []string
	_, uniform := present[FlagUniformIntervals]
	_, zeroVar := present[FlagZeroVariance]
	if uniform && zeroVar {
		fired = append(fired, RuleBotCadence)
	}
	if _, burst := present[FlagSuperhumanBurst]; burst && runSeconds(metrics) >= p.SustainedBurstSec {
		fired = append(fired, RuleSustainedSuperhuman)
	}
	return fired
}

// runSeconds reads durationSec out of the server's own metrics. A missing or
// unreadable value reads as 0, which makes the duration-floor rule abstain
// rather than fire on a guess.
func runSeconds(metrics json.RawMessage) float64 {
	if len(metrics) == 0 {
		return 0
	}
	var m struct {
		DurationSec float64 `json:"durationSec"`
	}
	if err := json.Unmarshal(metrics, &m); err != nil {
		return 0
	}
	return m.DurationSec
}
