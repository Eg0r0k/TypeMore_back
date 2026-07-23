package auth

import (
	"errors"
	"net/http"
)

// ErrNotFound is returned by Store/SessionStore when a lookup matches no row.
// Service translates it into the appropriate client-facing outcome (often a
// generic failure, to avoid leaking existence — see the anti-enumeration notes).
var ErrNotFound = errors.New("auth: not found")

// ErrDisplayNameTaken is returned by Store writes that collide with the
// case-insensitive unique display_name (users_display_name_key).
var ErrDisplayNameTaken = errors.New("auth: display name taken")

// ErrEmailOwnedByOtherUser is returned by Store writes that would leave the
// same email verified under two different users (the verified_email_one_user
// exclusion constraint). It is the DB-level backstop for the no-auto-link
// policy; handlers surface it as account_exists_use_linking.
var ErrEmailOwnedByOtherUser = errors.New("auth: verified email belongs to another user")

// ErrIdentityExists is returned by Store writes that collide with the
// unique(provider, provider_subject) constraint — the (provider, subject) pair
// already identifies some account. Handlers surface it as
// account_exists_use_linking (an email whose 'email' identity already exists).
var ErrIdentityExists = errors.New("auth: identity already exists")

// errInvalidToken is an internal sentinel for a malformed token string; it is
// surfaced to clients as apiErrInvalidToken.
var errInvalidToken = errors.New("auth: invalid token")

// apiError is a client-facing error carrying an HTTP status, a stable machine
// code (JSON "error"), and a human message (JSON "message"). Handlers render it
// directly; anything that is not an apiError becomes a generic 500.
type apiError struct {
	status  int
	Code    string `json:"error"`
	Message string `json:"message"`
}

func (e *apiError) Error() string { return e.Code + ": " + e.Message }

// newAPIError builds an apiError.
func newAPIError(status int, code, message string) *apiError {
	return &apiError{status: status, Code: code, Message: message}
}

// Client-facing errors shared across handlers. Constructors are used where the
// message varies; these vars cover the fixed cases.
var (
	apiErrInvalidToken = newAPIError(http.StatusBadRequest, "invalid_token",
		"the token is invalid, expired, or already used")
	apiErrInvalidCredentials = newAPIError(http.StatusUnauthorized, "invalid_credentials",
		"email or password is incorrect")
	apiErrEmailNotVerified = newAPIError(http.StatusForbidden, "email_not_verified",
		"verify your email address before signing in")
	apiErrRateLimited = newAPIError(http.StatusTooManyRequests, "rate_limited",
		"too many requests; slow down and try again shortly")
	apiErrUnauthorized = newAPIError(http.StatusUnauthorized, "unauthorized",
		"authentication required")
	apiErrForbiddenOrigin = newAPIError(http.StatusForbidden, "forbidden_origin",
		"request origin is not allowed")
	apiErrProviderUnknown = newAPIError(http.StatusNotFound, "unknown_provider",
		"unknown or unconfigured OAuth provider")
	apiErrInternal = newAPIError(http.StatusInternalServerError, "internal",
		"an unexpected error occurred")
	apiErrNameTaken = newAPIError(http.StatusConflict, "name_taken",
		"that display name is already in use")
	apiErrAccountExistsUseLinking = newAPIError(http.StatusConflict, "account_exists_use_linking",
		"this email already belongs to another account; sign in there and link providers explicitly")
	apiErrEmailAlreadySet = newAPIError(http.StatusConflict, "email_already_set",
		"this account already has an email identity")
	apiErrNoVerifiedEmail = newAPIError(http.StatusConflict, "no_verified_email",
		"add and verify an email address before setting a password")
	apiErrPasswordAlreadySet = newAPIError(http.StatusConflict, "password_already_set",
		"a password is already set; use the reset flow to change it")
)

// apiErrBadRequest is a helper for input-validation failures with a custom
// message.
func apiErrBadRequest(message string) *apiError {
	return newAPIError(http.StatusBadRequest, "bad_request", message)
}
