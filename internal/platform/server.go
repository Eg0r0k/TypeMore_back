package platform

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"
)

// RunHTTPServer starts srv and blocks until either the server fails or ctx is
// cancelled, then performs a graceful shutdown.
//
// Lifecycle / goroutine ownership:
//   - ListenAndServe runs on a child goroutine because it blocks until the
//     server stops; its result is delivered back over errCh.
//   - This function owns the shutdown decision. When ctx is cancelled (typically
//     by a SIGINT/SIGTERM handler in main), it calls srv.Shutdown with a fresh,
//     time-bounded context so a hung connection cannot block exit forever.
//
// For graceful shutdown of long-lived connections (WebSocket) to work, the
// caller MUST wire srv.BaseContext to the same ctx passed here: cancelling ctx
// then propagates into every in-flight request context, letting connection
// handlers notice and tear themselves down while Shutdown waits for them.
func RunHTTPServer(ctx context.Context, srv *http.Server, log *slog.Logger, shutdownTimeout time.Duration) error {
	// Buffered so the goroutine never leaks if we return via the ctx path.
	errCh := make(chan error, 1)

	go func() {
		log.Info("http server listening", "addr", srv.Addr)
		// ErrServerClosed is the expected result of a graceful Shutdown and is
		// not an error condition.
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		// The server failed to run at all (e.g. the port is in use).
		return err
	case <-ctx.Done():
		log.Info("shutdown signal received, draining connections", "timeout", shutdownTimeout)
		// A detached context: ctx is already cancelled, so we need a fresh one
		// to give in-flight work a bounded window to finish.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return err
		}
		log.Info("http server stopped cleanly")
		return nil
	}
}
