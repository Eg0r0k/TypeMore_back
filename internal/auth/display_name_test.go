package auth_test

// The rename and its once-per-30-days rule (00030). The cooldown lives in the
// UPDATE's predicate, so these tests exercise it through the wire and reach
// into the column only to travel in time.

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func decodeJSONBody(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	var body map[string]any
	require.NoError(t, json.Unmarshal([]byte(readBody(t, resp)), &body))
	return body
}

func TestDisplayNameChangeAndCooldown(t *testing.T) {
	h := newHarness(t)
	h.registerVerifyLogin("rename@example.com", "password-123", "FirstName")

	// The registration name did not start the clock: the first change works.
	resp := h.patch("/api/v1/me/display-name", map[string]string{"displayName": "SecondName"})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body := decodeJSONBody(t, resp)
	assert.Equal(t, "SecondName", body["displayName"])
	assert.NotEmpty(t, body["displayNameChangedAt"], "the clock must start with the change")

	// /me serves the new name and the cooldown fact.
	me := decodeJSONBody(t, h.get("/api/v1/me"))
	assert.Equal(t, "SecondName", me["displayName"])
	assert.NotEmpty(t, me["displayNameChangedAt"])

	// A second change inside the window is refused, naming the rule.
	resp = h.patch("/api/v1/me/display-name", map[string]string{"displayName": "ThirdName"})
	require.Equal(t, http.StatusConflict, resp.StatusCode)
	assert.Equal(t, "display_name_cooldown", decodeJSONBody(t, resp)["error"])

	// 31 days later the window has passed.
	_, err := h.pool.Exec(context.Background(),
		`UPDATE users SET display_name_changed_at = now() - interval '31 days'`)
	require.NoError(t, err)
	resp = h.patch("/api/v1/me/display-name", map[string]string{"displayName": "ThirdName"})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "ThirdName", decodeJSONBody(t, resp)["displayName"])
}

func TestDisplayNameChangeRefusals(t *testing.T) {
	h := newHarness(t)
	h.registerVerifyLogin("holder@example.com", "password-123", "Occupied")
	h.registerVerifyLogin("renamer@example.com", "password-123", "Renamer")

	// The unique index is citext: a case-variant of somebody's name is taken.
	resp := h.patch("/api/v1/me/display-name", map[string]string{"displayName": "OCCUPIED"})
	require.Equal(t, http.StatusConflict, resp.StatusCode)
	assert.Equal(t, "name_taken", decodeJSONBody(t, resp)["error"])

	// The charset/length rules are the registration's own.
	resp = h.patch("/api/v1/me/display-name", map[string]string{"displayName": "ab"})
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)

	// The byte-identical name would burn the month on a no-op.
	resp = h.patch("/api/v1/me/display-name", map[string]string{"displayName": "Renamer"})
	require.Equal(t, http.StatusConflict, resp.StatusCode)
	assert.Equal(t, "display_name_unchanged", decodeJSONBody(t, resp)["error"])

	// And none of the refusals started the clock.
	me := decodeJSONBody(t, h.get("/api/v1/me"))
	assert.Nil(t, me["displayNameChangedAt"])
}
