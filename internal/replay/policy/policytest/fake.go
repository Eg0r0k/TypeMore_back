package policytest

import (
	"sort"

	"github.com/typemore/typemore-server/internal/replay/policy"
)

// FakeThreshold is the suspicion at which Fake routes a run to review.
const FakeThreshold = 1.0

// FakeVersion is the version Fake reports. Not a number the real policy has
// ever used or will: a run judged by the fake must never be mistaken for a run
// judged by a policy.
const FakeVersion = "999"

// FakeShapeRule is the id Fake fires for its one combination rule. Named for
// what it is, so it cannot be confused with a real rule id in a log or a
// database row.
const FakeShapeRule = "fake_shape"

// fakeWeights are deliberately round, deliberately few, and deliberately NOT
// the calibrated table. Tests that need a judge to do something should be
// asserting the worker's plumbing — that flags reach a judge, that a decision
// reaches the audit document, that review routing lands in the status — and a
// fake carrying the real numbers would quietly turn every one of those into a
// test of the closed policy.
var fakeWeights = map[string]float64{
	"paste":                 1.00,
	"superhuman-burst":      0.50,
	"multi-grapheme-insert": 0.50,
	"min-interval":          0.25,
	"uniform-intervals":     0.10,
	"zero-variance":         0.10,
	"afk-heavy":             0.00,
	"trailing-afk":          0.00,
	"unpaired-keyup":        0.00,
}

// Fake is a deterministic judge for the open repo's tests.
//
// It exists so the worker's tests never depend on code that may not be built —
// or may not be present at all, once the real policy lives in a private module.
// It scores like a policy (weights, a threshold, one shape rule that outranks
// the threshold) without being one, which is exactly enough to exercise every
// path the worker has: accepted with no flags, accepted WITH a flag below the
// line, flagged on a magnitude, and flagged on a shape.
//
// Safe for concurrent use: it is immutable after construction.
type Fake struct {
	weights   map[string]float64
	threshold float64
	version   string
}

// NewFake returns the standard fake judge.
func NewFake() *Fake {
	return &Fake{weights: fakeWeights, threshold: FakeThreshold, version: FakeVersion}
}

// NewFakeWithThreshold returns a fake that reviews at a chosen threshold. A
// threshold of 0 reviews every run that raised ANY flag; an enormous one reviews
// none. Between them they bracket what a judge can do, which is how a test
// proves a HARD verdict does not depend on one.
//
// Note what a zero threshold does NOT do: review a run with no flags. Nothing
// was raised against such a run, so there is nothing for a reviewer to look at —
// and a judge that routed it anyway would be routing on the absence of evidence.
// The contract requires this of every judge, and the shipped policy gets it for
// free (its threshold cannot be set to zero); the fake has to say it, because
// its threshold can.
func NewFakeWithThreshold(threshold float64) *Fake {
	return &Fake{weights: fakeWeights, threshold: threshold, version: FakeVersion}
}

// NewFakeVersioned returns a fake with a chosen version and threshold — for a
// test that has to make a run look STALE and then re-judge it, which is what a
// revalidation pass is. The version must satisfy policy.ParseVersion.
func NewFakeVersioned(version string, threshold float64) *Fake {
	return &Fake{weights: fakeWeights, threshold: threshold, version: version}
}

// Version reports the fake's version.
func (f *Fake) Version() string { return f.version }

// Judge scores the flags, fires its one shape rule, and routes.
func (f *Fake) Judge(flags []policy.Flag, _ policy.RunMeta) policy.Decision {
	var suspicion float64
	present := make(map[string]struct{}, len(flags))
	var unknown []string
	for _, fl := range flags {
		if w, ok := f.weights[fl.Code]; ok {
			suspicion += w * fl.Score
		} else {
			unknown = append(unknown, fl.Code)
		}
		present[fl.Code] = struct{}{}
	}
	sort.Strings(unknown)

	// One shape rule, on the pair no hand produces: it fires whatever the
	// weights say, so a test can prove that a shape outranks a magnitude
	// without knowing any real rule's name.
	var reasons []string
	_, uniform := present["uniform-intervals"]
	_, zeroVar := present["zero-variance"]
	if uniform && zeroVar {
		reasons = append(reasons, FakeShapeRule)
	}

	return policy.Decision{
		Suspicion:    suspicion,
		NeedsReview:  len(flags) > 0 && (len(reasons) > 0 || suspicion >= f.threshold),
		Reasons:      reasons,
		Threshold:    f.threshold,
		UnknownFlags: unknown,
	}
}
