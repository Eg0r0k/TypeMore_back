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

	// TurnstileSecret is the Cloudflare Turnstile secret key guarding the three
	// auth endpoints that mail a stranger (register, verify/resend,
	// password-reset/request). EMPTY DISABLES CAPTCHA ENTIRELY: those endpoints
	// behave exactly as they did before it existed, and no token is required or
	// inspected. Empty is the DEFAULT because a captcha is a dependency on a
	// third party — `go run ./cmd/server` and the test suite must not need a
	// Cloudflare account, an outbound network path, or a site key pasted into
	// the frontend to register a user. Set it in staging and production; for
	// local experiments Cloudflare publishes always-pass / always-fail test
	// keys (docs/AUTH.md).
	TurnstileSecret string `env:"TURNSTILE_SECRET"`

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
	// how long that transaction holds its row locks: the claim, every verdict
	// and the commit share ONE transaction, so the locks are held for up to
	// batchSize × ReplayTimeout, inside the same transaction the leaderboard
	// projection runs in.
	//
	// Kept at 2, NOT restored to the pre-stop-gap 20. 20 was fine against a 5 s
	// budget (100 s of locks); against the 45 s below it would be 15 minutes,
	// which is worse than the stop-gap it would be replacing. 2 × 45 s = 90 s
	// is roughly what 20 × 5 s always cost, so this is the old lock ceiling
	// restored — the knob that moved is just not this one.
	ReplayBatchSize int32 `env:"REPLAY_BATCH_SIZE" envDefault:"2"`
	// ReplayConcurrency is the number of worker goroutines. Each owns a goja
	// runtime (~a few MB), and they share the queue via FOR UPDATE SKIP LOCKED.
	//
	// 4, not 1: one goroutine does 5.8 realistic runs/s, which is 0.42× a
	// plausible four-hour peak (docs/PERFORMANCE.md, zone 2). A maximum-size
	// submission now parks a goroutine for ~7 s rather than over a minute, but
	// 4 still buys the headroom cheaply and three keep draining regardless.
	ReplayConcurrency int `env:"REPLAY_CONCURRENCY" envDefault:"4"`
	// ReplayTimeout bounds one core call. A run that exceeds it is flagged
	// replay_timeout rather than allowed to occupy a worker.
	//
	// 45 s, down from the 300 s stop-gap. The core's quadratic fold is fixed
	// (O(1) amortized reduce), so the same maximum run that cost 76.5 s now
	// costs 3.4 s, and the ingestion caps have since risen to 120 000 events /
	// 6.5 MiB so that a full 10 000-word run is actually submittable. Measured
	// against THOSE caps, not the old ones:
	//
	//   german  10k words,  79 394 events — 7.18 s worst sample (measured)
	//   code_css 10k words, 108 274 events — ~8.1 s (projected at the measured
	//                                        17.5× V8→goja ratio; code_css is
	//                                        the worst of the nine published
	//                                        dictionaries, not german)
	//
	// 45 s is 5.6× that worst case, which is the ≥5× headroom this is sized
	// for. 30 s would have been 3.7× — it only cleared 5× under the OLD caps,
	// where the worst legal run was 4.3 s. The number that moved is the cap,
	// so the timeout moved with it.
	ReplayTimeout time.Duration `env:"REPLAY_TIMEOUT" envDefault:"45s"`
	// ReplayShutdownGrace bounds how long an in-flight batch may take to finish
	// after the shutdown signal, so verdicts commit instead of rolling back.
	//
	// It is also the deadline on EVERY batch context, and therefore an upper
	// bound on the per-run interrupt budget: the core call runs under
	// min(batch deadline, ReplayTimeout). Set it below batchSize × ReplayTimeout
	// and it silently clamps the budget above, and the expired context then
	// fails the decision write and rolls the batch back — a slow run retried
	// forever instead of judged once. So it covers the worst case the two knobs
	// above can produce: 2 × 45 s of interpreter plus the 30 s of database work
	// this default has always allowed.
	//
	// Cost: a deploy may wait two minutes for a batch to land, down from ten.
	ReplayShutdownGrace time.Duration `env:"REPLAY_SHUTDOWN_GRACE" envDefault:"120s"`

	// ReplayCanaryEpoch is the instant the client that RENDERS display canaries
	// went live. A run created at or after it is judged with the canary
	// detectors armed; every earlier run is judged exactly as it always was.
	//
	// UNSET (the zero value, and the default) means NO run is ever armed. That
	// is the whole point of the knob, not a conservative default to be tidied
	// away later: the canary schedule is computable from any historical seed,
	// so an ungated judge would score coincidental early commits on runs whose
	// client never drew a canary — and `make revalidate` walks all of history,
	// so it would do it to the entire archive at once.
	//
	// Set it BY HAND, once, to the frontend deploy instant, and only after that
	// deploy is out (docs/REPLAY.md, "Canary epoch"). RFC3339, e.g.
	// TYPEMORE_REPLAY_CANARY_EPOCH=2026-08-01T12:00:00Z.
	ReplayCanaryEpoch time.Time `env:"REPLAY_CANARY_EPOCH"`

	// --- Replay review policy (docs/REPLAY.md, "Review policy") ---
	//
	// Which plausibility flags are worth what, and how much weighted severity
	// sends a run to a human. Defaults live in internal/replay/policy behind
	// the `anticheat` build tag; these override them, and a typo is a startup
	// failure rather than a silently disarmed check.
	//
	// On a binary built WITHOUT that tag there is no policy to tune, and the
	// server warns instead of ignoring the value. The gate is build-time and not
	// runtime on purpose: a runtime switch would leave the weights and the rule
	// names in the binary whichever way it was set (docs/SELF_HOST.md).

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

	// --- Lobby discovery (docs/PROTOCOL.md §5) ---

	// LobbyRateEvery / LobbyRateBurst are the per-IP token bucket on the public
	// GET /api/v1/rooms room list. The lobby screen POLLS this endpoint every
	// 4 s, i.e. 0.25 req/s, while the bucket refills at 0.5 tokens/s — twice
	// the poll rate, so a client parked on the lobby all evening can never
	// deplete it and the limit is invisible to correct clients. The burst of 30
	// then covers what a steady rate does not: a reload storm, a handful of
	// tabs, or several players behind one NAT. The answer is a tiny in-memory
	// projection, so this is abuse control, not load shedding.
	LobbyRateEvery time.Duration `env:"LOBBY_RATE_EVERY" envDefault:"2s"`
	LobbyRateBurst int           `env:"LOBBY_RATE_BURST" envDefault:"30"`
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
