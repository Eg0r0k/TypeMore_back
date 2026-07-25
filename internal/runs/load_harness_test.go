//go:build load

package runs_test

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/typemore/typemore-server/internal/perf"
)

// loadSeed fixes every generated fixture in this suite. A load run that changed
// its own input between invocations would be measuring dice, not the server.
const loadSeed uint64 = 0x7a5eed

// capBody renders the largest POST /runs body ingestion actually accepts and
// returns its wire bytes together with the event count that fits.
//
// The two documented caps do not meet — 50 000 events marshal past the 2 MiB
// body limit — so perf.MaxLegalPayload is bounded by BYTES and carries fewer
// events than docs/RUNS.md advertises. That discrepancy is zone 5's headline
// finding; here it just means the fixture must come from the generator rather
// than from the documented constant.
func capBody(t *testing.T) (body []byte, events int) {
	t.Helper()
	events = perf.SubmittableEvents(loadSeed)
	body = perf.MustJSON(perf.MaxLegalPayload(loadSeed))
	require.LessOrEqual(t, len(body), perf.MaxBodyBytes,
		"the accept-path fixture must be a body the server will take")
	return body, events
}

// oversizeBody renders a syntactically valid body of close to want bytes (never
// under), by scaling the event count against a measured bytes-per-event.
//
// Validity matters: a body of random padding would be refused by the JSON
// decoder, and the question here is whether the SIZE cap fires first, not
// whether the parser does. Landing NEAR the target matters too, because the
// claim under test is "the server's cost does not track what was sent" and a
// probe that overshoots by 30% makes that ratio unreadable.
func oversizeBody(t *testing.T, want int) []byte {
	t.Helper()
	build := func(events int) []byte {
		return perf.MustJSON(perf.BuildPayload(perf.PayloadSpec{
			Setup: perf.SetupSpec{Mode: "words", WordCount: perf.MaxWordCount, DurationMs: perf.MaxDurationMs},
			Log:   perf.LogSpec{Events: events, Seed: loadSeed},
		}))
	}
	events := perf.MaxEvents
	body := build(events)
	for range 4 {
		if len(body) >= want {
			break
		}
		events = events * want / len(body)
		body = build(events + events/50)
	}
	require.GreaterOrEqual(t, len(body), want, "could not build an oversized body")
	return body
}

// sendBody posts an already-marshalled body with the session cookie and the
// CSRF Origin, draining the response so the connection returns to the pool.
//
// It returns errors instead of asserting because the fan-out helper calls it
// off the test goroutine, where testify's FailNow would abort the wrong stack.
func (h *harness) sendBody(path string, body []byte) (int, error) {
	req, err := http.NewRequest(http.MethodPost, h.server.URL+path, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", frontendOrigin)
	resp, err := h.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	_, err = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, err
}

// fetchDiscard GETs path and streams the body straight to io.Discard.
//
// Discarding rather than buffering is deliberate: holding a 2 MiB replay on the
// client side would add a copy per in-flight request to the very heap peak
// these tests are trying to attribute to the server.
func (h *harness) fetchDiscard(path string) (status int, n int64, err error) {
	req, err := http.NewRequest(http.MethodGet, h.server.URL+path, http.NoBody)
	if err != nil {
		return 0, 0, err
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	n, err = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, n, err
}

// burstResult is one delivered burst: what came back, how long each request
// took, how tightly they arrived, and how many were genuinely in flight.
type burstResult struct {
	statuses  map[int]int
	served    []int64
	latency   []time.Duration
	elapsed   time.Duration
	delivered time.Duration
	overlap   int
	baseline  uint64
	peakHeap  uint64
	peakSys   uint64
	samples   int
	err       error
}

// burst delivers n requests that genuinely arrive together, and reports the
// status distribution, latency, achieved concurrency and peak heap of the
// window.
//
// The obvious driver — n goroutines each calling http.Client.Do — understates
// concurrency badly, because client and server share this process: once server
// goroutines are burning every P on a 2 MiB JSON parse, a client goroutine that
// has not sent yet cannot be scheduled, and arrivals end up paced by
// completions. (Credit: internal/auth/hashgate_load_test.go found this first.)
//
// So every connection is dialled and given a parked reader before the clock
// starts — a goroutine blocked in the netpoller costs no CPU — and then ONE
// goroutine writes the head of every request back to back. The head is what
// starts a server handler, so after that single sub-millisecond pass all n
// requests exist server-side. Bodies here run to 2 MiB, far past any socket
// buffer, so the remainder is streamed by per-connection writers that are
// already parked at the same barrier; they block in the netpoller rather than
// competing for a P.
func (h *harness) burst(n int, wire []byte) burstResult {
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

	// head is what makes the server build a request and enter the handler; the
	// rest is body the handler pulls at its own pace.
	head := min(len(wire), 8<<10)

	var (
		readers = make([]*bufio.Reader, n)
		codes   = make([]int, n)
		served  = make([]int64, n)
		sentAt  = make([]time.Time, n)
		doneAt  = make([]time.Time, n)
		readErr = make([]error, n)
		done    sync.WaitGroup
		ready   sync.WaitGroup
		tails   sync.WaitGroup
		release = make(chan struct{})
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
				doneAt[i], readErr[i] = time.Now(), err
				return
			}
			served[i], readErr[i] = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			doneAt[i] = time.Now()
			codes[i] = resp.StatusCode
		}()
	}
	if head < len(wire) {
		tails.Add(n)
		ready.Add(n)
		for i := range n {
			go func() {
				defer tails.Done()
				ready.Done()
				<-release
				if _, err := conns[i].Write(wire[head:]); err != nil && readErr[i] == nil {
					readErr[i] = err
				}
			}()
		}
	}
	// The readers and writers are only parked once they have reached their
	// blocking call; one still waiting for its first schedule would add its
	// wake-up latency to the measurement.
	ready.Wait()
	time.Sleep(10 * time.Millisecond)

	baseline := liveHeap()
	sampler := perf.NewPeakSampler(2 * time.Millisecond)

	t0 := time.Now()
	for i := range n {
		sentAt[i] = time.Now()
		if _, err := conns[i].Write(wire[:head]); err != nil {
			require.NoError(h.t, err, "write request head %d of %d", i, n)
		}
	}
	delivered := time.Since(t0)
	close(release)
	tails.Wait()
	done.Wait()
	elapsed := time.Since(t0)

	res := burstResult{
		statuses: map[int]int{}, served: served,
		latency: make([]time.Duration, n),
		elapsed: elapsed, delivered: delivered, baseline: baseline,
	}
	res.peakHeap, res.peakSys = sampler.Stop()
	res.samples = sampler.Samples()
	for i := range n {
		res.statuses[codes[i]]++
		res.latency[i] = doneAt[i].Sub(sentAt[i])
		if readErr[i] != nil && res.err == nil {
			res.err = readErr[i]
		}
	}
	res.overlap = maxOverlap(sentAt, doneAt)
	return res
}

// maxOverlap is the largest number of requests in flight at any instant — the
// concurrency the burst ACHIEVED, as opposed to the one it asked for. A memory
// figure taken at an overlap well below n is measuring a smaller burst than the
// budget claims, so every burst reports this next to its peak.
func maxOverlap(sentAt, doneAt []time.Time) int {
	type edge struct {
		at   time.Time
		step int
	}
	edges := make([]edge, 0, 2*len(sentAt))
	for i := range sentAt {
		edges = append(edges, edge{sentAt[i], 1}, edge{doneAt[i], -1})
	}
	// A close and an open at the same instant must close first, or two disjoint
	// requests would read as one overlap.
	slices.SortFunc(edges, func(a, b edge) int {
		if !a.at.Equal(b.at) {
			return a.at.Compare(b.at)
		}
		return a.step - b.step
	})
	live, peak := 0, 0
	for _, e := range edges {
		live += e.step
		peak = max(peak, live)
	}
	return peak
}

// wire renders one request to its HTTP/1.1 bytes, carrying the harness session
// cookie and the CSRF Origin the raw-socket driver cannot get from the client's
// jar on its own. Built once and written down every connection, so the driver's
// own allocations stay out of the measurement.
func (h *harness) wire(method, path string, body []byte) []byte {
	h.t.Helper()
	var rdr io.Reader = http.NoBody
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, h.server.URL+path, rdr)
	require.NoError(h.t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", frontendOrigin)
	u, err := url.Parse(h.server.URL)
	require.NoError(h.t, err)
	for _, c := range h.client.Jar.Cookies(u) {
		req.AddCookie(c)
	}
	var buf bytes.Buffer
	require.NoError(h.t, req.Write(&buf))
	return buf.Bytes()
}

// warmPool opens every connection the harness pool may hold. A cold pgx pool
// pays its dial and its AfterConnect on the first queries of a burst, which
// would land in the latency distribution as if ingestion had cost it.
func (h *harness) warmPool() {
	h.t.Helper()
	n := int(h.pool.Config().MaxConns)
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// pg_sleep holds the connection so the next goroutine is forced to
			// open its own rather than reusing this one.
			_, errs[i] = h.pool.Exec(context.Background(), `SELECT pg_sleep(0.05)`)
		}()
	}
	wg.Wait()
	for _, err := range errs {
		require.NoError(h.t, err)
	}
}

// liveHeap is the settled heap before a measurement window opens. PeakSampler
// reports an absolute high-water mark, so the baseline is what turns it into
// "how much did THIS workload add".
func liveHeap() uint64 {
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms.HeapAlloc
}

// growth is peak minus baseline, floored at zero: a collection inside the
// window can leave the peak below the baseline, and a negative "growth" is
// noise, not a saving.
func growth(peak, baseline uint64) uint64 {
	if peak <= baseline {
		return 0
	}
	return peak - baseline
}
