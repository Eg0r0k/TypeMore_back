//go:build !anticheat

package policy

// BuildTag names the build constraint that switches the real policy in. It is
// reported by Provide so an operator reading a log line knows what to do about
// it, and so /healthz can say the same thing.
const BuildTag = "anticheat"

// Enabled reports whether this binary was built with a review policy in it. It
// is a build-time fact, not a runtime one.
const Enabled = false

// Provide returns the judge this build has. Without the tag there is exactly
// one: Noop.
//
// The weights, the threshold and the shape rules are not merely unreachable
// here — they are not compiled in at all, so `strings` on this binary finds no
// rule name and no number. That is the property the build tag exists for and
// the one TestBinaryWithoutTheTagCarriesNoPolicy checks.
func Provide(cfg Config) (Judge, error) {
	if !cfg.IsZero() {
		return Noop{}, ErrNoPolicy
	}
	return Noop{}, nil
}
