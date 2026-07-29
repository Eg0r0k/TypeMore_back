package ws_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/typemore/typemore-server/internal/protocol"
	"github.com/typemore/typemore-server/internal/ws"
)

// The match capture was written by a bare `go r.persist(snap)` on a background
// context: nothing owned the goroutine and nothing could wait for it, so a
// process that shut down while a match was ending simply lost the capture. The
// write is still off the room lock and still on its own context — a shutdown
// must not CANCEL it, because a match ending during shutdown is exactly the
// capture worth keeping — but the registry now owns it and can be waited on.

// slowStore is a MatchStore that takes its time, so "the write is still in
// flight" is a state a test can be in rather than a race it has to hope for.
type slowStore struct {
	delay time.Duration
	mu    sync.Mutex
	saved []ws.MatchRecord
	// entered closes when SaveMatch has actually begun, so a test can wait for
	// the goroutine to exist instead of guessing at scheduling.
	enteredOnce sync.Once
	entered     chan struct{}
}

func newSlowStore(delay time.Duration) *slowStore {
	return &slowStore{delay: delay, entered: make(chan struct{})}
}

func (s *slowStore) SaveMatch(_ context.Context, m ws.MatchRecord) error {
	s.enteredOnce.Do(func() { close(s.entered) })
	time.Sleep(s.delay)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saved = append(s.saved, m)
	return nil
}

func (s *slowStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.saved)
}

// persistServer is relayServer with the handler handed back, because the thing
// under test is a method on it.
func persistServer(t *testing.T, store ws.MatchStore) (*httptest.Server, *ws.Handler) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := ws.NewHandler(logger, nil, func(req *http.Request) (string, string, bool) {
		if name := req.Header.Get("X-Test-User"); name != "" {
			return name, req.Header.Get("X-Test-Uid"), true
		}
		return "", "", false
	}, store)
	r := chi.NewRouter()
	r.Handle("/ws", h)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv, h
}

// TestShutdownWaitsForTheMatchCapture ends a match against a store that has not
// finished writing, then waits. The assertion afterwards uses no Eventually and
// no polling: if the wait did not wait, the record is not there yet.
func TestShutdownWaitsForTheMatchCapture(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	store := newSlowStore(600 * time.Millisecond)
	srv, h := persistServer(t, store)

	m := startMatch(t, ctx, srv, 2, 30_000)
	for _, c := range m.conns {
		writeJSON(t, ctx, c, protocol.Finish{Type: protocol.TypeFinish, MatchID: m.matchID})
	}

	// The write has started and is deliberately still running.
	select {
	case <-store.entered:
	case <-ctx.Done():
		t.Fatal("the match capture never started")
	}
	require.Zero(t, store.count(), "the store finished early; the delay is not doing its job")

	waitCtx, waitCancel := context.WithTimeout(ctx, 10*time.Second)
	defer waitCancel()
	assert.True(t, h.WaitForPersists(waitCtx), "the capture did not finish inside the shutdown window")

	// No Eventually: the wait is the synchronisation.
	assert.Equal(t, 1, store.count(), "shutdown returned before the match capture was written")
}

// The other arm: a deadline that arrives first is reported, not swallowed. An
// operator whose shutdown window is too short for a large capture should see a
// line about it rather than a silently missing match.
func TestShutdownReportsCapturesStillInFlight(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	store := newSlowStore(3 * time.Second)
	srv, h := persistServer(t, store)

	m := startMatch(t, ctx, srv, 2, 30_000)
	for _, c := range m.conns {
		writeJSON(t, ctx, c, protocol.Finish{Type: protocol.TypeFinish, MatchID: m.matchID})
	}
	select {
	case <-store.entered:
	case <-ctx.Done():
		t.Fatal("the match capture never started")
	}

	waitCtx, waitCancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer waitCancel()
	assert.False(t, h.WaitForPersists(waitCtx),
		"a capture still in flight at the deadline must be reported, not reported as done")
}

// With nothing in flight the wait is immediate — shutdown does not pay for a
// feature nobody used.
func TestWaitForPersistsIsImmediateWhenIdle(t *testing.T) {
	_, h := persistServer(t, newSlowStore(time.Second))

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	assert.True(t, h.WaitForPersists(ctx))
	assert.Less(t, time.Since(start), 500*time.Millisecond)
}
