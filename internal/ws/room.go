package ws

import (
	"context"
	"fmt"
	"log/slog"
	mrand "math/rand/v2"
	"sync"
	"time"

	"github.com/typemore/typemore-server/internal/protocol"
)

// seatActive is the non-terminal match status; terminal ones reuse the protocol
// Status* strings (finished / dnf / left).
const seatActive = "active"

const (
	guestNickLow   = 1000
	guestNickCount = 9000
)

// seat is one room member. It is the durable object across a reconnect: on any
// drop the *seat survives for the grace window (sess goes nil; mid-match the
// backlog buffers) and a new session re-attaches to it via the resume token.
//
// The zero-value chat bucket is lazily initialized to a full burst on first use.
type seat struct {
	sess        *session // nil while disconnected (grace) or after a terminal exit
	playerID    string
	nick        string
	isGuest     bool
	userID      string // empty for a guest
	resumeToken string
	ready       bool
	freemods    protocol.Freemods
	joinSeq     uint64

	chatTokens float64
	chatLast   time.Time

	// Match/relay state — valid while r.match != nil and this seat is a roster
	// participant.
	status       string          // seatActive | finished | dnf | left
	lastSeq      int             // last accepted event_batch batchSeq (0 = none)
	batches      []CapturedBatch // authoritative per-player capture
	eventCount   int             // total events across accepted batches
	finishedAtMs int64           // server clock at receipt of this seat's finish (0 = none)
	lastBatchMs  int64           // server clock of the last accepted batch ("go" before any)
	// AFK accounting (docs/PROTOCOL.md §6): one-second buckets of the match
	// window, counted on the RECEIVE clock. activeBuckets counts the distinct
	// buckets that carried at least one accepted event_batch; lastBucket is the
	// most recent one counted (-1 = none yet). Batches arrive in order, so this
	// is O(1) per batch and needs no per-second storage.
	activeBuckets int
	lastBucket    int
	disconnected  bool  // true during the reconnect grace window
	backlog       []any // frames buffered for a graced seat (peer_batch, match_end)
	grace         *time.Timer
}

// Room is a lobby: the authoritative owner of its seats, settings, and the
// current match. Every mutation and broadcast happens under mu, so state changes
// and the frames that announce them are serialized — the room is the "actor" the
// protocol refers to. Frames are delivered with session.trySend (non-blocking)
// so a single stalled client cannot stall a broadcast held under mu; the
// exception is reconnect backlog replay, which is blocking and off-lock.
type Room struct {
	code string
	// createdAt stamps construction and is immutable thereafter. It exists for
	// the public lobby list, which orders rooms oldest-first so a newly opened
	// room cannot displace an established one (see lobby.go).
	createdAt time.Time
	reg       *Registry
	log       *slog.Logger
	store     MatchStore // nil disables persistence

	mu       sync.Mutex
	settings protocol.Settings
	seats    []*seat
	hostID   string
	inMatch  bool
	match    *matchState
	nextSeq  uint64
}

// newRoom builds an empty room with default settings.
func newRoom(code string, reg *Registry, log *slog.Logger, store MatchStore) *Room {
	return &Room{
		code:      code,
		createdAt: time.Now(),
		reg:       reg,
		log:       log,
		store:     store,
		settings:  protocol.DefaultSettings(""),
	}
}

// seat adds sess as a new seat, assigning its display identity, and broadcasts
// the result. asHost marks it the host (the room creator). It returns false
// without seating when the room is full. A non-host join also emits a system
// join chat.
func (r *Room) seat(sess *session, asHost bool) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	if len(r.seats) >= roomCapacity {
		return false
	}

	nick, isGuest := r.identityLocked(sess)
	st := &seat{
		sess:        sess,
		playerID:    sess.playerID,
		nick:        nick,
		isGuest:     isGuest,
		userID:      sess.userID,
		resumeToken: sess.resumeToken,
		freemods:    protocol.DefaultFreemods(),
		joinSeq:     r.nextSeq,
	}
	r.nextSeq++
	r.seats = append(r.seats, st)
	if asHost {
		r.hostID = sess.playerID
	}

	r.broadcastStateLocked()
	if !asHost {
		r.systemChatLocked(protocol.ChatKindJoin, nick+" joined")
	}
	return true
}

// leave removes sess's seat on a voluntary `leave`. Mid-match it marks the seat
// as terminally "left" (its capture persists in the roster) and may end the
// match. On a host departure the role passes to the earliest-joined remaining
// seat. The room is dropped from the registry if it becomes empty.
func (r *Room) leave(sess *session) {
	r.mu.Lock()
	seat := r.findSeatLocked(sess)
	if seat == nil {
		r.mu.Unlock()
		return
	}
	midMatch := r.match != nil && seat.status == seatActive
	if midMatch {
		seat.status = protocol.StatusLeft
		seat.sess = nil
	}
	wasHost := seat.playerID == r.hostID
	r.removeFromSeatsLocked(seat)
	if wasHost {
		r.reassignHostLocked()
	}
	if midMatch {
		r.broadcastPeerStatusLocked(seat.playerID, protocol.StatusLeft)
	}
	if len(r.seats) > 0 {
		r.broadcastStateLocked()
		r.systemChatLocked(protocol.ChatKindLeave, seat.nick+" left")
		if wasHost {
			r.systemChatLocked(protocol.ChatKindHostChanged, r.hostNickLocked()+" is now host")
		}
	}
	if r.match != nil && r.allTerminalLocked() {
		r.endMatchLocked(protocol.ReasonAllFinished)
	}
	r.mu.Unlock()

	r.reg.removeIfEmpty(r.code)
}

// disconnect handles a connection drop (readLoop teardown). The seat survives
// in ANY phase: it enters the reconnect grace window and the caller registers
// its resume token. Mid-match the drop is announced (peer_status disconnected)
// and the peer-relay backlog starts buffering; a lobby-phase drop is silent on
// the wire — the seat simply stays, keeping its ready flag and (if host) the
// host role until the grace expires.
func (r *Room) disconnect(sess *session) (bool, *seat) {
	r.mu.Lock()
	seat := r.findSeatLocked(sess)
	if seat == nil {
		r.mu.Unlock()
		return false, nil
	}
	seat.sess = nil
	seat.disconnected = true
	seat.grace = time.AfterFunc(r.reg.timing.grace, func() { r.onGraceExpire(seat) })
	if r.match != nil && seat.status == seatActive {
		r.broadcastPeerStatusLocked(seat.playerID, protocol.StatusDisconnected)
	}
	r.mu.Unlock()
	return true, seat
}

// reattach re-binds a fresh session to a disconnected seat in any phase. It
// returns false if the seat is no longer reclaimable (removed by a kick, grace
// expiry, or the match-end reap). The resumer always receives hello_ok followed
// by a fresh room_state; a mid-match resume then also replays its buffered
// backlog exactly once (in order) and announces reconnected. hello_ok,
// room_state, and the backlog are sent with the blocking, ordered send so a
// large backlog is never dropped.
func (r *Room) reattach(ctx context.Context, sess *session, seat *seat) bool {
	r.mu.Lock()
	if !seat.disconnected || r.seatIndexByIDLocked(seat.playerID) < 0 {
		r.mu.Unlock()
		return false
	}
	midMatch := r.match != nil && seat.status == seatActive
	seat.sess = sess
	if seat.grace != nil {
		seat.grace.Stop()
		seat.grace = nil
	}
	// Mid-match, keep disconnected=true so concurrent relay keeps appending to
	// the backlog while we flush it; flip to live only once it is drained. The
	// drain loop below also replays a post-match backlog (e.g. the match_end of
	// a match that ended during the grace window) before going live.
	state := r.stateLocked()
	r.mu.Unlock()

	sess.send(ctx, protocol.HelloOK{
		Type:          protocol.TypeHelloOK,
		PlayerID:      seat.playerID,
		ServerVersion: protocol.Version,
		ResumeToken:   seat.resumeToken,
	})
	sess.send(ctx, state)

	for {
		r.mu.Lock()
		if len(seat.backlog) == 0 {
			seat.disconnected = false
			if midMatch {
				r.broadcastPeerStatusLocked(seat.playerID, protocol.StatusReconnected)
			}
			r.mu.Unlock()
			return true
		}
		pending := seat.backlog
		seat.backlog = nil
		r.mu.Unlock()

		for _, pb := range pending {
			sess.send(ctx, pb)
		}
		if ctx.Err() != nil {
			return true // connection dying again; teardown will re-grace
		}
	}
}

// ready sets the sender's seat ready flag (true unless the frame carried an
// explicit ready:false) and rebroadcasts.
func (r *Room) ready(sess *session, value bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	st := r.findSeatLocked(sess)
	if st == nil {
		r.errLocked(sess, protocol.CodeNotInRoom, "not in a room")
		return
	}
	st.ready = value
	r.broadcastStateLocked()
}

// updateSettings replaces the room settings (host-only, between matches). It
// sanitizes and validates the incoming settings, resets every ready flag, and
// rebroadcasts.
func (r *Room) updateSettings(sess *session, ns protocol.Settings) {
	r.mu.Lock()
	defer r.mu.Unlock()

	st := r.findSeatLocked(sess)
	if st == nil {
		r.errLocked(sess, protocol.CodeNotInRoom, "not in a room")
		return
	}
	if st.playerID != r.hostID {
		r.errLocked(sess, protocol.CodeForbidden, "only the host can change settings")
		return
	}
	if r.inMatch {
		r.errLocked(sess, protocol.CodeBadMessage, "cannot change settings during a match")
		return
	}
	ns.Name = protocol.SanitizeRoomName(ns.Name)
	if err := protocol.ValidateSettings(ns); err != nil {
		r.errLocked(sess, protocol.CodeBadMessage, err.Error())
		return
	}
	r.settings = ns
	for _, s := range r.seats {
		s.ready = false
	}
	r.broadcastStateLocked()
	r.systemChatLocked(protocol.ChatKindSettings, "settings changed")
}

// setFreemods sets the sender's freemods (any seat, between matches only).
func (r *Room) setFreemods(sess *session, fm protocol.Freemods) {
	r.mu.Lock()
	defer r.mu.Unlock()

	st := r.findSeatLocked(sess)
	if st == nil {
		r.errLocked(sess, protocol.CodeNotInRoom, "not in a room")
		return
	}
	if r.inMatch {
		r.errLocked(sess, protocol.CodeBadMessage, "cannot change freemods during a match")
		return
	}
	if err := protocol.ValidateFreemods(fm); err != nil {
		r.errLocked(sess, protocol.CodeBadMessage, err.Error())
		return
	}
	st.freemods = fm
	r.broadcastStateLocked()
}

// kick removes another seat (host-only, between matches). The target receives a
// Kicked frame and is dropped; the remaining seats get a fresh room_state and a
// NEUTRAL "left" system chat (the departure is not exposed as a kick).
func (r *Room) kick(sess *session, targetID string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	st := r.findSeatLocked(sess)
	if st == nil {
		r.errLocked(sess, protocol.CodeNotInRoom, "not in a room")
		return
	}
	if st.playerID != r.hostID {
		r.errLocked(sess, protocol.CodeForbidden, "only the host can kick")
		return
	}
	if r.inMatch {
		r.errLocked(sess, protocol.CodeForbidden, "cannot kick during a match")
		return
	}
	idx := r.seatIndexByIDLocked(targetID)
	if idx < 0 {
		r.errLocked(sess, protocol.CodeBadMessage, "no such player")
		return
	}
	target := r.seats[idx]
	if target.playerID == r.hostID {
		r.errLocked(sess, protocol.CodeBadMessage, "cannot kick yourself")
		return
	}
	r.removeSeatLocked(idx)
	if target.sess != nil {
		target.sess.trySend(protocol.Kicked{Type: protocol.TypeKicked})
	}
	r.broadcastStateLocked()
	r.systemChatLocked(protocol.ChatKindLeave, target.nick+" left")
}

// transferHost hands the host role to another seat (host-only).
func (r *Room) transferHost(sess *session, targetID string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	st := r.findSeatLocked(sess)
	if st == nil {
		r.errLocked(sess, protocol.CodeNotInRoom, "not in a room")
		return
	}
	if st.playerID != r.hostID {
		r.errLocked(sess, protocol.CodeForbidden, "only the host can transfer the host role")
		return
	}
	target := r.findSeatByIDLocked(targetID)
	if target == nil {
		r.errLocked(sess, protocol.CodeBadMessage, "no such player")
		return
	}
	if target.playerID == r.hostID {
		r.broadcastStateLocked()
		return
	}
	r.hostID = target.playerID
	r.broadcastStateLocked()
	r.systemChatLocked(protocol.ChatKindHostChanged, target.nick+" is now host")
}

// hasSeat reports whether sess currently holds a seat in this room.
func (r *Room) hasSeat(sess *session) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.findSeatLocked(sess) != nil
}

// empty reports whether the room has no seats.
func (r *Room) empty() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.seats) == 0
}

// onGraceExpire resolves a seat whose reconnect grace elapsed. Mid-match the
// seat is dnf'd, freeing it for host succession and ending the match if it was
// the last unresolved seat; outside a match (or as a terminal match participant)
// the seat departs through the normal leave flow. A seat that was reclaimed or
// already removed (kick, match-end reap) is left alone.
func (r *Room) onGraceExpire(seat *seat) {
	r.mu.Lock()
	if seat.sess == nil && seat.disconnected && r.seatIndexByIDLocked(seat.playerID) >= 0 {
		if r.match != nil && seat.status == seatActive {
			seat.status = protocol.StatusDNF
			r.broadcastPeerStatusLocked(seat.playerID, protocol.StatusDNF)
			wasHost := seat.playerID == r.hostID
			r.removeFromSeatsLocked(seat)
			if wasHost {
				r.reassignHostLocked()
			}
			if r.allTerminalLocked() {
				r.endMatchLocked(protocol.ReasonAllFinished)
			} else if len(r.seats) > 0 {
				r.broadcastStateLocked()
				if wasHost {
					r.systemChatLocked(protocol.ChatKindHostChanged, r.hostNickLocked()+" is now host")
				}
			}
		} else {
			seat.disconnected = false
			wasHost := seat.playerID == r.hostID
			r.removeFromSeatsLocked(seat)
			if wasHost {
				r.reassignHostLocked()
			}
			if len(r.seats) > 0 {
				r.broadcastStateLocked()
				r.systemChatLocked(protocol.ChatKindLeave, seat.nick+" left")
				if wasHost {
					r.systemChatLocked(protocol.ChatKindHostChanged, r.hostNickLocked()+" is now host")
				}
			}
		}
	}
	r.mu.Unlock()
	r.reg.removeGrace(seat.resumeToken)
	r.reg.removeIfEmpty(r.code)
}

// --- locked helpers (caller holds r.mu) ---

// deliverLocked sends msg to a seat's session, skipping seats with no live
// connection (disconnected during grace, or terminally departed).
func (r *Room) deliverLocked(st *seat, msg any) {
	if st.sess != nil {
		st.sess.trySend(msg)
	}
}

// broadcastPeerStatusLocked sends a peer_status about subjectID to every other
// live participant of the current match.
func (r *Room) broadcastPeerStatusLocked(subjectID, status string) {
	if r.match == nil {
		return
	}
	msg := protocol.PeerStatus{Type: protocol.TypePeerStatus, PlayerID: subjectID, Status: status}
	// `finished` goes to EVERY seat, the finisher included; every other status
	// goes to the peers only.
	//
	// The asymmetry is deliberate and it is about who owns the fact. A seat
	// learns it disconnected by disconnecting — there is nothing to tell it, and
	// no socket to tell it on. But whether a `finish` was ACCEPTED is the
	// server's answer, not the client's: the client sends `finish` and then
	// believes its own optimistic transition, which is how a finish the server
	// rejected (wrong matchId, an already-terminal seat) leaves a client showing
	// a finished screen for a run nobody recorded. Echoing the status back means
	// the finisher and its opponents transition on the same message, at the same
	// instant, from the same authority.
	echoToSubject := status == protocol.StatusFinished
	for _, s := range r.match.roster {
		if s.playerID == subjectID && !echoToSubject {
			continue
		}
		if s.sess != nil && !s.disconnected {
			s.sess.trySend(msg)
		}
	}
}

// identityLocked resolves a session's room display identity: an authenticated
// connection uses its account displayName; a guest gets a unique per-room
// "Guest-XXXX".
func (r *Room) identityLocked(sess *session) (nick string, isGuest bool) {
	if sess.authed && sess.displayName != "" {
		return sess.displayName, false
	}
	return r.uniqueGuestNickLocked(), true
}

// uniqueGuestNickLocked returns a "Guest-XXXX" nick not already used in the room.
func (r *Room) uniqueGuestNickLocked() string {
	for {
		nick := fmt.Sprintf("Guest-%04d", guestNickLow+mrand.IntN(guestNickCount))
		if !r.nickTakenLocked(nick) {
			return nick
		}
	}
}

func (r *Room) nickTakenLocked(nick string) bool {
	for _, s := range r.seats {
		if s.nick == nick {
			return true
		}
	}
	return false
}

func (r *Room) findSeatLocked(sess *session) *seat {
	for _, s := range r.seats {
		if s.sess == sess {
			return s
		}
	}
	return nil
}

func (r *Room) removeFromSeatsLocked(target *seat) {
	for i, s := range r.seats {
		if s == target {
			r.removeSeatLocked(i)
			return
		}
	}
}

func (r *Room) findSeatByIDLocked(playerID string) *seat {
	for _, s := range r.seats {
		if s.playerID == playerID {
			return s
		}
	}
	return nil
}

func (r *Room) seatIndexByIDLocked(playerID string) int {
	for i, s := range r.seats {
		if s.playerID == playerID {
			return i
		}
	}
	return -1
}

// removeSeatLocked drops the seat at idx, preserving join order for the seats
// that remain.
func (r *Room) removeSeatLocked(idx int) {
	r.seats = append(r.seats[:idx], r.seats[idx+1:]...)
}

// reassignHostLocked promotes the earliest-joined remaining seat to host (the
// seats slice preserves join order, so that is seats[0]). It clears the host id
// when the room is now empty.
func (r *Room) reassignHostLocked() {
	if len(r.seats) == 0 {
		r.hostID = ""
		return
	}
	r.hostID = r.seats[0].playerID
}

func (r *Room) hostNickLocked() string {
	if s := r.findSeatByIDLocked(r.hostID); s != nil {
		return s.nick
	}
	return ""
}

// broadcastStateLocked sends a fresh room_state snapshot to every live seat.
func (r *Room) broadcastStateLocked() {
	state := r.stateLocked()
	for _, s := range r.seats {
		r.deliverLocked(s, state)
	}
}

func (r *Room) stateLocked() protocol.RoomState {
	players := make([]protocol.Player, len(r.seats))
	for i, s := range r.seats {
		players[i] = protocol.Player{
			PlayerID: s.playerID,
			Nick:     s.nick,
			IsGuest:  s.isGuest,
			Ready:    s.ready,
			Freemods: s.freemods,
		}
	}
	// A running match is named here so a client that reconnects into one it has
	// no state for (page reload) can tell — see protocol.RoomMatch.
	var match *protocol.RoomMatch
	if r.match != nil && !r.match.ended {
		match = &protocol.RoomMatch{MatchID: r.match.id, GoAtServerMs: r.match.goAtMs}
	}
	return protocol.RoomState{
		Type:         protocol.TypeRoomState,
		Code:         r.code,
		Name:         r.settings.Name,
		Visibility:   r.settings.Visibility,
		HostPlayerID: r.hostID,
		Settings:     r.settings,
		Players:      players,
		Match:        match,
	}
}

// errLocked sends an error frame to a single session.
func (r *Room) errLocked(sess *session, code, message string) {
	sess.trySend(protocol.NewError(code, message))
}
