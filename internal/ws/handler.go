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

	"github.com/coder/websocket"
)

// outboundBuffer is the depth of the per-connection send queue. It is small on
// purpose: a client we cannot keep up with is a slow-consumer problem we would
// rather surface (via a blocked send that ctx-cancellation unblocks) than paper
// over with unbounded buffering.
const outboundBuffer = 16

// readLimit caps a single inbound frame (bytes). The hello/NTP frames of this
// phase are tiny; the generous ceiling leaves headroom for the relay phase's
// event batches without allowing unbounded memory use per frame.
const readLimit = 1 << 20 // 1 MiB

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
}

// NewHandler builds a Handler. allowedOrigins is the browser Origin allow-list;
// when empty, all origins are accepted (convenient for local development, but
// callers SHOULD populate it in production to prevent cross-site WebSocket
// hijacking).
func NewHandler(log *slog.Logger, allowedOrigins []string) *Handler {
	return &Handler{
		log:            log,
		originPatterns: allowedOrigins,
		idGen:          newID,
	}
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

	h.serve(r.Context(), conn)
}

// serve owns one connection end to end. r.Context() is derived (via the
// server's BaseContext) from the process shutdown context, so a server shutdown
// cancels ctx here and tears the connection down gracefully.
func (h *Handler) serve(ctx context.Context, conn *websocket.Conn) {
	// A per-connection context so that either side finishing (read loop exit,
	// write failure, or server shutdown) cancels the other.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	s := &session{
		conn:     conn,
		log:      h.log,
		idGen:    h.idGen,
		outbound: make(chan []byte, outboundBuffer),
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

	// Read loop runs here, on the serve goroutine: the sole owner of conn.Read.
	// It returns the close code/reason to use once teardown completes.
	reason := s.readLoop(ctx)

	// Orderly teardown: closing outbound lets the writer flush any already-queued
	// frames (e.g. the error frame that precedes a version_mismatch close) and
	// then exit. Only after it has drained do we close the socket.
	close(s.outbound)
	wg.Wait()

	if err := conn.Close(reason.code, reason.reason); err != nil {
		h.log.Debug("websocket close", "err", err)
	}
}
