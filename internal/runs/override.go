package runs

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/typemore/typemore-server/internal/runstatus"
)

// THE OPERATOR'S HALF OF THE VERDICT.
//
// The replay worker decides every run's status, and until now its decision was
// final in the strongest sense: nothing could disagree with it. That is right
// while a verdict is arithmetic — a seed that does not reproduce, a log the
// reducer refused — and wrong the moment one is a JUDGEMENT.
//
// `superhuman-burst` is a judgement. It fires on speed; speed is a continuum
// with real people at the top of it; and `leaderboard_eligible_runs` selects on
// `status = 'accepted'`, so a wrongly flagged run does not annoy a player, it
// removes their result from the board. A check whose false positives cannot be
// undone has to be set so timidly that it catches nothing — which is how the
// speed ceiling ended up calibrated against this deployment's own slow
// population instead of against what a person can actually do.
//
// This file is what lets that ceiling be set honestly.

// OverrideParams is one operator decision about one run.
type OverrideParams struct {
	RunID    uuid.UUID
	ToStatus string
	// Reason is required by the schema as well as here. An override with no
	// stated reason is indistinguishable from a mistake six months later, and
	// this is the one place a human overrules the evidence.
	Reason    string
	DecidedBy uuid.UUID
}

// StatusOverride is a recorded decision, as the admin surface serves it.
type StatusOverride struct {
	ID            uuid.UUID `json:"id"`
	RunID         uuid.UUID `json:"runId"`
	FromStatus    string    `json:"fromStatus"`
	ToStatus      string    `json:"toStatus"`
	Reason        string    `json:"reason"`
	DecidedBy     uuid.UUID `json:"decidedBy"`
	DecidedByName string    `json:"decidedByName,omitempty"`
	DecidedAt     time.Time `json:"decidedAt"`
}

// ReviewRow is one entry of the review queue.
type ReviewRow struct {
	ID          uuid.UUID `json:"id"`
	UserID      uuid.UUID `json:"userId,omitempty"`
	DisplayName string    `json:"displayName,omitempty"`
	Status      string    `json:"status"`
	Mode        string    `json:"mode"`
	Lang        string    `json:"lang,omitempty"`
	Suspicion   float64   `json:"suspicion"`
	// Overridden marks a run a human has already decided about — the queue
	// keeps showing it, because "already handled" is what a reviewer most needs
	// to know before handling it again.
	Overridden bool            `json:"overridden"`
	Metrics    json.RawMessage `json:"metrics,omitempty"`
	Validation json.RawMessage `json:"validation,omitempty"`
	CreatedAt  time.Time       `json:"createdAt"`
}

// Moderator is the store half of the operator surface, declared at the consumer
// like every other seam in this package.
type Moderator interface {
	// OverrideRunStatus moves a run's status and records the decision, with the
	// leaderboard projection inside the same transaction.
	OverrideRunStatus(ctx context.Context, p OverrideParams) (StatusOverride, error)
	// RunStatusOverrides is one run's decision history, newest first.
	RunStatusOverrides(ctx context.Context, runID uuid.UUID) ([]StatusOverride, error)
	// RunsForReview lists judged runs at or above a suspicion floor, worst first.
	RunsForReview(ctx context.Context, minSuspicion float64, limit int32) ([]ReviewRow, error)
}

// WithModerator attaches the operator surface. Nil leaves the admin routes
// answering 503 rather than panicking, which is the same shape every other
// optional seam in this service takes.
func (s *Service) WithModerator(m Moderator) *Service {
	s.moderator = m
	return s
}

// AdminRoutes is the operator subtree, mounted by the composition root under
// /api/v1/admin/runs with the permission middleware supplied.
//
// The split between read and write mirrors moderation's: listing the queue is a
// different capability from acting on it, so a future triage role can look
// without being able to reinstate.
func (s *Service) AdminRoutes(requireRead, requireWrite func(http.Handler) http.Handler) http.Handler {
	r := chi.NewRouter()
	r.Group(func(r chi.Router) {
		r.Use(requireRead)
		r.Get("/review", s.handleReviewQueue)
		r.Get("/{id}/overrides", s.handleRunOverrides)
	})
	r.Group(func(r chi.Router) {
		r.Use(requireWrite)
		r.Post("/{id}/status", s.handleOverrideStatus)
	})
	return r
}

type overrideRequest struct {
	Status string `json:"status"`
	Reason string `json:"reason"`
}

func (s *Service) handleOverrideStatus(w http.ResponseWriter, r *http.Request) {
	if s.moderator == nil {
		s.writeError(w, r, apiErrUnavailable)
		return
	}
	actor, ok := s.currentUser(w, r)
	if !ok {
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		s.writeError(w, r, apiErrBadRequest("run id is not a uuid"))
		return
	}
	var req overrideRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		s.writeError(w, r, apiErrBadRequest("body is not valid json"))
		return
	}
	// 'pending' is refused at the edge as well as in the store: it is not a
	// judgement to disagree with, and moving a run there would hand it back to
	// the worker as new work.
	switch req.Status {
	case runstatus.Accepted, runstatus.Flagged, runstatus.Rejected:
	default:
		s.writeError(w, r, apiErrBadRequest(
			"status must be one of accepted, flagged, rejected"))
		return
	}
	if strings.TrimSpace(req.Reason) == "" {
		s.writeError(w, r, apiErrBadRequest("reason is required"))
		return
	}

	decision, err := s.moderator.OverrideRunStatus(r.Context(), OverrideParams{
		RunID:     id,
		ToStatus:  req.Status,
		Reason:    strings.TrimSpace(req.Reason),
		DecidedBy: actor,
	})
	switch {
	case errors.Is(err, ErrNotFound):
		s.writeError(w, r, apiErrNotFound)
		return
	case errors.Is(err, ErrRunNotJudged):
		s.writeError(w, r, apiErrConflict("run has not been judged yet"))
		return
	case errors.Is(err, ErrStatusUnchanged):
		s.writeError(w, r, apiErrConflict("run already has that status"))
		return
	case err != nil:
		s.log.Error("override run status", "err", err, "run", id)
		s.writeError(w, r, apiErrInternal)
		return
	}
	s.writeJSON(w, http.StatusOK, decision)
}

func (s *Service) handleRunOverrides(w http.ResponseWriter, r *http.Request) {
	if s.moderator == nil {
		s.writeError(w, r, apiErrUnavailable)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		s.writeError(w, r, apiErrBadRequest("run id is not a uuid"))
		return
	}
	rows, err := s.moderator.RunStatusOverrides(r.Context(), id)
	if err != nil {
		s.log.Error("list run overrides", "err", err, "run", id)
		s.writeError(w, r, apiErrInternal)
		return
	}
	if rows == nil {
		rows = []StatusOverride{}
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"overrides": rows})
}

// reviewQueueDefaultFloor is the suspicion a run needs before it is worth a
// human's time by default.
//
// Not zero: every judged run carries a suspicion, most of them a rounding error
// from key rollover, and a queue that listed all of them would be a list of the
// whole database. 0.1 is where the real population stops — over the 127 honest
// runs of the 2026-08-03 export the highest is 0.1776 and the mean is 0.0031 —
// so it is the floor at which the queue starts being short enough to read.
const reviewQueueDefaultFloor = 0.1

const reviewQueueMaxLimit = 200

func (s *Service) handleReviewQueue(w http.ResponseWriter, r *http.Request) {
	if s.moderator == nil {
		s.writeError(w, r, apiErrUnavailable)
		return
	}
	floor := reviewQueueDefaultFloor
	if raw := r.URL.Query().Get("minSuspicion"); raw != "" {
		parsed, err := strconv.ParseFloat(raw, 64)
		if err != nil || parsed < 0 {
			s.writeError(w, r, apiErrBadRequest("minSuspicion must be a non-negative number"))
			return
		}
		floor = parsed
	}
	limit := int32(50)
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			s.writeError(w, r, apiErrBadRequest("limit must be a positive integer"))
			return
		}
		limit = int32(min(parsed, reviewQueueMaxLimit))
	}

	rows, err := s.moderator.RunsForReview(r.Context(), floor, limit)
	if err != nil {
		s.log.Error("review queue", "err", err)
		s.writeError(w, r, apiErrInternal)
		return
	}
	if rows == nil {
		rows = []ReviewRow{}
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"runs":         rows,
		"minSuspicion": floor,
	})
}
