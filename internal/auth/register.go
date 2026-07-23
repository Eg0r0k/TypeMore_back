package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/google/uuid"
)

// genericEmailAccepted is the response returned by register / resend /
// reset-request in ALL cases (success, taken email, unknown email). It reveals
// nothing about whether an account exists — see the anti-enumeration notes.
const genericEmailAccepted = "if that email can receive mail, a message is on its way"

type registerRequest struct {
	Email       string `json:"email"`
	Password    string `json:"password"`
	DisplayName string `json:"displayName"`
}

// handleRegister creates an unverified email account and sends a verification
// link. To prevent account enumeration it ALWAYS returns the same success
// response, and it always performs the password hash work so timing does not
// betray whether the email was already taken.
func (s *Service) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if !s.decodeJSON(w, r, &req) {
		return
	}

	email := normalizeEmail(req.Email)
	if !validateEmail(email) {
		s.writeError(w, r, apiErrBadRequest("a valid email is required"))
		return
	}
	if err := validatePassword(req.Password); err != nil {
		s.writeError(w, r, err)
		return
	}
	displayName, err := cleanDisplayName(req.DisplayName, email)
	if err != nil {
		s.writeError(w, r, err)
		return
	}

	// Always hash: this is the dominant cost, and doing it unconditionally keeps
	// the taken-email path timing-indistinguishable from the happy path.
	hash, herr := HashPassword(req.Password)
	if herr != nil {
		s.writeError(w, r, herr)
		return
	}

	ctx := r.Context()
	if _, lookupErr := s.store.EmailIdentity(ctx, email); lookupErr == nil {
		// Email already registered: do NOT reveal it, do NOT create a second
		// account. Return the same success as the happy path.
		s.writeJSON(w, http.StatusOK, statusMessage(genericEmailAccepted))
		return
	} else if !errors.Is(lookupErr, ErrNotFound) {
		s.writeError(w, r, lookupErr)
		return
	}

	user, _, createErr := s.store.CreateEmailAccount(ctx, EmailAccountParams{
		DisplayName:  displayName,
		Email:        email,
		PasswordHash: hash,
	})
	if createErr != nil {
		s.writeError(w, r, createErr)
		return
	}

	if err := s.sendVerification(ctx, user.ID, email); err != nil {
		// The account exists; a failed email should not 500 the client into
		// retrying (which would hit the taken-email path). Log and succeed;
		// resend is available.
		s.log.ErrorContext(ctx, "send verification email failed", "err", err)
	}
	s.writeJSON(w, http.StatusOK, statusMessage(genericEmailAccepted))
}

type verifyRequest struct {
	Token string `json:"token"`
}

// handleVerify consumes a verification token (single-use, 24h) and marks the
// account's email verified.
func (s *Service) handleVerify(w http.ResponseWriter, r *http.Request) {
	var req verifyRequest
	if !s.decodeJSON(w, r, &req) {
		return
	}
	hash, err := hashToken(req.Token)
	if err != nil {
		s.writeError(w, r, apiErrInvalidToken)
		return
	}

	ctx := r.Context()
	tok, err := s.store.UseEmailToken(ctx, hash, PurposeVerify)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			s.writeError(w, r, apiErrInvalidToken)
			return
		}
		s.writeError(w, r, err)
		return
	}
	if err := s.store.MarkEmailVerified(ctx, tok.UserID); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, statusMessage("email verified; you can now sign in"))
}

type resendRequest struct {
	Email string `json:"email"`
}

// handleResend re-sends a verification link if the email exists and is not yet
// verified. Like register, it always returns the same success (anti-enum) and is
// rate-limited by the middleware.
func (s *Service) handleResend(w http.ResponseWriter, r *http.Request) {
	var req resendRequest
	if !s.decodeJSON(w, r, &req) {
		return
	}
	email := normalizeEmail(req.Email)
	ctx := r.Context()

	if identity, err := s.store.EmailIdentity(ctx, email); err == nil && !identity.EmailVerified {
		if serr := s.sendVerification(ctx, identity.UserID, email); serr != nil {
			s.log.ErrorContext(ctx, "resend verification failed", "err", serr)
		}
	} else if err != nil && !errors.Is(err, ErrNotFound) {
		s.writeError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, statusMessage(genericEmailAccepted))
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// handleLogin authenticates an email account and starts a session. Unknown email
// and wrong password both return the same invalid_credentials (with a decoy hash
// verify on the unknown-email path to equalize timing). A correct password on an
// unverified account returns email_not_verified.
func (s *Service) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if !s.decodeJSON(w, r, &req) {
		return
	}
	email := normalizeEmail(req.Email)
	ctx := r.Context()

	identity, err := s.store.EmailIdentity(ctx, email)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			// Decoy verify so timing matches the real path, then generic error.
			_, _ = VerifyPassword(req.Password, s.dummyHash)
			s.writeError(w, r, apiErrInvalidCredentials)
			return
		}
		s.writeError(w, r, err)
		return
	}

	cred, err := s.store.Credential(ctx, identity.UserID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			_, _ = VerifyPassword(req.Password, s.dummyHash)
			s.writeError(w, r, apiErrInvalidCredentials)
			return
		}
		s.writeError(w, r, err)
		return
	}

	ok, err := VerifyPassword(req.Password, cred.Hash)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if !ok {
		s.writeError(w, r, apiErrInvalidCredentials)
		return
	}
	// Only reveal verification status AFTER the password is proven correct.
	if !identity.EmailVerified {
		s.writeError(w, r, apiErrEmailNotVerified)
		return
	}

	if err := s.issueSession(ctx, w, identity.UserID); err != nil {
		s.writeError(w, r, err)
		return
	}
	user, err := s.store.User(ctx, identity.UserID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, toUserView(user))
}

// sendVerification issues a fresh verification token (invalidating older ones)
// and emails the link.
func (s *Service) sendVerification(ctx context.Context, userID uuid.UUID, email string) error {
	token, err := s.issueEmailToken(ctx, userID, PurposeVerify, verifyTokenTTL)
	if err != nil {
		return err
	}
	link := s.frontendLink("/verify-email", token)
	return s.mailer.Send(ctx, Mail{
		To:      email,
		Subject: "Verify your TypeMore email",
		Body: fmt.Sprintf("Welcome to TypeMore!\n\nConfirm your email by opening:\n%s\n\n"+
			"This link expires in 24 hours. If you did not sign up, ignore this message.\n", link),
	})
}

// issueEmailToken creates a single-use token of the given purpose, first
// invalidating any prior tokens of that purpose for the user so only the newest
// link works. Returns the plaintext token to embed in a link.
func (s *Service) issueEmailToken(ctx context.Context, userID uuid.UUID, purpose string, ttl time.Duration) (string, error) {
	if err := s.store.DeleteUserTokens(ctx, userID, purpose); err != nil {
		return "", err
	}
	token, hash, err := newToken()
	if err != nil {
		return "", err
	}
	if err := s.store.CreateEmailToken(ctx, EmailTokenParams{
		UserID:    userID,
		Purpose:   purpose,
		TokenHash: hash,
		ExpiresAt: s.now().Add(ttl),
	}); err != nil {
		return "", err
	}
	return token, nil
}

// frontendLink builds a link into the SPA carrying a token query parameter.
func (s *Service) frontendLink(path, token string) string {
	return fmt.Sprintf("%s%s?token=%s", s.cfg.FrontendOrigin, path, url.QueryEscape(token))
}

// statusMessage is the small {status, message} success body.
func statusMessage(message string) map[string]string {
	return map[string]string{"status": "ok", "message": message}
}
