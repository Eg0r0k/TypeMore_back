package main

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/typemore/typemore-server/internal/platform"
)

// TestNewCaptchaVerifier guards the typed-nil trap this bridge exists for.
// turnstile.New returns (*turnstile.Verifier)(nil) for an empty secret; handing
// that straight back would produce a NON-nil auth.CaptchaVerifier wrapping a nil
// pointer, the domain's "nil means disabled" check would pass, and every gated
// request would dereference it. `assert.Nil` on an interface is exactly the
// assertion that catches a regression here — `== nil` in the bridge is not.
func TestNewCaptchaVerifier(t *testing.T) {
	assert.Nil(t, newCaptchaVerifier(platform.Config{}),
		"no secret must yield a nil interface, not a typed nil")

	assert.NotNil(t, newCaptchaVerifier(platform.Config{TurnstileSecret: "1x0000000000000000000000000000000AA"}),
		"a configured secret must yield a live verifier")
}
