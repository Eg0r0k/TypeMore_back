package leaderboard

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
)

// ErrNoEntry is returned by Store.EntryFor when the player holds no visible slot
// in that bucket — either they never scored there, or a ban hides them. The two
// are deliberately the same answer: a board must not leak who is banned.
var ErrNoEntry = errors.New("leaderboard: no entry")

// Entry is one board row: a snapshot of the player's best eligible run in a
// bucket, taken when it was projected. The numbers are the SERVER's — the
// client's reported ones never reach a leaderboard.
type Entry struct {
	Rank        int64
	UserID      uuid.UUID
	DisplayName string
	RunID       uuid.UUID
	Score       int64
	WPM         float64
	Raw         float64
	Acc         float64
	Grade       string
	// Mods is the run's mod flags as stored (run_mods in the schema).
	Mods json.RawMessage
	// Source is the quote's attribution, and it is present on exactly the quote
	// boards. A language board has no quote and therefore nothing to attribute.
	Source     string
	AchievedAt time.Time
}

// BucketCount is one entry of the board index: a bucket and how many visible
// players it holds.
type BucketCount struct {
	Bucket  Bucket
	Entries int64
}

// Cursor is a keyset position in a bucket's ranking: the (score, achieved_at,
// user_id) of the last row a page returned. The triple is unique, which is what
// makes paging stable when many players share a score.
type Cursor struct {
	Score      int64
	AchievedAt time.Time
	UserID     uuid.UUID
}

// Store is the leaderboard read model, declared here at the consumer.
//
// Every method reads the ban-filtered view, so there is no way to ask this
// interface for a page that includes a banned player. Maintenance (projection
// and rebuild) is NOT here: it is driven by the replay transaction and by the
// operator tool, and the HTTP surface has no business reaching it.
type Store interface {
	// Buckets lists every bucket that currently has at least one visible entry,
	// with its count.
	Buckets(ctx context.Context) ([]BucketCount, error)
	// Page returns up to limit entries of a bucket's ranking, continuing after
	// the given keyset position when non-nil. Rank is not set by the store.
	Page(ctx context.Context, b Bucket, after *Cursor, limit int32) ([]Entry, error)
	// RankAbove counts the visible entries that outrank a position — the rank of
	// the row at that position is this plus one.
	RankAbove(ctx context.Context, b Bucket, at Cursor) (int64, error)
	// EntryFor returns one player's entry in a bucket, with its rank filled in,
	// or ErrNoEntry.
	EntryFor(ctx context.Context, b Bucket, userID uuid.UUID) (Entry, error)
}
