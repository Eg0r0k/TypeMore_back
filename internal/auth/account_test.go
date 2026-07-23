package auth_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/typemore/typemore-server/internal/auth"
)

// oauthOnlyLogin creates (or logs into) an OAuth account with NO email identity
// by driving the fake provider with a withheld email, leaving a live session in
// the jar.
func (h *harness) oauthOnlyLogin(t *testing.T, fp *fakeProvider, sub string) {
	t.Helper()
	fp.set(sub, "", false) // email withheld → only the provider identity exists
	loc := h.oauthLogin(t, auth.ProviderGoogle)
	require.Contains(t, loc, "status=ok")
}

// --- add email + set password: OAuth-only account gains email login ---

func TestAddEmailThenSetPasswordEndToEnd(t *testing.T) {
	fp := newFakeProvider(t)
	h := newHarness(t, withProviders(fp.creds()))

	h.oauthOnlyLogin(t, fp, "oauth-only-1")

	const email, password = "added@example.com", "brand-new-pass-1"

	// Add the email → verification sent, generic success.
	requireStatus(t, h.post(authBase+"/email/add", map[string]string{"email": email}), http.StatusOK)
	require.Equal(t, 1, h.mailer.count(), "add-email must send a verification link")

	// Verify → the email identity becomes verified.
	requireStatus(t, h.verifyLatest(), http.StatusOK)

	// Set a first-time password.
	requireStatus(t, h.post(authBase+"/password/set", map[string]string{"password": password}), http.StatusOK)

	// Now email+password login works from a clean session.
	requireStatus(t, h.post(authBase+"/logout", nil), http.StatusOK)
	requireStatus(t, h.get(mePath), http.StatusUnauthorized)
	requireStatus(t, h.login(email, password), http.StatusOK)
	requireStatus(t, h.get(mePath), http.StatusOK)
}

// --- set password rejected when a credential already exists ---

func TestSetPasswordRejectedWhenCredentialsExist(t *testing.T) {
	h := newHarness(t)
	const email, password = "haspw@example.com", "existing-pass-1"
	h.registerVerifyLogin(email, password, "HasPw")

	resp := h.post(authBase+"/password/set", map[string]string{"password": "another-pass-2"})
	require.Equal(t, http.StatusConflict, resp.StatusCode)
	assert.Equal(t, "password_already_set", decodeInto[errResponse](t, resp).Error)
}

// --- set password requires a verified email identity ---

func TestSetPasswordRequiresVerifiedEmail(t *testing.T) {
	fp := newFakeProvider(t)
	h := newHarness(t, withProviders(fp.creds()))

	h.oauthOnlyLogin(t, fp, "oauth-only-2")

	// No email at all → rejected.
	resp := h.post(authBase+"/password/set", map[string]string{"password": "cannot-set-1"})
	require.Equal(t, http.StatusConflict, resp.StatusCode)
	assert.Equal(t, "no_verified_email", decodeInto[errResponse](t, resp).Error)

	// Add an email but do NOT verify it → still rejected (identity unverified).
	requireStatus(t, h.post(authBase+"/email/add", map[string]string{"email": "unverified@example.com"}), http.StatusOK)
	resp = h.post(authBase+"/password/set", map[string]string{"password": "cannot-set-1"})
	require.Equal(t, http.StatusConflict, resp.StatusCode)
	assert.Equal(t, "no_verified_email", decodeInto[errResponse](t, resp).Error)
}

// --- add email collides with another account's verified email ---

func TestAddEmailCollision(t *testing.T) {
	fp := newFakeProvider(t)
	h := newHarness(t, withProviders(fp.creds()))

	// Account B owns a verified email.
	const taken = "taken@example.com"
	h.registerVerifyLogin(taken, "owner-pass-1", "Owner")
	requireStatus(t, h.post(authBase+"/logout", nil), http.StatusOK)

	// OAuth-only account A tries to add the same email.
	h.oauthOnlyLogin(t, fp, "oauth-only-3")
	resp := h.post(authBase+"/email/add", map[string]string{"email": taken})
	require.Equal(t, http.StatusConflict, resp.StatusCode)
	assert.Equal(t, "account_exists_use_linking", decodeInto[errResponse](t, resp).Error)
}

// --- add email rejected when the account already has an email identity ---

func TestAddEmailAlreadySet(t *testing.T) {
	h := newHarness(t)
	const email, password = "already@example.com", "already-pass-1"
	h.registerVerifyLogin(email, password, "Already")

	resp := h.post(authBase+"/email/add", map[string]string{"email": "second@example.com"})
	require.Equal(t, http.StatusConflict, resp.StatusCode)
	assert.Equal(t, "email_already_set", decodeInto[errResponse](t, resp).Error)
}
