package ws

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"slices"
	"time"

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
