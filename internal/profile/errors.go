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
	// apiErrNotFound: an unknown display name. Deliberately a plain 404 rather
	// than an indistinguishable-from-closed answer — names are public on every
	// leaderboard already, so there is nothing to hide by blurring the two.
	apiErrNotFound = newAPIError(http.StatusNotFound, "not_found", "no such profile")
	// apiErrProfileClosed: the profile exists and its owner keeps it closed.
	// The page is real (the header answers 200), the data is refused — an
	// explicit state, not a 404 (docs/PROFILE.md, "Public profiles").
	apiErrProfileClosed = newAPIError(http.StatusForbidden, "profile_closed",
		"this profile is private")
	// apiErrPortraitClosed: the profile is open but the keyboard portrait's own
	// opt-in is off. A distinct code so the page can say "closed by its owner"
	// on the one section rather than misreporting the whole profile.
	apiErrPortraitClosed = newAPIError(http.StatusForbidden, "portrait_closed",
		"the keyboard portrait is private")
)
