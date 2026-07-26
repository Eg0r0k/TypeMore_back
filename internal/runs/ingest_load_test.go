//go:build load

package runs_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/typemore/typemore-server/internal/perf"
)

// Zone 5 — POST /api/v1/runs at the wire cap.
//
// Everything here is measured through the real HTTP stack against a real
// Postgres, because the interesting costs (MaxBytesReader, the strict decoder,
// the seq scan, gzip, the INSERT) only exist together. The client runs in the
// same process as the server, so every heap figure is an UPPER bound on the
// server's own use — stated once here rather than repeated per test.

const zone5 = "5 ingest"

// ingestSamples is the sequential latency sample count. 25 puts p99 within one
// sample of the worst observation, which is the honest resolution: claiming a
// p99 off 20 samples is claiming the maximum.
const (
	ingestSamples = 25
	ingestWarmup  = 3
)

// The latency ceiling. POST /runs is the last hop of the finish screen: the
// client already shows its local preview, so this is a "saved" confirmation
// rather than a blocking render — but a player who navigates away before it
// lands loses the run, so it has to complete inside the second or so the finish
// animation buys. 150 ms at p50 keeps the confirmation inside one frame budget
// of human "instant"; 400 ms at p99 leaves room for one checkpoint or a cold
// index page underneath the INSERT.
var (
	ingestP50Budget = perf.Budget{
		Zone: zone5, Workload: "POST /runs at the 2 MiB cap, p50",
		Limit:     150 * time.Millisecond,
		Rationale: "finish-screen confirmation; the player must not out-navigate their own save",
	}
	ingestP99Budget = perf.Budget{
		Zone: zone5, Workload: "POST /runs at the 2 MiB cap, p99",
		Limit:     400 * time.Millisecond,
		Rationale: "one slow tail request may cost a checkpoint, not a lost run",
	}
)

// TestLoadIngestAtTheCap measures the accept path with the largest body the
// server will take: latency distribution and allocation cost per request.
func TestLoadIngestAtTheCap(t *testing.T) {
	body, events := capBody(t)

	// The fixture itself is a finding. It used to be that the documented event
	// cap and the documented body cap contradicted each other — 50 000 events
	// marshalled past 2 MiB, so the event cap was unreachable. They are now
	// ordered: the event cap is what a well-formed log runs into, and the body
	// cap sits above it. This records the remaining gap before measuring.
	atEventCap := perf.MustJSON(perf.MaxEventsPayload(loadSeed))
	perf.Report(t, zone5, fmt.Sprintf("documented event cap (%d events) marshals to", perf.MaxEvents),
		fmt.Sprintf("%s — %.2f× the %s body cap, so it IS submittable",
			perf.MiB(uint64(len(atEventCap))),
			float64(len(atEventCap))/float64(perf.MaxBodyBytes),
			perf.MiB(perf.MaxBodyBytes)))
	perf.Report(t, zone5, "largest submittable body",
		fmt.Sprintf("%d bytes (%s, %.1f%% of cap) carrying %d events",
			len(body), perf.MiB(uint64(len(body))),
			float64(len(body))/float64(perf.MaxBodyBytes)*100, events))
	rawLog := perf.MaxLegalPayload(loadSeed).Log
	stored := perf.Gzip(rawLog)
	perf.Report(t, zone5, "gzip of that log (BestSpeed, as stored)",
		fmt.Sprintf("%s of %s raw (%.1f%%)",
			perf.MiB(uint64(len(stored))), perf.MiB(uint64(len(rawLog))),
			float64(len(stored))/float64(len(rawLog))*100))

	h := newHarness(t, func(o *harnessOpts) { o.runsRateBurst = 10_000 })
	h.login("ingest-cap@example.com", "correct horse battery", "ingestcap")

	for range ingestWarmup {
		status, err := h.sendBody("/api/v1/runs", body)
		require.NoError(t, err)
		require.Equal(t, http.StatusAccepted, status)
	}

	samples := make([]time.Duration, 0, ingestSamples)
	for range ingestSamples {
		t0 := time.Now()
		status, err := h.sendBody("/api/v1/runs", body)
		took := time.Since(t0)
		require.NoError(t, err)
		require.Equal(t, http.StatusAccepted, status)
		samples = append(samples, took)
	}

	perf.Report(t, zone5, "POST /runs at the cap, single-threaded", perf.Summary(samples))
	ingestP50Budget.Assert(t, perf.Percentile(samples, 50))
	ingestP99Budget.Assert(t, perf.Percentile(samples, 99))

	// Where the time goes, measured rather than guessed. A body that fails the
	// seq rule at its last event walks the identical read → decode → unmarshal →
	// scan path and then returns 422 without gzipping or touching Postgres, so
	// the gap between the two medians IS the gzip and the INSERT.
	reject := bodyWithLog(t, brokenTailLog(t))
	rejected := make([]time.Duration, 0, ingestSamples)
	for range ingestSamples {
		t0 := time.Now()
		status, err := h.sendBody("/api/v1/runs", reject)
		took := time.Since(t0)
		require.NoError(t, err)
		require.Equal(t, http.StatusUnprocessableEntity, status)
		rejected = append(rejected, took)
	}
	pAccept := perf.Percentile(samples, 50)
	pValidate := perf.Percentile(rejected, 50)
	perf.Report(t, zone5, "same body rejected 422 after validation (no gzip, no INSERT)",
		perf.Summary(rejected))
	perf.Report(t, zone5, "cost split at the cap",
		fmt.Sprintf("read+decode+validate %v of %v median (%.0f%%); gzip+INSERT %v (%.0f%%)",
			pValidate, pAccept, float64(pValidate)/float64(pAccept)*100,
			pAccept-pValidate, float64(pAccept-pValidate)/float64(pAccept)*100))

	// Allocation cost of ONE request. The number worth reading is the multiplier
	// against the body: the path holds the decoder's buffered body, the log
	// re-captured as a RawMessage, the parsed seq envelope and the gzip output,
	// so a multiplier near 1 would mean the request never copied its own input.
	allocBytes, allocs := perf.Delta(func() {
		status, err := h.sendBody("/api/v1/runs", body)
		require.NoError(t, err)
		require.Equal(t, http.StatusAccepted, status)
	})
	perf.Report(t, zone5, "allocations per capped POST",
		fmt.Sprintf("%s in %d allocs = %.2f× the %s body",
			perf.MiB(allocBytes), allocs,
			float64(allocBytes)/float64(len(body)), perf.MiB(uint64(len(body)))))
}

// ingestConcurrency is the burst the memory budget is written against: twenty
// players finishing a run in the same second on one instance.
const ingestConcurrency = 20

// TestLoadIngestConcurrentPeakHeap is the memory question: 20 bodies at the cap
// in flight at once, each of which the server copies several times.
func TestLoadIngestConcurrentPeakHeap(t *testing.T) {
	body, _ := capBody(t)

	h := newHarness(t, func(o *harnessOpts) { o.runsRateBurst = 10_000 })
	h.login("ingest-burst@example.com", "correct horse battery", "ingestburst")

	// One warm request so the first-call costs (gzip tables, statement
	// preparation) are not attributed to the burst, and a warm pool so the
	// burst's INSERTs do not pay for a cold dial.
	status, err := h.sendBody("/api/v1/runs", body)
	require.NoError(t, err)
	require.Equal(t, http.StatusAccepted, status)
	h.warmPool()

	res := h.burst(ingestConcurrency, h.wire(http.MethodPost, "/api/v1/runs", body))
	require.NoError(t, res.err)
	require.Equal(t, map[int]int{http.StatusAccepted: ingestConcurrency}, res.statuses)
	require.Positive(t, res.samples, "the sampler must have run inside the window")

	// Achieved concurrency, not requested: a peak taken while only a handful of
	// requests were really in flight would describe a smaller burst than the
	// budget claims.
	perf.Report(t, zone5, fmt.Sprintf("%d capped POSTs, achieved concurrency", ingestConcurrency),
		fmt.Sprintf("%d in flight at peak; heads delivered in %v, window %v",
			res.overlap, res.delivered.Round(time.Millisecond), res.elapsed.Round(time.Millisecond)))
	require.GreaterOrEqual(t, res.overlap, ingestConcurrency,
		"the driver failed to deliver the burst; the memory figure would be for a smaller one")

	perf.Report(t, zone5, fmt.Sprintf("%d concurrent capped POSTs, latency", ingestConcurrency),
		perf.Summary(res.latency))
	perf.Report(t, zone5, "baseline heap before the burst", perf.MiB(res.baseline))
	perf.Report(t, zone5, "peak process memory (Sys) during the burst", perf.MiB(res.peakSys))
	perf.Report(t, zone5, "heap growth attributable to the burst",
		fmt.Sprintf("%s = %.1f× the %s of raw bodies in flight",
			perf.MiB(growth(res.peakHeap, res.baseline)),
			float64(growth(res.peakHeap, res.baseline))/float64(ingestConcurrency*len(body)),
			perf.MiB(uint64(ingestConcurrency*len(body)))))

	// 20 × 2 MiB of raw body is 40 MiB before the server touches it. The path
	// keeps roughly three shapes of each body alive at once (buffered body,
	// re-captured log, parsed envelope) plus the gzip output, so ~6 MiB per
	// request is the expected cost and 192 MiB is that with a 60% margin. The
	// ceiling matters because this server shares a container with argon2
	// (19 MiB per in-flight hash) and the replay worker: a burst of finishers
	// must not be able to starve a login.
	perf.AssertBytes(t, zone5,
		fmt.Sprintf("peak heap, %d concurrent capped POSTs", ingestConcurrency),
		res.peakHeap, 192<<20,
		"20 finishers at once must fit beside argon2 and the replay worker in a 512 MiB instance")
}

// TestLoadIngestOversizedBodyRejectedEarly checks that the cap is a STREAMING
// cap, not a post-hoc length check: an oversized body must be refused without
// the server ever having held it.
//
// The 8 MiB probe the brief asks for is one rung of a ladder, because a single
// absolute number cannot tell "buffered the whole body" from "buffered up to the
// cap and left the doublings uncollected". The ladder can: if the cost tracks
// what was SENT, the cap is not protecting anything; if it plateaus, the cap
// binds and the residue is garbage the collector has not reached yet.
func TestLoadIngestOversizedBodyRejectedEarly(t *testing.T) {
	h := newHarness(t, func(o *harnessOpts) { o.runsRateBurst = 10_000 })
	h.login("ingest-413@example.com", "correct horse battery", "ingest413")

	type rung struct {
		size    int
		sent    int
		peak    uint64
		total   uint64
		nallocs uint64
		samples int
	}
	rungs := []rung{{size: 4 << 20}, {size: 8 << 20}, {size: 16 << 20}}

	for i := range rungs {
		body := oversizeBody(t, rungs[i].size)
		rungs[i].sent = len(body)

		// The probe body is allocated and live BEFORE the baseline is taken, so
		// what is measured below is the server's cost, not the fixture's.
		var status int
		var err error
		baseline := liveHeap()
		sampler := perf.NewPeakSampler(time.Millisecond)
		rungs[i].total, rungs[i].nallocs = perf.Delta(func() {
			status, err = h.sendBody("/api/v1/runs", body)
		})
		peakHeap, _ := sampler.Stop()
		rungs[i].peak = growth(peakHeap, baseline)
		rungs[i].samples = sampler.Samples()

		require.NoError(t, err, "an oversized body must be answered, not reset")
		assert.Equal(t, http.StatusRequestEntityTooLarge, status,
			"a %s body must be rejected 413", perf.MiB(uint64(len(body))))
		require.Positive(t, rungs[i].samples, "the sampler must have run inside the window")

		perf.Report(t, zone5, fmt.Sprintf("rejecting a %s body", perf.MiB(uint64(len(body)))),
			fmt.Sprintf("peak heap %s over a %s baseline = growth %s (%.2f× sent, %.2f× the cap); allocated %s in %d allocs over %d samples",
				perf.MiB(peakHeap), perf.MiB(baseline), perf.MiB(rungs[i].peak),
				float64(rungs[i].peak)/float64(len(body)),
				float64(rungs[i].peak)/float64(perf.MaxBodyBytes),
				perf.MiB(rungs[i].total), rungs[i].nallocs, rungs[i].samples))
	}

	// Sampled peaks are the weaker instrument here and the report has to say so:
	// Windows' timer granularity is ~15 ms, so a window this short yields a
	// handful of observations and a buffer that lives for one of them can be
	// missed entirely. The allocation ladder below is the load-bearing evidence.
	perf.Report(t, zone5, "peak-sampler resolution over the 413 window",
		fmt.Sprintf("%d–%d samples per request; treat sampled peaks as a floor",
			min(rungs[0].samples, rungs[1].samples, rungs[2].samples),
			max(rungs[0].samples, rungs[1].samples, rungs[2].samples)))

	// The brief's rung, asserted on its own terms: json.Decoder buffers the value
	// it is scanning, so the server legitimately holds up to the cap plus scratch
	// before MaxBytesReader trips. Twice the cap is the line between "stopped at
	// the limit" and "read the whole thing"; anything near the body size is the
	// amplification an attacker would pay nothing for.
	perf.AssertBytes(t, zone5, "heap growth rejecting an 8 MiB body",
		rungs[1].peak, 2*perf.MaxBodyBytes,
		"the size cap must bound the buffer, not just the accepted length")

	// The same question asked of allocation, which no sampling interval can miss.
	// Eight times the cap is the ceiling because json.Decoder reaches its buffer
	// size by doubling — holding 2 MiB costs roughly 8 MiB of superseded buffers
	// — and that constant is the whole point: it is a function of the CAP, so an
	// attacker cannot buy more of it by sending more.
	perf.AssertBytes(t, zone5, "bytes allocated rejecting an 8 MiB body",
		rungs[1].total, 8*perf.MaxBodyBytes,
		"rejection must cost a constant multiple of the cap, never a function of the body")

	// The ladder's own verdict, and the sharper one: quadrupling the body must
	// not multiply the server's cost. A 25% allowance covers the read chunk that
	// straddles the limit; a linear response would show up as ~4×.
	small, large := rungs[0], rungs[len(rungs)-1]
	ratio := float64(large.total) / float64(small.total)
	perf.Report(t, zone5, "cost scaling from 4 MiB to 16 MiB sent",
		fmt.Sprintf("allocated %s → %s = %.2f× for %.1f× the body",
			perf.MiB(small.total), perf.MiB(large.total), ratio,
			float64(large.sent)/float64(small.sent)))
	assert.LessOrEqual(t, ratio, 1.25,
		"rejection cost must be bounded by the cap, not by what the client sent")

	// A 413 must leave a usable client. MaxBytesReader marks the connection for
	// close, so this also proves the transport recovers rather than wedging.
	status, err := h.sendBody("/api/v1/runs", mustCapBodyBytes(t))
	require.NoError(t, err, "the connection must survive a 413")
	assert.Equal(t, http.StatusAccepted, status, "a legal body must still be accepted after a 413")
}

// mustCapBodyBytes is capBody's body without the reporting, for the follow-up
// request above.
func mustCapBodyBytes(t *testing.T) []byte {
	t.Helper()
	return perf.MustJSON(perf.MaxLegalPayload(loadSeed))
}

// seqScanSamples is per variant; the two variants differ by a single event's
// seq, so the difference between their medians is the linear scan.
const seqScanSamples = 15

// TestLoadIngestSeqScanWorstCase isolates the strictly-increasing seq scan.
//
// validateLog is unexported, so this is the DIFFERENTIAL method the brief calls
// for: two bodies identical except for which event breaks monotonicity. Both
// are fully decoded and fully unmarshalled into the seq envelope before the scan
// starts, and both are rejected 422 before gzip or the INSERT, so the only work
// that differs is how many events the loop visits — one, versus all of them.
func TestLoadIngestSeqScanWorstCase(t *testing.T) {
	log := perf.GenerateLog(perf.LogSpec{Events: perf.SubmittableEvents(loadSeed), Seed: loadSeed})
	n := len(log.Events)

	// Fails at the first event: prev starts below zero, so a negative seq is
	// rejected on iteration one.
	first := log
	first.Events = append([]perf.Event(nil), log.Events...)
	first.Events[0].Seq = -1

	last := brokenTailLog(t)

	firstBody := bodyWithLog(t, first)
	lastBody := bodyWithLog(t, last)
	perf.Report(t, zone5, "seq-scan probe bodies",
		fmt.Sprintf("%d events; fail-first %d bytes, fail-last %d bytes",
			n, len(firstBody), len(lastBody)))

	h := newHarness(t, func(o *harnessOpts) { o.runsRateBurst = 10_000 })
	h.login("ingest-seq@example.com", "correct horse battery", "ingestseq")

	measure := func(body []byte) []time.Duration {
		status, err := h.sendBody("/api/v1/runs", body)
		require.NoError(t, err)
		require.Equal(t, http.StatusUnprocessableEntity, status,
			"the probe must be rejected by the seq rule, not stored")
		out := make([]time.Duration, 0, seqScanSamples)
		for range seqScanSamples {
			t0 := time.Now()
			status, err := h.sendBody("/api/v1/runs", body)
			took := time.Since(t0)
			require.NoError(t, err)
			require.Equal(t, http.StatusUnprocessableEntity, status)
			out = append(out, took)
		}
		return out
	}

	// Interleaved would be better still, but the two variants are measured
	// back-to-back on the same warm process, which is enough for a difference
	// this test expects to be small.
	firstSamples := measure(firstBody)
	lastSamples := measure(lastBody)

	pFirst := perf.Percentile(firstSamples, 50)
	pLast := perf.Percentile(lastSamples, 50)
	perf.Report(t, zone5, "422 fail at event 1", perf.Summary(firstSamples))
	perf.Report(t, zone5, fmt.Sprintf("422 fail at event %d", n), perf.Summary(lastSamples))
	perf.Report(t, zone5, "median difference = the whole seq scan",
		fmt.Sprintf("%v over %d events (%.1f ns/event)", pLast-pFirst, n,
			float64(pLast-pFirst)/float64(n)))
}

// bodyWithLog re-renders the capped payload around a hand-modified log.
func bodyWithLog(t *testing.T, log perf.EventLog) []byte {
	t.Helper()
	payload := perf.MaxLegalPayload(loadSeed)
	payload.Log = json.RawMessage(perf.MustJSON(log))
	return perf.MustJSON(payload)
}

// brokenTailLog is the capped log with its LAST event's seq repeated: the
// cheapest way to break "strictly increasing" without changing the encoded
// width of any other event, so the body stays the same size as the accepted
// one and the whole scan still runs before the rejection.
func brokenTailLog(t *testing.T) perf.EventLog {
	t.Helper()
	log := perf.GenerateLog(perf.LogSpec{Events: perf.SubmittableEvents(loadSeed), Seed: loadSeed})
	log.Events = append([]perf.Event(nil), log.Events...)
	n := len(log.Events)
	log.Events[n-1].Seq = log.Events[n-2].Seq
	return log
}

// BenchmarkIngestSeqScan is the in-process counterpart to the HTTP differential
// above: exact replicas of the shapes internal/runs decodes into (the real ones
// are unexported), each stage of the validate path benchmarked on its own.
//
// The split is the point. Ingestion parses the SAME 2 MiB twice — once by the
// outer json.Decoder to find the log's byte bounds, once by validateLog to read
// every event's seq — and the strictly-increasing scan everyone worries about is
// the last and smallest of those steps. Only a stage-by-stage benchmark can say
// so, because over HTTP the whole thing is one number.
func BenchmarkIngestSeqScan(b *testing.B) {
	events := perf.SubmittableEvents(loadSeed)
	payload := perf.MaxLegalPayload(loadSeed)
	body := perf.MustJSON(payload)
	raw := []byte(payload.Log)

	// The shape the handler decodes the BODY into: opaque snapshots captured as
	// RawMessage so the log's bytes survive to gzip untouched.
	type request struct {
		Mode          string          `json:"mode"`
		DurationMs    *int32          `json:"durationMs"`
		WordCount     *int32          `json:"wordCount"`
		Lang          string          `json:"lang"`
		Seed          *int64          `json:"seed"`
		DictHash      string          `json:"dictHash"`
		Setup         json.RawMessage `json:"setup"`
		Log           json.RawMessage `json:"log"`
		ClientMetrics json.RawMessage `json:"clientMetrics"`
		ClientScore   json.RawMessage `json:"clientScore"`
		ScoreVersion  *int            `json:"scoreVersion"`
	}

	// The shape validateLog decodes the LOG into: everything but version and seq
	// is discarded, and seq is a pointer so a missing field is distinguishable
	// from zero.
	type envelope struct {
		Version *int `json:"version"`
		Events  []struct {
			Seq *int64 `json:"seq"`
		} `json:"events"`
	}

	b.Run("body-decode", func(b *testing.B) {
		b.SetBytes(int64(len(body)))
		for b.Loop() {
			dec := json.NewDecoder(bytes.NewReader(body))
			dec.DisallowUnknownFields()
			var req request
			if err := dec.Decode(&req); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("log-unmarshal", func(b *testing.B) {
		b.SetBytes(int64(len(raw)))
		for b.Loop() {
			var env envelope
			if err := json.Unmarshal(raw, &env); err != nil {
				b.Fatal(err)
			}
		}
	})

	var env envelope
	require.NoError(b, json.Unmarshal(raw, &env))
	b.Run("seq-scan", func(b *testing.B) {
		for b.Loop() {
			prev := int64(-1)
			for i := range env.Events {
				seq := env.Events[i].Seq
				if seq == nil || *seq <= prev {
					b.Fatal("fixture is not strictly increasing")
				}
				prev = *seq
			}
		}
	})

	// The proposed alternative, measured rather than asserted: walk the events
	// array one element at a time into a reused struct. Same rule, same errors,
	// but no 40 000-element slice — which is where the unmarshal above spends
	// itself.
	b.Run("streamed-unmarshal-and-scan", func(b *testing.B) {
		b.SetBytes(int64(len(raw)))
		for b.Loop() {
			if n := streamSeqScan(raw); n != events {
				b.Fatalf("scanned %d events, want %d", n, events)
			}
		}
	})
}

// streamSeqScan is the streaming equivalent of validateLog's unmarshal-then-loop:
// it decodes the events array element by element and enforces the same
// strictly-increasing non-negative seq rule, returning how many events it saw
// (-1 on any violation). It exists only to price the proposal in the report.
func streamSeqScan(raw []byte) int {
	dec := json.NewDecoder(bytes.NewReader(raw))
	// Walk to the "events" key without materialising anything else.
	for {
		tok, err := dec.Token()
		if err != nil {
			return -1
		}
		if key, ok := tok.(string); ok && key == "events" {
			break
		}
	}
	if _, err := dec.Token(); err != nil { // opening '['
		return -1
	}
	var e struct {
		Seq *int64 `json:"seq"`
	}
	prev, n := int64(-1), 0
	for dec.More() {
		e.Seq = nil
		if err := dec.Decode(&e); err != nil {
			return -1
		}
		if e.Seq == nil || *e.Seq <= prev {
			return -1
		}
		prev = *e.Seq
		n++
	}
	return n
}
