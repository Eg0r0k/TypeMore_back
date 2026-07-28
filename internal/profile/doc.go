// Package profile serves a player's own statistics: the summary counters, the
// activity calendar, the wpm histogram, the daily time/speed series with its
// trend slope, and the personal-best cards (docs/PROFILE.md).
//
// Two decisions define the package:
//
//   - Everything is ON-DEMAND SQL over `runs` (and, for the PB cards, a read
//     of `leaderboard_entries`, which already IS the per-bucket best-run
//     store). Nothing is projected, nothing is cached — the profile is always
//     exactly what the runs table says, and the 100k-run perf suite pins every
//     query's plan to the per-user indexes instead of trusting that promise.
//
//   - Every route is session-scoped and answers about the CALLER only. Public
//     profiles are a later, deliberate flag — v1 has no handle for reading
//     another player's statistics at all, so privacy is the absence of a code
//     path rather than a check that could be forgotten. When public profiles
//     arrive, the keyboard heatmap stays private-by-default: per-key timing
//     aggregates are effectively biometric (docs/PROFILE.md, "Privacy").
package profile
