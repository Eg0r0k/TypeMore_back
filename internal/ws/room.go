package ws

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log/slog"
	mrand "math/rand/v2"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/typemore/typemore-server/internal/protocol"
)

// countdownLeadMs is how far ahead of "now" a Countdown schedules t=0, leaving
// the client room to run the local 5-4-3-2-1 after converting via its NTP
// offset. Five seconds, not three: the client cannot start counting until the
// frame arrives, and the countdown is also the only window a player has to look
// at the text before it starts moving.
const countdownLeadMs = 5000

// Match end timing defaults. deadlineSlackMs is the tolerance added past a
// match's nominal length before unfinished seats are force-dnf'd; wordModeMsPerWord
// is the generous per-word ceiling bounding word-mode matches (no fixed
// duration); graceDuration is the seat-reconnect window on any connection drop.
// These are the production values; a Registry may override them (see Option) so
// tests can drive the deadline/grace paths quickly.
const (
	deadlineSlackMsDefault   = 30_000
	wordModeMsPerWordDefault = 3_000
	graceDurationDefault     = 15 * time.Second
)

// matchTiming holds the tunable match-end timings a Registry applies to its rooms.
type matchTiming struct {
	grace           time.Duration
	deadlineSlackMs int64
	wordMsPerWord   int64
	afkKickShare    float64
	afkWarmupMs     int64
	afkTrailingMs   int64
	finishWindow    time.Duration
}

// defaultTiming returns the production match timings.
func defaultTiming() matchTiming {
	return matchTiming{
		grace:           graceDurationDefault,
		deadlineSlackMs: deadlineSlackMsDefault,
		wordMsPerWord:   wordModeMsPerWordDefault,
		afkKickShare:    protocol.AfkKickShare,
		afkWarmupMs:     protocol.AfkWarmupMs,
		afkTrailingMs:   protocol.AfkTrailingMs,
		finishWindow:    protocol.FinishWindowMs * time.Millisecond,
	}
}

// seatActive is the non-terminal match status; terminal ones reuse the protocol
// Status* strings (finished / dnf / left).
const seatActive = "active"

// Chat rate-limit token bucket: burst of 5, fully refilled over 2s (one token
// every 400ms). Matches the shared limiter's intent without importing the auth
// domain into the transport layer.
const (
	chatBurst      = 5
	chatRefill     = 400 * time.Millisecond
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

// allowChat reports whether this seat may send a chat message now, consuming a
// token if so. A token-bucket refilling one token per chatRefill up to chatBurst.
func (st *seat) allowChat(now time.Time) bool {
	if st.chatLast.IsZero() {
		st.chatTokens = chatBurst
	} else {
		st.chatTokens = min(float64(chatBurst), st.chatTokens+now.Sub(st.chatLast).Seconds()/chatRefill.Seconds())
	}
	st.chatLast = now
	if st.chatTokens >= 1 {
		st.chatTokens--
		return true
	}
	return false
}

// afkAtLocked reports the seat's idle time within its match window [goAt, endMs]
// and that idle time's share of the window: whole one-second buckets carrying no
// accepted event_batch, divided by the window's whole buckets. Zero buckets (a
// window shorter than a second) is 0 idle, never a division by zero. Caller holds
// the room lock.
func (st *seat) afkAtLocked(goAtMs, endMs int64) (int64, float64) {
	buckets := (endMs - goAtMs) / protocol.AfkBucketMs
	if buckets <= 0 {
		return 0, 0
	}
	idle := buckets - int64(st.activeBuckets)
	if idle < 0 {
		idle = 0
	}
	return idle * protocol.AfkBucketMs, float64(idle) / float64(buckets)
}

// matchState is the per-match state a room owns while inMatch. roster is the
// frozen set of participant seats; their captures live on the seats and persist
// (via the roster) even after a seat leaves the lobby seats slice on dnf/left.
type matchState struct {
	id           string
	seed         int64
	goAtMs       int64
	settings     protocol.Settings
	players      []protocol.CountdownPlayer // frozen freemod snapshot
	roster       []*seat
	deadline     *time.Timer
	finishWindow *time.Timer  // words mode: armed by the first finish
	afkSweep     *time.Ticker // words mode: the per-second AFK-share sweep
	afkStop      chan struct{}
	ended        bool
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

// startMatch begins the match (host-only). It requires at least two seats and
// every non-host seat ready; otherwise it rejects with not_ready. On success it
// freezes the settings and per-player freemods into a Countdown, stamps a
// server-generated seed and a fresh matchId, arms the hard deadline, and marks
// the room in-match.
func (r *Room) startMatch(sess *session) {
	r.mu.Lock()
	defer r.mu.Unlock()

	st := r.findSeatLocked(sess)
	if st == nil {
		r.errLocked(sess, protocol.CodeNotInRoom, "not in a room")
		return
	}
	if st.playerID != r.hostID {
		r.errLocked(sess, protocol.CodeForbidden, "only the host can start the match")
		return
	}
	if r.inMatch {
		r.errLocked(sess, protocol.CodeForbidden, "a match is already running")
		return
	}
	if len(r.seats) < 2 {
		r.errLocked(sess, protocol.CodeNotReady, "need at least two players")
		return
	}
	for _, s := range r.seats {
		if s.playerID != r.hostID && !s.ready {
			r.errLocked(sess, protocol.CodeNotReady, "all players must be ready")
			return
		}
	}

	settings := r.settings
	goAt := nowMs() + countdownLeadMs
	players := make([]protocol.CountdownPlayer, len(r.seats))
	roster := make([]*seat, len(r.seats))
	for i, s := range r.seats {
		s.status = seatActive
		s.lastSeq = 0
		s.batches = nil
		s.eventCount = 0
		s.finishedAtMs = 0
		s.lastBatchMs = goAt
		s.activeBuckets = 0
		s.lastBucket = -1
		s.disconnected = s.sess == nil // a lobby-graced seat starts the match still graced
		s.backlog = nil
		players[i] = protocol.CountdownPlayer{PlayerID: s.playerID, Freemods: s.freemods}
		roster[i] = s
	}

	id := newMatchID()
	m := &matchState{
		id:       id,
		seed:     newSeed(),
		goAtMs:   goAt,
		settings: settings,
		players:  players,
		roster:   roster,
	}
	endBy := goAt + r.matchDurationMs(settings) + r.reg.timing.deadlineSlackMs
	m.deadline = time.AfterFunc(time.Duration(endBy-nowMs())*time.Millisecond, func() {
		r.onDeadline(id)
	})
	// AFK sweep (§6): once per second, dnf every racing seat that has gone
	// silent (trailing rule, both modes) or that has been idle for most of the
	// window (share rule, words only). A graced seat is not exempt: a
	// disconnected player is the most AFK a player gets.
	m.afkSweep = time.NewTicker(protocol.AfkBucketMs * time.Millisecond)
	m.afkStop = make(chan struct{})
	go r.runAfkSweep(id, m.afkSweep.C, m.afkStop)
	r.match = m
	r.inMatch = true

	cd := protocol.Countdown{
		Type:         protocol.TypeCountdown,
		MatchID:      id,
		GoAtServerMs: goAt,
		Seed:         m.seed,
		Settings:     settings,
		Players:      players,
	}
	for _, s := range r.seats {
		r.deliverLocked(s, cd)
	}
}

// relayEventBatch validates an event_batch envelope, stamps the server receive
// time, appends to the sender's authoritative capture, and relays it as a
// peer_batch to every other participant (buffering for disconnected ones). The
// batch contents stay opaque. Any validation failure is a bad_message; the batch
// is dropped and the connection stays open.
func (r *Room) relayEventBatch(sess *session, eb protocol.EventBatch, recvMs int64) {
	r.mu.Lock()
	defer r.mu.Unlock()

	st := r.findSeatLocked(sess)
	if st == nil {
		r.errLocked(sess, protocol.CodeNotInRoom, "not in a room")
		return
	}
	if r.match == nil {
		r.errLocked(sess, protocol.CodeBadMessage, "no active match")
		return
	}
	if eb.MatchID != r.match.id {
		r.errLocked(sess, protocol.CodeBadMessage, "unknown match")
		return
	}
	if st.status != seatActive {
		r.errLocked(sess, protocol.CodeBadMessage, "not an active participant")
		return
	}
	if len(eb.Events) == 0 {
		r.errLocked(sess, protocol.CodeBadMessage, "event_batch has no events")
		return
	}
	if eb.BatchSeq != st.lastSeq+1 {
		r.errLocked(sess, protocol.CodeBadMessage, fmt.Sprintf("batchSeq %d out of order (expected %d)", eb.BatchSeq, st.lastSeq+1))
		return
	}

	st.lastSeq = eb.BatchSeq
	st.batches = append(st.batches, CapturedBatch{
		BatchSeq:     eb.BatchSeq,
		RecvServerMs: recvMs,
		Events:       eb.Events,
	})
	st.eventCount += len(eb.Events)
	st.lastBatchMs = recvMs
	// AFK accounting: mark this arrival's one-second bucket active. Batches are
	// accepted in order, so comparing against the last counted bucket is enough.
	if bucket := (recvMs - r.match.goAtMs) / protocol.AfkBucketMs; bucket > int64(st.lastBucket) {
		st.lastBucket = int(bucket)
		st.activeBuckets++
	}

	// The relayed batch inherits the SENDER's version: it carries the sender's
	// events, so it is described by the sender's schema (docs/PROTOCOL.md).
	pb := protocol.PeerBatch{
		Type:     protocol.TypePeerBatch,
		PlayerID: st.playerID,
		Version:  eb.Version,
		Events:   eb.Events,
	}
	for _, other := range r.match.roster {
		if other == st {
			continue
		}
		if other.disconnected {
			other.backlog = append(other.backlog, pb)
			continue
		}
		if other.sess != nil {
			other.sess.trySend(pb)
		}
	}
}

// finish resolves the sender's run for the given match and broadcasts the
// resulting peer_status, ending the match once every seat is terminal. A
// forfeit (protocol.Finish.Forfeit — a reloaded page abandoning a run it can no
// longer produce) resolves to dnf: no finish instant, and no words-mode finish
// window, because nobody finished.
func (r *Room) finish(sess *session, matchID string, forfeit bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	st := r.findSeatLocked(sess)
	if st == nil {
		r.errLocked(sess, protocol.CodeNotInRoom, "not in a room")
		return
	}
	if r.match == nil || matchID != r.match.id {
		r.errLocked(sess, protocol.CodeBadMessage, "unknown match")
		return
	}
	if st.status != seatActive {
		r.errLocked(sess, protocol.CodeBadMessage, "already finished")
		return
	}
	if forfeit {
		st.status = protocol.StatusDNF
		r.broadcastPeerStatusLocked(st.playerID, protocol.StatusDNF)
		if r.allTerminalLocked() {
			r.endMatchLocked(protocol.ReasonAllFinished)
		}
		return
	}
	st.status = protocol.StatusFinished
	st.finishedAtMs = nowMs()
	r.broadcastPeerStatusLocked(st.playerID, protocol.StatusFinished)
	if r.allTerminalLocked() {
		r.endMatchLocked(protocol.ReasonAllFinished)
	} else if protocol.IsCounted(r.match.settings.Mode) && r.match.finishWindow == nil {
		// First finish of a words-mode match opens the finish window: at close
		// every still-racing seat is dnf'd and the match ends (finish_window).
		id := r.match.id
		r.match.finishWindow = time.AfterFunc(r.reg.timing.finishWindow, func() { r.onFinishWindow(id) })
	}
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

// chat validates, rate-limits, and broadcasts a lobby chat message.
func (r *Room) chat(sess *session, text string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	st := r.findSeatLocked(sess)
	if st == nil {
		r.errLocked(sess, protocol.CodeNotInRoom, "not in a room")
		return
	}
	text = strings.TrimSpace(text)
	if n := utf8.RuneCountInString(text); n < protocol.ChatTextMinLen || n > protocol.ChatTextMaxLen {
		r.errLocked(sess, protocol.CodeBadMessage, "chat text must be 1-200 characters")
		return
	}
	if !st.allowChat(time.Now()) {
		r.errLocked(sess, protocol.CodeRateLimited, "chat rate limit exceeded")
		return
	}
	msg := protocol.Chat{Type: protocol.TypeChat, From: st.playerID, Text: text, Ts: nowMs()}
	for _, s := range r.seats {
		r.deliverLocked(s, msg)
	}
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

// --- match-end machinery (caller holds r.mu) ---

// onDeadline force-ends a match at its hard deadline, dnf'ing every seat that has
// not finished.
func (r *Room) onDeadline(matchID string) {
	r.mu.Lock()
	if r.match == nil || r.match.id != matchID || r.match.ended {
		r.mu.Unlock()
		return
	}
	for _, s := range r.match.roster {
		if s.status == seatActive {
			s.status = protocol.StatusDNF
			r.broadcastPeerStatusLocked(s.playerID, protocol.StatusDNF)
		}
	}
	r.endMatchLocked(protocol.ReasonDeadline)
	r.mu.Unlock()
	r.reg.removeIfEmpty(r.code)
}

// runAfkSweep is the words-mode AFK rule (§6): once per second it dnf's every
// racing seat whose IDLE SHARE of the elapsed match window has crossed
// afkKickShare. It runs on its own goroutine for the match's lifetime and exits
// on afkStop (closed by endMatchLocked) or as soon as it observes a different /
// ended match, so a rematch never inherits the previous sweep.
//
// Why a share and not the old fixed idle timer: the timer judged a pause, this
// judges participation. A player who thinks for eight seconds and types again is
// never punished; one who is simply not there crosses the line and stops holding
// the room hostage. The warm-up exists because a young window is idle by
// construction — at t=2s nobody has typed for "most" of the match yet.
func (r *Room) runAfkSweep(matchID string, tick <-chan time.Time, stop <-chan struct{}) {
	for {
		select {
		case <-stop:
			return
		case <-tick:
			if done := r.sweepAfk(matchID); done {
				return
			}
		}
	}
}

// sweepAfk performs one AFK pass, reporting whether the sweep should stop
// (the match ended or was replaced).
//
// Two rules, checked in this order per racing seat:
//   - TRAILING: nothing accepted for afkTrailingMs. Applies in every mode — the
//     player is not at the keyboard, whatever the mode's end condition is.
//   - SHARE: idle for at least afkKickShare of the elapsed window, words mode
//     only, and only once the window is old enough to judge (afkWarmupMs).
func (r *Room) sweepAfk(matchID string) bool {
	r.mu.Lock()
	m := r.match
	if m == nil || m.id != matchID || m.ended {
		r.mu.Unlock()
		return true
	}
	now := nowMs()
	windowOldEnough := now-m.goAtMs >= r.reg.timing.afkWarmupMs
	judgeShare := protocol.IsCounted(m.settings.Mode) && windowOldEnough
	kicked := false
	for _, s := range m.roster {
		if s.status != seatActive {
			continue
		}
		// lastBatchMs starts at "go", so a seat that never typed is measured
		// from the start of the match, not from its first (absent) batch.
		silentFor := now - s.lastBatchMs
		out := silentFor >= r.reg.timing.afkTrailingMs
		if !out && judgeShare {
			_, share := s.afkAtLocked(m.goAtMs, now)
			out = share >= r.reg.timing.afkKickShare
		}
		if !out {
			continue
		}
		s.status = protocol.StatusDNF
		r.broadcastPeerStatusLocked(s.playerID, protocol.StatusDNF)
		kicked = true
	}
	ended := false
	if kicked && r.allTerminalLocked() {
		r.endMatchLocked(protocol.ReasonAllFinished)
		ended = true
	}
	r.mu.Unlock()
	if ended {
		r.reg.removeIfEmpty(r.code)
	}
	return ended
}

// onFinishWindow closes a words-mode match's finish window: every still-racing
// seat is dnf'd and the match ends with reason finish_window.
func (r *Room) onFinishWindow(matchID string) {
	r.mu.Lock()
	if r.match == nil || r.match.id != matchID || r.match.ended {
		r.mu.Unlock()
		return
	}
	for _, s := range r.match.roster {
		if s.status == seatActive {
			s.status = protocol.StatusDNF
			r.broadcastPeerStatusLocked(s.playerID, protocol.StatusDNF)
		}
	}
	r.endMatchLocked(protocol.ReasonFinishWindow)
	r.mu.Unlock()
	r.reg.removeIfEmpty(r.code)
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

// allTerminalLocked reports whether every roster seat has reached a terminal
// status (finished / dnf / left) — i.e. the match is over.
func (r *Room) allTerminalLocked() bool {
	if r.match == nil {
		return false
	}
	for _, s := range r.match.roster {
		if s.status == seatActive {
			return false
		}
	}
	return true
}

// endMatchLocked ends the match: it broadcasts the single match_end frame (live
// roster seats directly; graced seats via their backlog so a resumer still
// receives it — after the caller's final peer_status, before the post-match
// room_state below), persists the match capture (off-lock), and returns the
// room to the lobby: match cleared, inMatch off, ready flags reset, dead seats
// reaped, final room_state broadcast. It assumes every roster seat is terminal.
// reason is one of the protocol.Reason* values.
func (r *Room) endMatchLocked(reason string) {
	m := r.match
	if m == nil || m.ended {
		return
	}
	m.ended = true
	if m.deadline != nil {
		m.deadline.Stop()
	}
	if m.finishWindow != nil {
		m.finishWindow.Stop()
	}
	if m.afkSweep != nil {
		m.afkSweep.Stop()
		close(m.afkStop)
		m.afkSweep = nil
	}
	endedAtMs := nowMs()

	snap := r.snapshotLocked(m)
	if r.store != nil {
		go r.persist(snap)
	}

	end := protocol.MatchEnd{Type: protocol.TypeMatchEnd, MatchID: m.id, Reason: reason}
	for _, s := range m.roster {
		status := s.status
		if status == seatActive {
			status = protocol.StatusDNF // defensive; should not happen
		}
		res := protocol.MatchResult{
			PlayerID:   s.playerID,
			Status:     status,
			BatchCount: len(s.batches),
			EventCount: s.eventCount,
		}
		if status == protocol.StatusFinished {
			at := s.finishedAtMs
			res.FinishedAtMs = &at
		}
		// AFK over THIS seat's window: go → its own finish, or go → match end for
		// everyone still racing. Measuring a finisher to the match end would
		// charge them for the time they spent waiting on the others.
		windowEnd := endedAtMs
		if status == protocol.StatusFinished && s.finishedAtMs > 0 {
			windowEnd = s.finishedAtMs
		}
		res.AfkMs, res.AfkShare = s.afkAtLocked(m.goAtMs, windowEnd)
		end.Results = append(end.Results, res)
	}
	for _, s := range m.roster {
		switch {
		case s.sess != nil:
			s.sess.trySend(end)
		case s.disconnected && r.seatIndexByIDLocked(s.playerID) >= 0:
			// A graced seat gets match_end via its backlog; a terminally
			// departed one (left / grace expired) gets nothing.
			s.backlog = append(s.backlog, end)
		}
	}

	r.match = nil
	r.inMatch = false
	for _, s := range r.seats {
		s.ready = false
		s.status = ""
		s.lastSeq = 0
		s.batches = nil
		if !s.disconnected {
			s.backlog = nil // a graced seat keeps its backlog (match_end) for resume
		}
	}
	// Reap seats whose connection is gone for good (dnf/left during the match).
	// A seat still inside its reconnect grace window survives into the lobby and
	// stays reclaimable until the grace expires.
	r.seats = slices.DeleteFunc(r.seats, func(s *seat) bool { return s.sess == nil && !s.disconnected })
	if r.findSeatByIDLocked(r.hostID) == nil {
		r.reassignHostLocked()
	}
	if len(r.seats) > 0 {
		r.broadcastStateLocked()
	}
}

// snapshotLocked copies everything persistence needs so the gzip/marshal work
// (and the store call) can happen off the room lock. The captured batch slices
// are immutable once appended, so sharing their backing arrays is safe.
func (r *Room) snapshotLocked(m *matchState) matchSnapshot {
	snap := matchSnapshot{
		id:        m.id,
		roomCode:  r.code,
		name:      m.settings.Name,
		settings:  m.settings,
		players:   m.players,
		seed:      m.seed,
		dictHash:  m.settings.DictHash,
		lang:      m.settings.Lang,
		goAtMs:    m.goAtMs,
		endedAtMs: nowMs(),
	}
	for _, s := range m.roster {
		status := s.status
		if status == seatActive {
			status = protocol.StatusDNF // defensive; should not happen
		}
		snap.runs = append(snap.runs, runSnapshot{
			playerID: s.playerID,
			nick:     s.nick,
			userID:   s.userID,
			freemods: s.freemods,
			status:   status,
			batches:  s.batches,
		})
	}
	return snap
}

type runSnapshot struct {
	playerID string
	nick     string
	userID   string
	freemods protocol.Freemods
	status   string
	batches  []CapturedBatch
}

type matchSnapshot struct {
	id        string
	roomCode  string
	name      string
	settings  protocol.Settings
	players   []protocol.CountdownPlayer
	seed      int64
	dictHash  string
	lang      string
	goAtMs    int64
	endedAtMs int64
	runs      []runSnapshot
}

// persist gzips each run's capture and writes the whole match in one store call.
// It runs off the room lock; a failure is logged (the capture is best-effort in
// v0, and the room has already returned to the lobby).
func (r *Room) persist(snap matchSnapshot) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	settingsJSON, err := json.Marshal(snap.settings)
	if err != nil {
		r.log.Error("marshal match settings", "matchId", snap.id, "err", err)
		return
	}
	freemodsJSON, err := json.Marshal(snap.players)
	if err != nil {
		r.log.Error("marshal match freemods", "matchId", snap.id, "err", err)
		return
	}
	rec := MatchRecord{
		ID:       snap.id,
		RoomCode: snap.roomCode,
		Name:     snap.name,
		Settings: settingsJSON,
		Freemods: freemodsJSON,
		Seed:     snap.seed,
		DictHash: snap.dictHash,
		Lang:     snap.lang,
		GoAt:     time.UnixMilli(snap.goAtMs),
		EndedAt:  time.UnixMilli(snap.endedAtMs),
	}
	for _, run := range snap.runs {
		logBytes, gzErr := gzipBatches(run.batches)
		if gzErr != nil {
			r.log.Error("gzip capture", "matchId", snap.id, "player", run.playerID, "err", gzErr)
			return
		}
		fmJSON, fmErr := json.Marshal(run.freemods)
		if fmErr != nil {
			r.log.Error("marshal run freemods", "matchId", snap.id, "err", fmErr)
			return
		}
		rec.Runs = append(rec.Runs, MatchRunRecord{
			PlayerID:    run.playerID,
			Nick:        run.nick,
			UserID:      run.userID,
			Freemods:    fmJSON,
			Log:         logBytes,
			BatchCount:  len(run.batches),
			FinalStatus: run.status,
		})
	}
	if err := r.store.SaveMatch(ctx, rec); err != nil {
		r.log.Error("persist match", "matchId", snap.id, "err", err)
	}
}

// gzipBatches marshals the capture to JSON and gzip-compresses it.
func gzipBatches(batches []CapturedBatch) ([]byte, error) {
	if batches == nil {
		batches = []CapturedBatch{}
	}
	raw, err := json.Marshal(batches)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
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

// systemChatLocked broadcasts a server system chat message to every live seat.
func (r *Room) systemChatLocked(kind, text string) {
	msg := protocol.Chat{
		Type: protocol.TypeChat,
		From: protocol.ChatFromSystem,
		Kind: kind,
		Text: text,
		Ts:   nowMs(),
	}
	for _, s := range r.seats {
		r.deliverLocked(s, msg)
	}
}

// errLocked sends an error frame to a single session.
func (r *Room) errLocked(sess *session, code, message string) {
	sess.trySend(protocol.NewError(code, message))
}

// matchDurationMs is a match's nominal length in ms: the configured duration for
// time modes, or a generous per-word ceiling for counted ones (words, quote).
func (r *Room) matchDurationMs(s protocol.Settings) int64 {
	if protocol.IsCounted(s.Mode) {
		return int64(s.WordCount) * r.reg.timing.wordMsPerWord
	}
	return int64(s.DurationMs)
}

// newSeed mints an unpredictable generation seed in [0, 2^32-1]. It is
// server-generated on purpose: a client-chosen seed would be a pre-practiced map.
func newSeed() int64 {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("ws: crypto/rand failed: " + err.Error())
	}
	return int64(binary.BigEndian.Uint32(b[:]))
}
