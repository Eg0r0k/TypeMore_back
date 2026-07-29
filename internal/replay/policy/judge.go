// Package policy is the seam between replay's correctness and replay's
// judgement.
//
// # What is open and what is not
//
// Everything the product needs to WORK is open and stays open: regenerating a
// run's words from its seed, folding its log through the same core the browser
// ran, recomputing the score and the metrics with exact equality, and the HARD
// refusals — a structurally invalid log, an unknown dictionary, a cap, a
// commit-consistency failure, a score that does not match. Bans, moderation,
// leaderboards, the worker and the queue are open too. A self-hosted instance
// with none of this package's real implementation still validates runs and still
// refuses bad ones.
//
// What goes behind this interface is ONLY the judgement policy: what each flag
// is worth, how much suspicion sends a run to review, which flag combinations
// are shapes rather than magnitudes, and the version those rules are known by.
// Publishing those is publishing the answer key.
//
// The detectors are NOT hidden — they live in the core, which executes in the
// browser and is vendored into this repo. Hiding them would be pretending.
// What is hidden is what the server DOES about what they report.
//
// # The open default judges nothing
//
// Noop is the shipped implementation when the real one is not built in. It
// computes no suspicion and sends nothing to review, and it says so: Version()
// is "none", which reaches the database on every run it judges, so runs judged
// without a policy stay distinguishable forever and a bundle-aware revalidate
// re-judges them if a policy is turned on later. Without that, an open verdict
// and a closed verdict would be indistinguishable in the database and enabling
// a policy retroactively would be impossible.
//
// The same pattern the captcha already uses: a nil verifier is a no-op
// (internal/auth/service.go), and the instance boots without one.
package policy

// Flag is one scored plausibility flag from the core's validateLog. Score is in
// [0, 1] — "how severe an instance of this is" — and Detail is the human string
// the core produced.
//
// It lives here rather than in the replay package because the Judge interface
// takes it and the replay package refers to it by alias; a judge must be
// writable against this package alone.
type Flag struct {
	Code   string  `json:"code"`
	Score  float64 `json:"score"`
	Detail string  `json:"detail,omitempty"`
}

// RunMeta is what a judge may know about a run BESIDES its flags. Deliberately
// small and deliberately typed: a judge that needed the log, the score
// breakdown or the player's history would be re-deciding correctness, which is
// not its job.
//
// Adding a field here is a change to the open interface, so it is visible even
// when the implementation that wanted it is not.
type RunMeta struct {
	// DurationSec is the server's own recomputed run duration. A combination
	// rule that distinguishes a two-second flurry from ten seconds of sustained
	// impossibility needs it; nothing else in the shape does.
	DurationSec float64
	// ScoreVersion is the score formula the run was played under.
	ScoreVersion int16
}

// Decision is a judge's answer about one run. It never decides whether the run
// is VALID — that is settled before a judge is consulted and does not depend on
// one.
type Decision struct {
	// Suspicion is the weighted total the judge computed. Recorded on every
	// judged run, accepted ones included, so moderation can audit the boundary
	// without re-running anything.
	Suspicion float64
	// NeedsReview is the answer. It is the judge's, not a comparison the caller
	// makes: the caller does not know the threshold and must not.
	NeedsReview bool
	// Reasons are the ids of the shape rules that fired, in a stable order. A
	// non-empty Reasons means the run was routed on a SHAPE rather than on a
	// magnitude, which the audit trail records differently.
	Reasons []string
	// Threshold is what Suspicion was compared against, recorded so a verdict
	// is explainable on an instance running tuned weights. Meaningless when the
	// judge does not score.
	Threshold float64
	// UnknownFlags are codes the judge has no opinion about, sorted. Non-empty
	// means the core emits a signal the policy has not been taught, which is a
	// review-the-policy event rather than something to paper over.
	UnknownFlags []string
}

// Judge turns the core's flags into a routing decision.
//
// Implementations must be safe for concurrent use and must be pure: the worker
// shares one across goroutines and calls it once per run, and a judge that
// depended on call order would make verdicts unreproducible.
type Judge interface {
	Judge(flags []Flag, meta RunMeta) Decision
	// Version identifies the rule set, and is stored on every run it judged.
	// Change it whenever a weight, a threshold or a rule changes in a way that
	// would re-judge an already-judged run, then run `make revalidate`.
	//
	// It must be either VersionNone or a positive integer in decimal — the
	// column it lands in is a smallint, and the verdict format does not change
	// for this. ParseVersion is the one place that mapping lives.
	Version() string
}

// VersionNone is the version of a judge that does not judge. It is not the
// absence of a value: it is the durable record that this run was decided for
// correctness only.
const VersionNone = "none"

// Noop is the open default: it judges nothing.
//
// An instance running Noop still replays every run, still recomputes every
// number, and still rejects everything the hard rules reject. What it does not
// do is compute suspicion or fill a review queue — so its leaderboards are not
// protected against a cheat that produces a structurally perfect log. See
// docs/SELF_HOST.md.
type Noop struct{}

// Judge returns the empty decision: no suspicion, no review, no reasons.
func (Noop) Judge([]Flag, RunMeta) Decision { return Decision{} }

// Version is VersionNone.
func (Noop) Version() string { return VersionNone }

// IsNoop reports whether a judge does no judging, by asking its version rather
// than its type — a closed implementation may have its own way of being turned
// off, and the startup warning and /healthz should believe it.
func IsNoop(j Judge) bool { return j == nil || j.Version() == VersionNone }

// Description is what a judge can tell OPERATOR TOOLING about itself: the line
// it routes on, and what each flag is worth. It exists for `replayctl
// calibrate`, whose whole job is to answer "what would happen if I changed this
// weight" and which cannot do that from verdicts alone.
//
// Nothing in the serving path reads it. A build without a policy has no judge
// that implements Describer, so the numbers are absent from the binary rather
// than merely unused — which is the property the build tag is for.
type Description struct {
	Threshold float64
	// Weights is what one unit of each flag's severity is worth. A copy: tooling
	// must not be able to retune a running judge by writing to the map it was
	// handed.
	Weights map[string]float64
}

// Describer is implemented by judges that can explain their own arithmetic.
// OPTIONAL by design — a judge is complete without it, and Noop does not
// implement it because it has nothing to describe.
type Describer interface {
	Describe() Description
}

// Describe returns a judge's arithmetic if it will say, and ok=false otherwise.
// Tooling should degrade to what the verdicts themselves carry rather than
// insisting.
func Describe(j Judge) (Description, bool) {
	d, ok := j.(Describer)
	if !ok {
		return Description{}, false
	}
	return d.Describe(), true
}
