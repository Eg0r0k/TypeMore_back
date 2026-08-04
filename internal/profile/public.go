package profile

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/typemore/typemore-server/internal/platform/httpx"
)

// The public profile surface: GET /api/v1/users/{name}/… (docs/PROFILE.md,
// "Public profiles"). Privacy is enforced HERE, on the server — every data
// route refuses a closed profile with 403 profile_closed before any query
// runs; a frontend hiding sections over an API that still answers would be
// privacy theatre.
//
// What this surface deliberately does NOT do:
//
//   - It never replaces the owner's session-scoped /api/v1/profile/* routes.
//     The owner reads their own profile through those exactly as before — the
//     public paths merely also answer the owner (resolved via OptionalAuth)
//     so a closed profile's owner can preview their own public page.
//   - It does not touch the leaderboards. A closed profile stays ranked under
//     its name, and its board runs stay watchable: what closes is the
//     AGGREGATED history on this page, not a result the player put into a
//     public ranking (the extended predicate on the public replay pair in
//     internal/runs/queries.sql is that boundary's other half).

// Pagination bounds for the public run history — the same as the owner's feed.
const (
	publicRunsDefaultLimit = 20
	publicRunsMaxLimit     = 100
)

// Bounds for the player search.
//
// searchMinQueryLen is load-bearing twice over. It keeps an anonymous endpoint
// from asking for "every account", and — the reason it is 3 and not 2 — a
// trigram index cannot serve a pattern shorter than three characters, so a
// shorter q would silently become a sequential scan over users. It forbids no
// real search: a display_name shorter than 3 characters cannot exist (the
// 00001 CHECK), so no shorter string was ever going to be a whole name either.
//
// searchMaxQueryLen is the same CHECK's upper bound: a longer q cannot be
// contained in any name, so answering it with 400 is more honest than running
// a scan guaranteed to return nothing.
const (
	searchMinQueryLen  = 3
	searchMaxQueryLen  = 20
	searchDefaultLimit = 20
	searchMaxLimit     = 50
)

// PublicRoutes mounts the public profile surface, intended for /api/v1/users
// behind OptionalAuth: a session is never required, but when one is present it
// is what lets the owner through their own closed profile.
func (s *Service) PublicRoutes() http.Handler {
	r := chi.NewRouter()
	// Search sits on the SUBTREE ROOT — /users?q=… — rather than at
	// /users/search. chi resolves a static segment ahead of a parameter, so a
	// /search route would permanently shadow the profile of anyone named
	// "search", and that is a legal display_name under the 00001 CHECK. A
	// query parameter cannot collide with a name at all.
	r.With(s.rateLimitSearch).Get("/", s.handleSearch)
	r.Get("/{name}", s.handlePublicHeader)
	r.Get("/{name}/summary", s.publicData(s.serveSummary))
	r.Get("/{name}/activity", s.publicData(s.serveActivity))
	r.Get("/{name}/histogram", s.publicData(s.serveHistogram))
	r.Get("/{name}/timeseries", s.publicData(s.serveTimeseries))
	r.Get("/{name}/pbs", s.handlePublicPBs)
	r.Get("/{name}/runs", s.handlePublicRuns)
	r.Get("/{name}/portrait", s.handlePublicPortrait)
	return r
}

// rateLimitSearch is the per-IP bucket on the search route. Keyed by IP rather
// than by session because the route is anonymous by design: requiring a login
// to look up a player would be a heavier answer to abuse than the abuse.
func (s *Service) rateLimitSearch(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.searchLimiter.Allow(httpx.ClientIP(r)) {
			s.writeError(w, r, apiErrRateLimited)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// searchHit is one search result: the header's identity fields, verbatim, so a
// client renders a hit and a profile header through the same shape.
type searchHit struct {
	Name   string    `json:"name"`
	Joined time.Time `json:"joined"`
	Public bool      `json:"public"`
}

// searchResponse wraps the hits in an object rather than answering with a bare
// JSON array: every other list on this API is an object with a named field, and
// the wrapper is what lets a "did your query get truncated" signal be added
// later without breaking a client that already parses this.
type searchResponse struct {
	Users []searchHit `json:"users"`
}

// handleSearch serves GET /users?q=&limit= — find a player by (part of) their
// name. There is no cursor on purpose: a search box is refined, not paged, and
// a keyset over this ranking would need a total order the ranking does not
// have. A client that wants more asks a more specific question.
func (s *Service) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	// Runes, not bytes: q is arbitrary client input even though a name is not,
	// so a multi-byte string must be rejected for its length rather than for
	// its encoding.
	if n := utf8.RuneCountInString(q); n < searchMinQueryLen || n > searchMaxQueryLen {
		s.writeError(w, r, apiErrBadRequest(fmt.Sprintf(
			"q must be between %d and %d characters", searchMinQueryLen, searchMaxQueryLen)))
		return
	}

	limit := httpx.ParseLimit(r.URL.Query().Get("limit"), searchDefaultLimit, searchMaxLimit)
	rows, err := s.store.SearchUsers(r.Context(), q, int32(limit))
	if err != nil {
		s.writeError(w, r, err)
		return
	}

	// No hits is an empty list and a 200, never a 404: the question "who is
	// called something like this" was answered, and the answer was "nobody".
	hits := make([]searchHit, len(rows))
	for i := range rows {
		hits[i] = searchHit{
			Name: rows[i].DisplayName, Joined: rows[i].Joined, Public: rows[i].ProfilePublic,
		}
	}
	s.writeJSON(w, http.StatusOK, searchResponse{Users: hits})
}

// resolvePublicUser turns the {name} path parameter into the account row, or
// writes the 404. The lookup is case-insensitive (citext), matching how the
// name is unique in the first place.
func (s *Service) resolvePublicUser(w http.ResponseWriter, r *http.Request) (PublicUser, bool) {
	user, err := s.store.PublicUser(r.Context(), chi.URLParam(r, "name"))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			s.writeError(w, r, apiErrNotFound)
		} else {
			s.writeError(w, r, err)
		}
		return PublicUser{}, false
	}
	return user, true
}

// isOwner reports whether the (optional) session belongs to the profile's
// owner. Anonymous callers are never the owner.
func (s *Service) isOwner(r *http.Request, user PublicUser) bool {
	callerID, ok := s.userID(r.Context())
	return ok && callerID == user.ID
}

// headerView is the one payload a CLOSED profile still answers with: the
// identity a leaderboard already shows, plus the fact of being closed. The
// page exists for every registered name — closed is a state, not a 404.
type headerView struct {
	Name string `json:"name"`
	// Joined is on the header (not just the summary) so the closed-profile
	// page can still render its identity block; it discloses nothing the open
	// summary would not.
	Joined time.Time `json:"joined"`
	Public bool      `json:"public"`

	// The identity half (00029), present only on an OPEN profile and only when
	// its owner filled it in. omitempty throughout: an untouched profile serves
	// the byte-identical header it served before these existed.
	Bio      *string `json:"bio,omitempty"`
	Keyboard *string `json:"keyboard,omitempty"`
	// Links carry the HANDLE, never a URL: the client owns the prefix list, so
	// the set of hosts this product links to is a list in the source rather
	// than data anybody can write (internal/profile/links.go).
	Links []publicLinkView `json:"links,omitempty"`
	// Badges is the showcase, in its owner's order — codes only. What a badge
	// LOOKS like (name, description, icon, colour) lives in the frontend
	// registry; this server has never had an opinion about any of it.
	Badges []string `json:"badges,omitempty"`
}

// publicLinkView is one social link on the public header.
type publicLinkView struct {
	Kind   string `json:"kind"`
	Handle string `json:"handle"`
}

// handlePublicHeader serves GET /users/{name}: 200 for every existing name,
// 404 for an unknown one.
//
// The IDENTITY half (bio, keyboard, links, badge showcase) rides along only
// when the profile is open, or for the owner previewing their own page. It is
// gated even though it is small and self-authored, because the closed state
// means "this page does not answer strangers" — and a bio and a set of social
// handles are exactly the kind of thing somebody closes a profile to withhold.
// A closed header therefore still answers, and still answers with a name, a
// join date and the closed flag, which is what it has always carried.
func (s *Service) handlePublicHeader(w http.ResponseWriter, r *http.Request) {
	user, ok := s.resolvePublicUser(w, r)
	if !ok {
		return
	}
	// The owner sees their real setting (that is the preview's point); for
	// anyone else `public` IS the answer to "may I ask for more".
	view := headerView{
		Name: user.DisplayName, Joined: user.Joined, Public: user.ProfilePublic,
	}
	if user.ProfilePublic || s.isOwner(r, user) {
		if err := s.decorateHeader(r, user.ID, &view); err != nil {
			s.writeError(w, r, err)
			return
		}
	}
	s.writeJSON(w, http.StatusOK, view)
}

// decorateHeader fills the identity half of an OPEN profile's header.
//
// Every one of these is omitempty: a profile with no bio, no links and no
// badges answers exactly the bytes it answered before this feature existed,
// which is what keeps the public payload an allowlist that grows only when
// somebody actually filled a field in.
func (s *Service) decorateHeader(r *http.Request, userID uuid.UUID, view *headerView) error {
	identity, err := s.store.Identity(r.Context(), userID)
	if err != nil {
		return err
	}
	view.Bio, view.Keyboard = identity.Bio, identity.Keyboard

	links, err := s.store.Links(r.Context(), userID)
	if err != nil {
		return err
	}
	for _, l := range links {
		view.Links = append(view.Links, publicLinkView{Kind: l.Kind, Handle: l.Handle})
	}

	// The showcase read carries the live predicate in SQL (`revoked_at IS
	// NULL`), so a revoked badge leaves a public page the moment it is revoked,
	// with nothing to rebuild and no display_order to trust.
	showcase, err := s.store.ShowcaseBadges(r.Context(), userID)
	if err != nil {
		return err
	}
	for _, b := range showcase {
		view.Badges = append(view.Badges, b.Code)
	}
	return nil
}

// publicData wraps a shared serve func with the public surface's gate:
// resolve the name, then let through the owner always (they see their own
// profile whole, whatever the switch says) and strangers only while the
// profile is open.
func (s *Service) publicData(serve func(http.ResponseWriter, *http.Request, uuid.UUID)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, ok := s.resolvePublicUser(w, r)
		if !ok {
			return
		}
		if !user.ProfilePublic && !s.isOwner(r, user) {
			s.writeError(w, r, apiErrProfileClosed)
			return
		}
		serve(w, r, user.ID)
	}
}

// handlePublicPBs serves GET /users/{name}/pbs — the profile gate, then the
// BAN-FILTERED entries read (the session route reads raw entries; a public
// surface must not show what every board hides).
func (s *Service) handlePublicPBs(w http.ResponseWriter, r *http.Request) {
	user, ok := s.resolvePublicUser(w, r)
	if !ok {
		return
	}
	if !user.ProfilePublic && !s.isOwner(r, user) {
		s.writeError(w, r, apiErrProfileClosed)
		return
	}
	pbs, err := s.store.PublicPBs(r.Context(), user.ID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.servePBs(w, pbs)
}

// handlePublicPortrait serves GET /users/{name}/portrait: the keyboard
// aggregates behind BOTH switches. The portrait has its own opt-in, off by
// default, because per-key error rates and inter-key timings are effectively
// biometric — nobody may turn that on for somebody else — and the general
// switch closing the profile closes the portrait with it regardless
// (docs/PROFILE.md, "Privacy"). The owner passes both gates: they always see
// their own profile whole.
func (s *Service) handlePublicPortrait(w http.ResponseWriter, r *http.Request) {
	user, ok := s.resolvePublicUser(w, r)
	if !ok {
		return
	}
	if owner := s.isOwner(r, user); !owner {
		if !user.ProfilePublic {
			s.writeError(w, r, apiErrProfileClosed)
			return
		}
		if !user.KeyboardPublic {
			s.writeError(w, r, apiErrPortraitClosed)
			return
		}
	}
	s.serveKeyboard(w, r, user.ID)
}

// publicRunView is one public-history row — the wire twin of PublicRun, and
// an allowlist on purpose: no client-reported numbers, no validation trail,
// no setup snapshot, no restart counter, no log size. Everything here is
// either on the run's public replay envelope already or derived from it.
type publicRunView struct {
	ID            uuid.UUID       `json:"id"`
	Mode          string          `json:"mode"`
	DurationMs    *int32          `json:"durationMs,omitempty"`
	WordCount     *int32          `json:"wordCount,omitempty"`
	Lang          string          `json:"lang"`
	ServerMetrics json.RawMessage `json:"serverMetrics"`
	ServerScore   json.RawMessage `json:"serverScore"`
	CreatedAt     time.Time       `json:"createdAt"`
	// Status is a constant by construction — only accepted runs are public —
	// and is still spelled out so the owner-feed and public-feed rows read the
	// same to a shared client renderer.
	Status           string          `json:"status"`
	Grade            *string         `json:"grade,omitempty"`
	Consistency      *float64        `json:"consistency,omitempty"`
	Chars            json.RawMessage `json:"chars,omitempty"`
	QuoteID          *uuid.UUID      `json:"quoteId,omitempty"`
	AdoptedFromRunID *uuid.UUID      `json:"adoptedFromRunId,omitempty"`
	Mods             json.RawMessage `json:"mods,omitempty"`
}

type publicRunsResponse struct {
	Runs       []publicRunView `json:"runs"`
	NextCursor string          `json:"nextCursor,omitempty"`
}

// handlePublicRuns serves GET /users/{name}/runs?cursor=&limit= — the public
// run history, keyset-paginated exactly like the owner's feed. Which runs are
// in it at all is the QUERY's predicate (accepted, owner not banned), so this
// handler only gates the profile and pages.
func (s *Service) handlePublicRuns(w http.ResponseWriter, r *http.Request) {
	user, ok := s.resolvePublicUser(w, r)
	if !ok {
		return
	}
	if !user.ProfilePublic && !s.isOwner(r, user) {
		s.writeError(w, r, apiErrProfileClosed)
		return
	}

	limit := httpx.ParseLimit(r.URL.Query().Get("limit"), publicRunsDefaultLimit, publicRunsMaxLimit)
	var after *RunCursor
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		cur, err := decodeRunCursor(raw)
		if err != nil {
			s.writeError(w, r, apiErrBadRequest("invalid cursor"))
			return
		}
		after = &cur
	}

	// One extra row answers "is there another page" without a second query.
	rows, err := s.store.PublicRuns(r.Context(), user.ID, after, int32(limit+1))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	next := ""
	if len(rows) > limit {
		last := rows[limit-1]
		rows = rows[:limit]
		next = encodeRunCursor(RunCursor{CreatedAt: last.CreatedAt, ID: last.ID})
	}

	views := make([]publicRunView, len(rows))
	for i := range rows {
		run := &rows[i]
		views[i] = publicRunView{
			ID: run.ID, Mode: run.Mode, DurationMs: run.DurationMs, WordCount: run.WordCount,
			Lang: run.Lang, ServerMetrics: run.ServerMetrics, ServerScore: run.ServerScore,
			CreatedAt: run.CreatedAt, Status: "accepted",
			Grade: run.Grade, Consistency: run.Consistency, Chars: run.Chars,
			QuoteID: run.QuoteID, AdoptedFromRunID: run.AdoptedFromRunID, Mods: run.Mods,
		}
	}
	s.writeJSON(w, http.StatusOK, publicRunsResponse{Runs: views, NextCursor: next})
}

// encodeRunCursor / decodeRunCursor: the same opaque (created_at, id) token
// shape the runs domain uses, via the shared httpx plumbing.
func encodeRunCursor(c RunCursor) string {
	return httpx.EncodeCursor(strconv.FormatInt(c.CreatedAt.UTC().UnixNano(), 10), c.ID.String())
}

func decodeRunCursor(token string) (RunCursor, error) {
	parts, err := httpx.DecodeCursor(token, 2)
	if err != nil {
		return RunCursor{}, err
	}
	nanos, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return RunCursor{}, err
	}
	id, err := uuid.Parse(parts[1])
	if err != nil {
		return RunCursor{}, err
	}
	return RunCursor{CreatedAt: time.Unix(0, nanos).UTC(), ID: id}, nil
}
