// Package runstatus is the single source of the run-status vocabulary — the
// values the runs.status column admits (its CHECK constraint) and every
// transition speaks. The runs domain writes the initial value, the replay
// worker owns every transition out of it; both re-export these constants under
// their historical names, so the two lists can never drift apart again.
package runstatus

// Status is a run's judgement state. It is an alias, not a defined type: the
// vocabulary is shared by the runs and replay domains, whose existing APIs —
// and the tests pinning them — speak plain strings on the wire and in SQL.
type Status = string

// The four states of runs.status. A run lands as Pending; the replay worker's
// decision moves it to exactly one of the other three.
const (
	Pending  Status = "pending"
	Accepted Status = "accepted"
	Flagged  Status = "flagged"
	Rejected Status = "rejected"
)
