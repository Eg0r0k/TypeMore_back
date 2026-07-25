// Package replay owns the vendored TypeScript game core and everything derived
// from it. Today that is the dictionary registry (and the public HTTP surface
// that serves it); the goja replay worker — score/metric recomputation from a
// stored event log — lands on top of the same vendored bundle.
//
// # Why the bundle is vendored
//
// Anti-cheat and scoring stand on server-side replay, and the core (reducer,
// mulberry32, FNV-1a, metrics) is written in TypeScript. Rather than maintain a
// second implementation that will inevitably drift, the server executes the
// exact same compiled bundle inside goja. See ARCHITECTURE.md §4.2 and
// BACKEND.md §1.
//
// Consequence, and it is the rule of this package: nothing here reimplements a
// core algorithm in Go. The dictionary fingerprint is whatever
// TypeMoreCore.dictVersion returns — never a Go FNV-1a that "should" agree.
//
// # Dictionaries
//
// corejs/core.bundle.js and dicts/*.json are compiled into the binary with
// embed directives, so a deployed server has no runtime asset dependency. The
// registry is seeded once at startup: parse each dictionary, hash its word list
// through the bundle, pre-compress the body. Serving is then pure byte-slinging
// — no per-request marshalling, no per-request compression.
//
// Dictionary bodies are addressed by content hash and served immutable; see
// docs/DICTIONARIES.md for the caching contract and the immutability rule that
// keeps old runs replayable.
package replay
