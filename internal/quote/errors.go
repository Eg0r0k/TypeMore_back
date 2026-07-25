package quote

import "net/http"

// apiError is a client-facing error carrying an HTTP status, a stable machine
// code (JSON "error"), and a human message (JSON "message"). Mirrors the auth,
// runs and leaderboard domains' shape so the wire format is uniform across the
// API; kept private here so each domain owns its own error surface.
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
	// An unknown id and a malformed one are the same answer on purpose: both
	// mean "this link resolves to no quote", and distinguishing them only tells
	// a scraper which of its guesses were well-formed.
	apiErrNotFound = newAPIError(http.StatusNotFound, "not_found",
		"no such quote")
	// A group name we do not know is REJECTED rather than ignored. Silently
	// dropping the filter would hand back long quotes to a client that asked
	// for short ones and look like a corpus bug for weeks.
	apiErrBadGroup = newAPIError(http.StatusBadRequest, "bad_request",
		"group must be one of short, medium, long, thicc")
	apiErrBadCursor = newAPIError(http.StatusBadRequest, "bad_request",
		"invalid cursor")
	apiErrInternal = newAPIError(http.StatusInternalServerError, "internal",
		"an unexpected error occurred")
)
