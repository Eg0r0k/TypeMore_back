package moderation_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The badge half of the admin surface (00029). What matters here is that a
// grant is idempotent, a revocation is soft and idempotent, and neither can be
// talked into recording a code this build does not know.

type badgeView struct {
	Code      string  `json:"code"`
	GrantedBy string  `json:"grantedBy"`
	RevokedBy string  `json:"revokedBy"`
	Granted   bool    `json:"granted"`
	Shown     bool    `json:"shown"`
	RevokedAt *string `json:"revokedAt"`
}

func badgesOf(t *testing.T, h *adminHarness, name string) []badgeView {
	t.Helper()
	resp, body := h.do(t, http.MethodGet, "/users/"+name+"/badges", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var out []badgeView
	require.NoError(t, json.Unmarshal(body["badges"], &out))
	return out
}

// Granting twice is granting once: the operator who clicked again needs to see
// that the account has the badge, not an error to interpret.
func TestGrantBadgeIsIdempotentAndAudited(t *testing.T) {
	h := newAdminHarness(t)
	h.user(t, "decorated")

	resp, body := h.do(t, http.MethodPost, "/users/decorated/badges",
		map[string]string{"code": "beta_tester"})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var granted badgeView
	require.NoError(t, json.Unmarshal(body["badge"], &granted))
	assert.Equal(t, "beta_tester", granted.Code)
	assert.True(t, granted.Granted)
	// A fresh grant is HIDDEN: putting a badge on somebody's public page is
	// their own act, not the operator's.
	assert.False(t, granted.Shown, "a fresh grant must not appear in a showcase by itself")

	resp, _ = h.do(t, http.MethodPost, "/users/decorated/badges",
		map[string]string{"code": "beta_tester"})
	require.Equal(t, http.StatusOK, resp.StatusCode)

	list := badgesOf(t, h, "decorated")
	require.Len(t, list, 1, "the second grant must not stack a second row")
	assert.Equal(t, "rootadmin", list[0].GrantedBy, "the grant records who made it")
}

// Revocation is SOFT (the row stays, with a time and an actor) and idempotent
// in the same shape unbanning is: DELETE twice means what DELETE once means.
func TestRevokeBadgeIsSoftAndIdempotent(t *testing.T) {
	h := newAdminHarness(t)
	h.user(t, "demoted")

	h.do(t, http.MethodPost, "/users/demoted/badges", map[string]string{"code": "staff"})

	resp, body := h.do(t, http.MethodDelete, "/users/demoted/badges/staff", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.JSONEq(t, "true", string(body["revoked"]))

	resp, body = h.do(t, http.MethodDelete, "/users/demoted/badges/staff", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.JSONEq(t, "false", string(body["revoked"]),
		"a second revoke is not an error — it is the same instruction, already carried out")

	// The row survives with its history: "why did they used to have that" is
	// the question the soft revoke exists to answer.
	list := badgesOf(t, h, "demoted")
	require.Len(t, list, 1)
	assert.False(t, list[0].Granted)
	require.NotNil(t, list[0].RevokedAt)
	assert.Equal(t, "rootadmin", list[0].RevokedBy)

	// And it can be granted again — a new live row beside the revoked one.
	resp, _ = h.do(t, http.MethodPost, "/users/demoted/badges", map[string]string{"code": "staff"})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	list = badgesOf(t, h, "demoted")
	require.Len(t, list, 2, "re-granting after a revocation is a NEW grant, and history keeps both")
	live := 0
	for _, b := range list {
		if b.Granted {
			live++
		}
	}
	assert.Equal(t, 1, live, "exactly one live grant per code")
}

// The code list is the server's only say about a badge. An unknown one is a
// refusal, because the schema's CHECK bounds only the code's SHAPE — this is
// what stands between a typo and a row that renders as a blank chip.
func TestGrantRefusesUnknownBadgeCodes(t *testing.T) {
	h := newAdminHarness(t)
	h.user(t, "typo")

	for _, code := range []string{"not_a_badge", "STAFF", "", "staff2"} {
		resp, body := h.do(t, http.MethodPost, "/users/typo/badges",
			map[string]string{"code": code})
		require.Equal(t, http.StatusBadRequest, resp.StatusCode, "code %q must be refused", code)
		assert.JSONEq(t, `"unknown_badge"`, string(body["error"]))
	}
	assert.Empty(t, badgesOf(t, h, "typo"), "a refused grant stores nothing")
}

// "Who holds X" is the listing that makes a batch of grants checkable without
// walking accounts. Live grants only — a revocation is that account's history.
func TestBadgeHoldersListsLiveGrantsOnly(t *testing.T) {
	h := newAdminHarness(t)
	h.user(t, "holderone")
	h.user(t, "holdertwo")

	h.do(t, http.MethodPost, "/users/holderone/badges", map[string]string{"code": "translator"})
	h.do(t, http.MethodPost, "/users/holdertwo/badges", map[string]string{"code": "translator"})

	holders := func() []map[string]any {
		resp, body := h.do(t, http.MethodGet, "/badges/translator/holders", nil)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		var out []map[string]any
		require.NoError(t, json.Unmarshal(body["holders"], &out))
		return out
	}
	require.Len(t, holders(), 2)

	h.do(t, http.MethodDelete, "/users/holderone/badges/translator", nil)
	remaining := holders()
	require.Len(t, remaining, 1)
	assert.Equal(t, "holdertwo", remaining[0]["displayName"])
}
