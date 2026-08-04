package leaderboard

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/typemore/typemore-server/internal/platform/httpx"
)

// UserIDFunc extracts the authenticated user's id from the request context. The
// composition root provides it (adapting the auth session middleware), keeping
// this domain free of an auth import. ok is false for an anonymous reader —
// which is the normal case here, since the boards are public.
type UserIDFunc func(ctx context.Context) (uuid.UUID, bool)

// WithdrawnQuotesFunc reports the quotes a moderator has taken out of
// circulation (docs/REPORTS.md). The board INDEX drops their boards; nothing
// else does — a direct link to such a board still answers, and the runs on it
// stay ranked and replayable, because withdrawal is a discovery rule and the
// results were legitimately played.
//
// It is a function supplied by the composition root rather than SQL inside this
// domain's own queries on purpose. Excluding those boards in SQL would mean
// rebuilding the bucket key from a quote id — `'quote:' || q.id` — which would
// make a SECOND producer of a format this codebase deliberately keeps to one
// (Bucket.Key / ParseBucketKey). The two spellings would then have to be kept
// in step by nobody, and the failure would be silent: the index would quietly
// stop hiding withdrawn boards.
type WithdrawnQuotesFunc func(ctx context.Context) (map[uuid.UUID]struct{}, error)

// RateLimiter is the narrow token-bucket seam, the same shape the auth, runs
// and profile domains declare, so the composition root can hand this one an
// instance without this package importing a sibling.
type RateLimiter interface {
	Allow(key string) bool
}

// Service serves the public leaderboard read model.
type Service struct {
	store  Store
	userID UserIDFunc
	// indexLimiter rations the ONE route here whose cost is not bounded by a
	// single board: `GET /` aggregates over EVERY board (up to ~9 881 quote
	// ones) plus the withdrawn-quote read, on every hit, for an anonymous
	// caller. A page read is bounded by its limit; the index is bounded by how
	// much the product has grown.
	//
	// Keyed by IP, like every other anonymous surface's bucket, and its own
	// instance rather than a share of auth's: an index flood must not spend the
	// budget that exists to protect argon2id.
	indexLimiter RateLimiter
	withdrawn    WithdrawnQuotesFunc
	log          *slog.Logger
}

// NewService wires the leaderboard service. withdrawn must not be nil; pass one
// that reports an empty set if nothing can be withdrawn in this build.
// indexLimiter may be nil, which disables the index bucket (tests, and a
// self-hosted stand that would rather not ration a read at all).
func NewService(store Store, userID UserIDFunc, withdrawn WithdrawnQuotesFunc, indexLimiter RateLimiter, log *slog.Logger) *Service {
	return &Service{
		store:        store,
		userID:       userID,
		withdrawn:    withdrawn,
		indexLimiter: indexLimiter,
		log:          log,
	}
}

// --- shared HTTP helpers (mirroring the auth/runs domains', kept private) ---

func (s *Service) writeJSON(w http.ResponseWriter, status int, v any) {
	if err := httpx.WriteJSON(w, status, v); err != nil {
		s.log.Error("encode response", "err", err)
	}
}

// writeError renders err. Known apiErrors are sent with their status/code;
// anything else is logged and returned as a generic 500 so internals never leak.
func (s *Service) writeError(w http.ResponseWriter, r *http.Request, err error) {
	var apiErr *apiError
	if errors.As(err, &apiErr) {
		s.writeJSON(w, apiErr.status, apiErr)
		return
	}
	s.log.ErrorContext(r.Context(), "leaderboard request failed", "err", err, "path", r.URL.Path)
	s.writeJSON(w, apiErrInternal.status, apiErrInternal)
}
