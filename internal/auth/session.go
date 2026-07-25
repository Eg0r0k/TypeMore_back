package auth

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
)

// sessionRefreshInterval bounds how often a session's sliding expiry is written
// back: we only extend expires_at / last_seen_at once the session has not been
// touched for this long, so a burst of requests is not a burst of UPDATEs.
const sessionRefreshInterval = 1 * time.Hour

// issueSession mints a new session for userID, persists only its hash, and sets
// the session cookie on w. The plaintext token exists only inside the cookie.
func (s *Service) issueSession(ctx context.Context, w http.ResponseWriter, userID uuid.UUID) error {
	token, hash, err := newToken()
	if err != nil {
		return err
	}
	expiresAt := s.now().Add(s.cfg.SessionTTL)
	if _, err := s.sessions.CreateSession(ctx, hash, userID, expiresAt); err != nil {
		return err
	}
	http.SetCookie(w, s.sessionCookie(token, s.cfg.SessionTTL))
	return nil
}

// authenticate resolves the session cookie on r to a User, applying sliding
// expiry. It returns apiErrUnauthorized when there is no valid session. A
// present-but-expired session is treated as unauthenticated and best-effort
// deleted.
func (s *Service) authenticate(ctx context.Context, r *http.Request) (User, error) {
	cookie, err := r.Cookie(s.cfg.CookieName)
	if err != nil {
		return User{}, apiErrUnauthorized
	}
	hash, err := hashToken(cookie.Value)
	if err != nil {
		return User{}, apiErrUnauthorized
	}

	session, err := s.sessions.SessionByTokenHash(ctx, hash)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return User{}, apiErrUnauthorized
		}
		return User{}, err
	}

	now := s.now()
	if !session.ExpiresAt.After(now) {
		// Expired: clean it up and reject.
		_ = s.sessions.DeleteSession(ctx, hash)
		return User{}, apiErrUnauthorized
	}

	// Sliding expiry: extend, but only occasionally to avoid a write per request.
	if now.Sub(session.LastSeenAt) > sessionRefreshInterval {
		if err := s.sessions.TouchSession(ctx, session.ID, now.Add(s.cfg.SessionTTL)); err != nil {
			// A failed refresh is not fatal to the request; log and continue.
			s.log.WarnContext(ctx, "session refresh failed", "err", err)
		}
	}

	user, err := s.store.User(ctx, session.UserID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			// Session outlived its user (deleted account): reject.
			return User{}, apiErrUnauthorized
		}
		return User{}, err
	}
	return user, nil
}

// Identify resolves the session cookie on r to a User for callers outside the
// HTTP-middleware path (notably the WebSocket upgrade, which is not behind
// RequireAuth). It returns (User{}, false) for an anonymous/guest connection.
func (s *Service) Identify(r *http.Request) (User, bool) {
	u, err := s.authenticate(r.Context(), r)
	if err != nil {
		return User{}, false
	}
	return u, true
}

// logout deletes the current session (if any) and clears the cookie. It is
// idempotent: no cookie or an unknown token still succeeds.
func (s *Service) logout(ctx context.Context, r *http.Request, w http.ResponseWriter) error {
	if cookie, err := r.Cookie(s.cfg.CookieName); err == nil {
		if hash, herr := hashToken(cookie.Value); herr == nil {
			if derr := s.sessions.DeleteSession(ctx, hash); derr != nil {
				return derr
			}
		}
	}
	http.SetCookie(w, s.sessionCookie("", -time.Hour)) // expire immediately
	return nil
}

// sessionCookie builds the session cookie. httpOnly keeps it away from JS;
// SameSite=Lax blocks it on cross-site POSTs (defense in depth with the Origin
// check); Secure is config-driven (true in prod). A non-positive ttl produces an
// already-expired deletion cookie.
func (s *Service) sessionCookie(value string, ttl time.Duration) *http.Cookie {
	c := &http.Cookie{
		Name:     s.cfg.CookieName,
		Value:    value,
		Path:     "/",
		Domain:   s.cfg.CookieDomain,
		HttpOnly: true,
		Secure:   s.cfg.CookieSecure,
		SameSite: http.SameSiteLaxMode,
	}
	if ttl > 0 {
		c.Expires = s.now().Add(ttl)
		c.MaxAge = int(ttl.Seconds())
	} else {
		c.Expires = time.Unix(0, 0)
		c.MaxAge = -1
	}
	return c
}
