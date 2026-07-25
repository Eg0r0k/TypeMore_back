package platform

import (
	"fmt"
	"time"

	"github.com/caarlos0/env/v11"
)

// envPrefix namespaces every environment variable so that platform config never
// collides with unrelated variables in a shared environment (e.g. a container
// host). ADDR is therefore read from TYPEMORE_ADDR, and so on.
const envPrefix = "TYPEMORE_"

// Config is the fully-resolved server configuration. It is populated once at
// startup from environment variables (see LoadConfig) and then treated as
// immutable — pass it by value to consumers.
//
// Defaults are chosen so that `go run ./cmd/server` works with no environment
// set at all; every field can be overridden via its TYPEMORE_-prefixed variable.
type Config struct {
	// Addr is the TCP address the HTTP server listens on, in Go's host:port
	// form. An empty host (":8080") binds all interfaces.
	Addr string `env:"ADDR" envDefault:":8080"`

	// LogLevel is the minimum slog level to emit: debug, info, warn, or error.
	LogLevel string `env:"LOG_LEVEL" envDefault:"info"`

	// LogFormat selects the slog handler: "json" (default, machine-readable) or
	// "text" (human-readable, handy during local development).
	LogFormat string `env:"LOG_FORMAT" envDefault:"json"`

	// ShutdownTimeout bounds how long graceful shutdown waits for in-flight
	// work to finish before the process exits anyway.
	ShutdownTimeout time.Duration `env:"SHUTDOWN_TIMEOUT" envDefault:"10s"`

	// ReadHeaderTimeout bounds how long the server waits for a request's
	// headers. It is deliberately the only HTTP timeout set: whole-request
	// Read/Write timeouts would kill long-lived WebSocket connections.
	ReadHeaderTimeout time.Duration `env:"READ_HEADER_TIMEOUT" envDefault:"5s"`

	// AllowedOrigins is the WebSocket Origin allow-list (browser CSRF defense).
	// Values are matched against the request Origin host with wildcard support
	// (e.g. "*.typemore.gg"). When empty, all origins are allowed — convenient
	// for local development but you SHOULD set it in production.
	AllowedOrigins []string `env:"ALLOWED_ORIGINS" envSeparator:","`

	// --- Persistence ---

	// DatabaseURL is the PostgreSQL connection string (pgx DSN/URL form).
	DatabaseURL string `env:"DATABASE_URL" envDefault:"postgres://typemore:typemore@localhost:5432/typemore?sslmode=disable"`
	// DBMaxConns caps the pgx connection pool size.
	DBMaxConns int32 `env:"DB_MAX_CONNS" envDefault:"10"`

	// --- Sessions & CSRF ---

	// FrontendOrigin is the browser origin of the SPA. It is the allowed CORS
	// origin, the Origin header value mutating endpoints require (CSRF defense),
	// and the base URL for links embedded in emails.
	FrontendOrigin string `env:"FRONTEND_ORIGIN" envDefault:"http://localhost:5173"`
	// CookieName is the session cookie name.
	CookieName string `env:"COOKIE_NAME" envDefault:"tm_session"`
	// CookieDomain scopes the session cookie; empty = host-only (dev default).
	CookieDomain string `env:"COOKIE_DOMAIN"`
	// CookieSecure sets the Secure attribute; true in prod (HTTPS), false for
	// plain-HTTP local dev.
	CookieSecure bool `env:"COOKIE_SECURE" envDefault:"true"`
	// SessionTTL is the sliding session lifetime (each use extends it).
	SessionTTL time.Duration `env:"SESSION_TTL" envDefault:"720h"` // 30 days

	// --- OAuth ---

	GitHubClientID     string `env:"GITHUB_CLIENT_ID"`
	GitHubClientSecret string `env:"GITHUB_CLIENT_SECRET"`
	GoogleClientID     string `env:"GOOGLE_CLIENT_ID"`
	GoogleClientSecret string `env:"GOOGLE_CLIENT_SECRET"`
	// OAuthRedirectBase is the public base URL of THIS server, used to build the
	// provider redirect URIs (must match what is registered with the provider).
	OAuthRedirectBase string `env:"OAUTH_REDIRECT_BASE" envDefault:"http://localhost:8080"`

	// --- Email / SMTP ---

	// SMTPHost empty disables real mail sending (the server logs the link
	// instead — handy for tests/dev without Mailpit).
	SMTPHost     string `env:"SMTP_HOST"`
	SMTPPort     int    `env:"SMTP_PORT" envDefault:"1025"`
	SMTPUsername string `env:"SMTP_USERNAME"`
	SMTPPassword string `env:"SMTP_PASSWORD"`
	SMTPFrom     string `env:"SMTP_FROM" envDefault:"no-reply@typemore.local"`

	// --- Abuse control ---

	// AuthRateEvery is the token-bucket refill interval and AuthRateBurst the
	// bucket size, applied per client IP across the auth endpoints.
	AuthRateEvery time.Duration `env:"AUTH_RATE_EVERY" envDefault:"1s"`
	AuthRateBurst int           `env:"AUTH_RATE_BURST" envDefault:"10"`

	// RunsRateEvery / RunsRateBurst are the per-user token bucket on POST /runs.
	// A typing session is many runs, so the default is generous: burst 120 with
	// one token refilled every 30s ≈ 120 runs/hour sustained.
	RunsRateEvery time.Duration `env:"RUNS_RATE_EVERY" envDefault:"30s"`
	RunsRateBurst int           `env:"RUNS_RATE_BURST" envDefault:"120"`

	// AuthHashConcurrency bounds how many argon2id password hashes run at once.
	// Each costs ~19 MiB of live heap and is paid BEFORE any check that could
	// reject the caller, so unbounded hashing is a memory-exhaustion DoS made of
	// ordinary login attempts — one the per-IP limiter cannot stop, because a
	// distributed caller never trips it. Zero (the default) derives the limit
	// from AuthHashMemoryBudget or, failing that, from the detected memory
	// ceiling; see docs/PERFORMANCE.md, zone 1.
	AuthHashConcurrency int `env:"AUTH_HASH_CONCURRENCY"`
	// AuthHashMemoryBudget is how much memory hashing may hold at peak, as a
	// byte count. Zero derives it from the process memory ceiling (GOMEMLIMIT,
	// cgroup limit, or MemAvailable) — a quarter of it, leaving the rest for
	// request handling, the replay worker and the database pool.
	AuthHashMemoryBudget uint64 `env:"AUTH_HASH_MEMORY_BUDGET"`
	// AuthHashWait is how long a request may queue for a hashing slot before it
	// is shed with 503. Zero uses the auth domain's default.
	AuthHashWait time.Duration `env:"AUTH_HASH_WAIT"`

	// --- Background cleanup ---

	// AuthCleanupInterval is how often the janitor deletes expired sessions
	// and stale email tokens. Zero or negative disables the janitor.
	AuthCleanupInterval time.Duration `env:"AUTH_CLEANUP_INTERVAL" envDefault:"1h"`

	// --- Replay worker ---
	//
	// The worker recomputes every pending run through the vendored core bundle
	// in goja and sets its authoritative status (docs/REPLAY.md).

	// ReplayEnabled turns the worker on. Disable it to run an API-only replica
	// (or in tests that drive batches by hand).
	ReplayEnabled bool `env:"REPLAY_ENABLED" envDefault:"true"`
	// ReplayPollInterval is how long a worker waits after an empty batch. A
	// full batch is followed immediately, so a backlog drains at full speed.
	ReplayPollInterval time.Duration `env:"REPLAY_POLL_INTERVAL" envDefault:"2s"`
	// ReplayBatchSize is how many runs one transaction claims. It also bounds
	// how long that transaction holds its row locks.
	ReplayBatchSize int32 `env:"REPLAY_BATCH_SIZE" envDefault:"20"`
	// ReplayConcurrency is the number of worker goroutines. Each owns a goja
	// runtime (~a few MB), and they share the queue via FOR UPDATE SKIP LOCKED.
	ReplayConcurrency int `env:"REPLAY_CONCURRENCY" envDefault:"1"`
	// ReplayTimeout bounds one core call. A run that exceeds it is flagged
	// replay_timeout rather than allowed to occupy a worker.
	ReplayTimeout time.Duration `env:"REPLAY_TIMEOUT" envDefault:"5s"`
	// ReplayShutdownGrace bounds how long an in-flight batch may take to finish
	// after the shutdown signal, so verdicts commit instead of rolling back.
	ReplayShutdownGrace time.Duration `env:"REPLAY_SHUTDOWN_GRACE" envDefault:"30s"`

	// --- Replay review policy (docs/REPLAY.md, "Review policy") ---
	//
	// Which plausibility flags are worth what, and how much weighted severity
	// sends a run to a human. Defaults live in internal/replay; these override
	// them, and a typo here is a startup failure rather than a silently
	// disarmed check.

	// ReplayFlagWeights overrides individual flag weights, as
	// "code=weight,code=weight" (e.g. "min-interval=0.4,paste=1.0"). Unlisted
	// codes keep their default; an unknown code is an error.
	ReplayFlagWeights string `env:"REPLAY_FLAG_WEIGHTS"`
	// ReplayReviewThreshold is the suspicion at or above which a run is
	// flagged for review. Zero keeps the calibrated default.
	ReplayReviewThreshold float64 `env:"REPLAY_REVIEW_THRESHOLD"`
	// ReplaySustainedBurstSec is the duration floor for the sustained-burst
	// combination rule. Zero keeps the calibrated default.
	ReplaySustainedBurstSec float64 `env:"REPLAY_SUSTAINED_BURST_SEC"`

	// --- Leaderboards (docs/LEADERBOARDS.md) ---

	// LeaderboardRequireVerifiedEmail gates board eligibility on the player
	// holding a verified email identity. On by default: a board slot is worth
	// more than a throwaway account costs, and requiring an address someone can
	// actually receive mail at is the cheapest barrier that does not punish
	// legitimate players. Changing it takes effect on runs already judged only
	// after `make rebuild-leaderboards`.
	LeaderboardRequireVerifiedEmail bool `env:"LEADERBOARD_REQUIRE_VERIFIED_EMAIL" envDefault:"true"`

	// LeaderboardReplayRateEvery / LeaderboardReplayRateBurst are the per-IP
	// token bucket on the public GET /runs/{id}/replay endpoint. An event log is
	// the heaviest payload the server serves and the route needs no session, so
	// it is limited far more tightly than the authenticated surface.
	LeaderboardReplayRateEvery time.Duration `env:"LEADERBOARD_REPLAY_RATE_EVERY" envDefault:"2s"`
	LeaderboardReplayRateBurst int           `env:"LEADERBOARD_REPLAY_RATE_BURST" envDefault:"30"`
}

// LoadConfig reads Config from the process environment. It returns an error if a
// value is present but malformed (e.g. a non-parseable duration), so a
// misconfigured deployment fails fast at startup instead of misbehaving later.
func LoadConfig() (Config, error) {
	var c Config
	if err := env.ParseWithOptions(&c, env.Options{Prefix: envPrefix}); err != nil {
		return Config{}, fmt.Errorf("parse env config: %w", err)
	}
	return c, nil
}
