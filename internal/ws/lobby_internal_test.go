package ws

import (
	"encoding/json"
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

// TestLobbyEntryCarriesTheModesDimension pins that every listed room advertises
// how long it is. A lobby card shows a room's length, and the dimension it comes
// from is chosen by the mode: time rooms are a duration, counted rooms (words,
// quote) are a word count.
//
// Quote used to fall through both arms of that choice and be listed with
// NEITHER — a card advertising a match of unstated length. Every fixture below
// sets BOTH dimensions to distinct non-zero values, so a mode that reports the
// wrong one is as visible as a mode that reports nothing.
func TestLobbyEntryCarriesTheModesDimension(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	const (
		duration = 45_000
		words    = 77
	)
	cases := []struct {
		mode    string
		wantKey string
		wantVal int
	}{
		{protocol.ModeTime, "durationMs", duration},
		{protocol.ModeWords, "wordCount", words},
		{protocol.ModeQuote, "wordCount", words},
	}

	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			reg := NewRegistry(logger, nil)
			room := newRoom("AAA"+tc.mode[:3], reg, logger, nil)
			room.settings.Visibility = protocol.VisibilityOpen
			room.settings.Mode = tc.mode
			room.settings.DurationMs = duration
			room.settings.WordCount = words
			require.True(t, room.seat(&session{playerID: "p1", log: logger}, true))

			entry, ok := room.lobbyEntryOf()
			require.True(t, ok)

			// The wire shape is what a client sees, and `omitempty` on the two
			// pointers is what makes "absent" mean absent. Asserting the encoded
			// key set is the only way to catch the dimension going missing.
			raw, err := json.Marshal(entry.view.Settings)
			require.NoError(t, err)
			var got map[string]any
			require.NoError(t, json.Unmarshal(raw, &got))

			require.Contains(t, got, tc.wantKey,
				"a %s room is listed without its dimension: %s", tc.mode, raw)
			assert.EqualValues(t, tc.wantVal, got[tc.wantKey])

			// Exactly one of the two, never both — they are mutually exclusive
			// and a card that showed both would be describing two matches.
			other := map[string]string{"durationMs": "wordCount", "wordCount": "durationMs"}[tc.wantKey]
			assert.NotContains(t, got, other,
				"a %s room carries both dimensions: %s", tc.mode, raw)
		})
	}
}

// Every counted mode is a word-count mode. lobbyEntryOf asks IsCounted rather
// than naming modes precisely so that a mode added to that predicate is listed
// correctly without anyone remembering to come back here — this asserts the two
// stay in step.
func TestEveryCountedModeIsListedWithAWordCount(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	for _, mode := range []string{protocol.ModeTime, protocol.ModeWords, protocol.ModeQuote} {
		if !protocol.IsCounted(mode) {
			continue
		}
		reg := NewRegistry(logger, nil)
		room := newRoom("BBB"+mode[:3], reg, logger, nil)
		room.settings.Visibility = protocol.VisibilityOpen
		room.settings.Mode = mode
		room.settings.WordCount = 12
		require.True(t, room.seat(&session{playerID: "p1", log: logger}, true))

		entry, ok := room.lobbyEntryOf()
		require.True(t, ok)
		require.NotNil(t, entry.view.Settings.WordCount, "counted mode %q listed without a word count", mode)
		assert.Equal(t, 12, *entry.view.Settings.WordCount)
		assert.Nil(t, entry.view.Settings.DurationMs, "counted mode %q listed with a duration", mode)
	}
}
