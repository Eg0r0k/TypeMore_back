package auth

import (
	"context"
	"log/slog"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// Token lifetimes. Verification links last a day; reset links are short-lived
// because a reset grants a password change.
const (
	verifyTokenTTL = 24 * time.Hour
	resetTokenTTL  = 1 * time.Hour
)

// Display-name / password bounds. Display names are 3–20 characters from a
// conservative charset and unique case-insensitively; the users table enforces
// the same rules (citext UNIQUE + CHECK) as the last line of defense.
const (
	displayNameMinLen = 3
	displayNameMaxLen = 20
	passwordMinLen    = 8
	passwordMaxLen    = 128
)

// displayNameRe is the allowed display-name charset; mirrors the display_name
// CHECK constraint in the schema.
var displayNameRe = regexp.MustCompile(`^[a-zA-Z0-9_.-]+$`)

// Config is the auth service configuration, populated at the composition root
// from platform.Config. Keeping it here (not importing platform) leaves the
// domain independent of env parsing.
type Config struct {
	// FrontendOrigin is the SPA origin: the required Origin for mutating
	// requests (CSRF) and the base URL for links in emails.
	FrontendOrigin string
	// Session cookie attributes.
	CookieName   string
	CookieDomain string
	CookieSecure bool
	// SessionTTL is the sliding session lifetime.
	SessionTTL time.Duration
	// OAuthRedirectBase is this server's public base URL for provider callbacks.
	OAuthRedirectBase string
	// Providers holds OAuth client credentials keyed by provider name
	// (ProviderGitHub, ProviderGoogle). A provider absent here is disabled.
	Providers map[string]ProviderCredentials
	// HashConcurrency bounds how many argon2id hashes may run at once. Each
	// costs HashCostBytes (19 MiB) of live heap, so this is a memory ceiling
	// expressed as a count — see hashgate.go for why the rate limiter cannot
	// substitute for it. Zero or negative disables the gate, which is only ever
	// correct in a benchmark measuring the unbounded behaviour.
	HashConcurrency int
	// HashWait is how long a request may queue for a hashing slot before it is
	// shed with 503. Zero uses DefaultHashWait.
	HashWait time.Duration
}

// ProviderCredentials are one OAuth provider's client id/secret.
type ProviderCredentials struct {
	ClientID     string
	ClientSecret string
	// Optional endpoint overrides. Empty values use the provider's well-known
	// endpoints; tests point these at a fake provider's httptest server.
	AuthURL     string
	TokenURL    string
	UserInfoURL string
	// EmailsURL is GitHub-specific (its verified-email list endpoint).
	EmailsURL string
}

// Mailer sends a plain-text email. It is consumer-declared here; SMTP and
// test/recorder implementations live in the platform / test layers.
type Mailer interface {
	Send(ctx context.Context, msg Mail) error
}

// Mail is a plain-text message.
type Mail struct {
	To      string
	Subject string
	Body    string
}

// RateLimiter decides whether an action keyed by a string (typically a client
// IP) may proceed. Consumer-declared so an in-memory bucket now can become a
// Redis limiter later.
type RateLimiter interface {
	// Allow reports whether the action for key is permitted right now.
	Allow(key string) bool
}

// Service holds the auth business logic. Handlers call it; it calls the store,
// mailer, and limiter interfaces.
type Service struct {
	store    Store
	sessions SessionStore
	mailer   Mailer
	limiter  RateLimiter
	cfg      Config
	log      *slog.Logger
	// now is time.Now in production; tests may override it.
	now func() time.Time
	// oauth holds the per-provider OAuth configuration built from cfg.Providers.
	oauth map[string]*oauthProvider
	// dummyHash is a valid argon2id hash of a random secret. Login verifies
	// against it when the email is unknown so an attacker cannot distinguish
	// "no such user" from "wrong password" by timing (both pay the hash cost).
	dummyHash string
	// hashes bounds concurrent argon2id work. See hashgate.go: this is the only
	// thing standing between the server and a memory-exhaustion DoS made of
	// ordinary login attempts.
	hashes *hashGate
}

// NewService wires the auth service. store and sessions may be the same
// concrete value (the Postgres adapter) or different (e.g. Redis sessions
// later).
func NewService(store Store, sessions SessionStore, mailer Mailer, limiter RateLimiter, cfg Config, log *slog.Logger) *Service {
	wait := cfg.HashWait
	if wait <= 0 {
		wait = DefaultHashWait
	}
	s := &Service{
		store:    store,
		sessions: sessions,
		mailer:   mailer,
		limiter:  limiter,
		cfg:      cfg,
		log:      log,
		now:      time.Now,
		hashes:   newHashGate(cfg.HashConcurrency, wait),
	}
	// Precompute the timing-decoy hash once, ungated: it runs at startup with no
	// contention, and a gate that could reject it would leave the decoy empty.
	// If hashing fails we fall back to an empty string; VerifyPassword against
	// it simply returns false without panicking.
	if h, err := HashPassword(uuid.NewString()); err == nil {
		s.dummyHash = h
	}
	s.oauth = s.buildProviders()
	return s
}

// hashPassword is the gated HashPassword every request path must use. It
// returns apiErrOverloaded when the server has no hashing capacity.
func (s *Service) hashPassword(ctx context.Context, password string) (string, error) {
	release, ok := s.hashes.acquire(ctx)
	if !ok {
		return "", apiErrOverloaded
	}
	defer release()
	return HashPassword(password)
}

// verifyPassword is the gated VerifyPassword. The decoy verify on the
// unknown-email path goes through it too: it costs exactly as much as a real
// one, so exempting it would leave the hole open through a login form that
// names accounts that do not exist.
func (s *Service) verifyPassword(ctx context.Context, password, encoded string) (bool, error) {
	release, ok := s.hashes.acquire(ctx)
	if !ok {
		return false, apiErrOverloaded
	}
	defer release()
	return VerifyPassword(password, encoded)
}

// normalizeEmail canonicalizes an address for storage and comparison: trimmed
// and lower-cased. citext makes the column case-insensitive too, but we
// normalize on write so provider_subject (plain text) is canonical as well.
func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// validateEmail does a cheap structural check — a full RFC validation is
// pointless since deliverability is proven by the verification email.
func validateEmail(email string) bool {
	at := strings.IndexByte(email, '@')
	// need something before '@', a '.' after it, and no spaces
	return at > 0 && strings.IndexByte(email[at+1:], '.') >= 0 &&
		!strings.ContainsAny(email, " \t\r\n") && utf8.RuneCountInString(email) <= 254
}

// validatePassword enforces length bounds. Composition rules are deliberately
// not imposed (length is what matters); the argon2id cost does the rest.
func validatePassword(pw string) *apiError {
	if n := len(pw); n < passwordMinLen || n > passwordMaxLen {
		return apiErrBadRequest("password must be between 8 and 128 characters")
	}
	return nil
}

// cleanDisplayName trims and validates a display name against the schema rules
// (3–20 characters of [a-zA-Z0-9_.-]). An empty name falls back to a name
// derived from the email local-part; when that derivation is unusable the user
// must pick a name explicitly.
func cleanDisplayName(name, email string) (string, *apiError) {
	name = strings.TrimSpace(name)
	if name == "" {
		if at := strings.IndexByte(email, '@'); at > 0 {
			name = sanitizeDisplayName(email[:at])
		}
		if name == "" {
			return "", apiErrBadRequest("please choose a display name: 3-20 characters using letters, digits, '_', '.', '-'")
		}
		return name, nil
	}
	if !validDisplayName(name) {
		return "", apiErrBadRequest("display name must be 3-20 characters using only letters, digits, '_', '.', '-'")
	}
	return name, nil
}

// validDisplayName reports whether name satisfies the display-name rules. The
// charset is ASCII-only, so byte length equals character count once the
// pattern matches.
func validDisplayName(name string) bool {
	return len(name) >= displayNameMinLen && len(name) <= displayNameMaxLen &&
		displayNameRe.MatchString(name)
}

// sanitizeDisplayName reduces an arbitrary string (OAuth profile name, email
// local-part) to the display-name charset, truncated to the maximum length.
// It returns "" when the result would be too short to be a valid name.
func sanitizeDisplayName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '_', r == '.', r == '-':
			b.WriteRune(r)
		}
		if b.Len() == displayNameMaxLen {
			break
		}
	}
	if b.Len() < displayNameMinLen {
		return ""
	}
	return b.String()
}

// userView is the public JSON shape of a user (GET /me, and returned by flows).
type userView struct {
	ID          uuid.UUID `json:"id"`
	DisplayName string    `json:"displayName"`
	CreatedAt   time.Time `json:"createdAt"`
}

func toUserView(u User) userView {
	return userView(u)
}
