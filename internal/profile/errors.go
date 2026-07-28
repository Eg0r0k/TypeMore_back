package profile

import "net/http"

// apiError mirrors the auth/runs/leaderboard domains' wire shape so the error
// surface is uniform across the API; kept private so each domain owns its own.
type apiError struct {
	status  int
	Code    string `json:"error"`
	Message string `json:"message"`
}

func (e *apiError) Error() string { return e.Code + ": " + e.Message }

func newAPIError(status int, code, message string) *apiError {
	return &apiError{status: status, Code: code, Message: message}
}

func apiErrBadRequest(message string) *apiError {
	return newAPIError(http.StatusBadRequest, "bad_request", message)
}

var (
	apiErrUnauthorized = newAPIError(http.StatusUnauthorized, "unauthorized",
		"authentication required")
	apiErrInternal = newAPIError(http.StatusInternalServerError, "internal",
		"an unexpected error occurred")
)
