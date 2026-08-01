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

// CaptchaVerifier checks a captcha token minted by the client's challenge
// widget. Like Mailer it is consumer-declared here: the real implementation
// (Cloudflare Turnstile) lives in internal/platform/turnstile, and the
// composition root wires the two together, so the domain owns the contract
// without importing the infrastructure.
//
// A NIL CaptchaVerifier means captcha is disabled and every gate is a no-op —
// the dev default, and the reason "is captcha on?" is asked exactly once, when
// the verifier is constructed, rather than at each call site.
type CaptchaVerifier interface {
	// Verify returns nil when the token is valid. remoteIP is the client's
	// address, forwarded to the provider when it is meaningful. Every failure
	// mode is an error and callers treat them identically: the reason a token
	// was refused is operator information, never client information.
	Verify(ctx context.Context, token, remoteIP string) error
}

// Service holds the auth business logic. Handlers call it; it calls the store,
// mailer, limiter, and captcha interfaces.
type Service struct {
	store    Store
	sessions SessionStore
	mailer   Mailer
	limiter  RateLimiter
	// captcha gates the abuse-prone endpoints. Nil = disabled; see
	// CaptchaVerifier and captcha.go.
	captcha CaptchaVerifier
	cfg     Config
	log     *slog.Logger
	// restrictions answers "is this account banned right now", for the /me
	// flag only. Nil means nothing is wired and nobody is restricted, which is
	// what every test that does not care about moderation gets.
	restrictions Restrictions
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
func NewService(store Store, sessions SessionStore, mailer Mailer, limiter RateLimiter,
	captcha CaptchaVerifier, cfg Config, log *slog.Logger,
) *Service {
	wait := cfg.HashWait
	if wait <= 0 {
		wait = DefaultHashWait
	}
	s := &Service{
		store:    store,
		sessions: sessions,
		mailer:   mailer,
		limiter:  limiter,
		captcha:  captcha,
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
	// Restricted is true while the account is under an active ban.
	//
	// It is a bare boolean and it stays one. No reason, no expiry, no issuer:
	// the banner a restricted player sees is deliberately opaque, and a field
	// that is not in this struct is a field no handler can leak
	// (docs/MODERATION.md). The flow responses (register, login) carry it as
	// `false`: toUserView does not consult moderation, because a flow returns
	// the account that was just created or logged into and GET /me is the read
	// that answers "what is the state of this account now".
	Restricted bool `json:"restricted"`
	// The two privacy switches (docs/PROFILE.md, "Public profiles"). They ride
	// on /me so the settings UI reads its state from the session read it
	// already makes, and they are wire facts about the CALLER's own account —
	// what a stranger may see is the public surface's decision, not a field
	// here.
	ProfilePublic  bool `json:"profilePublic"`
	KeyboardPublic bool `json:"keyboardPublic"`
	// Permissions is the caller's expanded permission set (permissions.go) —
	// what tells the client which surfaces to render. Deliberately the
	// PERMISSIONS and not the role: the client renders capabilities, and the
	// role is a storage detail the map behind this list is free to refactor.
	// Omitted entirely for a plain player, so the common payload is unchanged.
	Permissions []string `json:"permissions,omitempty"`
}

func toUserView(u User) userView {
	return userView{
		ID: u.ID, DisplayName: u.DisplayName, CreatedAt: u.CreatedAt,
		ProfilePublic: u.ProfilePublic, KeyboardPublic: u.KeyboardPublic,
		Permissions: u.Permissions(),
	}
}

// Restrictions answers whether an account is under an active ban. Declared here
// at the consumer: auth needs one boolean and must not be able to reach a
// reason or an expiry, because neither is ever shown to the player.
type Restrictions interface {
	IsRestricted(ctx context.Context, userID uuid.UUID) (bool, error)
}

// WithRestrictions wires the moderation lookup behind GET /me. Without it the
// flag is always false, which is the correct behaviour for a deployment that
// has no moderation store and for every test that is not about bans.
func (s *Service) WithRestrictions(r Restrictions) *Service {
	s.restrictions = r
	return s
}

// BootstrapAdmins promotes the configured admin emails (TYPEMORE_ADMINS) and
// returns how many accounts changed. Called once by the composition root at
// startup — promotion only, verified identities only (PromoteAdmins in
// queries.sql carries the full rationale). An empty list is a no-op, and it is
// also the composition root's signal not to mount the admin subtree at all.
func (s *Service) BootstrapAdmins(ctx context.Context, emails []string) (int64, error) {
	if len(emails) == 0 {
		return 0, nil
	}
	return s.store.PromoteAdmins(ctx, emails)
}
