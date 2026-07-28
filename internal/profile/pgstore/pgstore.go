// Package pgstore implements the profile domain's Store against Postgres,
// backed by the sqlc-generated profiledb queries. Aggregation lives entirely
// in SQL (internal/profile/queries.sql); this adapter only assembles the
// summary from its component queries and maps generated rows onto the domain
// types.
package pgstore

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"

	"github.com/typemore/typemore-server/internal/profile"
	"github.com/typemore/typemore-server/internal/profile/profiledb"
)

// Store implements profile.Store.
type Store struct {
	q *profiledb.Queries
}

var _ profile.Store = (*Store)(nil)

// New builds a Store from a pgx pool.
func New(pool *pgxpool.Pool) *Store {
	return &Store{q: profiledb.New(pool)}
}

// Summary assembles GET /profile/summary from its component aggregates,
// issued CONCURRENTLY on separate pool connections: the four heavy ones each
// walk the same ~N index entries of the user's history, so running them
// serially would charge the page ~4× the cost of its slowest query (measured:
// 1.1 s serial vs ~0.3 s concurrent on the zone-9 fixture). Separate
// snapshots, not one transaction: a run accepted between two of them can skew
// a counter by one for one request, which statistics can afford — a
// transaction would put the whole page back on one connection.
func (s *Store) Summary(ctx context.Context, userID uuid.UUID, today time.Time) (profile.Summary, error) {
	var (
		user      profiledb.GetProfileUserRow
		counts    profiledb.GetProfileCountsRow
		stats     profiledb.GetProfileMetricStatsRow
		last10    profiledb.GetProfileLast10Row
		streaks   profiledb.GetProfileStreaksRow
		languages json.RawMessage
	)
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() (err error) { user, err = s.q.GetProfileUser(gctx, userID); return })
	g.Go(func() (err error) { counts, err = s.q.GetProfileCounts(gctx, userID); return })
	g.Go(func() (err error) { stats, err = s.q.GetProfileMetricStats(gctx, userID); return })
	g.Go(func() (err error) { last10, err = s.q.GetProfileLast10(gctx, userID); return })
	g.Go(func() (err error) {
		streaks, err = s.q.GetProfileStreaks(gctx, profiledb.GetProfileStreaksParams{
			UserID: userID,
			Today:  today,
		})
		return
	})
	g.Go(func() (err error) { languages, err = s.q.GetProfileLanguages(gctx, userID); return })
	if err := g.Wait(); err != nil {
		return profile.Summary{}, err
	}

	restartsPerCompleted := 0.0
	if counts.TestsCompleted > 0 {
		restartsPerCompleted = float64(counts.Restarts) / float64(counts.TestsCompleted)
	}
	return profile.Summary{
		DisplayName:          user.DisplayName,
		Joined:               user.CreatedAt,
		TestsStarted:         counts.TestsCompleted + counts.Restarts,
		TestsCompleted:       counts.TestsCompleted,
		RestartsPerCompleted: restartsPerCompleted,
		TimeTypingMs:         counts.TimeTypingMs,
		EstimatedWordsTyped:  int64(math.Round(counts.EstimatedWords)),
		Wpm: profile.MetricStats{
			Highest: stats.WpmHighest, Average: stats.WpmAverage, AverageLast10: last10.WpmAverage,
		},
		Raw: profile.MetricStats{
			Highest: stats.RawHighest, Average: stats.RawAverage, AverageLast10: last10.RawAverage,
		},
		Acc: profile.MetricStats{
			Highest: stats.AccHighest, Average: stats.AccAverage, AverageLast10: last10.AccAverage,
		},
		Consistency: profile.MetricStats{
			Highest: stats.ConsistencyHighest, Average: stats.ConsistencyAverage,
			AverageLast10: last10.ConsistencyAverage,
		},
		Streak:    profile.Streak{Current: streaks.Current, Best: streaks.Best},
		Languages: languages,
	}, nil
}

// Activity returns the populated day buckets since the cutoff.
func (s *Store) Activity(ctx context.Context, userID uuid.UUID, since time.Time) ([]profile.ActivityDay, error) {
	rows, err := s.q.GetProfileActivity(ctx, profiledb.GetProfileActivityParams{
		UserID:    userID,
		CreatedAt: since,
	})
	if err != nil {
		return nil, err
	}
	out := make([]profile.ActivityDay, len(rows))
	for i, r := range rows {
		out[i] = profile.ActivityDay{Date: r.Day, Tests: r.Tests, TimeMs: r.TimeMs}
	}
	return out, nil
}

// Histogram returns the populated 10-wpm buckets.
func (s *Store) Histogram(ctx context.Context, userID uuid.UUID) ([]profile.HistogramBucket, error) {
	rows, err := s.q.GetProfileHistogram(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]profile.HistogramBucket, len(rows))
	for i, r := range rows {
		out[i] = profile.HistogramBucket{Bucket: r.Bucket, Tests: r.Tests}
	}
	return out, nil
}

// Timeseries returns the per-day series inside [from, to) and the wpm-per-hour
// regression slope over the same range.
func (s *Store) Timeseries(ctx context.Context, userID uuid.UUID, from, to time.Time) ([]profile.TimeseriesDay, float64, error) {
	rows, err := s.q.GetProfileTimeseries(ctx, profiledb.GetProfileTimeseriesParams{
		UserID:      userID,
		CreatedAt:   from,
		CreatedAt_2: to,
	})
	if err != nil {
		return nil, 0, err
	}
	slope, err := s.q.GetProfileWpmPerHour(ctx, profiledb.GetProfileWpmPerHourParams{
		UserID:      userID,
		CreatedAt:   from,
		CreatedAt_2: to,
	})
	if err != nil {
		return nil, 0, err
	}
	out := make([]profile.TimeseriesDay, len(rows))
	for i, r := range rows {
		out[i] = profile.TimeseriesDay{
			Date: r.Day, TimeTypingMs: r.TimeMs, AvgWpm: r.AvgWpm, AvgAcc: r.AvgAcc,
		}
	}
	return out, slope, nil
}

// PBs returns the caller's leaderboard entries — one best run per bucket.
func (s *Store) PBs(ctx context.Context, userID uuid.UUID) ([]profile.PB, error) {
	rows, err := s.q.GetProfilePBs(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]profile.PB, len(rows))
	for i, r := range rows {
		out[i] = profile.PB{
			BucketKey:   r.BucketKey,
			RunID:       r.RunID,
			Score:       r.Score,
			Wpm:         r.Wpm,
			Raw:         r.Raw,
			Acc:         r.Acc,
			Grade:       r.Grade,
			Mods:        r.Mods,
			QuoteSource: r.QuoteSource,
			AchievedAt:  r.AchievedAt,
		}
	}
	return out, nil
}

// Keyboard returns the user_keyboard_profile rows and the dominant language.
func (s *Store) Keyboard(ctx context.Context, userID uuid.UUID) ([]profile.KeyboardKey, string, error) {
	rows, err := s.q.GetProfileKeyboard(ctx, userID)
	if err != nil {
		return nil, "", err
	}
	lang, err := s.q.GetProfileDominantLang(ctx, userID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			lang = "" // a fresh account has no dominant language yet
		} else {
			return nil, "", err
		}
	}
	out := make([]profile.KeyboardKey, len(rows))
	for i, r := range rows {
		out[i] = profile.KeyboardKey{
			KeyID:         r.KeyID,
			Presses:       r.Presses,
			Errors:        r.Errors,
			IntervalSumMs: r.IntervalSumMs,
			IntervalCount: r.IntervalCount,
		}
	}
	return out, lang, nil
}
