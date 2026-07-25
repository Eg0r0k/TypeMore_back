//go:build load

package ws_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/typemore/typemore-server/internal/perf"
	"github.com/typemore/typemore-server/internal/platform/db"
	"github.com/typemore/typemore-server/internal/platform/migrate"
	"github.com/typemore/typemore-server/internal/protocol"
	"github.com/typemore/typemore-server/internal/ws"
	"github.com/typemore/typemore-server/internal/ws/wspg"
)

// Zone 8 — the match-end persistence burst.
//
// endMatchLocked snapshots under the room lock and then spawns `go persist`,
// which gzips every participant's capture and writes the whole match in ONE
// SaveMatch transaction. Twenty rooms ending at the same instant therefore fire
// twenty concurrent gzip-then-transaction pipelines at a ten-connection pool.
// The question worth asking is not how fast that is on its own — it is what it
// does to everything else touching the same database.

// Burst shape. Five seats is the room capacity, and 600 batches is a 60 s match
// at the protocol's 100 ms flush — the same stream zone 7 measures, captured
// instead of merely relayed.
const (
	burstRooms   = 20
	burstBatches = 600
	// burstFillPeriod fills 600 batches per seat in six seconds. It is ten times
	// the protocol's flush rate: the fill is setup, not the measurement.
	burstFillPeriod = 10 * time.Millisecond
	// burstPoolConns mirrors the production default (platform.Config.DBMaxConns
	// = 10). Sizing the pool to the burst would measure a deployment nobody
	// runs.
	burstPoolConns = 10
	// The burst itself is short, so the probe has to be dense enough that a p99
	// "during the burst" is a distribution and not a rounded maximum: three
	// concurrent probers on a 2 ms tick. A prober whose query outlasts the tick
	// simply runs closed-loop, which is exactly when samples matter most.
	// Three is also a realistic amount of unrelated traffic to have in flight.
	probePeriod   = 2 * time.Millisecond
	probeWorkers  = 3
	baselineHold  = 6 * time.Second
	afterburnHold = 3 * time.Second
)

// The Postgres testcontainer is started lazily on first use and torn down in
// TestMain, mirroring internal/runs and internal/ws/wspg.
var (
	burstDBOnce      sync.Once
	burstDBContainer *postgres.PostgresContainer
	burstDSN         string
	burstDBErr       error
)

func ensureBurstDB(t *testing.T) string {
	t.Helper()
	burstDBOnce.Do(func() {
		ctx := context.Background()
		burstDBContainer, burstDBErr = postgres.Run(ctx, "postgres:17",
			postgres.WithDatabase("typemore"),
			postgres.WithUsername("typemore"),
			postgres.WithPassword("typemore"),
			testcontainers.WithWaitStrategy(
				wait.ForLog("database system is ready to accept connections").
					WithOccurrence(2).
					WithStartupTimeout(90*time.Second),
			),
		)
		if burstDBErr != nil {
			return
		}
		burstDSN, burstDBErr = burstDBContainer.ConnectionString(ctx, "sslmode=disable")
		if burstDBErr != nil {
			return
		}
		burstDBErr = migrate.Up(ctx, burstDSN)
	})
	require.NoError(t, burstDBErr, "start/migrate postgres testcontainer")
	return burstDSN
}

func TestMain(m *testing.M) {
	code := m.Run()
	if burstDBContainer != nil {
		_ = burstDBContainer.Terminate(context.Background())
	}
	os.Exit(code)
}

// saveCall is one observed MatchStore.SaveMatch.
type saveCall struct {
	matchID  string
	start    time.Time
	dur      time.Duration
	logBytes int
	runs     int
	logs     [][]byte // the gzip'd captures, kept so the gzip cost can be re-priced
	err      error
}

// timedStore wraps the real Postgres store and times every write. It is the
// only way to separate DB time from gzip time without instrumenting
// Room.persist, which is another engineer's file.
type timedStore struct {
	inner ws.MatchStore
	mu    sync.Mutex
	calls []saveCall
}

func (ts *timedStore) SaveMatch(ctx context.Context, m ws.MatchRecord) error {
	call := saveCall{matchID: m.ID, start: time.Now(), runs: len(m.Runs)}
	for _, r := range m.Runs {
		call.logBytes += len(r.Log)
		call.logs = append(call.logs, r.Log)
	}
	call.err = ts.inner.SaveMatch(ctx, m)
	call.dur = time.Since(call.start)
	ts.mu.Lock()
	ts.calls = append(ts.calls, call)
	ts.mu.Unlock()
	return call.err
}

func (ts *timedStore) snapshot() []saveCall {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return append([]saveCall(nil), ts.calls...)
}

// probeSample is one unrelated request's latency, stamped so it can be placed
// before, during, or after the burst.
type probeSample struct {
	at  time.Time
	dur time.Duration
}

// dbProbe hammers a trivial read of a table the burst never touches. It is the
// "everything else on this instance" signal: pool acquisition, backend
// scheduling, and disk contention all show up here, table locks do not.
// probeCollector is the mutex-guarded result of the background probers: their
// samples, and the first error any of them hit.
type probeCollector struct {
	mu      sync.Mutex
	samples []probeSample
	err     error
}

// snapshot copies the samples taken so far and reports the first probe error.
func (c *probeCollector) snapshot() ([]probeSample, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.samples), c.err
}

func dbProbe(ctx context.Context, pool *pgxpool.Pool, c *probeCollector) {
	tick := time.NewTicker(probePeriod)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		start := time.Now()
		var n int64
		err := pool.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&n)
		d := time.Since(start)
		if err != nil {
			if ctx.Err() == nil {
				c.mu.Lock()
				c.err = err
				c.mu.Unlock()
			}
			return
		}
		c.mu.Lock()
		c.samples = append(c.samples, probeSample{at: start, dur: d})
		c.mu.Unlock()
	}
}

func probeWindow(samples []probeSample, from, to time.Time) []time.Duration {
	var out []time.Duration
	for _, s := range samples {
		if !s.at.Before(from) && s.at.Before(to) {
			out = append(out, s.dur)
		}
	}
	return out
}

// TestLoadMatchEndBurst ends twenty full 5-player matches at the same instant
// and measures what that costs the rooms, the heap, and everything else using
// the database.
func TestLoadMatchEndBurst(t *testing.T) {
	loadSettle(t)
	ctx := context.Background()
	pool, err := db.NewPool(ctx, ensureBurstDB(t), burstPoolConns)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	_, err = pool.Exec(ctx, `TRUNCATE matches, match_runs RESTART IDENTITY CASCADE`)
	require.NoError(t, err)

	// pgx opens connections on demand, so a single warm-up query opens exactly
	// one. Hold the whole pool at once, then release: otherwise the burst pays
	// nine TCP handshakes and a startup packet each, inside the window.
	warm := make([]*pgxpool.Conn, 0, burstPoolConns)
	for range burstPoolConns {
		c, aerr := pool.Acquire(ctx)
		require.NoError(t, aerr)
		warm = append(warm, c)
	}
	for _, c := range warm {
		c.Release()
	}

	store := &timedStore{inner: wspg.New(pool)}
	srv := loadServer(t, store)
	rooms := buildLoadRooms(t, srv, burstRooms, loadSeats, burstBatches+16)
	defer closeLoadRooms(rooms)

	// --- fill every capture -------------------------------------------------
	runCtx, runCancel := context.WithCancel(context.Background())
	var readers, fillers sync.WaitGroup
	for _, lr := range rooms {
		for _, lc := range lr.conns {
			readers.Add(1)
			go func() { defer readers.Done(); lc.readFrames(runCtx) }()
		}
	}
	fillStart := time.Now()
	stop := make(chan struct{})
	for _, lr := range rooms {
		for _, lc := range lr.conns {
			fillers.Add(1)
			go func() {
				defer fillers.Done()
				lc.writeBatches(lr.matchID, burstFillPeriod, stop)
			}()
		}
	}
	// Wait on the counters, not the clock: a 10 ms ticker on Windows is not
	// exactly 10 ms, and the captures have to be full before the burst.
	fillDeadline := time.Now().Add(90 * time.Second)
	for {
		full := true
		for _, lr := range rooms {
			for _, lc := range lr.conns {
				if lc.written.Load() < int64(burstBatches) {
					full = false
				}
			}
		}
		if full {
			break
		}
		require.True(t, time.Now().Before(fillDeadline), "captures never filled")
		time.Sleep(50 * time.Millisecond)
	}
	close(stop)
	fillers.Wait()
	for _, lr := range rooms {
		for _, lc := range lr.conns {
			require.NoErrorf(t, lc.writeErr, "room %s seat %d could not fill its capture", lr.code, lc.seat)
			require.GreaterOrEqualf(t, lc.written.Load(), int64(burstBatches),
				"room %s seat %d only captured %d batches", lr.code, lc.seat, lc.written.Load())
		}
	}
	perf.Report(t, "8", "capture fill",
		fmt.Sprintf("%d rooms x %d seats x %d batches (%d event_batches) in %s",
			burstRooms, loadSeats, burstBatches, burstRooms*loadSeats*burstBatches,
			time.Since(fillStart).Round(time.Millisecond)))

	// --- baseline: what an unrelated request costs with nothing happening ----
	var probeC probeCollector
	probeCtx, probeCancel := context.WithCancel(context.Background())
	for range probeWorkers {
		go dbProbe(probeCtx, pool, &probeC)
	}
	baselineFrom := time.Now()
	time.Sleep(baselineHold)
	baselineTo := time.Now()

	// --- arm the burst ------------------------------------------------------
	// Every seat but the last finishes first, so the last one's `finish` is the
	// single frame that ends the match. Twenty of those are released together.
	for _, lr := range rooms {
		for _, lc := range lr.conns[:loadSeats-1] {
			require.NoError(t, loadWriteJSON(lc.c, protocol.Finish{Type: protocol.TypeFinish, MatchID: lr.matchID}))
		}
	}
	armed := time.Now()
	for _, lr := range rooms {
		last := lr.conns[loadSeats-1]
		for last.peerFinished.Load() < int64(loadSeats-1) {
			require.Lessf(t, time.Since(armed), 30*time.Second,
				"room %s: only %d of %d peers finished before the barrier", lr.code, last.peerFinished.Load(), loadSeats-1)
			time.Sleep(5 * time.Millisecond)
		}
	}

	// The room-liveness probe: a chat sent the instant the burst starts. Its
	// round trip goes through the SAME room lock endMatchLocked holds, so if
	// persist ran on-lock this would cost the whole gzip-plus-transaction time.
	chatter := rooms[0].conns[0]
	chatter.chatArmed.Store(true)

	sampler := perf.NewPeakSampler(5 * time.Millisecond)
	release := make(chan struct{})
	var enders sync.WaitGroup
	for _, lr := range rooms {
		enders.Add(1)
		go func() {
			defer enders.Done()
			<-release
			_ = loadWriteJSON(lr.conns[loadSeats-1].c, protocol.Finish{Type: protocol.TypeFinish, MatchID: lr.matchID})
		}()
	}
	releasedAt := time.Now()
	close(release)
	chatSentAt := loadNanos()
	require.NoError(t, loadWriteJSON(chatter.c, protocol.ChatSend{Type: protocol.TypeChatSend, Text: "burst probe"}))
	enders.Wait()

	// --- wait out the burst -------------------------------------------------
	var calls []saveCall
	for {
		calls = store.snapshot()
		if len(calls) >= burstRooms {
			break
		}
		require.Lessf(t, time.Since(releasedAt), 3*time.Minute,
			"only %d of %d matches persisted", len(calls), burstRooms)
		time.Sleep(2 * time.Millisecond)
	}
	burstEnd := time.Now()
	burstWall := burstEnd.Sub(releasedAt)
	peakHeap, peakSys := sampler.Stop()

	time.Sleep(afterburnHold)
	probeCancel()
	runCancel()
	readers.Wait()
	samples, pErr := probeC.snapshot()
	require.NoError(t, pErr, "the unrelated-request probe failed")

	// --- what the store saw -------------------------------------------------
	var dbTotal, dbMax time.Duration
	var bytesTotal int
	dbDurs := make([]time.Duration, 0, len(calls))
	lastFinish := releasedAt
	for _, c := range calls {
		require.NoErrorf(t, c.err, "match %s failed to persist", c.matchID)
		require.Equal(t, loadSeats, c.runs)
		dbTotal += c.dur
		dbDurs = append(dbDurs, c.dur)
		if c.dur > dbMax {
			dbMax = c.dur
		}
		bytesTotal += c.logBytes
		if end := c.start.Add(c.dur); end.After(lastFinish) {
			lastFinish = end
		}
	}
	// Every SaveMatch call arrives after its match's gzip work, so the earliest
	// start dates the fastest gzip and the spread dates the queue behind the
	// ten-connection pool.
	firstStart, lastStart := calls[0].start, calls[0].start
	for _, c := range calls {
		if c.start.Before(firstStart) {
			firstStart = c.start
		}
		if c.start.After(lastStart) {
			lastStart = c.start
		}
	}

	perf.Report(t, "8", "burst shape",
		fmt.Sprintf("%d matches x %d runs x %d batches ended simultaneously; pool=%d conns; NumCPU=%d",
			burstRooms, loadSeats, burstBatches, burstPoolConns, runtime.NumCPU()))
	perf.Report(t, "8", "compressed capture size",
		fmt.Sprintf("%s per match (%s per run, %s for the whole burst)",
			perf.MiB(uint64(bytesTotal/burstRooms)), perf.MiB(uint64(bytesTotal/(burstRooms*loadSeats))),
			perf.MiB(uint64(bytesTotal))))
	perf.Report(t, "8", "SaveMatch (DB) time",
		fmt.Sprintf("%s | sum %s across %d transactions on a %d-conn pool",
			perf.Summary(dbDurs), dbTotal.Round(time.Millisecond), len(calls), burstPoolConns))
	perf.Report(t, "8", "gzip-to-DB handoff",
		fmt.Sprintf("first SaveMatch entered %s after release, last entered %s after release",
			firstStart.Sub(releasedAt).Round(time.Millisecond), lastStart.Sub(releasedAt).Round(time.Millisecond)))

	// --- re-price the gzip on the exact bytes that were written -------------
	gzipTotal, gzipRuns := repriceGzip(t, calls)
	perf.Report(t, "8", "gzip+marshal cost (re-run on the captures that were persisted)",
		fmt.Sprintf("%s of CPU across %d runs = %s per run, %s per match; %.1f%% of the %s burst on %d cores",
			gzipTotal.Round(time.Millisecond), gzipRuns, (gzipTotal/time.Duration(gzipRuns)).Round(time.Microsecond),
			(gzipTotal/burstRooms).Round(time.Millisecond),
			100*gzipTotal.Seconds()/(burstWall.Seconds()*float64(runtime.NumCPU())), burstWall.Round(time.Millisecond),
			runtime.NumCPU()))

	// --- the headline: unrelated requests during the burst ------------------
	baseline := probeWindow(samples, baselineFrom, baselineTo)
	during := probeWindow(samples, releasedAt, burstEnd)
	after := probeWindow(samples, burstEnd, burstEnd.Add(afterburnHold))
	require.NotEmpty(t, baseline, "no baseline probe samples")
	require.NotEmptyf(t, during, "no probe samples inside the %s burst", burstWall)

	basep99 := perf.Percentile(baseline, 99)
	duringp99 := perf.Percentile(during, 99)
	perf.Report(t, "8", "unrelated request, baseline", perf.Summary(baseline))
	perf.Report(t, "8", "unrelated request, during the burst", perf.Summary(during))
	if len(after) > 0 {
		perf.Report(t, "8", "unrelated request, after the burst", perf.Summary(after))
	}
	degradation := float64(duringp99) / float64(basep99)
	perf.Report(t, "8", "unrelated-request p99 degradation",
		fmt.Sprintf("%s → %s = %.2fx", basep99.Round(10*time.Microsecond), duringp99.Round(10*time.Microsecond), degradation))

	perf.Budget{
		Zone:      "8",
		Workload:  "unrelated request p99 during a 20-match end burst",
		Limit:     50 * time.Millisecond,
		Rationale: "a match end is routine; an unrelated read that crosses 50 ms is a stall a player feels on an unrelated page",
	}.Assert(t, duringp99)
	if degradation > 3 {
		t.Errorf("BUDGET MISSED 8 | unrelated request p99 degradation | %.2fx baseline (%s → %s), expected <=3x; "+
			"the burst is starving the shared pool", degradation, basep99.Round(10*time.Microsecond),
			duringp99.Round(10*time.Microsecond))
	}

	// --- the burst itself ---------------------------------------------------
	perf.Budget{
		Zone:      "8",
		Workload:  fmt.Sprintf("%d simultaneous match ends, capture persisted", burstRooms),
		Limit:     5 * time.Second,
		Rationale: "Room.persist gives itself a 15 s context; a routine burst that eats a third of it is one spike away from losing captures",
	}.Assert(t, burstWall)
	perf.Report(t, "8", "burst wall time breakdown",
		fmt.Sprintf("%s total; slowest single SaveMatch %s; DB time summed %s = %.1fx the wall, i.e. that many transactions in flight on average against %d connections",
			burstWall.Round(time.Millisecond), dbMax.Round(time.Millisecond), dbTotal.Round(time.Millisecond),
			dbTotal.Seconds()/burstWall.Seconds(), burstPoolConns))

	perf.AssertBytes(t, "8", "peak live heap during the burst", peakHeap, 256<<20,
		"20 matches x 5 runs x 600 batches are marshalled to JSON and gzip'd concurrently; the server must survive its worst realistic simultaneous end")
	perf.Report(t, "8", "peak memory",
		fmt.Sprintf("heap %s, sys %s, %d samples", perf.MiB(peakHeap), perf.MiB(peakSys), sampler.Samples()))

	// --- is persist really off-lock? ----------------------------------------
	var endLag, stateLag time.Duration
	for _, lr := range rooms {
		lc := lr.conns[0]
		require.Positivef(t, lc.matchEndAt.Load(), "room %s never received match_end", lr.code)
		require.Positivef(t, lc.roomStateAt.Load(), "room %s never returned to the lobby", lr.code)
		if d := time.Duration(lc.matchEndAt.Load()) - time.Duration(releaseNanos(releasedAt)); d > endLag {
			endLag = d
		}
		if d := time.Duration(lc.roomStateAt.Load()) - time.Duration(releaseNanos(releasedAt)); d > stateLag {
			stateLag = d
		}
	}
	perf.Report(t, "8", "persist is off-lock (code): endMatchLocked spawns `go r.persist(snap)` before it broadcasts",
		"confirmed by reading internal/ws/room.go:922-925 — the snapshot is taken under the lock, the gzip and the transaction are not")
	perf.Report(t, "8", "persist is off-lock (measured)",
		fmt.Sprintf("slowest room reached match_end %s after release and was back in the lobby %s after release, "+
			"while the burst took %s and the slowest transaction alone took %s",
			endLag.Round(time.Millisecond), stateLag.Round(time.Millisecond),
			burstWall.Round(time.Millisecond), dbMax.Round(time.Millisecond)))

	chatRT := time.Duration(chatter.chatAt.Load() - chatSentAt)
	require.Positive(t, chatter.chatAt.Load(), "the chat probe never came back: a room was wedged by the burst")
	perf.Budget{
		Zone:      "8",
		Workload:  "room round trip (chat) issued at the instant the burst starts",
		Limit:     100 * time.Millisecond,
		Rationale: "this round trip takes the same room lock endMatchLocked holds; on-lock persistence would make it cost the whole gzip-plus-transaction time",
	}.Assert(t, chatRT)

	// The capture really is in the database.
	var matches, runs int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM matches`).Scan(&matches))
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM match_runs`).Scan(&runs))
	require.EqualValues(t, burstRooms, matches)
	require.EqualValues(t, burstRooms*loadSeats, runs)
	var storedBatches int
	require.NoError(t, pool.QueryRow(ctx, `SELECT min(batch_count) FROM match_runs`).Scan(&storedBatches))
	perf.Report(t, "8", "persisted",
		fmt.Sprintf("%d matches, %d runs, min batch_count %d", matches, runs, storedBatches))

	// One more write, alone, on the same pool and the same payload shape. The
	// difference against the burst's mean is queueing, not work: it says how
	// much of a match end's cost comes from the other nineteen.
	solo := calls[0]
	rec := ws.MatchRecord{
		ID: "m_soloref01", RoomCode: "SOLO01", Name: "Solo Reference",
		Settings: json.RawMessage(`{"mode":"time","durationMs":900000,"textSource":{"kind":"seeded"}}`),
		Freemods: json.RawMessage(`[]`),
		Seed:     1, DictHash: "en-default", Lang: "en",
		GoAt: time.Now().Add(-time.Minute), EndedAt: time.Now(),
	}
	for i, blob := range solo.logs {
		rec.Runs = append(rec.Runs, ws.MatchRunRecord{
			PlayerID: fmt.Sprintf("solo%d", i), Nick: fmt.Sprintf("Solo-%d", i),
			Freemods: json.RawMessage(`{"difficulty":"normal","minWpm":0,"nospace":false}`),
			Log:      blob, BatchCount: burstBatches, FinalStatus: protocol.StatusFinished,
		})
	}
	soloStart := time.Now()
	require.NoError(t, store.inner.SaveMatch(ctx, rec))
	soloDur := time.Since(soloStart)
	perf.Report(t, "8", "SaveMatch alone vs in the burst",
		fmt.Sprintf("%s uncontended vs %s mean under 20-way contention = %.1fx queueing on a %d-conn pool",
			soloDur.Round(time.Millisecond), (dbTotal/time.Duration(len(calls))).Round(time.Millisecond),
			float64(dbTotal/time.Duration(len(calls)))/float64(soloDur), burstPoolConns))
}

// releaseNanos converts a wall instant into this file's loadNanos timeline. Both
// come from the same process clock, so the offset is a single subtraction.
func releaseNanos(at time.Time) int64 {
	return int64(at.Sub(loadEpoch))
}

// repriceGzip re-runs marshal+gzip over the captures that were actually
// persisted, which is the only way to separate gzip time from DB time without
// touching Room.persist. The input is recovered by decompressing what the store
// received, so it is byte-for-byte the work the server did.
func repriceGzip(t *testing.T, calls []saveCall) (time.Duration, int) {
	t.Helper()
	var payloads [][]ws.CapturedBatch
	for _, c := range calls {
		for _, blob := range c.logs {
			zr, err := gzip.NewReader(bytes.NewReader(blob))
			require.NoError(t, err)
			raw, err := io.ReadAll(zr)
			require.NoError(t, err)
			require.NoError(t, zr.Close())
			var cb []ws.CapturedBatch
			require.NoError(t, json.Unmarshal(raw, &cb))
			payloads = append(payloads, cb)
		}
	}
	start := time.Now()
	for _, cb := range payloads {
		raw, err := json.Marshal(cb)
		require.NoError(t, err)
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		_, err = zw.Write(raw)
		require.NoError(t, err)
		require.NoError(t, zw.Close())
	}
	return time.Since(start), len(payloads)
}
