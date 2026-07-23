// Package auth is the authentication domain: email/password and OAuth
// (GitHub, Google) sign-in, opaque server-side sessions, email verification, and
// password reset.
//
// # Layering
//
// Following BACKEND.md §2, this package depends on infrastructure only through
// interfaces it declares itself (Store, SessionStore, Mailer, RateLimiter). The
// concrete PostgreSQL/SMTP implementations are wired in at the composition root
// (cmd/server). HTTP handlers here are thin: they parse input, call Service, and
// render the result; all rules live in Service.
//
// # Deliberate deviation from BACKEND.md
//
// BACKEND.md §6 puts sessions in Redis. This phase keeps them in Postgres: one
// store is less operational surface than standing up Redis for a single table,
// and Redis arrives with leaderboards where it is genuinely needed. Session
// storage lives behind the SessionStore interface precisely so that swap is
// mechanical — implement SessionStore against Redis and change one line in
// cmd/server.
//
// # Security posture (summary)
//
//   - Passwords: argon2id, parameters stored in the hash (PHC string) so they
//     can be raised later without breaking existing hashes. See password.go.
//   - Sessions: 256-bit opaque tokens; only their SHA-256 hash is persisted.
//   - Anti-enumeration: registering a taken email and requesting a reset for an
//     unknown email return the SAME success as the happy path, with the password
//     hashing work done regardless to keep timing uniform.
//   - OAuth: no silent account linking on matching email (see oauth.go).
//   - CSRF: mutating endpoints require the browser Origin to match the frontend.
package auth
