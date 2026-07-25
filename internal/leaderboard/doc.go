// Package leaderboard is the bucketed score-board domain: which run holds which
// slot, and how a reader pages through a ranking. See SCORING_CONCEPT §4 and
// docs/LEADERBOARDS.md.
//
// # A projection, not a source of truth
//
// A board row is a snapshot of an accepted run, and every row can be recomputed
// from the runs table alone (`make rebuild-leaderboards`). Maintenance runs in
// the SAME transaction that writes a run's status, so "accepted" and "on the
// board" are one atomic fact and there is no window where they disagree.
//
// The operation is always "recompute this (player, bucket) cell", never "insert
// if better". Promotion, demotion and re-judging are then the same code path,
// which is what keeps demotion — the case that rots quietly — correct without a
// second implementation to keep in step.
//
// # Where the rules live
//
// One bucket key format, one eligibility definition, one visibility definition:
//
//   - Bucket.Key formats "<mode>:<dimension>:<lang>:<textSource>" and is the
//     only producer of that string anywhere in the system. SQL matches sibling
//     runs on the bucket's components instead of parsing keys.
//   - leaderboard_eligible_runs (schema) decides which runs may hold a slot.
//     The projection and the rebuild both read it.
//   - leaderboard_rows (schema) decides what a reader may see, and is the only
//     way a query can reach the entries table — which is how ban filtering
//     stays impossible to forget on a new endpoint.
//
// # Layering
//
// Like the other domains, leaderboard declares its dependencies as
// consumer-side interfaces (Store) plus a UserIDFunc for the one authenticated
// route, and imports no sibling domain. The replay worker reaches the write
// side through an interface declared at ITS end (replay/pgstore.Projector),
// wired by the composition root; neither domain imports the other.
package leaderboard
