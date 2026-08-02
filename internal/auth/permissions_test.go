package auth_test

// The authorization model (internal/auth/permissions.go, migration 00023) and
// its two contracts:
//
//   - the bootstrap promotes exactly the VERIFIED accounts on the configured
//     emails, promotes only (never demotes), and is idempotent;
//   - a route behind RequirePermission answers 404 — not 401, not 403 — to
//     everyone the permission does not belong to, anonymous or logged in, and
//     the role is read fresh per request so a promotion takes effect on the
//     caller's very next request, no re-login.

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/typemore/typemore-server/internal/auth"
)

const probePath = "/api/v1/permissions-probe"

func TestAdminBootstrapPromotesOnlyVerifiedEmails(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// ada completes verification; bob registers and never clicks the link.
	h.registerVerifyLogin("ada@example.com", "correct horse battery", "ada")
	resp := h.register("bob@example.com", "correct horse battery", "bobby")
	require.Equal(t, http.StatusOK, resp.StatusCode)

	promoted, err := h.store.PromoteAdmins(ctx,
		[]string{"ada@example.com", "bob@example.com", "ghost@example.com"})
	require.NoError(t, err)
	assert.EqualValues(t, 1, promoted,
		"only the verified account may be promoted: unverified and unknown emails are not")

	var adaRole, bobRole string
	require.NoError(t, h.pool.QueryRow(ctx,
		`SELECT role FROM users WHERE display_name = 'ada'`).Scan(&adaRole))
	require.NoError(t, h.pool.QueryRow(ctx,
		`SELECT role FROM users WHERE display_name = 'bobby'`).Scan(&bobRole))
	assert.Equal(t, "admin", adaRole)
	assert.Equal(t, "player", bobRole)

	// Idempotent: the second pass finds nothing to change.
	promoted, err = h.store.PromoteAdmins(ctx, []string{"ada@example.com"})
	require.NoError(t, err)
	assert.Zero(t, promoted, "an already-admin account is not touched again")
}

func TestPermissionGateIsInvisibleToEveryoneElse(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// Anonymous: the probe does not exist.
	resp := h.get(probePath)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode,
		"an anonymous caller must see the same 404 an unknown route gives — a 401 would confirm the surface exists")

	// Logged in without the permission: still does not exist.
	h.registerVerifyLogin("player@example.com", "correct horse battery", "player")
	resp = h.get(probePath)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode,
		"a plain player must see the same 404 — a 403 would advertise the admin surface to every account")

	// Promotion is visible on the NEXT request, same session: the role is
	// resolved per request, never cached into the session.
	_, err := h.store.PromoteAdmins(ctx, []string{"player@example.com"})
	require.NoError(t, err)
	resp = h.get(probePath)
	assert.Equal(t, http.StatusOK, resp.StatusCode,
		"the role must be read fresh per request — no re-login to pick up a promotion")
}

func TestMeCarriesPermissionsOnlyForAdmins(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	h.registerVerifyLogin("mod@example.com", "correct horse battery", "modmod")

	me := func() map[string]json.RawMessage {
		resp := h.get("/api/v1/me")
		require.Equal(t, http.StatusOK, resp.StatusCode)
		var payload map[string]json.RawMessage
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&payload))
		require.NoError(t, resp.Body.Close())
		return payload
	}

	// A player's payload does not carry the field at all — the common case is
	// byte-identical to what it was before permissions existed.
	_, present := me()["permissions"]
	assert.False(t, present, "a player's /me must omit permissions entirely")

	_, err := h.store.PromoteAdmins(ctx, []string{"mod@example.com"})
	require.NoError(t, err)

	var perms []string
	require.NoError(t, json.Unmarshal(me()["permissions"], &perms))
	assert.ElementsMatch(t, []string{
		string(auth.PermBansRead), string(auth.PermBansWrite),
		string(auth.PermReportsRead), string(auth.PermReportsWrite),
		string(auth.PermQuotesWrite),
	}, perms,
		"an admin's /me lists the expanded permission set, which is what the client renders surfaces from")
}
