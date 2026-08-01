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

// Service serves the public leaderboard read model.
type Service struct {
	store  Store
	userID UserIDFunc
	log    *slog.Logger
}

// NewService wires the leaderboard service.
func NewService(store Store, userID UserIDFunc, log *slog.Logger) *Service {
	return &Service{store: store, userID: userID, log: log}
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
