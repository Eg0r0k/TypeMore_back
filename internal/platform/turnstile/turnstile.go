// Package turnstile verifies Cloudflare Turnstile captcha responses against
// the siteverify endpoint. It is platform infrastructure and imports no domain:
// the auth domain declares its own CaptchaVerifier interface and the
// composition root hands it a *Verifier (keeping platform domain-free per
// BACKEND.md §2). The method set here matches that interface exactly, so no
// adapter is needed — unlike mail, there is no domain type in the signature.
package turnstile

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// SiteVerifyURL is Cloudflare's published siteverify endpoint.
const SiteVerifyURL = "https://challenges.cloudflare.com/turnstile/v0/siteverify"

// DefaultTimeout bounds one siteverify round-trip. Verification sits in front
// of a user-facing POST, so a slow or wedged upstream must fail fast rather
// than hold the request (and its goroutine) open: five seconds is far longer
// than the call ever legitimately takes and short enough that a Cloudflare
// outage degrades into prompt captcha_failed responses instead of a pile-up.
const DefaultTimeout = 5 * time.Second

// maxResponseBytes caps how much of the siteverify reply is read. The documented
// body is a handful of fields; anything larger is a compromised or misdirected
// endpoint, and streaming it into a decoder would be its own small DoS.
const maxResponseBytes = 64 << 10

// Verifier checks Turnstile tokens against siteverify.
//
// A Verifier is safe for concurrent use and is meant to be built once at
// startup. Construction is where "captcha is disabled" is decided: New returns
// nil for an empty secret, so no call site ever has to re-ask the question.
type Verifier struct {
	secret   string
	endpoint string
	client   *http.Client
	timeout  time.Duration
}

// Option customizes a Verifier at construction.
type Option func(*Verifier)

// WithHTTPClient supplies the client used for siteverify calls. Tests point it
// (together with WithEndpoint) at an httptest server; production deployments
// can use it to attach a proxy or shared transport.
func WithHTTPClient(c *http.Client) Option {
	return func(v *Verifier) {
		if c != nil {
			v.client = c
		}
	}
}

// WithEndpoint overrides the siteverify URL. Intended for tests.
func WithEndpoint(endpoint string) Option {
	return func(v *Verifier) {
		if endpoint != "" {
			v.endpoint = endpoint
		}
	}
}

// WithTimeout overrides DefaultTimeout. A non-positive value is ignored: the
// call is always bounded, whatever the caller passes.
func WithTimeout(d time.Duration) Option {
	return func(v *Verifier) {
		if d > 0 {
			v.timeout = d
		}
	}
}

// New builds a Verifier for the given Turnstile secret key. An EMPTY secret
// returns nil — captcha is disabled, expressed once, here. Callers must not
// store that nil in an interface variable without checking it (a typed nil is
// not a nil interface); see the composition root for the one place that does.
func New(secret string, opts ...Option) *Verifier {
	if secret == "" {
		return nil
	}
	v := &Verifier{
		secret:   secret,
		endpoint: SiteVerifyURL,
		timeout:  DefaultTimeout,
	}
	for _, opt := range opts {
		opt(v)
	}
	if v.client == nil {
		v.client = &http.Client{Timeout: v.timeout}
	}
	return v
}

// RejectedError reports that siteverify answered `success: false`. Codes are
// Cloudflare's `error-codes` (e.g. invalid-input-response, timeout-or-duplicate)
// and exist for operator diagnosis only: the HTTP layer collapses every
// verification failure — rejection, transport error, malformed reply — into one
// opaque client-facing code, so a probing client learns nothing about why.
type RejectedError struct {
	Codes []string
}

func (e *RejectedError) Error() string {
	if len(e.Codes) == 0 {
		return "turnstile: token rejected"
	}
	return "turnstile: token rejected: " + strings.Join(e.Codes, ", ")
}

// Verify POSTs the token to siteverify and returns nil when Cloudflare accepts
// it. remoteIP is the caller's IP; it is sent as `remoteip` only when it parses
// as an address, since a bogus value is worse than an absent one. Every failure
// mode — transport error, non-200, unparseable body, `success:false` — is an
// error; the caller is expected to treat them identically.
func (v *Verifier) Verify(ctx context.Context, token, remoteIP string) error {
	form := url.Values{
		"secret":   {v.secret},
		"response": {token},
	}
	if net.ParseIP(remoteIP) != nil {
		form.Set("remoteip", remoteIP)
	}

	ctx, cancel := context.WithTimeout(ctx, v.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, v.endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("turnstile: build siteverify request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := v.client.Do(req)
	if err != nil {
		return fmt.Errorf("turnstile: siteverify request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("turnstile: siteverify returned status %d", resp.StatusCode)
	}

	var out struct {
		Success    bool     `json:"success"`
		ErrorCodes []string `json:"error-codes"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&out); err != nil {
		return fmt.Errorf("turnstile: decode siteverify response: %w", err)
	}
	if !out.Success {
		return &RejectedError{Codes: out.ErrorCodes}
	}
	return nil
}
