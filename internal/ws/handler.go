// Package ws implements the TypeMore WebSocket transport.
//
// # Concurrency shape (read this first)
//
// Every accepted connection is served by exactly three goroutines, and the
// ownership boundaries between them are the load-bearing design of this package
// — the relay phase grows on this exact template:
//
//   - the HTTP handler goroutine (serve) owns the connection lifecycle: it
//     accepts, wires up the other two, and performs the final close;
//   - one writer goroutine is the SOLE caller of Conn.Write — coder/websocket
//     forbids concurrent writes, so ALL outbound frames funnel through a
//     channel to this goroutine;
//   - one reader goroutine (the read loop, run on the serve goroutine itself) is
//     the SOLE caller of Conn.Read and decodes/dispatches inbound frames.
//
// Anything that wants to send a frame calls session.send, which marshals and
// hands the bytes to the writer over a channel. No other goroutine ever touches
// the socket directly. This makes the data races structurally impossible rather
// than merely avoided by convention.
package ws

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// outboundBuffer is the depth of the per-connection send queue. It is sized to
// absorb relay bursts (small opaque event batches) without dropping frames for a
// briefly-behind client; a client that stays behind long enough to fill it is
// treated as a slow consumer (its frames are dropped and the heartbeat will
// eventually disconnect it).
const outboundBuffer = 256

// readLimit is the hard transport cap on a single inbound frame. It sits above
// the 1 MiB event_batch cap so the application can soft-reject an oversized
// event_batch (bad_message, connection stays) instead of the transport closing
// the connection; a frame past this hard limit is a protocol abuse and closes.
const readLimit = 2 << 20 // 2 MiB

// eventBatchMaxBytes is the per-frame ceiling for event_batch (docs/PROTOCOL.md
// §6). An over-cap batch is dropped with bad_message; the connection stays open.
const eventBatchMaxBytes = 1 << 20 // 1 MiB

// Heartbeat: ping after heartbeatInterval of no successful ping, and treat the
// peer as dead after heartbeatMissed consecutive failures (§2).
const (
	heartbeatInterval = 15 * time.Second
	heartbeatMissed   = 2
)

// Handler is the http.Handler for the WebSocket endpoint. It is safe for
// concurrent use: it holds only immutable configuration and spawns fresh
// per-connection state on every request.
type Handler struct {
	log *slog.Logger
	// originPatterns is the WebSocket Origin allow-list. Empty means "accept any
	// origin" (see NewHandler).
	originPatterns []string
	// idGen mints player ids; a field so tests can inject a deterministic one.
	idGen func() string
	// reg is the shared room registry; every connection routes room messages
	// through it.
	reg *Registry
	// identify resolves a connection's authenticated identity from the upgrade
	// request (session cookie). It may be nil (all connections are guests) and,
	// when set, returns the account displayName, its user id, and true.
	identify func(*http.Request) (name, userID string, authed bool)
}

// NewHandler builds a Handler. allowedOrigins is the browser Origin allow-list;
// when empty, all origins are accepted (convenient for local development, but
// callers SHOULD populate it in production to prevent cross-site WebSocket
// hijacking). identify resolves the authenticated identity of an upgrade request
// (session cookie); pass nil to treat every connection as a guest. store (may be
// nil) receives each finished match's authoritative capture.
func NewHandler(log *slog.Logger, allowedOrigins []string, identify func(*http.Request) (name, userID string, authed bool), store MatchStore, opts ...Option) *Handler {
	reg := NewRegistry(log, store)
	for _, o := range opts {
		o(&reg.timing)
	}
	return &Handler{
		log:            log,
		originPatterns: allowedOrigins,
		idGen:          newID,
		reg:            reg,
		identify:       identify,
	}
}

// WaitForPersists blocks until every in-flight match-capture write has finished
// or ctx is done, reporting whether they all completed. Call it during shutdown,
// after the HTTP server has stopped accepting connections and before the
// database pool closes: a match that ends as the process is going down is
// exactly the capture worth keeping.
func (h *Handler) WaitForPersists(ctx context.Context) bool {
	return h.reg.WaitForPersists(ctx)
}

// Option tunes a Handler's registry. The match-timing options exist mainly so
// tests can drive the deadline and reconnect-grace paths without real-time waits.
type Option func(*matchTiming)

// WithGrace sets the seat-reconnect grace window applied on any connection drop.
func WithGrace(d time.Duration) Option {
	return func(t *matchTiming) { t.grace = d }
}

// WithDeadlineSlack sets the milliseconds of slack past a match's nominal length
// before unfinished seats are force-dnf'd at the hard deadline.
func WithDeadlineSlack(ms int64) Option {
	return func(t *matchTiming) { t.deadlineSlackMs = ms }
}

// WithAfkKick tunes the AFK dnf rules: `share` is the idle fraction of a racing
// seat's elapsed match window that dnf's it in words mode (production: 0.6),
// `warmupMs` the age below which that window is too young to judge (10 s), and
// `trailingMs` the run of continuous silence that dnf's a seat in ANY mode
// (15 s). Tests shrink them to drive the rules without real-time waits.
func WithAfkKick(share float64, warmupMs, trailingMs int64) Option {
	return func(t *matchTiming) {
		t.afkKickShare = share
		t.afkWarmupMs = warmupMs
		t.afkTrailingMs = trailingMs
	}
}

// WithFinishWindow sets the words-mode window opened by the first finish; at
// close every still-racing seat is dnf'd and the match ends (production: 120 s).
func WithFinishWindow(d time.Duration) Option {
	return func(t *matchTiming) { t.finishWindow = d }
}

// ServeHTTP upgrades the request to a WebSocket connection and serves the
// session. The upgrade response is written by websocket.Accept; from here on the
// handler speaks only frames.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	opts := &websocket.AcceptOptions{}
	if len(h.originPatterns) == 0 {
		// No allow-list configured: skip the Origin check entirely. Documented
		// as a development convenience in Config.AllowedOrigins.
		opts.InsecureSkipVerify = true
	} else {
		opts.OriginPatterns = h.originPatterns
	}

	conn, err := websocket.Accept(w, r, opts)
	if err != nil {
		// Accept has already written an HTTP error response; just log.
		h.log.Debug("websocket accept failed", "err", err)
		return
	}
	conn.SetReadLimit(readLimit)

	// Resolve the connection's identity from the upgrade request (session
	// cookie) before the frame loop begins. A guest connection has no name.
	var displayName, userID string
	var authed bool
	if h.identify != nil {
		displayName, userID, authed = h.identify(r)
	}

	h.serve(r.Context(), conn, displayName, userID, authed)
}

// serve owns one connection end to end. r.Context() is derived (via the
// server's BaseContext) from the process shutdown context, so a server shutdown
// cancels ctx here and tears the connection down gracefully.
func (h *Handler) serve(ctx context.Context, conn *websocket.Conn, displayName, userID string, authed bool) {
	// A per-connection context so that either side finishing (read loop exit,
	// write failure, or server shutdown) cancels the other.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	s := &session{
		conn:        conn,
		log:         h.log,
		idGen:       h.idGen,
		reg:         h.reg,
		outbound:    make(chan []byte, outboundBuffer),
		authed:      authed,
		displayName: displayName,
		userID:      userID,
	}

	// Writer goroutine: the sole owner of conn.Write. It drains s.outbound and
	// exits when the channel is closed (orderly shutdown) or a write fails
	// (broken/slow peer), cancelling ctx on the way out so the read loop and any
	// blocked send unblock.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer cancel()
		s.writeLoop(ctx)
	}()

	// Heartbeat goroutine: pings the peer on an interval and cancels the
	// connection after heartbeatMissed consecutive failures (§2). Ping is safe
	// to call concurrently with the writer.
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.heartbeat(ctx, cancel)
	}()

	// Read loop runs here, on the serve goroutine: the sole owner of conn.Read.
	// It returns the close code/reason to use once teardown completes.
	reason := s.readLoop(ctx)

	// On teardown, hand the seat to the room: in any phase it enters the
	// reconnect grace window, and disconnect registers the resume token and arms
	// the expiry itself — in that order. Done before closing outbound so a
	// graced seat is never sent to on a closed queue.
	if s.room != nil {
		s.room.disconnect(s)
	}

	// Orderly teardown: closing outbound lets the writer flush any already-queued
	// frames (e.g. the error frame that precedes a version_mismatch close) and
	// then exit. Only after it has drained do we close the socket.
	close(s.outbound)
	wg.Wait()

	if err := conn.Close(reason.code, reason.reason); err != nil {
		h.log.Debug("websocket close", "err", err)
	}
}
