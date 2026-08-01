package moderation_test

// The admin bans surface (internal/moderation/handler.go): the HTTP contract
// over the same store the CLI used to drive. Permission middlewares are the
// composition root's business (their 404 contract is asserted in
// internal/auth/permissions_test.go), so here they are pass-throughs and the
// suite exercises what is BEHIND them: amend-with-diff, idempotent revoke,
// resolution that refuses ambiguity, and the audit columns.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/typemore/typemore-server/internal/moderation"
)

type adminHarness struct {
	*harness
	server *httptest.Server
	admin  uuid.UUID
}

func newAdminHarness(t *testing.T) *adminHarness {
	t.Helper()
	h := newHarness(t)

	admin := h.user(t, "rootadmin")
	svc := moderation.NewService(h.store,
		func(*http.Request) (moderation.Actor, bool) {
			return moderation.Actor{ID: admin, Name: "rootadmin"}, true
		}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	passthrough := func(next http.Handler) http.Handler { return next }
	server := httptest.NewServer(svc.AdminRoutes(passthrough, passthrough))
	t.Cleanup(server.Close)
	return &adminHarness{harness: h, server: server, admin: admin}
}

func (h *adminHarness) do(t *testing.T, method, path string, body any) (*http.Response, map[string]json.RawMessage) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(t, err)
		reader = strings.NewReader(string(b))
	}
	req, err := http.NewRequest(method, h.server.URL+path, reader)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	var payload map[string]json.RawMessage
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	if len(raw) > 0 && json.Valid(raw) {
		require.NoError(t, json.Unmarshal(raw, &payload))
	}
	return resp, payload
}

func TestAdminBanAmendAndDiff(t *testing.T) {
	h := newAdminHarness(t)
	target := h.user(t, "cheater")

	resp, body := h.do(t, http.MethodPost, "/bans",
		map[string]string{"user": "cheater", "reason": "macro use"})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.JSONEq(t, "false", string(body["amended"]), "the first ban is not an amendment")
	assert.NotContains(t, body, "previous")

	// Re-banning amends in place and reports what moved — never a second row.
	resp, body = h.do(t, http.MethodPost, "/bans",
		map[string]string{"user": target.String(), "reason": "macro use, appealed", "until": "72h"})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.JSONEq(t, "true", string(body["amended"]))
	require.Contains(t, body, "previous", "an amendment must carry the ban as it stood")

	var count int
	require.NoError(t, h.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM bans WHERE user_id = $1`, target).Scan(&count))
	assert.Equal(t, 1, count, "amending must not stack a second ban")

	// The audit column carries the ACTOR's account, not just the note.
	var issuedBy string
	var issuedByUser uuid.UUID
	require.NoError(t, h.pool.QueryRow(context.Background(),
		`SELECT issued_by, issued_by_user FROM bans WHERE user_id = $1`, target).
		Scan(&issuedBy, &issuedByUser))
	assert.Equal(t, "rootadmin", issuedBy)
	assert.Equal(t, h.admin, issuedByUser)
}

func TestAdminUnbanIsIdempotent(t *testing.T) {
	h := newAdminHarness(t)
	target := h.user(t, "banned")

	resp, _ := h.do(t, http.MethodPost, "/bans",
		map[string]string{"user": "banned", "reason": "note"})
	require.Equal(t, http.StatusOK, resp.StatusCode)

	resp, body := h.do(t, http.MethodDelete, "/users/"+target.String()+"/ban", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.JSONEq(t, "true", string(body["revoked"]))

	// The revocation records its actor.
	var revokedBy uuid.UUID
	require.NoError(t, h.pool.QueryRow(context.Background(),
		`SELECT revoked_by_user FROM bans WHERE user_id = $1`, target).Scan(&revokedBy))
	assert.Equal(t, h.admin, revokedBy)

	// DELETE twice means the same thing as DELETE once.
	resp, body = h.do(t, http.MethodDelete, "/users/"+target.String()+"/ban", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.JSONEq(t, "false", string(body["revoked"]))
}

func TestAdminResolutionRefusesAmbiguity(t *testing.T) {
	h := newAdminHarness(t)
	// One VERIFIED email per address is schema law (verified_email_one_user),
	// but an unverified identity can carry the same address on another
	// account — and the resolver matches any identity's email, so the
	// identifier is genuinely ambiguous. It must refuse, never pick.
	a := h.user(t, "doppel")
	b := h.user(t, "doppel2")
	h.identity(t, a, "same@example.com")
	_, err := h.pool.Exec(context.Background(),
		`INSERT INTO auth_identities (user_id, provider, provider_subject, email, email_verified)
		 VALUES ($1, 'email', 'same-unverified', 'SAME@example.com', false)`, b)
	require.NoError(t, err) // citext: the same address, different case

	resp, body := h.do(t, http.MethodGet, "/users/same@example.com/bans", nil)
	require.Equal(t, http.StatusConflict, resp.StatusCode)
	var candidates []map[string]string
	require.NoError(t, json.Unmarshal(body["candidates"], &candidates))
	assert.Len(t, candidates, 2, "an ambiguous identifier must list every candidate and ban nobody")

	// An identifier matching nothing is a 404 — the same answer the whole
	// subtree gives outsiders, so probing resolves nothing.
	resp, _ = h.do(t, http.MethodGet, "/users/ghost@example.com/bans", nil)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestAdminListAndHistory(t *testing.T) {
	h := newAdminHarness(t)
	active := h.user(t, "stillon")
	lifted := h.user(t, "wason")

	for _, name := range []string{"stillon", "wason"} {
		resp, _ := h.do(t, http.MethodPost, "/bans",
			map[string]string{"user": name, "reason": "note " + name})
		require.Equal(t, http.StatusOK, resp.StatusCode)
	}
	resp, _ := h.do(t, http.MethodDelete, "/users/"+lifted.String()+"/ban", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// Default list: in-force bans only.
	resp, body := h.do(t, http.MethodGet, "/bans", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var bans []map[string]any
	require.NoError(t, json.Unmarshal(body["bans"], &bans))
	require.Len(t, bans, 1)
	assert.Equal(t, active.String(), bans[0]["userId"])

	// ?active=0: the full feed, revoked included.
	resp, body = h.do(t, http.MethodGet, "/bans?active=0", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NoError(t, json.Unmarshal(body["bans"], &bans))
	assert.Len(t, bans, 2)

	// History of the lifted account: the revoked row is still there, with the
	// account marked unrestricted — an unban is a fact, not an erasure.
	resp, body = h.do(t, http.MethodGet, fmt.Sprintf("/users/%s/bans", lifted), nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.JSONEq(t, "false", string(body["restricted"]))
	require.NoError(t, json.Unmarshal(body["bans"], &bans))
	require.Len(t, bans, 1)
	assert.NotEmpty(t, bans[0]["revokedAt"])
}

func TestAdminBanValidation(t *testing.T) {
	h := newAdminHarness(t)
	h.user(t, "target")

	// A ban with no note is one nobody can review later.
	resp, _ := h.do(t, http.MethodPost, "/bans", map[string]string{"user": "target"})
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)

	// until must be a duration or a FUTURE instant.
	past := time.Now().Add(-time.Hour).Format(time.RFC3339)
	resp, _ = h.do(t, http.MethodPost, "/bans",
		map[string]string{"user": "target", "reason": "r", "until": past})
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)

	resp, _ = h.do(t, http.MethodPost, "/bans",
		map[string]string{"user": "target", "reason": "r", "until": "banana"})
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}
