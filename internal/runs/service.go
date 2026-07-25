package runs

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/google/uuid"
)

// RateLimiter decides whether an action keyed by a string may proceed. It is the
// same shape as the auth domain's limiter so the composition root can reuse that
// machinery (an in-memory bucket now, Redis later) keyed per user.
type RateLimiter interface {
	Allow(key string) bool
}

// UserIDFunc extracts the authenticated user's id from the request context. The
// composition root provides it (adapting the auth session middleware), keeping
// this domain free of an auth import. ok is false when no user is present.
type UserIDFunc func(ctx context.Context) (uuid.UUID, bool)

// Service holds the run-ingestion logic. HTTP handlers call it; it calls the
// store and the limiters.
//
// There are two limiters because the two exposed costs are different: ingestion
// is per-user and generous (a typing session is many runs), while public replay
// is per-IP and modest (an event log is the heaviest payload this server hands
// out, and the caller needs no account to ask for one). BOTH public replay
// routes — metadata and log — draw on that one bucket, so splitting the payload
// did not double what an anonymous IP may command.
type Service struct {
	store         Store
	limiter       RateLimiter
	replayLimiter RateLimiter
	userID        UserIDFunc
	log           *slog.Logger
}

// NewService wires the runs service.
func NewService(store Store, limiter, replayLimiter RateLimiter, userID UserIDFunc, log *slog.Logger) *Service {
	return &Service{
		store: store, limiter: limiter, replayLimiter: replayLimiter,
		userID: userID, log: log,
	}
}

// currentUser resolves the authenticated user id, writing an unauthorized error
// and returning ok=false if absent (defensive: routes sit behind RequireAuth).
func (s *Service) currentUser(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, ok := s.userID(r.Context())
	if !ok {
		s.writeError(w, r, apiErrUnauthorized)
		return uuid.Nil, false
	}
	return id, true
}

// --- shared HTTP helpers (mirroring the auth domain's, kept private here) ---

// writeJSON encodes v as the response body with the given status.
func (s *Service) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
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
	s.log.ErrorContext(r.Context(), "runs request failed", "err", err, "path", r.URL.Path)
	s.writeJSON(w, apiErrInternal.status, apiErrInternal)
}
