package auth

import (
	"encoding/json"
	"errors"
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
	view := toUserView(user)
	if s.restrictions != nil {
		restricted, err := s.restrictions.IsRestricted(r.Context(), user.ID)
		if err != nil {
			// Reported, not fatal. The flag drives an informational banner; the
			// ENFORCEMENT of a ban is in SQL, on the run-submission gate and on
			// every board and replay read, and none of it consults this call.
			// Failing /me because the banner could not be resolved would take
			// the whole session endpoint down for a cosmetic field.
			s.log.Error("resolve account restriction", "err", err, "userId", user.ID)
		}
		view.Restricted = restricted
	}
	s.writeJSON(w, http.StatusOK, view)
}

// HandleUpdateSettings serves PATCH /api/v1/me/settings: the account's two
// privacy switches. Mounted beside /me by the caller (RequireOrigin +
// RequireAuth — a mutating route needs the CSRF check /me itself does not).
//
// The body is a partial: each field updates only when present, so a client can
// flip one switch without knowing (or racing) the other. The response is the
// same userView /me serves, because the settings UI's next question after a
// write is always "so what is the state now".
func (s *Service) HandleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	user, ok := UserFrom(r.Context())
	if !ok {
		s.writeError(w, r, apiErrUnauthorized)
		return
	}
	var req struct {
		ProfilePublic  *bool `json:"profilePublic"`
		KeyboardPublic *bool `json:"keyboardPublic"`
	}
	if !s.decodeJSON(w, r, &req) {
		return
	}
	if req.ProfilePublic == nil && req.KeyboardPublic == nil {
		s.writeError(w, r, apiErrBadRequest("no settings to update"))
		return
	}
	// Resolve the partial against the session's user row — the middleware just
	// read it, so the pair we write is the pair the caller saw.
	p := SettingsParams{ProfilePublic: user.ProfilePublic, KeyboardPublic: user.KeyboardPublic}
	if req.ProfilePublic != nil {
		p.ProfilePublic = *req.ProfilePublic
	}
	if req.KeyboardPublic != nil {
		p.KeyboardPublic = *req.KeyboardPublic
	}
	updated, err := s.store.UpdateUserSettings(r.Context(), user.ID, p)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	view := toUserView(updated)
	if s.restrictions != nil {
		restricted, err := s.restrictions.IsRestricted(r.Context(), user.ID)
		if err != nil {
			s.log.Error("resolve account restriction", "err", err, "userId", user.ID)
		}
		view.Restricted = restricted
	}
	s.writeJSON(w, http.StatusOK, view)
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
