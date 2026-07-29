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
//
// # Correctness here, judgement next door
//
// This package decides whether a run is REAL: the words regenerate from the
// seed, the log folds through the core, the score and the metrics match the
// client's, the structure holds. Every one of those refusals is made here and
// made unconditionally.
//
// Whether a valid run is SUSPICIOUS is a different question, and it lives behind
// policy.Judge (internal/replay/policy). The worker builds a Decider around one
// and consults it in exactly one place — after all the hard checks have passed.
// The open default judges nothing, and an instance running it is still correct
// about every run; it simply routes none of them to review.
//
// TestHardVerdictsDoNotDependOnTheJudge is what keeps that separation honest: it
// runs the tamper matrix against judges from "review nothing, ever" to "review
// everything" and requires identical verdicts. See docs/SELF_HOST.md.
package replay
