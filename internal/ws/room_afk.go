package ws

import (
	"time"

	"github.com/typemore/typemore-server/internal/protocol"
)

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
