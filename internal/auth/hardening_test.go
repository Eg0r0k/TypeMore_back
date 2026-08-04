package auth_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/typemore/typemore-server/internal/auth"
	"github.com/typemore/typemore-server/internal/auth/pgstore"
)

// Tests for the schema-hardening invariants: email case normalization, the
// verified_email_one_user exclusion constraint, the expiry janitor, and
// display-name uniqueness.

func mustExec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	_, err := pool.Exec(context.Background(), sql, args...)
	require.NoError(t, err)
}

// --- 0. Request body ceiling ---

// TestOversizedBodyIsRefused pins the cap on what an auth endpoint will parse.
//
// It is a REGRESSION test: the per-IP limiter and the argon2id hash gate both
// guard the wrong side of the allocation. By the time either can refuse
// anything, the decoder has already read whatever the caller sent — so without
// a ceiling a single login could allocate a multi-hundred-megabyte JSON string,
// and the hash gate (which bounds hashing memory, not parsing memory) would
// never see it. The captcha gate does bound the body, but only on three routes
// and only when a deployment configures Turnstile, which is off by default.
func TestOversizedBodyIsRefused(t *testing.T) {
	h := newHarness(t)

	// Well past the 64 KiB ceiling, in a field the handler would otherwise
	// happily decode.
	huge := strings.Repeat("a", 256<<10)
	resp := h.login(huge+"@example.com", "password-oversize-1")
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	// Deliberately the SAME answer a malformed body gets: telling an anonymous
	// caller where the ceiling is only helps them sit just under it.
	assert.Contains(t, readBody(t, resp), "bad_request")

	// A normal body still works on the same connection, so the cap is a per
	// request bound and not a connection the server gave up on.
	requireStatus(t, h.register("sized@example.com", "password-oversize-1", "Sized"), http.StatusOK)
}

// --- 1. Email-subject case ---

func TestEmailCaseNormalization(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	const password = "password-case-1"

	// Register with mixed case, verify, then login with a different casing.
	requireStatus(t, h.register("Foo@Example.com", password, "CaseUser"), http.StatusOK)
	requireStatus(t, h.verifyLatest(), http.StatusOK)
	requireStatus(t, h.login("fOO@EXAMPLE.com", password), http.StatusOK)

	// A re-register under yet another casing is the anti-enumeration no-op: no
	// second identity, no second email.
	requireStatus(t, h.register("FOO@EXAMPLE.COM", password, "CaseUser2"), http.StatusOK)
	require.Equal(t, 1, h.mailer.count(), "no verification mail for a taken address")

	var n int
	require.NoError(t, h.pool.QueryRow(ctx, `SELECT count(*) FROM auth_identities`).Scan(&n))
	assert.Equal(t, 1, n, "exactly one identity regardless of casing")

	var subject string
	require.NoError(t, h.pool.QueryRow(ctx,
		`SELECT provider_subject FROM auth_identities WHERE provider = 'email'`).Scan(&subject))
	assert.Equal(t, "foo@example.com", subject, "provider_subject stored lower-cased")

	// The CHECK constraint is the backstop: a mixed-case provider_subject for
	// provider='email' is rejected by the schema itself.
	_, err := h.pool.Exec(ctx, `
		INSERT INTO auth_identities (user_id, provider, provider_subject, email)
		SELECT id, 'email', 'Mixed@Case.com', 'mixed@case.com' FROM users LIMIT 1`)
	require.ErrorContains(t, err, "email_subject_lowercase")
}

// --- 2. No-auto-link exclusion constraint ---

func TestVerifiedEmailSingleOwner(t *testing.T) {
	fp := newFakeProvider(t)
	h := newHarness(t, withProviders(fp.creds()))
	ctx := context.Background()
	store := pgstore.New(h.pool)

	const email, password = "owner@example.com", "password-owner-1"

	// LEGAL: one user holding several verified identities with the same email
	// (email identity + explicitly linked Google) must keep working.
	h.registerVerifyLogin(email, password, "OwnerOne")
	fp.set("google-owner", email, true)

	resp := h.post(authBase+"/link/google/start", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	authorizeURL := decodeInto[struct {
		AuthorizeURL string `json:"authorizeUrl"`
	}](t, resp).AuthorizeURL
	loc := h.oauthCallback(t, auth.ProviderGoogle, stateFromURL(t, authorizeURL))
	assert.Contains(t, loc, "linked=google")

	var n int
	require.NoError(t, h.pool.QueryRow(ctx,
		`SELECT count(*) FROM auth_identities WHERE email = $1 AND email_verified`, email).Scan(&n))
	assert.Equal(t, 2, n, "same user may hold two verified identities with one email")

	// ORDER 1 (verify-then-insert): the email is already verified for OwnerOne;
	// a direct insert for a DIFFERENT user — bypassing every application
	// pre-check, as a concurrent request would — is rejected by the DB.
	other, _, err := store.CreateOAuthAccount(ctx, auth.OAuthAccountParams{
		DisplayName:   "OtherUser",
		Provider:      auth.ProviderGitHub,
		Subject:       "gh-other",
		Email:         "other@example.com",
		EmailVerified: true,
	})
	require.NoError(t, err)
	_, err = store.LinkIdentity(ctx, other.ID, auth.IdentityParams{
		Provider:      auth.ProviderGoogle,
		Subject:       "google-other",
		Email:         email,
		EmailVerified: true,
	})
	require.ErrorIs(t, err, auth.ErrEmailOwnedByOtherUser)

	// A second sequential insert attempt (fresh subject, same email) fails the
	// same way: the constraint, not the lookup, is what holds.
	_, err = store.LinkIdentity(ctx, other.ID, auth.IdentityParams{
		Provider:      auth.ProviderGoogle,
		Subject:       "google-other-2",
		Email:         email,
		EmailVerified: true,
	})
	require.ErrorIs(t, err, auth.ErrEmailOwnedByOtherUser)

	// ORDER 2 (insert-then-verify): an unverified email registration slips in
	// first, an OAuth account then verifies the same address, and the email
	// user's verify — the UPDATE — is rejected and surfaced as
	// account_exists_use_linking.
	const raced = "raced@example.com"
	requireStatus(t, h.register(raced, "password-raced-1", "RacedUser"), http.StatusOK)
	_, _, err = store.CreateOAuthAccount(ctx, auth.OAuthAccountParams{
		DisplayName:   "RacerOauth",
		Provider:      auth.ProviderGitHub,
		Subject:       "gh-racer",
		Email:         raced,
		EmailVerified: true,
	})
	require.NoError(t, err)

	verifyResp := h.verifyLatest()
	require.Equal(t, http.StatusConflict, verifyResp.StatusCode)
	assert.Equal(t, "account_exists_use_linking", decodeInto[errResponse](t, verifyResp).Error)

	var racedVerified bool
	require.NoError(t, h.pool.QueryRow(ctx,
		`SELECT email_verified FROM auth_identities WHERE provider = 'email' AND provider_subject = $1`,
		raced).Scan(&racedVerified))
	assert.False(t, racedVerified, "the losing identity stays unverified")
}

// --- 3. Expiry janitor ---

func TestJanitorSweep(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	store := pgstore.New(h.pool)

	var userID string
	require.NoError(t, h.pool.QueryRow(ctx,
		`INSERT INTO users (display_name) VALUES ('JanitorGuy') RETURNING id::text`).Scan(&userID))

	// Sessions: one expired (swept), one live (kept).
	mustExec(t, h.pool, `INSERT INTO sessions (token_hash, user_id, expires_at)
		VALUES ($1, $2, now() - interval '1 minute')`, []byte("hash-expired"), userID)
	mustExec(t, h.pool, `INSERT INTO sessions (token_hash, user_id, expires_at)
		VALUES ($1, $2, now() + interval '1 hour')`, []byte("hash-live"), userID)

	// Email tokens: swept = expired >24h ago, used >24h ago.
	mustExec(t, h.pool, `INSERT INTO email_tokens (user_id, purpose, token_hash, expires_at)
		VALUES ($1, 'verify', $2, now() - interval '25 hours')`, userID, []byte("tok-expired-old"))
	mustExec(t, h.pool, `INSERT INTO email_tokens (user_id, purpose, token_hash, expires_at, used_at)
		VALUES ($1, 'verify', $2, now() + interval '1 hour', now() - interval '25 hours')`, userID, []byte("tok-used-old"))
	// Kept: within the 24h grace (recently expired / recently used) or active.
	mustExec(t, h.pool, `INSERT INTO email_tokens (user_id, purpose, token_hash, expires_at)
		VALUES ($1, 'verify', $2, now() - interval '1 hour')`, userID, []byte("tok-expired-recent"))
	mustExec(t, h.pool, `INSERT INTO email_tokens (user_id, purpose, token_hash, expires_at, used_at)
		VALUES ($1, 'reset', $2, now() + interval '1 hour', now() - interval '1 hour')`, userID, []byte("tok-used-recent"))
	mustExec(t, h.pool, `INSERT INTO email_tokens (user_id, purpose, token_hash, expires_at)
		VALUES ($1, 'verify', $2, now() + interval '24 hours')`, userID, []byte("tok-active"))

	nSessions, err := store.DeleteExpiredSessions(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(1), nSessions)

	nTokens, err := store.DeleteStaleEmailTokens(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(2), nTokens)

	var remainingSessions, remainingTokens int
	require.NoError(t, h.pool.QueryRow(ctx, `SELECT count(*) FROM sessions`).Scan(&remainingSessions))
	require.NoError(t, h.pool.QueryRow(ctx, `SELECT count(*) FROM email_tokens`).Scan(&remainingTokens))
	assert.Equal(t, 1, remainingSessions, "live session survives")
	assert.Equal(t, 3, remainingTokens, "grace-period and active tokens survive")

	// RunJanitor honors context cancellation.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	done := make(chan struct{})
	go func() {
		auth.RunJanitor(cctx, store, time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunJanitor did not stop on context cancellation")
	}
}

// --- 4. Display-name uniqueness ---

func TestDisplayNameUniqueness(t *testing.T) {
	h := newHarness(t)

	requireStatus(t, h.register("egor1@example.com", "password-name-1", "Egor"), http.StatusOK)

	// Case-insensitive collision: 'egor' collides with 'Egor'.
	resp := h.register("egor2@example.com", "password-name-2", "egor")
	require.Equal(t, http.StatusConflict, resp.StatusCode)
	assert.Equal(t, "name_taken", decodeInto[errResponse](t, resp).Error)

	// Invalid names are rejected before touching the database.
	for _, bad := range []string{"ab", "no spaces", "nope!", strings.Repeat("a", 21)} {
		resp := h.register("invalid@example.com", "password-name-3", bad)
		require.Equal(t, http.StatusBadRequest, resp.StatusCode, "name %q", bad)
		assert.Equal(t, "bad_request", decodeInto[errResponse](t, resp).Error)
	}

	require.Equal(t, 1, h.mailer.count(), "only the successful register sent mail")
}

func TestOAuthDisplayNameSuffix(t *testing.T) {
	fp := newFakeProvider(t)
	h := newHarness(t, withProviders(fp.creds()))

	// Occupy the sanitized profile name: "OAuth User" -> "OAuthUser".
	requireStatus(t, h.register("squatter@example.com", "password-squat-1", "OAuthUser"), http.StatusOK)

	fp.set("google-suffix-1", "suffix1@example.com", true)
	loc := h.oauthLogin(t, auth.ProviderGoogle)
	assert.Contains(t, loc, "status=ok")
	assert.Equal(t, "OAuthUser1", decodeInto[meResponse](t, h.get(mePath)).DisplayName)

	fp.set("google-suffix-2", "suffix2@example.com", true)
	loc = h.oauthLogin(t, auth.ProviderGoogle)
	assert.Contains(t, loc, "status=ok")
	assert.Equal(t, "OAuthUser2", decodeInto[meResponse](t, h.get(mePath)).DisplayName)
}
