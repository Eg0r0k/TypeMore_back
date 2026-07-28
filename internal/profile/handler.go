package profile

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// Activity window bounds: the GitHub-style calendar shows at most a year plus
// the leap-day column.
const (
	activityDaysDefault = 366
	activityDaysMax     = 366
)

// dateOnly is the wire format for day buckets.
const dateOnly = "2006-01-02"

// Routes mounts the profile surface. Every route requires a session — the
// profile answers about the caller and no one else (doc.go).
func (s *Service) Routes(requireAuth func(http.Handler) http.Handler) http.Handler {
	r := chi.NewRouter()
	r.Group(func(r chi.Router) {
		r.Use(requireAuth)
		r.Get("/summary", s.handleSummary)
		r.Get("/activity", s.handleActivity)
		r.Get("/histogram", s.handleHistogram)
		r.Get("/timeseries", s.handleTimeseries)
		r.Get("/pbs", s.handlePBs)
		r.Get("/keyboard", s.handleKeyboard)
	})
	return r
}

// handleSummary serves GET /profile/summary. Day buckets are UTC throughout
// the profile: the streak's "today" is the server's UTC date.
func (s *Service) handleSummary(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUser(w, r)
	if !ok {
		return
	}
	summary, err := s.store.Summary(r.Context(), userID, time.Now().UTC())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, summary)
}

type activityDayView struct {
	Date   string `json:"date"`
	Tests  int32  `json:"tests"`
	TimeMs int64  `json:"timeMs"`
}

type activityResponse struct {
	Days []activityDayView `json:"days"`
}

// handleActivity serves GET /profile/activity?days=N (default and cap 366):
// the populated day buckets of the last N UTC days, today included.
func (s *Service) handleActivity(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUser(w, r)
	if !ok {
		return
	}
	days := activityDaysDefault
	if raw := r.URL.Query().Get("days"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 {
			s.writeError(w, r, apiErrBadRequest("days must be a positive integer"))
			return
		}
		days = min(parsed, activityDaysMax)
	}
	// The window starts at UTC midnight (days-1) days ago, so days=1 is
	// exactly "today".
	now := time.Now().UTC()
	since := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).
		AddDate(0, 0, -(days - 1))

	buckets, err := s.store.Activity(r.Context(), userID, since)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	out := activityResponse{Days: make([]activityDayView, len(buckets))}
	for i, d := range buckets {
		out.Days[i] = activityDayView{Date: d.Date.Format(dateOnly), Tests: d.Tests, TimeMs: d.TimeMs}
	}
	s.writeJSON(w, http.StatusOK, out)
}

type histogramResponse struct {
	Buckets []HistogramBucket `json:"buckets"`
}

// handleHistogram serves GET /profile/histogram: accepted-run counts per
// 10-wpm bucket, lower bounds, populated buckets only.
func (s *Service) handleHistogram(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUser(w, r)
	if !ok {
		return
	}
	buckets, err := s.store.Histogram(r.Context(), userID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, histogramResponse{Buckets: buckets})
}

type timeseriesDayView struct {
	Date         string  `json:"date"`
	TimeTypingMs int64   `json:"timeTypingMs"`
	AvgWpm       float64 `json:"avgWpm"`
	AvgAcc       float64 `json:"avgAcc"`
}

type timeseriesResponse struct {
	Days []timeseriesDayView `json:"days"`
	// WpmPerHour is the header stat "speed change per hour spent typing": the
	// least-squares slope of run wpm over cumulative hours typed inside the
	// requested range, computed server-side (docs/PROFILE.md documents the
	// method). 0 when the range holds fewer than two accepted runs.
	WpmPerHour float64 `json:"wpmPerHour"`
}

// parseDayBound parses a ?from= / ?to= value: an RFC3339 instant, or a plain
// YYYY-MM-DD date taken as UTC midnight.
func parseDayBound(raw string) (time.Time, bool) {
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t.UTC(), true
	}
	if t, err := time.Parse(dateOnly, raw); err == nil {
		return t, true
	}
	return time.Time{}, false
}

// handleTimeseries serves GET /profile/timeseries?from=&to=. The window is
// [from, to): `from` defaults to the beginning of time, `to` to "now" — and a
// date-only `to` is taken INCLUSIVELY (its whole day is inside the window),
// because that is what every range picker means by an end date.
func (s *Service) handleTimeseries(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUser(w, r)
	if !ok {
		return
	}
	from := time.Unix(0, 0).UTC()
	if raw := r.URL.Query().Get("from"); raw != "" {
		parsed, ok := parseDayBound(raw)
		if !ok {
			s.writeError(w, r, apiErrBadRequest("from must be RFC3339 or YYYY-MM-DD"))
			return
		}
		from = parsed
	}
	to := time.Now().UTC()
	if raw := r.URL.Query().Get("to"); raw != "" {
		parsed, ok := parseDayBound(raw)
		if !ok {
			s.writeError(w, r, apiErrBadRequest("to must be RFC3339 or YYYY-MM-DD"))
			return
		}
		if len(raw) == len(dateOnly) {
			parsed = parsed.AddDate(0, 0, 1) // inclusive end date
		}
		to = parsed
	}
	if !to.After(from) {
		s.writeError(w, r, apiErrBadRequest("to must be after from"))
		return
	}

	series, slope, err := s.store.Timeseries(r.Context(), userID, from, to)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	out := timeseriesResponse{Days: make([]timeseriesDayView, len(series)), WpmPerHour: slope}
	for i, d := range series {
		out.Days[i] = timeseriesDayView{
			Date:         d.Date.Format(dateOnly),
			TimeTypingMs: d.TimeTypingMs,
			AvgWpm:       d.AvgWpm,
			AvgAcc:       d.AvgAcc,
		}
	}
	s.writeJSON(w, http.StatusOK, out)
}

// pbView is one PB card: the entries row plus the bucket's parsed components,
// rendered exactly the way the boards' own index renders a bucket — a language
// board's fields absent on a quote board and vice versa.
type pbView struct {
	Bucket     string     `json:"bucket"`
	Mode       string     `json:"mode,omitempty"`
	DurationMs *int32     `json:"durationMs,omitempty"`
	WordCount  *int32     `json:"wordCount,omitempty"`
	Lang       string     `json:"lang,omitempty"`
	TextSource string     `json:"textSource,omitempty"`
	QuoteID    *uuid.UUID `json:"quoteId,omitempty"`
	// Source is the quote's attribution, snapshotted on the entry — on a quote
	// PB it is never absent (LEADERBOARDS.md).
	Source     *string         `json:"source,omitempty"`
	RunID      uuid.UUID       `json:"runId"`
	Score      int64           `json:"score"`
	Wpm        float64         `json:"wpm"`
	Raw        float64         `json:"raw"`
	Acc        float64         `json:"acc"`
	Grade      string          `json:"grade"`
	Mods       json.RawMessage `json:"mods"`
	AchievedAt time.Time       `json:"achievedAt"`
}

type pbsResponse struct {
	PBs []pbView `json:"pbs"`
}

// handlePBs serves GET /profile/pbs: the caller's best entry per bucket,
// straight out of leaderboard_entries — zero new computation.
func (s *Service) handlePBs(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUser(w, r)
	if !ok {
		return
	}
	pbs, err := s.store.PBs(r.Context(), userID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	out := pbsResponse{PBs: make([]pbView, len(pbs))}
	for i, pb := range pbs {
		view := pbView{
			Bucket:     pb.BucketKey,
			Source:     pb.QuoteSource,
			RunID:      pb.RunID,
			Score:      pb.Score,
			Wpm:        pb.Wpm,
			Raw:        pb.Raw,
			Acc:        pb.Acc,
			Grade:      pb.Grade,
			Mods:       pb.Mods,
			AchievedAt: pb.AchievedAt,
		}
		if info, ok := s.bucket(pb.BucketKey); ok {
			view.Mode = info.Mode
			view.DurationMs = info.DurationMs
			view.WordCount = info.WordCount
			view.Lang = info.Lang
			view.TextSource = info.TextSource
			view.QuoteID = info.QuoteID
		}
		out.PBs[i] = view
	}
	s.writeJSON(w, http.StatusOK, out)
}

// keyboardKeyView is one heatmap key: derived ratios, not raw sums — the UI
// renders errorRate and avgIntervalMs, and shipping the sums would push the
// division into every client.
type keyboardKeyView struct {
	KeyID string `json:"keyId"`
	Count int64  `json:"count"`
	// ErrorRate in [0, 1]; 0 when the key was never pressed.
	ErrorRate float64 `json:"errorRate"`
	// AvgIntervalMs is the mean inter-key interval ENDING on this key; 0 when
	// no interval was ever counted for it.
	AvgIntervalMs float64 `json:"avgIntervalMs"`
	// Intervals is the observation count behind AvgIntervalMs, so the UI can
	// refuse to color a key from three presses (the low-data rule).
	Intervals int64 `json:"intervals"`
}

type keyboardResponse struct {
	// Layout is the heatmap's default: the layout of the user's most-played
	// dictionary language ("qwerty" for a fresh account).
	Layout string            `json:"layout"`
	Keys   []keyboardKeyView `json:"keys"`
}

// handleKeyboard serves GET /profile/keyboard: the projection's aggregates,
// own profile only — and deliberately so even after public profiles arrive:
// per-key timing is effectively biometric (docs/PROFILE.md, "Privacy").
func (s *Service) handleKeyboard(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.currentUser(w, r)
	if !ok {
		return
	}
	keys, lang, err := s.store.Keyboard(r.Context(), userID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	out := keyboardResponse{Layout: s.layoutFor(lang), Keys: make([]keyboardKeyView, len(keys))}
	for i, k := range keys {
		view := keyboardKeyView{KeyID: k.KeyID, Count: k.Presses, Intervals: k.IntervalCount}
		if k.Presses > 0 {
			view.ErrorRate = float64(k.Errors) / float64(k.Presses)
		}
		if k.IntervalCount > 0 {
			view.AvgIntervalMs = k.IntervalSumMs / float64(k.IntervalCount)
		}
		out.Keys[i] = view
	}
	s.writeJSON(w, http.StatusOK, out)
}
