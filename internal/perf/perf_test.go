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
		{Mode: "words", WordCount: new(int32(100)), Lang: "code_css"},
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
// It also pins the RELATIONSHIP between the two caps, which is the thing that
// was wrong before. The old pair (50 000 events / 2 MiB) could not both be
// obeyed: 50 000 events marshal past 2 MiB, so the body cap silently bounded
// every submission at 39 915 and the documented event cap was a phantom. The
// new pair is ordered on purpose:
//
//	the EVENT cap is operative — it is what a real log runs into, and it is
//	sized above the largest run the game permits on any published dictionary;
//	the BODY cap sits ABOVE it — it no longer bounds a well-formed log at all,
//	and exists to catch a payload that is fat for another reason (a paste, an
//	IME commit: one insert carrying many graphemes).
//
// A body cap that bounds a well-formed log is doing the event cap's job badly.
// These assertions are what keep it out of that job.
func TestMaxLegalPayloadSitsAtTheCaps(t *testing.T) {
	// ── the v1 envelope: event cap operative, v1 log-byte bound above it ────
	submittable := perf.SubmittableEvents(1)
	perf.Report(t, "fixture", "v1 events that fit under the v1 log-byte bound", submittable)

	// The phantom is retired: a client that obeys the documented event cap is
	// no longer refused by the size cap.
	atEventCap := perf.MustJSON(perf.MaxEventsPayload(1))
	perf.Report(t, "fixture", "body at the documented v1 event cap", perf.MiB(uint64(len(atEventCap))))
	assert.LessOrEqual(t, len(atEventCap), perf.MaxLogBytesV1,
		"the documented event cap is un-submittable again: the v1 log-byte bound has become a second, hidden event cap")
	assert.Equal(t, perf.MaxEvents, submittable,
		"the v1 event cap must be the operative one; the log-byte bound is bounding a well-formed log")

	// ── the v2 envelope: event cap operative, transport cap above it ────────
	submittableV2 := perf.SubmittableEventsV2(1)
	perf.Report(t, "fixture", "v2 events that fit under the transport cap", submittableV2)
	atEventCapV2 := perf.MustJSON(perf.MaxEventsV2Payload(1))
	perf.Report(t, "fixture", "body at the documented v2 event cap", perf.MiB(uint64(len(atEventCapV2))))
	assert.LessOrEqual(t, len(atEventCapV2), perf.MaxBodyBytes,
		"the v2 event cap is un-submittable: the transport cap has become a second, hidden event cap")
	assert.Equal(t, perf.MaxEventsV2, submittableV2,
		"the v2 event cap must be the operative one; the transport cap is bounding a well-formed log")

	// The transport cap is not dead weight: it still has to be REACHABLE, or
	// it is a constant nothing can trip. A paste — one insert carrying many
	// graphemes — gets there.
	fat := perf.MustJSON(perf.BuildPayload(perf.PayloadSpec{
		Setup: perf.SetupSpec{Mode: "words", WordCount: perf.MaxWordCount, DurationMs: perf.MaxDurationMs},
		Log:   perf.LogSpec{Events: perf.MaxEvents, Seed: 1, TextLen: 512},
	}))
	perf.Report(t, "fixture", "body at the v1 event cap, 512-grapheme inserts",
		perf.MiB(uint64(len(fat))))
	assert.Greater(t, len(fat), perf.MaxBodyBytes,
		"the transport cap can no longer be tripped by anything: it has stopped guarding the ingest envelope")

	p := perf.MaxLegalPayload(1)
	var log perf.EventLog
	require.NoError(t, json.Unmarshal(p.Log, &log))
	assert.Len(t, log.Events, submittable)

	require.NotNil(t, p.WordCount)
	assert.EqualValues(t, perf.MaxWordCount, *p.WordCount)

	body := perf.MustJSON(p)
	perf.Report(t, "fixture", "max ACCEPTED v1 payload body", perf.MiB(uint64(len(body))))
	assert.LessOrEqual(t, len(body), perf.MaxLogBytesV1,
		"the v1 fixture must be ACCEPTED at its bound, not rejected by it")

	bodyV2 := perf.MustJSON(perf.MaxLegalPayloadV2(1))
	perf.Report(t, "fixture", "max ACCEPTED v2 payload body", perf.MiB(uint64(len(bodyV2))))
	assert.LessOrEqual(t, len(bodyV2), perf.MaxBodyBytes,
		"the v2 fixture must be ACCEPTED at the cap, not rejected by it")
	assert.Greater(t, len(bodyV2), perf.MaxBodyBytes*80/100,
		"and it must still sit NEAR the transport cap: the two caps have drifted far enough apart "+
			"that the worst accepted payload no longer measures the worst case")

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
