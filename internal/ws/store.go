package ws

import (
	"context"
	"encoding/json"
	"time"
)

// MatchStore persists a finished match's authoritative capture. It is called
// once, at match end, with the whole record; the implementation is expected to
// write it in a single transaction. A nil MatchStore disables persistence
// (useful in tests and for a DB-less deployment).
type MatchStore interface {
	SaveMatch(ctx context.Context, m MatchRecord) error
}

// MatchRecord is the persisted match header plus one run per participant. It
// mirrors the matches / match_runs schema (db/migrations/00003_matches.sql).
type MatchRecord struct {
	ID       string          // wire matchId, also the matches primary key
	RoomCode string          // the room the match was played in
	Name     string          // room name at start
	Settings json.RawMessage // frozen settings incl. textSource
	Freemods json.RawMessage // frozen [{playerId, freemods}] snapshot
	Seed     int64           // generation seed [0, 2^32-1]
	DictHash string
	Lang     string
	GoAt     time.Time // scheduled t=0
	EndedAt  time.Time // when the match ended on the server
	Runs     []MatchRunRecord
}

// MatchRunRecord is one participant's authoritative capture. Log is the gzip of
// the ordered CapturedBatch stream (with server recv stamps) — the input the
// future replay worker validates. No validation happens now.
type MatchRunRecord struct {
	PlayerID    string
	Nick        string
	UserID      string // empty for a guest (persisted as NULL)
	Freemods    json.RawMessage
	Log         []byte // gzip(JSON([]CapturedBatch))
	BatchCount  int
	FinalStatus string // finished | dnf | left
}

// CapturedBatch is one relayed event_batch as stored in a run's capture: the
// client's batchSeq, the server receive timestamp, and the opaque events. This
// is the on-disk (pre-gzip) JSON shape of the authoritative log.
type CapturedBatch struct {
	BatchSeq     int               `json:"batchSeq"`
	RecvServerMs int64             `json:"recvServerMs"`
	Events       []json.RawMessage `json:"events"`
}
