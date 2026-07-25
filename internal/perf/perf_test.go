package perf_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/typemore/typemore-server/internal/leaderboard"
	"github.com/typemore/typemore-server/internal/perf"
	"github.com/typemore/typemore-server/internal/platform/db"
	"github.com/typemore/typemore-server/internal/platform/migrate"
)

// The measuring tape needs its own calibration. A generator that quietly emits
// a log the server would reject, or a bucket key that does not match the one the
// domain produces, turns every number in docs/PERFORMANCE.md into fiction — and
// fiction that looks like data is worse than no data.

// perf.Bucket.Key deliberately does not import the leaderboard domain (the thing
// under measurement must not be a dependency of the thing measuring it). This is
// the fence that keeps the two spellings identical anyway.
func TestSeedBucketKeysMatchTheDomain(t *testing.T) {
	cases := []perf.Bucket{
		perf.HotBucket(),
		{Mode: "time", DurationMs: new(int32(15_000)), Lang: "en"},
		{Mode: "words", WordCount: new(int32(50)), Lang: "ru-RU"},
		{Mode: "words", WordCount: new(int32(100)), Lang: "css_code"},
	}
	for _, b := range cases {
		t.Run(b.Key(), func(t *testing.T) {
			want, err := leaderboard.NewBucket(b.Mode, b.DurationMs, b.WordCount, b.Lang,
				leaderboard.TextSourceSeeded)
			require.NoError(t, err)
			assert.Equal(t, want.Key(), b.Key(),
				"the seeder and the domain must agree on the key, or every seeded board is unreachable")
		})
	}
}

// A generated log has to satisfy the structural rules in docs/RUNS.md, or the
// ingestion benchmarks measure the rejection path instead of the accept path.
func TestGeneratedLogIsStructurallyValid(t *testing.T) {
	log := perf.GenerateLog(perf.LogSpec{Events: 5_000, Seed: 7})

	require.Equal(t, 1, log.Version)
	require.Len(t, log.Events, 5_000)

	prevSeq, prevT := 0, int64(-1)
	for i, e := range log.Events {
		require.Equal(t, "insert", e.Kind)
		require.Greater(t, e.Seq, prevSeq, "seq must be strictly increasing (event %d)", i)
		require.GreaterOrEqual(t, e.T, prevT, "time must be monotonic (event %d)", i)
		require.NotEmpty(t, e.Text)
		prevSeq, prevT = e.Seq, e.T
	}
	require.Equal(t, 1, log.Events[0].Seq, "the core emits seq contiguous from 1")

	// Reproducible: a benchmark that regenerates its fixture must get the same
	// bytes, or run-to-run differences are dice rather than regressions.
	again := perf.GenerateLog(perf.LogSpec{Events: 5_000, Seed: 7})
	assert.Equal(t, log, again)
	assert.NotEqual(t, log, perf.GenerateLog(perf.LogSpec{Events: 5_000, Seed: 8}))
}

// The zone 2 and 5 fixtures claim to be "the largest thing ingestion accepts".
// If that claim drifts — a cap changes, the generator shrinks — the zones stop
// measuring the worst case without anyone noticing.
//
// It also pins a discrepancy found while building them: the documented event cap
// (50 000) and the documented body cap (2 MiB) do not meet. A client that obeys
// the first is rejected by the second.
func TestMaxLegalPayloadSitsAtTheCaps(t *testing.T) {
	submittable := perf.SubmittableEvents(1)
	perf.Report(t, "fixture", "events that fit under the 2 MiB body cap", submittable)

	// The finding, asserted so a future cap change surfaces it rather than
	// silently reconciling it.
	overCap := perf.MustJSON(perf.MaxEventsPayload(1))
	perf.Report(t, "fixture", "body at the documented 50k-event cap", perf.MiB(uint64(len(overCap))))
	assert.Greater(t, len(overCap), perf.MaxBodyBytes,
		"if this ever passes, the two ingestion caps have been reconciled — update docs/PERFORMANCE.md zone 5")
	assert.Less(t, submittable, perf.MaxEvents,
		"the body cap, not the event cap, is what actually bounds a submission")

	p := perf.MaxLegalPayload(1)
	var log perf.EventLog
	require.NoError(t, json.Unmarshal(p.Log, &log))
	assert.Len(t, log.Events, submittable)

	require.NotNil(t, p.WordCount)
	assert.EqualValues(t, perf.MaxWordCount, *p.WordCount)

	body := perf.MustJSON(p)
	perf.Report(t, "fixture", "max ACCEPTED payload body", perf.MiB(uint64(len(body))))
	assert.LessOrEqual(t, len(body), perf.MaxBodyBytes,
		"the fixture must be ACCEPTED at the cap, not rejected by it")
	assert.Greater(t, len(body), perf.MaxBodyBytes*98/100,
		"and it must sit AT the cap, not comfortably below it")

	// Gzip is what actually gets stored; the ratio matters for the zone 6 budget.
	gz := perf.Gzip(body)
	perf.Report(t, "fixture", "max legal payload gzipped",
		fmt.Sprintf("%s (%.0f%% of raw)", perf.MiB(uint64(len(gz))),
			float64(len(gz))/float64(len(body))*100))
	assert.Less(t, len(gz), len(body))
}

// The seeder is the foundation of zones 3 and 4. Exercised at SmallSeed volume
// so the check itself stays cheap; the shape it produces is what matters.
func TestSeedProducesAnEligiblePopulation(t *testing.T) {
	if testing.Short() {
		t.Skip("needs a Postgres container")
	}
	ctx := context.Background()
	pool := testPool(t)
	require.NoError(t, perf.Truncate(ctx, pool))

	spec := perf.SmallSeed()
	res, err := perf.Seed(ctx, pool, spec)
	require.NoError(t, err)

	perf.Report(t, "fixture", "small seed", res.TotalRuns)
	assert.Positive(t, res.TotalRuns)
	assert.Len(t, res.Users, spec.Users)
	assert.NotEmpty(t, res.HotUsers)
	assert.NotEmpty(t, res.ColdBuckets)

	var users, runs, accepted, eligible, bans int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&users))
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM runs`).Scan(&runs))
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM runs WHERE status='accepted'`).Scan(&accepted))
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM leaderboard_eligible_runs`).Scan(&eligible))
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM bans`).Scan(&bans))

	assert.Equal(t, spec.Users, users)
	assert.Equal(t, res.TotalRuns, runs)
	assert.Positive(t, bans, "the population must include banned players, or the ban filter is never exercised")
	assert.Less(t, accepted, runs, "some runs must be flagged/rejected, or the eligible view filters nothing")
	assert.Positive(t, eligible)
	assert.LessOrEqual(t, eligible, accepted,
		"every eligible run is accepted; the view must not admit more than exist")

	// The hot bucket has to actually be hot, or the /me rank curve measures nothing.
	var hotRuns int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM leaderboard_eligible_runs WHERE mode='time' AND duration_ms=60000 AND lang='en'`).
		Scan(&hotRuns))
	perf.Report(t, "fixture", "hot bucket eligible runs", hotRuns)
	assert.Positive(t, hotRuns)

	// The leaderboard is left EMPTY on purpose: populating it is what zone 4
	// measures.
	var entries int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM leaderboard_entries`).Scan(&entries))
	assert.Zero(t, entries, "Seed must leave projection work to be measured")
}

// --- container plumbing (mirrors the runs and leaderboard suites) ---

var (
	dbOnce      sync.Once
	dbContainer *postgres.PostgresContainer
	testDSN     string
	dbErr       error
)

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	dbOnce.Do(func() {
		dbContainer, dbErr = postgres.Run(ctx, "postgres:17",
			postgres.WithDatabase("typemore"),
			postgres.WithUsername("typemore"),
			postgres.WithPassword("typemore"),
			testcontainers.WithWaitStrategy(
				wait.ForLog("database system is ready to accept connections").
					WithOccurrence(2).
					WithStartupTimeout(90*time.Second),
			),
		)
		if dbErr != nil {
			return
		}
		testDSN, dbErr = dbContainer.ConnectionString(ctx, "sslmode=disable")
		if dbErr != nil {
			return
		}
		dbErr = migrate.Up(ctx, testDSN)
	})
	require.NoError(t, dbErr, "start/migrate postgres testcontainer")

	pool, err := db.NewPool(ctx, testDSN, 8)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

func TestMain(m *testing.M) {
	code := m.Run()
	if dbContainer != nil {
		_ = dbContainer.Terminate(context.Background())
	}
	os.Exit(code)
}
