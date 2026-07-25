package auth

import (
	"errors"
	"net/http"
)

// This file holds the account-management addendum for accounts that started as
// OAuth-only: adding an email identity (with verification) and setting a
// first-time password. Both require a live session and are mounted in the
// authenticated group of AuthRoutes.

type addEmailRequest struct {
	Email string `json:"email"`
}

// handleAddEmail attaches an email identity to the current (typically OAuth-only)
// account and sends a verification link. The identity is created UNVERIFIED; the
// existing POST /verify flow flips it to verified, reusing the same token
// machinery as registration. A collision with an email already claimed by
// another account surfaces as account_exists_use_linking (the unique
// provider/subject constraint, and the verified-email exclusion at verify time).
func (s *Service) handleAddEmail(w http.ResponseWriter, r *http.Request) {
	user, ok := UserFrom(r.Context())
	if !ok {
		s.writeError(w, r, apiErrUnauthorized)
		return
	}
	var req addEmailRequest
	if !s.decodeJSON(w, r, &req) {
		return
	}
	email := normalizeEmail(req.Email)
	if !validateEmail(email) {
		s.writeError(w, r, apiErrBadRequest("a valid email is required"))
		return
	}

	ctx := r.Context()
	identities, err := s.store.IdentitiesByUser(ctx, user.ID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	// Only accounts without an email identity may add one; changing an existing
	// email is a separate concern (not in scope).
	for _, id := range identities {
		if id.Provider == ProviderEmail {
			s.writeError(w, r, apiErrEmailAlreadySet)
			return
		}
	}

	// Create the unverified email identity. A unique(provider, provider_subject)
	// collision means the address already has an email identity elsewhere, and
	// the verified-email exclusion means it is verified under another user; both
	// are the no-auto-link signal.
	if _, err := s.store.LinkIdentity(ctx, user.ID, IdentityParams{
		Provider:      ProviderEmail,
		Subject:       email,
		Email:         email,
		EmailVerified: false,
	}); err != nil {
		if errors.Is(err, ErrIdentityExists) || errors.Is(err, ErrEmailOwnedByOtherUser) {
			s.writeError(w, r, apiErrAccountExistsUseLinking)
			return
		}
		s.writeError(w, r, err)
		return
	}

	if serr := s.sendVerification(ctx, user.ID, email); serr != nil {
		// The identity exists; a failed email should not fail the request. The
		// user can trigger a resend. (Consistent with register.)
		s.log.ErrorContext(ctx, "send add-email verification failed", "err", serr)
	}
	s.writeJSON(w, http.StatusOK, statusMessage(genericEmailAccepted))
}

type setPasswordRequest struct {
	Password string `json:"password"`
}

// handleSetPassword sets a first-time password for an account that has a
// verified email identity but no credentials yet (e.g. an OAuth account that
// added and verified an email). It is a one-time set: an existing credential
// row makes this a password_already_set conflict, and changing a password stays
// under the reset flow. After a successful set, email+password login works.
func (s *Service) handleSetPassword(w http.ResponseWriter, r *http.Request) {
	user, ok := UserFrom(r.Context())
	if !ok {
		s.writeError(w, r, apiErrUnauthorized)
		return
	}
	var req setPasswordRequest
	if !s.decodeJSON(w, r, &req) {
		return
	}
	if err := validatePassword(req.Password); err != nil {
		s.writeError(w, r, err)
		return
	}

	ctx := r.Context()
	identities, err := s.store.IdentitiesByUser(ctx, user.ID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	hasVerifiedEmail := false
	for _, id := range identities {
		if id.Provider == ProviderEmail && id.EmailVerified {
			hasVerifiedEmail = true
			break
		}
	}
	if !hasVerifiedEmail {
		s.writeError(w, r, apiErrNoVerifiedEmail)
		return
	}

	// One-time set: refuse when a credential already exists.
	if _, err := s.store.Credential(ctx, user.ID); err == nil {
		s.writeError(w, r, apiErrPasswordAlreadySet)
		return
	} else if !errors.Is(err, ErrNotFound) {
		s.writeError(w, r, err)
		return
	}

	hash, err := s.hashPassword(r.Context(), req.Password)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.store.UpdateCredential(ctx, user.ID, hash); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, statusMessage("password set; you can now sign in with email and password"))
}
