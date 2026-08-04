package ws

// The idle-room reaper (room_idle.go), driven directly rather than through a
// socket: the whole behaviour is "how long since a command CHANGED this room",
// and the sweep is a pure function of that clock. An hour is not a thing a
// wire-level test can wait for, and shrinking the constant would test a
// different number from the one that ships.
//
// This is an internal test on purpose — the thing under test is the predicate,
// and exposing it just to assert it from the outside would widen the package's
// surface for a rule that has no business being public.

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/typemore/typemore-server/internal/protocol"
)

func idleTestRegistry(t *testing.T) *Registry {
	t.Helper()
	return NewRegistry(slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
}

// A room with one seat, seated the ordinary way so every field the reaper
// reads is in the state a real room has.
func idleTestRoom(t *testing.T, reg *Registry) (*Room, *session) {
	t.Helper()
	room := newRoom("IDLE01", reg, reg.log, nil)
	reg.mu.Lock()
	reg.rooms[room.code] = room
	reg.mu.Unlock()
	sess := &session{playerID: "p1", outbound: make(chan outFrame, 64)}
	require.True(t, room.seat(sess, true))
	return room, sess
}

func TestIdleRoomIsWarnedThenClosed(t *testing.T) {
	reg := idleTestRegistry(t)
	room, _ := idleTestRoom(t, reg)

	now := nowMs()

	// Fresh: nothing to do.
	reg.reapIdleRooms(now)
	assert.False(t, room.idleWarned)
	assert.Len(t, room.seats, 1)

	// A minute before the hour: the warning, once.
	warnAt := now + (roomIdleTimeout - roomIdleWarning).Milliseconds()
	reg.reapIdleRooms(warnAt)
	assert.True(t, room.idleWarned, "the room is told a minute before it closes")
	require.Len(t, room.seats, 1, "warning does not close anything")

	// Sweeping again inside the warning window must not repeat it.
	reg.reapIdleRooms(warnAt + 1000)
	assert.Len(t, room.seats, 1)

	// The hour: every seat leaves by the ordinary path and the room is dropped
	// from the registry by the same removeIfEmpty every departure runs.
	reg.reapIdleRooms(now + roomIdleTimeout.Milliseconds())
	assert.Empty(t, room.seats)
	reg.mu.Lock()
	_, still := reg.rooms[room.code]
	reg.mu.Unlock()
	assert.False(t, still, "an emptied room leaves the registry")
}

// The point of the whole design: a HEARTBEAT is not activity. A tab that is
// merely connected is exactly the state this reaper exists to end, and the
// ping/pong the transport runs every 15 s must never look like a person.
func TestHeartbeatIsNotActivity(t *testing.T) {
	reg := idleTestRegistry(t)
	room, sess := idleTestRoom(t, reg)

	now := nowMs()
	// Whatever the transport does on its own — pings, pongs, an ntp_ping — none
	// of it reaches touchLocked, so the clock keeps running. Simulated here by
	// doing nothing at all for an hour, which is what a heartbeat amounts to
	// from the room's point of view.
	reg.reapIdleRooms(now + roomIdleTimeout.Milliseconds())
	assert.Empty(t, room.seats, "a connected-but-silent room still closes")

	// And a REFUSED command is not activity either: it proves a client is
	// connected, which the heartbeat already proves.
	room2, _ := idleTestRoom(t, reg)
	room2.code = "IDLE02"
	reg.mu.Lock()
	reg.rooms[room2.code] = room2
	reg.mu.Unlock()
	room2.lastActivityMs = now
	// A non-host settings update: refused, and it must leave the clock alone.
	other := &session{playerID: "p2", outbound: make(chan outFrame, 64)}
	room2.updateSettings(other, protocol.DefaultSettings("nope"))
	assert.Equal(t, now, room2.lastActivityMs, "a refused command is not activity")

	_ = sess
}

// Any command that DID something resets the clock, and a room that comes back
// to life gets a fresh warning if it goes quiet again.
func TestActivityResetsTheIdleClock(t *testing.T) {
	reg := idleTestRegistry(t)
	room, sess := idleTestRoom(t, reg)

	// Both rooms are put an hour past their last activity: overdue, and about
	// to be closed by the next sweep.
	overdue := nowMs() - roomIdleTimeout.Milliseconds()
	room.lastActivityMs = overdue
	room.idleWarned = true

	control := newRoom("IDLE0C", reg, reg.log, nil)
	reg.mu.Lock()
	reg.rooms[control.code] = control
	reg.mu.Unlock()
	require.True(t, control.seat(&session{playerID: "c1", outbound: make(chan outFrame, 64)}, true))
	control.lastActivityMs = overdue

	// One of them speaks. The CONTROL is what makes this evidence: the two rooms
	// differ in exactly one thing, and the sweep is the same sweep.
	room.chat(sess, "still here")
	assert.False(t, room.idleWarned, "activity cancels a pending warning")
	assert.Greater(t, room.lastActivityMs, overdue, "the clock moved to the chat")

	reg.reapIdleRooms(nowMs())
	assert.Empty(t, control.seats, "the room that said nothing is closed")
	assert.Len(t, room.seats, 1, "the room that spoke keeps its seats")
}

// A running match is never idle by this measure: it has a deadline and AFK
// rules of its own, both far stricter than an hour, and closing a room out from
// under a match would destroy a capture.
func TestAMatchIsNeverIdle(t *testing.T) {
	reg := idleTestRegistry(t)
	room, _ := idleTestRoom(t, reg)

	room.mu.Lock()
	room.inMatch = true
	room.mu.Unlock()

	reg.reapIdleRooms(nowMs() + roomIdleTimeout.Milliseconds()*2)
	assert.Len(t, room.seats, 1, "a room in a match is not reaped")
}

// The sweep is stopped by the handler that owns it.
func TestReaperStopsWithTheHandler(t *testing.T) {
	h := NewHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil, nil)
	h.Close()
	h.Close() // idempotent
	// Nothing to assert beyond "this returns and does not panic": the goroutine
	// is gone, and a leaked ticker would show up as a failing -race/goleak run.
	time.Sleep(time.Millisecond)
}
