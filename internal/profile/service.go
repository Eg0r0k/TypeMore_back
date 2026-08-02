package profile

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/typemore/typemore-server/internal/platform/httpx"
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

// LayoutNamer maps a dictionary language onto the layout the heatmap should
// default to. The rule lives in internal/keyboard beside the asset; the
// composition root injects it so this domain stays layout-agnostic.
type LayoutNamer func(lang string) string

// RateLimiter decides whether an action keyed by a string may proceed. It is
// the same shape the auth and runs domains declare, so the composition root can
// hand this one an instance of the same token bucket without this package
// importing a sibling.
type RateLimiter interface {
	Allow(key string) bool
}

// Service serves the session-scoped profile read model.
type Service struct {
	store  Store
	userID UserIDFunc
	// searchLimiter rations the ONE public route whose cost does not scale with
	// a single account: /users?q= reads an index built over every user, while
	// every other route on this surface is bounded by one player's history. It
	// gets its own bucket rather than sharing auth's, because sharing would let
	// a search flood spend the budget that exists to protect argon2id and
	// outbound email.
	searchLimiter RateLimiter
	bucket        BucketParser
	layoutFor     LayoutNamer
	log           *slog.Logger
}

// NewService wires the profile service.
func NewService(store Store, searchLimiter RateLimiter, userID UserIDFunc, bucket BucketParser, layoutFor LayoutNamer, log *slog.Logger) *Service {
	return &Service{
		store: store, searchLimiter: searchLimiter, userID: userID,
		bucket: bucket, layoutFor: layoutFor, log: log,
	}
}

// --- shared HTTP helpers (mirroring the sibling domains', kept private) -----

func (s *Service) writeJSON(w http.ResponseWriter, status int, v any) {
	if err := httpx.WriteJSON(w, status, v); err != nil {
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
