package runs

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// maxBodyBytes caps the raw POST /runs body at 2 MB; a larger body is rejected
// 413 before it is fully read (BACKEND.md §3, structural size check).
const maxBodyBytes = 2 << 20 // 2 MiB

// Pagination bounds for GET /runs.
const (
	defaultLimit = 20
	maxLimit     = 100
)

// Routes returns the runs router, mounted at /api/v1/runs.
//
// The auth boundary is drawn HERE rather than by the caller wrapping the whole
// mount, because one route is deliberately outside it: GET /{id}/replay serves
// accepted runs to anyone, which is what makes a leaderboard row watchable
// without an account. Keeping the split next to the routes means the public one
// cannot be widened by an unrelated change to the mount.
//
// requireOrigin is the CSRF check (a no-op on safe methods) and requireAuth
// rejects sessionless requests; both come from the auth domain via the
// composition root.
func (s *Service) Routes(requireOrigin, requireAuth func(http.Handler) http.Handler) http.Handler {
	r := chi.NewRouter()
	r.Get("/{id}/replay", s.handlePublicReplay)
	r.Group(func(r chi.Router) {
		r.Use(requireOrigin)
		r.Use(requireAuth)
		r.Post("/", s.handleIngest)
		r.Get("/", s.handleList)
		r.Get("/{id}", s.handleDetail)
	})
	return r
}

// ingestResponse is the 202 body: the client keeps showing its local preview
// while the run sits 'pending' for the future replay worker.
type ingestResponse struct {
	ID     uuid.UUID `json:"id"`
	Status string    `json:"status"`
}

// handleIngest accepts a finished run: rate-limit → 2 MB cap → structural
// validation → gzip + store 'pending' → 202 { id, status }.
func (s *Service) handleIngest(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUser(w, r)
	if !ok {
		return
	}

	// Per-user rate limit (generous; a typing session is many runs). Checked
	// before reading the body so a spamming client cannot force large reads.
	if !s.limiter.Allow(userID.String()) {
		s.writeError(w, r, apiErrRateLimited)
		return
	}

	// Cap the raw body. MaxBytesReader makes an oversized read fail with a
	// *http.MaxBytesError, which we translate to 413.
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	var req ingestRequest
	if err := dec.Decode(&req); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			s.writeError(w, r, apiErrPayloadTooLarge)
			return
		}
		s.writeError(w, r, apiErrBadRequest("request body is not valid JSON"))
		return
	}

	params, verr := validateIngest(userID, &req)
	if verr != nil {
		s.writeError(w, r, verr)
		return
	}

	run, err := s.store.CreateRun(r.Context(), params)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusAccepted, ingestResponse{ID: run.ID, Status: run.Status})
}

// summaryView is the JSON shape of a run without its log payload. The replay
// worker's verdict (serverMetrics / serverScore / validation / validatedAt) is
// omitted entirely until the run has been judged, so a client can tell
// "not replayed yet" from "replayed and empty".
type summaryView struct {
	ID            uuid.UUID       `json:"id"`
	Mode          string          `json:"mode"`
	DurationMs    *int32          `json:"durationMs,omitempty"`
	WordCount     *int32          `json:"wordCount,omitempty"`
	Lang          string          `json:"lang"`
	Seed          int64           `json:"seed"`
	DictHash      string          `json:"dictHash"`
	Setup         json.RawMessage `json:"setup"`
	ClientMetrics json.RawMessage `json:"clientMetrics"`
	ClientScore   json.RawMessage `json:"clientScore"`
	ScoreVersion  int16           `json:"scoreVersion"`
	Status        string          `json:"status"`
	ServerMetrics json.RawMessage `json:"serverMetrics,omitempty"`
	ServerScore   json.RawMessage `json:"serverScore,omitempty"`
	Validation    json.RawMessage `json:"validation,omitempty"`
	ValidatedAt   *time.Time      `json:"validatedAt,omitempty"`
	LogBytes      int32           `json:"logBytes"`
	CreatedAt     time.Time       `json:"createdAt"`
}

// toSummaryView converts the domain Summary to its JSON view. The two structs
// share field names, types, and order (only json tags differ), so a plain
// conversion suffices.
func toSummaryView(s Summary) summaryView {
	return summaryView(s)
}

// listResponse is the GET /runs page: a slice of summaries plus an opaque cursor
// for the next page (empty when the last page has been reached).
type listResponse struct {
	Runs       []summaryView `json:"runs"`
	NextCursor string        `json:"nextCursor,omitempty"`
}

// handleList returns the user's own runs newest-first, keyset-paginated on
// (created_at, id).
func (s *Service) handleList(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUser(w, r)
	if !ok {
		return
	}

	limit := parseLimit(r.URL.Query().Get("limit"))
	var after *Cursor
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		cur, err := decodeCursor(raw)
		if err != nil {
			s.writeError(w, r, apiErrBadRequest("invalid cursor"))
			return
		}
		after = &cur
	}

	// Fetch one extra row to learn whether another page exists without a second
	// query or an empty trailing page.
	rows, err := s.store.ListRuns(r.Context(), userID, after, int32(limit+1))
	if err != nil {
		s.writeError(w, r, err)
		return
	}

	next := ""
	if len(rows) > limit {
		last := rows[limit-1]
		rows = rows[:limit]
		next = encodeCursor(Cursor{CreatedAt: last.CreatedAt, ID: last.ID})
	}

	views := make([]summaryView, len(rows))
	for i := range rows {
		views[i] = toSummaryView(rows[i])
	}
	s.writeJSON(w, http.StatusOK, listResponse{Runs: views, NextCursor: next})
}

// handleDetail returns one own run's summary; with ?log=1 it streams the
// gunzipped EventLog JSON instead (the frontend's later replay feature).
func (s *Service) handleDetail(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUser(w, r)
	if !ok {
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		// A non-uuid id cannot name any run; treat as not found (no enumeration).
		s.writeError(w, r, apiErrNotFound)
		return
	}

	if r.URL.Query().Get("log") == "1" {
		gz, err := s.store.RunLog(r.Context(), id, userID)
		if err != nil {
			s.writeNotFoundOr(w, r, err)
			return
		}
		raw, err := gunzipLog(gz)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(raw)
		return
	}

	run, err := s.store.Run(r.Context(), id, userID)
	if err != nil {
		s.writeNotFoundOr(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, toSummaryView(run))
}

// replayView is one accepted run as a spectator sees it: everything needed to
// watch it back, and nothing about how it was judged. The event log is inlined
// rather than linked so watching a leaderboard row is ONE request.
type replayView struct {
	RunID         uuid.UUID       `json:"runId"`
	DisplayName   string          `json:"displayName"`
	Mode          string          `json:"mode"`
	DurationMs    *int32          `json:"durationMs,omitempty"`
	WordCount     *int32          `json:"wordCount,omitempty"`
	Lang          string          `json:"lang"`
	Setup         json.RawMessage `json:"setup"`
	ServerMetrics json.RawMessage `json:"serverMetrics"`
	ServerScore   json.RawMessage `json:"serverScore"`
	Grade         string          `json:"grade"`
	AchievedAt    time.Time       `json:"achievedAt"`
	Log           json.RawMessage `json:"log"`
}

// handlePublicReplay streams one accepted run for playback, to anyone.
//
// This is what makes a leaderboard row watchable: the board links a run id, and
// this returns the setup, the log and the server's numbers in one response. The
// owner-only ?log=1 path is untouched and still the only way to reach a run
// that is pending, flagged or rejected.
//
// Rate-limited per IP: the log is the largest thing this server serves, and no
// session is required to ask for one.
func (s *Service) handlePublicReplay(w http.ResponseWriter, r *http.Request) {
	if !s.replayLimiter.Allow(clientIP(r)) {
		s.writeError(w, r, apiErrRateLimited)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		s.writeError(w, r, apiErrNotFound)
		return
	}

	run, err := s.store.PublicReplay(r.Context(), id)
	if err != nil {
		s.writeNotFoundOr(w, r, err)
		return
	}
	raw, err := gunzipLog(run.Log)
	if err != nil {
		s.writeError(w, r, err)
		return
	}

	s.writeJSON(w, http.StatusOK, replayView{
		RunID:         run.RunID,
		DisplayName:   run.DisplayName,
		Mode:          run.Mode,
		DurationMs:    run.DurationMs,
		WordCount:     run.WordCount,
		Lang:          run.Lang,
		Setup:         run.Setup,
		ServerMetrics: run.ServerMetrics,
		ServerScore:   run.ServerScore,
		Grade:         run.Grade,
		AchievedAt:    run.AchievedAt,
		Log:           raw,
	})
}

// clientIP is the rate-limit key for the one unauthenticated endpoint here.
// RemoteAddr only: the server is expected to sit behind a proxy that sets it,
// and trusting a forwarded header would make the limit trivially bypassable.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// writeNotFoundOr maps ErrNotFound to a 404 and anything else to a generic 500.
func (s *Service) writeNotFoundOr(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, ErrNotFound) {
		s.writeError(w, r, apiErrNotFound)
		return
	}
	s.writeError(w, r, err)
}

// parseLimit clamps the ?limit= query to [1, maxLimit], defaulting on absent or
// unparseable input.
func parseLimit(raw string) int {
	if raw == "" {
		return defaultLimit
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return defaultLimit
	}
	if n > maxLimit {
		return maxLimit
	}
	return n
}

// encodeCursor packs a keyset position into an opaque base64url token.
// Nanosecond precision is exact for Postgres timestamptz (microseconds), so the
// round-trip reproduces the stored instant for the equality tie-break.
func encodeCursor(c Cursor) string {
	raw := fmt.Sprintf("%d:%s", c.CreatedAt.UTC().UnixNano(), c.ID)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// decodeCursor reverses encodeCursor, rejecting anything malformed.
func decodeCursor(token string) (Cursor, error) {
	b, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return Cursor{}, err
	}
	ns, idStr, found := strings.Cut(string(b), ":")
	if !found {
		return Cursor{}, errors.New("runs: malformed cursor")
	}
	nanos, err := strconv.ParseInt(ns, 10, 64)
	if err != nil {
		return Cursor{}, err
	}
	id, err := uuid.Parse(idStr)
	if err != nil {
		return Cursor{}, err
	}
	return Cursor{CreatedAt: time.Unix(0, nanos).UTC(), ID: id}, nil
}
