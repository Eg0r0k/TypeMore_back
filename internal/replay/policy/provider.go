package policy

import "errors"

// Config is the operator's tuning surface for the review policy.
//
// It is declared in the open because the composition root has to be able to
// read the environment without knowing whether a policy exists. The VALUES it
// carries only mean anything inside a build that has one — see provide_off.go
// and provide_anticheat.go.
//
// A runtime switch would not have been enough. An env-var-driven policy leaves
// its weights, its threshold and its rule names in the binary whether or not
// the switch is on, and `strings` finds them in ten seconds. The knob is
// therefore a build tag, and this Config only tunes what the build already
// contains.
type Config struct {
	// FlagWeights is TYPEMORE_REPLAY_FLAG_WEIGHTS: "code=weight,code=weight".
	FlagWeights string
	// ReviewThreshold overrides the suspicion at which a run goes to review.
	// Zero means "leave it alone".
	ReviewThreshold float64
	// SustainedBurstSec overrides the duration floor a shape rule uses. Zero
	// means "leave it alone".
	SustainedBurstSec float64
}

// IsZero reports whether the operator set none of the knobs. Used to tell a
// meaningless override apart from no override at all, so a build with no policy
// can say something useful about an env var that will not do anything.
func (c Config) IsZero() bool {
	return c.FlagWeights == "" && c.ReviewThreshold == 0 && c.SustainedBurstSec == 0
}

// ErrNoPolicy is returned beside a Noop judge when the operator has set the
// policy knobs on a build that has no policy. It is a WARNING, not a startup
// failure: an instance deliberately running without anti-cheat must not refuse
// to boot over a leftover environment variable — working without one is the
// entire point of that build.
//
// Declared here rather than beside the untagged Provide so that callers can
// match on it in EITHER build. A caller that had to guard the errors.Is with a
// build tag of its own would be a second copy of this seam.
var ErrNoPolicy = errors.New("policy: this binary was built without a review policy; " +
	"TYPEMORE_REPLAY_* tuning has nothing to tune (rebuild with -tags " + BuildTag + ")")
