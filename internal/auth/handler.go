package auth

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"

	"github.com/go-chi/chi/v5"
)

// AuthRoutes returns the router for the auth endpoints, intended to be mounted
// at /api/v1/auth. Every route is rate-limited per client IP and, for mutating
// methods, Origin-checked. GET /api/v1/me is mounted separately by the caller
// (it is not under /auth) using RequireAuth + HandleMe.
//
// The three endpoints that mail a stranger — register, resend, reset-request —
// additionally sit behind the captcha gate, and the gate is chained BEFORE the
// limiter (see requireCaptcha for why). Everything downstream of it, including
// the order of rate-limiting and the Origin check, is unchanged.
func (s *Service) AuthRoutes() http.Handler {
	r := chi.NewRouter()

	r.Group(func(r chi.Router) {
		r.Use(s.requireCaptcha) // no-op when no verifier is configured
		r.Use(s.rateLimit)      // per-IP token bucket
		r.Use(s.RequireOrigin)  // CSRF: mutating methods must carry the frontend Origin

		r.Post("/register", s.handleRegister)
		r.Post("/verify/resend", s.handleResend)
		r.Post("/password-reset/request", s.handleResetRequest)
	})

	r.Group(func(r chi.Router) {
		r.Use(s.rateLimit)
		r.Use(s.RequireOrigin)

		// Public endpoints (no session required).
		r.Post("/verify", s.handleVerify)
		r.Post("/login", s.handleLogin)
		r.Post("/password-reset/confirm", s.handleResetConfirm)
		r.Get("/oauth/{provider}/start", s.handleOAuthStart)
		r.Get("/oauth/{provider}/callback", s.handleOAuthCallback)

		// Authenticated endpoints.
		r.Group(func(r chi.Router) {
			r.Use(s.RequireAuth)
			r.Post("/logout", s.handleLogout)
			r.Post("/link/{provider}/start", s.handleLinkStart)
			r.Post("/email/add", s.handleAddEmail)
			r.Post("/password/set", s.handleSetPassword)
		})
	})
	return r
}

// HandleMe returns the currently authenticated user. It must be wrapped in
// RequireAuth by the caller.
func (s *Service) HandleMe(w http.ResponseWriter, r *http.Request) {
	user, ok := UserFrom(r.Context())
	if !ok {
		s.writeError(w, r, apiErrUnauthorized)
		return
	}
	s.writeJSON(w, http.StatusOK, toUserView(user))
}

// handleLogout ends the current session.
func (s *Service) handleLogout(w http.ResponseWriter, r *http.Request) {
	if err := s.logout(r.Context(), r, w); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// --- shared HTTP helpers ---

// writeJSON encodes v as the response body with the given status.
func (s *Service) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// The header is already sent; nothing to do but note it.
		s.log.Error("encode response", "err", err)
	}
}

// writeError renders err. Known apiErrors are sent with their status/code;
// anything else is logged (with context, never tokens) and returned as a
// generic 500 so internal details never leak to clients.
func (s *Service) writeError(w http.ResponseWriter, r *http.Request, err error) {
	var apiErr *apiError
	if errors.As(err, &apiErr) {
		s.writeJSON(w, apiErr.status, apiErr)
		return
	}
	s.log.ErrorContext(r.Context(), "auth request failed", "err", err, "path", r.URL.Path)
	s.writeJSON(w, apiErrInternal.status, apiErrInternal)
}

// decodeJSON strictly decodes the request body into dst. On failure it writes a
// bad_request error and returns false, so callers just `if !s.decodeJSON(...) {
// return }`.
func (s *Service) decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		s.writeError(w, r, apiErrBadRequest("request body is not valid JSON"))
		return false
	}
	return true
}

// clientIP extracts the client IP for rate-limit keying. It uses the transport
// RemoteAddr (single-binary assumption); if a reverse proxy is added later,
// trust its forwarded header here instead.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
