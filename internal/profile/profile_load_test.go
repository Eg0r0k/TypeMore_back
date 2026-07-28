//go:build load

package profile_test

// The profile zone's load suite (docs/PERFORMANCE.md, zone 9): a 100k-run
// account on top of a 6-figure background population, every aggregate's PLAN
// pinned to the per-user indexes (no seq scan of runs, no external sort), and
// every endpoint's cost measured against a budget.
//
// The owner's requirement is explicit: the profile is on-demand SQL with no
// cache, so the only thing standing between "profile page" and "full-table
// scan per request" is what this file asserts.

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/typemore/typemore-server/internal/keyboard"
	keyboardpg "github.com/typemore/typemore-server/internal/keyboard/pgstore"
	"github.com/typemore/typemore-server/internal/perf"
	"github.com/typemore/typemore-server/internal/platform/db"
	"github.com/typemore/typemore-server/internal/platform/migrate"
	profilepg "github.com/typemore/typemore-server/internal/profile/pgstore"
	"github.com/typemore/typemore-server/internal/replay"
)

const zone9 = "zone 9 (profile aggregates)"

// --- fixture ----------------------------------------------------------------

type loadFixture struct {
	pool  *pgxpool.Pool
	store *profilepg.Store
	// user is the 100k-run account every query below is scoped to.
	user       uuid.UUID
	seed       perf.ProfileSeedResult
	background perf.SeedResult
}

var (
	fixtureOnce sync.Once
	sharedFix   *loadFixture
	fixtureErr  error
	dbContainer *postgres.PostgresContainer
)

func TestMain(m *testing.M) {
	code := m.Run()
	if dbContainer != nil {
		_ = dbContainer.Terminate(context.Background())
	}
	os.Exit(code)
}

func fixture(t *testing.T) *loadFixture {
	t.Helper()
	fixtureOnce.Do(func() { sharedFix, fixtureErr = buildFixture(t) })
	require.NoError(t, fixtureErr, "build the profile load fixture")
	return sharedFix
}

func buildFixture(t *testing.T) (*loadFixture, error) {
	t.Helper()
	ctx := context.Background()

	var err error
	dbContainer, err = postgres.Run(ctx, "postgres:17",
		postgres.WithDatabase("typemore"),
		postgres.WithUsername("typemore"),
		postgres.WithPassword("typemore"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(90*time.Second),
		),
	)
	if err != nil {
		return nil, err
	}
	dsn, err := dbContainer.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return nil, err
	}
	if err := migrate.Up(ctx, dsn); err != nil {
		return nil, err
	}
	pool, err := db.NewPool(ctx, dsn, 8)
	if err != nil {
		return nil, err
	}

	f := &loadFixture{pool: pool, store: profilepg.New(pool)}

	// Background population first: other players' runs are what a seq scan
	// would have to wade through, so without them "no seq scan" asserts
	// nothing. The full zone-3/4 population (~1M runs) — anything smaller and
	// the 100k-run user IS the table, at which point the planner is right to
	// scan it sequentially and the assertion pins nothing real.
	t.Logf("seeding background population (~1M runs) + the 100k-run profile user")
	if f.background, err = perf.Seed(ctx, pool, perf.DefaultSeed()); err != nil {
		return nil, err
	}
	if f.seed, err = perf.SeedProfileUser(ctx, pool, perf.DefaultProfileSeed()); err != nil {
		return nil, err
	}
	f.user = f.seed.UserID

	// Populate the entries table from the background's hot bucket (one best
	// run per player, exactly the projection's invariant) so the PB read is
	// planned against ~100k rows — against the fixture user's three dozen
	// alone, a seq scan would be the right plan and the assertion noise.
	if _, err := pool.Exec(ctx, `
		INSERT INTO leaderboard_entries
		       (bucket_key, user_id, run_id, score, wpm, raw, acc, grade, mods, achieved_at)
		SELECT DISTINCT ON (user_id)
		       'time:60000:en:seeded', user_id, id, 1000, 100, 101, 0.97, 'S',
		       '{}'::jsonb, created_at
		FROM runs
		WHERE mode = 'time' AND duration_ms = 60000 AND lang = 'en'
		  AND status = 'accepted' AND user_id <> $1
		ORDER BY user_id, created_at DESC
		ON CONFLICT DO NOTHING`, f.user); err != nil {
		return nil, err
	}
	if _, err := pool.Exec(ctx, `ANALYZE leaderboard_entries`); err != nil {
		return nil, err
	}
	t.Logf("fixture ready: profile user %s with %d runs (%d accepted) over background of %d runs, seeded in %s + %s",
		f.user, f.seed.TotalRuns, f.seed.Accepted, f.background.TotalRuns,
		f.background.Elapsed, f.seed.Elapsed)
	return f, nil
}

// --- the statements under measurement ---------------------------------------
//
// Restated from internal/profile/queries.sql exactly as the leaderboard suite
// restates its reads: sqlc's generated constants are unexported, and the plan
// contract belongs to the SQL, not to the wrapper. A drift between these and
// queries.sql is a drift the budget numbers would expose immediately.

const sqlCounts = `
SELECT count(*)::bigint,
       coalesce(sum(restarts_since_last_submit), 0)::bigint,
       coalesce(sum((server_metrics ->> 'durationSec')::float8 * 1000)
                FILTER (WHERE status = 'accepted'), 0)::bigint,
       coalesce(sum(((server_metrics -> 'chars' ->> 'correct')::float8
                   + (server_metrics -> 'chars' ->> 'incorrect')::float8
                   + (server_metrics -> 'chars' ->> 'extra')::float8
                   + (server_metrics ->> 'spaces')::float8) / 5)
                FILTER (WHERE status = 'accepted'), 0)::float8
FROM runs
WHERE user_id = $1`

const sqlMetricStats = `
SELECT coalesce(max((server_metrics ->> 'wpm')::float8), 0)::float8,
       coalesce(avg((server_metrics ->> 'wpm')::float8), 0)::float8,
       coalesce(max((server_metrics ->> 'raw')::float8), 0)::float8,
       coalesce(avg((server_metrics ->> 'raw')::float8), 0)::float8,
       coalesce(max((server_metrics ->> 'accuracy')::float8), 0)::float8,
       coalesce(avg((server_metrics ->> 'accuracy')::float8), 0)::float8,
       coalesce(max((server_metrics ->> 'consistency')::float8), 0)::float8,
       coalesce(avg((server_metrics ->> 'consistency')::float8), 0)::float8
FROM runs
WHERE user_id = $1 AND status = 'accepted'`

const sqlLast10 = `
SELECT coalesce(avg(t.wpm), 0)::float8,
       coalesce(avg(t.raw), 0)::float8,
       coalesce(avg(t.acc), 0)::float8,
       coalesce(avg(t.consistency), 0)::float8
FROM (SELECT (server_metrics ->> 'wpm')::float8         AS wpm,
             (server_metrics ->> 'raw')::float8         AS raw,
             (server_metrics ->> 'accuracy')::float8    AS acc,
             (server_metrics ->> 'consistency')::float8 AS consistency
      FROM runs
      WHERE user_id = $1 AND status = 'accepted'
      ORDER BY created_at DESC, id DESC
      LIMIT 10) t`

const sqlStreaks = `
WITH days AS (SELECT DISTINCT (created_at AT TIME ZONE 'UTC')::date AS day
              FROM runs WHERE user_id = $1),
     islands AS (SELECT day,
                        day - (row_number() OVER (ORDER BY day))::int AS anchor
                 FROM days),
     lengths AS (SELECT count(*)::int AS len, max(day) AS last_day
                 FROM islands GROUP BY anchor)
SELECT coalesce(max(len), 0)::int,
       coalesce(max(len) FILTER (WHERE last_day >= $2::date - 1), 0)::int
FROM lengths`

const sqlLanguages = `
SELECT (coalesce(jsonb_agg(jsonb_build_object('lang', lang, 'tests', tests)
                           ORDER BY tests DESC, lang), '[]'::jsonb))::jsonb
FROM (SELECT lang, count(*)::int AS tests
      FROM runs WHERE user_id = $1
      GROUP BY lang) t`

const sqlActivity = `
SELECT (created_at AT TIME ZONE 'UTC')::date,
       count(*)::int,
       coalesce(sum((server_metrics ->> 'durationSec')::float8 * 1000)
                FILTER (WHERE status = 'accepted'), 0)::bigint
FROM runs
WHERE user_id = $1 AND created_at >= $2
GROUP BY 1
ORDER BY 1`

const sqlHistogram = `
SELECT (floor((server_metrics ->> 'wpm')::float8 / 10) * 10)::int,
       count(*)::int
FROM runs
WHERE user_id = $1 AND status = 'accepted'
GROUP BY 1
ORDER BY 1`

const sqlTimeseries = `
SELECT (created_at AT TIME ZONE 'UTC')::date,
       coalesce(sum((server_metrics ->> 'durationSec')::float8 * 1000)
                FILTER (WHERE status = 'accepted'), 0)::bigint,
       coalesce(avg((server_metrics ->> 'wpm')::float8)
                FILTER (WHERE status = 'accepted'), 0)::float8,
       coalesce(avg((server_metrics ->> 'accuracy')::float8)
                FILTER (WHERE status = 'accepted'), 0)::float8
FROM runs
WHERE user_id = $1 AND created_at >= $2 AND created_at < $3
GROUP BY 1
ORDER BY 1`

const sqlWpmPerHour = `
WITH days AS (SELECT (created_at AT TIME ZONE 'UTC')::date AS day,
                     avg((server_metrics ->> 'wpm')::float8)          AS wpm,
                     sum((server_metrics ->> 'durationSec')::float8)  AS secs
              FROM runs
              WHERE user_id = $1 AND status = 'accepted'
                AND created_at >= $2 AND created_at < $3
              GROUP BY 1),
     pts AS (SELECT wpm, sum(secs) OVER (ORDER BY day) / 3600.0 AS hours
             FROM days)
SELECT coalesce(regr_slope(wpm, hours), 0)::float8
FROM pts`

const sqlPBs = `
SELECT bucket_key, run_id, score, wpm::float8, raw::float8,
       acc::float8, grade, mods, quote_source, achieved_at
FROM leaderboard_entries
WHERE user_id = $1
ORDER BY achieved_at DESC`

// The runs-list page, restated from internal/runs/queries.sql: the owner's
// requirement is that the profile's table page STAYS keyset — first page and a
// deep continuation both walk (user_id, created_at DESC) with a LIMIT, never
// sort, never scan.
const sqlRunsFirst = `
SELECT id, mode, duration_ms, word_count, lang, seed, dict_hash,
       setup, client_metrics, client_score, score_version, status,
       server_metrics, server_score, validation, validated_at,
       log_bytes, restarts_since_last_submit, created_at,
       (jsonb_strip_nulls(jsonb_build_object(
           'grade',       run_grade((server_metrics ->> 'accuracy')::numeric),
           'consistency', (server_metrics ->> 'consistency')::float8,
           'chars',       server_metrics -> 'chars',
           'quoteId',     run_quote_id(setup),
           'mods',        run_mods(setup))))::jsonb AS derived
FROM runs
WHERE user_id = $1
ORDER BY created_at DESC, id DESC
LIMIT $2`

const sqlRunsAfter = `
SELECT id, mode, duration_ms, word_count, lang, seed, dict_hash,
       setup, client_metrics, client_score, score_version, status,
       server_metrics, server_score, validation, validated_at,
       log_bytes, restarts_since_last_submit, created_at,
       (jsonb_strip_nulls(jsonb_build_object(
           'grade',       run_grade((server_metrics ->> 'accuracy')::numeric),
           'consistency', (server_metrics ->> 'consistency')::float8,
           'chars',       server_metrics -> 'chars',
           'quoteId',     run_quote_id(setup),
           'mods',        run_mods(setup))))::jsonb AS derived
FROM runs
WHERE user_id = $1
  AND created_at <= $2
  AND (created_at < $2 OR (created_at = $2 AND id < $3))
ORDER BY created_at DESC, id DESC
LIMIT $4`

// --- plans ------------------------------------------------------------------

// Every aggregate must be driven by runs' (user_id, created_at) index — or
// leaderboard_entries_user_idx for the PBs — with no sequential scan and no
// sort that spills. Small in-memory sorts (a GROUP BY's output ordering over a
// few hundred day rows) are the plan working as designed and stay allowed;
// NoSort is asserted only where the index itself must supply the order.
func TestLoadProfilePlans(t *testing.T) {
	f := fixture(t)
	ctx := context.Background()
	today := time.Now().UTC()
	yearAgo := today.AddDate(-1, 0, 0)

	cases := []struct {
		name   string
		sql    string
		args   []any
		noSort bool
		noSeq  []string
	}{
		{"counts", sqlCounts, []any{f.user}, true, []string{"runs"}},
		{"metric-stats", sqlMetricStats, []any{f.user}, true, []string{"runs"}},
		{"last-10", sqlLast10, []any{f.user}, true, []string{"runs"}},
		{"streaks", sqlStreaks, []any{f.user, today}, false, []string{"runs"}},
		{"languages", sqlLanguages, []any{f.user}, false, []string{"runs"}},
		{"activity", sqlActivity, []any{f.user, yearAgo}, false, []string{"runs"}},
		{"histogram", sqlHistogram, []any{f.user}, false, []string{"runs"}},
		{"timeseries", sqlTimeseries, []any{f.user, yearAgo, today}, false, []string{"runs"}},
		{"wpm-per-hour", sqlWpmPerHour, []any{f.user, yearAgo, today}, false, []string{"runs"}},
		{"pbs", sqlPBs, []any{f.user}, false, []string{"leaderboard_entries", "runs"}},
		{"runs-list-first", sqlRunsFirst, []any{f.user, int32(20)}, true, []string{"runs"}},
	}
	for _, c := range cases {
		plan, err := perf.Explain(ctx, f.pool, c.sql, c.args...)
		require.NoError(t, err, c.name)
		perf.AssertPlan(t, plan, perf.PlanAssertion{
			Zone:        zone9,
			Query:       c.name,
			WantAny:     []string{"Index Scan", "Index Only Scan", "Bitmap Index Scan"},
			NoSeqScanOn: c.noSeq,
			NoSort:      c.noSort,
		})
	}

	// The deep continuation gets its cursor from halfway down the real data.
	var at time.Time
	var id uuid.UUID
	require.NoError(t, f.pool.QueryRow(ctx, `
		SELECT created_at, id FROM runs WHERE user_id = $1
		ORDER BY created_at DESC, id DESC OFFSET 50000 LIMIT 1`, f.user).Scan(&at, &id))
	plan, err := perf.Explain(ctx, f.pool, sqlRunsAfter, f.user, at, id, int32(20))
	require.NoError(t, err)
	perf.AssertPlan(t, plan, perf.PlanAssertion{
		Zone:        zone9,
		Query:       "runs-list-after (row 50000)",
		WantAny:     []string{"Index Scan", "Bitmap Index Scan"},
		NoSeqScanOn: []string{"runs"},
		NoSort:      true,
	})
}

// --- budgets ----------------------------------------------------------------

// measure runs fn `iters` times and returns the median wall time. The first
// call is a warm-up (catalog caches, buffer pool) and is not counted.
func measure(t *testing.T, iters int, fn func() error) time.Duration {
	t.Helper()
	require.NoError(t, fn())
	samples := make([]time.Duration, 0, iters)
	for range iters {
		start := time.Now()
		require.NoError(t, fn())
		samples = append(samples, time.Since(start))
	}
	for i := range samples {
		for j := i + 1; j < len(samples); j++ {
			if samples[j] < samples[i] {
				samples[i], samples[j] = samples[j], samples[i]
			}
		}
	}
	return samples[len(samples)/2]
}

// The endpoint budgets. Every number is a measured median × ~3 headroom on the
// 100k-run fixture (see docs/PERFORMANCE.md, zone 9, for the measurements) —
// generous enough to survive CI noise, tight enough that an accidental table
// scan (tens of ms → seconds at this volume) fails loudly.
func TestLoadProfileBudgets(t *testing.T) {
	f := fixture(t)
	ctx := context.Background()
	store := f.store
	today := time.Now().UTC()
	yearAgo := today.AddDate(-1, 0, 0)

	run := func(name string, limit time.Duration, rationale string, fn func() error) {
		t.Helper()
		med := measure(t, 15, fn)
		perf.Budget{Zone: zone9, Workload: name, Limit: limit, Rationale: rationale}.
			Assert(t, med)
	}

	run("GET /profile/summary", 800*time.Millisecond,
		"six concurrent aggregates over the user's 100k index entries, jsonb extraction dominating; measured median ~265 ms ×3",
		func() error {
			_, err := store.Summary(ctx, f.user, today)
			return err
		})
	run("GET /profile/activity (366d)", 500*time.Millisecond,
		"one grouped index range scan over a year of the history; measured median ~170 ms ×3",
		func() error {
			_, err := store.Activity(ctx, f.user, today.AddDate(0, 0, -365))
			return err
		})
	run("GET /profile/histogram", 600*time.Millisecond,
		"one grouped index scan over ALL the user's accepted runs; measured median ~190 ms ×3",
		func() error {
			_, err := store.Histogram(ctx, f.user)
			return err
		})
	run("GET /profile/timeseries (1y)", 700*time.Millisecond,
		"two grouped scans of the range (series + regression); measured median ~370 ms ×2",
		func() error {
			_, _, err := store.Timeseries(ctx, f.user, yearAgo, today)
			return err
		})
	run("GET /profile/pbs", 25*time.Millisecond,
		"one index scan of the user's ≤ dozens of entries rows; measured median ~2 ms, floored well above jitter",
		func() error {
			_, err := store.PBs(ctx, f.user)
			return err
		})
}

// --- the keyboard heatmap (docs/PROFILE.md, "Keyboard") ---------------------

const sqlKeyboard = `
SELECT key_id, presses, errors, interval_sum_ms, interval_count
FROM user_keyboard_profile
WHERE user_id = $1
ORDER BY key_id`

// seedKeyboard populates user_keyboard_profile: ~46 keys for the profile user
// and for a slice of the background population — without the background rows
// the whole table would be one user's four dozen rows and a seq scan would be
// the planner's correct answer.
func seedKeyboard(t *testing.T, f *loadFixture) {
	t.Helper()
	ctx := context.Background()
	var n int64
	require.NoError(t, f.pool.QueryRow(ctx,
		`SELECT count(*) FROM user_keyboard_profile`).Scan(&n))
	if n > 0 {
		return
	}
	_, err := f.pool.Exec(ctx, `
		INSERT INTO user_keyboard_profile (user_id, key_id, presses, errors, interval_sum_ms, interval_count)
		SELECT u.id, k.key_id, 1000, 20, 150000, 900
		FROM (SELECT id FROM users LIMIT 5000) u
		CROSS JOIN (SELECT 'Key' || chr(65 + g) AS key_id FROM generate_series(0, 25) g
		            UNION ALL SELECT 'Space' UNION ALL SELECT 'other') k`)
	require.NoError(t, err)
	_, err = f.pool.Exec(ctx, `
		INSERT INTO user_keyboard_profile (user_id, key_id, presses, errors, interval_sum_ms, interval_count)
		SELECT $1, 'Key' || chr(65 + g), 5000, 100, 700000, 4500
		FROM generate_series(0, 25) g
		ON CONFLICT (user_id, key_id) DO NOTHING`, f.user)
	require.NoError(t, err)
	_, err = f.pool.Exec(ctx, `ANALYZE user_keyboard_profile`)
	require.NoError(t, err)
}

// The heatmap read must be one PK-prefix index scan of the user's four dozen
// rows — never a scan of everyone's.
func TestLoadProfileKeyboardPlan(t *testing.T) {
	f := fixture(t)
	seedKeyboard(t, f)

	plan, err := perf.Explain(context.Background(), f.pool, sqlKeyboard, f.user)
	require.NoError(t, err)
	perf.AssertPlan(t, plan, perf.PlanAssertion{
		Zone:        zone9,
		Query:       "keyboard",
		WantAny:     []string{"Index Scan", "Index Only Scan", "Bitmap Index Scan"},
		NoSeqScanOn: []string{"user_keyboard_profile", "runs"},
	})
}

// The endpoint budget (read side) and the projection's write cost — the upsert
// that joins every verdict transaction, measured as one add + one reversal per
// iteration so the fixture ends where it began.
func TestLoadProfileKeyboardBudgets(t *testing.T) {
	f := fixture(t)
	seedKeyboard(t, f)
	ctx := context.Background()

	med := measure(t, 15, func() error {
		_, _, err := f.store.Keyboard(ctx, f.user)
		return err
	})
	perf.Budget{
		Zone: zone9, Workload: "GET /profile/keyboard", Limit: 500 * time.Millisecond,
		Rationale: "one PK-prefix scan of ~46 rows plus the dominant-language group over the user's index entries; measured median ~150 ms ×3",
	}.Assert(t, med)

	// The projection write: a representative per-run contribution (46 keys),
	// added and then reversed inside transactions on a run of the seeded user.
	layouts, err := keyboard.Load()
	require.NoError(t, err)
	projector := keyboardpg.New(layouts)
	var runID uuid.UUID
	require.NoError(t, f.pool.QueryRow(ctx, `
		SELECT id FROM runs
		WHERE user_id = $1 AND status = 'accepted' AND NOT keyboard_projected
		LIMIT 1`, f.user).Scan(&runID))
	observations := make([]replay.CharObservation, 0, 46)
	for c := 'a'; c <= 'z'; c++ {
		observations = append(observations, replay.CharObservation{
			Char: string(c), Presses: 12, Errors: 1, IntervalSumMs: 1400, IntervalCount: 11,
		})
	}
	observations = append(observations, replay.CharObservation{Char: " ", Presses: 9, IntervalSumMs: 900, IntervalCount: 9})

	project := func(accepted bool) error {
		tx, err := f.pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if err := projector.ProjectKeyboard(ctx, tx, runID, accepted, observations); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	med = measure(t, 15, func() error {
		if err := project(true); err != nil { // add + stamp
			return err
		}
		return project(false) // reverse + unstamp
	})
	perf.Budget{
		Zone: zone9, Workload: "keyboard projection per verdict (add+reverse)", Limit: 50 * time.Millisecond,
		Rationale: "two ~46-row PK upserts plus the stamp, the cost every verdict transaction carries; measured median ~6 ms ×4, doubled here because the probe does both directions",
	}.Assert(t, med)
}
