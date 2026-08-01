//go:build load

package leaderboard_test

import (
	"context"
	"fmt"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/typemore/typemore-server/internal/leaderboard"
	leaderboardpg "github.com/typemore/typemore-server/internal/leaderboard/pgstore"
	"github.com/typemore/typemore-server/internal/perf"
	"github.com/typemore/typemore-server/internal/platform/migrate"
)

// The load suite lives in its OWN database inside the shared testcontainer.
// The untagged tests truncate every table in newBoard, so a million-row fixture
// sharing `typemore` with them would be destroyed the moment someone runs
// `go test -tags=load` without the `-run '^TestLoad'` filter.
const perfDatabase = "typemore_perf"

// requireVerifiedEmail mirrors the production default
// (TYPEMORE_LEADERBOARD_REQUIRE_VERIFIED_EMAIL=true). It is not a detail: it
// puts an EXISTS over auth_identities inside both the per-cell recompute and
// the rebuild's enumeration, and measuring the projection without it would
// measure a query nobody deploys.
const requireVerifiedEmail = true

// noEmailGate is the same store with the gate off. The fixture's board is built
// with it, and NOT because the default is inconvenient: with the gate on,
// EnumerateLeaderboardCells does not finish. Measured on this fixture, the
// verified-email EXISTS costs milliseconds per row (2.5 ms mean, 1 614 buffer
// hits) because the planner satisfies it with the GiST exclusion index rather
// than the btree on user_id (TestLoadPlanProjectionEmailGate has the numbers,
// and the fix), and the enumeration evaluates it once per eligible run. The
// first attempt at this fixture sat in that one statement for 28 minutes and
// was killed.
//
// So the rebuild is measured with the gate OFF — which is the pure "one round
// trip per cell" cost the design trades for a single producer of the bucket key
// — and what the gate adds is measured separately and reported as arithmetic
// over the two, clearly labelled as a projection rather than a stopwatch.
const noEmailGate = false

// statementCounter counts the statements pgx puts on the wire. It is how the
// rebuild's round-trip count is a measurement rather than an inference: the
// design walks cells one statement at a time on purpose (docs/LEADERBOARDS.md,
// "Rebuild"), and the cost of that decision is exactly this number.
//
// It sees Query/QueryRow/Exec — including the implicit `begin`/`commit` — but
// not the extra round trip a first-time Prepare costs, so it is a lower bound.
type statementCounter struct{ n atomic.Int64 }

func (c *statementCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	c.n.Add(1)
	return ctx
}

func (c *statementCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// fixture is the seeded population every load test in this package shares:
// ~1M runs, a 100k-player hot bucket, 500 boards, projected onto
// leaderboard_entries by a real Rebuild.
//
// Built once (sync.Once) because seeding and projecting a million runs per test
// would cost more than the whole suite is allowed to take.
type fixture struct {
	pool *pgxpool.Pool
	// store carries the production email gate; ungated is the same store with
	// it off. Zone 4 measures both, because the difference between them is the
	// single largest cost in this package.
	store   *leaderboardpg.Store
	ungated *leaderboardpg.Store
	seed    perf.SeedResult
	// hot is seed.Hot as the domain sees it — the same string, parsed back
	// through the one producer of the key format.
	hot leaderboard.Bucket

	// entries / hotEntries are what the rebuild actually produced.
	entries    int64
	hotEntries int64
	// eligibleRuns is how many rows leaderboard_eligible_runs admits — the
	// number of times the rebuild's enumeration evaluates the email gate.
	eligibleRuns int64

	// The zone-4 rebuild measurement. It is taken here rather than in the
	// rebuild test because populating the board IS the rebuild: running it a
	// second time just to time it would measure a no-op rebuild against a warm
	// cache, which is the easy case.
	rebuild      leaderboardpg.RebuildStats
	rebuildWall  time.Duration
	rebuildStmts int64
	seedWall     time.Duration

	stmts *statementCounter
}

var (
	fixtureOnce sync.Once
	sharedFix   *fixture
	fixtureErr  error
)

// loadFixture returns the shared population, building it on first use.
func loadFixture(t *testing.T) *fixture {
	t.Helper()
	base := ensureDB(t)
	fixtureOnce.Do(func() { sharedFix, fixtureErr = buildFixture(t, base) })
	require.NoError(t, fixtureErr, "build the shared load fixture")
	return sharedFix
}

func buildFixture(t *testing.T, baseDSN string) (*fixture, error) {
	t.Helper()
	ctx := context.Background()

	dsn, err := createPerfDatabase(ctx, baseDSN)
	if err != nil {
		return nil, err
	}
	if err := migrate.Up(ctx, dsn); err != nil {
		return nil, fmt.Errorf("migrate perf database: %w", err)
	}
	// Production planner constants before the pool opens its sessions — the
	// same SSD-vs-rust reasoning as the profile fixture (perf.SetPlannerCosts).
	if err := perf.SetPlannerCosts(ctx, dsn); err != nil {
		return nil, err
	}

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse perf dsn: %w", err)
	}
	stmts := &statementCounter{}
	cfg.MaxConns = 8
	cfg.ConnConfig.Tracer = stmts
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("open perf pool: %w", err)
	}
	// Deliberately never closed: the pool outlives every test in the package
	// and the container it talks to is terminated by TestMain.

	f := &fixture{
		pool:    pool,
		store:   leaderboardpg.New(pool, requireVerifiedEmail),
		ungated: leaderboardpg.New(pool, noEmailGate),
		stmts:   stmts,
	}

	t.Logf("seeding the load fixture (~1M runs); this is the expensive part")
	if f.seed, err = perf.Seed(ctx, pool, perf.DefaultSeed()); err != nil {
		return nil, err
	}
	f.seedWall = f.seed.Elapsed
	if f.hot, err = leaderboard.ParseBucketKey(f.seed.Hot.Key()); err != nil {
		return nil, fmt.Errorf("hot bucket key: %w", err)
	}

	// Seed leaves the board empty on purpose, so the fixture is populated the
	// way an operator would populate it — and that is a zone-4 measurement.
	// See noEmailGate for why this one rebuild runs ungated.
	before := stmts.n.Load()
	start := time.Now()
	if f.rebuild, err = f.ungated.Rebuild(ctx); err != nil {
		return nil, err
	}
	f.rebuildWall = time.Since(start)
	f.rebuildStmts = stmts.n.Load() - before

	// The reads are measured against a planner that knows what it is looking
	// at; a board nobody has ANALYZEd is a board with default statistics.
	if _, err := pool.Exec(ctx, `ANALYZE leaderboard_entries`); err != nil {
		return nil, fmt.Errorf("analyze leaderboard_entries: %w", err)
	}

	f.entries = f.rebuild.After
	// Through the view, not the table: a banned player still holds a row, and
	// every read test addresses ranks a reader can actually reach.
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM leaderboard_rows WHERE bucket_key = $1`, f.hot.Key(),
	).Scan(&f.hotEntries); err != nil {
		return nil, fmt.Errorf("count hot bucket: %w", err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM leaderboard_eligible_runs`).Scan(&f.eligibleRuns); err != nil {
		return nil, fmt.Errorf("count eligible runs: %w", err)
	}

	perf.Report(t, "fixture", "seed", fmt.Sprintf(
		"%d runs, %d users, %d banned, %d unverified in %s",
		f.seed.TotalRuns, len(f.seed.Users), len(f.seed.BannedUsers),
		len(f.seed.UnverifiedIDs), f.seedWall.Round(time.Millisecond)))
	perf.Report(t, "fixture", "board", fmt.Sprintf(
		"%d entries across %d buckets, hot bucket %q shows %d visible rows, %d eligible runs",
		f.entries, len(f.seed.ColdBuckets)+1, f.hot.Key(), f.hotEntries, f.eligibleRuns))
	return f, nil
}

// createPerfDatabase drops and recreates the load suite's own database next to
// the one the untagged tests use, and returns its DSN.
func createPerfDatabase(ctx context.Context, baseDSN string) (string, error) {
	admin, err := pgx.Connect(ctx, baseDSN)
	if err != nil {
		return "", fmt.Errorf("connect for CREATE DATABASE: %w", err)
	}
	defer func() { _ = admin.Close(ctx) }()

	if _, err := admin.Exec(ctx, `DROP DATABASE IF EXISTS `+perfDatabase); err != nil {
		return "", fmt.Errorf("drop %s: %w", perfDatabase, err)
	}
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+perfDatabase); err != nil {
		return "", fmt.Errorf("create %s: %w", perfDatabase, err)
	}

	u, err := url.Parse(baseDSN)
	if err != nil {
		return "", fmt.Errorf("parse base dsn: %w", err)
	}
	u.Path = "/" + perfDatabase
	return u.String(), nil
}

// --- addressing the fixture ---

// position is one row of the hot ranking, addressed by its 0-based offset.
type position struct {
	offset int64
	cursor leaderboard.Cursor
}

// positionAt finds the keyset position of the row at a given depth.
//
// It uses OFFSET, which is exactly what the read path refuses to do — that is
// the point: locating a deep row is fixture setup, and doing it the slow way
// here keeps the measured call the keyset one.
func (f *fixture) positionAt(t *testing.T, offset int64) position {
	t.Helper()
	var c leaderboard.Cursor
	err := f.pool.QueryRow(context.Background(), `
		SELECT score, achieved_at, user_id
		FROM leaderboard_rows
		WHERE bucket_key = $1
		ORDER BY score DESC, achieved_at ASC, user_id ASC
		OFFSET $2 LIMIT 1`, f.hot.Key(), offset,
	).Scan(&c.Score, &c.AchievedAt, &c.UserID)
	require.NoError(t, err, "locate hot-bucket row at offset %d", offset)
	return position{offset: offset, cursor: c}
}

// visibleHot counts the hot bucket through the view AT CALL TIME.
//
// The number moves during the suite, and legitimately so: the worker-throughput
// test judges runs through a projector carrying the production email gate, and
// a player who never verified an address loses their slot the moment their next
// verdict is projected. A depth captured once at fixture build time is a depth
// that no longer exists by the time the read tests ask for it.
func (f *fixture) visibleHot(t *testing.T) int64 {
	t.Helper()
	var n int64
	require.NoError(t, f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM leaderboard_rows WHERE bucket_key = $1`, f.hot.Key()).Scan(&n))
	return n
}

// verifiedUser creates an account that may hold a board slot.
func (f *fixture) verifiedUser(t *testing.T, name string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	var id uuid.UUID
	require.NoError(t, f.pool.QueryRow(ctx,
		`INSERT INTO users (display_name) VALUES ($1) RETURNING id`, name).Scan(&id))
	_, err := f.pool.Exec(ctx, `
		INSERT INTO auth_identities (user_id, provider, provider_subject, email, email_verified)
		VALUES ($1, 'email', $2, $3::citext, true)`,
		id, name+"@perf.local", name+"@perf.local")
	require.NoError(t, err)
	return id
}

// --- measurement ---

// sample times fn n times and returns every latency. One untimed warm-up pass
// runs first: the first call of a session pays for a connection handshake and a
// statement prepare, and a report that folds those into the p99 is a report
// about pgx's cold start.
func sample(t *testing.T, n int, fn func()) []time.Duration {
	t.Helper()
	fn()
	out := make([]time.Duration, n)
	for i := range out {
		start := time.Now()
		fn()
		out[i] = time.Since(start)
	}
	return out
}

// p99 is the number the budgets are asserted against. p50 says what the query
// usually costs; p99 says what a user occasionally waits, and a board page is a
// public request nobody retries.
func p99(samples []time.Duration) time.Duration { return perf.Percentile(samples, 99) }
