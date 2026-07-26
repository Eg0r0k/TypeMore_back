//go:build load

package auth_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"runtime/debug"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/typemore/typemore-server/internal/auth"
	"github.com/typemore/typemore-server/internal/auth/pgstore"
	"github.com/typemore/typemore-server/internal/perf"
	"github.com/typemore/typemore-server/internal/platform/db"
)

// Zone 1: argon2id memory under concurrent auth.
//
// The subject is the only unauthenticated path in the system that can commit
// tens of megabytes of heap per request. Every scenario drives real HTTP
// against a real Postgres, because the interesting hash — the decoy verify on
// an unknown email — only happens after the store has been consulted, and a
// fake store would be measuring a different flow.
const zoneAuth = "zone1-auth-hashing"

// gateLimit is the gate size the budgets are written against. Eight is the
// order of magnitude a small deployment lands on (a ~640 MiB hash budget), and
// large enough that queueing rather than shedding dominates.
const gateLimit = 8

// harnessOverheadBytes is what this process holds that is not argon2: the
// httptest server, the pgx pool and its buffers, chi, the burst driver's
// connections and goroutines, and the testcontainers client that stays alive
// for the whole run. Measured baseline between scenarios is 1–9 MiB; 64 MiB
// leaves room for a burst's connections and response bodies while staying well
// under the 19 MiB granularity of a leaked hash.
const harnessOverheadBytes = 64 << 20

// hashHeapFactor sizes the one absolute peak-heap ceiling still asserted here:
// the saturated case, where shed requests never hash and the measurement is
// consequently stable (160.2 / 160.4 / 181.5 MiB across three runs).
//
// # Why there is no absolute ceiling on the un-shed scenarios
//
// There was one, at 2x nominal, from first principles: the collector lets the
// heap reach roughly twice the live set before it runs. That was wrong for this
// workload — argon2id's garbage is coarse 19 MiB blocks, which the default
// hysteresis handles worst — so it was re-derived at 3x from measurement. Then
// the identical scenario (gate 8, 200 concurrent) measured, across three runs:
//
//	407.1 MiB (2.68x nominal), 464.0 MiB (3.05x), 558.6 MiB (3.67x)
//
// A 37% spread. Peak HeapAlloc at GOGC=100 is not a function of the gate; it is
// a function of where the collector happened to be in its cycle when the burst
// landed, quantised to 19 MiB per uncollected block. Raising the factor again
// would be fitting a threshold to noise — the ratchet that turns a budget into
// a number nobody believes.
//
// So the noisy quantity is REPORTED (it is what a deployment needs for sizing,
// and a range is the honest form of it), and the assertions moved to two things
// that are deterministic:
//
//  1. PeakInFlight <= limit — exact, held in every scenario of every run. This
//     is the gate's actual contract, and live hashing memory is exactly
//     PeakInFlight x HashCostBytes.
//  2. Gated peak heap against the UNGATED peak on the same burst, same box,
//     same run — self-calibrating, so arena growth and collector phase move
//     both sides together. Measured 3.9x and 5.6x; asserted at 2x.
const hashHeapFactor = 3

// heapBudget is the peak-heap ceiling for a saturated gate of n slots.
func heapBudget(n int) uint64 {
	return hashHeapFactor*uint64(n)*auth.HashCostBytes + harnessOverheadBytes
}

// heapRationale is the whole story behind that ceiling, carried on the
// assertion so a reader of the test log never has to take the number on trust.
func heapRationale(n int) string {
	return fmt.Sprintf(
		"%d slots x %s nominal x %d + %s harness. %dx not 2x: measured GOGC=100 hysteresis on 19 MiB blocks is 2.4-3.67x across runs (GOGC=20 -> 1.55-1.80x). Only the saturated case is bounded absolutely, because shed requests never hash and its measurement is stable; the un-shed scenarios report peak heap and assert against the ungated run instead. The gate's own invariant (PeakInFlight <= limit) is asserted separately and exactly",
		n, perf.MiB(auth.HashCostBytes), hashHeapFactor, perf.MiB(harnessOverheadBytes), hashHeapFactor)
}

// --- harness ---

type loadHarness struct {
	t      *testing.T
	server *httptest.Server
	client *http.Client
	svc    *auth.Service
	mailer *recorderMailer
	pool   *pgxpool.Pool
}

// newLoadHarness wires the auth service as cmd/server does, with the hashing
// gate configured for the scenario. A non-positive concurrency disables the
// gate — that is the ungated baseline.
func newLoadHarness(t *testing.T, concurrency int, wait time.Duration) *loadHarness {
	t.Helper()
	ctx := context.Background()

	// Sized above the widest burst's steady-state demand: a login makes up to
	// four round trips and none of them should queue behind the pool while we
	// are attributing memory and time to hashing.
	pool, err := db.NewPool(ctx, ensureDB(t), 32)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	_, err = pool.Exec(ctx, `TRUNCATE users, sessions, email_tokens, auth_identities, user_credentials RESTART IDENTITY CASCADE`)
	require.NoError(t, err)
	// Opening 32 Postgres connections through Docker's network stack costs
	// hundreds of milliseconds, and pgx opens them on demand. Left cold, the
	// first measured burst spends that time in the store instead of in argon2,
	// which both inflates its latency and staggers the arrivals at the gate.
	warm := make([]*pgxpool.Conn, 0, 32)
	for range cap(warm) {
		c, err := pool.Acquire(ctx)
		require.NoError(t, err)
		warm = append(warm, c)
	}
	for _, c := range warm {
		c.Release()
	}

	store := pgstore.New(pool)
	mailer := &recorderMailer{}
	svc := auth.NewService(store, store, mailer,
		// Burst 0 disables the per-IP limiter, which is the point of the zone:
		// the limiter cannot bound hashing memory — it is per IP, so a botnet
		// never trips it — and leaving it in the path would measure the token
		// bucket instead of the gate.
		auth.NewInMemoryRateLimiter(0, 0),
		nil, // no captcha: the zone measures the hashing gate, not the front door
		auth.Config{
			FrontendOrigin:  frontendOrigin,
			CookieName:      "tm_session",
			CookieSecure:    false,
			SessionTTL:      time.Hour,
			HashConcurrency: concurrency,
			HashWait:        wait,
		}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	r := chi.NewRouter()
	r.Route("/api/v1", func(r chi.Router) { r.Mount("/auth", svc.AuthRoutes()) })
	server := httptest.NewServer(r)
	t.Cleanup(server.Close)

	h := &loadHarness{
		t:      t,
		server: server,
		svc:    svc,
		mailer: mailer,
		pool:   pool,
		// Setup and follow-up requests only; the bursts use raw connections.
		// No cookie jar: nothing here needs a session.
		client: &http.Client{Timeout: 5 * time.Minute},
	}
	t.Cleanup(h.client.CloseIdleConnections)
	return h
}

// postJSON sends one sequential auth request with the CSRF Origin header.
func (h *loadHarness) postJSON(path string, body any) (int, string) {
	raw, err := json.Marshal(body)
	require.NoError(h.t, err)
	req, err := http.NewRequest(http.MethodPost, h.server.URL+authBase+path, bytes.NewReader(raw))
	require.NoError(h.t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", frontendOrigin)

	resp, err := h.client.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, err.Error()
	}
	return resp.StatusCode, string(out)
}

// setupUser registers and verifies an account so the valid-login arm exercises
// a real credential verify rather than the decoy.
func (h *loadHarness) setupUser(email, password, name string) {
	h.t.Helper()
	status, body := h.postJSON("/register", map[string]string{
		"email": email, "password": password, "displayName": name,
	})
	require.Equal(h.t, http.StatusOK, status, "register: %s", body)
	status, body = h.postJSON("/verify", map[string]string{"token": h.mailer.lastToken(h.t)})
	require.Equal(h.t, http.StatusOK, status, "verify: %s", body)
}

// --- burst driver ---

type burstResult struct {
	statuses map[int]int
	// sample keeps one response body per status so a surprising code can be
	// diagnosed from the test log instead of rerun under a debugger.
	sample  map[int]string
	latency []time.Duration
	elapsed time.Duration
	// delivered is how long the driver took to put all n requests on the wire.
	// It is the honesty check on "concurrent": if it is milliseconds, every
	// request really did arrive together and any stagger at the gate is the
	// server's own backpressure, not the harness.
	delivered time.Duration
	baseline  uint64
	peakHeap  uint64
	peakSys   uint64
	samples   int
}

func (r burstResult) perSec() float64 {
	return float64(len(r.latency)) / r.elapsed.Seconds()
}

func (r burstResult) String() string {
	return fmt.Sprintf("%v in %s (delivered in %s, %.0f/s), peak heap %s (baseline %s, peak sys %s), %s",
		r.statuses, r.elapsed.Round(time.Millisecond), r.delivered.Round(time.Millisecond), r.perSec(),
		perf.MiB(r.peakHeap), perf.MiB(r.baseline), perf.MiB(r.peakSys),
		perf.Summary(append([]time.Duration(nil), r.latency...)))
}

// wire renders one request to its HTTP/1.1 bytes. The burst sends the same
// bytes down every connection, so building them once keeps the driver's own
// allocations out of the measurement.
func (h *loadHarness) wire(path string, body any) []byte {
	h.t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(h.t, err)
	req, err := http.NewRequest(http.MethodPost, h.server.URL+authBase+path, bytes.NewReader(raw))
	require.NoError(h.t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", frontendOrigin)
	var buf bytes.Buffer
	require.NoError(h.t, req.Write(&buf))
	return buf.Bytes()
}

// burst delivers n requests that genuinely arrive together, and reports the
// status distribution, latency and peak heap of the window.
//
// The obvious driver — n goroutines each calling http.Client.Do — does not work
// here, and understates the problem badly. Client and server share this
// process, so once a few dozen server goroutines are burning every P on
// argon2, the client goroutines that have not sent yet cannot be scheduled:
// arrivals end up paced by completions. Measured with that driver, a
// 200-request ungated burst only ever had 36 hashes in flight, which is not the
// scenario anyone is worried about.
//
// So every connection is dialled and given a parked reader first — a goroutine
// blocked in the netpoller costs no CPU — and then a single goroutine writes
// all n requests. Writing n small buffers needs one P for about a millisecond,
// so arrival is simultaneous no matter what the server is doing.
func (h *loadHarness) burst(n int, wire []byte) burstResult {
	h.t.Helper()
	addr := h.server.Listener.Addr().String()

	conns := make([]net.Conn, n)
	for i := range n {
		c, err := net.Dial("tcp", addr)
		require.NoError(h.t, err, "dial %d of %d", i, n)
		conns[i] = c
	}
	defer func() {
		for _, c := range conns {
			_ = c.Close()
		}
	}()

	// Each goroutine writes only its own index, so the results need no lock:
	// done.Wait() is the happens-before edge that publishes them.
	var (
		done    sync.WaitGroup
		ready   sync.WaitGroup
		codes   = make([]int, n)
		bodies  = make([]string, n)
		sentAt  = make([]time.Time, n)
		doneAt  = make([]time.Time, n)
		readers = make([]*bufio.Reader, n)
	)
	done.Add(n)
	ready.Add(n)
	for i := range n {
		readers[i] = bufio.NewReader(conns[i])
		go func() {
			defer done.Done()
			ready.Done()
			resp, err := http.ReadResponse(readers[i], nil)
			if err != nil {
				doneAt[i] = time.Now()
				bodies[i] = err.Error()
				return
			}
			out, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			doneAt[i] = time.Now()
			codes[i] = resp.StatusCode
			if err != nil {
				bodies[i] = err.Error()
				return
			}
			bodies[i] = string(out)
		}()
	}
	// The readers are only parked once they have reached the socket read; a
	// reader still waiting for its first schedule would add its wake-up latency
	// to the measurement.
	ready.Wait()
	time.Sleep(10 * time.Millisecond)

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	sampler := perf.NewPeakSampler(2 * time.Millisecond)

	t0 := time.Now()
	for i := range n {
		sentAt[i] = time.Now()
		_, err := conns[i].Write(wire)
		require.NoError(h.t, err, "write request %d of %d", i, n)
	}
	delivered := time.Since(t0)
	done.Wait()
	elapsed := time.Since(t0)

	res := burstResult{
		statuses:  map[int]int{},
		sample:    map[int]string{},
		latency:   make([]time.Duration, n),
		elapsed:   elapsed,
		delivered: delivered,
		baseline:  before.HeapAlloc,
	}
	res.peakHeap, res.peakSys = sampler.Stop()
	res.samples = sampler.Samples()
	for i := range n {
		res.statuses[codes[i]]++
		if _, seen := res.sample[codes[i]]; !seen {
			res.sample[codes[i]] = bodies[i]
		}
		res.latency[i] = doneAt[i].Sub(sentAt[i])
	}
	return res
}

// --- scenarios ---

const (
	loadUserEmail    = "load-user@example.com"
	loadUserPassword = "correct horse battery staple"
	// unknownEmail never exists, so a login for it takes the decoy-verify path:
	// one full argon2id hash for a request that carries no credential and needs
	// no account. That is the cheapest possible attack and the one the gate
	// mainly has to cover.
	unknownEmail  = "nobody-at-all@example.com"
	wrongPassword = "whatever-it-does-not-matter"
)

// burstSizes is the concurrency curve every arm is measured at. 200 is the
// number quoted in hashgate.go's own doc comment (~3.8 GiB ungated), so it is
// the one that has to be demonstrated rather than assumed.
var burstSizes = []int{10, 50, 200}

// reportBurst emits the common measurement lines for one scenario.
func reportBurst(t *testing.T, workload string, res burstResult, stats auth.HashGateStats) {
	t.Helper()
	perf.Report(t, zoneAuth, workload, res)
	perf.Report(t, zoneAuth, workload+": peak heap", perf.MiB(res.peakHeap))
	perf.Report(t, zoneAuth, workload+": logins/sec", fmt.Sprintf("%.1f", res.perSec()))
	perf.Report(t, zoneAuth, workload+": peak hashes in flight", stats.PeakInFlight)
	perf.Report(t, zoneAuth, workload+": committed hashing memory",
		perf.MiB(uint64(stats.PeakInFlight)*auth.HashCostBytes))
	perf.Report(t, zoneAuth, workload+": heap per hash slot",
		perf.MiB(res.peakHeap/uint64(max(stats.PeakInFlight, 1))))
	perf.Report(t, zoneAuth, workload+": delivery window", res.delivered.Round(100*time.Microsecond))
}

// TestLoadAuthHashUngatedBaseline documents the DoS the gate exists to close.
// It asserts nothing about memory on purpose: the finding IS the number, and a
// budget here would either be met trivially or fail on every box.
func TestLoadAuthHashUngatedBaseline(t *testing.T) {
	h := newLoadHarness(t, -1, auth.DefaultHashWait)
	require.Zero(t, h.svc.HashGateStats().Limit, "the baseline must run with the gate disabled")
	invalid := h.wire("/login", map[string]string{"email": unknownEmail, "password": wrongPassword})

	for _, n := range burstSizes {
		before := h.svc.HashGateStats()
		res := h.burst(n, invalid)
		after := h.svc.HashGateStats()

		require.Positive(t, res.samples, "the sampler must have observed the window")
		assert.Equal(t, n, res.statuses[http.StatusUnauthorized],
			"every unknown-email login should reach the decoy verify: %v %v", res.statuses, res.sample)
		assert.Zero(t, after.Shed-before.Shed, "an ungated service must never shed")

		// Peak in flight is the whole point: nothing bounds it, so committed
		// hashing memory tracks it in 19 MiB steps.
		reportBurst(t, fmt.Sprintf("UNGATED %d concurrent invalid logins", n), res, after)
		perf.Report(t, zoneAuth, fmt.Sprintf("UNGATED %d concurrent: fraction of the burst hashing at once", n),
			fmt.Sprintf("%d/%d (%.0f%%)", after.PeakInFlight, n, float64(after.PeakInFlight)/float64(n)*100))

		// What the harness controls is delivery, and that is what it asserts:
		// every request is on the wire within a few milliseconds. How many of
		// them are hashing simultaneously after that is the SERVER's answer —
		// on a box with fewer cores than the burst, GC assist and the scheduler
		// pace arrivals at the gate, so the observed peak is a lower bound on
		// the exposure, not a ceiling on it.
		assert.Less(t, res.delivered, 100*time.Millisecond,
			"the burst must be delivered essentially at once, or it is not measuring concurrency")
	}
}

// TestLoadAuthHashGatedMemory is the budgeted arm: the same curve with the gate
// on, holding peak heap under a ceiling derived from the gate size, on both the
// valid-credential path and the decoy path an attacker gets for free.
func TestLoadAuthHashGatedMemory(t *testing.T) {
	// A queue deep enough that all 200 requests are served rather than shed.
	// This scenario measures the ceiling under sustained load, and a shed
	// request costs no memory at all — a short wait would let the budget pass
	// by doing less work. Shedding is tested separately, at the production
	// default.
	h := newLoadHarness(t, gateLimit, 60*time.Second)
	require.Equal(t, gateLimit, h.svc.HashGateStats().Limit)
	h.setupUser(loadUserEmail, loadUserPassword, "LoadUser")

	nominal := uint64(gateLimit) * auth.HashCostBytes
	arms := []struct {
		name string
		want int
		wire []byte
	}{
		{"invalid", http.StatusUnauthorized,
			h.wire("/login", map[string]string{"email": unknownEmail, "password": wrongPassword})},
		{"valid", http.StatusOK,
			h.wire("/login", map[string]string{"email": loadUserEmail, "password": loadUserPassword})},
	}

	// widest is the peak heap of the largest gated burst, kept for the
	// gated-versus-ungated comparison below.
	var widest uint64
	for _, arm := range arms {
		for _, n := range burstSizes {
			before := h.svc.HashGateStats()
			res := h.burst(n, arm.wire)
			after := h.svc.HashGateStats()

			require.Positive(t, res.samples)
			assert.Less(t, res.delivered, 100*time.Millisecond,
				"the burst must be delivered essentially at once, or it is not measuring concurrency")
			workload := fmt.Sprintf("gated(%d) %d concurrent %s logins", gateLimit, n, arm.name)
			reportBurst(t, workload, res, after)

			assert.Equal(t, n, res.statuses[arm.want],
				"a 60s queue should serve every request: %v %v", res.statuses, res.sample)
			assert.Zero(t, after.Shed-before.Shed, "nothing should be shed with a 60s wait")
			assert.Equal(t, int64(n), after.Admitted-before.Admitted,
				"exactly one hash per login attempt, valid or not")

			// The exact invariant. Peak in flight × 19 MiB is the live hashing
			// memory the process actually committed.
			assert.LessOrEqual(t, after.PeakInFlight, int64(gateLimit),
				"the gate admitted more concurrent hashes than it has slots")

			// Absolute peak heap is REPORTED, not bounded: see hashHeapFactor
			// for the three runs that showed a 37% spread on this exact
			// measurement. The ratio to nominal is what a deployment sizes
			// from, so that is what gets logged.
			perf.Report(t, zoneAuth, workload+": peak heap / nominal gate cost",
				fmt.Sprintf("%.2fx (%s against %s)", float64(res.peakHeap)/float64(nominal),
					perf.MiB(res.peakHeap), perf.MiB(nominal)))

			if n == burstSizes[len(burstSizes)-1] {
				widest = max(widest, res.peakHeap)
			}
		}
	}

	// The budget that replaces the absolute one. It compares the gated server
	// against the SERVER WITHOUT THE GATE — same burst, same box, same run — so
	// arena growth and collector phase move both sides together and the
	// comparison stays meaningful when the raw number will not hold still.
	// Measured 3.9x and 5.6x; the ceiling is half the ungated peak, which fails
	// loudly if the gate ever stops doing anything while leaving room for the
	// drift on both sides.
	u := newLoadHarness(t, -1, auth.DefaultHashWait)
	ungated := u.burst(burstSizes[len(burstSizes)-1],
		u.wire("/login", map[string]string{"email": unknownEmail, "password": wrongPassword}))
	require.Positive(t, ungated.peakHeap)
	perf.Report(t, zoneAuth, "gated vs ungated peak heap at the widest burst",
		fmt.Sprintf("%s gated vs %s ungated (%.1fx)", perf.MiB(widest), perf.MiB(ungated.peakHeap),
			float64(ungated.peakHeap)/float64(widest)))
	perf.AssertBytes(t, zoneAuth,
		fmt.Sprintf("gated(%d) peak heap vs ungated, %d concurrent", gateLimit, burstSizes[len(burstSizes)-1]),
		widest, ungated.peakHeap/2,
		"half the peak heap the SAME burst reaches with the gate off, measured in the same run. A relative ceiling because the absolute one is not a function of the gate: peak HeapAlloc at GOGC=100 varied 407-559 MiB across three runs of this scenario while PeakInFlight stayed at exactly 8")
}

// TestLoadAuthGateShedsUnderSaturation runs the production wait and asserts
// what a client sees when the server is out of hashing capacity: a prompt 503
// "overloaded", never a 500 and never a hang — and a working login immediately
// afterwards.
func TestLoadAuthGateShedsUnderSaturation(t *testing.T) {
	const limit = 4
	h := newLoadHarness(t, limit, auth.DefaultHashWait)
	h.setupUser(loadUserEmail, loadUserPassword, "LoadUser")

	// 200 attempts against 4 slots at ~50 ms per hash is ~2.5 s of work behind
	// a 500 ms queue: saturation is certain, not timing-dependent.
	const attempts = 200
	before := h.svc.HashGateStats()
	res := h.burst(attempts, h.wire("/login", map[string]string{"email": unknownEmail, "password": wrongPassword}))
	after := h.svc.HashGateStats()

	workload := fmt.Sprintf("saturated gate(%d), %d concurrent invalid logins", limit, attempts)
	reportBurst(t, workload, res, after)

	shed := int64(res.statuses[http.StatusServiceUnavailable])
	assert.Positive(t, shed, "a 4-slot gate under 200 concurrent logins must shed: %v", res.statuses)
	assert.Equal(t, shed, after.Shed-before.Shed, "every 503 must be a gate shed, and every shed a 503")
	assert.Zero(t, res.statuses[http.StatusInternalServerError], "shedding is not an error: %v", res.sample)
	assert.Zero(t, res.statuses[http.StatusTooManyRequests], "the rate limiter is disabled here")
	assert.Zero(t, res.statuses[0], "no request may fail at the transport: %v", res.sample)
	assert.Equal(t, attempts, res.statuses[http.StatusUnauthorized]+res.statuses[http.StatusServiceUnavailable],
		"only 401 and 503 are legal outcomes here: %v", res.statuses)

	// The shed response is the documented contract, not an anonymous 503.
	var body errResponse
	require.NoError(t, json.Unmarshal([]byte(res.sample[http.StatusServiceUnavailable]), &body))
	assert.Equal(t, "overloaded", body.Error)

	assert.LessOrEqual(t, after.PeakInFlight, int64(limit))
	perf.AssertBytes(t, zoneAuth, workload+": peak heap", res.peakHeap, heapBudget(limit),
		"shed requests never hash, so a saturated server must stay at its slot ceiling. "+heapRationale(limit))

	// A shed burst must not have poisoned anything: the gate is a semaphore,
	// not a circuit breaker, so the next login works.
	status, respBody := h.postJSON("/login", map[string]string{"email": loadUserEmail, "password": loadUserPassword})
	assert.Equal(t, http.StatusOK, status, "the gate must not be sticky after a burst: %s", respBody)

	perf.Report(t, zoneAuth, workload+": shed rate",
		fmt.Sprintf("%d/%d (%.0f%%)", shed, attempts, float64(shed)/attempts*100))
	perf.Report(t, zoneAuth, workload+": p99 latency",
		perf.Percentile(append([]time.Duration(nil), res.latency...), 99))
}

// TestLoadAuthHashGateGCSensitivity explains the gap between the gate's exact
// bound (slots × 19 MiB of LIVE memory) and the heap a deployment actually
// needs, by rerunning one scenario under two collector settings.
//
// This is diagnosis, not a budget: if the excess is garbage the collector has
// not got to yet, then tightening GOGC or setting GOMEMLIMIT removes it and the
// gate can be sized from its nominal cost. If it does not shrink, the excess is
// live and the gate is sized wrong. Throughput is reported alongside, because
// collecting harder is not free.
func TestLoadAuthHashGateGCSensitivity(t *testing.T) {
	h := newLoadHarness(t, gateLimit, 60*time.Second)
	invalid := h.wire("/login", map[string]string{"email": unknownEmail, "password": wrongPassword})
	const n = 200

	nominal := uint64(gateLimit) * auth.HashCostBytes
	perf.Report(t, zoneAuth, "GC sensitivity: nominal gate cost", perf.MiB(nominal))

	run := func(label string) burstResult {
		res := h.burst(n, invalid)
		require.Equal(t, n, res.statuses[http.StatusUnauthorized], "%s: %v", label, res.statuses)
		perf.Report(t, zoneAuth, fmt.Sprintf("GC sensitivity: %s peak heap", label), perf.MiB(res.peakHeap))
		perf.Report(t, zoneAuth, fmt.Sprintf("GC sensitivity: %s logins/sec", label),
			fmt.Sprintf("%.1f", res.perSec()))
		perf.Report(t, zoneAuth, fmt.Sprintf("GC sensitivity: %s peak heap / nominal", label),
			fmt.Sprintf("%.2fx", float64(res.peakHeap)/float64(nominal)))
		return res
	}

	def := run("GOGC=100 (default)")

	oldGC := debug.SetGCPercent(20)
	tight := run("GOGC=20")
	debug.SetGCPercent(oldGC)

	// A soft memory limit is the knob a container deployment actually has, and
	// unlike GOGC it is expressed in the same unit as the gate's budget.
	oldLimit := debug.SetMemoryLimit(math.MaxInt64)
	require.Equal(t, int64(math.MaxInt64), oldLimit, "the suite should start with no memory limit set")
	debug.SetMemoryLimit(384 << 20)
	limited := run("GOMEMLIMIT=384MiB")
	debug.SetMemoryLimit(oldLimit)

	// Each knob is measured in the same process, so the heap arena the previous
	// run grew is still mapped. Repeating the default at the end is the control
	// that says whether ordering, rather than the setting, moved the number.
	control := run("GOGC=100 (control, repeated last)")

	perf.Report(t, zoneAuth, "GC sensitivity: throughput cost of GOGC=20",
		fmt.Sprintf("%.0f%% of default", tight.perSec()/def.perSec()*100))
	perf.Report(t, zoneAuth, "GC sensitivity: throughput cost of GOMEMLIMIT=384MiB",
		fmt.Sprintf("%.0f%% of default", limited.perSec()/def.perSec()*100))
	perf.Report(t, zoneAuth, "GC sensitivity: control drift vs first default",
		fmt.Sprintf("%.2fx peak heap", float64(control.peakHeap)/float64(def.peakHeap)))
	assert.LessOrEqual(t, h.svc.HashGateStats().PeakInFlight, int64(gateLimit),
		"the gate must hold regardless of collector settings")
}

// --- per-hash cost ---

// BenchmarkHashPassword is the number every budget in this zone derives from:
// what one argon2id hash costs in time and in bytes.
func BenchmarkHashPassword(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		if _, err := auth.HashPassword(loadUserPassword); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkVerifyPassword is the login-path cost. It should match the hash cost
// closely: verification runs the same KDF with the parameters read back out of
// the stored PHC string.
func BenchmarkVerifyPassword(b *testing.B) {
	encoded, err := auth.HashPassword(loadUserPassword)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ok, err := auth.VerifyPassword(loadUserPassword, encoded)
		if err != nil || !ok {
			b.Fatalf("verify failed: %v %v", ok, err)
		}
	}
}
