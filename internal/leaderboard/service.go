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

// Service serves the public leaderboard read model.
type Service struct {
	store     Store
	userID    UserIDFunc
	withdrawn WithdrawnQuotesFunc
	log       *slog.Logger
}

// NewService wires the leaderboard service. withdrawn must not be nil; pass one
// that reports an empty set if nothing can be withdrawn in this build.
func NewService(store Store, userID UserIDFunc, withdrawn WithdrawnQuotesFunc, log *slog.Logger) *Service {
	return &Service{store: store, userID: userID, withdrawn: withdrawn, log: log}
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
