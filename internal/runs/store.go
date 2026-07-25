package runs

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Run status values, mirroring the runs.status CHECK constraint. This phase only
// ever writes StatusPending; the replay worker owns every other transition.
const (
	StatusPending  = "pending"
	StatusAccepted = "accepted"
	StatusFlagged  = "flagged"
	StatusRejected = "rejected"
)

// CreateRunParams is the fully-validated input to Store.CreateRun. Log is the
// gzip-compressed EventLog JSON; LogBytes is its uncompressed size. Exactly one
// of DurationMs / WordCount is non-nil (enforced by validation and the schema).
type CreateRunParams struct {
	UserID        uuid.UUID
	Mode          string
	DurationMs    *int32
	WordCount     *int32
	Lang          string
	Seed          int64
	DictHash      string
	Setup         json.RawMessage
	ClientMetrics json.RawMessage
	ClientScore   json.RawMessage
	ScoreVersion  int16
	Log           []byte
	LogBytes      int32
}

// Run is the persisted outcome of CreateRun: the server-assigned id, the landed
// status (always StatusPending this phase), and the creation time.
type Run struct {
	ID        uuid.UUID
	Status    string
	CreatedAt time.Time
}

// Summary is one run WITHOUT its log payload — the shape returned by the list
// and detail endpoints. The opaque jsonb snapshots are passed through verbatim.
//
// The Server* / Validation / ValidatedAt fields are the replay worker's verdict
// (internal/replay, docs/REPLAY.md). They are nil until the run has been
// judged, which is exactly what the client shows as "pending".
type Summary struct {
	ID            uuid.UUID
	Mode          string
	DurationMs    *int32
	WordCount     *int32
	Lang          string
	Seed          int64
	DictHash      string
	Setup         json.RawMessage
	ClientMetrics json.RawMessage
	ClientScore   json.RawMessage
	ScoreVersion  int16
	Status        string
	ServerMetrics json.RawMessage
	ServerScore   json.RawMessage
	Validation    json.RawMessage
	ValidatedAt   *time.Time
	LogBytes      int32
	CreatedAt     time.Time
}

// Cursor is a keyset pagination position: the (CreatedAt, ID) of the last row a
// page returned. Listing continues with rows ordered strictly before it.
type Cursor struct {
	CreatedAt time.Time
	ID        uuid.UUID
}

// PublicReplay is one ACCEPTED run as anyone may watch it: the setup needed to
// regenerate its text, the log to play back, and the server's own numbers. It
// carries no client-reported values and no validation detail — a spectator gets
// the verdict's result, not its reasoning.
//
// Log is the stored gzip blob, decompressed by the handler exactly as the
// owner-only ?log=1 path does.
type PublicReplay struct {
	RunID         uuid.UUID
	DisplayName   string
	Mode          string
	DurationMs    *int32
	WordCount     *int32
	Lang          string
	Setup         json.RawMessage
	Log           []byte
	ServerMetrics json.RawMessage
	ServerScore   json.RawMessage
	Grade         string
	AchievedAt    time.Time
}

// Store is the runs persistence contract, declared at the consumer and
// implemented by the Postgres adapter. A missing (or not-owned) run is reported
// as ErrNotFound.
type Store interface {
	// CreateRun inserts a run and returns its id, status, and creation time.
	CreateRun(ctx context.Context, p CreateRunParams) (Run, error)
	// ListRuns returns up to limit of userID's runs, newest first. A nil after
	// starts from the top; otherwise listing continues after that keyset
	// position.
	ListRuns(ctx context.Context, userID uuid.UUID, after *Cursor, limit int32) ([]Summary, error)
	// Run returns one run summary owned by userID (no log payload).
	Run(ctx context.Context, id, userID uuid.UUID) (Summary, error)
	// RunLog returns the gzip log blob for one run owned by userID.
	RunLog(ctx context.Context, id, userID uuid.UUID) ([]byte, error)
	// PublicReplay returns one accepted run for public playback. A run that is
	// not accepted, or whose owner is banned, is ErrNotFound — the same answer
	// as one that does not exist.
	PublicReplay(ctx context.Context, id uuid.UUID) (PublicReplay, error)
}
