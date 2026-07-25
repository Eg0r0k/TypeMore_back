package leaderboard

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// Pagination bounds for a board page.
const (
	defaultLimit = 50
	maxLimit     = 100
)

// Routes returns the leaderboard router, mounted at /api/v1/leaderboards.
//
// The whole subtree is PUBLIC: a leaderboard nobody can read without an account
// is a leaderboard nobody links to. `/me` is the one route that needs a session,
// and it enforces that itself rather than dragging the group behind middleware
// the other two must not have.
func (s *Service) Routes() http.Handler {
	r := chi.NewRouter()
	r.Get("/", s.handleBuckets)
	r.Get("/{bucket}", s.handlePage)
	r.Get("/{bucket}/me", s.handleMe)
	return r
}

// bucketView is one board in the index. The dimension is rendered under the name
// the mode gives it, so a client never has to know that "the number" means
// milliseconds here and words there.
type bucketView struct {
	Bucket     string `json:"bucket"`
	Mode       string `json:"mode"`
	DurationMs *int32 `json:"durationMs,omitempty"`
	WordCount  *int32 `json:"wordCount,omitempty"`
	Lang       string `json:"lang"`
	TextSource string `json:"textSource"`
	Entries    int64  `json:"entries"`
}

func toBucketView(b Bucket, entries int64) bucketView {
	v := bucketView{
		Bucket: b.Key(), Mode: b.Mode, Lang: b.Lang,
		TextSource: b.TextSource, Entries: entries,
	}
	dim := b.Dimension
	if b.Mode == ModeTime {
		v.DurationMs = &dim
	} else {
		v.WordCount = &dim
	}
	return v
}

type bucketsResponse struct {
	Buckets []bucketView `json:"buckets"`
}

// handleBuckets lists the boards that currently hold at least one visible entry.
// Empty buckets are absent rather than enumerated: which shapes are ranked is a
// property of the schema, and a board with nothing in it is not news.
func (s *Service) handleBuckets(w http.ResponseWriter, r *http.Request) {
	counts, err := s.store.Buckets(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	views := make([]bucketView, len(counts))
	for i := range counts {
		views[i] = toBucketView(counts[i].Bucket, counts[i].Entries)
	}
	s.writeJSON(w, http.StatusOK, bucketsResponse{Buckets: views})
}

// entryView is one ranked row.
type entryView struct {
	Rank        int64           `json:"rank"`
	UserID      uuid.UUID       `json:"userId"`
	DisplayName string          `json:"displayName"`
	Score       int64           `json:"score"`
	WPM         float64         `json:"wpm"`
	Raw         float64         `json:"raw"`
	Acc         float64         `json:"acc"`
	Grade       string          `json:"grade"`
	Mods        json.RawMessage `json:"mods"`
	RunID       uuid.UUID       `json:"runId"`
	AchievedAt  time.Time       `json:"achievedAt"`
}

func toEntryView(e Entry) entryView {
	return entryView{
		Rank: e.Rank, UserID: e.UserID, DisplayName: e.DisplayName,
		Score: e.Score, WPM: e.WPM, Raw: e.Raw, Acc: e.Acc, Grade: e.Grade,
		Mods: e.Mods, RunID: e.RunID, AchievedAt: e.AchievedAt,
	}
}

type pageResponse struct {
	Bucket     string      `json:"bucket"`
	Entries    []entryView `json:"entries"`
	NextCursor string      `json:"nextCursor,omitempty"`
}

// handlePage returns one page of a bucket's ranking, keyset-paginated.
//
// The rank of the first row on a continuation page is COUNTED rather than
// carried in the cursor: a rank that was true when the token was minted is a
// lie by the time anyone reads it, and the count is one indexed range scan.
func (s *Service) handlePage(w http.ResponseWriter, r *http.Request) {
	bucket, ok := s.bucketParam(w, r)
	if !ok {
		return
	}

	limit := parseLimit(r.URL.Query().Get("limit"))
	var after *Cursor
	firstRank := int64(1)
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		cur, err := decodeCursor(raw)
		if err != nil {
			s.writeError(w, r, apiErrBadCursor)
			return
		}
		after = &cur
		above, err := s.store.RankAbove(r.Context(), bucket, cur)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		// The cursor row itself is above everything on this page.
		firstRank = above + 2
	}

	// Fetch one extra row to learn whether another page exists without a second
	// query or an empty trailing page.
	rows, err := s.store.Page(r.Context(), bucket, after, int32(limit+1))
	if err != nil {
		s.writeError(w, r, err)
		return
	}

	next := ""
	if len(rows) > limit {
		last := rows[limit-1]
		rows = rows[:limit]
		next = encodeCursor(Cursor{
			Score: last.Score, AchievedAt: last.AchievedAt, UserID: last.UserID,
		})
	}

	views := make([]entryView, len(rows))
	for i := range rows {
		rows[i].Rank = firstRank + int64(i)
		views[i] = toEntryView(rows[i])
	}
	s.writeJSON(w, http.StatusOK, pageResponse{
		Bucket: bucket.Key(), Entries: views, NextCursor: next,
	})
}

type meResponse struct {
	Bucket string    `json:"bucket"`
	Entry  entryView `json:"entry"`
}

// handleMe returns the caller's own rank and entry in a bucket, or 204 when they
// hold no visible slot there. It is the one route in this domain that needs a
// session, so it checks for one itself.
func (s *Service) handleMe(w http.ResponseWriter, r *http.Request) {
	bucket, ok := s.bucketParam(w, r)
	if !ok {
		return
	}
	userID, ok := s.userID(r.Context())
	if !ok {
		s.writeError(w, r, apiErrUnauthorized)
		return
	}

	entry, err := s.store.EntryFor(r.Context(), bucket, userID)
	if errors.Is(err, ErrNoEntry) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, meResponse{Bucket: bucket.Key(), Entry: toEntryView(entry)})
}

// bucketParam parses the {bucket} path parameter, answering 404 for anything
// that could not name a board.
func (s *Service) bucketParam(w http.ResponseWriter, r *http.Request) (Bucket, bool) {
	b, err := ParseBucketKey(chi.URLParam(r, "bucket"))
	if err != nil {
		s.writeError(w, r, apiErrUnknownBucket)
		return Bucket{}, false
	}
	return b, true
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
	raw := fmt.Sprintf("%d:%d:%s", c.Score, c.AchievedAt.UTC().UnixNano(), c.UserID)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// decodeCursor reverses encodeCursor, rejecting anything malformed.
func decodeCursor(token string) (Cursor, error) {
	b, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return Cursor{}, err
	}
	parts := strings.Split(string(b), ":")
	if len(parts) != 3 {
		return Cursor{}, errors.New("leaderboard: malformed cursor")
	}
	score, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return Cursor{}, err
	}
	nanos, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return Cursor{}, err
	}
	id, err := uuid.Parse(parts[2])
	if err != nil {
		return Cursor{}, err
	}
	return Cursor{Score: score, AchievedAt: time.Unix(0, nanos).UTC(), UserID: id}, nil
}
