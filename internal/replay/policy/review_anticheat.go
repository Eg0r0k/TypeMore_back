//go:build anticheat

package policy

import (
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// This file is the judgement policy: what a flag is worth, how much suspicion
// sends a run to review, and which flag combinations are shapes rather than
// magnitudes. It compiles ONLY under -tags anticheat.
//
// It is the file that would move to a private module behind a `replace`
// directive; nothing outside this package refers to anything in it, so moving
// it is a build-file change and not a refactor.
//
// Everything a self-hosted instance needs to validate runs correctly is
// elsewhere and open. What is here is the answer key.

// currentVersion identifies the rule set below. Bump it whenever a weight, the
// threshold, or a combination rule changes in a way that would re-judge an
// already-judged run, then run `make revalidate` to walk stored runs forward.
//
// It continues the same sequence the policy had before it moved behind this
// interface: v1 was the calibrated table, v2 pinned the telemetry flags at zero
// by contract, v3 weighted the two canary flags. Runs judged by a Noop carry no
// version at all, which is how they stay re-judgeable.
//
// v3 is deliberately inert on its own: the canary flags cannot appear on a run
// the epoch has not armed (replay.CanariesArmedAt), and the epoch is unset
// until a human sets it. A revalidate pass at v3 with no epoch therefore
// reproduces every v2 verdict exactly.
//
// v4 is NOT inert, and it is the first version that is not. No weight and no
// threshold moved — the change is in the CORE, where `superhuman-burst` stopped
// being gated on `accuracy === 1` and its ceiling became a function of the run's
// duration. The rule set here reads the same, but the flags it reads are
// different, so the version has to move for `revalidate` to walk stored runs
// forward. Runs that were accepted at ~0.007 suspicion because one mistyped
// character hid a 336 wpm minute are the runs that change.
const currentVersion = 4

// Flag codes emitted by the core's validateLog (shared/core/validate.ts).
// Listed here so the weights table is exhaustive by construction — a code the
// table has never heard of is a bundle change the policy has not been taught.
const (
	FlagMultiGraphemeInsert = "multi-grapheme-insert"
	FlagPaste               = "paste"
	FlagMinInterval         = "min-interval"
	FlagUniformIntervals    = "uniform-intervals"
	FlagZeroVariance        = "zero-variance"
	FlagSuperhumanBurst     = "superhuman-burst"
	FlagAfkHeavy            = "afk-heavy"
	FlagTrailingAfk         = "trailing-afk"
	// FlagUnpairedKeyup is log v2's telemetry pairing sanity: a key release
	// without a preceding press. Structural bookkeeping, not a cheat signal.
	FlagUnpairedKeyup = "unpaired-keyup"
	// FlagCanaryGrapheme is an insert carrying an invisible codepoint that only
	// the display layer ever emits. Direct evidence the text was read off the
	// screen rather than typed.
	FlagCanaryGrapheme = "canary-grapheme"
	// FlagCanaryCommit is repeated commits landing exactly on seed-scheduled
	// offsets. Circumstantial, and scored as such.
	FlagCanaryCommit = "canary-commit"
)

// TelemetryOnlyFlags are the codes derived from log v2's keyboard telemetry.
// They are COLLECTED and stored on every decision, and they are worth exactly
// nothing: the phase decision for log v2 is a structural layer only, with scored
// heuristics deferred until there is calibration data from a real population to
// weight them against.
//
// The zero is the contract, not an accident of the table. An unpaired keyup is
// something honest players produce all day — alt-tab with a key held, a layout
// switch mid-word, a window that lost focus — and there is no v2 calibration
// data at all, so any weight on it would be a guess that lands on real players.
// Judging telemetry starts by removing a code from this list, deliberately, with
// the numbers to justify it.
var TelemetryOnlyFlags = []string{FlagUnpairedKeyup}

// defaultFlagWeights is THE weights table: how much one unit of a flag's
// severity counts toward sending a run to review. Suspicion is
// Σ weight[code] × score, and the core already emits `score` in [0, 1], so a
// weight is "what a maximally severe instance of this flag is worth".
//
// Calibrated against the flags real runs actually raise (`make calibrate`):
//
//   - zero-variance (1.00) — every keystroke interval identical to the
//     millisecond. Humans do not do this; a script does. Worth review alone.
//
//   - uniform-intervals (0.90) — ≥90% of intervals within ±2 ms of the mean.
//     Same signal, slightly softer, because a metronome-steady typist is rare
//     but not impossible.
//
//   - superhuman-burst (0.80) — above the WPM ceiling FOR THE RUN'S OWN
//     DURATION (core: BURST_CEILING, 250 wpm at 15 s falling to 200 at 60 s+).
//     Strong, but its severity is speed against a fixed 500 wpm scale, softened
//     by an accuracy curve that bottoms out at 0.25 rather than at zero — so a
//     genuine short burst lands well under the threshold, and a fast run made
//     sloppy on purpose still counts for something. Sustained speed is caught by
//     the combination rule instead.
//
//     The weight is unchanged at v4 and that is deliberate: the core fix alone
//     changes which runs raise the flag, and re-weighting at the same time would
//     make the revalidate diff unreadable. Retune after `calibrate` runs against
//     the repaired detector, not before.
//
//   - paste (0.80) — text that arrived without being typed. Unambiguous, but
//     the severity is pastes/events, so one paste in a long log stays small.
//
//   - multi-grapheme-insert (0.50) — more than one grapheme per event. Usually
//     an IME or a mobile keyboard, occasionally automation.
//
//   - min-interval (0.30) — intervals under 15 ms. THE false-positive
//     generator: ordinary key rollover produces one or two of these in a
//     hundred, which is a severity around 0.02 and must never reach review on
//     its own.
//
//   - afk-heavy / trailing-afk (0.02) — idle is a bad run, not a suspicious
//     one. It is already punished by its own score and buys the player nothing,
//     so it must not fill the review queue. Non-zero on purpose so a run that is
//     BOTH mostly idle and otherwise suspicious still tips a little.
//
//   - unpaired-keyup (0.00) — see TelemetryOnlyFlags.
//
//   - canary-grapheme (1.00) — an invisible codepoint that exists ONLY in the
//     rendered text reached the event log. No keyboard, layout, IME or compose
//     sequence produces it, and pasted text is a different flag entirely, so it
//     belongs in the zero-variance class: one occurrence is worth review by
//     itself. The core already emits score 1 whatever the count.
//
//   - canary-commit (0.60) — commits landing on seed-scheduled offsets. Real
//     evidence, but positional rather than impossible: the core already refuses
//     to raise it below three hits and scales severity from there, so the
//     weight is set so a MINIMUM hit (0.25) stays well under the threshold
//     while a saturated one (1.0) needs one more weak signal to cross. 0.60 is
//     a STARTING point — the number to revisit with `make calibrate` once armed
//     runs exist; there is no armed population to calibrate against yet, and
//     inventing precision here would be inventing data.
var defaultFlagWeights = map[string]float64{
	FlagZeroVariance:        1.00,
	FlagCanaryGrapheme:      1.00,
	FlagUniformIntervals:    0.90,
	FlagSuperhumanBurst:     0.80,
	FlagPaste:               0.80,
	FlagCanaryCommit:        0.60,
	FlagMultiGraphemeInsert: 0.50,
	FlagMinInterval:         0.30,
	FlagAfkHeavy:            0.02,
	FlagTrailingAfk:         0.02,
	FlagUnpairedKeyup:       0.00,
}

const (
	// defaultReviewThreshold is the suspicion at which a run goes to review.
	// 1.0 means "one maximally severe strong flag, or a believable combination
	// of weaker ones". Every flag distribution seen on real runs sits two orders
	// of magnitude below it; the bot-shaped fixtures sit comfortably above.
	defaultReviewThreshold = 1.0
	// defaultSustainedBurstSec is the duration floor for the sustained-burst
	// combination rule. Under it, superhuman speed is a flurry — a few words
	// typed from muscle memory, or a short run where the clock barely moved.
	// Over it, it is a claim that has to be reviewed.
	defaultSustainedBurstSec = 10.0
)

// Combination rule ids. A rule is a shape that is suspicious even when no single
// flag is severe enough to reach the threshold on its own.
const (
	// ruleBotCadence: uniform intervals AND zero variance together. Machine
	// timing — a human hand cannot hold an identical interval, and the two flags
	// reinforce rather than repeat each other (uniformity is a ratio, variance
	// is absolute).
	ruleBotCadence = "bot_cadence"
	// ruleSustainedSuperhuman: superhuman burst held past the duration floor. A
	// two-second flurry above the WPM ceiling is rollover or a short sample; ten
	// seconds of it is not.
	//
	// It carries NO accuracy condition of its own and never did — but until the
	// v4 core it inherited one, because it cannot fire unless
	// FlagSuperhumanBurst is present and that flag was gated on
	// `accuracy === 1`. One typo therefore disabled the combination rule as
	// well as the flag, which is why a 336 wpm run held for a full minute was
	// never routed to review. Nothing in this file had to change to fix that,
	// and TestSustainedSuperhumanHasNoAccuracyGate is what keeps it that way.
	ruleSustainedSuperhuman = "sustained_superhuman"
)

// reviewPolicy is the flag-scoring rule set. Immutable once built — the worker
// shares one across goroutines.
type reviewPolicy struct {
	version           int
	weights           map[string]float64
	reviewThreshold   float64
	sustainedBurstSec float64
}

// Provide builds the review policy, applying the operator's overrides. An
// unknown code or an unparseable weight is an ERROR: a typo in the tuning knob
// must stop the process at startup, not quietly leave a check at its default.
func Provide(cfg Config) (Judge, error) {
	p := &reviewPolicy{
		version:           currentVersion,
		weights:           maps.Clone(defaultFlagWeights),
		reviewThreshold:   defaultReviewThreshold,
		sustainedBurstSec: defaultSustainedBurstSec,
	}
	if cfg.ReviewThreshold > 0 {
		p.reviewThreshold = cfg.ReviewThreshold
	}
	if cfg.SustainedBurstSec > 0 {
		p.sustainedBurstSec = cfg.SustainedBurstSec
	}
	if err := p.applyWeightOverrides(cfg.FlagWeights); err != nil {
		return nil, err
	}
	return p, nil
}

// applyWeightOverrides parses "code=weight,code=weight"
// (TYPEMORE_REPLAY_FLAG_WEIGHTS) onto the table.
func (p *reviewPolicy) applyWeightOverrides(spec string) error {
	for part := range strings.SplitSeq(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		code, raw, ok := strings.Cut(part, "=")
		if !ok {
			return fmt.Errorf("policy: flag weight %q is not code=weight", part)
		}
		code = strings.TrimSpace(code)
		if _, known := p.weights[code]; !known {
			return fmt.Errorf("policy: unknown flag code %q (known: %s)",
				code, strings.Join(slices.Sorted(maps.Keys(p.weights)), ", "))
		}
		w, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
		if err != nil {
			return fmt.Errorf("policy: flag weight for %q: %w", code, err)
		}
		if w < 0 {
			return fmt.Errorf("policy: flag weight for %q is negative", code)
		}
		p.weights[code] = w
	}
	return nil
}

func (p *reviewPolicy) Version() string { return strconv.Itoa(p.version) }

// Judge scores the flags and decides whether the run is routed to review.
//
// A SHAPE outranks a magnitude: a combination rule sends a run to review
// whatever the weights say, so no amount of tuning can hide a pattern only a
// machine produces.
func (p *reviewPolicy) Judge(flags []Flag, meta RunMeta) Decision {
	suspicion := p.suspicion(flags)
	reasons := p.combinations(flags, meta)
	return Decision{
		Suspicion:    suspicion,
		NeedsReview:  len(reasons) > 0 || suspicion >= p.reviewThreshold,
		Reasons:      reasons,
		Threshold:    p.reviewThreshold,
		UnknownFlags: p.unknownFlagCodes(flags),
	}
}

// suspicion is Σ weight × severity. Codes the policy does not know contribute
// nothing — they are surfaced separately rather than guessed at.
func (p *reviewPolicy) suspicion(flags []Flag) float64 {
	var total float64
	for _, f := range flags {
		total += p.weights[f.Code] * f.Score
	}
	return total
}

// unknownFlagCodes lists codes the policy has no weight for, sorted.
func (p *reviewPolicy) unknownFlagCodes(flags []Flag) []string {
	var out []string
	for _, f := range flags {
		if _, ok := p.weights[f.Code]; !ok {
			out = append(out, f.Code)
		}
	}
	sort.Strings(out)
	return slices.Compact(out)
}

// combinations returns the ids of the bot-shaped rules that fire, in a stable
// order.
func (p *reviewPolicy) combinations(flags []Flag, meta RunMeta) []string {
	present := make(map[string]struct{}, len(flags))
	for _, f := range flags {
		present[f.Code] = struct{}{}
	}

	var fired []string
	_, uniform := present[FlagUniformIntervals]
	_, zeroVar := present[FlagZeroVariance]
	if uniform && zeroVar {
		fired = append(fired, ruleBotCadence)
	}
	// A missing or unreadable duration reads as 0, which makes the floor rule
	// abstain rather than fire on a guess.
	if _, burst := present[FlagSuperhumanBurst]; burst && meta.DurationSec >= p.sustainedBurstSec {
		fired = append(fired, ruleSustainedSuperhuman)
	}
	return fired
}

// Describe reports the policy's arithmetic to operator tooling. The weights are
// copied: `replayctl calibrate` must not be able to retune a running judge by
// writing to the map it was handed.
func (p *reviewPolicy) Describe() Description {
	return Description{Threshold: p.reviewThreshold, Weights: maps.Clone(p.weights)}
}
