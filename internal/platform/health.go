package platform

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// ReviewPolicy is what /healthz says about this instance's anti-cheat.
//
// It is here, on the liveness endpoint, because the failure mode being guarded
// against is an instance running without a review policy and nobody noticing.
// The startup WARN covers the operator who reads boot logs; this covers everyone
// else — a monitor, a deploy check, or the person who inherited the instance.
//
// It exposes whether a policy is running and which version, and NOTHING about
// what it decides: no weights, no threshold, no rule names. "Is anti-cheat on"
// is an operational fact; "here is how it works" is the answer key.
type ReviewPolicy struct {
	// Enabled is false when runs are judged for correctness only.
	Enabled bool `json:"enabled"`
	// Version is the judge's own version string — "none" when disabled. It is
	// the same value stored on every run this instance judges.
	Version string `json:"version"`
	// Warning states the consequence in words, present only when disabled. A
	// boolean is easy to skim past; a sentence is not.
	Warning string `json:"warning,omitempty"`
}

// healthResponse is the JSON body returned by the health endpoint: a fixed
// status plus this binary's build metadata, so an operator hitting /healthz can
// confirm both liveness and exactly which build is running.
type healthResponse struct {
	Status string    `json:"status"`
	Build  BuildInfo `json:"build"`
	// ReviewPolicy answers "is this instance judging runs, or only validating
	// them?" — see the type.
	ReviewPolicy ReviewPolicy `json:"reviewPolicy"`
}

// noPolicyWarning is the sentence /healthz returns when nothing is judging. It
// says what does not happen rather than only that something is off, because the
// reader deciding whether it matters is often not the person who turned it off.
const noPolicyWarning = "review policy disabled: runs are judged for correctness only. " +
	"No suspicion is computed and no run reaches the review queue, so this " +
	"instance's leaderboards are not protected. See docs/SELF_HOST.md."

// HealthHandler serves the liveness endpoint: it always returns 200 with build
// info and checks no dependencies. Use it for "is the process alive?"; use
// ReadyHandler for "can it serve traffic?".
//
// It returns 200 even with the review policy disabled: a self-hosted instance
// without anti-cheat is a supported deployment, not an unhealthy one. It just
// has to say so.
func HealthHandler(review ReviewPolicy) http.HandlerFunc {
	if !review.Enabled {
		review.Warning = noPolicyWarning
	}
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		// Encoding a small fixed struct to a fresh ResponseWriter does not fail
		// in practice; ignore the error rather than complicate the signature.
		_ = json.NewEncoder(w).Encode(healthResponse{
			Status:       "ok",
			Build:        CurrentBuild(),
			ReviewPolicy: review,
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
