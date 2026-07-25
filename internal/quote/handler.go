package quote

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// Pagination bounds for a browse page.
const (
	defaultLimit = 50
	maxLimit     = 200
)

// Routes returns the quote router, mounted at /api/v1/quotes.
//
// The whole subtree is PUBLIC and unauthenticated. Quotes are static published
// assets — the same argument the dictionaries are served under: a guest picking
// a text to type has no account yet, and gating the corpus would gate the game.
func (s *Service) Routes() http.Handler {
	r := chi.NewRouter()
	r.Get("/", s.handleList)
	// Registered before "/{id}": chi matches static segments first regardless of
	// order, but stating the intent here is cheaper than re-deriving it.
	r.Get("/random", s.handleRandom)
	r.Get("/{id}", s.handleByID)
	return r
}

// metaView is one row of the browse index.
//
// It carries NO `text` field, and that is the point of the endpoint rather than
// an omission: /quotes is walkable end to end with a cursor, so putting bodies
// on it would turn "list the corpus" into "download the corpus in one pass".
// Text is reachable one quote at a time, through /quotes/random (which the
// caller does not choose) or /quotes/{id} (which needs the id first).
type metaView struct {
	ID         uuid.UUID `json:"id"`
	Lang       string    `json:"lang"`
	UpstreamID int32     `json:"upstreamId"`
	Source     string    `json:"source"`
	Length     int32     `json:"length"`
	LenGroup   string    `json:"lenGroup"`
	TextHash   string    `json:"textHash"`
}

func toMetaView(m Meta) metaView {
	return metaView{
		ID: m.ID, Lang: m.Lang, UpstreamID: m.UpstreamID, Source: m.Source,
		Length: m.Length, LenGroup: m.LenGroup.String(), TextHash: m.TextHash,
	}
}

// quoteView is a full quote: the metadata above plus the bytes to type.
// `superseded` rides along so a client resolving a stored run can tell that the
// text it just fetched is a retired revision without a second lookup.
type quoteView struct {
	metaView
	Text       string `json:"text"`
	Superseded bool   `json:"superseded"`
}

func toQuoteView(q Quote) quoteView {
	return quoteView{metaView: toMetaView(q.Meta), Text: q.Text, Superseded: q.Superseded}
}

type listResponse struct {
	Quotes     []metaView `json:"quotes"`
	NextCursor string     `json:"nextCursor,omitempty"`
}

// handleList returns one page of quote METADATA, keyset-paginated.
//
// Unlike the leaderboard page there is no rank to count: the ordering here is a
// stable property of the row (lang, len_group, id), not of the row's position,
// so a page never has to ask how many rows precede it — and a concurrent insert
// before the cursor cannot shift anything the walk has already seen.
func (s *Service) handleList(w http.ResponseWriter, r *http.Request) {
	filter, ok := s.filterParam(w, r)
	if !ok {
		return
	}

	limit := parseLimit(r.URL.Query().Get("limit"))
	var after *Cursor
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		cur, err := decodeCursor(raw)
		if err != nil {
			s.writeError(w, r, apiErrBadCursor)
			return
		}
		after = &cur
	}

	// One extra row tells us whether another page exists, without a second
	// query or an empty trailing page.
	rows, err := s.store.Page(r.Context(), filter, after, int32(limit+1))
	if err != nil {
		s.writeError(w, r, err)
		return
	}

	next := ""
	if len(rows) > limit {
		last := rows[limit-1]
		rows = rows[:limit]
		next = encodeCursor(Cursor{Lang: last.Lang, Group: last.LenGroup, ID: last.ID})
	}

	views := make([]metaView, len(rows))
	for i := range rows {
		views[i] = toMetaView(rows[i])
	}
	s.writeJSON(w, http.StatusOK, listResponse{Quotes: views, NextCursor: next})
}

// handleRandom draws one quote, text included. Starting a quote run is exactly
// this call, which is why the draw happens in SQL: handing the client a list to
// pick from would be the corpus scrape /quotes refuses to be.
//
// Superseded revisions are never drawn — nobody should start a run on a text
// that has already been replaced.
func (s *Service) handleRandom(w http.ResponseWriter, r *http.Request) {
	filter, ok := s.filterParam(w, r)
	if !ok {
		return
	}
	q, err := s.store.Random(r.Context(), filter)
	if errors.Is(err, ErrNotFound) {
		s.writeError(w, r, apiErrNotFound)
		return
	}
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, toQuoteView(q))
}

// handleByID resolves one quote by id, text included, SUPERSEDED ONES INCLUDED.
// This is what a replay and a per-quote board resolve their text through: a
// retired quote that stopped being fetchable would make every run played on it
// unwatchable, which is the exact failure a frozen dict_hash exists to prevent.
func (s *Service) handleByID(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		s.writeError(w, r, apiErrNotFound)
		return
	}
	q, err := s.store.ByID(r.Context(), id)
	if errors.Is(err, ErrNotFound) {
		s.writeError(w, r, apiErrNotFound)
		return
	}
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, toQuoteView(q))
}

// filterParam parses ?lang= and ?group=. An absent filter matches everything; a
// group name we do not know is a 400, never a silently dropped filter.
func (s *Service) filterParam(w http.ResponseWriter, r *http.Request) (Filter, bool) {
	f := Filter{Lang: r.URL.Query().Get("lang")}
	if raw := r.URL.Query().Get("group"); raw != "" {
		g, err := ParseLenGroup(raw)
		if err != nil {
			s.writeError(w, r, apiErrBadGroup)
			return Filter{}, false
		}
		f.Group = &g
	}
	return f, true
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

// encodeCursor packs a browse position into an opaque base64url token. Language
// ids never contain a colon (they are the dictionary catalogue's own ids), so
// the three fields split unambiguously.
func encodeCursor(c Cursor) string {
	raw := fmt.Sprintf("%s:%d:%s", c.Lang, int16(c.Group), c.ID)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// decodeCursor reverses encodeCursor, rejecting anything malformed. The group
// is range-checked here rather than trusted into SQL: the token is client input
// like any other.
func decodeCursor(token string) (Cursor, error) {
	b, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return Cursor{}, err
	}
	parts := strings.Split(string(b), ":")
	if len(parts) != 3 {
		return Cursor{}, errors.New("quote: malformed cursor")
	}
	group, err := strconv.ParseInt(parts[1], 10, 16)
	if err != nil {
		return Cursor{}, err
	}
	if g := LenGroup(group); !g.Valid() {
		return Cursor{}, fmt.Errorf("quote: cursor length group %d out of range", group)
	}
	id, err := uuid.Parse(parts[2])
	if err != nil {
		return Cursor{}, err
	}
	return Cursor{Lang: parts[0], Group: LenGroup(group), ID: id}, nil
}
