package runs

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/typemore/typemore-server/internal/platform/httpx"
)

// maxBodyBytes caps the raw POST /runs body; a larger body is rejected 413
// before it is fully read (BACKEND.md §3, structural size check).
//
// 25 MiB, sized for log v2: a full-length telemetry run on the worst published
// dictionary measures ~21.5 MiB of log (the caps table in docs/RUNS.md), and
// the version is not knowable before the body is parsed, so the transport cap
// must admit the largest grammar. A v1 log is still bounded post-decode by
// maxLogBytesV1, so the pre-telemetry envelope did not widen. The cost is real
// and stated plainly in docs/RUNS.md — this remains the largest single
// allocation on the ingest path.
const maxBodyBytes = 50 << 19 // 25 MiB (perf.MaxBodyBytes mirrors this)

// Pagination bounds for GET /runs.
const (
	defaultLimit = 20
	maxLimit     = 100
)

// Routes returns the runs router, mounted at /api/v1/runs.
//
// The auth boundary is drawn HERE rather than by the caller wrapping the whole
// mount, because two routes are deliberately outside it: GET /{id}/replay and
// GET /{id}/replay/log serve accepted runs to anyone, which is what makes a
// leaderboard row watchable without an account. Keeping the split next to the
// routes means the public ones cannot be widened by an unrelated change to the
// mount.
//
// requireOrigin is the CSRF check (a no-op on safe methods) and requireAuth
// rejects sessionless requests; both come from the auth domain via the
// composition root.
func (s *Service) Routes(requireOrigin, requireAuth func(http.Handler) http.Handler) http.Handler {
	r := chi.NewRouter()
	r.Get("/{id}/replay", s.handlePublicReplay)
	r.Get("/{id}/replay/log", s.handlePublicReplayLog)
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

// handleIngest accepts a finished run: rate-limit → body cap → structural
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

	// A restricted account's run is refused BEFORE the body is read: there is
	// nothing to store, so there is no reason to spend a 6.5 MiB read on it.
	//
	// Refused, not silently dropped. The standing decision is an honest "not
	// counted" rather than a shadow ban — a player typing into a void and
	// wondering why their rank never moves is a worse outcome than one who
	// knows, and it is also the version that can be appealed.
	if s.restrictions != nil {
		restricted, err := s.restrictions.IsRestricted(r.Context(), userID)
		if err != nil {
			s.log.Error("resolve account restriction", "err", err, "userId", userID)
			s.writeError(w, r, apiErrInternal)
			return
		}
		if restricted {
			s.writeError(w, r, apiErrRestricted)
			return
		}
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

	limit := httpx.ParseLimit(r.URL.Query().Get("limit"), defaultLimit, maxLimit)
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
// SET UP the playback, and nothing about how the run was judged.
//
// `seed` and `dictHash` are what stand in for the word list. The client feeds
// (seed, setup.generation) to the core's own generator to reproduce the exact
// text, after checking that the dictionary it holds hashes to `dictHash` — the
// same check the live match path makes. Serialising the words here instead
// would put a second copy of the generator's output on the wire for the server
// to keep in step with the core. Both fields are already on the owner's
// authenticated summary (docs/RUNS.md), so this is reach, not disclosure.
//
// The event log is NOT here. It has its own route (handlePublicReplayLog),
// because inlining it meant gunzipping a 373 KiB blob into 2.0 MiB and letting
// the JSON encoder compact that into a third buffer — 5.5 MiB live per request
// (docs/PERFORMANCE.md, zone 6).
type replayView struct {
	RunID         uuid.UUID       `json:"runId"`
	DisplayName   string          `json:"displayName"`
	Mode          string          `json:"mode"`
	DurationMs    *int32          `json:"durationMs,omitempty"`
	WordCount     *int32          `json:"wordCount,omitempty"`
	Lang          string          `json:"lang"`
	Seed          int64           `json:"seed"`
	DictHash      string          `json:"dictHash"`
	Setup         json.RawMessage `json:"setup"`
	ServerMetrics json.RawMessage `json:"serverMetrics"`
	ServerScore   json.RawMessage `json:"serverScore"`
	Grade         string          `json:"grade"`
	AchievedAt    time.Time       `json:"achievedAt"`
}

// toReplayView converts the domain PublicReplay to its JSON view. As with
// summaryView, the two structs share field names, types, and order (only json
// tags differ), so a plain conversion suffices.
func toReplayView(p PublicReplay) replayView {
	return replayView(p)
}

// replayLogCacheControl: a judged run's event log is immutable. The row is
// written once at ingestion and never updated — revalidation only ever moves
// the verdict columns — so the run id alone identifies the bytes forever.
const replayLogCacheControl = "public, max-age=31536000, immutable"

// handlePublicReplay serves one accepted run's playback METADATA, to anyone.
//
// This is half of what makes a leaderboard row watchable: the board links a run
// id, this returns the setup and the server's numbers, and
// GET /{id}/replay/log returns the log itself. The owner-only ?log=1 path is
// untouched and still the only way to reach a run that is pending, flagged or
// rejected.
func (s *Service) handlePublicReplay(w http.ResponseWriter, r *http.Request) {
	if !s.replayLimiter.Allow(httpx.ClientIP(r)) {
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

	s.writeJSON(w, http.StatusOK, toReplayView(run))
}

// handlePublicReplayLog serves the run's event log as the STORED GZIP BYTES.
//
// Nothing is decompressed, re-encoded or base64'd: the blob the ingest path
// wrote goes straight to the ResponseWriter under Content-Encoding: gzip, and
// every conforming client — every browser, Go's own Transport, curl with
// --compressed — decodes it into the plain EventLog JSON it always was. That is
// the whole point. Zone 6 measured the inlined version at 7.5 MiB allocated and
// 5.5 MiB LIVE per request to hand out a 373 KiB blob; passthrough is ~0.4 MiB.
//
// The cost, accepted deliberately: watching a row is now TWO requests instead of
// one, because a gzip stream cannot be a field inside a JSON object.
//
// No Vary: Accept-Encoding. The dictionary routes need it because they choose
// between two representations; this route has exactly one, always gzip, so
// claiming the response varies would be false and would only cost caches a
// dimension.
//
// Rate limiting: the SAME per-IP bucket as the metadata route, by construction —
// one limiter, one key. Giving the new route its own bucket would have doubled
// what one anonymous IP may command the moment the payload was split in two,
// which is a refactor quietly loosening a security control. A spectator now
// spends two tokens per run watched; the default burst of 30 is therefore 15
// runs, and the memory that burst can command went DOWN, not up.
func (s *Service) handlePublicReplayLog(w http.ResponseWriter, r *http.Request) {
	if !s.replayLimiter.Allow(httpx.ClientIP(r)) {
		s.writeError(w, r, apiErrRateLimited)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		s.writeError(w, r, apiErrNotFound)
		return
	}

	// Eligibility is resolved BEFORE the conditional check, even though a 304
	// needs no bytes: answering "not modified" for a run the caller may not
	// watch would be a second, softer answer beside the 404 the access matrix
	// promises. One query, the same three rules, then caching.
	gz, err := s.store.PublicReplayLog(r.Context(), id)
	if err != nil {
		s.writeNotFoundOr(w, r, err)
		return
	}

	etag := `"` + id.String() + `"`
	h := w.Header()
	h.Set("Cache-Control", replayLogCacheControl)
	h.Set("ETag", etag)
	if httpx.ETagMatches(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	// application/json is the type of the DECODED representation, which is what
	// Content-Type describes; Content-Encoding says how it arrived.
	h.Set("Content-Type", "application/json")
	h.Set("Content-Encoding", "gzip")
	h.Set("Content-Length", strconv.Itoa(len(gz)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(gz)
}

// writeNotFoundOr maps ErrNotFound to a 404 and anything else to a generic 500.
func (s *Service) writeNotFoundOr(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, ErrNotFound) {
		s.writeError(w, r, apiErrNotFound)
		return
	}
	s.writeError(w, r, err)
}

// encodeCursor packs a keyset position into an opaque base64url token.
// Nanosecond precision is exact for Postgres timestamptz (microseconds), so the
// round-trip reproduces the stored instant for the equality tie-break.
func encodeCursor(c Cursor) string {
	return httpx.EncodeCursor(strconv.FormatInt(c.CreatedAt.UTC().UnixNano(), 10), c.ID.String())
}

// decodeCursor reverses encodeCursor, rejecting anything malformed.
func decodeCursor(token string) (Cursor, error) {
	parts, err := httpx.DecodeCursor(token, 2)
	if err != nil {
		return Cursor{}, err
	}
	nanos, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return Cursor{}, err
	}
	id, err := uuid.Parse(parts[1])
	if err != nil {
		return Cursor{}, err
	}
	return Cursor{CreatedAt: time.Unix(0, nanos).UTC(), ID: id}, nil
}
