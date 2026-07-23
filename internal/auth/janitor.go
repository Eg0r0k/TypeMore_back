package auth

import (
	"context"
	"log/slog"
	"time"
)

// Cleaner is the janitor's persistence contract: bulk-delete rows that can no
// longer authenticate anything. Implemented by the Postgres store adapter.
type Cleaner interface {
	// DeleteExpiredSessions removes sessions with expires_at in the past and
	// returns the number of rows deleted.
	DeleteExpiredSessions(ctx context.Context) (int64, error)
	// DeleteStaleEmailTokens removes email tokens that expired or were
	// consumed more than 24 hours ago and returns the number of rows deleted.
	DeleteStaleEmailTokens(ctx context.Context) (int64, error)
}

// RunJanitor sweeps expired sessions and stale email tokens once immediately
// and then every interval, until ctx is cancelled. Expiry is already enforced
// at read time (SessionByTokenHash / UseEmailToken check expires_at), so this
// is purely hygiene: it keeps dead rows from accumulating forever. Started as
// a goroutine from the composition root; ctx is the server shutdown context.
func RunJanitor(ctx context.Context, c Cleaner, interval time.Duration, log *slog.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		Sweep(ctx, c, log)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Sweep performs one janitor pass, logging the per-table delete counts. A
// failed delete is logged and skipped — the next tick retries; a failure here
// must never take the server down. Exported so tests can drive a single pass.
func Sweep(ctx context.Context, c Cleaner, log *slog.Logger) {
	sessions, serr := c.DeleteExpiredSessions(ctx)
	tokens, terr := c.DeleteStaleEmailTokens(ctx)
	// During shutdown the pool may already be closing; that is not an error
	// worth alarming anyone about.
	if ctx.Err() != nil {
		return
	}
	if serr != nil {
		log.ErrorContext(ctx, "janitor: delete expired sessions failed", "err", serr)
	}
	if terr != nil {
		log.ErrorContext(ctx, "janitor: delete stale email tokens failed", "err", terr)
	}
	if serr == nil && terr == nil {
		log.InfoContext(ctx, "janitor sweep complete",
			"expired_sessions", sessions,
			"stale_email_tokens", tokens,
		)
	}
}
