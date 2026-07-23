// Command server is the TypeMore game server: a single static binary.
//
// This is the composition root — the one place that wires concrete
// dependencies together ("manual DI"). It stays deliberately thin: it loads
// configuration, builds the logger and router, mounts the HTTP/WebSocket
// endpoints, and delegates the run/shutdown lifecycle to the platform package.
package main

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"

	"github.com/google/uuid"

	"github.com/typemore/typemore-server/internal/auth"
	"github.com/typemore/typemore-server/internal/auth/pgstore"
	"github.com/typemore/typemore-server/internal/platform"
	"github.com/typemore/typemore-server/internal/platform/db"
	"github.com/typemore/typemore-server/internal/platform/mail"
	"github.com/typemore/typemore-server/internal/runs"
	runspg "github.com/typemore/typemore-server/internal/runs/pgstore"
	"github.com/typemore/typemore-server/internal/ws"
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
	defer pool.Close()

	// Build the auth domain. One Postgres store backs both the general store and
	// (this phase) the session store; email goes through SMTP or, when unset,
	// the dev log sender. See internal/auth for the session-storage deviation.
	authStore := pgstore.New(pool)
	authSvc := auth.NewService(authStore, authStore, newMailer(cfg, logger),
		auth.NewInMemoryRateLimiter(cfg.AuthRateEvery, cfg.AuthRateBurst),
		authConfig(cfg), logger)

	// Expiry janitor: periodically deletes expired sessions and stale email
	// tokens. Tied to ctx, so the shutdown signal stops it with the server.
	if cfg.AuthCleanupInterval > 0 {
		go auth.RunJanitor(ctx, authStore, cfg.AuthCleanupInterval, logger)
	}

	// Build the runs domain: run ingestion + own-runs listing. It reuses the
	// auth rate-limiter machinery (keyed per user) and reads the authenticated
	// principal from the request context via an adapter over auth.UserFrom, so
	// the domain imports no sibling package.
	runsStore := runspg.New(pool)
	runsSvc := runs.NewService(runsStore,
		auth.NewInMemoryRateLimiter(cfg.RunsRateEvery, cfg.RunsRateBurst),
		func(ctx context.Context) (uuid.UUID, bool) {
			u, ok := auth.UserFrom(ctx)
			return u.ID, ok
		}, logger)

	router := chi.NewRouter()
	// RequestID tags each request; Recoverer turns a handler panic into a 500
	// instead of crashing the process.
	router.Use(middleware.RequestID)
	router.Use(middleware.Recoverer)
	// CORS for the browser SPA: allow exactly the configured frontend origin and
	// let it send the session cookie (credentials).
	router.Use(cors.Handler(cors.Options{
		AllowedOrigins:   []string{cfg.FrontendOrigin},
		AllowedMethods:   []string{http.MethodGet, http.MethodPost, http.MethodOptions},
		AllowedHeaders:   []string{"Content-Type"},
		AllowCredentials: true,
		MaxAge:           300,
	}))

	router.Get("/healthz", platform.HealthHandler())
	router.Get("/readyz", platform.ReadyHandler(pool))
	router.Handle("/ws", ws.NewHandler(logger, cfg.AllowedOrigins))

	router.Route("/api/v1", func(r chi.Router) {
		r.Mount("/auth", authSvc.AuthRoutes())
		r.With(authSvc.RequireAuth).Get("/me", authSvc.HandleMe)
		// Runs: session required (guests play client-only). RequireOrigin is a
		// no-op on safe methods, so this group covers GET listing/detail and the
		// Origin-checked POST ingestion alike.
		r.Group(func(r chi.Router) {
			r.Use(authSvc.RequireOrigin)
			r.Use(authSvc.RequireAuth)
			r.Mount("/runs", runsSvc.Routes())
		})
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
	}
}
