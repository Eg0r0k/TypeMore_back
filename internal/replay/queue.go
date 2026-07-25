package replay

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
)

// Run status values, mirroring the runs.status CHECK constraint. The replay
// worker owns every transition out of 'pending'.
const (
	StatusPending  = "pending"
	StatusAccepted = "accepted"
	StatusFlagged  = "flagged"
	StatusRejected = "rejected"
)

// PendingRun is one claimed run, exactly as the worker needs it. The jsonb
// snapshots are the client's own bytes: the server parses only what it must
// (the setup's three sub-objects) and forwards the rest to the core untouched.
type PendingRun struct {
	ID            uuid.UUID
	Seed          int64
	DictHash      string
	ScoreVersion  int16
	Setup         json.RawMessage
	ClientMetrics json.RawMessage
	ClientScore   json.RawMessage
	// Log is the gzip-compressed EventLog JSON as stored.
	Log []byte
	// Attempts is how many times replay has already failed on this run.
	Attempts int16
}

// Decision is the worker's verdict for one run: the new status plus everything
// the audit trail needs. Written in the same transaction the run was claimed in.
type Decision struct {
	Status string
	// ServerMetrics / ServerScore are the core's own JSON, never re-encoded by
	// Go. Nil when the run could not be replayed or its log was invalid.
	ServerMetrics json.RawMessage
	ServerScore   json.RawMessage
	// Validation is the {verdict, reason, flags[]} report.
	Validation json.RawMessage
	// BundleSHA identifies the core that produced the numbers.
	BundleSHA string
	// Attempts is the new attempt count (incremented only by a failed replay).
	Attempts int16
	// LastError is the operator-facing failure detail; empty clears the column.
	LastError string
}

// Queue is the worker's persistence contract, declared here at the consumer.
//
// ProcessBatch is deliberately a unit of work rather than claim/apply calls:
// the claim, every decision, and the commit belong to ONE transaction, and that
// transaction is what makes the queue safe. Holding the row locks for the length
// of the batch means a crashed worker rolls back to 'pending' with no
// 'processing' state to reconcile, and a second worker's FOR UPDATE SKIP LOCKED
// simply steps over the locked rows. See docs/REPLAY.md.
type Queue interface {
	// ProcessBatch claims up to limit pending runs (oldest first), calls decide
	// for each, applies what it returns, and commits. It returns the number of
	// runs claimed — zero means the queue is empty. An error from decide is
	// impossible by construction: decide is total.
	ProcessBatch(ctx context.Context, limit int32, decide func(context.Context, PendingRun) Decision) (int, error)
}
