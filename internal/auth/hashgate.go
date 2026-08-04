package auth

import (
	"context"
	"sync/atomic"
	"time"
)

// HashCostBytes is what ONE argon2id hash holds live while it runs: the
// memory-cost parameter, which the implementation allocates as a single block.
// It is the unit every capacity calculation here is expressed in.
const HashCostBytes = argonMemoryKiB * 1024 // 19 MiB

// hashGate bounds how many password hashes may run at once.
//
// # The problem it solves
//
// argon2id is expensive ON PURPOSE — 19 MiB and ~50 ms per hash is what makes an
// offline attack on a leaked table unaffordable. But the server pays that cost
// on the REQUEST path, before it knows anything about the caller, and it pays it
// on every login whether the email exists or not (the decoy verify that keeps
// the two timing-indistinguishable). So 200 concurrent login attempts is ~3.8 GiB
// of live heap, from unauthenticated requests, with no credential required.
//
// The per-IP rate limiter does not help. It is per IP, so a botnet spread across
// a thousand addresses never trips it; and the memory is committed by the hash
// itself, which the limiter has already let through.
//
// # What it does instead
//
// A counting semaphore sized from a memory budget: at most `limit` hashes are in
// flight, so peak hashing memory is bounded at `limit × 19 MiB` no matter how
// many requests arrive. A request that cannot get a slot waits briefly — bursts
// are normal and a queue absorbs them — and is then rejected with 503 rather
// than being admitted into memory the process does not have.
//
// Shedding load is not a compromise here, it is the correct behaviour: an
// overloaded server that answers "try again in a moment" is up, and one that
// OOMs takes every established session down with it.
//
// # What it deliberately does NOT do
//
// It does not make rejection depend on anything about the request, so it opens
// no oracle: a saturated server rejects a valid login and an invalid one
// identically, at the same point in the flow, before either touches a hash.
type hashGate struct {
	slots chan struct{}
	// wait is how long a request may queue for a slot before being shed.
	wait time.Duration

	// Counters for observability and for the load suite's assertions.
	admitted atomic.Int64
	shed     atomic.Int64
	peak     atomic.Int64
	inFlight atomic.Int64
}

// newHashGate builds a gate admitting at most limit concurrent hashes, each
// waiting up to wait for a slot. A non-positive limit disables gating entirely
// (used by tests that want the old unbounded behaviour to measure it).
func newHashGate(limit int, wait time.Duration) *hashGate {
	if limit <= 0 {
		return &hashGate{}
	}
	if wait < 0 {
		wait = 0
	}
	return &hashGate{slots: make(chan struct{}, limit), wait: wait}
}

// acquire takes a slot, returning a release func. ok is false when the gate is
// saturated and the wait elapsed — the caller must then shed the request and
// must NOT hash.
func (g *hashGate) acquire(ctx context.Context) (release func(), ok bool) {
	if g == nil || g.slots == nil {
		g.count()
		return func() { g.done() }, true
	}

	// Fast path: a free slot, no timer allocation.
	select {
	case g.slots <- struct{}{}:
		g.count()
		return g.releaser(), true
	default:
	}

	timer := time.NewTimer(g.wait)
	defer timer.Stop()
	select {
	case g.slots <- struct{}{}:
		g.count()
		return g.releaser(), true
	case <-ctx.Done():
		g.shed.Add(1)
		return nil, false
	case <-timer.C:
		g.shed.Add(1)
		return nil, false
	}
}

// releaser gives back one slot, ORDER MATTERS: the in-flight counter is
// decremented BEFORE the semaphore slot is handed on.
//
// The other order overcounts. Freeing the slot first lets a queued caller take
// it and increment in-flight while this one has not yet decremented, so the
// counter — and with it `PeakInFlight` — reads one higher than the number of
// hashes actually running, and can exceed the limit outright. The ceiling
// itself was never violated (the channel is the ceiling), but the statistic
// that reports it was, and `PeakInFlight × HashCostBytes` is documented as the
// peak hashing memory this process has committed. A number used to size a
// deployment must not be able to report memory that was never held.
func (g *hashGate) releaser() func() {
	return func() {
		g.done()
		<-g.slots
	}
}

func (g *hashGate) count() {
	g.admitted.Add(1)
	n := g.inFlight.Add(1)
	for {
		peak := g.peak.Load()
		if n <= peak || g.peak.CompareAndSwap(peak, n) {
			return
		}
	}
}

func (g *hashGate) done() { g.inFlight.Add(-1) }

// HashGateStats is a snapshot of the gate, exported for the load suite and for
// whatever admin surface eventually wants it.
type HashGateStats struct {
	// Limit is the configured ceiling (0 = unbounded).
	Limit int
	// Admitted / Shed count hashes let through and requests rejected.
	Admitted int64
	Shed     int64
	// PeakInFlight is the highest concurrency observed. Multiplied by
	// HashCostBytes it is the peak hashing memory this process has committed.
	PeakInFlight int64
}

func (g *hashGate) stats() HashGateStats {
	return HashGateStats{
		Limit:        cap(g.slots),
		Admitted:     g.admitted.Load(),
		Shed:         g.shed.Load(),
		PeakInFlight: g.peak.Load(),
	}
}

// HashGateStats returns the current hashing-gate counters.
func (s *Service) HashGateStats() HashGateStats { return s.hashes.stats() }

// HashConcurrencyFor sizes the gate from a memory budget: how many 19 MiB hashes
// fit in budgetBytes, clamped to [minHashConcurrency, maxHashConcurrency].
//
// The clamps matter in both directions. A tiny container must still be able to
// serve one login at a time rather than rejecting everything; and a very large
// host should not conclude it may run 800 hashes at once, because at that point
// the bottleneck stopped being memory and became the CPU those hashes all want.
func HashConcurrencyFor(budgetBytes uint64) int {
	n := int(budgetBytes / HashCostBytes)
	if n < minHashConcurrency {
		return minHashConcurrency
	}
	if n > maxHashConcurrency {
		return maxHashConcurrency
	}
	return n
}

const (
	// minHashConcurrency keeps a memory-starved deployment serving logins,
	// slowly, instead of failing them all.
	minHashConcurrency = 2
	// maxHashConcurrency is where more parallel hashing stops buying throughput:
	// argon2id with p=1 is CPU-bound per hash, so far past core count the queue
	// is just memory held hostage.
	maxHashConcurrency = 64
	// DefaultHashWait is how long a request queues for a slot before being shed.
	// One hash is ~50 ms, so this is roughly "wait for a few ahead of you" —
	// long enough to absorb a burst, short enough that a saturated server says
	// so instead of leaving clients hanging.
	DefaultHashWait = 500 * time.Millisecond
)
