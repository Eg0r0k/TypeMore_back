package moderation

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

// The report surfaces (docs/REPORTS.md): one player-facing route to file a
// report, and an admin subtree to work the queue.
//
// They are one service because they are one model seen from two sides, and two
// route sets because they are mounted behind different gates: filing needs a
// session and an Origin check, working the queue needs a permission.

// Bounds. maxComment matches the CHECK in 00026 — the constraint is the
// guarantee, this is the error message.
const (
	maxComment            = 1000
	maxResolutionNote     = 1000
	queueDefaultLimit     = 50
	queueMaxLimit         = 200
	subjectReportsLimit   = 200
	maxOpenReportsPerUser = 20
)

// RateLimiter decides whether an action keyed by a string may proceed — the
// same shape the auth and runs domains declare, so the composition root can
// hand this one an instance of the same token bucket.
type RateLimiter interface {
	Allow(key string) bool
}

// PrincipalFunc reads the authenticated player from the request context. The
// composition root adapts the auth session middleware, keeping this domain free
// of an auth import.
type PrincipalFunc func(r *http.Request) (uuid.UUID, bool)

// ReportService serves both report surfaces.
type ReportService struct {
	store     ReportStore
	principal PrincipalFunc
	actor     ActorFunc
	limiter   RateLimiter
	log       *slog.Logger
}

// NewReportService wires the report surfaces.
func NewReportService(store ReportStore, principal PrincipalFunc, actor ActorFunc,
	limiter RateLimiter, log *slog.Logger) *ReportService {
	return &ReportService{store: store, principal: principal, actor: actor, limiter: limiter, log: log}
}

// Routes returns the PLAYER-facing subtree, mounted at /api/v1/reports behind
// the Origin check and RequireAuth. Filing is authenticated on purpose:
// anonymous reports cannot be deduplicated, cannot be rate-limited beyond an
// IP, and cannot be weighed later by who filed them.
func (s *ReportService) Routes(requireOrigin, requireAuth func(http.Handler) http.Handler) http.Handler {
	r := chi.NewRouter()
	r.With(requireOrigin, requireAuth).Post("/", s.handleFile)
	return r
}

// AdminRoutes returns the queue subtree, mounted at /api/v1/admin/reports. The
// permission middlewares are passed in, as everywhere else on the admin tree.
func (s *ReportService) AdminRoutes(requireRead, requireWrite func(http.Handler) http.Handler) http.Handler {
	r := chi.NewRouter()
	r.Group(func(r chi.Router) {
		r.Use(requireRead)
		r.Get("/", s.handleQueue)
		r.Get("/{subjectType}/{subjectID}", s.handleSubject)
	})
	r.With(requireWrite).Post("/{subjectType}/{subjectID}/resolve", s.handleResolve)
	return r
}

// --- filing -----------------------------------------------------------------

type subjectBody struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

type fileRequest struct {
	Subject subjectBody `json:"subject"`
	Reason  string      `json:"reason"`
	Comment string      `json:"comment"`
}

type reportView struct {
	ID        uuid.UUID   `json:"id"`
	Subject   subjectBody `json:"subject"`
	Reason    string      `json:"reason"`
	Comment   string      `json:"comment,omitempty"`
	Status    string      `json:"status"`
	CreatedAt time.Time   `json:"createdAt"`
}

// handleFile serves POST /api/v1/reports.
//
// A repeat of a report the caller already has open answers 200 with that same
// report rather than 409: the player expressed a state that already holds, and
// making them read an error to learn "yes, we know" is worse than saying so.
// A first report answers 201.
func (s *ReportService) handleFile(w http.ResponseWriter, r *http.Request) {
	reporter, ok := s.principal(r)
	if !ok {
		s.writeErr(w, http.StatusUnauthorized, "unauthorized", "authentication required")
		return
	}
	if !s.limiter.Allow(reporter.String()) {
		s.writeErr(w, http.StatusTooManyRequests, "rate_limited", "too many reports, slow down")
		return
	}

	var body fileRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&body); err != nil {
		s.writeErr(w, http.StatusBadRequest, "bad_request", "malformed body")
		return
	}
	subject, err := parseSubject(body.Subject.Type, body.Subject.ID)
	if err != nil {
		s.writeErr(w, http.StatusBadRequest, "bad_request", "unknown subject")
		return
	}
	if !subject.ReasonValid(body.Reason) {
		s.writeErr(w, http.StatusBadRequest, "bad_request",
			"reason does not apply to that subject type")
		return
	}
	comment := strings.TrimSpace(body.Comment)
	if utf8.RuneCountInString(comment) > maxComment {
		s.writeErr(w, http.StatusBadRequest, "bad_request", "comment is too long")
		return
	}

	// A banned account cannot file: the restriction gate is the same one that
	// stops their runs counting, read through the same view.
	restricted, err := s.store.IsRestricted(r.Context(), reporter)
	if err != nil {
		s.internalErr(w, r, "restriction check", err)
		return
	}
	if restricted {
		s.writeErr(w, http.StatusForbidden, "restricted", "this account is restricted")
		return
	}

	// The breadth cap the token bucket cannot express: the limiter bounds how
	// FAST reports arrive, this bounds how much of the queue one person can be
	// holding open at once. Resolving their earlier reports frees the budget.
	open, err := s.store.OpenBy(r.Context(), reporter)
	if err != nil {
		s.internalErr(w, r, "count open reports", err)
		return
	}
	if open >= maxOpenReportsPerUser {
		s.writeErr(w, http.StatusTooManyRequests, "too_many_open_reports",
			"you have too many reports awaiting review")
		return
	}

	res, err := s.store.File(r.Context(), subject, reporter, body.Reason, comment)
	if err != nil {
		// The subject does not exist — the foreign keys are what caught it.
		if errors.Is(err, ErrSubjectMissing) {
			s.writeErr(w, http.StatusNotFound, "not_found", "no such subject")
			return
		}
		s.internalErr(w, r, "file report", err)
		return
	}

	status := http.StatusOK
	if res.Created {
		status = http.StatusCreated
	}
	s.writeJSON(w, status, reportView{
		ID:      res.Report.ID,
		Subject: subjectBody{Type: string(subject.Type), ID: subject.ID.String()},
		Reason:  res.Report.Reason, Comment: res.Report.Comment,
		Status: res.Report.Status, CreatedAt: res.Report.CreatedAt,
	})
}

// --- the queue --------------------------------------------------------------

type queueItemView struct {
	Subject       subjectBody `json:"subject"`
	OpenReports   int64       `json:"openReports"`
	FirstReported time.Time   `json:"firstReported"`
	LastReported  time.Time   `json:"lastReported"`
	Reasons       []string    `json:"reasons"`
	// Snapshot is the subject as it stands now — enough to triage without
	// opening the item, including whether it has already been dealt with.
	Snapshot snapshotView `json:"snapshot"`
}

type snapshotView struct {
	UserName       string `json:"userName,omitempty"`
	QuoteText      string `json:"quoteText,omitempty"`
	QuoteLang      string `json:"quoteLang,omitempty"`
	QuoteWithdrawn bool   `json:"quoteWithdrawn,omitempty"`
	RunOwnerName   string `json:"runOwnerName,omitempty"`
	RunStatus      string `json:"runStatus,omitempty"`
}

type queueResponse struct {
	Items []queueItemView `json:"items"`
}

// handleQueue serves GET /api/v1/admin/reports?type=&limit=.
func (s *ReportService) handleQueue(w http.ResponseWriter, r *http.Request) {
	var filter *SubjectType
	if raw := r.URL.Query().Get("type"); raw != "" {
		t := SubjectType(raw)
		if _, known := reasonsBySubject[t]; !known {
			s.writeErr(w, http.StatusBadRequest, "bad_request", "unknown subject type")
			return
		}
		filter = &t
	}
	limit := httpx.ParseLimit(r.URL.Query().Get("limit"), queueDefaultLimit, queueMaxLimit)

	items, err := s.store.Queue(r.Context(), filter, int32(limit))
	if err != nil {
		s.internalErr(w, r, "report queue", err)
		return
	}
	views := make([]queueItemView, len(items))
	for i := range items {
		it := &items[i]
		views[i] = queueItemView{
			Subject:       subjectBody{Type: string(it.Subject.Type), ID: it.Subject.ID.String()},
			OpenReports:   it.OpenReports,
			FirstReported: it.FirstReported,
			LastReported:  it.LastReported,
			Reasons:       it.Reasons,
			Snapshot: snapshotView{
				UserName: it.Snapshot.UserName, QuoteText: it.Snapshot.QuoteText,
				QuoteLang: it.Snapshot.QuoteLang, QuoteWithdrawn: it.Snapshot.QuoteWithdrawn,
				RunOwnerName: it.Snapshot.RunOwnerName, RunStatus: it.Snapshot.RunStatus,
			},
		}
	}
	s.writeJSON(w, http.StatusOK, queueResponse{Items: views})
}

type subjectReportView struct {
	ID             uuid.UUID  `json:"id"`
	Reason         string     `json:"reason"`
	Comment        string     `json:"comment,omitempty"`
	Status         string     `json:"status"`
	CreatedAt      time.Time  `json:"createdAt"`
	ResolvedAt     *time.Time `json:"resolvedAt,omitempty"`
	ResolutionNote string     `json:"resolutionNote,omitempty"`
	// ReporterName is visible to moderators on purpose: twelve reports from one
	// friend group and twelve from strangers are different evidence.
	ReporterName string `json:"reporterName"`
	ResolverName string `json:"resolverName,omitempty"`
}

type subjectResponse struct {
	Subject subjectBody         `json:"subject"`
	Reports []subjectReportView `json:"reports"`
}

// handleSubject serves GET /api/v1/admin/reports/{subjectType}/{subjectID}.
func (s *ReportService) handleSubject(w http.ResponseWriter, r *http.Request) {
	subject, ok := s.pathSubject(w, r)
	if !ok {
		return
	}
	reports, err := s.store.ForSubject(r.Context(), subject, subjectReportsLimit)
	if err != nil {
		s.internalErr(w, r, "reports for subject", err)
		return
	}
	views := make([]subjectReportView, len(reports))
	for i := range reports {
		rep := &reports[i]
		views[i] = subjectReportView{
			ID: rep.ID, Reason: rep.Reason, Comment: rep.Comment, Status: rep.Status,
			CreatedAt: rep.CreatedAt, ResolvedAt: rep.ResolvedAt,
			ResolutionNote: rep.ResolutionNote,
			ReporterName:   rep.ReporterName, ResolverName: rep.ResolverName,
		}
	}
	s.writeJSON(w, http.StatusOK, subjectResponse{
		Subject: subjectBody{Type: string(subject.Type), ID: subject.ID.String()},
		Reports: views,
	})
}

type resolveRequest struct {
	// Verdict is "actioned" or "dismissed". There is no "resolve without
	// saying which": the whole value of the record is which way it went.
	Verdict string `json:"verdict"`
	Note    string `json:"note"`
}

type resolveResponse struct {
	Subject subjectBody `json:"subject"`
	Status  string      `json:"status"`
	// Resolved is how many open reports this call closed. Zero is a valid,
	// successful answer: somebody else got there first.
	Resolved int64 `json:"resolved"`
}

// handleResolve serves POST /api/v1/admin/reports/{subjectType}/{subjectID}/resolve.
//
// It closes the whole group of open reports on that subject and writes NOTHING
// else. Banning the player or withdrawing the quote is a separate call to the
// surface that owns that action (docs/REPORTS.md, "Resolution records, it does
// not act") — which is what keeps a ban to one birthplace and this package
// free of a dependency on the quote domain.
func (s *ReportService) handleResolve(w http.ResponseWriter, r *http.Request) {
	subject, ok := s.pathSubject(w, r)
	if !ok {
		return
	}
	var body resolveRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&body); err != nil {
		s.writeErr(w, http.StatusBadRequest, "bad_request", "malformed body")
		return
	}
	if body.Verdict != StatusActioned && body.Verdict != StatusDismissed {
		s.writeErr(w, http.StatusBadRequest, "bad_request",
			"verdict must be actioned or dismissed")
		return
	}
	note := strings.TrimSpace(body.Note)
	if utf8.RuneCountInString(note) > maxResolutionNote {
		s.writeErr(w, http.StatusBadRequest, "bad_request", "note is too long")
		return
	}
	actor, ok := s.actor(r)
	if !ok {
		http.NotFound(w, r)
		return
	}

	n, err := s.store.Resolve(r.Context(), subject, body.Verdict, actor.ID, note)
	if err != nil {
		s.internalErr(w, r, "resolve reports", err)
		return
	}
	s.writeJSON(w, http.StatusOK, resolveResponse{
		Subject:  subjectBody{Type: string(subject.Type), ID: subject.ID.String()},
		Status:   body.Verdict,
		Resolved: n,
	})
}

// --- shared -----------------------------------------------------------------

// pathSubject parses {subjectType}/{subjectID}. Anything malformed is the admin
// tree's usual 404: a link that names no subject names nothing.
func (s *ReportService) pathSubject(w http.ResponseWriter, r *http.Request) (Subject, bool) {
	subject, err := parseSubject(chi.URLParam(r, "subjectType"), chi.URLParam(r, "subjectID"))
	if err != nil {
		http.NotFound(w, r)
		return Subject{}, false
	}
	return subject, true
}

// parseSubject validates the type and the id together — the pair is what names
// a subject, and neither half is meaningful alone.
func parseSubject(subjectType, subjectID string) (Subject, error) {
	s := Subject{Type: SubjectType(subjectType)}
	id, err := uuid.Parse(subjectID)
	if err != nil {
		return Subject{}, errors.New("moderation: subject id is not a uuid")
	}
	s.ID = id
	if err := s.Validate(); err != nil {
		return Subject{}, err
	}
	return s, nil
}

func (s *ReportService) writeJSON(w http.ResponseWriter, status int, v any) {
	if err := httpx.WriteJSON(w, status, v); err != nil {
		s.log.Error("moderation: encode response", "err", err)
	}
}

func (s *ReportService) writeErr(w http.ResponseWriter, status int, code, message string) {
	s.writeJSON(w, status, struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}{Error: code, Message: message})
}

func (s *ReportService) internalErr(w http.ResponseWriter, r *http.Request, op string, err error) {
	s.log.ErrorContext(r.Context(), "moderation: "+op, "err", err, "path", r.URL.Path)
	s.writeErr(w, http.StatusInternalServerError, "internal", "internal error")
}
