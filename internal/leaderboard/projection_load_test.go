//go:build load

package leaderboard_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	leaderboardpg "github.com/typemore/typemore-server/internal/leaderboard/pgstore"
	"github.com/typemore/typemore-server/internal/perf"
	"github.com/typemore/typemore-server/internal/replay"
	replaypg "github.com/typemore/typemore-server/internal/replay/pgstore"
)

// Zone 4: the write side — the per-verdict projection, the offline rebuild, and
// what the projection costs the worker that carries it.

const zone4 = "zone 4 projection"

// whaleRuns is how many accepted runs the pathological player holds in ONE
// bucket. It is the shape RecomputeLeaderboardCell is most exposed to: the
// statement's job is "this player's best run here", and nothing bounds how many
// runs that has to choose between.
const whaleRuns = 100_000

// --- the pathological player ---

var (
	whaleOnce   sync.Once
	whaleUser   uuid.UUID
	whaleRunID  uuid.UUID
	whaleSeeded time.Duration
)

// seedWhale plants whaleRuns accepted hot-bucket runs for a single new player.
// Built once and left in place: the runs are never projected (every measurement
// rolls its transaction back), so the shared board is untouched.
func seedWhale(t *testing.T, f *fixture) (uuid.UUID, uuid.UUID) {
	t.Helper()
	whaleOnce.Do(func() {
		ctx := context.Background()
		started := time.Now()
		whaleUser = f.verifiedUser(t, "perfwhale")

		setup := perf.MustJSON(perf.BuildSetup(perf.SetupSpec{Mode: "time", DurationMs: 60_000}))
		log := perf.Gzip([]byte(`{"version":1,"events":[]}`))
		validation := []byte(`{"verdict":"valid","flags":[],"policy":{"version":1,"suspicion":0,"threshold":1}}`)
		base := time.Now().UTC().Add(-365 * 24 * time.Hour)
		duration := int32(60_000)
		// A typed nil so pgx sends an int4 NULL rather than an untyped one:
		// runs.word_count is the other half of the mode XOR.
		var noWords *int32

		rows := make([][]any, whaleRuns)
		verdicts := make([][]any, whaleRuns)
		for i := range rows {
			id := uuid.New()
			if i == whaleRuns/2 {
				whaleRunID = id
			}
			at := base.Add(time.Duration(i) * time.Minute)
			rows[i] = []any{
				id, whaleUser, "time", duration, noWords, "en", int64(i), "804728e8",
				setup, []byte(`{"wpm":100,"raw":100,"acc":1}`), []byte(`{"version":2,"total":1000}`),
				int16(2), "accepted", log, int32(64), at,
			}
			verdicts[i] = []any{
				id, whaleUser,
				[]byte(`{"wpm":100.0,"raw":101.0,"accuracy":0.97}`),
				[]byte(fmt.Sprintf(`{"version":2,"total":%d}`, 100+i%9000)),
				validation, "perfbundle", int16(1), at,
			}
		}
		_, err := f.pool.CopyFrom(ctx, pgx.Identifier{"runs"}, []string{
			"id", "user_id", "mode", "duration_ms", "word_count", "lang", "seed",
			"dict_hash", "setup", "client_metrics", "client_score", "score_version",
			"status", "log", "log_bytes", "created_at",
		}, pgx.CopyFromRows(rows))
		require.NoError(t, err, "seed the whale's runs")
		_, err = f.pool.CopyFrom(ctx, pgx.Identifier{"run_verdicts"}, []string{
			"run_id", "user_id", "server_metrics", "server_score", "validation",
			"bundle_sha", "policy_version", "validated_at",
		}, pgx.CopyFromRows(verdicts))
		require.NoError(t, err, "seed the whale's verdicts")

		// Without fresh statistics the planner still believes every user holds
		// ~8 runs, and would pick a plan for a player who does not exist.
		_, err = f.pool.Exec(ctx, `ANALYZE runs, run_verdicts`)
		require.NoError(t, err)
		whaleSeeded = time.Since(started)
	})
	perf.Report(t, zone4, "whale fixture", fmt.Sprintf(
		"%d accepted runs in %s for one player in %s", whaleRuns, f.hot.Key(),
		whaleSeeded.Round(time.Millisecond)))
	return whaleUser, whaleRunID
}

// TestLoadProjectionRecompute measures the statement that runs on EVERY verdict,
// inside the replay worker's transaction: for a typical player, for one with
// 100 000 runs in the same bucket, and with the verified-email gate both on
// (the production default) and off.
//
// Each measurement is a real ProjectRun in a transaction that is rolled back:
// that is exactly what the worker does (minus the commit), and rolling back
// keeps the shared fixture's board intact.
func TestLoadProjectionRecompute(t *testing.T) {
	f := loadFixture(t)
	typicalRun := typicalHotRun(t, f)

	ungated := sample(t, 100, func() { projectInRolledBackTx(t, f, f.ungated, typicalRun) })
	perf.Report(t, zone4, "ProjectRun, typical player, email gate OFF", perf.Summary(ungated))

	gated := sample(t, 100, func() { projectInRolledBackTx(t, f, f.store, typicalRun) })
	perf.Report(t, zone4, "ProjectRun, typical player, email gate ON (default)", perf.Summary(gated))

	perf.Budget{
		Zone:     zone4,
		Workload: "ProjectRun for a typical player, production configuration",
		Limit:    5 * time.Millisecond,
		Rationale: "it runs once per verdict inside the worker's transaction, and the " +
			"transaction holds FOR UPDATE row locks for the whole batch " +
			"(docs/REPLAY.md): every millisecond here is a millisecond of queue lock.",
	}.Assert(t, p99(gated))

	perf.Report(t, zone4, "cost of the verified-email gate on one projection", fmt.Sprintf(
		"+%s p50 (%s → %s)",
		(perf.Percentile(gated, 50)-perf.Percentile(ungated, 50)).Round(time.Microsecond),
		perf.Percentile(ungated, 50).Round(time.Microsecond),
		perf.Percentile(gated, 50).Round(time.Microsecond)))

	_, runID := seedWhale(t, f)
	whaleUngated := sample(t, 10, func() { projectInRolledBackTx(t, f, f.ungated, runID) })
	perf.Report(t, zone4, fmt.Sprintf("ProjectRun, %d runs in the cell, gate OFF", whaleRuns), perf.Summary(whaleUngated))

	// One sample, and no warm-up pass: in the production configuration this
	// single statement runs for MINUTES (the gate below is evaluated once per
	// candidate run, not once per player), and a distribution over something
	// that costs five minutes a draw is not worth twenty of them.
	start := time.Now()
	projectInRolledBackTx(t, f, f.store, runID)
	whaleGated := time.Since(start)
	perf.Report(t, zone4, fmt.Sprintf("ProjectRun, %d runs in the cell, gate ON (n=1)", whaleRuns),
		whaleGated.Round(time.Millisecond))

	perf.Budget{
		Zone:     zone4,
		Workload: fmt.Sprintf("ProjectRun for a player with %d runs in one bucket", whaleRuns),
		Limit:    10 * time.Millisecond,
		Rationale: "the phase brief's 'single-digit milliseconds'. The statement asks " +
			"for one row — the player's best — so its cost should be a function of " +
			"the index, not of how many runs that player has.",
	}.Assert(t, whaleGated)

	perf.Report(t, zone4, "whale/typical p50 ratio (gate off, so this is the run scan alone)",
		fmt.Sprintf("%.1fx", float64(perf.Percentile(whaleUngated, 50))/float64(perf.Percentile(ungated, 50))))
	perf.Report(t, zone4, "cost of the verified-email gate on the whale's projection", fmt.Sprintf(
		"+%s (%s → %s): the gate is a per-ROW filter, so it is paid once per candidate run",
		(whaleGated-perf.Percentile(whaleUngated, 50)).Round(time.Millisecond),
		perf.Percentile(whaleUngated, 50).Round(time.Millisecond),
		whaleGated.Round(time.Millisecond)))
}

// projectInRolledBackTx runs one projection the way the worker does and throws
// the result away.
func projectInRolledBackTx(t *testing.T, f *fixture, store *leaderboardpg.Store, runID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	tx, err := f.pool.Begin(ctx)
	require.NoError(t, err)
	require.NoError(t, store.ProjectRun(ctx, tx, runID))
	require.NoError(t, tx.Rollback(ctx))
}

// typicalHotRun is an accepted hot-bucket run belonging to a player who already
// holds a slot there: six runs in the cell, which is what the worker sees
// essentially always. Every projection measurement uses the SAME run, so the
// numbers can be compared to each other.
func typicalHotRun(t *testing.T, f *fixture) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	require.NoError(t, f.pool.QueryRow(context.Background(), `
		SELECT r.id FROM runs r
		JOIN leaderboard_entries e ON e.user_id = r.user_id AND e.bucket_key = $1
		WHERE r.mode = 'time' AND r.duration_ms = 60000 AND r.lang = 'en' AND r.status = 'accepted'
		LIMIT 1`, f.hot.Key()).Scan(&id))
	return id
}

// userOf is the player a run belongs to.
func userOf(t *testing.T, f *fixture, runID uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	require.NoError(t, f.pool.QueryRow(context.Background(),
		`SELECT user_id FROM runs WHERE id = $1`, runID).Scan(&id))
	return id
}

// recomputeOnce runs one maintenance statement for one player's hot-bucket cell
// in a transaction it rolls back — the projection's write half, without the
// RunBucketCell lookup in front of it, so two spellings of the statement can be
// compared against each other.
func recomputeOnce(t *testing.T, f *fixture, sql string, user uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	tx, err := f.pool.Begin(ctx)
	require.NoError(t, err)
	duration := int32(60_000)
	var noWords *int32
	// The hot bucket is a language board, so its quote coordinate is absent —
	// which is also the coordinate that keeps quote runs out of it.
	var noQuote *uuid.UUID
	_, err = tx.Exec(ctx, sql, f.hot.Key(), user, noQuote, f.hot.Mode, duration, noWords,
		f.hot.Lang, f.hot.TextSource, requireVerifiedEmail)
	require.NoError(t, err)
	require.NoError(t, tx.Rollback(ctx))
}

// --- plan assertions ---
//
// Copied VERBATIM from the sqlc output in leaderboarddb/queries.sql.go.

const sqlRecomputeCell = `WITH best AS (
    SELECT e.run_id, e.user_id, e.mode, e.duration_ms, e.word_count, e.lang, e.text_source_kind, e.quote_id, e.score, e.wpm, e.raw, e.acc, e.mods, e.achieved_at
    FROM leaderboard_eligible_runs e
    WHERE e.user_id = $2
      AND e.quote_id IS NOT DISTINCT FROM $3::uuid
      AND (e.quote_id IS NOT NULL
           OR (e.mode = $4
               AND e.duration_ms IS NOT DISTINCT FROM $5::int
               AND e.word_count IS NOT DISTINCT FROM $6::int
               AND e.lang = $7
               AND e.text_source_kind = $8))
      -- The economic barrier against throwaway accounts. Off by config, and
      -- flipping it needs a rebuild to take effect on existing runs.
      --
      -- Note ` + "`" + `ai.user_id = @user_id` + "`" + `, NOT ` + "`" + `= e.user_id` + "`" + `. The two are the same
      -- value — ` + "`" + `e.user_id = @user_id` + "`" + ` is asserted five lines above — but
      -- correlating on the column makes this a SubPlan the executor runs once
      -- per CANDIDATE RUN instead of an InitPlan it runs once per statement.
      -- Measured (docs/PERFORMANCE.md, zone 4): a six-run cell 34.6 ms → 8.0 ms,
      -- and a player with 100 000 runs in one bucket 1 m 30 s → 458 ms, which is
      -- the gate costing nothing at all. This projection runs inside the replay
      -- worker's verdict transaction, holding its row locks, so the difference
      -- is not academic.
      AND (NOT $9::boolean
           OR EXISTS (SELECT 1 FROM auth_identities ai
                      WHERE ai.user_id = $2 AND ai.email_verified))
    ORDER BY e.score DESC, e.achieved_at ASC, e.run_id ASC
    LIMIT 1
),
cleared AS (
    DELETE FROM leaderboard_entries le
    WHERE le.bucket_key = $1
      AND le.user_id = $2
      AND NOT EXISTS (SELECT 1 FROM best)
)
INSERT INTO leaderboard_entries
    (bucket_key, user_id, run_id, score, wpm, raw, acc, grade, mods, achieved_at,
     quote_source)
SELECT $1, b.user_id, b.run_id, b.score, b.wpm, b.raw, b.acc,
       run_grade(b.acc), b.mods, b.achieved_at, q.source
FROM best b
         LEFT JOIN quotes q ON q.id = b.quote_id
ON CONFLICT (bucket_key, user_id) DO UPDATE
    SET run_id      = EXCLUDED.run_id,
        score       = EXCLUDED.score,
        wpm         = EXCLUDED.wpm,
        raw         = EXCLUDED.raw,
        acc         = EXCLUDED.acc,
        grade       = EXCLUDED.grade,
        mods        = EXCLUDED.mods,
        achieved_at = EXCLUDED.achieved_at,
        quote_source = EXCLUDED.quote_source
    -- An unchanged best is left physically alone.
    WHERE leaderboard_entries.run_id <> EXCLUDED.run_id`

// sqlRecomputeCellCorrelated is the spelling this suite MEASURED AND REPLACED:
// the gate's EXISTS correlated on e.user_id rather than on the $2 parameter,
// which made it a SubPlan the executor ran once per candidate row instead of an
// InitPlan it runs once. The two are semantically identical — `e.user_id = $2`
// is asserted four lines above — and the shipped query now uses $2.
//
// It stays here as the control arm: the comparison in
// TestLoadPlanProjectionEmailGate is what justified the change, and it is what
// makes a revert show up as a number rather than as a slow week.
const sqlRecomputeCellCorrelated = `WITH best AS (
    SELECT e.run_id, e.user_id, e.mode, e.duration_ms, e.word_count, e.lang, e.text_source_kind, e.quote_id, e.score, e.wpm, e.raw, e.acc, e.mods, e.achieved_at
    FROM leaderboard_eligible_runs e
    WHERE e.user_id = $2
      AND e.quote_id IS NOT DISTINCT FROM $3::uuid
      AND (e.quote_id IS NOT NULL
           OR (e.mode = $4
               AND e.duration_ms IS NOT DISTINCT FROM $5::int
               AND e.word_count IS NOT DISTINCT FROM $6::int
               AND e.lang = $7
               AND e.text_source_kind = $8))
      -- The economic barrier against throwaway accounts. Off by config, and
      -- flipping it needs a rebuild to take effect on existing runs.
      --
      -- Note ` + "`" + `ai.user_id = @user_id` + "`" + `, NOT ` + "`" + `= e.user_id` + "`" + `. The two are the same
      -- value — ` + "`" + `e.user_id = @user_id` + "`" + ` is asserted five lines above — but
      -- correlating on the column makes this a SubPlan the executor runs once
      -- per CANDIDATE RUN instead of an InitPlan it runs once per statement.
      -- Measured (docs/PERFORMANCE.md, zone 4): a six-run cell 34.6 ms → 8.0 ms,
      -- and a player with 100 000 runs in one bucket 1 m 30 s → 458 ms, which is
      -- the gate costing nothing at all. This projection runs inside the replay
      -- worker's verdict transaction, holding its row locks, so the difference
      -- is not academic.
      AND (NOT $9::boolean
           OR EXISTS (SELECT 1 FROM auth_identities ai
                      WHERE ai.user_id = e.user_id AND ai.email_verified))
    ORDER BY e.score DESC, e.achieved_at ASC, e.run_id ASC
    LIMIT 1
),
cleared AS (
    DELETE FROM leaderboard_entries le
    WHERE le.bucket_key = $1
      AND le.user_id = $2
      AND NOT EXISTS (SELECT 1 FROM best)
)
INSERT INTO leaderboard_entries
    (bucket_key, user_id, run_id, score, wpm, raw, acc, grade, mods, achieved_at,
     quote_source)
SELECT $1, b.user_id, b.run_id, b.score, b.wpm, b.raw, b.acc,
       run_grade(b.acc), b.mods, b.achieved_at, q.source
FROM best b
         LEFT JOIN quotes q ON q.id = b.quote_id
ON CONFLICT (bucket_key, user_id) DO UPDATE
    SET run_id      = EXCLUDED.run_id,
        score       = EXCLUDED.score,
        wpm         = EXCLUDED.wpm,
        raw         = EXCLUDED.raw,
        acc         = EXCLUDED.acc,
        grade       = EXCLUDED.grade,
        mods        = EXCLUDED.mods,
        achieved_at = EXCLUDED.achieved_at,
        quote_source = EXCLUDED.quote_source
    -- An unchanged best is left physically alone.
    WHERE leaderboard_entries.run_id <> EXCLUDED.run_id`

const sqlEnumerateCells = `SELECT DISTINCT e.user_id, e.mode, e.duration_ms, e.word_count, e.lang,
                e.text_source_kind, e.quote_id
FROM leaderboard_eligible_runs e
WHERE (NOT $1::boolean
       OR EXISTS (SELECT 1 FROM auth_identities ai
                  WHERE ai.user_id = e.user_id AND ai.email_verified))
ORDER BY e.user_id, e.mode, e.duration_ms, e.word_count, e.lang,
         e.text_source_kind, e.quote_id`

// sqlEmailGate is the verified-email EXISTS on its own, exactly as it appears
// inside both RecomputeLeaderboardCell and EnumerateLeaderboardCells.
const sqlEmailGate = `SELECT EXISTS (SELECT 1 FROM auth_identities ai
                WHERE ai.user_id = $1 AND ai.email_verified)`

// proposedGateIndex is the index this suite ends up proposing. It is never
// created outside a transaction that is rolled back — the schema is not this
// zone's to change.
const proposedGateIndex = `CREATE INDEX auth_identities_verified_user_idx
    ON auth_identities (user_id) WHERE email_verified`

// TestLoadPlanProjectionRecompute pins the shape of the per-verdict statement
// against the pathological player. It writes, so it is planned rather than
// executed (EXPLAIN without ANALYZE) inside a transaction that is rolled back
// anyway.
//
// The interesting half is `best`: ORDER BY score DESC, achieved_at, run_id
// LIMIT 1 over one player's eligible runs. score is
// (server_score->>'total')::bigint — an expression, not a column — so no index
// can supply that order, and the only question is how many of the player's runs
// have to be read to sort it.
func TestLoadPlanProjectionRecompute(t *testing.T) {
	f := loadFixture(t)
	ctx := context.Background()
	user, _ := seedWhale(t, f)

	tx, err := f.pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()

	duration := int32(60_000)
	var noWords *int32
	var noQuote *uuid.UUID
	plan, err := perf.ExplainOnly(ctx, tx, sqlRecomputeCell,
		f.hot.Key(), user, noQuote, f.hot.Mode, duration, noWords, f.hot.Lang,
		f.hot.TextSource, requireVerifiedEmail)
	require.NoError(t, err)
	perf.AssertPlan(t, plan, perf.PlanAssertion{
		Zone:        zone4,
		Query:       fmt.Sprintf("RecomputeLeaderboardCell (player with %d runs in the cell)", whaleRuns),
		WantAny:     []string{"Index Scan", "Index Only Scan", "Bitmap Index Scan"},
		NoSeqScanOn: []string{"runs", "leaderboard_entries"},
	})
	perf.Report(t, zone4, "recompute plan", fmt.Sprintf("%v", plan.Nodes))
	perf.Report(t, zone4, "recompute plan (raw)", "\n"+plan.Raw)

	// WantAny cannot tell a good index from a bad one, and here the difference
	// is three orders of magnitude — see TestLoadPlanProjectionEmailGate.
	if strings.Contains(plan.Raw, "verified_email_one_user") {
		perf.Report(t, zone4, "FINDING",
			"the email gate inside the recompute is served by the GiST exclusion index "+
				"verified_email_one_user, not by a btree on user_id")
	}

	// The gate must be an InitPlan — evaluated once for the statement — and not
	// a SubPlan the executor re-runs for every candidate row. That distinction
	// is the whole difference between 8 ms and a minute and a half for this
	// cell, and it survives only as long as the EXISTS correlates on the
	// PARAMETER rather than on e.user_id (see sqlRecomputeCellCorrelated).
	if strings.Contains(plan.Raw, `"Parent Relationship": "SubPlan"`) {
		t.Errorf("PLAN %s | RecomputeLeaderboardCell | the verified-email gate is a per-row SubPlan again; "+
			"it must correlate on the $2 parameter so the planner hoists it to an InitPlan\nplan:\n%s",
			zone4, plan.Raw)
	}
	perf.Report(t, zone4, "gate is hoisted",
		fmt.Sprintf("InitPlan present=%t, per-row SubPlan present=%t",
			strings.Contains(plan.Raw, `"Parent Relationship": "InitPlan"`),
			strings.Contains(plan.Raw, `"Parent Relationship": "SubPlan"`)))
}

// TestLoadPlanProjectionEmailGate isolates the single most expensive thing in
// this package: `EXISTS (SELECT 1 FROM auth_identities WHERE user_id = $1 AND
// email_verified)`, the deployment-policy gate that travels with every
// projection query.
//
// auth_identities carries a btree on user_id, but it also carries
// `verified_email_one_user`, the GiST exclusion index over
// (lower(email), user_id) WHERE email_verified that enforces "one verified
// owner per address" (00001_init_auth.sql). The planner prefers the GiST one
// because it is partial and covering, so the lookup is an *Index Only Scan*
// and costs 8.32 — and then executes as a search on the index's SECOND key,
// which GiST cannot descend on, so it walks the whole index.
//
// The counterfactual is measured rather than argued: the proposed index is
// created, ProjectRun is re-timed through it, and it is dropped again. Whole
// projections rather than bare lookups — a single lookup's wall time is mostly
// the round trip to the container, while the projection pays the gate once per
// candidate run inside one statement and so measures the server.
func TestLoadPlanProjectionEmailGate(t *testing.T) {
	f := loadFixture(t)
	ctx := context.Background()

	users := f.seed.Users[:32]
	asIs := make([]time.Duration, 0, len(users))
	for _, u := range users {
		start := time.Now()
		var ok bool
		require.NoError(t, f.pool.QueryRow(ctx, sqlEmailGate, u).Scan(&ok))
		asIs = append(asIs, time.Since(start))
	}
	perf.Report(t, zone4, "verified-email gate, one lookup, current schema", perf.Summary(asIs))

	plan, err := perf.Explain(ctx, f.pool, sqlEmailGate, users[0])
	require.NoError(t, err)
	perf.Report(t, zone4, "gate plan, current schema", fmt.Sprintf("%v in %.2f ms", plan.Nodes, plan.TotalMs))
	perf.Report(t, zone4, "gate plan (raw)", "\n"+plan.Raw)

	// The counterfactual, on the same run the headline measurement used and
	// through the same code path — store.ProjectRun in a fresh transaction each
	// time, which is exactly what the worker does. The index is created for
	// real and dropped again, because an index that only exists inside one
	// transaction is invisible to the fresh transactions being measured, and a
	// comparison against a path nobody runs proves nothing.
	run := typicalHotRun(t, f)
	before := sample(t, 30, func() { projectInRolledBackTx(t, f, f.store, run) })
	perf.Report(t, zone4, "ProjectRun, gate ON, current schema", perf.Summary(before))

	_, err = f.pool.Exec(ctx, proposedGateIndex)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := f.pool.Exec(context.Background(), `DROP INDEX auth_identities_verified_user_idx`)
		require.NoError(t, err, "the proposed index must not outlive this test")
	})
	_, err = f.pool.Exec(ctx, `ANALYZE auth_identities`)
	require.NoError(t, err)

	after := sample(t, 30, func() { projectInRolledBackTx(t, f, f.store, run) })
	perf.Report(t, zone4, "ProjectRun, gate ON, WITH the proposed index", perf.Summary(after))
	perf.Report(t, zone4, "proposed index", strings.Join(strings.Fields(proposedGateIndex), " "))
	perf.Report(t, zone4, "speed-up from the proposed index", fmt.Sprintf("%.1fx (%s → %s p50)",
		float64(perf.Percentile(before, 50))/float64(perf.Percentile(after, 50)),
		perf.Percentile(before, 50).Round(time.Microsecond),
		perf.Percentile(after, 50).Round(time.Microsecond)))

	withIndex, err := perf.Explain(ctx, f.pool, sqlEmailGate, users[0])
	require.NoError(t, err)
	perf.Report(t, zone4, "gate plan, with the proposed index",
		fmt.Sprintf("%v in %.2f ms", withIndex.Nodes, withIndex.TotalMs))
	perf.Report(t, zone4, "gate plan with the proposed index (raw)", "\n"+withIndex.Raw)

	// The index is not the fix. This is: the gate USED to correlate on
	// `e.user_id`, which made it a SubPlan the executor ran once per candidate
	// row — even though `e.user_id = $2` is asserted two lines above it, so the
	// answer is the same for every row. The shipped query now correlates on the
	// PARAMETER, which makes the subquery uncorrelated, and an uncorrelated
	// subquery is an InitPlan: evaluated once per statement. Identical
	// semantics, one identifier.
	//
	// Both arms still see the proposed index created above — it was just
	// measured to make no difference.
	user := userOf(t, f, run)
	shipped := sample(t, 30, func() { recomputeOnce(t, f, sqlRecomputeCell, user) })
	correlated := sample(t, 30, func() { recomputeOnce(t, f, sqlRecomputeCellCorrelated, user) })
	perf.Report(t, zone4, "recompute, gate correlated on e.user_id (the old spelling)", perf.Summary(correlated))
	perf.Report(t, zone4, "recompute, gate correlated on $2 (as shipped)", perf.Summary(shipped))
	perf.Report(t, zone4, "speed-up from un-correlating the gate", fmt.Sprintf("%.1fx (%s → %s p50)",
		float64(perf.Percentile(correlated, 50))/float64(perf.Percentile(shipped, 50)),
		perf.Percentile(correlated, 50).Round(time.Microsecond),
		perf.Percentile(shipped, 50).Round(time.Microsecond)))

	// And on the player the per-row multiplication actually hurt. Only the
	// shipped spelling is run here: the old one cost 1 m 30 s – 8 m 01 s for
	// this cell across five runs, and re-establishing that is minutes of CPU
	// to reproduce a number the report already carries.
	whale, _ := seedWhale(t, f)
	whaleShipped := sample(t, 3, func() { recomputeOnce(t, f, sqlRecomputeCell, whale) })
	perf.Report(t, zone4, fmt.Sprintf("recompute for the %d-run cell, as shipped", whaleRuns),
		perf.Summary(whaleShipped))

	var total time.Duration
	for _, d := range asIs {
		total += d
	}
	perf.Budget{
		Zone:     zone4,
		Workload: "verified-email gate, one lookup (mean)",
		Limit:    time.Millisecond,
		Rationale: "it is an equality lookup on an indexed uuid column, and it sits in " +
			"a per-ROW filter: once per candidate run in every projection, once per " +
			"eligible run in the rebuild's enumeration. The MEAN is the statistic " +
			"that matters when a cost is summed a million times.",
	}.Assert(t, total/time.Duration(len(asIs)))
}

// TestLoadPlanRebuildEnumerate records the rebuild's one set-based step.
//
// It is a DISTINCT over every eligible run in the database, so a sort and a
// sequential scan are the correct plan and are not asserted against; what is
// asserted is that it does not spill to disk, because a rebuild that exceeds
// work_mem stops being bounded by CPU. The email-gated variant is only PLANNED,
// never executed: at this volume it does not finish (see noEmailGate).
func TestLoadPlanRebuildEnumerate(t *testing.T) {
	f := loadFixture(t)
	ctx := context.Background()

	// The EXPLAIN runs under the same session the shipped path uses:
	// Store.Rebuild grants its transaction `SET LOCAL work_mem = '256MB'`
	// before enumerating (pgstore.go carries the why), and a plan measured
	// under a different work_mem than production runs would pin nothing real.
	tx, err := f.pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `SET LOCAL work_mem = '256MB'`)
	require.NoError(t, err)

	start := time.Now()
	plan, err := perf.Explain(ctx, tx, sqlEnumerateCells, noEmailGate)
	require.NoError(t, err)
	perf.AssertPlan(t, plan, perf.PlanAssertion{Zone: zone4, Query: "EnumerateLeaderboardCells (gate off)"})
	perf.Report(t, zone4, "enumerate cells, gate off", fmt.Sprintf(
		"%v, %.0f ms of planner-reported execution, %s wall, sorts=%v",
		plan.Nodes, plan.TotalMs, time.Since(start).Round(time.Millisecond), plan.SortMethods))

	gated, err := perf.ExplainOnly(ctx, tx, sqlEnumerateCells, requireVerifiedEmail)
	require.NoError(t, err)
	perf.Report(t, zone4, "enumerate cells, gate on (planned, not executed)", fmt.Sprintf("%v", gated.Nodes))
	perf.Report(t, zone4, "enumerate cells plan, gate on (raw)", "\n"+gated.Raw)
}

// TestLoadRebuild reports the rebuild that populated the fixture: wall time,
// statements on the wire, cells per second.
//
// The budget is not "a maintenance command may take as long as it likes".
// ClearLeaderboard is a TRUNCATE, which takes ACCESS EXCLUSIVE on
// leaderboard_entries and holds it until the transaction commits — and every
// read goes through a view over that table. The rebuild's wall time IS the
// board's downtime, so the ceiling is what an operator may take the public
// board offline for without scheduling a maintenance window.
func TestLoadRebuild(t *testing.T) {
	f := loadFixture(t)

	cellsPerSec := float64(f.rebuild.Cells) / f.rebuildWall.Seconds()
	perf.Report(t, zone4, "rebuild (email gate OFF)", fmt.Sprintf(
		"%d cells, %d entries before / %d after, %s wall, %.0f cells/s",
		f.rebuild.Cells, f.rebuild.Before, f.rebuild.After,
		f.rebuildWall.Round(time.Millisecond), cellsPerSec))
	perf.Report(t, zone4, "rebuild round trips", fmt.Sprintf(
		"%d statements for %d cells (%.2f per cell, %.0f µs each)",
		f.rebuildStmts, f.rebuild.Cells,
		float64(f.rebuildStmts)/float64(f.rebuild.Cells),
		f.rebuildWall.Seconds()*1e6/float64(f.rebuildStmts)))

	// The design is one statement per cell (docs/LEADERBOARDS.md, "Rebuild") —
	// asserted, so that a future "optimisation" that makes the rebuild set-based
	// without moving the bucket-key format into SQL has to say so here.
	require.Greater(t, f.rebuildStmts, int64(f.rebuild.Cells),
		"the rebuild is documented as one round trip per cell")

	perf.Budget{
		Zone:     zone4,
		Workload: fmt.Sprintf("rebuild of %d cells from %d runs, email gate OFF", f.rebuild.Cells, f.seed.TotalRuns),
		Limit:    60 * time.Second,
		Rationale: "TRUNCATE holds ACCESS EXCLUSIVE on leaderboard_entries until the " +
			"rebuild commits, and every read goes through a view over that table, so " +
			"this number is board downtime. A minute is the most an operator should " +
			"be able to take the public board offline without planning to.",
	}.Assert(t, f.rebuildWall)

	// What the production configuration would cost. This is arithmetic over two
	// measurements, not a stopwatch, and is labelled as such.
	//
	// Since the gate was un-correlated in RecomputeLeaderboardCell the recompute
	// loop pays it once per CELL. EnumerateLeaderboardCells still spells it the
	// old way — the same hoist does not apply there, because that statement is
	// genuinely per-user across every player — so the enumeration still pays it
	// once per eligible RUN, and that term dominates.
	gate := f.gateCost(t)
	enumerate := time.Duration(f.eligibleRuns) * gate
	recompute := time.Duration(f.rebuild.Cells) * gate
	perf.Report(t, zone4, "PROJECTED rebuild with the email gate ON (the default)", fmt.Sprintf(
		"%s = %s measured + %d eligible runs × %s (enumeration) + %d cells × %s (recompute loop)",
		(f.rebuildWall+enumerate+recompute).Round(time.Second),
		f.rebuildWall.Round(time.Second),
		f.eligibleRuns, gate.Round(time.Microsecond),
		f.rebuild.Cells, gate.Round(time.Microsecond)))

	assertRebuildBlocksReads(t, f)
}

// gateCost is the measured cost of one verified-email lookup, cached so the
// rebuild's projection and the throughput attribution agree on one number.
func (f *fixture) gateCost(t *testing.T) time.Duration {
	t.Helper()
	gateOnce.Do(func() {
		ctx := context.Background()
		samples := make([]time.Duration, 0, 32)
		for _, u := range f.seed.Users[:32] {
			start := time.Now()
			var ok bool
			require.NoError(t, f.pool.QueryRow(ctx, sqlEmailGate, u).Scan(&ok))
			samples = append(samples, time.Since(start))
		}
		gateMeasured = perf.Percentile(samples, 50)
	})
	return gateMeasured
}

var (
	gateOnce     sync.Once
	gateMeasured time.Duration
)

// assertRebuildBlocksReads demonstrates the claim the budget rests on: while a
// rebuild's TRUNCATE is uncommitted, a board page cannot be served.
//
// It fakes only the TRUNCATE, not the whole rebuild — the lock is taken by the
// first statement and held to commit, so a rolled-back one proves the same
// thing in a second instead of in minutes.
func assertRebuildBlocksReads(t *testing.T, f *fixture) {
	t.Helper()
	ctx := context.Background()

	tx, err := f.pool.Begin(ctx)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `TRUNCATE leaderboard_entries`)
	require.NoError(t, err)

	const wait = 2 * time.Second
	blocked, cancel := context.WithTimeout(ctx, wait)
	start := time.Now()
	_, readErr := f.store.Page(blocked, f.hot, nil, pageLimit)
	waited := time.Since(start)
	cancel()

	require.NoError(t, tx.Rollback(ctx), "the rollback is what puts the board back")
	require.Error(t, readErr, "a page read during an uncommitted rebuild must not succeed")
	require.GreaterOrEqual(t, waited, wait-100*time.Millisecond,
		"the read should have waited on the lock, not failed for another reason: %v", readErr)
	perf.Report(t, zone4, "rebuild lock", fmt.Sprintf(
		"a board page blocked for the whole %s it was given (TRUNCATE holds ACCESS EXCLUSIVE until commit): %v",
		wait, readErr))

	var after int64
	require.NoError(t, f.pool.QueryRow(ctx, `SELECT count(*) FROM leaderboard_entries`).Scan(&after))
	require.Equal(t, f.entries, after, "rolling the TRUNCATE back must restore the board")
}

// --- worker throughput ---

// throughputRuns is how many pending runs each arm of the comparison judges.
const throughputRuns = 200

// throughputBatch matches the worker's default claim size.
const throughputBatch = 20

// TestLoadProjectionWorkerThroughput measures what carrying the projection costs
// the replay worker: the same batches through replaypg.New(pool, projector) and
// replaypg.New(pool, nil), with the projector configured both ways.
//
// `decide` is trivial on purpose. Running the real goja replay would put tens of
// milliseconds of interpreter time in front of a sub-millisecond statement and
// hide exactly the thing being isolated — this measures the PROJECTION's cost,
// not the replay's, so the ratio below is the worst case the projection can ever
// represent and the absolute per-run number is the one that transfers.
func TestLoadProjectionWorkerThroughput(t *testing.T) {
	f := loadFixture(t)
	ctx := context.Background()

	arms := []struct {
		name  string
		queue *replaypg.Queue
		took  time.Duration
		stmts int64
	}{
		{name: "no projector", queue: replaypg.New(f.pool, nil)},
		{name: "projector, email gate OFF", queue: replaypg.New(f.pool, f.ungated)},
		{name: "projector, email gate ON (default)", queue: replaypg.New(f.pool, f.store)},
	}
	seedPendingRuns(t, f, len(arms)*throughputRuns)

	// Alternating the arms rather than running one after the other: the first
	// arm would otherwise pay for a cold cache and the last would look faster
	// for reasons that have nothing to do with the projection.
	for range throughputRuns / throughputBatch {
		for i := range arms {
			d, s := timeBatch(ctx, t, f, arms[i].queue, throughputBatch)
			arms[i].took += d
			arms[i].stmts += s
		}
	}

	for _, a := range arms {
		perf.Report(t, zone4, "worker: "+a.name, fmt.Sprintf(
			"%d runs in %s (%.0f runs/s, %.1f statements/run)",
			throughputRuns, a.took.Round(time.Millisecond),
			float64(throughputRuns)/a.took.Seconds(), float64(a.stmts)/throughputRuns))
	}

	bare, ungated, gated := arms[0].took, arms[1].took, arms[2].took
	perf.Report(t, zone4, "projection share of a bare transaction", fmt.Sprintf(
		"gate off %.0f%%, gate on %.0f%%",
		float64(ungated-bare)/float64(bare)*100, float64(gated-bare)/float64(bare)*100))

	// The threshold is absolute, not a percentage of the bare transaction.
	// A percentage against a `decide` that does nothing measures how little the
	// baseline does; what the worker actually has to absorb is a per-run cost
	// sitting next to a goja replay whose interrupt budget is 5 s
	// (TYPEMORE_REPLAY_TIMEOUT) and whose typical cost is milliseconds of
	// interpretation. 2 ms per run keeps the projection a rounding error on a
	// real verdict and still catches it degrading by an order of magnitude.
	perf.Budget{
		Zone:      zone4,
		Workload:  "projection overhead per judged run, production configuration",
		Limit:     2 * time.Millisecond,
		Rationale: "must stay a rounding error next to the goja replay it rides along with",
	}.Assert(t, (gated-bare)/throughputRuns)

	perf.Report(t, zone4, "projection overhead per judged run", fmt.Sprintf(
		"gate off +%s, gate on +%s",
		((ungated-bare)/throughputRuns).Round(time.Microsecond),
		((gated-bare)/throughputRuns).Round(time.Microsecond)))
}

// timeBatch runs one ProcessBatch and reports what it cost and how many
// statements it put on the wire.
func timeBatch(ctx context.Context, t *testing.T, f *fixture, q *replaypg.Queue, limit int32) (time.Duration, int64) {
	t.Helper()
	before := f.stmts.n.Load()
	start := time.Now()
	n, err := q.ProcessBatch(ctx, limit, decideAccepted)
	took := time.Since(start)
	require.NoError(t, err)
	require.Equal(t, int(limit), n, "the queue ran dry")
	return took, f.stmts.n.Load() - before
}

// decideAccepted is a verdict with no replay behind it — see the note on
// TestLoadProjectionWorkerThroughput.
func decideAccepted(context.Context, replay.PendingRun) replay.Decision {
	return replay.Decision{
		Status:        "accepted",
		ServerMetrics: []byte(`{"wpm":100.0,"raw":101.0,"accuracy":0.97}`),
		ServerScore:   []byte(`{"version":2,"total":4242}`),
		Validation:    []byte(`{"verdict":"valid","flags":[],"policy":{"version":1,"suspicion":0,"threshold":1}}`),
		BundleSHA:     "perfbundle",
		PolicyVersion: 1,
	}
}

// seedPendingRuns plants n pending hot-bucket runs for players who already hold
// a slot there, so the projection has a real cell to recompute rather than an
// empty one.
func seedPendingRuns(t *testing.T, f *fixture, n int) {
	t.Helper()
	ctx := context.Background()

	rows, err := f.pool.Query(ctx,
		`SELECT user_id FROM leaderboard_entries WHERE bucket_key = $1 LIMIT $2`, f.hot.Key(), n)
	require.NoError(t, err)
	users, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	require.NoError(t, err)
	require.Len(t, users, n)

	setup := perf.MustJSON(perf.BuildSetup(perf.SetupSpec{Mode: "time", DurationMs: 60_000}))
	log := perf.Gzip([]byte(`{"version":1,"events":[]}`))
	duration := int32(60_000)
	var noWords *int32
	at := time.Now().UTC()

	pending := make([][]any, n)
	for i, u := range users {
		pending[i] = []any{
			uuid.New(), u, "time", duration, noWords, "en", int64(i), "804728e8",
			setup, []byte(`{"wpm":100,"raw":100,"acc":1}`), []byte(`{"version":2,"total":1000}`),
			int16(2), "pending", log, int32(64), at.Add(time.Duration(i) * time.Millisecond),
		}
	}
	_, err = f.pool.CopyFrom(ctx, pgx.Identifier{"runs"}, []string{
		"id", "user_id", "mode", "duration_ms", "word_count", "lang", "seed",
		"dict_hash", "setup", "client_metrics", "client_score", "score_version",
		"status", "log", "log_bytes", "created_at",
	}, pgx.CopyFromRows(pending))
	require.NoError(t, err, "seed pending runs")
}
