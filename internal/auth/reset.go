package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"
)

type resetRequestBody struct {
	Email string `json:"email"`
}

// handleResetRequest sends a password-reset link if the email has an account.
// It always returns the same generic success (anti-enum) and is rate-limited.
func (s *Service) handleResetRequest(w http.ResponseWriter, r *http.Request) {
	var req resetRequestBody
	if !s.decodeJSON(w, r, &req) {
		return
	}
	email := normalizeEmail(req.Email)
	ctx := r.Context()

	if identity, err := s.store.EmailIdentity(ctx, email); err == nil {
		if serr := s.sendPasswordReset(ctx, identity.UserID, email); serr != nil {
			s.log.ErrorContext(ctx, "send reset email failed", "err", serr)
		}
	} else if !errors.Is(err, ErrNotFound) {
		s.writeError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, statusMessage(genericEmailAccepted))
}

type resetConfirmBody struct {
	Token       string `json:"token"`
	NewPassword string `json:"newPassword"`
}

// handleResetConfirm consumes a reset token (single-use, 1h), sets the new
// password, and revokes ALL of the user's sessions (standard "my password was
// stolen" response). The user must log in again afterward.
func (s *Service) handleResetConfirm(w http.ResponseWriter, r *http.Request) {
	var req resetConfirmBody
	if !s.decodeJSON(w, r, &req) {
		return
	}
	if err := validatePassword(req.NewPassword); err != nil {
		s.writeError(w, r, err)
		return
	}
	hash, err := hashToken(req.Token)
	if err != nil {
		s.writeError(w, r, apiErrInvalidToken)
		return
	}

	ctx := r.Context()
	tok, err := s.store.UseEmailToken(ctx, hash, PurposeReset)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			s.writeError(w, r, apiErrInvalidToken)
			return
		}
		s.writeError(w, r, err)
		return
	}

	newHash, err := HashPassword(req.NewPassword)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.store.UpdateCredential(ctx, tok.UserID, newHash); err != nil {
		s.writeError(w, r, err)
		return
	}
	// Completing a reset proves control of the mailbox, so the email is now
	// verified too (covers the "registered but never verified, then reset" case).
	if err := s.store.MarkEmailVerified(ctx, tok.UserID); err != nil {
		s.writeError(w, r, err)
		return
	}
	// Revoke every session: anyone holding a stolen session is logged out.
	if err := s.sessions.DeleteUserSessions(ctx, tok.UserID); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, statusMessage("password updated; sign in with your new password"))
}

// sendPasswordReset issues a reset token and emails the link.
func (s *Service) sendPasswordReset(ctx context.Context, userID uuid.UUID, email string) error {
	token, err := s.issueEmailToken(ctx, userID, PurposeReset, resetTokenTTL)
	if err != nil {
		return err
	}
	link := s.frontendLink("/reset-password", token)
	return s.mailer.Send(ctx, Mail{
		To:      email,
		Subject: "Reset your TypeMore password",
		Body: fmt.Sprintf("A password reset was requested for your TypeMore account.\n\n"+
			"Reset it by opening:\n%s\n\nThis link expires in 1 hour. "+
			"If you did not request this, you can ignore this message.\n", link),
	})
}
