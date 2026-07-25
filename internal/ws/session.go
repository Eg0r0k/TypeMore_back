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
//   - helloDone/playerID are touched only by readLoop's dispatch, so they need
//     no synchronization; if a future phase mutates them from another goroutine,
//     that invariant must be revisited.
//   - room is likewise touched only by this session's read-loop goroutine; the
//     Room is the authoritative owner of seat membership, so a stale pointer
//     (e.g. after a kick removed the seat) is harmless — room methods re-check.
type session struct {
	conn     *websocket.Conn
	log      *slog.Logger
	idGen    func() string
	reg      *Registry
	outbound chan []byte

	// Identity is resolved at the WS upgrade: an authenticated connection carries
	// its account displayName; a guest gets a server-assigned per-room nick when
	// it joins a room.
	authed      bool
	displayName string
	userID      string // account id (empty for a guest); persisted with the match

	helloDone bool
	playerID  string
	// resumeToken is the per-connection reconnect capability (docs/PROTOCOL.md
	// §6), minted at hello and echoed to the client; a seat records it so a
	// reconnect can reclaim the seat.
	resumeToken string
	room        *Room
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

// heartbeat pings the peer every heartbeatInterval and cancels the connection
// after heartbeatMissed consecutive ping failures (§2), so a silently-dead peer
// is detected and torn down (starting the seat's reconnect grace mid-match). It
// exits when ctx is cancelled. Ping is safe to call concurrently with the writer.
func (s *session) heartbeat(ctx context.Context, cancel context.CancelFunc) {
	missed := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(heartbeatInterval):
		}
		pctx, pcancel := context.WithTimeout(ctx, heartbeatInterval)
		err := s.conn.Ping(pctx)
		pcancel()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			missed++
			if missed >= heartbeatMissed {
				s.log.Debug("heartbeat: peer unresponsive, closing")
				cancel()
				return
			}
			continue
		}
		missed = 0
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
	case protocol.TypeCreateRoom:
		s.handleCreateRoom(ctx)
	case protocol.TypeJoinRoom:
		s.handleJoinRoom(ctx, data)
	case protocol.TypeLeave:
		s.handleLeave(ctx)
	case protocol.TypeReady:
		var m protocol.Ready
		if s.decode(ctx, data, &m) {
			value := m.Ready == nil || *m.Ready
			s.withRoom(ctx, func(r *Room) { r.ready(s, value) })
		}
	case protocol.TypeSettingsUpdate:
		var m protocol.SettingsUpdate
		if s.decode(ctx, data, &m) {
			s.withRoom(ctx, func(r *Room) { r.updateSettings(s, m.Settings) })
		}
	case protocol.TypeSetFreemods:
		var m protocol.SetFreemods
		if s.decode(ctx, data, &m) {
			s.withRoom(ctx, func(r *Room) { r.setFreemods(s, m.Freemods) })
		}
	case protocol.TypeStartMatch:
		s.withRoom(ctx, func(r *Room) { r.startMatch(s) })
	case protocol.TypeKick:
		var m protocol.Kick
		if s.decode(ctx, data, &m) {
			s.withRoom(ctx, func(r *Room) { r.kick(s, m.PlayerID) })
		}
	case protocol.TypeTransferHost:
		var m protocol.TransferHost
		if s.decode(ctx, data, &m) {
			s.withRoom(ctx, func(r *Room) { r.transferHost(s, m.PlayerID) })
		}
	case protocol.TypeChatSend:
		var m protocol.ChatSend
		if s.decode(ctx, data, &m) {
			s.withRoom(ctx, func(r *Room) { r.chat(s, m.Text) })
		}
	case protocol.TypeEventBatch:
		s.handleEventBatch(ctx, data, recvMs)
	case protocol.TypeFinish:
		var m protocol.Finish
		if s.decode(ctx, data, &m) {
			s.withRoom(ctx, func(r *Room) { r.finish(s, m.MatchID, m.Forfeit) })
		}
	default:
		// Unknown or not-yet-implemented types (the relay lands in a later
		// phase): reject in-band without closing.
		s.send(ctx, protocol.NewError(protocol.CodeBadMessage, "unsupported message type: "+env.Type))
	}
	return closeReason{}, false
}

// handleHello validates the client's hello and, on success, assigns a player id
// and acknowledges. A version mismatch is fatal: the error is sent and the
// connection is closed (the server never translates between protocol versions).
// The hello nick is ignored: guests get a server-assigned per-room identity and
// authed connections use their account displayName (resolved at the WS upgrade).
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

	// Reconnect: a hello carrying a resume token tries to reclaim a graced seat.
	// An unknown or expired token is NOT an error: the grace window simply
	// elapsed (e.g. a tab reopened much later), so the hello degrades to a
	// fresh connection — rejecting it would brick every stale-token client.
	if h.ResumeToken != "" {
		if room, seat, ok := s.reg.claimGrace(h.ResumeToken); ok && room.reattach(ctx, s, seat) {
			s.helloDone = true
			s.playerID = seat.playerID
			s.resumeToken = seat.resumeToken
			s.room = room
			return closeReason{}, false
		}
	}

	s.helloDone = true
	s.playerID = s.idGen()
	s.resumeToken = newResumeToken()
	s.send(ctx, protocol.HelloOK{
		Type:          protocol.TypeHelloOK,
		PlayerID:      s.playerID,
		ServerVersion: protocol.Version,
		ResumeToken:   s.resumeToken,
	})
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

// handleEventBatch validates the frame-size cap, then relays the batch. The
// transport read limit is above 1 MiB so the app-level cap is a soft reject
// (bad_message, connection stays) rather than a hard transport close. All other
// envelope checks live in Room.relayEventBatch.
func (s *session) handleEventBatch(ctx context.Context, data []byte, recvMs int64) {
	if len(data) > eventBatchMaxBytes {
		s.send(ctx, protocol.NewError(protocol.CodeBadMessage, "event_batch exceeds 1 MiB"))
		return
	}
	var eb protocol.EventBatch
	if !s.decode(ctx, data, &eb) {
		return
	}
	if s.room == nil {
		s.send(ctx, protocol.NewError(protocol.CodeNotInRoom, "not in a room"))
		return
	}
	s.room.relayEventBatch(s, eb, recvMs)
}

// handleCreateRoom opens a new room with this session as host. The registry
// seats the creator and broadcasts the initial room_state.
func (s *session) handleCreateRoom(ctx context.Context) {
	if s.inRoom() {
		s.send(ctx, protocol.NewError(protocol.CodeBadMessage, "already in a room"))
		return
	}
	s.room = s.reg.Create(s)
}

// handleJoinRoom joins an existing room by code. On success the room seats this
// session and broadcasts room_state plus a system join chat.
func (s *session) handleJoinRoom(ctx context.Context, data []byte) {
	var m protocol.JoinRoom
	if !s.decode(ctx, data, &m) {
		return
	}
	if s.inRoom() {
		s.send(ctx, protocol.NewError(protocol.CodeBadMessage, "already in a room"))
		return
	}
	room, code := s.reg.Join(m.Code, s)
	if code != "" {
		msg := "room not found"
		if code == protocol.CodeRoomFull {
			msg = "room is full"
		}
		s.send(ctx, protocol.NewError(code, msg))
		return
	}
	s.room = room
}

// handleLeave removes this session from its room. It clears the room pointer even
// if the seat was already gone (e.g. after a kick), which makes leave idempotent.
func (s *session) handleLeave(ctx context.Context) {
	if s.room == nil {
		s.send(ctx, protocol.NewError(protocol.CodeNotInRoom, "not in a room"))
		return
	}
	s.room.leave(s)
	s.room = nil
}

// withRoom routes a room-scoped action, rejecting with not_in_room when the
// session has no room. The room method re-checks seat membership, so a stale
// room pointer (after a kick) still surfaces not_in_room.
func (s *session) withRoom(ctx context.Context, fn func(*Room)) {
	if s.room == nil {
		s.send(ctx, protocol.NewError(protocol.CodeNotInRoom, "not in a room"))
		return
	}
	fn(s.room)
}

// inRoom reports whether the session currently holds a seat (not merely a stale
// room pointer left over from a kick).
func (s *session) inRoom() bool {
	return s.room != nil && s.room.hasSeat(s)
}

// decode unmarshals a room message payload, reporting a bad_message error and
// returning false on failure.
func (s *session) decode(ctx context.Context, data []byte, v any) bool {
	if err := json.Unmarshal(data, v); err != nil {
		s.send(ctx, protocol.NewError(protocol.CodeBadMessage, "invalid message payload"))
		return false
	}
	return true
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

// trySend marshals msg and enqueues it non-blockingly for the writer goroutine.
// Unlike send it never blocks: if the outbound queue is full (a stalled consumer)
// the frame is dropped. It is the delivery path the room uses for broadcasts and
// acks, which run under the room lock, so one stuck client cannot stall the room;
// that client's read/heartbeat path tears its connection down independently.
func (s *session) trySend(msg any) {
	b, err := json.Marshal(msg)
	if err != nil {
		s.log.Error("marshal outbound frame", "err", err)
		return
	}
	select {
	case s.outbound <- b:
	default:
	}
}
