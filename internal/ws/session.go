package ws

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/coder/websocket"

	"github.com/typemore/typemore-server/internal/protocol"
)

// session holds the per-connection state and the three primitives the goroutines
// share: the socket, the outbound queue, and the (small) protocol state machine.
//
// Field ownership:
//   - conn.Read is touched only by readLoop; conn.Write only by writeLoop.
//   - outbound is written by senders (readLoop / future goroutines) via send and
//     read only by writeLoop.
//   - helloDone/playerID/nick are touched only by readLoop's dispatch, so they
//     need no synchronization; if a future phase mutates them from another
//     goroutine, that invariant must be revisited.
type session struct {
	conn     *websocket.Conn
	log      *slog.Logger
	idGen    func() string
	outbound chan []byte

	helloDone bool
	playerID  string
	nick      string
}

// closeReason bundles the WebSocket close code and human-readable reason used
// when the connection is torn down.
type closeReason struct {
	code   websocket.StatusCode
	reason string
}

// nowMs returns the current server wall-clock time in milliseconds since the
// Unix epoch — the unit both NTP timestamps and countdown times are expressed in.
func nowMs() int64 { return time.Now().UnixMilli() }

// writeLoop is the sole owner of conn.Write. It serializes every outbound frame:
// senders never touch the socket, they only enqueue bytes on s.outbound. The
// loop ends when the channel is closed (orderly shutdown) or a write fails.
func (s *session) writeLoop(ctx context.Context) {
	for b := range s.outbound {
		// Write is bounded by ctx: a server shutdown or dead peer aborts it
		// instead of blocking forever.
		if err := s.conn.Write(ctx, websocket.MessageText, b); err != nil {
			s.log.Debug("websocket write failed", "err", err)
			return
		}
	}
}

// readLoop is the sole owner of conn.Read. It decodes and dispatches inbound
// frames until the connection ends, then returns the close reason for teardown.
func (s *session) readLoop(ctx context.Context) closeReason {
	for {
		typ, data, err := s.conn.Read(ctx)
		if err != nil {
			// Any read error — clean client close, broken pipe, or ctx
			// cancellation on shutdown — ends the session. Report normal
			// closure; if the client already sent a close frame, our close is a
			// no-op on the wire.
			return closeReason{websocket.StatusNormalClosure, ""}
		}
		// recvMs is captured as early as possible so it is a faithful "server
		// receive" timestamp for NTP, before any decode/dispatch cost.
		recvMs := nowMs()

		if typ != websocket.MessageText {
			s.send(ctx, protocol.NewError(protocol.CodeBadMessage, "expected a JSON text frame"))
			continue
		}

		if reason, closing := s.dispatch(ctx, data, recvMs); closing {
			return reason
		}
	}
}

// dispatch routes a single decoded frame. It returns (reason, true) only when
// the frame requires closing the connection (version_mismatch); every other
// error is reported in-band and the connection stays open.
func (s *session) dispatch(ctx context.Context, data []byte, recvMs int64) (closeReason, bool) {
	var env protocol.Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		s.send(ctx, protocol.NewError(protocol.CodeBadMessage, "frame is not valid JSON"))
		return closeReason{}, false
	}

	// The protocol requires hello before anything else so the server knows the
	// client's version and identity up front.
	if !s.helloDone {
		if env.Type != protocol.TypeHello {
			s.send(ctx, protocol.NewError(protocol.CodeBadMessage, "hello must be the first message"))
			return closeReason{}, false
		}
		return s.handleHello(ctx, data)
	}

	switch env.Type {
	case protocol.TypeHello:
		// A second hello on an already-established session is a client bug.
		s.send(ctx, protocol.NewError(protocol.CodeBadMessage, "hello already completed"))
	case protocol.TypeNTPPing:
		s.handleNTPPing(ctx, data, recvMs)
	default:
		// Unknown or not-yet-implemented types (rooms/relay land in a later
		// phase): reject in-band without closing.
		s.send(ctx, protocol.NewError(protocol.CodeBadMessage, "unsupported message type: "+env.Type))
	}
	return closeReason{}, false
}

// handleHello validates the client's hello and, on success, assigns a player id
// and acknowledges. A version mismatch is fatal: the error is sent and the
// connection is closed (the server never translates between protocol versions).
func (s *session) handleHello(ctx context.Context, data []byte) (closeReason, bool) {
	var h protocol.Hello
	if err := json.Unmarshal(data, &h); err != nil {
		s.send(ctx, protocol.NewError(protocol.CodeBadMessage, "invalid hello payload"))
		return closeReason{}, false
	}

	if h.ProtocolVersion != protocol.Version {
		msg := fmt.Sprintf("server speaks protocol %d, client sent %d", protocol.Version, h.ProtocolVersion)
		s.send(ctx, protocol.NewError(protocol.CodeVersionMismatch, msg))
		// Close after the error frame is flushed (see serve's teardown order).
		return closeReason{websocket.StatusPolicyViolation, "protocol version mismatch"}, true
	}

	if !protocol.ValidNick(h.Nick) {
		s.send(ctx, protocol.NewError(protocol.CodeBadMessage, "nick must be 1-16 characters"))
		// Not fatal: the client may retry hello with a valid nick.
		return closeReason{}, false
	}

	s.helloDone = true
	s.playerID = s.idGen()
	s.nick = h.Nick
	s.send(ctx, protocol.NewHelloOK(s.playerID))
	return closeReason{}, false
}

// handleNTPPing answers a clock-sync ping. t0 is echoed unchanged; recvMs is the
// receive timestamp (t1); t2 is captured now, as close to the send as this
// design allows (the frame is then queued for the writer). Together they let the
// client estimate its clock offset — see docs/PROTOCOL.md.
func (s *session) handleNTPPing(ctx context.Context, data []byte, recvMs int64) {
	var p protocol.NTPPing
	if err := json.Unmarshal(data, &p); err != nil {
		s.send(ctx, protocol.NewError(protocol.CodeBadMessage, "invalid ntp_ping payload"))
		return
	}
	s.send(ctx, protocol.NewNTPPong(p.T0, recvMs, nowMs()))
}

// send marshals msg and enqueues it for the writer goroutine. It never touches
// the socket directly. If the connection is tearing down (ctx cancelled) the
// frame is dropped rather than blocking the caller forever on a full queue.
func (s *session) send(ctx context.Context, msg any) {
	b, err := json.Marshal(msg)
	if err != nil {
		// Our own message types always marshal; this only fires on a programmer
		// error, so log loudly rather than fail the connection silently.
		s.log.Error("marshal outbound frame", "err", err)
		return
	}
	select {
	case s.outbound <- b:
	case <-ctx.Done():
	}
}
