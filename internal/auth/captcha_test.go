package auth_test

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/typemore/typemore-server/internal/platform/turnstile"
)

// The gate is exercised through the real Turnstile verifier pointed at a fake
// siteverify server, not a hand-rolled stub: the interesting failures (a
// provider rejection, an unreachable provider) live in the seam between the two,
// and a stub would assert only that the stub was called.

// genericAccepted is the anti-enumeration body register / resend /
// reset-request return in every case. Pinned literally so "captcha did not
// change the anti-enumeration answer" is anchored to the wire format rather
// than to the test's own other assertions.
const genericAccepted = `{"message":"if that email can receive mail, a message is on its way","status":"ok"}` + "\n"

// fakeSiteVerify is a stand-in for Cloudflare whose verdict a test can flip.
type fakeSiteVerify struct {
	*httptest.Server
	body  atomic.Value // string
	calls atomic.Int64
}

func newFakeSiteVerify(t *testing.T) *fakeSiteVerify {
	t.Helper()
	f := &fakeSiteVerify{}
	f.body.Store(`{"success":true}`)
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		f.calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(f.body.Load().(string)))
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeSiteVerify) reject(codes string) {
	f.body.Store(`{"success":false,"error-codes":` + codes + `}`)
}

// withCaptcha enables the gate against the given endpoint.
func withCaptcha(endpoint string) func(*serverOpts) {
	return func(o *serverOpts) {
		o.captcha = turnstile.New("test-secret",
			turnstile.WithEndpoint(endpoint),
			turnstile.WithTimeout(2*time.Second))
	}
}

// gatedPaths are exactly the endpoints the contract puts behind the gate.
var gatedPaths = []string{"/register", "/verify/resend", "/password-reset/request"}

// --- disabled mode ---

// TestCaptchaDisabledNeedsNoToken pins the dev default: with no secret
// configured the endpoints behave as they did before captcha existed.
func TestCaptchaDisabledNeedsNoToken(t *testing.T) {
	h := newHarness(t) // no captcha verifier

	body := readBody(t, h.register("nocaptcha@example.com", "password-1234", "NoCaptcha"))
	assert.Equal(t, genericAccepted, body, "a token-less register must still succeed")

	// A client that sends a token anyway (frontend configured, backend not) is
	// not punished for it: the field is accepted and ignored.
	resp := h.post(authBase+"/password-reset/request", map[string]string{
		"email": "nocaptcha@example.com", "turnstileToken": "ignored",
	})
	assert.Equal(t, genericAccepted, readBody(t, resp),
		"a token sent to a disabled server must be ignored, not rejected")
}

// --- enabled: missing token ---

// TestCaptchaRequiredWhenTokenMissing covers every gated endpoint, and asserts
// the provider is never consulted: a request with nothing to verify must not
// cost an outbound round-trip.
func TestCaptchaRequiredWhenTokenMissing(t *testing.T) {
	fake := newFakeSiteVerify(t)
	h := newHarness(t, withCaptcha(fake.URL))

	for _, path := range gatedPaths {
		resp := h.post(authBase+path, map[string]string{
			"email": "gated@example.com", "password": "password-1234", "displayName": "Gated",
		})
		require.Equal(t, http.StatusBadRequest, resp.StatusCode, path)
		assert.Equal(t, "captcha_required", decodeInto[errResponse](t, resp).Error, path)
	}

	// An explicitly empty / whitespace token is the same as none.
	resp := h.post(authBase+"/register", map[string]string{
		"email": "gated@example.com", "password": "password-1234",
		"displayName": "Gated", "turnstileToken": "   ",
	})
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.Equal(t, "captcha_required", decodeInto[errResponse](t, resp).Error)

	assert.Zero(t, fake.calls.Load(), "siteverify must not be called without a token")
	assert.Zero(t, h.mailer.count(), "a gated-out request must not send mail")
}

// TestCaptchaUngatedEndpointsUnaffected pins the blast radius: login and the
// token-consuming endpoints gain no captcha field and no gate.
func TestCaptchaUngatedEndpointsUnaffected(t *testing.T) {
	fake := newFakeSiteVerify(t)
	h := newHarness(t, withCaptcha(fake.URL))

	resp := h.login("nobody@example.com", "password-1234")
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode,
		"login is not gated; it answers invalid_credentials as always")
	assert.Equal(t, "invalid_credentials", decodeInto[errResponse](t, resp).Error)

	resp = h.post(authBase+"/verify", map[string]string{"token": "nonsense"})
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.Equal(t, "invalid_token", decodeInto[errResponse](t, resp).Error)

	assert.Zero(t, fake.calls.Load(), "ungated endpoints must not consult the provider")
}

// --- enabled: provider says no ---

func TestCaptchaFailedWhenProviderRejects(t *testing.T) {
	fake := newFakeSiteVerify(t)
	fake.reject(`["invalid-input-response"]`)
	h := newHarness(t, withCaptcha(fake.URL))

	for _, path := range gatedPaths {
		resp := h.post(authBase+path, map[string]string{
			"email": "rejected@example.com", "password": "password-1234",
			"displayName": "Rejected", "turnstileToken": "bad-token",
		})
		require.Equal(t, http.StatusBadRequest, resp.StatusCode, path)
		assert.Equal(t, "captcha_failed", decodeInto[errResponse](t, resp).Error, path)
	}
	assert.Equal(t, int64(len(gatedPaths)), fake.calls.Load())
	assert.Zero(t, h.mailer.count(), "a rejected token must not send mail")
}

// TestCaptchaFailedLeaksNothing checks that Cloudflare's own error codes stay
// server-side: they name the reason a token was refused, which is precisely
// what a caller probing the gate would like to be told.
func TestCaptchaFailedLeaksNothing(t *testing.T) {
	fake := newFakeSiteVerify(t)
	fake.reject(`["timeout-or-duplicate","invalid-input-secret"]`)
	h := newHarness(t, withCaptcha(fake.URL))

	body := readBody(t, h.post(authBase+"/register", map[string]string{
		"email": "leak@example.com", "password": "password-1234",
		"displayName": "Leak", "turnstileToken": "spent",
	}))
	assert.NotContains(t, body, "timeout-or-duplicate")
	assert.NotContains(t, body, "invalid-input-secret")
	assert.NotContains(t, body, "secret")
}

// --- enabled: provider unreachable ---

// TestCaptchaTransportFailureIsCaptchaFailed is the one that matters
// operationally: when Cloudflare is unreachable the request is a client-facing
// 400 captcha_failed, NOT a 500. A 500 would be a lie (the server is fine), it
// would page the wrong team, and it would tell the client to give up rather
// than solve a fresh challenge.
func TestCaptchaTransportFailureIsCaptchaFailed(t *testing.T) {
	fake := newFakeSiteVerify(t)
	endpoint := fake.URL
	fake.Close() // provider is now unreachable: connection refused

	h := newHarness(t, withCaptcha(endpoint))

	for _, path := range gatedPaths {
		resp := h.post(authBase+path, map[string]string{
			"email": "outage@example.com", "password": "password-1234",
			"displayName": "Outage", "turnstileToken": "perfectly-good-token",
		})
		require.Equal(t, http.StatusBadRequest, resp.StatusCode,
			"%s: an upstream outage is not a server error", path)

		got := decodeInto[errResponse](t, resp)
		assert.Equal(t, "captcha_failed", got.Error, path)
		// The upstream reason (dial errors, ports, endpoint URL) stays in the log.
		assert.NotContains(t, got.Message, "connect", path)
		assert.NotContains(t, got.Message, "127.0.0.1", path)
		assert.NotContains(t, got.Message, endpoint, path)
	}
	assert.Zero(t, h.mailer.count())
}

// TestCaptchaTimeoutIsCaptchaFailed covers the other half of "the provider did
// not answer": a hung siteverify, bounded by the verifier's own timeout.
func TestCaptchaTimeoutIsCaptchaFailed(t *testing.T) {
	release := make(chan struct{})
	stalled := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	defer func() { close(release); stalled.Close() }()

	h := newHarness(t, func(o *serverOpts) {
		o.captcha = turnstile.New("test-secret",
			turnstile.WithEndpoint(stalled.URL),
			turnstile.WithTimeout(100*time.Millisecond))
	})

	start := time.Now()
	resp := h.post(authBase+"/register", map[string]string{
		"email": "slow@example.com", "password": "password-1234",
		"displayName": "Slow", "turnstileToken": "token",
	})
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.Equal(t, "captcha_failed", decodeInto[errResponse](t, resp).Error)
	assert.Less(t, time.Since(start), 5*time.Second, "the gate must not wait on a wedged provider")
}

// --- ordering ---

// TestCaptchaGatePrecedesRateLimiter pins the contract's ordering. The bucket
// is drained by a legitimate request, so the follow-up would be a 429 if the
// limiter ran first; because the gate runs first it is a 400 captcha_required,
// and the token-less caller never touched the IP's budget.
func TestCaptchaGatePrecedesRateLimiter(t *testing.T) {
	fake := newFakeSiteVerify(t)
	h := newHarness(t, withCaptcha(fake.URL), func(o *serverOpts) {
		o.rateEvery = time.Hour // no refill within the test
		o.rateBurst = 1
	})

	// Spend the single token on a fully valid request.
	requireStatus(t, h.post(authBase+"/register", map[string]string{
		"email": "first@example.com", "password": "password-1234",
		"displayName": "First", "turnstileToken": "good",
	}), http.StatusOK)

	// The bucket is empty. A token-less caller still hears about the captcha.
	resp := h.post(authBase+"/register", map[string]string{
		"email": "second@example.com", "password": "password-1234", "displayName": "Second",
	})
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.Equal(t, "captcha_required", decodeInto[errResponse](t, resp).Error,
		"the gate must answer before the limiter's decision is surfaced")

	// And the limiter is otherwise untouched: a request that clears the gate
	// still meets the empty bucket.
	resp = h.post(authBase+"/register", map[string]string{
		"email": "third@example.com", "password": "password-1234",
		"displayName": "Third", "turnstileToken": "good",
	})
	require.Equal(t, http.StatusTooManyRequests, resp.StatusCode)
	assert.Equal(t, "rate_limited", decodeInto[errResponse](t, resp).Error)
}

// TestCaptchaAntiEnumerationUnchanged is TestAntiEnumeration run with the gate
// enabled: once a token passes, the three generic answers must be byte-identical
// to each other and to the pre-captcha wire format. The gate is allowed to turn
// callers away; it is not allowed to become an existence oracle.
func TestCaptchaAntiEnumerationUnchanged(t *testing.T) {
	fake := newFakeSiteVerify(t)
	h := newHarness(t, withCaptcha(fake.URL))
	const email, password = "enum-captcha@example.com", "password-1234"

	register := func(addr string) string {
		return readBody(t, h.post(authBase+"/register", map[string]string{
			"email": addr, "password": password, "displayName": "EnumCaptcha",
			"turnstileToken": "good",
		}))
	}

	first := register(email)
	assert.Equal(t, genericAccepted, first, "the gate must not change the success body")

	second := register(email) // taken email
	assert.Equal(t, first, second, "taken-email register must match fresh register")
	assert.Equal(t, 1, h.mailer.count(), "no second email for a taken address")

	reset := readBody(t, h.post(authBase+"/password-reset/request", map[string]string{
		"email": "ghost-captcha@example.com", "turnstileToken": "good",
	}))
	assert.Equal(t, first, reset, "unknown-email reset must match the generic success")
}
