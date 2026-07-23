package auth_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/typemore/typemore-server/internal/auth"
	"github.com/typemore/typemore-server/internal/platform/db"
	"github.com/typemore/typemore-server/internal/platform/migrate"
)

const (
	authBase = "/api/v1/auth"
	mePath   = "/api/v1/me"
)

// --- small response shapes ---

type meResponse struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
}

type errResponse struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

func decodeInto[T any](t *testing.T, resp *http.Response) T {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	var v T
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&v))
	return v
}

// --- options ---

func withProviders(p map[string]auth.ProviderCredentials) func(*serverOpts) {
	return func(o *serverOpts) { o.providers = p }
}

func withRate(every time.Duration, burst int) func(*serverOpts) {
	return func(o *serverOpts) { o.rateEvery = every; o.rateBurst = burst }
}

// --- convenience flows ---

func (h *harness) register(email, password, name string) *http.Response {
	return h.post(authBase+"/register", map[string]string{
		"email": email, "password": password, "displayName": name,
	})
}

func (h *harness) verifyLatest() *http.Response {
	return h.post(authBase+"/verify", map[string]string{"token": h.mailer.lastToken(h.t)})
}

func (h *harness) login(email, password string) *http.Response {
	return h.post(authBase+"/login", map[string]string{"email": email, "password": password})
}

// registerVerifyLogin runs the full happy path and leaves a live session in the
// jar.
func (h *harness) registerVerifyLogin(email, password, name string) {
	h.t.Helper()
	requireStatus(h.t, h.register(email, password, name), http.StatusOK)
	requireStatus(h.t, h.verifyLatest(), http.StatusOK)
	requireStatus(h.t, h.login(email, password), http.StatusOK)
}

func requireStatus(t *testing.T, resp *http.Response, want int) {
	t.Helper()
	got := resp.StatusCode
	_ = resp.Body.Close()
	require.Equal(t, want, got)
}

// --- TestEmailFlow: register -> login blocked -> verify -> login -> me -> logout ---

func TestEmailFlow(t *testing.T) {
	h := newHarness(t)
	const email, password = "neo@example.com", "trinity-1999"

	requireStatus(t, h.register(email, password, "Neo"), http.StatusOK)
	require.Equal(t, 1, h.mailer.count(), "register should send one verification email")

	// Login before verification is blocked.
	resp := h.login(email, password)
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	assert.Equal(t, "email_not_verified", decodeInto[errResponse](t, resp).Error)

	// Verify, then login succeeds.
	requireStatus(t, h.verifyLatest(), http.StatusOK)

	resp = h.login(email, password)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	loggedIn := decodeInto[meResponse](t, resp)
	assert.Equal(t, "Neo", loggedIn.DisplayName)

	// /me returns the same user.
	resp = h.get(mePath)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, loggedIn.ID, decodeInto[meResponse](t, resp).ID)

	// Logout, then /me is unauthorized.
	requireStatus(t, h.post(authBase+"/logout", nil), http.StatusOK)
	requireStatus(t, h.get(mePath), http.StatusUnauthorized)
}

// --- TestPasswordResetRevokesSessions ---

func TestPasswordResetRevokesSessions(t *testing.T) {
	h := newHarness(t)
	const email, oldPw, newPw = "reset@example.com", "old-password-1", "new-password-2"

	h.registerVerifyLogin(email, oldPw, "Resetter")
	requireStatus(t, h.get(mePath), http.StatusOK) // session works

	// Request + confirm reset.
	requireStatus(t, h.post(authBase+"/password-reset/request", map[string]string{"email": email}), http.StatusOK)
	requireStatus(t, h.post(authBase+"/password-reset/confirm", map[string]string{
		"token": h.mailer.lastToken(t), "newPassword": newPw,
	}), http.StatusOK)

	// The previously-live session is revoked.
	requireStatus(t, h.get(mePath), http.StatusUnauthorized)

	// Old password no longer works; new password does.
	resp := h.login(email, oldPw)
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	assert.Equal(t, "invalid_credentials", decodeInto[errResponse](t, resp).Error)

	requireStatus(t, h.login(email, newPw), http.StatusOK)
}

// --- TestAntiEnumeration: taken-email register and unknown-email reset look
// identical to the happy path. ---

func TestAntiEnumeration(t *testing.T) {
	h := newHarness(t)
	const email, password = "enum@example.com", "password-1234"

	first := readBody(t, h.register(email, password, "Enum"))
	// Registering the SAME email again must be byte-identical and must not create
	// a second account or send a second email.
	second := readBody(t, h.register(email, password, "Enum"))
	assert.Equal(t, first, second, "taken-email register must match fresh register")
	assert.Equal(t, 1, h.mailer.count(), "no second email for a taken address")

	// Reset for an unknown email returns the same generic success body.
	reset := readBody(t, h.post(authBase+"/password-reset/request", map[string]string{"email": "ghost@example.com"}))
	assert.Equal(t, first, reset, "unknown-email reset must match the generic success")
}

// --- OAuth ---

func TestOAuthCreateAndRelogin(t *testing.T) {
	fp := newFakeProvider(t)
	fp.set("google-create", "oauth-new@example.com", true)
	h := newHarness(t, withProviders(fp.creds()))

	// First OAuth login creates the account.
	loc := h.oauthLogin(t, auth.ProviderGoogle)
	assert.Contains(t, loc, "status=ok")

	resp := h.get(mePath)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	created := decodeInto[meResponse](t, resp)
	assert.Equal(t, "OAuthUser", created.DisplayName) // provider name "OAuth User", sanitized

	// Re-login with the same subject returns the same account (no duplicate).
	loc = h.oauthLogin(t, auth.ProviderGoogle)
	assert.Contains(t, loc, "status=ok")
	again := decodeInto[meResponse](t, h.get(mePath))
	assert.Equal(t, created.ID, again.ID)
}

func TestOAuthLinkToExistingAccount(t *testing.T) {
	fp := newFakeProvider(t)
	fp.set("google-link-sub", "linker-provider@example.com", true)
	h := newHarness(t, withProviders(fp.creds()))

	const email, password = "linker@example.com", "password-link-1"
	h.registerVerifyLogin(email, password, "Linker")
	me := decodeInto[meResponse](t, h.get(mePath))

	// Explicit link: POST start (returns authorize URL), then complete callback.
	resp := h.post(authBase+"/link/google/start", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	authorizeURL := decodeInto[struct {
		AuthorizeURL string `json:"authorizeUrl"`
	}](t, resp).AuthorizeURL
	state := stateFromURL(t, authorizeURL)

	loc := h.oauthCallback(t, auth.ProviderGoogle, state)
	assert.Contains(t, loc, "linked=google")

	// The google identity is now attached to the SAME user.
	var userID string
	require.NoError(t, h.pool.QueryRow(context.Background(),
		`SELECT user_id::text FROM auth_identities WHERE provider='google' AND provider_subject=$1`,
		"google-link-sub").Scan(&userID))
	assert.Equal(t, me.ID, userID)
}

func TestOAuthEmailCollisionNoAutoLink(t *testing.T) {
	fp := newFakeProvider(t)
	h := newHarness(t, withProviders(fp.creds()))

	const email, password = "collide@example.com", "password-collide-1"
	// A verified email account already owns the address.
	requireStatus(t, h.register(email, password, "Owner"), http.StatusOK)
	requireStatus(t, h.verifyLatest(), http.StatusOK)

	// OAuth arrives with the same verified email but a new subject: no auto-link.
	fp.set("google-collision", email, true)
	loc := h.oauthLogin(t, auth.ProviderGoogle)
	assert.Contains(t, loc, "error=account_exists_use_linking")

	// No session was created.
	requireStatus(t, h.get(mePath), http.StatusUnauthorized)
}

// --- TestRateLimit ---

func TestRateLimit(t *testing.T) {
	h := newHarness(t, withRate(time.Hour, 1)) // one request, then blocked

	// First request is allowed (invalid creds, but it reached the handler).
	first := h.login("nobody@example.com", "whatever-123")
	require.Equal(t, http.StatusUnauthorized, first.StatusCode)
	_ = first.Body.Close()

	// Second request from the same IP is rate limited.
	second := h.login("nobody@example.com", "whatever-123")
	require.Equal(t, http.StatusTooManyRequests, second.StatusCode)
	assert.Equal(t, "rate_limited", decodeInto[errResponse](t, second).Error)
}

// --- TestMigrationsIdempotent (migration drift) ---

func TestMigrationsIdempotent(t *testing.T) {
	ctx := context.Background()
	dsn := ensureDB(t)
	// Re-applying on the already-migrated scratch DB is a clean no-op.
	require.NoError(t, migrate.Up(ctx, dsn))

	pool, err := db.NewPool(ctx, dsn, 2)
	require.NoError(t, err)
	defer pool.Close()

	var version int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT max(version_id) FROM goose_db_version`).Scan(&version))
	assert.Equal(t, int64(2), version, "expected to be at migration version 2")
}

// --- fake OAuth provider ---

// fakeProvider is an httptest OAuth provider exposing /token and /userinfo. The
// browser authorize step is skipped in tests (we drive the callback directly),
// so it needs no /authorize handler.
type fakeProvider struct {
	server        *httptest.Server
	mu            sync.Mutex
	sub           string
	email         string
	name          string
	emailVerified bool
}

func newFakeProvider(t *testing.T) *fakeProvider {
	t.Helper()
	fp := &fakeProvider{sub: "google-default", name: "OAuth User", emailVerified: true}

	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "fake-access-token",
			"token_type":   "Bearer",
			"expires_in":   3600,
		})
	})
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, _ *http.Request) {
		fp.mu.Lock()
		defer fp.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"sub":            fp.sub,
			"email":          fp.email,
			"email_verified": fp.emailVerified,
			"name":           fp.name,
		})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	fp.server = srv
	return fp
}

func (fp *fakeProvider) set(sub, email string, verified bool) {
	fp.mu.Lock()
	defer fp.mu.Unlock()
	fp.sub, fp.email, fp.emailVerified = sub, email, verified
}

func (fp *fakeProvider) creds() map[string]auth.ProviderCredentials {
	return map[string]auth.ProviderCredentials{
		auth.ProviderGoogle: {
			ClientID:     "test-client-id",
			ClientSecret: "test-client-secret",
			AuthURL:      fp.server.URL + "/authorize",
			TokenURL:     fp.server.URL + "/token",
			UserInfoURL:  fp.server.URL + "/userinfo",
		},
	}
}

// oauthLogin runs the non-link OAuth flow (start -> callback) and returns the
// final redirect Location.
func (h *harness) oauthLogin(t *testing.T, provider string) string {
	t.Helper()
	resp := h.get(authBase + "/oauth/" + provider + "/start")
	require.Equal(t, http.StatusFound, resp.StatusCode)
	state := stateFromURL(t, resp.Header.Get("Location"))
	_ = resp.Body.Close()
	return h.oauthCallback(t, provider, state)
}

// oauthCallback drives the provider callback with a fixed code and the given
// state, returning the final redirect Location.
func (h *harness) oauthCallback(t *testing.T, provider, state string) string {
	t.Helper()
	resp := h.get(authBase + "/oauth/" + provider + "/callback?code=fake-code&state=" + url.QueryEscape(state))
	require.Equal(t, http.StatusFound, resp.StatusCode)
	loc := resp.Header.Get("Location")
	_ = resp.Body.Close()
	return loc
}

func stateFromURL(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	require.NoError(t, err)
	state := u.Query().Get("state")
	require.NotEmpty(t, state, "authorize URL missing state")
	return state
}
