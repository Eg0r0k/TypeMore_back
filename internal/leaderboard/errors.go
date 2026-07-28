package leaderboard

import "net/http"

// apiError is a client-facing error carrying an HTTP status, a stable machine
// code (JSON "error"), and a human message (JSON "message"). Mirrors the auth
// and runs domains' shape so the wire format is uniform across the API; kept
// private here so each domain owns its own error surface.
type apiError struct {
	status  int
	Code    string `json:"error"`
	Message string `json:"message"`
}

func (e *apiError) Error() string { return e.Code + ": " + e.Message }

func newAPIError(status int, code, message string) *apiError {
	return &apiError{status: status, Code: code, Message: message}
}

var (
	// A bucket key that cannot name a board is not an error the caller can fix
	// by retrying — and it must not be distinguishable from an empty board's
	// sibling, so it is a plain 404.
	apiErrUnknownBucket = newAPIError(http.StatusNotFound, "unknown_bucket",
		"no such leaderboard")
	apiErrBadCursor = newAPIError(http.StatusBadRequest, "bad_request",
		"invalid cursor")
	// `cursor`, `before` and `around` name mutually exclusive positions in the
	// ranking; a request carrying two is asking for two different pages.
	apiErrConflictingAsks = newAPIError(http.StatusBadRequest, "bad_request",
		"cursor, before and around are mutually exclusive")
	// The only supported window anchor is `me`.
	apiErrBadAround = newAPIError(http.StatusBadRequest, "bad_request",
		"around must be \"me\"")
	apiErrUnauthorized = newAPIError(http.StatusUnauthorized, "unauthorized",
		"authentication required")
	apiErrInternal = newAPIError(http.StatusInternalServerError, "internal",
		"an unexpected error occurred")
)
