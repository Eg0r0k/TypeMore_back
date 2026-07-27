package auth

import (
	"net/http"
	"sync"
	"time"

	"github.com/typemore/typemore-server/internal/platform/httpx"
)

// InMemoryRateLimiter is a per-key token-bucket RateLimiter. It is process-local
// — correct under the single-binary assumption of this phase. When the service
// is scaled horizontally, replace it with a shared (e.g. Redis) implementation
// of the RateLimiter interface; nothing else changes.
type InMemoryRateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	every   time.Duration // time to refill one token
	burst   int           // bucket capacity
	now     func() time.Time
	// lastSweep / sweepEvery drive lazy eviction of idle buckets so the map does
	// not grow without bound.
	lastSweep  time.Time
	sweepEvery time.Duration
}

type bucket struct {
	tokens float64
	last   time.Time
}

// NewInMemoryRateLimiter builds a limiter that refills one token every `every`
// with a bucket size of `burst`. A non-positive burst disables limiting
// (Allow always true) — handy for tests.
func NewInMemoryRateLimiter(every time.Duration, burst int) *InMemoryRateLimiter {
	return &InMemoryRateLimiter{
		buckets:    make(map[string]*bucket),
		every:      every,
		burst:      burst,
		now:        time.Now,
		sweepEvery: 5 * time.Minute,
	}
}

// Allow reports whether the action for key may proceed, consuming a token if so.
func (l *InMemoryRateLimiter) Allow(key string) bool {
	if l.burst <= 0 || l.every <= 0 {
		return true
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	l.sweep(now)

	b := l.buckets[key]
	if b == nil {
		b = &bucket{tokens: float64(l.burst), last: now}
		l.buckets[key] = b
	}

	// Refill proportionally to elapsed time, capped at burst.
	b.tokens = min(float64(l.burst), b.tokens+now.Sub(b.last).Seconds()/l.every.Seconds())
	b.last = now

	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

// sweep drops buckets that have been idle long enough to have fully refilled;
// they are indistinguishable from a fresh bucket, so forgetting them is free.
// Runs at most once per sweepEvery. Caller holds the mutex.
func (l *InMemoryRateLimiter) sweep(now time.Time) {
	if now.Sub(l.lastSweep) < l.sweepEvery {
		return
	}
	l.lastSweep = now
	idleForFull := l.every * time.Duration(l.burst)
	for key, b := range l.buckets {
		if now.Sub(b.last) > idleForFull {
			delete(l.buckets, key)
		}
	}
}

// rateLimit is middleware applying the limiter keyed by client IP, returning 429
// when the bucket is empty.
func (s *Service) rateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.limiter.Allow(httpx.ClientIP(r)) {
			s.writeError(w, r, apiErrRateLimited)
			return
		}
		next.ServeHTTP(w, r)
	})
}
