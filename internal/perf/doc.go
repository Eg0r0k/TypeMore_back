// Package perf is the shared toolkit for the load and performance suites:
// realistic data generators, query-plan assertions, memory measurement, and the
// budgets each zone is held to. See docs/PERFORMANCE.md.
//
// # Why the generators live here rather than in each test
//
// A benchmark is only as honest as its data. Numbers produced against ten rows
// tell you nothing, and every zone that invents its own fixtures drifts into
// measuring a different shape of workload than the one production sees. One
// package, one definition of "a million realistic runs", reused by the
// leaderboard, projection and rebuild zones, keeps the four sets of numbers
// comparable — and means raising a volume is a single edit.
//
// Volumes are documented at each generator and chosen to be *uncomfortable*:
// a hot bucket larger than any board this game will plausibly have, a run at the
// exact ingestion ceiling, a match log at the relay cap.
//
// # Build tags
//
// This package itself is untagged (it is ordinary library code, compiled and
// vetted by the normal build). The suites that USE it are behind
// `//go:build load`, so `go test ./...` stays fast and `make load` runs the
// heavy work. Anything that merely takes a few seconds uses testing.Short()
// instead, so `go test -short` skips it without a tag.
package perf
