package platform

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// healthResponse is the JSON body returned by the health endpoint: a fixed
// status plus this binary's build metadata, so an operator hitting /healthz can
// confirm both liveness and exactly which build is running.
type healthResponse struct {
	Status string    `json:"status"`
	Build  BuildInfo `json:"build"`
}

// HealthHandler serves the liveness endpoint: it always returns 200 with build
// info and checks no dependencies. Use it for "is the process alive?"; use
// ReadyHandler for "can it serve traffic?".
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

// Pinger is the minimal dependency-health contract ReadyHandler needs. A
// *pgxpool.Pool satisfies it, but the interface keeps platform from importing
// any concrete store.
type Pinger interface {
	Ping(ctx context.Context) error
}

// readyResponse is the JSON body of the readiness endpoint.
type readyResponse struct {
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// ReadyHandler serves the readiness endpoint: it pings the database and returns
// 200 only if the ping succeeds, otherwise 503. Load balancers and
// orchestrators use it to decide whether to route traffic to this instance.
func ReadyHandler(db Pinger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if err := db.Ping(ctx); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(readyResponse{Status: "unavailable", Error: "database unreachable"})
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(readyResponse{Status: "ready"})
	}
}
