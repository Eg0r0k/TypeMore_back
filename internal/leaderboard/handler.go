package leaderboard

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
	r.With(s.rateLimitIndex).Get("/", s.handleBuckets)
	r.Get("/{bucket}", s.handlePage)
	r.Get("/{bucket}/me", s.handleMe)
	return r
}

// rateLimitIndex is the per-IP bucket on the board index — see the field
// comment on Service.indexLimiter for why that route and no other. A nil
// limiter disables the gate entirely rather than refusing everything.
func (s *Service) rateLimitIndex(next http.Handler) http.Handler {
	if s.indexLimiter == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.indexLimiter.Allow(httpx.ClientIP(r)) {
			s.writeError(w, r, apiErrRateLimited)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// bucketView is one board in the index. A language board renders its dimension
// under the name its mode gives it, so a client never has to know that "the
// number" means milliseconds here and words there; a quote board renders its
// quote id and NOTHING else, because it has nothing else — the fields a
// language board fills are absent rather than empty, so a client cannot read a
// mode off a board that has none.
type bucketView struct {
	Bucket     string     `json:"bucket"`
	QuoteID    *uuid.UUID `json:"quoteId,omitempty"`
	Mode       string     `json:"mode,omitempty"`
	DurationMs *int32     `json:"durationMs,omitempty"`
	WordCount  *int32     `json:"wordCount,omitempty"`
	Lang       string     `json:"lang,omitempty"`
	TextSource string     `json:"textSource,omitempty"`
	Entries    int64      `json:"entries"`
}

func toBucketView(b Bucket, entries int64) bucketView {
	v := bucketView{Bucket: b.Key(), Entries: entries}
	if b.IsQuote() {
		id := b.QuoteID
		v.QuoteID = &id
		return v
	}
	v.Mode, v.Lang, v.TextSource = b.Mode, b.Lang, b.TextSource
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
// Empty boards are absent rather than enumerated: a board with nothing in it is
// not news, and for quotes it is also the only thing keeping this response
// finite — there are 9 881 of them (docs/LEADERBOARDS.md, "The board index").
// A withdrawn quote's board is absent from this list and from this list ONLY
// (docs/REPORTS.md): the board still answers on its own URL, its entries keep
// their ranks, and the runs behind them stay replayable. What a withdrawal
// takes away is the quote being OFFERED — in browsing, in random selection, and
// here — never a result somebody already earned.
func (s *Service) handleBuckets(w http.ResponseWriter, r *http.Request) {
	counts, err := s.store.Buckets(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	withdrawn, err := s.withdrawn(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	views := make([]bucketView, 0, len(counts))
	for i := range counts {
		b := counts[i].Bucket
		if b.IsQuote() {
			if _, gone := withdrawn[b.QuoteID]; gone {
				continue
			}
		}
		views = append(views, toBucketView(b, counts[i].Entries))
	}
	// A minute of shared caching, and it is the limiter's other half: the bucket
	// bounds one caller, this bounds how often ALL of them can reach the two
	// aggregations behind this route. `public` because the answer carries
	// nothing reader-specific — the index is the same bytes for everyone, signed
	// in or not — so a CDN or a browser cache may serve it to anybody.
	//
	// A minute is chosen against what the response actually says: a board
	// appearing in the index at all takes a first accepted run on it, and a
	// withdrawal is a moderator's deliberate act. Neither is something a reader
	// is waiting on to the second, and the per-board reads are uncached.
	w.Header().Set("Cache-Control", "public, max-age=60")
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
	// Source is the quote's attribution and appears on quote boards only. It is
	// not optional there: a quote is someone's words, and a board that shows the
	// text without saying whose is not something to ship.
	Source     string    `json:"source,omitempty"`
	RunID      uuid.UUID `json:"runId"`
	AchievedAt time.Time `json:"achievedAt"`
}

func toEntryView(e Entry) entryView {
	return entryView{
		Rank: e.Rank, UserID: e.UserID, DisplayName: e.DisplayName,
		Score: e.Score, WPM: e.WPM, Raw: e.Raw, Acc: e.Acc, Grade: e.Grade,
		Mods: e.Mods, Source: e.Source, RunID: e.RunID, AchievedAt: e.AchievedAt,
	}
}

type pageResponse struct {
	Bucket  string      `json:"bucket"`
	Entries []entryView `json:"entries"`
	// PrevCursor continues UPWARD (rows outranking the first one here), via
	// `?before=`. Present only when the first row is not rank 1.
	PrevCursor string `json:"prevCursor,omitempty"`
	NextCursor string `json:"nextCursor,omitempty"`
}

// handlePage returns one page of a bucket's ranking, keyset-paginated.
//
// Three shapes of ask, decided by the query string:
//
//	(nothing)     page one, from rank 1
//	?cursor=      the downward continuation
//	?before=      the upward continuation (rows outranking the position)
//	?around=me    the window centred on the caller's own row
//
// The rank of the first row on a continuation page is COUNTED rather than
// carried in the cursor: a rank that was true when the token was minted is a
// lie by the time anyone reads it, and the count is one indexed range scan.
func (s *Service) handlePage(w http.ResponseWriter, r *http.Request) {
	bucket, ok := s.bucketParam(w, r)
	if !ok {
		return
	}

	limit := httpx.ParseLimit(r.URL.Query().Get("limit"), defaultLimit, maxLimit)

	// The three asks name mutually exclusive positions; a request combining
	// them is asking for two different pages at once.
	asks := 0
	for _, p := range []string{"cursor", "before", "around"} {
		if r.URL.Query().Get(p) != "" {
			asks++
		}
	}
	if asks > 1 {
		s.writeError(w, r, apiErrConflictingAsks)
		return
	}

	if r.URL.Query().Get("around") != "" {
		s.handleAround(w, r, bucket, limit)
		return
	}
	if raw := r.URL.Query().Get("before"); raw != "" {
		s.handleBefore(w, r, bucket, raw, limit)
		return
	}

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
		rows = rows[:limit]
		next = cursorOf(rows[limit-1])
	}

	s.writeJSON(w, http.StatusOK, s.pageView(bucket, rows, firstRank, next))
}

// handleAround serves `?around=me`: the window centred on the caller's own
// row. 401 without a session; 204 when the caller holds no visible slot here —
// the same two answers `/me` gives, because it is the same question with
// neighbours attached.
//
// The window is the caller's row, up to half the limit above it (fewer only at
// the top of the board — the spare capacity goes below), and the remainder
// below. Both sides come off leaderboard_sort_idx as start-condition scans:
// the rows above are the same seek as the downward continuation, run backward.
func (s *Service) handleAround(w http.ResponseWriter, r *http.Request, bucket Bucket, limit int) {
	if r.URL.Query().Get("around") != "me" {
		// The only window anyone has asked for is "me"; an unknown anchor is a
		// bad request, not an empty page.
		s.writeError(w, r, apiErrBadAround)
		return
	}
	userID, ok := s.userID(r.Context())
	if !ok {
		s.writeError(w, r, apiErrUnauthorized)
		return
	}

	me, err := s.store.EntryFor(r.Context(), bucket, userID)
	if errors.Is(err, ErrNoEntry) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err != nil {
		s.writeError(w, r, err)
		return
	}

	// Half above, half below — and what one side cannot fill (the caller near
	// rank 1, or near the bottom), the other side absorbs, so the window stays
	// `limit` rows whenever the board has that many. The rank bounds the rows
	// above exactly, so no probe is needed there.
	wantAbove := (limit - 1) / 2
	if above := int(me.Rank - 1); wantAbove > above {
		wantAbove = above
	}
	wantBelow := limit - 1 - wantAbove
	below, err := s.store.Page(r.Context(), bucket, ptr(cursorFor(me)), int32(wantBelow+1))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	next := ""
	if len(below) > wantBelow {
		below = below[:wantBelow]
		if wantBelow > 0 {
			next = cursorOf(below[wantBelow-1])
		} else {
			next = cursorOf(me)
		}
	} else if spare := min(limit-1-len(below), int(me.Rank-1)); spare > wantAbove {
		// The board ran out below; spend the spare capacity above.
		wantAbove = spare
	}

	above := []Entry{}
	if wantAbove > 0 {
		above, err = s.store.PageBefore(r.Context(), bucket, cursorFor(me), int32(wantAbove))
		if err != nil {
			s.writeError(w, r, err)
			return
		}
	}

	rows := make([]Entry, 0, len(above)+1+len(below))
	rows = append(rows, above...)
	rows = append(rows, me)
	rows = append(rows, below...)

	firstRank := me.Rank - int64(len(above))
	prev := ""
	if firstRank > 1 {
		prev = cursorOf(rows[0])
	}

	view := s.pageView(bucket, rows, firstRank, next)
	view.PrevCursor = prev
	s.writeJSON(w, http.StatusOK, view)
}

// handleBefore serves `?before=`: the rows strictly outranking a position,
// nearest last, so a client prepends the page verbatim.
func (s *Service) handleBefore(w http.ResponseWriter, r *http.Request, bucket Bucket, raw string, limit int) {
	cur, err := decodeCursor(raw)
	if err != nil {
		s.writeError(w, r, apiErrBadCursor)
		return
	}

	rows, err := s.store.PageBefore(r.Context(), bucket, cur, int32(limit))
	if err != nil {
		s.writeError(w, r, err)
		return
	}

	// The rank of the position, counted fresh; everything returned sits in the
	// len(rows) slots directly above it.
	above, err := s.store.RankAbove(r.Context(), bucket, cur)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	firstRank := above + 1 - int64(len(rows))

	prev := ""
	if len(rows) > 0 && firstRank > 1 {
		prev = cursorOf(rows[0])
	}

	view := s.pageView(bucket, rows, firstRank, "")
	view.PrevCursor = prev
	s.writeJSON(w, http.StatusOK, view)
}

// pageView ranks the rows from firstRank on and wraps them in the wire shape.
func (s *Service) pageView(bucket Bucket, rows []Entry, firstRank int64, next string) pageResponse {
	views := make([]entryView, len(rows))
	for i := range rows {
		rows[i].Rank = firstRank + int64(i)
		views[i] = toEntryView(rows[i])
	}
	return pageResponse{Bucket: bucket.Key(), Entries: views, NextCursor: next}
}

// cursorFor is the keyset position OF an entry; cursorOf is its wire token.
func cursorFor(e Entry) Cursor {
	return Cursor{Score: e.Score, AchievedAt: e.AchievedAt, UserID: e.UserID}
}

func cursorOf(e Entry) string { return encodeCursor(cursorFor(e)) }

func ptr[T any](v T) *T { return &v }

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

// encodeCursor packs a keyset position into an opaque base64url token.
// Nanosecond precision is exact for Postgres timestamptz (microseconds), so the
// round-trip reproduces the stored instant for the equality tie-break.
func encodeCursor(c Cursor) string {
	return httpx.EncodeCursor(
		strconv.FormatInt(c.Score, 10),
		strconv.FormatInt(c.AchievedAt.UTC().UnixNano(), 10),
		c.UserID.String(),
	)
}

// decodeCursor reverses encodeCursor, rejecting anything malformed.
func decodeCursor(token string) (Cursor, error) {
	parts, err := httpx.DecodeCursor(token, 3)
	if err != nil {
		return Cursor{}, err
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
