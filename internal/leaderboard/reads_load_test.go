//go:build load

package leaderboard_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/typemore/typemore-server/internal/leaderboard"
	"github.com/typemore/typemore-server/internal/perf"
)

// Zone 3: the read path, at a board an order of magnitude larger than anything
// this game will plausibly host. Every measurement below is client-side wall
// time — it includes the round trip to the container, which is what an HTTP
// handler actually pays for.

const zone3 = "zone 3 reads"

// pageLimit is the endpoint's default page size (docs/LEADERBOARDS.md).
const pageLimit = 50

// deepPageIndex is the page the "keyset does not degrade" claim is tested at.
// 1 000 pages of 50 is 50 000 rows deep — past the point where OFFSET would be
// scanning half the board on every request.
const deepPageIndex = 1_000

// TestLoadBoardPage measures the two page reads that must cost the same, and
// asserts that they do. A first page that is fast and a deep page that is not
// means the ordering is being produced rather than read, and no budget on page
// one would ever catch it.
func TestLoadBoardPage(t *testing.T) {
	f := loadFixture(t)
	ctx := context.Background()
	require.Greater(t, f.hotEntries, int64(deepPageIndex*pageLimit),
		"the hot bucket must be deeper than the page this test walks to")

	first := sample(t, 200, func() {
		rows, err := f.store.Page(ctx, f.hot, nil, pageLimit)
		require.NoError(t, err)
		require.Len(t, rows, pageLimit)
	})
	perf.Report(t, zone3, "hot first page, limit 50", perf.Summary(first))
	perf.Budget{
		Zone:     zone3,
		Workload: fmt.Sprintf("first page of a %d-entry board, limit %d", f.hotEntries, pageLimit),
		Limit:    10 * time.Millisecond,
		Rationale: "the public board page; one index range scan plus 50 PK lookups. " +
			"100 ms is the perceived-instant ceiling for the whole request, so the " +
			"database's share of it must stay an order of magnitude under that.",
	}.Assert(t, p99(first))

	// Walking there with real cursors rather than synthesising one proves the
	// continuation predicate is what a client would actually follow, and the
	// walk itself is a number worth having: it is what an export or a scraper
	// costs the board.
	walkStart := time.Now()
	var cursor *leaderboard.Cursor
	var walked int
	for page := range deepPageIndex {
		rows, err := f.store.Page(ctx, f.hot, cursor, pageLimit)
		require.NoError(t, err)
		require.NotEmpty(t, rows, "board ran out after %d pages", page)
		last := rows[len(rows)-1]
		cursor = &leaderboard.Cursor{Score: last.Score, AchievedAt: last.AchievedAt, UserID: last.UserID}
		walked += len(rows)
	}
	walk := time.Since(walkStart)
	perf.Report(t, zone3, "sequential keyset walk", fmt.Sprintf(
		"%d pages / %d rows in %s (%.0f pages/s)",
		deepPageIndex, walked, walk.Round(time.Millisecond),
		float64(deepPageIndex)/walk.Seconds()))

	deep := sample(t, 200, func() {
		rows, err := f.store.Page(ctx, f.hot, cursor, pageLimit)
		require.NoError(t, err)
		require.Len(t, rows, pageLimit)
	})
	perf.Report(t, zone3, fmt.Sprintf("hot page %d (row %d), limit 50", deepPageIndex+1, walked), perf.Summary(deep))
	perf.Budget{
		Zone:     zone3,
		Workload: fmt.Sprintf("keyset page %d of the same board", deepPageIndex+1),
		Limit:    10 * time.Millisecond,
		Rationale: "same absolute budget as page one, deliberately: keyset paging " +
			"exists so that depth costs nothing, and a separate, looser budget for " +
			"deep pages would be conceding the property the design is built on.",
	}.Assert(t, p99(deep))

	// The relative assertion is the one that survives a faster machine: it holds
	// whatever the absolute numbers turn out to be.
	firstMedian, deepMedian := perf.Percentile(first, 50), perf.Percentile(deep, 50)
	perf.Report(t, zone3, "deep/first p50 ratio", fmt.Sprintf("%.2fx", float64(deepMedian)/float64(firstMedian)))
	if deepMedian > 3*firstMedian {
		t.Errorf("BUDGET MISSED %s | keyset depth independence | page %d p50 %s is %.1f× page 1 p50 %s; "+
			"the continuation is not reading the index from the cursor",
			zone3, deepPageIndex+1, deepMedian.Round(time.Microsecond),
			float64(deepMedian)/float64(firstMedian), firstMedian.Round(time.Microsecond))
	}
}

// TestLoadBoardRank measures /me — Store.EntryFor, which is a PK lookup plus a
// COUNT of everyone above. The count is the part that scales with the board, so
// the curve is reported at three depths rather than at one.
//
// Method: all three points are taken in the SAME 100k-entry board, at players
// standing 1 000 / 10 000 / (board size − 1) rows down. The cost driver of
// CountLeaderboardAbove is the number of rows it has to count, so counting N
// rows here is the same index range scan as counting N rows in a board of size
// N — with the larger index, if anything, making this the pessimistic version.
// Re-seeding three boards would cost three million-row fixtures to measure the
// same range scan.
func TestLoadBoardRank(t *testing.T) {
	f := loadFixture(t)
	ctx := context.Background()

	visible := f.visibleHot(t)
	deepest := visible - 1
	depths := []int64{1_000, 10_000, deepest}

	for _, depth := range depths {
		pos := f.positionAt(t, depth)

		var got leaderboard.Entry
		entry := sample(t, 20, func() {
			var err error
			got, err = f.store.EntryFor(ctx, f.hot, pos.cursor.UserID)
			require.NoError(t, err)
		})
		require.Equal(t, depth+1, got.Rank, "the row at offset %d must rank %d", depth, depth+1)

		above := sample(t, 20, func() {
			n, err := f.store.RankAbove(ctx, f.hot, pos.cursor)
			require.NoError(t, err)
			require.Equal(t, depth, n)
		})

		perf.Report(t, zone3, fmt.Sprintf("EntryFor at rank %d", depth+1), perf.Summary(entry))
		perf.Report(t, zone3, fmt.Sprintf("RankAbove counting %d rows", depth), perf.Summary(above))

		// Only the worst point carries the phase brief's budget; the shallower
		// two are the curve that says whether it is linear.
		if depth == deepest {
			perf.Budget{
				Zone:     zone3,
				Workload: fmt.Sprintf("/me rank at the bottom of a %d-entry board", visible),
				Limit:    30 * time.Millisecond,
				Rationale: "the phase brief's number. /me is one authenticated request " +
					"per board view; 30 ms of database time is the most a personalised " +
					"row may add to a page that is otherwise already served.",
			}.Assert(t, p99(entry))
		}
	}
}

// TestLoadBoardCatalogue measures GET /api/v1/leaderboards: a GROUP BY over
// every entry in the table, through the ban view. Nothing indexes it and
// nothing bounds it — it is the one read whose cost grows with the number of
// boards AND with their size.
func TestLoadBoardCatalogue(t *testing.T) {
	f := loadFixture(t)
	ctx := context.Background()

	var buckets []leaderboard.BucketCount
	catalogue := sample(t, 10, func() {
		var err error
		buckets, err = f.store.Buckets(ctx)
		require.NoError(t, err)
	})
	require.NotEmpty(t, buckets)

	perf.Report(t, zone3, fmt.Sprintf("catalogue over %d buckets / %d entries", len(buckets), f.entries),
		perf.Summary(catalogue))
	perf.Budget{
		Zone:     zone3,
		Workload: fmt.Sprintf("bucket catalogue, %d buckets over %d entries", len(buckets), f.entries),
		Limit:    200 * time.Millisecond,
		Rationale: "the board index is the landing page of the whole feature and is " +
			"served uncached on every visit. 200 ms is the point at which a page " +
			"stops feeling like a lookup and starts feeling like a report.",
	}.Assert(t, p99(catalogue))
}

// TestLoadBoardBannedLeader re-measures the same reads with a banned player
// sitting in the top ranks.
//
// The ban filter is a NOT EXISTS the reader cannot bypass (docs/LEADERBOARDS.md,
// "Why bans are filtered on read"). The risk that buys is a planner that
// switches from a nested-loop anti-join to a hash anti-join once the ban table
// looks interesting — which would materialise the whole bucket and sort it. The
// assertion is that neither the latency nor the plan moves.
func TestLoadBoardBannedLeader(t *testing.T) {
	f := loadFixture(t)
	ctx := context.Background()

	clean := sample(t, 200, func() {
		_, err := f.store.Page(ctx, f.hot, nil, pageLimit)
		require.NoError(t, err)
	})

	leader := f.positionAt(t, 0)
	_, err := f.pool.Exec(ctx,
		`INSERT INTO bans (user_id, reason) VALUES ($1, 'zone 3 measurement')`, leader.cursor.UserID)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := f.pool.Exec(context.Background(), `DELETE FROM bans WHERE user_id = $1`, leader.cursor.UserID)
		require.NoError(t, err, "restore the shared fixture")
	})

	rows, err := f.store.Page(ctx, f.hot, nil, pageLimit)
	require.NoError(t, err)
	for _, r := range rows {
		require.NotEqual(t, leader.cursor.UserID, r.UserID, "a banned player must not appear on a page")
	}

	banned := sample(t, 200, func() {
		_, err := f.store.Page(ctx, f.hot, nil, pageLimit)
		require.NoError(t, err)
	})
	perf.Report(t, zone3, "first page, rank-1 player banned", perf.Summary(banned))

	cleanMedian, bannedMedian := perf.Percentile(clean, 50), perf.Percentile(banned, 50)
	perf.Report(t, zone3, "banned/clean p50 ratio", fmt.Sprintf("%.2fx", float64(bannedMedian)/float64(cleanMedian)))
	perf.Budget{
		Zone:     zone3,
		Workload: "first page with a banned player in the top ranks",
		Limit:    10 * time.Millisecond,
		Rationale: "the ban filter is on the read path of every board query; it must " +
			"cost what the unfiltered page costs, or the filter becomes something " +
			"someone eventually argues for moving to write time.",
	}.Assert(t, p99(banned))

	plan, err := perf.Explain(ctx, f.pool, sqlPageFirst, f.hot.Key(), int32(pageLimit))
	require.NoError(t, err)
	perf.AssertPlan(t, plan, perf.PlanAssertion{
		Zone:        zone3,
		Query:       "ListLeaderboardPageFirst, with a banned leader",
		WantAny:     []string{"Index Scan", "Index Only Scan", "Bitmap Index Scan"},
		NoSeqScanOn: []string{"leaderboard_entries", "runs"},
		NoSort:      true,
	})
}

// --- plan assertions ---
//
// A latency budget is only true on the machine and the volume it was measured
// at; "this query must not sort, and must not sequentially scan the entries
// table" is true everywhere and goes red the moment a migration drops the index
// (internal/perf/explain.go).
//
// The SQL below is copied VERBATIM from the sqlc output in
// leaderboarddb/queries.sql.go — the constants there are unexported, and
// paraphrasing the query would assert the plan of something the server never
// runs.

const sqlPageFirst = `-- name: ListLeaderboardPageFirst :many
SELECT user_id, display_name, run_id, score, wpm, raw, acc, grade, mods,
       achieved_at, quote_source
FROM leaderboard_rows
WHERE bucket_key = $1
ORDER BY sort_key DESC, achieved_at ASC, user_id ASC
LIMIT $2`

const sqlPageAfter = `-- name: ListLeaderboardPageAfter :many
SELECT user_id, display_name, run_id, score, wpm, raw, acc, grade, mods,
       achieved_at, quote_source
FROM leaderboard_rows
WHERE bucket_key = $1
  AND sort_key <= leaderboard_sort_key($2, $3)
  AND (sort_key < leaderboard_sort_key($2, $3)
       OR achieved_at > $3::timestamptz
       OR (achieved_at = $3::timestamptz AND user_id > $4::uuid))
ORDER BY sort_key DESC, achieved_at ASC, user_id ASC
LIMIT $5`

const sqlPageBefore = `-- name: ListLeaderboardPageBefore :many
SELECT user_id, display_name, run_id, score, wpm, raw, acc, grade, mods,
       achieved_at, quote_source
FROM leaderboard_rows
WHERE bucket_key = $1
  AND sort_key >= leaderboard_sort_key($2, $3)
  AND (sort_key > leaderboard_sort_key($2, $3)
       OR achieved_at < $3::timestamptz
       OR (achieved_at = $3::timestamptz AND user_id < $4::uuid))
ORDER BY sort_key ASC, achieved_at DESC, user_id DESC
LIMIT $5`

const sqlRankAbove = `-- name: CountLeaderboardAbove :one
SELECT count(*)::bigint
FROM leaderboard_ranked
WHERE bucket_key = $1
  AND sort_key >= leaderboard_sort_key($2, $3)
  AND (sort_key > leaderboard_sort_key($2, $3)
       OR achieved_at < $3::timestamptz
       OR (achieved_at = $3::timestamptz AND user_id < $4::uuid))`

const sqlEntryFor = `-- name: GetLeaderboardEntry :one
SELECT user_id, display_name, run_id, score, wpm, raw, acc, grade, mods,
       achieved_at, quote_source
FROM leaderboard_rows
WHERE bucket_key = $1 AND user_id = $2`

const sqlCatalogue = `-- name: ListLeaderboardBuckets :many
SELECT bucket_key, count(*)::bigint AS entries
FROM leaderboard_ranked
GROUP BY bucket_key
ORDER BY bucket_key`

func TestLoadPlanBoardPageFirst(t *testing.T) {
	f := loadFixture(t)

	plan, err := perf.Explain(context.Background(), f.pool, sqlPageFirst, f.hot.Key(), int32(pageLimit))
	require.NoError(t, err)
	perf.AssertPlan(t, plan, perf.PlanAssertion{
		Zone:        zone3,
		Query:       "ListLeaderboardPageFirst",
		WantAny:     []string{"Index Scan", "Index Only Scan", "Bitmap Index Scan"},
		NoSeqScanOn: []string{"leaderboard_entries", "runs"},
		NoSort:      true,
	})
	perf.Report(t, zone3, "page-one plan (raw)", "\n"+plan.Raw)
}

func TestLoadPlanBoardPageAfter(t *testing.T) {
	f := loadFixture(t)
	pos := f.positionAt(t, min(deepPageIndex*pageLimit, f.visibleHot(t)-1))

	plan, err := perf.Explain(context.Background(), f.pool, sqlPageAfter,
		f.hot.Key(), pos.cursor.Score, pos.cursor.AchievedAt, pos.cursor.UserID, int32(pageLimit))
	require.NoError(t, err)
	perf.AssertPlan(t, plan, perf.PlanAssertion{
		Zone:        zone3,
		Query:       fmt.Sprintf("ListLeaderboardPageAfter (row %d)", pos.offset),
		WantAny:     []string{"Index Scan", "Index Only Scan", "Bitmap Index Scan"},
		NoSeqScanOn: []string{"leaderboard_entries", "runs"},
		NoSort:      true,
	})
	// The raw plan is the evidence for whether the cursor became an index
	// SEEK or a filter the scan has to walk up to: the node list is identical
	// either way, and only the row counts tell them apart.
	perf.Report(t, zone3, "deep-page plan (raw)", "\n"+plan.Raw)
}

// TestLoadPlanBoardPageBefore pins the upward continuation to the same shape
// as the downward one: a start-condition seek on leaderboard_sort_idx (run
// BACKWARD — a btree walks either way, which is the whole reason around=me
// needed no second index), never a sort, never a scan of the entries table.
// Asserted at depth, where a filter-shaped regression would have the most to
// walk past.
func TestLoadPlanBoardPageBefore(t *testing.T) {
	f := loadFixture(t)
	pos := f.positionAt(t, min(deepPageIndex*pageLimit, f.visibleHot(t)-1))

	plan, err := perf.Explain(context.Background(), f.pool, sqlPageBefore,
		f.hot.Key(), pos.cursor.Score, pos.cursor.AchievedAt, pos.cursor.UserID, int32(pageLimit))
	require.NoError(t, err)
	perf.AssertPlan(t, plan, perf.PlanAssertion{
		Zone:        zone3,
		Query:       fmt.Sprintf("ListLeaderboardPageBefore (row %d)", pos.offset),
		WantAny:     []string{"Index Scan", "Index Only Scan", "Bitmap Index Scan"},
		NoSeqScanOn: []string{"leaderboard_entries", "runs"},
		NoSort:      true,
	})
	perf.Report(t, zone3, "page-before plan (raw)", "\n"+plan.Raw)
}

// TestLoadPlanBoardRankAbove checks the count at both ends of the curve. A
// shallow count is a short range scan whatever the planner does; the deep one
// is where it may decide the range is most of the bucket and switch to reading
// the table.
func TestLoadPlanBoardRankAbove(t *testing.T) {
	f := loadFixture(t)
	ctx := context.Background()

	for _, depth := range []int64{1_000, f.visibleHot(t) - 1} {
		pos := f.positionAt(t, depth)
		plan, err := perf.Explain(ctx, f.pool, sqlRankAbove,
			f.hot.Key(), pos.cursor.Score, pos.cursor.AchievedAt, pos.cursor.UserID)
		require.NoError(t, err)
		perf.AssertPlan(t, plan, perf.PlanAssertion{
			Zone:        zone3,
			Query:       fmt.Sprintf("CountLeaderboardAbove (%d rows above)", depth),
			WantAny:     []string{"Index Scan", "Index Only Scan", "Bitmap Index Scan"},
			NoSeqScanOn: []string{"leaderboard_entries", "runs"},
		})
	}
}

func TestLoadPlanBoardEntryFor(t *testing.T) {
	f := loadFixture(t)
	pos := f.positionAt(t, 10_000)

	plan, err := perf.Explain(context.Background(), f.pool, sqlEntryFor, f.hot.Key(), pos.cursor.UserID)
	require.NoError(t, err)
	perf.AssertPlan(t, plan, perf.PlanAssertion{
		Zone:        zone3,
		Query:       "GetLeaderboardEntry",
		WantAny:     []string{"Index Scan", "Index Only Scan", "Bitmap Index Scan"},
		NoSeqScanOn: []string{"leaderboard_entries", "runs"},
		NoSort:      true,
	})
}

// TestLoadPlanBoardCatalogue records the catalogue's plan without pretending it
// can be index-driven: `GROUP BY bucket_key` over the whole table has no
// predicate to satisfy, so a sequential scan is the CORRECT plan and asserting
// against it would be asserting a lie. What is worth pinning is that it does
// not spill to disk — the aggregate is bounded by the number of buckets, not by
// the number of entries, and a spill would mean that stopped being true.
func TestLoadPlanBoardCatalogue(t *testing.T) {
	f := loadFixture(t)

	plan, err := perf.Explain(context.Background(), f.pool, sqlCatalogue)
	require.NoError(t, err)
	perf.AssertPlan(t, plan, perf.PlanAssertion{
		Zone:  zone3,
		Query: "ListLeaderboardBuckets",
	})
	perf.Report(t, zone3, "catalogue plan", fmt.Sprintf("%v in %.1f ms", plan.Nodes, plan.TotalMs))
}
