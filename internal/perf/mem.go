package perf

import (
	"runtime"
	"sync"
	"time"
)

// PeakSampler watches process memory while something runs and reports the high
// water mark.
//
// It samples runtime.MemStats rather than OS RSS, deliberately: RSS on Windows,
// Linux and macOS mean three different things, and the number this project needs
// to bound is "how much did the Go heap have live at once" — argon2 allocates its
// 19 MiB block from the Go heap, so HeapAlloc is the honest measure of the
// exhaustion risk. The OS number is strictly larger (it includes the runtime,
// stacks, and memory the collector has not returned yet), so a heap budget that
// holds implies an RSS one at a known constant offset.
//
// Sampling can miss a spike shorter than the interval. Every zone here holds its
// peak for the duration of a hash or a request — orders of magnitude above the
// sample period — but a test that needs an exact figure should use Delta.
type PeakSampler struct {
	interval time.Duration
	stop     chan struct{}
	done     chan struct{}

	mu       sync.Mutex
	peakHeap uint64
	peakSys  uint64
	samples  int
}

// NewPeakSampler starts sampling immediately. Call Stop to finish.
func NewPeakSampler(interval time.Duration) *PeakSampler {
	if interval <= 0 {
		interval = time.Millisecond
	}
	p := &PeakSampler{
		interval: interval,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	go p.loop()
	return p
}

func (p *PeakSampler) loop() {
	defer close(p.done)
	t := time.NewTicker(p.interval)
	defer t.Stop()
	for {
		p.sample()
		select {
		case <-p.stop:
			p.sample()
			return
		case <-t.C:
		}
	}
}

func (p *PeakSampler) sample() {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.samples++
	if m.HeapAlloc > p.peakHeap {
		p.peakHeap = m.HeapAlloc
	}
	if m.Sys > p.peakSys {
		p.peakSys = m.Sys
	}
}

// Stop ends sampling and returns the peak live heap and the peak memory obtained
// from the OS.
func (p *PeakSampler) Stop() (peakHeap, peakSys uint64) {
	close(p.stop)
	<-p.done
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.peakHeap, p.peakSys
}

// Samples is how many observations were taken — a sanity check that the sampler
// actually ran during the window rather than starting and stopping around it.
func (p *PeakSampler) Samples() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.samples
}

// Delta measures the heap growth caused by fn, with the garbage collector run on
// both sides so the number is live data rather than accumulated garbage.
//
// Use it for "how much does ONE of these cost"; use PeakSampler for "how high did
// this concurrent workload get".
func Delta(fn func()) (allocBytes, allocs uint64) {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc, after.Mallocs - before.Mallocs
}
