package profile

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// MetricStats is one profile metric's three aggregates. Acc and consistency
// travel as [0, 1] fractions, exactly as the core reports them; formatting as
// a percentage is the display edge's job.
type MetricStats struct {
	Highest       float64 `json:"highest"`
	Average       float64 `json:"average"`
	AverageLast10 float64 `json:"averageLast10"`
}

// Streak is the consecutive-days-played pair. Days are runs-based and bucketed
// in UTC — logins are not tracked, by design.
type Streak struct {
	Current int32 `json:"current"`
	Best    int32 `json:"best"`
}

// Summary is GET /profile/summary: identity, counters, the four metric groups,
// the streaks, and per-language play counts.
type Summary struct {
	DisplayName string    `json:"displayName"`
	Joined      time.Time `json:"joined"`
	// TestsStarted = TestsCompleted + the client-reported restart counts
	// (docs/RUNS.md, restartsSinceLastSubmit): a restarted test never became a
	// row, so the completed count alone would undercount every starter.
	TestsStarted   int64 `json:"testsStarted"`
	TestsCompleted int64 `json:"testsCompleted"`
	// RestartsPerCompleted is the ratio the counters row renders; 0 for a
	// fresh account rather than a division error.
	RestartsPerCompleted float64     `json:"restartsPerCompleted"`
	TimeTypingMs         int64       `json:"timeTypingMs"`
	EstimatedWordsTyped  int64       `json:"estimatedWordsTyped"`
	Wpm                  MetricStats `json:"wpm"`
	Raw                  MetricStats `json:"raw"`
	Acc                  MetricStats `json:"acc"`
	Consistency          MetricStats `json:"consistency"`
	Streak               Streak      `json:"streak"`
	// Languages is [{"lang": "...", "tests": N}], most played first — built in
	// SQL and passed through verbatim.
	Languages json.RawMessage `json:"languages"`
}

// ActivityDay is one populated day of the activity calendar. Days without runs
// are absent — the calendar renders its own gaps.
type ActivityDay struct {
	Date   time.Time
	Tests  int32
	TimeMs int64
}

// HistogramBucket is one 10-wpm histogram bar; Bucket is the lower bound.
type HistogramBucket struct {
	Bucket int32 `json:"wpm"`
	Tests  int32 `json:"tests"`
}

// TimeseriesDay is one day of the charts series.
type TimeseriesDay struct {
	Date         time.Time
	TimeTypingMs int64
	AvgWpm       float64
	AvgAcc       float64
}

// PB is one personal-best card: a leaderboard_entries row, verbatim — the
// entries table already stores exactly one best run per (player, bucket).
type PB struct {
	BucketKey   string
	RunID       uuid.UUID
	Score       int64
	Wpm         float64
	Raw         float64
	Acc         float64
	Grade       string
	Mods        json.RawMessage
	QuoteSource *string
	AchievedAt  time.Time
}

// Store is the profile read model, implemented by pgstore. Every method is
// scoped to one user; today is the caller's UTC date for the streak boundary.
type Store interface {
	Summary(ctx context.Context, userID uuid.UUID, today time.Time) (Summary, error)
	Activity(ctx context.Context, userID uuid.UUID, since time.Time) ([]ActivityDay, error)
	Histogram(ctx context.Context, userID uuid.UUID) ([]HistogramBucket, error)
	// Timeseries returns the per-day series inside [from, to) plus the OLS
	// slope of wpm over cumulative hours typed in that range (docs/PROFILE.md).
	Timeseries(ctx context.Context, userID uuid.UUID, from, to time.Time) ([]TimeseriesDay, float64, error)
	PBs(ctx context.Context, userID uuid.UUID) ([]PB, error)
}
