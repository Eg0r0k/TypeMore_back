// Package runs is the run-ingestion domain: it accepts finished typing tests
// (event log + client-reported preview numbers), validates them STRUCTURALLY
// only, stores the log as an immutable gzip blob, and lists/returns a user's own
// runs. See BACKEND.md §3–4 and docs/RUNS.md.
//
// # What this phase does NOT do (deliberately deferred to the replay-worker phase)
//
// Validation here is structural and fast — no game knowledge. It never replays
// the log, never recomputes metrics or score, never touches a dictionary, and
// never moves a run past 'pending'. Deep semantics (goja replay, score
// recomputation, plausibility/anti-cheat, dict-hash registry lookup, status
// transitions, leaderboards) belong to internal/replay and are out of scope.
// Every run lands 'pending'; the client keeps showing its own preview result
// because nothing here is authoritative yet.
//
// # Layering
//
// Like the other domains, runs declares its dependencies as consumer-side
// interfaces (Store, RateLimiter) plus a UserIDFunc for reading the
// authenticated principal from the request context, and imports no sibling
// domain. The composition root wires the Postgres adapter, the shared auth
// rate-limiter machinery, and the auth session middleware.
package runs
