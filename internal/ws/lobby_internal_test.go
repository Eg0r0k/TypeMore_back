package ws

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/typemore/typemore-server/internal/protocol"
)

// TestLobbyOrderIsTotal is the one ordering case the black-box test cannot
// reach: two rooms born in the SAME clock tick. Nothing about creation time
// separates them, so without the code tie-break their relative order would come
// from Go's randomized map iteration and an idle lobby would visibly reshuffle
// between two polls four seconds apart.
//
// White-box because the collision has to be constructed: real rooms are created
// through round-trips and never share a timestamp on purpose.
func TestLobbyOrderIsTotal(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg := NewRegistry(logger, nil)

	born := time.Now()
	// Inserted in an order that is neither ascending nor descending by code, so
	// a comparator that fell through to insertion order would be caught too.
	for _, code := range []string{"MMM222", "AAA999", "ZZZ111"} {
		room := newRoom(code, reg, logger, nil)
		room.createdAt = born
		room.settings.Visibility = protocol.VisibilityOpen
		require.True(t, room.seat(&session{playerID: "p-" + code, log: logger}, true))
		reg.rooms[code] = room
	}

	// Ten walks: map iteration order differs between them, the answer must not.
	for range 10 {
		entries := reg.openRooms()
		require.Len(t, entries, 3)
		assert.Equal(t, []string{"AAA999", "MMM222", "ZZZ111"},
			[]string{entries[0].view.Code, entries[1].view.Code, entries[2].view.Code},
			"rooms of equal population and equal age order by code")
	}
}

// TestLobbySkipsSeatlessRooms covers the teardown window: a room whose last seat
// has left but whose registry entry has not been dropped yet is observable by a
// walk that copied its pointer a moment earlier. It must be filtered, not
// published as a room with zero players.
func TestLobbySkipsSeatlessRooms(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg := NewRegistry(logger, nil)

	room := newRoom("ABC234", reg, logger, nil)
	room.settings.Visibility = protocol.VisibilityOpen
	reg.rooms[room.code] = room

	assert.Empty(t, reg.openRooms(), "a seatless room is mid-teardown, not a lobby")

	require.True(t, room.seat(&session{playerID: "p1", log: logger}, true))
	require.Len(t, reg.openRooms(), 1)
}
