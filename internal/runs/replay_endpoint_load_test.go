//go:build load

package runs_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/typemore/typemore-server/internal/perf"
)

// Zone 6 — GET /api/v1/runs/{id}/replay, the public, unauthenticated endpoint
// that serves the heaviest payload this server produces.
//
// As in zone 5 the client shares the process with the server, so heap figures
// are upper bounds on the server's own use; the bodies are streamed to
// io.Discard so the client side contributes read buffers, not copies.

const zone6 = "6 replay"

// The latency ceiling. This is a spectator clicking a leaderboard row: the
// fetch blocks the replay player from starting, and unlike ingestion there is
// no local preview to hide behind. 250 ms at p50 is the point where a click
// still feels connected to its result; 600 ms at p99 is the edge of "the page
// is loading" for a payload of this size.
var (
	replayP50Budget = perf.Budget{
		Zone: zone6, Workload: "GET /runs/{id}/replay, max run, p50 at 20 concurrent",
		Limit:     250 * time.Millisecond,
		Rationale: "a spectator's click blocks on this; there is no local preview to mask it",
	}
	replayP99Budget = perf.Budget{
		Zone: zone6, Workload: "GET /runs/{id}/replay, max run, p99 at 20 concurrent",
		Limit:     600 * time.Millisecond,
		Rationale: "the tail may look like a page load, never like a hang",
	}
)

const replayConcurrency = 20

// storeMaxAcceptedRun ingests the largest submittable run and puts it in the one
// state the public endpoint serves.
//
// The verdict is written in SQL rather than produced by the real worker: the
// fixture is 39 913 synthetic single-character inserts against a 10 000-word
// setup, which the core would (correctly) reject as implausible, and zone 6 is
// measuring the SERVING path, not the judging one. The write still goes through
// one transaction with ProjectRun attached, which is the contract every writer
// of runs.status has to honour.
func storeMaxAcceptedRun(t *testing.T, h *harness) (id string, rawLogBytes int) {
	t.Helper()

	payload := perf.MaxLegalPayload(loadSeed)
	body := perf.MustJSON(payload)
	require.LessOrEqual(t, len(body), perf.MaxBodyBytes)

	status, err := h.sendBody("/api/v1/runs", body)
	require.NoError(t, err)
	require.Equal(t, http.StatusAccepted, status)

	var row struct {
		ID string
	}
	ctx := context.Background()
	require.NoError(t, h.pool.QueryRow(ctx,
		`SELECT id::text FROM runs ORDER BY created_at DESC LIMIT 1`).Scan(&row.ID))

	tx, err := h.pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()

	_, err = tx.Exec(ctx, `
		UPDATE runs
		SET status = 'accepted',
		    server_metrics = '{"wpm":101.2,"raw":105.4,"accuracy":0.987,"chars":{"correct":9990}}'::jsonb,
		    server_score   = '{"version":2,"total":1234,"base":1000,"comboPeak":42}'::jsonb,
		    validation     = '{"verdict":"valid","reason":"","flags":[]}'::jsonb,
		    validated_at   = now()
		WHERE id = $1`, row.ID)
	require.NoError(t, err)
	require.NoError(t, h.board.ProjectRun(ctx, tx, uuid.MustParse(row.ID)))
	require.NoError(t, tx.Commit(ctx))

	return row.ID, len(payload.Log)
}

// TestLoadReplayMaxRunConcurrent is the memory and latency question: twenty
// spectators pulling the same maximal run at the same instant.
func TestLoadReplayMaxRunConcurrent(t *testing.T) {
	h := newHarness(t, func(o *harnessOpts) {
		o.runsRateBurst = 10_000
		o.replayRateBurst = 10_000
	})
	h.login("replay-max@example.com", "correct horse battery", "replaymax")
	id, rawLogBytes := storeMaxAcceptedRun(t, h)
	path := "/api/v1/runs/" + id + "/replay"

	status, served, err := h.fetchDiscard(path)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)
	perf.Report(t, zone6, "served replay body",
		fmt.Sprintf("%s (%d bytes) wrapping a %s event log",
			perf.MiB(uint64(served)), served, perf.MiB(uint64(rawLogBytes))))

	h.warmPool()
	res := h.burst(replayConcurrency, h.wire(http.MethodGet, path, nil))
	require.NoError(t, res.err)
	require.Equal(t, map[int]int{http.StatusOK: replayConcurrency}, res.statuses)
	require.Positive(t, res.samples)
	for i, n := range res.served {
		require.Equal(t, served, n, "short body on fetch %d", i)
	}

	perf.Report(t, zone6, fmt.Sprintf("%d max-run fetches, achieved concurrency", replayConcurrency),
		fmt.Sprintf("%d in flight at peak; delivered in %v, window %v",
			res.overlap, res.delivered.Round(time.Millisecond), res.elapsed.Round(time.Millisecond)))
	require.GreaterOrEqual(t, res.overlap, replayConcurrency,
		"the driver failed to deliver the burst; the memory figure would be for a smaller one")

	perf.Report(t, zone6, fmt.Sprintf("%d concurrent max-run fetches", replayConcurrency),
		perf.Summary(res.latency))
	replayP50Budget.Assert(t, perf.Percentile(res.latency, 50))
	replayP99Budget.Assert(t, perf.Percentile(res.latency, 99))

	perf.Report(t, zone6, "baseline heap before the burst", perf.MiB(res.baseline))
	perf.Report(t, zone6, "peak process memory (Sys) during the burst", perf.MiB(res.peakSys))
	perf.Report(t, zone6, "heap growth attributable to the burst",
		fmt.Sprintf("%s = %.1f× the %s of payload served",
			perf.MiB(growth(res.peakHeap, res.baseline)),
			float64(growth(res.peakHeap, res.baseline))/float64(replayConcurrency*int(served)),
			perf.MiB(uint64(replayConcurrency*int(served)))))

	// The handler gunzips the whole log into a []byte and then hands it to
	// json.NewEncoder, which compacts it into a second buffer before a byte
	// reaches the socket — so a concurrent fetch is expected to cost multiples of
	// the payload, not one payload. 192 MiB for 20 in flight is that expectation
	// (~2 MiB served, ~3 live copies, plus the gzip blob and encoder growth) with
	// margin. The ceiling matters more here than in zone 5 because this route
	// needs no session: whatever it costs, an anonymous client can ask for.
	perf.AssertBytes(t, zone6,
		fmt.Sprintf("peak heap, %d concurrent max-run replays", replayConcurrency),
		res.peakHeap, 192<<20,
		"an unauthenticated route must not be able to price itself out of a 512 MiB instance")
}

// TestLoadReplayAllocationMultiplier answers "does it stream or does it buffer"
// with a number: how many bytes the process allocates to serve one replay,
// against the size of the thing it served.
//
// The response is drained to io.Discard, so a streaming implementation would
// land near 1× plus small fixed buffers. Anything at 3× or above means the
// payload exists several times over at once, per concurrent request.
func TestLoadReplayAllocationMultiplier(t *testing.T) {
	h := newHarness(t, func(o *harnessOpts) {
		o.runsRateBurst = 10_000
		o.replayRateBurst = 10_000
	})
	h.login("replay-alloc@example.com", "correct horse battery", "replayalloc")
	id, rawLogBytes := storeMaxAcceptedRun(t, h)
	path := "/api/v1/runs/" + id + "/replay"

	status, served, err := h.fetchDiscard(path)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)

	var stored int
	require.NoError(t, h.pool.QueryRow(context.Background(),
		`SELECT octet_length(log) FROM runs WHERE id = $1`, id).Scan(&stored))

	allocBytes, allocs := perf.Delta(func() {
		code, n, err := h.fetchDiscard(path)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, code)
		require.Equal(t, served, n)
	})

	perf.Report(t, zone6, "stored gzip blob", perf.MiB(uint64(stored)))
	perf.Report(t, zone6, "allocations to serve one max replay",
		fmt.Sprintf("%s in %d allocs = %.2f× the %s response (%.2f× the %s raw log)",
			perf.MiB(allocBytes), allocs,
			float64(allocBytes)/float64(served), perf.MiB(uint64(served)),
			float64(allocBytes)/float64(rawLogBytes), perf.MiB(uint64(rawLogBytes))))

	// A single peak-sampled fetch says how much of that is live AT ONCE rather
	// than merely allocated over the request's life — the number the concurrency
	// envelope is built from.
	baseline := liveHeap()
	sampler := perf.NewPeakSampler(time.Millisecond)
	code, _, err := h.fetchDiscard(path)
	peakHeap, _ := sampler.Stop()
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, code)
	perf.Report(t, zone6, "live heap growth during ONE max replay",
		fmt.Sprintf("%s = %.2f× the %s response",
			perf.MiB(growth(peakHeap, baseline)),
			float64(growth(peakHeap, baseline))/float64(served),
			perf.MiB(uint64(served))))
}

// TestLoadReplayRateLimitShedsBeforeMemory runs the endpoint with the
// PRODUCTION per-IP limiter and asks the question that matters for a route with
// no session: is the burst the limiter permits itself affordable?
//
// The limiter is the only thing standing between one anonymous IP and this
// server's memory, so the honest experiment is to spend the whole burst AT ONCE
// and weigh it — not to reason about a per-request cost and multiply.
func TestLoadReplayRateLimitShedsBeforeMemory(t *testing.T) {
	// The cmd/server defaults: LEADERBOARD_REPLAY_RATE_EVERY=2s, _BURST=30.
	const prodBurst = 30
	const prodEvery = 2 * time.Second

	h := newHarness(t, func(o *harnessOpts) {
		o.runsRateBurst = 10_000
		o.replayRateEvery = prodEvery
		o.replayRateBurst = prodBurst
	})
	h.login("replay-flood@example.com", "correct horse battery", "replayflood")
	id, _ := storeMaxAcceptedRun(t, h)
	path := "/api/v1/runs/" + id + "/replay"

	// No warm fetch: one would spend a token, and the burst below has to be the
	// whole bucket. The pool is warmed instead, which costs nothing on this route.
	h.warmPool()
	res := h.burst(prodBurst, h.wire(http.MethodGet, path, nil))
	require.NoError(t, res.err)
	require.Equal(t, map[int]int{http.StatusOK: prodBurst}, res.statuses,
		"every request inside the permitted burst must be served")
	require.Positive(t, res.samples)
	require.GreaterOrEqual(t, res.overlap, prodBurst,
		"the driver failed to deliver the burst; the memory figure would be for a smaller one")

	var served int64
	for _, n := range res.served {
		served += n
	}

	// The bucket is now empty and refills one token per 2 s, so everything that
	// follows immediately must be shed. That is the property the memory argument
	// rests on: the burst above is the MOST one IP can hold at once.
	var shed int
	for range 10 {
		code, n, err := h.fetchDiscard(path)
		require.NoError(t, err)
		if code == http.StatusTooManyRequests {
			shed++
			continue
		}
		assert.Equal(t, http.StatusTooManyRequests, code,
			"a spent bucket must shed, not serve another %s", perf.MiB(uint64(n)))
	}
	assert.Equal(t, 10, shed, "the limiter must shed every request past the burst")

	perf.Report(t, zone6, "one-IP burst under production limits",
		fmt.Sprintf("%d concurrent served (%s total, %d in flight at peak) then %d × 429; refill %v/token",
			prodBurst, perf.MiB(uint64(served)), res.overlap, shed, prodEvery))
	perf.Report(t, zone6, fmt.Sprintf("%d concurrent max-run fetches, latency", prodBurst),
		perf.Summary(res.latency))
	perf.Report(t, zone6, "peak process memory (Sys) during the burst", perf.MiB(res.peakSys))
	perf.Report(t, zone6, "heap growth for the full permitted burst",
		fmt.Sprintf("%s = %.1f× the %s actually served",
			perf.MiB(growth(res.peakHeap, res.baseline)),
			float64(growth(res.peakHeap, res.baseline))/float64(served),
			perf.MiB(uint64(served))))

	// The ceiling a per-IP limiter has to respect. This route needs no account,
	// and the limit is per IP, so whatever ONE client can command, N clients can
	// command N times over. A quarter of a 512 MiB instance is already a generous
	// share for a single anonymous spectator; past that the burst is not a rate
	// limit but a memory allocation with extra steps.
	perf.AssertBytes(t, zone6,
		fmt.Sprintf("peak heap, one IP spending its full burst of %d", prodBurst),
		res.peakHeap, 128<<20,
		"one unauthenticated IP must not be able to claim a quarter of a 512 MiB instance")
}

// getPublicReplaySQL is the query behind the endpoint, copied verbatim from
// internal/runs/runsdb/queries.sql.go. Copied rather than imported because the
// generated constant is unexported; if the two drift, the assertion below stops
// describing the endpoint and this comment is the reason to re-copy it.
const getPublicReplaySQL = `SELECT r.setup, r.log, r.server_metrics, r.server_score,
       run_grade((r.server_metrics ->> 'accuracy')::numeric)::text AS grade,
       r.mode, r.duration_ms, r.word_count, r.lang, r.created_at,
       u.display_name
FROM runs r
         JOIN users u ON u.id = r.user_id
WHERE r.id = $1
  AND r.status = 'accepted'
  AND jsonb_typeof(r.server_metrics -> 'accuracy') = 'number'
  AND NOT EXISTS (SELECT 1 FROM active_bans b WHERE b.user_id = r.user_id)`

// TestLoadPlanPublicReplay pins the SHAPE of the endpoint's only query.
//
// A latency budget on a table with one run in it proves nothing — every plan is
// fast at n=1. What has to hold at every volume is that the lookup is anchored
// on the primary key and the join on users is an index lookup, so that a
// spectator's click costs the same when the table holds a million runs. A seq
// scan of `bans` is deliberately allowed: it is a handful of rows forever.
func TestLoadPlanPublicReplay(t *testing.T) {
	h := newHarness(t, func(o *harnessOpts) {
		o.runsRateBurst = 10_000
		o.replayRateBurst = 10_000
	})
	h.login("replay-plan@example.com", "correct horse battery", "replayplan")
	id, _ := storeMaxAcceptedRun(t, h)

	plan, err := perf.Explain(context.Background(), h.pool, getPublicReplaySQL, uuid.MustParse(id))
	require.NoError(t, err)
	perf.AssertPlan(t, plan, perf.PlanAssertion{
		Zone:        zone6,
		Query:       "GetPublicReplay",
		WantAny:     []string{"Index Scan", "Index Only Scan", "Bitmap Index Scan"},
		NoSeqScanOn: []string{"runs", "users"},
		NoSort:      true,
	})
}
