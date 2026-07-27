package runs

import (
	"errors"
	"net/http"
)

// ErrNotFound is returned by Store when a run does not exist or is not owned by
// the requesting user. The two cases are deliberately indistinguishable: a run
// you do not own must look exactly like one that does not exist.
var ErrNotFound = errors.New("runs: not found")

// apiError is a client-facing error carrying an HTTP status, a stable machine
// code (JSON "error"), and a human message (JSON "message"). Mirrors the auth
// domain's shape so the wire format is uniform across the API; kept private here
// so each domain owns its own error surface.
type apiError struct {
	status  int
	Code    string `json:"error"`
	Message string `json:"message"`
}

func (e *apiError) Error() string { return e.Code + ": " + e.Message }

func newAPIError(status int, code, message string) *apiError {
	return &apiError{status: status, Code: code, Message: message}
}

// Fixed client-facing errors. Structural-validation failures (422) use distinct
// codes so the client can tell exactly which rule tripped; see validate.go.
var (
	apiErrUnauthorized = newAPIError(http.StatusUnauthorized, "unauthorized",
		"authentication required")
	apiErrRateLimited = newAPIError(http.StatusTooManyRequests, "rate_limited",
		"too many runs submitted; slow down and try again shortly")
	apiErrPayloadTooLarge = newAPIError(http.StatusRequestEntityTooLarge, "payload_too_large",
		"run payload exceeds the 6.5 MiB limit")
	apiErrNotFound = newAPIError(http.StatusNotFound, "not_found",
		"run not found")
	// apiErrRestricted is what a banned account gets when it submits a run.
	//
	// 403 and an honest message, not a silent 202 that drops the run: a
	// shadow-ban that pretends to accept is a player typing into a void and
	// wondering why their board rank never moves. The message says the run was
	// not counted and says nothing about WHY the account is restricted — the
	// reason is an internal moderation note and never leaves the database
	// (docs/MODERATION.md).
	apiErrRestricted = newAPIError(http.StatusForbidden, "account_restricted",
		"this account is restricted; the run was not counted")
	apiErrInternal = newAPIError(http.StatusInternalServerError, "internal",
		"an unexpected error occurred")
)

// apiErrBadRequest is a 400 for malformed input with a custom message.
func apiErrBadRequest(message string) *apiError {
	return newAPIError(http.StatusBadRequest, "bad_request", message)
}

// apiErrUnprocessable is a 422 for a request that is well-formed JSON but fails
// a structural validation rule (code is one of the validate.go constants).
func apiErrUnprocessable(code, message string) *apiError {
	return newAPIError(http.StatusUnprocessableEntity, code, message)
}
