package quote

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/typemore/typemore-server/internal/platform/httpx"
)

// maxWithdrawalReason bounds the note. Long enough for a moderator to explain
// themselves, short enough that the column is not a free-text dumping ground.
const maxWithdrawalReason = 500

// The quote moderation surface: /api/v1/admin/quotes/… (docs/REPORTS.md, "The
// action a report points at").
//
// It exists because a report queue needs something to DO. A moderator holding
// forty reports on one offensive quote must be able to act on them, and before
// this surface the only writer of the corpus was `make import-quotes` — so the
// answer to "this quote is bad" was a shell on the box, or nothing.
//
// What this surface cannot do is as important as what it can. There is no
// publish, no edit, no delete: the quote's bytes, language and hash are exactly
// as immutable from here as they are from the public API, because a stored run
// regenerates its text from the row it was played on. The only bit a moderator
// may move is whether the quote is OFFERED — which no run's replay depends on.

// AdminActorFunc resolves the authenticated moderator performing a request.
// Supplied by the composition root over the auth middleware's context user, so
// this domain never imports auth. The permission middlewares this subtree is
// mounted behind guarantee it succeeds; a false return is a wiring bug and is
// answered as a plain 404, like every other miss on the admin tree.
type AdminActorFunc func(r *http.Request) (uuid.UUID, bool)

// AdminService is the moderation surface over the quote corpus. It is a
// SEPARATE type from Service, holding a separate interface, on purpose: the
// public service is structurally incapable of withdrawing a quote, rather than
// merely never routed to do so.
type AdminService struct {
	store ModerationStore
	actor AdminActorFunc
	log   *slog.Logger
}

// NewAdminService wires the quote moderation surface.
func NewAdminService(store ModerationStore, actor AdminActorFunc, log *slog.Logger) *AdminService {
	return &AdminService{store: store, actor: actor, log: log}
}

// Routes returns the /admin/quotes subtree. The permission middlewares are
// passed IN, exactly as the ban surface takes them: this domain knows that one
// route reads and two write, and nothing about roles.
func (s *AdminService) Routes(requireRead, requireWrite func(http.Handler) http.Handler) http.Handler {
	r := chi.NewRouter()
	r.With(requireRead).Get("/{id}", s.handleModerated)
	r.Group(func(r chi.Router) {
		r.Use(requireWrite)
		r.Post("/{id}/withdrawal", s.handleWithdraw)
		r.Delete("/{id}/withdrawal", s.handleRestore)
	})
	return r
}

// withdrawalView is the moderation record on a quote. `withdrawn` is stated
// rather than left implicit in the presence of `at`, so a client renders the
// state from one field it cannot misread.
type withdrawalView struct {
	Withdrawn bool       `json:"withdrawn"`
	At        *time.Time `json:"at,omitempty"`
	By        *uuid.UUID `json:"by,omitempty"`
	Reason    string     `json:"reason,omitempty"`
}

func toWithdrawalView(w Withdrawal) withdrawalView {
	if w.At.IsZero() {
		return withdrawalView{Withdrawn: false}
	}
	at := w.At
	return withdrawalView{Withdrawn: true, At: &at, By: w.By, Reason: w.Reason}
}

// moderatedView is one quote as a moderator sees it: the text (they have to
// read what they are judging) plus the withdrawal record.
type moderatedView struct {
	ID         uuid.UUID      `json:"id"`
	Lang       string         `json:"lang"`
	UpstreamID int32          `json:"upstreamId"`
	Text       string         `json:"text"`
	Source     string         `json:"source"`
	Length     int32          `json:"length"`
	LenGroup   string         `json:"lenGroup"`
	Superseded bool           `json:"superseded"`
	CreatedAt  time.Time      `json:"createdAt"`
	Withdrawal withdrawalView `json:"withdrawal"`
}

// handleModerated serves GET /admin/quotes/{id}.
func (s *AdminService) handleModerated(w http.ResponseWriter, r *http.Request) {
	id, ok := s.quoteID(w, r)
	if !ok {
		return
	}
	q, record, err := s.store.Moderated(r.Context(), id)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, moderatedView{
		ID: q.ID, Lang: q.Lang, UpstreamID: q.UpstreamID, Text: q.Text,
		Source: q.Source, Length: q.Length, LenGroup: q.LenGroup.String(),
		Superseded: q.Superseded, CreatedAt: q.CreatedAt,
		Withdrawal: toWithdrawalView(record),
	})
}

// withdrawRequest is the body of a withdrawal. reason is REQUIRED, for the same
// reason a ban's is: a decision with no note is one nobody can review later.
type withdrawRequest struct {
	Reason string `json:"reason"`
}

// withdrawResponse reports the record AND whether this call is what changed it,
// so a second identical call reads as "already withdrawn, by them, then" rather
// than as a fresh success. The ban surface answers with a diff for the same
// reason.
type withdrawResponse struct {
	ID         uuid.UUID      `json:"id"`
	Changed    bool           `json:"changed"`
	Withdrawal withdrawalView `json:"withdrawal"`
}

// handleWithdraw serves POST /admin/quotes/{id}/withdrawal.
func (s *AdminService) handleWithdraw(w http.ResponseWriter, r *http.Request) {
	id, ok := s.quoteID(w, r)
	if !ok {
		return
	}
	var body withdrawRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
		s.writeJSON(w, http.StatusBadRequest, apiErrBadWithdrawal)
		return
	}
	reason := strings.TrimSpace(body.Reason)
	if reason == "" || utf8.RuneCountInString(reason) > maxWithdrawalReason {
		s.writeJSON(w, http.StatusBadRequest, apiErrBadWithdrawal)
		return
	}
	actor, ok := s.actor(r)
	if !ok {
		http.NotFound(w, r)
		return
	}

	record, changed, err := s.store.Withdraw(r.Context(), id, actor, reason)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, withdrawResponse{
		ID: id, Changed: changed, Withdrawal: toWithdrawalView(record),
	})
}

// handleRestore serves DELETE /admin/quotes/{id}/withdrawal — idempotent, like
// revoking a ban: a quote that was never withdrawn answers `changed: false`
// rather than a 404 for a state the caller asked for and already has.
func (s *AdminService) handleRestore(w http.ResponseWriter, r *http.Request) {
	id, ok := s.quoteID(w, r)
	if !ok {
		return
	}
	changed, err := s.store.Restore(r.Context(), id)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, withdrawResponse{
		ID: id, Changed: changed, Withdrawal: withdrawalView{Withdrawn: false},
	})
}

// quoteID parses the path parameter, answering the surface's usual 404 for
// anything that is not a quote id.
func (s *AdminService) quoteID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		s.writeJSON(w, http.StatusNotFound, apiErrNotFound)
		return uuid.Nil, false
	}
	return id, true
}

func (s *AdminService) writeErr(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, ErrNotFound) {
		s.writeJSON(w, http.StatusNotFound, apiErrNotFound)
		return
	}
	s.log.ErrorContext(r.Context(), "quote admin request failed", "err", err, "path", r.URL.Path)
	s.writeJSON(w, apiErrInternal.status, apiErrInternal)
}

func (s *AdminService) writeJSON(w http.ResponseWriter, status int, v any) {
	if err := httpx.WriteJSON(w, status, v); err != nil {
		s.log.Error("encode response", "err", err)
	}
}
