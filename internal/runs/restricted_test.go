package runs_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/typemore/typemore-server/internal/perf"
)

// A restricted account's run is refused, and the refusal is honest.
//
// 403 `account_restricted`, and the run is NOT stored. The standing decision is
// an honest "not counted" rather than a shadow ban: a player typing into a void
// and wondering why their rank never moves is a worse outcome than one who
// knows, and it is the only version that can be appealed.
func TestSubmittingUnderAnActiveBanIs403AndStoresNothing(t *testing.T) {
	h := newHarness(t)
	userID := uuid.MustParse(h.login("mallory@example.com", "correct horse battery", "mallory"))

	// Baseline: this exact payload is accepted while the account is clean, so
	// the 403 below is about the ban and not about the body.
	resp := h.post("/api/v1/runs", perf.BuildPayload(perf.PayloadSpec{}))
	requireStatus(t, resp, http.StatusAccepted)
	require.Equal(t, 1, h.storedRunCount(userID))

	h.banFor(userID, nil)

	resp = h.post("/api/v1/runs", perf.BuildPayload(perf.PayloadSpec{}))
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	body := decodeInto[struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}](t, resp)
	assert.Equal(t, "account_restricted", body.Error)
	assert.NotEmpty(t, body.Message)

	// The reason is an internal moderation note. It must not be anywhere near
	// the wire, and this is the assertion that says so.
	assert.NotContains(t, body.Message, banReason)

	assert.Equal(t, 1, h.storedRunCount(userID),
		"the refused run was stored anyway; 403 must mean not counted AND not kept")
}

// The gate is expiry-aware because it reads the same predicate everything else
// does: a lapsed ban stops refusing, with nothing swept and nothing restarted.
func TestSubmissionResumesWhenABanLapses(t *testing.T) {
	h := newHarness(t)
	userID := uuid.MustParse(h.login("temp@example.com", "correct horse battery", "temp"))

	soon := time.Now().Add(1500 * time.Millisecond)
	h.banFor(userID, &soon)

	resp := h.post("/api/v1/runs", perf.BuildPayload(perf.PayloadSpec{}))
	requireStatus(t, resp, http.StatusForbidden)

	require.Eventually(t, func() bool {
		r := h.post("/api/v1/runs", perf.BuildPayload(perf.PayloadSpec{}))
		code := r.StatusCode
		_ = r.Body.Close()
		return code == http.StatusAccepted
	}, 8*time.Second, 200*time.Millisecond,
		"submission never resumed; the gate is not evaluating expiry at read time")
}

// GET /me carries the flag, and carries nothing else about the ban.
func TestMeReportsRestrictedAndNothingMore(t *testing.T) {
	h := newHarness(t)
	userID := uuid.MustParse(h.login("flag@example.com", "correct horse battery", "flag"))

	type meView struct {
		ID         string `json:"id"`
		Restricted bool   `json:"restricted"`
	}

	me := decodeInto[meView](t, h.get("/api/v1/me"))
	require.False(t, me.Restricted)

	h.banFor(userID, nil)
	raw := readBody(t, h.get("/api/v1/me"))
	assert.Contains(t, string(raw), `"restricted":true`)
	assert.NotContains(t, string(raw), banReason,
		"/me leaked the moderation note")
	assert.NotContains(t, string(raw), "expires",
		"/me exposed when the ban lifts; the banner is deliberately opaque")
	assert.NotContains(t, string(raw), "issued",
		"/me exposed who issued the ban")
}

// The scope proof.
//
// A ban stops runs being counted and hides board entries. It is NOT an account
// suspension: login, sessions and the ability to play are untouched, and this
// is the test that keeps a future change from quietly widening it. If somebody
// decides bans should block login, they have to delete this test and say so.
func TestABannedUserStillLogsInAndKeepsItsSession(t *testing.T) {
	h := newHarness(t)
	const email, password = "still@example.com", "correct horse battery"
	userID := uuid.MustParse(h.login(email, password, "still"))
	h.banFor(userID, nil)

	// The existing session still works.
	requireStatus(t, h.get("/api/v1/me"), http.StatusOK)

	// And so does a fresh login: logging out and back in is not blocked.
	h.logout()
	requireStatus(t, h.post("/api/v1/auth/login",
		map[string]string{"email": email, "password": password}), http.StatusOK)

	me := decodeInto[struct {
		Restricted bool `json:"restricted"`
	}](t, h.get("/api/v1/me"))
	assert.True(t, me.Restricted, "the account is banned and /me should say so")

	// Reading boards is not blocked either — a restricted player can still look
	// at the leaderboard they are no longer on.
	requireStatus(t, h.get("/api/v1/leaderboards"), http.StatusOK)
}

const banReason = "internal-note-do-not-leak"

// banFor puts the user under restriction through the same table banctl writes,
// with a reason this suite can grep the wire for and an optional expiry.
func (h *harness) banFor(userID uuid.UUID, expiresAt *time.Time) {
	h.t.Helper()
	_, err := h.pool.Exec(context.Background(),
		`INSERT INTO bans (user_id, reason, issued_by, expires_at) VALUES ($1, $2, 'test', $3)`,
		userID, banReason, expiresAt)
	require.NoError(h.t, err)
}

func (h *harness) storedRunCount(userID uuid.UUID) int {
	h.t.Helper()
	var n int
	require.NoError(h.t, h.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM runs WHERE user_id = $1`, userID).Scan(&n))
	return n
}
