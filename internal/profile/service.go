package profile

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/google/uuid"
)

// UserIDFunc extracts the authenticated user's id from the request context —
// the composition root adapts the auth session middleware, keeping this domain
// free of an auth import. Every profile route REQUIRES a session: the profile
// is the caller's own data and nothing else's (see doc.go).
type UserIDFunc func(ctx context.Context) (uuid.UUID, bool)

// BucketInfo is a parsed leaderboard bucket key, for decorating a PB card the
// way the boards' own index decorates its entries. Exactly one of the two
// shapes is populated: a language board carries mode/dimension/lang/textSource;
// a quote board carries only the quote id.
type BucketInfo struct {
	Mode       string
	DurationMs *int32
	WordCount  *int32
	Lang       string
	TextSource string
	QuoteID    *uuid.UUID
}

// BucketParser turns a bucket key into its components. The leaderboard domain
// owns the key format (exactly one producer, one parser — LEADERBOARDS.md), so
// the composition root injects an adapter over leaderboard.ParseBucketKey
// rather than this package growing a second parser. ok is false for a key this
// build cannot parse — served raw rather than dropped, so a format extension
// cannot silently hide a player's own PB.
type BucketParser func(key string) (BucketInfo, bool)

// Service serves the session-scoped profile read model.
type Service struct {
	store  Store
	userID UserIDFunc
	bucket BucketParser
	log    *slog.Logger
}

// NewService wires the profile service.
func NewService(store Store, userID UserIDFunc, bucket BucketParser, log *slog.Logger) *Service {
	return &Service{store: store, userID: userID, bucket: bucket, log: log}
}

// --- shared HTTP helpers (mirroring the sibling domains', kept private) -----

func (s *Service) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		s.log.Error("encode response", "err", err)
	}
}

func (s *Service) writeError(w http.ResponseWriter, r *http.Request, err error) {
	var apiErr *apiError
	if errors.As(err, &apiErr) {
		s.writeJSON(w, apiErr.status, apiErr)
		return
	}
	s.log.ErrorContext(r.Context(), "profile request failed", "err", err, "path", r.URL.Path)
	s.writeJSON(w, apiErrInternal.status, apiErrInternal)
}

// currentUser resolves the session or answers 401.
func (s *Service) currentUser(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, ok := s.userID(r.Context())
	if !ok {
		s.writeJSON(w, apiErrUnauthorized.status, apiErrUnauthorized)
		return uuid.Nil, false
	}
	return id, true
}
