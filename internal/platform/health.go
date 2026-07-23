package platform

import (
	"encoding/json"
	"net/http"
)

// healthResponse is the JSON body returned by the health endpoint: a fixed
// status plus this binary's build metadata, so an operator hitting /healthz can
// confirm both liveness and exactly which build is running.
type healthResponse struct {
	Status string    `json:"status"`
	Build  BuildInfo `json:"build"`
}

// HealthHandler serves the liveness/build-info endpoint. It always returns 200
// with a small JSON body; it performs no dependency checks because this phase
// has no dependencies (no DB/Redis) to check.
func HealthHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		// Encoding a small fixed struct to a fresh ResponseWriter does not fail
		// in practice; ignore the error rather than complicate the signature.
		_ = json.NewEncoder(w).Encode(healthResponse{
			Status: "ok",
			Build:  CurrentBuild(),
		})
	}
}
