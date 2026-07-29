//go:build anticheat

package policy

// BuildTag names the build constraint that switches the real policy in.
const BuildTag = "anticheat"

// Enabled reports whether this binary was built with a review policy in it.
const Enabled = true
