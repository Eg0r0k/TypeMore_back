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

	"github.com/typemore/typemore-server/internal/platform"
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

	router := chi.NewRouter()
	// RequestID tags each request; Recoverer turns a handler panic into a 500
	// instead of crashing the process.
	router.Use(middleware.RequestID)
	router.Use(middleware.Recoverer)

	router.Get("/healthz", platform.HealthHandler())
	router.Handle("/ws", ws.NewHandler(logger, cfg.AllowedOrigins))

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
