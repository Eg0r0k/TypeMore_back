package runs_test

// Public profiles end-to-end (docs/PROFILE.md, "Public profiles"): the
// /users/{name} surface over real HTTP, with real ingested-and-judged runs.
// Privacy must be enforced by the SERVER — every test here talks HTTP past any
// frontend, because a frontend hiding sections over an API that still answers
// is not privacy.

import (
	"net/http"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// meResp is the slice of GET /me these tests read.
type meResp struct {
	DisplayName    string `json:"displayName"`
	ProfilePublic  bool   `json:"profilePublic"`
	KeyboardPublic bool   `json:"keyboardPublic"`
}

// publicDataPaths are every public route that serves profile DATA (the header
// is deliberately absent: it answers 200 for a closed profile).
func publicDataPaths(name string) []string {
	return []string{
		"/api/v1/users/" + name + "/summary",
		"/api/v1/users/" + name + "/activity",
		"/api/v1/users/" + name + "/histogram",
		"/api/v1/users/" + name + "/timeseries",
		"/api/v1/users/" + name + "/pbs",
		"/api/v1/users/" + name + "/runs",
	}
}

// The migration's defaults, read through the API: a fresh account is open, and
// its keyboard portrait is its own opt-in, OFF — per-key timing is effectively
// biometric and nobody may publish it for somebody else.
func TestNewAccountDefaultsOpenProfileClosedPortrait(t *testing.T) {
	h := newHarness(t)
	h.login("defaults@example.com", "correct horse battery", "defaultish")

	me := decodeInto[meResp](t, h.get("/api/v1/me"))
	assert.True(t, me.ProfilePublic, "profiles are open by default")
	assert.False(t, me.KeyboardPublic, "the portrait is private by default")
}

// PATCH /me/settings is a partial write: each switch moves alone.
func TestSettingsPatchIsPartial(t *testing.T) {
	h := newHarness(t)
	h.login("partial@example.com", "correct horse battery", "partialer")

	after := decodeInto[meResp](t, h.patch("/api/v1/me/settings",
		map[string]bool{"keyboardPublic": true}))
	assert.True(t, after.ProfilePublic, "an untouched switch must not move")
	assert.True(t, after.KeyboardPublic)

	after = decodeInto[meResp](t, h.patch("/api/v1/me/settings",
		map[string]bool{"profilePublic": false}))
	assert.False(t, after.ProfilePublic)
	assert.True(t, after.KeyboardPublic, "an untouched switch must not move")

	// An empty patch is a client bug, not a no-op write.
	requireStatus(t, h.patch("/api/v1/me/settings", map[string]bool{}), http.StatusBadRequest)
	// And the whole route needs a session.
	h.logout()
	requireStatus(t, h.patch("/api/v1/me/settings", map[string]bool{"profilePublic": true}),
		http.StatusUnauthorized)
}

// An open profile answers strangers and the anonymous alike; an unknown name
// is a plain 404 (names are public on every board — there is no enumeration
// story to blur it for).
func TestOpenProfileAnswersStrangers(t *testing.T) {
	h := newHarness(t)
	h.login("open@example.com", "correct horse battery", "openplayer")
	requireStatus(t, h.post("/api/v1/runs", goldenPayload(t, "time-clean")), http.StatusAccepted)
	h.replayOnce(t)
	h.logout() // anonymous from here on

	header := decodeInto[struct {
		Name   string `json:"name"`
		Public bool   `json:"public"`
	}](t, h.get("/api/v1/users/openplayer"))
	assert.Equal(t, "openplayer", header.Name)
	assert.True(t, header.Public)

	for _, path := range publicDataPaths("openplayer") {
		requireStatus(t, h.get(path), http.StatusOK)
	}

	// citext: the lookup is case-insensitive, like the uniqueness it mirrors.
	requireStatus(t, h.get("/api/v1/users/OPENplayer"), http.StatusOK)
	requireStatus(t, h.get("/api/v1/users/nosuchname"), http.StatusNotFound)

	runsPage := decodeInto[struct {
		Runs []map[string]any `json:"runs"`
	}](t, h.get("/api/v1/users/openplayer/runs"))
	require.Len(t, runsPage.Runs, 1, "the accepted run is public history")
}

// A closed profile: 403 profile_closed on every data route for strangers and
// the anonymous, 200 with public:false on the header (the page EXISTS — closed
// is a state, not a 404), and the owner still sees everything.
func TestClosedProfileRefusesDataButKeepsTheHeader(t *testing.T) {
	h := newHarness(t)
	h.login("closed@example.com", "correct horse battery", "closedplayer")
	requireStatus(t, h.post("/api/v1/runs", goldenPayload(t, "time-clean")), http.StatusAccepted)
	h.replayOnce(t)
	requireStatus(t, h.patch("/api/v1/me/settings", map[string]bool{"profilePublic": false}),
		http.StatusOK)

	// The owner walks their own public paths freely — the preview case — and
	// their session-scoped surface is untouched by the switch.
	for _, path := range publicDataPaths("closedplayer") {
		requireStatus(t, h.get(path), http.StatusOK)
	}
	requireStatus(t, h.get("/api/v1/users/closedplayer/portrait"), http.StatusOK)
	requireStatus(t, h.get("/api/v1/profile/summary"), http.StatusOK)
	requireStatus(t, h.get("/api/v1/profile/keyboard"), http.StatusOK)

	// A logged-in STRANGER is refused like the anonymous — closed is closed.
	h.logout()
	h.login("stranger@example.com", "correct horse battery", "strangerer")
	for _, path := range publicDataPaths("closedplayer") {
		resp := h.get(path)
		body := decodeInto[map[string]any](t, resp)
		require.Equal(t, http.StatusForbidden, resp.StatusCode, "%s must refuse", path)
		assert.Equal(t, "profile_closed", body["error"], "%s carries the state code", path)
	}
	requireStatus(t, h.get("/api/v1/users/closedplayer/portrait"), http.StatusForbidden)

	header := decodeInto[struct {
		Name   string `json:"name"`
		Public bool   `json:"public"`
	}](t, h.get("/api/v1/users/closedplayer"))
	assert.Equal(t, "closedplayer", header.Name)
	assert.False(t, header.Public)

	// Anonymous: the same refusals.
	h.logout()
	for _, path := range publicDataPaths("closedplayer") {
		requireStatus(t, h.get(path), http.StatusForbidden)
	}
	requireStatus(t, h.get("/api/v1/users/closedplayer"), http.StatusOK)
}

// The portrait's own switch: an OPEN profile still refuses the portrait until
// its owner turned it on themselves, with a distinct code so the page can name
// the state; flipping it opens exactly that one section; closing the profile
// closes the portrait regardless of the portrait switch.
func TestPortraitIsItsOwnOptIn(t *testing.T) {
	h := newHarness(t)
	h.login("portrait@example.com", "correct horse battery", "portraiter")

	// Stranger view of an open profile with the default portrait switch.
	h.logout()
	h.login("viewer@example.com", "correct horse battery", "viewerer")
	resp := h.get("/api/v1/users/portraiter/portrait")
	body := decodeInto[map[string]any](t, resp)
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	assert.Equal(t, "portrait_closed", body["error"],
		"an open profile's closed portrait is its own state, not profile_closed")

	// The owner flips the portrait on; the stranger now sees it.
	h.logout()
	h.loginAs("portrait@example.com", "correct horse battery")
	requireStatus(t, h.patch("/api/v1/me/settings", map[string]bool{"keyboardPublic": true}),
		http.StatusOK)
	h.logout()
	h.loginAs("viewer@example.com", "correct horse battery")
	requireStatus(t, h.get("/api/v1/users/portraiter/portrait"), http.StatusOK)

	// General privacy wins over the portrait switch: profile closed ⇒ portrait
	// closed, whatever keyboardPublic says.
	h.logout()
	h.loginAs("portrait@example.com", "correct horse battery")
	requireStatus(t, h.patch("/api/v1/me/settings", map[string]bool{"profilePublic": false}),
		http.StatusOK)
	h.logout()
	resp = h.get("/api/v1/users/portraiter/portrait")
	body = decodeInto[map[string]any](t, resp)
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	assert.Equal(t, "profile_closed", body["error"])
}

// THE BOUNDARY PIN (the line the task forbids crossing): profile privacy does
// not touch the boards. A player with a CLOSED profile keeps their board row,
// under their name, and the run holding the slot still resolves and replays —
// what closes is the aggregated history page, never a result the player put
// into a public ranking. Meanwhile a NON-board run of the same closed profile
// stops being publicly watchable: that one is only reachable through the
// history page privacy just closed.
func TestClosedProfileKeepsItsBoardRowAndItsBoardReplay(t *testing.T) {
	h := newHarness(t)
	h.login("pinned@example.com", "correct horse battery", "pinnedplayer")

	// One ranked run (15s time — holds a board slot) and one unranked run
	// (10 words — a perfectly good run that ranks nowhere).
	board := decodeInto[struct {
		ID string `json:"id"`
	}](t, h.post("/api/v1/runs", goldenPayload(t, "time-clean")))
	unranked := decodeInto[struct {
		ID string `json:"id"`
	}](t, h.post("/api/v1/runs", goldenPayload(t, "words-clean")))
	h.replayOnce(t)

	// Sanity while open: both replays public.
	h.logout()
	requireStatus(t, h.get("/api/v1/runs/"+board.ID+"/replay"), http.StatusOK)
	requireStatus(t, h.get("/api/v1/runs/"+unranked.ID+"/replay"), http.StatusOK)

	// Close the profile.
	h.loginAs("pinned@example.com", "correct horse battery")
	requireStatus(t, h.patch("/api/v1/me/settings", map[string]bool{"profilePublic": false}),
		http.StatusOK)
	h.logout()

	// The board row is still there, clickable name and all.
	page := decodeInto[struct {
		Entries []struct {
			DisplayName string `json:"displayName"`
			RunID       string `json:"runId"`
		} `json:"entries"`
	}](t, h.get("/api/v1/leaderboards/time:15000:german:seeded"))
	require.Len(t, page.Entries, 1, "a closed profile must NOT leave its boards")
	assert.Equal(t, "pinnedplayer", page.Entries[0].DisplayName)
	assert.Equal(t, board.ID, page.Entries[0].RunID)

	// The board run still resolves AND replays, anonymously.
	requireStatus(t, h.get("/api/v1/runs/"+board.ID+"/replay"), http.StatusOK)
	requireStatus(t, h.get("/api/v1/runs/"+board.ID+"/replay/log"), http.StatusOK)

	// The non-board run is now the same indistinguishable 404 as every other
	// unwatchable run, on both routes.
	requireStatus(t, h.get("/api/v1/runs/"+unranked.ID+"/replay"), http.StatusNotFound)
	requireStatus(t, h.get("/api/v1/runs/"+unranked.ID+"/replay/log"), http.StatusNotFound)

	// And the history page that WOULD have listed both is the closed one.
	requireStatus(t, h.get("/api/v1/users/pinnedplayer/runs"), http.StatusForbidden)

	// Reopening restores the non-board replay — the switch is a filter, not a
	// deletion.
	h.loginAs("pinned@example.com", "correct horse battery")
	requireStatus(t, h.patch("/api/v1/me/settings", map[string]bool{"profilePublic": true}),
		http.StatusOK)
	h.logout()
	requireStatus(t, h.get("/api/v1/runs/"+unranked.ID+"/replay"), http.StatusOK)
}

// The public payloads are explicit allowlists. This is the snapshot: a public
// run row carries exactly these keys and nothing else — no client-reported
// numbers, no validation trail, no setup snapshot, no restart counter, no log
// size, and (transitively) no email or provider identity, which no profile
// payload has ever carried.
func TestPublicRunPayloadIsAnAllowlist(t *testing.T) {
	h := newHarness(t)
	h.login("allow@example.com", "correct horse battery", "allowlisted")
	requireStatus(t, h.post("/api/v1/runs", goldenPayload(t, "time-clean")), http.StatusAccepted)
	h.replayOnce(t)
	h.logout()

	page := decodeInto[struct {
		Runs []map[string]any `json:"runs"`
	}](t, h.get("/api/v1/users/allowlisted/runs"))
	require.Len(t, page.Runs, 1)

	got := make([]string, 0, len(page.Runs[0]))
	for k := range page.Runs[0] {
		got = append(got, k)
	}
	sort.Strings(got)
	// The full possible key set, minus the ones absent on this particular run
	// (a judged seeded non-adopted run has no quoteId / adoptedFromRunId).
	assert.Equal(t, []string{
		"chars", "consistency", "createdAt", "durationMs", "grade", "id", "lang",
		"mode", "mods", "serverMetrics", "serverScore", "status",
	}, got, "a new field on the public run row is a DELIBERATE disclosure — update this snapshot in the same commit that argues why")

	// The owner's own feed still carries the private half, untouched.
	h.loginAs("allow@example.com", "correct horse battery")
	own := decodeInto[struct {
		Runs []map[string]any `json:"runs"`
	}](t, h.get("/api/v1/runs"))
	require.Len(t, own.Runs, 1)
	for _, key := range []string{"clientMetrics", "clientScore", "validation", "setup"} {
		assert.Contains(t, own.Runs[0], key, "the session feed must not lose %s", key)
	}
}

// A banned owner's public history is empty and their public PBs are hidden —
// the same active_bans predicate the boards read through — while the summary
// aggregates deliberately keep answering (existing semantics: aggregates are
// not rebuilt under bans, docs/PROFILE.md).
func TestBanEmptiesPublicHistoryAndPBs(t *testing.T) {
	h := newHarness(t)
	userID := h.login("banned@example.com", "correct horse battery", "bannedplayer")
	requireStatus(t, h.post("/api/v1/runs", goldenPayload(t, "time-clean")), http.StatusAccepted)
	h.replayOnce(t)
	h.logout()

	before := decodeInto[struct {
		Runs []map[string]any `json:"runs"`
	}](t, h.get("/api/v1/users/bannedplayer/runs"))
	require.Len(t, before.Runs, 1)
	beforePBs := decodeInto[struct {
		PBs []map[string]any `json:"pbs"`
	}](t, h.get("/api/v1/users/bannedplayer/pbs"))
	require.Len(t, beforePBs.PBs, 1)

	h.ban(userID)

	after := decodeInto[struct {
		Runs []map[string]any `json:"runs"`
	}](t, h.get("/api/v1/users/bannedplayer/runs"))
	assert.Empty(t, after.Runs, "a banned owner's public history hides every run")
	afterPBs := decodeInto[struct {
		PBs []map[string]any `json:"pbs"`
	}](t, h.get("/api/v1/users/bannedplayer/pbs"))
	assert.Empty(t, afterPBs.PBs, "a banned owner's public PBs hide with the boards")

	// Aggregates keep answering — they were never ban-filtered, and going
	// quiet here would leak the ban through a side door the boards already
	// refuse to leak through.
	summary := decodeInto[map[string]any](t, h.get("/api/v1/users/bannedplayer/summary"))
	assert.EqualValues(t, 1, summary["testsCompleted"])

	h.unban(userID)
	restored := decodeInto[struct {
		Runs []map[string]any `json:"runs"`
	}](t, h.get("/api/v1/users/bannedplayer/runs"))
	assert.Len(t, restored.Runs, 1, "an unban restores the history with no rebuild")
}

// The public history pages with the same keyset contract as the owner's feed.
func TestPublicRunsPaginate(t *testing.T) {
	h := newHarness(t)
	h.login("pages@example.com", "correct horse battery", "pagedplayer")
	for range 3 {
		requireStatus(t, h.post("/api/v1/runs", goldenPayload(t, "words-clean")), http.StatusAccepted)
	}
	h.replayOnce(t)
	h.logout()

	first := decodeInto[struct {
		Runs       []map[string]any `json:"runs"`
		NextCursor string           `json:"nextCursor"`
	}](t, h.get("/api/v1/users/pagedplayer/runs?limit=2"))
	require.Len(t, first.Runs, 2)
	require.NotEmpty(t, first.NextCursor)

	second := decodeInto[struct {
		Runs       []map[string]any `json:"runs"`
		NextCursor string           `json:"nextCursor"`
	}](t, h.get("/api/v1/users/pagedplayer/runs?limit=2&cursor="+first.NextCursor))
	require.Len(t, second.Runs, 1)
	assert.Empty(t, second.NextCursor)

	seen := map[any]bool{}
	for _, r := range append(first.Runs, second.Runs...) {
		seen[r["id"]] = true
	}
	assert.Len(t, seen, 3, "the pages tile the history with no duplicate")

	requireStatus(t, h.get("/api/v1/users/pagedplayer/runs?cursor=garbage"), http.StatusBadRequest)
}
