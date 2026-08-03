package ws

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
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
//   - the ONE exception is displacement (see displace): another connection's
//     goroutine ends this one when it takes over its account's seat. Everything
//     it touches is the socket's own close (idempotent) and displacedAs, which is
//     under displaceMu.
type session struct {
	conn     *websocket.Conn
	log      *slog.Logger
	idGen    func() string
	reg      *Registry
	outbound chan outFrame

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

	// displaceMu guards displacedAs, the close reason a displacing connection
	// leaves behind for this session's own serve goroutine to close with. A
	// mutex and not an atomic flag because the two goroutines need an ordering
	// edge for the reason itself, not merely for the fact.
	displaceMu  sync.Mutex
	displacedAs *closeReason
}

// outFrame is one queued outbound message. sent, when non-nil, is closed by the
// writer the moment the frame has been handed to the socket.
//
// Only displacement uses sent, and it is the whole reason the queue carries a
// struct instead of bare bytes: a close handshake started while the error frame
// explaining the close is still sitting in this queue would reach the client
// FIRST, and the tab would be closed without ever being told why. The signal
// turns "error, then close" from an ordering we hope for into one we wait for.
type outFrame struct {
	data []byte
	sent chan struct{}
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
	for f := range s.outbound {
		// Write is bounded by ctx: a server shutdown or dead peer aborts it
		// instead of blocking forever.
		err := s.conn.Write(ctx, websocket.MessageText, f.data)
		if f.sent != nil {
			// Signalled on failure too: the waiter is waiting for "this frame is
			// no longer ahead of you", and a frame that will never be written
			// satisfies that as surely as one that was.
			close(f.sent)
		}
		if err != nil {
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
	s.enterRoom(ctx, func() seatOutcome { return s.reg.create(s) })
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
	s.enterRoom(ctx, func() seatOutcome { return s.reg.join(m.Code, s) })
}

// enterRoom runs one create_room/join_room to completion, applying the registry's
// account-seat decision (docs/PROTOCOL.md §5, "One seat per account").
//
// The interesting case is a RECLAIM: the account was already in this room, so
// this connection re-attaches to the existing seat and inherits its player id,
// resume token, host role and (mid-match) its whole capture. That is finished
// off here rather than in the registry because it writes frames, and frames
// under reg.mu would hold the whole process's room map open for the length of a
// backlog replay.
//
// The retry exists for exactly one window: between the registry releasing its
// lock and the reattach taking the room lock, the reclaimed seat can be kicked,
// dnf'd, or lose its grace. It is then gone from the room AND from the account
// index, so the second attempt sees a seatless account and takes a fresh seat —
// which is why two iterations are enough and a third is unreachable.
func (s *session) enterRoom(ctx context.Context, attempt func() seatOutcome) {
	for range 2 {
		out := attempt()
		if out.errCode != "" {
			s.send(ctx, protocol.NewError(out.errCode, seatErrorMessage(out.errCode)))
			return
		}
		if out.reclaim == nil {
			s.room = out.room
			return
		}
		if !out.room.reattach(ctx, s, out.reclaim) {
			continue
		}
		// The seat is live again on this connection, so its resume token must
		// stop being claimable by the old one. The hello path gets this from
		// claimGrace, which consumes the entry; taking the seat over by ACCOUNT
		// never touched it, and leaving it behind would strand a grace entry that
		// no timer will ever come back to remove.
		s.reg.removeGrace(out.reclaim.resumeToken)
		s.playerID = out.reclaim.playerID
		s.resumeToken = out.reclaim.resumeToken
		s.room = out.room
		return
	}
	s.send(ctx, protocol.NewError(protocol.CodeInternal, "could not take a seat; try again"))
}

// seatErrorMessage is the human-readable half of a create_room/join_room refusal.
func seatErrorMessage(code string) string {
	switch code {
	case protocol.CodeRoomFull:
		return "room is full"
	case protocol.CodeInMatchElsewhere:
		return "this account is playing a match in another room"
	default:
		return "room not found"
	}
}

// displaceFlushTimeout bounds how long the displacement teardown waits for the
// error frame to reach the socket before closing anyway. A peer stalled long
// enough to still be behind after this has stopped reading; it gets the close
// without the explanation, which is strictly better than a seat held open by a
// tab that is not listening.
const displaceFlushTimeout = 2 * time.Second

// displace ends this connection because a NEWER connection of the same account
// has taken its seat (docs/PROTOCOL.md §5). It is the one path where a session
// is torn down by somebody else's goroutine.
//
// Two constraints shape it, and they pull in opposite directions.
//
// The client is owed BOTH an in-band error (uniform with every other refusal,
// and the only place the machine-readable reason fits) and a close code it can
// distinguish (4001, "do not reconnect"). That means the error frame has to be
// on the wire before the close handshake starts — hence the wait on the writer's
// per-frame signal rather than a plain enqueue-and-close, which would race.
//
// But this runs UNDER THE ROOM LOCK (Room.displaceLocked explains why it has
// to), so it must not block for a microsecond longer than an enqueue: waiting on
// a stalled peer's socket here would stall the room itself, which is the one
// thing every trySend in this package is built to avoid. So the synchronous half
// is a non-blocking enqueue and nothing else, and the half that waits runs on its
// own goroutine. By the time that goroutine starts the seat has already changed
// hands, so nothing downstream depends on when it finishes.
//
// The read side is deliberately NOT cancelled: cancelling the context of a
// coder/websocket read tears the connection down at the transport, and the
// client would get an abnormal close with neither the frame nor the code.
// Conn.Close is what ends the read here, and it is idempotent with serve's own.
func (s *session) displace() {
	s.displaceMu.Lock()
	first := s.displacedAs == nil
	if first {
		s.displacedAs = &closeReason{
			code:   closeSeatTakenOver,
			reason: "seat taken over by a newer connection",
		}
	}
	s.displaceMu.Unlock()
	if !first {
		return
	}

	sent := make(chan struct{})
	b, err := json.Marshal(protocol.NewError(protocol.CodeSeatTakenOver,
		"this account took the seat on another connection"))
	if err != nil {
		s.log.Error("marshal outbound frame", "err", err)
		close(sent)
	} else {
		select {
		case s.outbound <- outFrame{data: b, sent: sent}:
		default:
			close(sent) // queue full: a peer this far behind gets the close alone
		}
	}

	go s.closeDisplaced(sent)
}

// closeDisplaced waits for the displacement error frame to clear the writer and
// then runs the close handshake. It runs on its own goroutine (see displace).
func (s *session) closeDisplaced(sent <-chan struct{}) {
	select {
	case <-sent:
	case <-time.After(displaceFlushTimeout):
	}
	r := s.displacedReason()
	if err := s.conn.Close(r.code, r.reason); err != nil {
		s.log.Debug("close displaced connection", "err", err)
	}
}

// displacedReason returns the close reason left by displace, or nil if this
// connection ended for any other reason.
func (s *session) displacedReason() *closeReason {
	s.displaceMu.Lock()
	defer s.displaceMu.Unlock()
	return s.displacedAs
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
	case s.outbound <- outFrame{data: b}:
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
	case s.outbound <- outFrame{data: b}:
	default:
	}
}
