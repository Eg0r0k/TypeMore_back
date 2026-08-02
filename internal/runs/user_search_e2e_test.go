package runs_test

// Player search end-to-end (docs/PROFILE.md, "Search"): GET /api/v1/users?q=
// over real HTTP against real accounts. The three things worth defending here
// are that substring matching actually finds the middle of a handle, that the
// input is escaped before it reaches LIKE, and that the visibility rules match
// the boards' — searching must not become the one surface that shows what
// every other one hides.

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// searchResp is the wire shape of a search answer.
type searchResp struct {
	Users []struct {
		Name   string    `json:"name"`
		Joined time.Time `json:"joined"`
		Public bool      `json:"public"`
	} `json:"users"`
}

// search runs one query and returns the hit names IN THE ORDER THE SERVER SENT
// THEM — the ranking is part of the contract, so no test here may sort.
func (h *harness) search(t *testing.T, query string) []string {
	t.Helper()
	body := decodeInto[searchResp](t, h.get("/api/v1/users?q="+url.QueryEscape(query)))
	names := make([]string, len(body.Users))
	for i, u := range body.Users {
		names[i] = u.Name
	}
	return names
}

// The feature's whole reason for existing: a handle whose identity sits in the
// MIDDLE is findable. A prefix-only search would return "egor" and "egorking"
// and quietly lose "ttv_egor" — the player the searcher was most likely
// looking for by that name.
//
// The order is the documented ranking: exact match, then prefix, then shortest,
// then alphabetical.
func TestSearchMatchesSubstringAndRanksExactFirst(t *testing.T) {
	h := newHarness(t)
	h.login("king@example.com", "correct horse battery", "egorking")
	h.login("ttv@example.com", "correct horse battery", "ttv_egor")
	h.login("plain@example.com", "correct horse battery", "egor")
	h.logout()

	assert.Equal(t, []string{"egor", "egorking", "ttv_egor"}, h.search(t, "egor"),
		"exact, then prefix, then the substring hit")

	// Case folds both ways: the searcher does not know how the name was typed.
	assert.Equal(t, []string{"egor", "egorking", "ttv_egor"}, h.search(t, "EGoR"))

	// A substring that only ever occurs mid-name still resolves.
	assert.Equal(t, []string{"ttv_egor"}, h.search(t, "tv_"))
}

// '_' is both a legal display_name character and LIKE's single-character
// wildcard. Unescaped, this search would also return "fooxbar" — a wrong
// answer, not a loose one, and one that only shows up once somebody registers
// a name with an underscore.
func TestSearchEscapesLikeWildcards(t *testing.T) {
	h := newHarness(t)
	h.login("under@example.com", "correct horse battery", "foo_bar")
	h.login("nounder@example.com", "correct horse battery", "fooxbar")
	h.login("wild@example.com", "correct horse battery", "azzzb")
	h.logout()

	assert.Equal(t, []string{"foo_bar"}, h.search(t, "foo_bar"),
		"'_' must match itself, not any character")
	// '%' cannot occur in a name at all, so a query containing one matches
	// nothing. If it reached LIKE unescaped, "a%b" would match "azzzb".
	assert.Empty(t, h.search(t, "a%b"), "'%' must be a literal, not a wildcard")
	// Same for the escape character itself.
	assert.Empty(t, h.search(t, `a\zb`))
}

// Search hides banned players exactly as every leaderboard does. The header
// route is deliberately NOT part of that rule — it has never been what hides a
// ban, and a direct link to a banned name keeps answering 200.
func TestSearchHidesBannedPlayers(t *testing.T) {
	h := newHarness(t)
	userID := h.login("banned@example.com", "correct horse battery", "bannedguy")
	h.login("clean@example.com", "correct horse battery", "banneddude")
	h.logout()

	require.Equal(t, []string{"bannedguy", "banneddude"}, h.search(t, "banned"),
		"both are findable before the ban (equal prefix rank, so shortest first)")

	h.ban(userID)
	assert.Equal(t, []string{"banneddude"}, h.search(t, "banned"),
		"a banned account leaves the search results")
	requireStatus(t, h.get("/api/v1/users/bannedguy"), http.StatusOK)

	h.unban(userID)
	assert.Equal(t, []string{"bannedguy", "banneddude"}, h.search(t, "banned"),
		"and comes back when the ban lifts")
}

// A closed profile stays findable by name — it is still ranked on the boards
// under that name and its header still answers 200, so hiding it from search
// would conceal nothing while breaking "closed is a state, not a 404". The
// `public` flag is what lets a client render the closed state instead of
// linking to a page that will 403.
func TestSearchListsClosedProfilesAsClosed(t *testing.T) {
	h := newHarness(t)
	h.login("closed@example.com", "correct horse battery", "closedone")
	requireStatus(t, h.patch("/api/v1/me/settings", map[string]bool{"profilePublic": false}),
		http.StatusOK)
	h.logout()

	body := decodeInto[searchResp](t, h.get("/api/v1/users?q=closed"))
	require.Len(t, body.Users, 1)
	assert.Equal(t, "closedone", body.Users[0].Name)
	assert.False(t, body.Users[0].Public, "the hit must announce the closed state")
	assert.False(t, body.Users[0].Joined.IsZero(), "joined is part of the hit")

	// And search remains a way to FIND the profile, never a way to read it.
	requireStatus(t, h.get("/api/v1/users/closedone/summary"), http.StatusForbidden)
}

// The length bounds are the trigram index's floor and the display_name CHECK's
// ceiling. Both are 400s rather than empty results: they are questions the
// server will not ask the database, not questions with no answer.
func TestSearchRejectsOutOfRangeQueries(t *testing.T) {
	h := newHarness(t)
	h.login("bounds@example.com", "correct horse battery", "boundsy")
	h.logout()

	for _, q := range []string{"", "a", "ab", "  b  ", strings.Repeat("a", 21)} {
		requireStatus(t, h.get("/api/v1/users?q="+url.QueryEscape(q)), http.StatusBadRequest)
	}
	// A missing q is the same client bug as a short one.
	requireStatus(t, h.get("/api/v1/users"), http.StatusBadRequest)

	// Whitespace is trimmed, not counted: this is the same query as "bounds".
	assert.Equal(t, []string{"boundsy"}, h.search(t, "  bounds  "))
	// A name nobody has is an empty list and a 200 — the question was answered.
	body := decodeInto[searchResp](t, h.get("/api/v1/users?q=nobodyhasthis"))
	assert.Empty(t, body.Users)
}

// limit is honoured and capped, and the cap cannot be raised past the maximum
// by asking for more.
func TestSearchLimitIsBounded(t *testing.T) {
	h := newHarness(t)
	for _, name := range []string{"limita", "limitb", "limitc"} {
		h.login(name+"@example.com", "correct horse battery", name)
	}
	h.logout()

	body := decodeInto[searchResp](t, h.get("/api/v1/users?q=limit&limit=2"))
	assert.Len(t, body.Users, 2, "limit is honoured")

	// Over the maximum, and garbage, both fall back to a bounded answer rather
	// than an error — a limit is a hint, unlike q, which is the question.
	for _, raw := range []string{"9999", "-1", "abc"} {
		body = decodeInto[searchResp](t, h.get("/api/v1/users?q=limit&limit="+raw))
		assert.Len(t, body.Users, 3)
	}
}
