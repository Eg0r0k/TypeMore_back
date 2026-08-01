package quote

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/typemore/typemore-server/internal/platform/httpx"
)

// Service serves the public quote registry.
//
// There is no UserIDFunc here, unlike the leaderboard domain: every route is
// anonymous. A quote is a static, published artefact — who is asking changes
// nothing about the answer.
type Service struct {
	store Store
	log   *slog.Logger
}

// NewService wires the quote service.
func NewService(store Store, log *slog.Logger) *Service {
	return &Service{store: store, log: log}
}

// --- shared HTTP helpers (mirroring the sibling domains', kept private) ---

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
	s.log.ErrorContext(r.Context(), "quote request failed", "err", err, "path", r.URL.Path)
	s.writeJSON(w, apiErrInternal.status, apiErrInternal)
}
