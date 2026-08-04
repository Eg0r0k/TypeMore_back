package ws

import (
	"time"

	"github.com/typemore/typemore-server/internal/protocol"
)

// The idle-room reaper: a BACKSTOP, not the answer.
//
// The answer is on the client (`features/room/presence`): a tab that has been
// idle and out of focus for a quarter of an hour asks whether anybody is there
// and, unanswered, leaves the seat by the ordinary path. That check is where
// this problem belongs, because only the browser can tell "the person walked
// away" from "the host is waiting for players" — waiting produces no traffic at
// all, so no server-side rule can distinguish them without punishing the host
// who is the whole point of the public room list.
//
// What is left for the server is the case the client cannot cover: a client
// that never runs the check. An old tab from before this shipped, a script, a
// bot, a fork. For those the room would otherwise live until the process
// restarts, holding a code and a slot in the listing.
//
// So the numbers here are deliberately FAR outside anything a human session
// reaches. An hour of a room existing without a single state-changing command
// is not "idle" in any product sense — it is a room nobody is using, and the
// only reason it still exists is that somebody's socket is still open.

const (
	// roomIdleTimeout is how long a room may go without a state-changing
	// command before it is closed.
	roomIdleTimeout = time.Hour
	// roomIdleWarning is how long before that the room is told. One minute is
	// enough for anyone actually present to type something and cancel it, and
	// short enough that the warning is not forgotten before it matters.
	roomIdleWarning = time.Minute
	// roomIdleSweep is how often the registry looks. The window is an hour;
	// checking every half minute is precise to well under a percent of it and
	// costs one map walk.
	roomIdleSweep = 30 * time.Second
)

// touchLocked records that something changed the room's state.
//
// Called from the command handlers themselves rather than from the dispatch
// loop, and that is the point: what keeps a room alive is a command that DID
// something. A refused one (ready during a match, a settings update from a
// non-host) proves a client is connected, which the heartbeat already proves
// and which is exactly the state this reaper exists to end.
//
// The heartbeat is therefore NOT activity, and neither is `ntp_ping`: a clock
// sync is a client keeping its own arithmetic straight, not a person using the
// room.
func (r *Room) touchLocked() {
	r.lastActivityMs = nowMs()
	// A warning already sent is cancelled by any activity, so a room that comes
	// back to life gets a fresh hour AND a fresh warning if it goes quiet again.
	r.idleWarned = false
}

// idleStateLocked reports what the reaper should do about this room right now.
func (r *Room) idleStateLocked(now int64) (warn, close bool) {
	// A running match is never idle by this measure: the match has its own
	// deadline and its own AFK rules, and both are stricter than an hour.
	if r.inMatch {
		return false, false
	}
	idle := now - r.lastActivityMs
	switch {
	case idle >= roomIdleTimeout.Milliseconds():
		return false, true
	case idle >= (roomIdleTimeout - roomIdleWarning).Milliseconds() && !r.idleWarned:
		return true, false
	default:
		return false, false
	}
}

// reapIdleRooms is one sweep: warn the rooms about to go, close the ones whose
// hour is up.
//
// Closing is the EXISTING departure path, seat by seat — the same thing every
// occupant leaving would do — so the room empties, the account index is kept
// honest, and `removeIfEmpty` drops it exactly as it drops any room whose last
// seat left. Nothing here is a second way to destroy a room.
func (reg *Registry) reapIdleRooms(now int64) {
	reg.mu.Lock()
	rooms := make([]*Room, 0, len(reg.rooms))
	for _, room := range reg.rooms {
		rooms = append(rooms, room)
	}
	reg.mu.Unlock()

	var emptied []string
	for _, room := range rooms {
		room.mu.Lock()
		warn, close := room.idleStateLocked(now)
		switch {
		case warn:
			room.idleWarned = true
			room.systemChatLocked(protocol.ChatKindLeave,
				"this room has been idle for an hour and will close in a minute")
		case close:
			for len(room.seats) > 0 {
				// leaveSeatLocked is what a voluntary departure runs; the last
				// one leaves the room empty and the sweep below drops it.
				room.leaveSeatLocked(room.seats[0])
			}
			emptied = append(emptied, room.code)
		}
		room.mu.Unlock()
	}

	for _, code := range emptied {
		reg.removeIfEmpty(code)
		reg.log.Info("room closed: idle", "code", code)
	}
}

// runIdleReaper sweeps until ctx is done. Started by the handler that owns the
// registry, so it dies with the server rather than outliving it.
func (reg *Registry) runIdleReaper(stop <-chan struct{}) {
	ticker := time.NewTicker(roomIdleSweep)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			reg.reapIdleRooms(nowMs())
		}
	}
}
