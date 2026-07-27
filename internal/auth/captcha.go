package auth

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/typemore/typemore-server/internal/platform/httpx"
)

// maxCaptchaBodyBytes bounds what the gate will buffer while it looks for the
// token. The gated bodies are an email, a password and a display name; a body
// larger than this is not a client we owe a captcha round-trip, so it is turned
// away with the same answer as a missing token.
const maxCaptchaBodyBytes = 64 << 10

// captchaProbe is the sliver of a gated request body the gate reads. It is
// decoded leniently (unlike the handlers' strict decode) because the gate runs
// before validation: its only question is "is there a token".
type captchaProbe struct {
	TurnstileToken string `json:"turnstileToken"`
}

// requireCaptcha is middleware gating the abuse-prone endpoints on a valid
// captcha token. It is the FIRST link in their chain — ahead of the per-IP
// limiter — for two reasons:
//
//   - The limiter exists to ration a scarce resource (outbound email, argon2id
//     memory) among callers who have at least proven they are a browser with a
//     human behind it. Spending a caller's token budget before that proof lets
//     an unsolvable flood of anonymous requests exhaust an IP's bucket and
//     take the legitimate user behind that NAT down with it.
//   - A caller who cannot produce a token must learn that, not "429 later".
//     captcha_required is actionable; rate_limited would send a client that
//     silently dropped its widget into a retry loop.
//
// When s.captcha is nil the gate disappears entirely: the wrapper is not even
// built, so a disabled deployment runs byte-for-byte the pre-captcha chain.
func (s *Service) requireCaptcha(next http.Handler) http.Handler {
	if s.captcha == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Read the body here and hand it back below: the handler still needs it,
		// and the gate cannot ask the question without it. The limit reader
		// takes one byte past the cap so an oversized body is detectable.
		body, err := io.ReadAll(io.LimitReader(r.Body, maxCaptchaBodyBytes+1))
		if err != nil || len(body) > maxCaptchaBodyBytes {
			s.writeError(w, r, apiErrCaptchaRequired)
			return
		}

		// A body that does not parse carries no token. The gate runs before
		// validation by design, so that is captcha_required, not bad_request:
		// no unproven caller gets to find out how our parser feels about them.
		var probe captchaProbe
		_ = json.Unmarshal(body, &probe)
		token := strings.TrimSpace(probe.TurnstileToken)
		if token == "" {
			s.writeError(w, r, apiErrCaptchaRequired)
			return
		}

		// Same client IP the limiter keys on — one derivation, one behaviour
		// behind a future reverse proxy (see clientIP).
		if err := s.captcha.Verify(r.Context(), token, httpx.ClientIP(r)); err != nil {
			// The reason stays in the logs. A rejected token and an unreachable
			// provider are one code to the client: it can act on neither, and
			// the difference is exactly what a prober would want.
			s.log.WarnContext(r.Context(), "captcha verification failed",
				"err", err, "path", r.URL.Path)
			s.writeError(w, r, apiErrCaptchaFailed)
			return
		}

		r.Body = io.NopCloser(bytes.NewReader(body))
		next.ServeHTTP(w, r)
	})
}
