package auth

import (
	"errors"
	"net/http"
)

// ErrNotFound is returned by Store/SessionStore when a lookup matches no row.
// Service translates it into the appropriate client-facing outcome (often a
// generic failure, to avoid leaking existence — see the anti-enumeration notes).
var ErrNotFound = errors.New("auth: not found")

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
)

// apiErrBadRequest is a helper for input-validation failures with a custom
// message.
func apiErrBadRequest(message string) *apiError {
	return newAPIError(http.StatusBadRequest, "bad_request", message)
}
