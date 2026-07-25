package auth

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests are internal to the package because the gate is unexported: the
// load suite in zone 1 measures the gate through HTTP, but the invariant it
// relies on — "never more than limit hashes live at once" — has to be pinned
// here, where it can be checked without spending 19 MiB per observation.

// TestHashGateAdmitsAtMostLimit drives far more callers than slots and checks
// the ceiling with a counter of our own, not just the gate's bookkeeping: if
// the semaphore leaked a slot, the gate's own peak would be wrong in exactly
// the same way and would not catch it.
func TestHashGateAdmitsAtMostLimit(t *testing.T) {
	const (
		limit   = 4
		callers = 64
	)
	// Generous wait: this test asserts the ceiling, not the shedding, so every
	// caller must get through. 64 callers × 1 ms held / 4 slots ≈ 16 ms of
	// queueing against a 5 s wait.
	g := newHashGate(limit, 5*time.Second)

	var inFlight, observedPeak atomic.Int64
	var wg sync.WaitGroup
	wg.Add(callers)
	for range callers {
		go func() {
			defer wg.Done()
			release, ok := g.acquire(context.Background())
			if !ok {
				return
			}
			defer release()

			n := inFlight.Add(1)
			for {
				peak := observedPeak.Load()
				if n <= peak || observedPeak.CompareAndSwap(peak, n) {
					break
				}
			}
			time.Sleep(time.Millisecond)
			inFlight.Add(-1)
		}()
	}
	wg.Wait()

	stats := g.stats()
	assert.LessOrEqual(t, observedPeak.Load(), int64(limit),
		"more callers were inside the gate at once than it has slots")
	assert.LessOrEqual(t, stats.PeakInFlight, int64(limit))
	assert.Equal(t, int64(callers), stats.Admitted, "every caller should eventually be admitted")
	assert.Zero(t, stats.Shed, "nothing should be shed with a 5s wait")
	assert.Equal(t, limit, stats.Limit)
	assert.Zero(t, g.inFlight.Load(), "in-flight must return to zero")
}

// TestHashGateShedsWhenSaturated pins the property the 503 path depends on: a
// full gate answers, late but definitively, instead of blocking until the
// client gives up.
func TestHashGateShedsWhenSaturated(t *testing.T) {
	const wait = 40 * time.Millisecond
	g := newHashGate(1, wait)

	release, ok := g.acquire(context.Background())
	require.True(t, ok, "first caller takes the only slot")

	start := time.Now()
	release2, ok2 := g.acquire(context.Background())
	elapsed := time.Since(start)

	assert.False(t, ok2, "a saturated gate must shed")
	assert.Nil(t, release2, "a shed caller gets no release func to call")
	assert.GreaterOrEqual(t, elapsed, wait, "the caller should have queued for the full wait")
	assert.Less(t, elapsed, 2*time.Second, "shedding must not degenerate into blocking")
	assert.Equal(t, int64(1), g.stats().Shed)
	assert.Equal(t, int64(1), g.stats().Admitted, "a shed caller was never admitted")

	// The slot comes back, so the gate is not sticky.
	release()
	release3, ok3 := g.acquire(context.Background())
	require.True(t, ok3, "the freed slot must be usable again")
	release3()
	assert.Zero(t, g.inFlight.Load())
}

// TestHashGateDisabledWhenLimitNonPositive covers the ungated configuration the
// zone-1 load suite measures as its DoS baseline. It must admit everything, and
// it must still count, or the baseline run would report nothing.
func TestHashGateDisabledWhenLimitNonPositive(t *testing.T) {
	for _, limit := range []int{0, -1} {
		g := newHashGate(limit, time.Millisecond)
		require.Nil(t, g.slots, "a non-positive limit must not allocate a semaphore")

		const callers = 32
		var wg sync.WaitGroup
		wg.Add(callers)
		for range callers {
			go func() {
				defer wg.Done()
				release, ok := g.acquire(context.Background())
				assert.True(t, ok, "an ungated gate admits everyone")
				time.Sleep(time.Millisecond)
				release()
			}()
		}
		wg.Wait()

		stats := g.stats()
		assert.Equal(t, 0, stats.Limit, "an ungated gate reports no limit")
		assert.Equal(t, int64(callers), stats.Admitted)
		assert.Zero(t, stats.Shed)
		assert.Zero(t, g.inFlight.Load())
		// Unbounded concurrency is the whole point of this configuration; with
		// 32 goroutines it should have been observed at least once.
		assert.Greater(t, stats.PeakInFlight, int64(1),
			"ungated callers should have overlapped, otherwise the baseline measures nothing")
	}
}

// TestHashGateRespectsContextCancellation matters for request handling: a
// client that disconnects while queued must free the goroutine immediately
// rather than holding it for the remaining wait.
func TestHashGateRespectsContextCancellation(t *testing.T) {
	g := newHashGate(1, 10*time.Second)
	release, ok := g.acquire(context.Background())
	require.True(t, ok)
	defer release()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	release2, ok2 := g.acquire(ctx)
	elapsed := time.Since(start)

	assert.False(t, ok2)
	assert.Nil(t, release2)
	assert.Less(t, elapsed, time.Second, "a cancelled caller must not serve out the 10s wait")
	assert.Equal(t, int64(1), g.stats().Shed, "a cancelled caller is shed, not admitted")
}

// TestHashGateReleaseIsBalanced runs many sequential cycles through a
// single-slot gate: a release that forgot to decrement in-flight, or to return
// its semaphore token, shows up here as a hang or a non-zero residue.
func TestHashGateReleaseIsBalanced(t *testing.T) {
	const cycles = 200
	g := newHashGate(2, 100*time.Millisecond)
	for range cycles {
		release, ok := g.acquire(context.Background())
		require.True(t, ok)
		release()
	}
	stats := g.stats()
	assert.Equal(t, int64(cycles), stats.Admitted)
	assert.Zero(t, stats.Shed)
	assert.Equal(t, int64(1), stats.PeakInFlight, "sequential cycles never overlap")
	assert.Zero(t, g.inFlight.Load())
	assert.Empty(t, g.slots, "every token must have been returned to the semaphore")
}

// TestHashConcurrencyForClamps pins the sizing table the composition root uses.
// The clamps are the interesting part: they are what keeps a 256 MiB container
// serving logins and a 128 GiB host from admitting 6900 hashes.
func TestHashConcurrencyForClamps(t *testing.T) {
	const mib = 1 << 20
	cases := []struct {
		name   string
		budget uint64
		want   int
	}{
		{"no budget clamps up to the floor", 0, 2},
		{"one hash worth of budget still clamps up", HashCostBytes, 2},
		{"two hashes is the first exact fit", 2 * HashCostBytes, 2},
		{"the 512 MiB fallback of cmd/server", 512 * mib, 26},
		{"an absurd budget clamps down to the ceiling", 100 * 1024 * mib, 64},
		{"exactly the ceiling", 64 * HashCostBytes, 64},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, HashConcurrencyFor(tc.budget))
		})
	}

	// The result is a memory promise, so it must never exceed the budget it was
	// derived from once the floor is satisfied.
	for budget := uint64(2 * HashCostBytes); budget < 2048*mib; budget += 37 * mib {
		n := HashConcurrencyFor(budget)
		if n < maxHashConcurrency {
			assert.LessOrEqual(t, uint64(n)*HashCostBytes, budget,
				"gate of %d would peak above its %d byte budget", n, budget)
		}
	}
}
