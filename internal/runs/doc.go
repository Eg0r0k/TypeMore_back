// Package runs is the run-ingestion domain: it accepts finished typing tests
// (event log + client-reported preview numbers), validates them STRUCTURALLY
// only, stores the log as an immutable gzip blob, and lists/returns a user's own
// runs. See BACKEND.md §3–4 and docs/RUNS.md.
//
// # What this package does NOT do (it is internal/replay's job)
//
// Validation here is structural and fast — no game knowledge. It never replays
// the log, never recomputes metrics or score, never touches a dictionary, and
// never moves a run past 'pending'. Deep semantics — goja replay, score
// recomputation, plausibility flags, dict-hash resolution, and the transition
// to accepted/flagged/rejected — belong to the replay worker (internal/replay,
// docs/REPLAY.md), which writes the verdict columns this package then exposes
// read-only on the summary endpoints. Every run lands 'pending'; the client
// keeps showing its own preview result because nothing here is authoritative.
//
// # Layering
//
// Like the other domains, runs declares its dependencies as consumer-side
// interfaces (Store, RateLimiter) plus a UserIDFunc for reading the
// authenticated principal from the request context, and imports no sibling
// domain. The composition root wires the Postgres adapter, the shared auth
// rate-limiter machinery, and the auth session middleware.
package runs
