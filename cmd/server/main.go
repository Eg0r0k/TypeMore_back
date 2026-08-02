// Command server is the TypeMore game server: a single static binary.
//
// This is the composition root — the one place that wires concrete
// dependencies together ("manual DI"). It stays deliberately thin: it loads
// configuration, builds the logger and router, mounts the HTTP/WebSocket
// endpoints, and delegates the run/shutdown lifecycle to the platform package.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"syscall"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"

	"github.com/google/uuid"

	"github.com/typemore/typemore-server/api"
	"github.com/typemore/typemore-server/internal/auth"
	"github.com/typemore/typemore-server/internal/auth/pgstore"
	"github.com/typemore/typemore-server/internal/keyboard"
	keyboardpg "github.com/typemore/typemore-server/internal/keyboard/pgstore"
	"github.com/typemore/typemore-server/internal/leaderboard"
	leaderboardpg "github.com/typemore/typemore-server/internal/leaderboard/pgstore"
	"github.com/typemore/typemore-server/internal/moderation"
	"github.com/typemore/typemore-server/internal/platform"
	"github.com/typemore/typemore-server/internal/platform/db"
	"github.com/typemore/typemore-server/internal/platform/mail"
	"github.com/typemore/typemore-server/internal/platform/turnstile"
	"github.com/typemore/typemore-server/internal/profile"
	profilepg "github.com/typemore/typemore-server/internal/profile/pgstore"
	"github.com/typemore/typemore-server/internal/quote"
	quotepg "github.com/typemore/typemore-server/internal/quote/pgstore"
	"github.com/typemore/typemore-server/internal/replay"
	replaypg "github.com/typemore/typemore-server/internal/replay/pgstore"
	"github.com/typemore/typemore-server/internal/replay/policy"
	"github.com/typemore/typemore-server/internal/runs"
	runspg "github.com/typemore/typemore-server/internal/runs/pgstore"
	"github.com/typemore/typemore-server/internal/ws"
	"github.com/typemore/typemore-server/internal/ws/wspg"
)

func main() {
	if err := run(); err != nil {
		// The logger may not exist yet if config failed, so use the default.
		slog.Error("server exited with error", "err", err)
		os.Exit(1)
	}
}

// run holds the real program so it can return errors (main only translates them
// to an exit code). It blocks until a shutdown signal is received or the server
// fails.
func run() error {
	// signal.NotifyContext gives us a context that is cancelled on Ctrl+C
	// (SIGINT) or SIGTERM. Everything downstream watches this context, so one
	// signal drains the whole server.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := platform.LoadConfig()
	if err != nil {
		return err
	}

	logger := platform.NewLogger(cfg)
	// Make the configured logger the default so any library that reaches for
	// slog.Default() (and our own convenience calls) is consistent.
	slog.SetDefault(logger)

	// Connect to Postgres up front; a bad DB is a startup failure, not a
	// first-request surprise. The pool is closed on the way out.
	pool, err := db.NewPool(ctx, cfg.DatabaseURL, cfg.DBMaxConns)
	if err != nil {
		return err
	}
	// Registered here and therefore LAST to run: every defer below is registered
	// after it and unwinds first, so the replay worker's drain and the room
	// registry's final persists all still have a database underneath them.
	defer pool.Close()
	// Build the auth domain. One Postgres store backs both the general store and
	// (this phase) the session store; email goes through SMTP or, when unset,
	// the dev log sender. See internal/auth for the session-storage deviation.
	//
	// The password-hashing gate is resolved and logged here: it is a memory
	// ceiling expressed as a concurrency, and an operator who cannot see the
	// number cannot tell a shed request from a bug (docs/PERFORMANCE.md, zone 1).
	authCfg := authConfig(cfg)
	ceiling, ceilingSource := platform.MemoryCeiling()
	logger.Info("password hashing gate",
		"concurrency", authCfg.HashConcurrency,
		"peakHashMemory", authCfg.HashConcurrency*auth.HashCostBytes,
		"memoryCeiling", ceiling, "ceilingSource", ceilingSource)

	authStore := pgstore.New(pool)
	captcha := newCaptchaVerifier(cfg)
	logger.Info("captcha", "enabled", captcha != nil)
	// Moderation. One store, read by two places that must agree about what
	// "banned right now" means: the /me flag that renders the banner, and the
	// gate in front of run submission. Both go through the same active_bans
	// view every board and replay read already filters on, so an unban or an
	// expiry lifts everywhere at once (docs/MODERATION.md).
	moderationStore := moderation.New(pool)

	authSvc := auth.NewService(authStore, authStore, newMailer(cfg, logger),
		auth.NewInMemoryRateLimiter(cfg.AuthRateEvery, cfg.AuthRateBurst),
		captcha, authCfg, logger).WithRestrictions(moderationStore)

	// Expiry janitor: periodically deletes expired sessions and stale email
	// tokens. Tied to ctx, so the shutdown signal stops it with the server.
	if cfg.AuthCleanupInterval > 0 {
		go auth.RunJanitor(ctx, authStore, cfg.AuthCleanupInterval, logger)
	}

	// Admin bootstrap (docs/MODERATION.md, "The admin surface"): the accounts
	// behind TYPEMORE_ADMINS' VERIFIED emails are promoted to the admin role.
	// Promotion only — the env list is how the first admin appears, never a
	// sync source that demotes. An empty list is also the mount switch below:
	// no admins configured, no /admin subtree, no surface to attack.
	adminSurface := len(cfg.Admins) > 0
	if adminSurface {
		promoted, err := authSvc.BootstrapAdmins(ctx, cfg.Admins)
		if err != nil {
			logger.Error("admin bootstrap", "err", err)
			return err
		}
		logger.Info("admin bootstrap", "configured", len(cfg.Admins), "promoted", promoted)
	}

	// Build the runs domain: run ingestion + own-runs listing + the public
	// replay endpoint. It reuses the auth rate-limiter machinery (keyed per user
	// for ingestion, per IP for public replay) and reads the authenticated
	// principal from the request context via an adapter over auth.UserFrom, so
	// the domain imports no sibling package.
	runsStore := runspg.New(pool)
	runsSvc := runs.NewService(runsStore,
		auth.NewInMemoryRateLimiter(cfg.RunsRateEvery, cfg.RunsRateBurst),
		auth.NewInMemoryRateLimiter(cfg.LeaderboardReplayRateEvery, cfg.LeaderboardReplayRateBurst),
		func(ctx context.Context) (uuid.UUID, bool) {
			u, ok := auth.UserFrom(ctx)
			return u.ID, ok
		}, logger).WithRestrictions(moderationStore)

	// Quotes: the fixed-text corpus (SCORING_CONCEPT §6, docs/QUOTES.md). Read
	// only on the public path, and unauthenticated like the boards and the
	// dictionaries — a guest picking a text to type has no account yet. The
	// corpus itself is published out of band by `make import-quotes`; the one
	// write reachable over HTTP is a moderator's withdrawal, which cannot touch
	// a quote's bytes (docs/REPORTS.md). Built HERE, above the boards, because
	// the board index needs it — see the next paragraph.
	quoteStore := quotepg.New(pool)

	// Leaderboards: a public read model projected from accepted runs
	// (docs/LEADERBOARDS.md). The same store is both the read side the HTTP
	// service uses and the projector the replay worker calls inside its own
	// transaction — the composition root is the only place that knows both.
	//
	// The withdrawn-quotes reader is the composition root doing what it exists
	// for: the board index must not list a withdrawn quote's board, and that
	// fact lives in the quote corpus. Passing a function keeps the two domains
	// from importing each other, and keeps the bucket-key format to its single
	// producer — the alternative was rebuilding `quote:<id>` inside the
	// leaderboard's SQL, where nothing would keep it in step.
	boardStore := leaderboardpg.New(pool, cfg.LeaderboardRequireVerifiedEmail)
	boardSvc := leaderboard.NewService(boardStore,
		func(ctx context.Context) (uuid.UUID, bool) {
			u, ok := auth.UserFrom(ctx)
			return u.ID, ok
		},
		quoteStore.WithdrawnIDs, logger)

	// Keyboard layouts: the shared data asset (internal/keyboard/layouts) —
	// the char → physical-key mapping the keyboard projection folds through,
	// the heatmap's geometry, and (later) the anticheat bigram heuristics'
	// finger map. A malformed asset is a startup error by design.
	layouts, err := keyboard.Load()
	if err != nil {
		logger.Error("load keyboard layouts", "err", err)
		os.Exit(1)
	}

	// Profile: the caller's own statistics (docs/PROFILE.md), on-demand SQL
	// over runs plus a read of the PB slots the leaderboard projection already
	// maintains. One service serves two surfaces mounted separately below — the
	// session-scoped /profile subtree and the public /users one — because they
	// read the same model through different gates, not different models. The
	// bucket parser is the leaderboard domain's own (the key format has one
	// producer and one parser), adapted here so the profile package does not
	// import its sibling.
	//
	// The search limiter is a SEPARATE bucket from auth's, deliberately: those
	// tokens ration argon2id and outbound email, and a search flood must not be
	// able to spend them.
	profileStore := profilepg.New(pool)
	profileSvc := profile.NewService(profileStore,
		auth.NewInMemoryRateLimiter(cfg.ProfileSearchRateEvery, cfg.ProfileSearchRateBurst),
		func(ctx context.Context) (uuid.UUID, bool) {
			u, ok := auth.UserFrom(ctx)
			return u.ID, ok
		},
		func(key string) (profile.BucketInfo, bool) {
			b, err := leaderboard.ParseBucketKey(key)
			if err != nil {
				return profile.BucketInfo{}, false
			}
			info := profile.BucketInfo{Mode: b.Mode, Lang: b.Lang, TextSource: b.TextSource}
			if b.QuoteID != uuid.Nil {
				id := b.QuoteID
				info.QuoteID = &id
				return info, true
			}
			dim := b.Dimension
			switch b.Mode {
			case leaderboard.ModeTime:
				info.DurationMs = &dim
			case leaderboard.ModeWords:
				info.WordCount = &dim
			}
			return info, true
		}, layouts.LayoutFor, logger)

	quoteSvc := quote.NewService(quoteStore, logger)

	// Reports (docs/REPORTS.md): the SIGNAL half of moderation, next to bans,
	// which are the action half. One service serves two surfaces — a player
	// files, a moderator triages — and they are mounted separately below,
	// behind different gates.
	//
	// The player-facing route is mounted whether or not any admin is
	// configured. A deployment with nobody to work the queue still wants the
	// reports recorded: promoting an admin later must not mean the complaints
	// of the first month were dropped on the floor.
	reportSvc := moderation.NewReportService(moderationStore,
		func(req *http.Request) (uuid.UUID, bool) {
			u, ok := auth.UserFrom(req.Context())
			return u.ID, ok
		},
		func(req *http.Request) (moderation.Actor, bool) {
			u, ok := auth.UserFrom(req.Context())
			return moderation.Actor{ID: u.ID, Name: u.DisplayName}, ok
		},
		auth.NewInMemoryRateLimiter(cfg.ReportRateEvery, cfg.ReportRateBurst), logger)

	// Dictionaries: the server is the single source of the word lists the client
	// generates text from. The registry is seeded once here — every fingerprint
	// computed by the vendored core bundle running in goja, so the server and
	// the client can never disagree about a dict_hash. A broken bundle or an
	// unhashable dictionary is a startup failure, not a 500 later.
	core, err := replay.NewCore(cfg.ReplayTimeout)
	if err != nil {
		return err
	}
	// The bundle's identity, next to its SHA: which @typemore/core version,
	// which log formats, and which frontend commit this binary judges runs
	// with. `make core-bundle` refuses dirty/stale provenance at vendoring
	// time; this line is how a running instance answers the same question.
	coreInfo := core.BuildInfo()
	logger.Info("core bundle loaded",
		"bundleSHA", replay.BundleSHA(),
		"coreVersion", coreInfo.PackageVersion,
		"eventLogVersion", coreInfo.EventLogVersion,
		"telemetryLogVersion", coreInfo.TelemetryLogVersion,
		"frontendCommit", coreInfo.GitSHA,
		"builtFromDirtyTree", coreInfo.GitDirty)
	dictReg, err := replay.NewRegistry(core)
	if err != nil {
		return err
	}
	dictSvc, err := replay.NewDictionaryService(dictReg)
	if err != nil {
		return err
	}
	logger.Info("dictionary registry seeded", "count", len(dictReg.Catalogue()))

	// Replay worker: the queue that turns 'pending' runs into
	// accepted/flagged/rejected (docs/REPLAY.md). It is started here and drained
	// below, after the HTTP server has stopped accepting requests — the deferred
	// Wait runs before the deferred pool.Close, so in-flight batches still have
	// a database.
	//
	// The review policy is resolved up front: a typo in a weight override must
	// stop the process, not leave a check quietly disarmed.
	//
	// Which policy exists at all is a BUILD-time fact. Without -tags anticheat
	// this returns policy.Noop and the binary contains no weight, no threshold
	// and no rule name to find — see internal/replay/policy. A runtime switch
	// would have left all three in the binary regardless of its position.
	judge, err := policy.Provide(policy.Config{
		FlagWeights:       cfg.ReplayFlagWeights,
		ReviewThreshold:   cfg.ReplayReviewThreshold,
		SustainedBurstSec: cfg.ReplaySustainedBurstSec,
	})
	if err != nil && !errors.Is(err, policy.ErrNoPolicy) {
		return err
	}
	decider, err := replay.NewDecider(judge)
	if err != nil {
		return err
	}
	// Loud, once, at the top: an instance that judges runs for correctness only
	// is a legitimate deployment and a SILENT one is the worst outcome of making
	// the policy removable. The same fact is served by /healthz, because the
	// person who needs it most is the one who did not read the boot log.
	if policy.IsNoop(judge) {
		logger.Warn("review policy disabled: runs are judged for correctness only",
			"policyVersion", judge.Version(),
			"rebuildWith", "-tags "+policy.BuildTag,
			"consequence", "no suspicion is computed, no run reaches the review queue, "+
				"and this instance's leaderboards are not protected — see docs/SELF_HOST.md")
		if errors.Is(err, policy.ErrNoPolicy) {
			logger.Warn("replay policy tuning is set but this build has no policy to tune",
				"err", err)
		}
	} else {
		logger.Info("review policy enabled", "policyVersion", judge.Version())
	}
	var workers sync.WaitGroup
	defer workers.Wait()
	if cfg.ReplayEnabled {
		// Two text sources, two registries: dictionary bodies by hash for seeded
		// runs, quote bytes by id for quote runs. The worker declares both as
		// narrow interfaces; this is the only place that knows they are the
		// dictionary registry and Postgres.
		worker := replay.NewWorker(
			replaypg.New(pool, boardStore).WithKeyboard(keyboardpg.New(layouts)),
			dictReg,
			quote.ReplayResolver{Store: quoteStore},
			replay.WorkerConfig{
				PollInterval:  cfg.ReplayPollInterval,
				BatchSize:     cfg.ReplayBatchSize,
				Concurrency:   cfg.ReplayConcurrency,
				ReplayTimeout: cfg.ReplayTimeout,
				ShutdownGrace: cfg.ReplayShutdownGrace,
				Decider:       decider,
				// Unset until a human sets it — see the config field. An
				// instance with no epoch judges every run exactly as it did
				// before the canary detectors existed, which is the state this
				// deployment is in until the rendering client is out.
				CanaryEpoch: cfg.ReplayCanaryEpoch,
			},
			logger,
		)
		workers.Add(1)
		go func() {
			defer workers.Done()
			if err := worker.Run(ctx); err != nil {
				logger.Error("replay worker exited with error", "err", err)
			}
		}()
	}

	router := chi.NewRouter()
	// RequestID tags each request; Recoverer turns a handler panic into a 500
	// instead of crashing the process.
	router.Use(middleware.RequestID)
	router.Use(middleware.Recoverer)
	// CORS for the browser SPA: allow exactly the configured frontend origin and
	// let it send the session cookie (credentials).
	router.Use(cors.Handler(cors.Options{
		AllowedOrigins: []string{cfg.FrontendOrigin},
		// PATCH is here for /me/settings; DELETE for the admin surface's
		// unban (docs/MODERATION.md) — without it the browser preflight
		// refuses the request before the server ever sees it.
		AllowedMethods:   []string{http.MethodGet, http.MethodPost, http.MethodPatch, http.MethodDelete, http.MethodOptions},
		AllowedHeaders:   []string{"Content-Type"},
		AllowCredentials: true,
		MaxAge:           300,
	}))

	router.Get("/healthz", platform.HealthHandler(platform.ReviewPolicy{
		Enabled: !policy.IsNoop(judge),
		Version: judge.Version(),
	}))
	router.Get("/readyz", platform.ReadyHandler(pool))
	// Dictionary bodies are public, immutable, content-addressed static assets.
	// They sit outside /api/v1 (and outside auth) on purpose: this path is a CDN
	// origin, not an API.
	router.Mount("/static/dictionaries", dictSvc.StaticRoutes())
	// The WS upgrade carries the session cookie; resolve it to the account
	// identity so authed connections use their name (guests get a server-assigned
	// per-room nick) and match runs are attributed to the account. A nil resolver
	// would make everyone a guest. Finished matches are persisted via the pool.
	wsHandler := ws.NewHandler(logger, cfg.AllowedOrigins, func(r *http.Request) (string, string, bool) {
		if u, ok := authSvc.Identify(r); ok {
			return u.DisplayName, u.ID.String(), true
		}
		return "", "", false
	}, wspg.New(pool))
	router.Handle("/ws", wsHandler)
	// A match that ends while the process is going down is exactly the capture
	// worth keeping, so its write runs on a background context of its own and is
	// waited for here instead. Registered after pool.Close, so it unwinds first
	// and the write still has a database.
	defer func() {
		wait, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.ShutdownTimeout)
		defer cancel()
		if !wsHandler.WaitForPersists(wait) {
			logger.Warn("match captures still writing at shutdown deadline; some may be lost",
				"timeout", cfg.ShutdownTimeout)
		}
	}()

	router.Route("/api/v1", func(r chi.Router) {
		// The machine-readable contract of everything mounted below — public
		// and cacheable like the other assets, so the frontend can point
		// Swagger UI / codegen at a live instance (api/api.go has the why).
		r.Get("/openapi.yaml", api.Handler())
		r.Mount("/auth", authSvc.AuthRoutes())
		r.With(authSvc.RequireAuth).Get("/me", authSvc.HandleMe)
		// The account's privacy switches. RequireOrigin because it mutates —
		// /me itself is a safe read and deliberately carries no CSRF check.
		r.With(authSvc.RequireOrigin, authSvc.RequireAuth).
			Patch("/me/settings", authSvc.HandleUpdateSettings)
		// The dictionary catalogue is a public asset too — no session: guests
		// play client-side and still need to pick a language.
		r.Mount("/dictionaries", dictSvc.Routes())
		// Runs: the ingestion and own-runs routes require a session (guests play
		// client-only); GET /runs/{id}/replay and its /log sibling are public
		// and the runs router draws that line itself, so the middleware is
		// passed in rather than wrapped around the whole mount.
		r.Mount("/runs", runsSvc.Routes(authSvc.RequireOrigin, authSvc.RequireAuth))
		// Leaderboards are public: a board nobody can read without an account is
		// a board nobody links to. OptionalAuth resolves the session WITHOUT
		// requiring one, which is what lets /{bucket}/me answer "your rank" on
		// an otherwise anonymous surface.
		r.With(authSvc.OptionalAuth).Mount("/leaderboards", boardSvc.Routes())
		// Profile: session-scoped, the caller's own statistics only — the
		// subtree carries its own RequireAuth so no route can be added public
		// by accident (docs/PROFILE.md, "Privacy").
		r.Mount("/profile", profileSvc.Routes(authSvc.RequireAuth))
		// Public profiles: /users/{name}/… — no session required, but
		// OptionalAuth resolves one when present, which is what lets an owner
		// through their own closed profile. Privacy is enforced inside these
		// handlers (403 profile_closed), server-side, before any query runs.
		r.With(authSvc.OptionalAuth).Mount("/users", profileSvc.PublicRoutes())
		// The layouts asset, public and cacheable like the dictionaries: the
		// heatmap needs geometry before it needs data, and a guest reading a
		// public page one day will too.
		r.Mount("/layouts", layouts.Routes(logger))
		// Quotes need no session at all — not even an optional one. Nothing on
		// this subtree varies by who is asking, so it is mounted bare rather
		// than behind OptionalAuth like the boards.
		r.Mount("/quotes", quoteSvc.Routes())
		// Filing a report needs a session and the Origin check — both applied
		// inside the domain's own Routes, since every route on this subtree
		// needs them and none is public.
		r.Mount("/reports", reportSvc.Routes(authSvc.RequireOrigin, authSvc.RequireAuth))
		// The admin surface (docs/MODERATION.md): per-route PERMISSION gates
		// whose refusal is a 404, so the subtree is invisible to anyone it
		// does not belong to. OptionalAuth rather than RequireAuth on purpose
		// — RequireAuth's 401 would already confirm to an anonymous prober
		// that something lives here, while OptionalAuth + a permission miss
		// answers exactly like a route that does not exist. The permission
		// check runs BEFORE the Origin (CSRF) check for the same reason:
		// nobody without the permission learns anything, whatever they send.
		// Mounted at all only when admins are configured — a deployment with
		// no admins has no admin routes, not admin routes nobody can pass.
		if adminSurface {
			moderationSvc := moderation.NewService(moderationStore,
				func(req *http.Request) (moderation.Actor, bool) {
					u, ok := auth.UserFrom(req.Context())
					return moderation.Actor{ID: u.ID, Name: u.DisplayName}, ok
				}, logger)
			// writeGate pairs a permission with the Origin check, in that
			// order: the permission first, so a caller without it learns
			// nothing whatever they send.
			writeGate := func(p auth.Permission) func(http.Handler) http.Handler {
				return func(next http.Handler) http.Handler {
					return authSvc.RequirePermission(p)(authSvc.RequireOrigin(next))
				}
			}
			quoteAdminSvc := quote.NewAdminService(quoteStore,
				func(req *http.Request) (uuid.UUID, bool) {
					u, ok := auth.UserFrom(req.Context())
					return u.ID, ok
				}, logger)

			// The three admin subtrees share one prefix. The ban surface is
			// mounted LAST, on "/", because it owns paths at the root of
			// /admin (/bans, /users/{id}/bans) while the other two live under
			// their own segments: chi resolves a static segment ahead of the
			// catch-all a root mount installs, so /admin/reports reaches the
			// report surface and /admin/bans falls through to the ban one.
			r.With(authSvc.OptionalAuth).Route("/admin", func(ar chi.Router) {
				ar.Mount("/reports", reportSvc.AdminRoutes(
					authSvc.RequirePermission(auth.PermReportsRead),
					writeGate(auth.PermReportsWrite)))
				ar.Mount("/quotes", quoteAdminSvc.Routes(
					authSvc.RequirePermission(auth.PermReportsRead),
					writeGate(auth.PermQuotesWrite)))
				ar.Mount("/", moderationSvc.AdminRoutes(
					authSvc.RequirePermission(auth.PermBansRead),
					writeGate(auth.PermBansWrite)))
			})
		}
		// Room discovery, served by the WS handler's registry but mounted HERE,
		// as a public HTTP read — deliberately outside the protocol
		// (docs/PROTOCOL.md §5): browsing the lobby is a stateless question,
		// not a session. It reuses the shared token-bucket machinery, keyed per
		// IP, because the lobby screen polls it.
		r.Method(http.MethodGet, "/rooms", wsHandler.LobbyHandler(
			auth.NewInMemoryRateLimiter(cfg.LobbyRateEvery, cfg.LobbyRateBurst)))
	})

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           router,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		// BaseContext ties every request context to our shutdown context, so
		// cancelling ctx (on a signal) propagates into live WebSocket handlers
		// and lets them tear down while Shutdown waits. See RunHTTPServer.
		BaseContext: func(net.Listener) context.Context { return ctx },
	}

	logger.Info("starting typemore server",
		"addr", cfg.Addr,
		"version", platform.Version,
		"commit", platform.Commit,
	)

	return platform.RunHTTPServer(ctx, srv, logger, cfg.ShutdownTimeout)
}

// mailerAdapter adapts a platform mail.Sender to the auth.Mailer interface. It
// lives here (the composition root) so platform stays free of any auth import.
type mailerAdapter struct{ sender mail.Sender }

func (m mailerAdapter) Send(ctx context.Context, msg auth.Mail) error {
	return m.sender.Send(ctx, mail.Message{To: msg.To, Subject: msg.Subject, Body: msg.Body})
}

// newMailer picks the SMTP sender when a host is configured, otherwise the dev
// log sender (which prints the verification/reset link to the logs).
func newMailer(cfg platform.Config, log *slog.Logger) auth.Mailer {
	var sender mail.Sender
	if cfg.SMTPHost != "" {
		sender = mail.NewSMTPSender(cfg.SMTPHost, cfg.SMTPPort, cfg.SMTPUsername, cfg.SMTPPassword, cfg.SMTPFrom)
	} else {
		sender = mail.NewLogSender(log)
	}
	return mailerAdapter{sender: sender}
}

// newCaptchaVerifier builds the Turnstile verifier, or returns a nil interface
// when no secret is configured (captcha disabled — the dev default).
//
// The explicit nil return is not ceremony: turnstile.New yields a typed
// (*turnstile.Verifier)(nil), and returning that directly would produce a
// NON-nil auth.CaptchaVerifier wrapping a nil pointer, so the domain's
// "nil means disabled" check would pass and every gated request would
// dereference it. This is the one place that knows both types, so it is the
// one place that has to be careful.
func newCaptchaVerifier(cfg platform.Config) auth.CaptchaVerifier {
	v := turnstile.New(cfg.TurnstileSecret)
	if v == nil {
		return nil
	}
	return v
}

// authConfig translates platform.Config into the auth domain's own config,
// enabling only the OAuth providers that have credentials set.
func authConfig(cfg platform.Config) auth.Config {
	providers := map[string]auth.ProviderCredentials{}
	if cfg.GitHubClientID != "" {
		providers[auth.ProviderGitHub] = auth.ProviderCredentials{
			ClientID:     cfg.GitHubClientID,
			ClientSecret: cfg.GitHubClientSecret,
		}
	}
	if cfg.GoogleClientID != "" {
		providers[auth.ProviderGoogle] = auth.ProviderCredentials{
			ClientID:     cfg.GoogleClientID,
			ClientSecret: cfg.GoogleClientSecret,
		}
	}
	return auth.Config{
		FrontendOrigin:    cfg.FrontendOrigin,
		CookieName:        cfg.CookieName,
		CookieDomain:      cfg.CookieDomain,
		CookieSecure:      cfg.CookieSecure,
		SessionTTL:        cfg.SessionTTL,
		OAuthRedirectBase: cfg.OAuthRedirectBase,
		Providers:         providers,
		HashConcurrency:   hashConcurrency(cfg),
		HashWait:          cfg.AuthHashWait,
	}
}

// Gate-sizing constants, all three derived from measurement, not intuition
// (docs/PERFORMANCE.md, zone 1).
const (
	// hashMemoryFraction is the share of the process memory ceiling password
	// hashing may occupy at peak. A quarter: the rest belongs to request
	// handling, the goja replay worker (a few MB per runtime) and the
	// connection pool, and a gate that could consume everything is not a gate.
	hashMemoryFraction = 4

	// hashHeapMultiplier converts the gate's NOMINAL cost (slots × 19 MiB) into
	// the process heap actually observed at that concurrency.
	//
	// Sizing by the nominal figure is what the first cut of this function did,
	// and it was optimistic by more than a factor of two. Measured on a
	// 12-thread box: a gate of 8 (152 MiB nominal) peaked at 370–425 MiB of
	// heap across repeated 200-request bursts — 2.4–2.8×. The excess is
	// collectable garbage, not live data: argon2's 19 MiB blocks are coarse
	// enough that GOGC=100's "let the heap double before collecting" hysteresis
	// applies to them almost in full. Re-running the same burst at GOGC=20 gave
	// 236 MiB (1.55×) at 99% of the throughput, which confirms the diagnosis.
	//
	// 3 rather than 2.8 leaves a margin for the drift the same measurement
	// showed (±15% run to run).
	hashHeapMultiplier = 3

	// hashCPUFactor caps the gate by CPU as well as memory. argon2id runs with
	// p=1, so each hash saturates one thread for ~25 ms; past roughly GOMAXPROCS
	// the extra slots do not hash faster, they just hold memory while queueing
	// for a core. Measured throughput by concurrency on 12 threads: 4→90/s,
	// 8→114/s, 10→127/s, 33→105/s, 44→97/s. It peaks near the core count and
	// DECLINES past it, so admitting more is strictly worse on both axes.
	hashCPUFactor = 2

	// hashFallbackBudget is used when no memory ceiling can be detected
	// (Windows, macOS). Conservative on purpose: under-admitting costs latency
	// under load, over-admitting costs the process.
	hashFallbackBudget = 512 << 20
)

// hashConcurrency resolves the argon2id gate size (docs/PERFORMANCE.md, zone 1).
//
// An explicit count wins. Otherwise a memory budget — explicit, or a quarter of
// the detected ceiling — is divided by the MEASURED per-slot heap cost, and the
// result is capped by what the CPU can actually use.
func hashConcurrency(cfg platform.Config) int {
	if cfg.AuthHashConcurrency > 0 {
		return cfg.AuthHashConcurrency
	}
	budget := cfg.AuthHashMemoryBudget
	if budget == 0 {
		if ceiling, _ := platform.MemoryCeiling(); ceiling > 0 {
			budget = ceiling / hashMemoryFraction
		} else {
			budget = hashFallbackBudget
		}
	}
	n := auth.HashConcurrencyFor(budget / hashHeapMultiplier)
	if cpuCap := runtime.GOMAXPROCS(0) * hashCPUFactor; n > cpuCap {
		return cpuCap
	}
	return n
}
