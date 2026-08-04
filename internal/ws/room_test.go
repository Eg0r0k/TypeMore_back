package ws_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/typemore/typemore-server/internal/protocol"
	"github.com/typemore/typemore-server/internal/ws"
)

// roomCapacity mirrors the server room capacity (docs/PROTOCOL.md §5) for tests.
const roomCapacity = 5

// roomTestServer wires the /ws endpoint with an identity resolver that treats
// the "X-Test-User" header as the authenticated displayName (empty ⇒ guest),
// standing in for the session-cookie lookup a real upgrade performs.
func roomTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := chi.NewRouter()
	r.Handle("/ws", ws.NewHandler(logger, nil, func(req *http.Request) (string, string, bool) {
		if name := req.Header.Get("X-Test-User"); name != "" {
			return name, "", true
		}
		return "", "", false
	}, nil))
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}

// dialAs opens a WebSocket client. A non-empty name authenticates the connection
// (via the X-Test-User header); an empty name is a guest.
func dialAs(t *testing.T, ctx context.Context, srv *httptest.Server, name string) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
	var opts *websocket.DialOptions
	if name != "" {
		opts = &websocket.DialOptions{HTTPHeader: http.Header{"X-Test-User": []string{name}}}
	}
	c, _, err := websocket.Dial(ctx, url, opts)
	require.NoError(t, err, "dial %s", url)
	t.Cleanup(func() { _ = c.Close(websocket.StatusNormalClosure, "") })
	return c
}

// expect reads the next frame and asserts its discriminator, returning raw bytes.
// It is used where the exact next frame is deterministic (the server serializes
// every broadcast under the room lock).
func expect(t *testing.T, ctx context.Context, c *websocket.Conn, typ string) []byte {
	t.Helper()
	gotType, data := readFrame(t, ctx, c)
	require.Equal(t, typ, gotType, "unexpected frame; want %s", typ)
	return data
}

// readUntil reads frames until one matches typ, returning its raw bytes. Use it
// only when the target type is distinct from any stale frames still queued.
func readUntil(t *testing.T, ctx context.Context, c *websocket.Conn, typ string) []byte {
	t.Helper()
	for range 30 {
		gotType, data := readFrame(t, ctx, c)
		if gotType == typ {
			return data
		}
	}
	t.Fatalf("did not receive a %q frame", typ)
	return nil
}

func decodeRoomState(t *testing.T, data []byte) protocol.RoomState {
	t.Helper()
	var s protocol.RoomState
	require.NoError(t, json.Unmarshal(data, &s))
	return s
}

// hostRoom completes hello, creates a room, and returns the host player id plus
// the initial room_state. The host's queue is empty on return.
func hostRoom(t *testing.T, ctx context.Context, c *websocket.Conn) (string, protocol.RoomState) {
	t.Helper()
	id := doHello(t, ctx, c, "host")
	writeJSON(t, ctx, c, protocol.CreateRoom{Type: protocol.TypeCreateRoom})
	st := decodeRoomState(t, expect(t, ctx, c, protocol.TypeRoomState))
	return id, st
}

// joinRoom completes hello, joins code, and consumes the join frames on both the
// joiner (room_state + join chat) and every already-seated member passed in, so
// all their queues are empty on return.
func joinRoom(t *testing.T, ctx context.Context, joiner *websocket.Conn, code string, members ...*websocket.Conn) (string, protocol.RoomState) {
	t.Helper()
	id := doHello(t, ctx, joiner, "guest")
	writeJSON(t, ctx, joiner, protocol.JoinRoom{Type: protocol.TypeJoinRoom, Code: code})
	st := decodeRoomState(t, expect(t, ctx, joiner, protocol.TypeRoomState))
	expect(t, ctx, joiner, protocol.TypeChat) // the joiner's own "joined" system chat
	for _, m := range members {
		expect(t, ctx, m, protocol.TypeRoomState)
		expect(t, ctx, m, protocol.TypeChat)
	}
	return id, st
}

func playerByID(st protocol.RoomState, id string) (protocol.Player, bool) {
	for _, p := range st.Players {
		if p.PlayerID == id {
			return p, true
		}
	}
	return protocol.Player{}, false
}

func decodeErr(t *testing.T, data []byte) protocol.Error {
	t.Helper()
	var e protocol.Error
	require.NoError(t, json.Unmarshal(data, &e))
	return e
}

// TestGuestNickUniqueness fills a room with guests and asserts every seat gets a
// distinct "Guest-XXXX" identity.
func TestGuestNickUniqueness(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	srv := roomTestServer(t)

	host := dialAs(t, ctx, srv, "")
	_, hs := hostRoom(t, ctx, host)
	members := []*websocket.Conn{host}

	var last protocol.RoomState
	for range roomCapacity - 1 {
		g := dialAs(t, ctx, srv, "")
		_, last = joinRoom(t, ctx, g, hs.Code, members...)
		members = append(members, g)
	}

	require.Len(t, last.Players, roomCapacity)
	guestRe := regexp.MustCompile(`^Guest-\d{4}$`)
	seen := map[string]bool{}
	for _, p := range last.Players {
		assert.True(t, p.IsGuest, "player %s should be a guest", p.PlayerID)
		assert.Regexp(t, guestRe, p.Nick)
		assert.False(t, seen[p.Nick], "duplicate guest nick %q", p.Nick)
		seen[p.Nick] = true
	}
}

// TestAuthedNickFromSession asserts an authenticated connection uses its account
// displayName, while a guest in the same room gets Guest-XXXX.
func TestAuthedNickFromSession(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	srv := roomTestServer(t)

	host := dialAs(t, ctx, srv, "Neo")
	hostID, st := hostRoom(t, ctx, host)
	hp, ok := playerByID(st, hostID)
	require.True(t, ok)
	assert.Equal(t, "Neo", hp.Nick)
	assert.False(t, hp.IsGuest)

	guest := dialAs(t, ctx, srv, "")
	guestID, gst := joinRoom(t, ctx, guest, st.Code, host)
	gp, ok := playerByID(gst, guestID)
	require.True(t, ok)
	assert.True(t, gp.IsGuest)
	assert.Regexp(t, `^Guest-\d{4}$`, gp.Nick)
}

// TestStartMatchGating covers the start_match validity table: too few seats and
// an unready seat both reject with not_ready, a non-host caller is forbidden, and
// the ready two-seat case yields a countdown for everyone.
func TestStartMatchGating(t *testing.T) {
	t.Run("too few players", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		srv := roomTestServer(t)
		host := dialAs(t, ctx, srv, "")
		hostRoom(t, ctx, host)
		writeJSON(t, ctx, host, protocol.StartMatch{Type: protocol.TypeStartMatch})
		assert.Equal(t, protocol.CodeNotReady, decodeErr(t, expect(t, ctx, host, protocol.TypeError)).Code)
	})

	t.Run("not all ready", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		srv := roomTestServer(t)
		host := dialAs(t, ctx, srv, "")
		_, hs := hostRoom(t, ctx, host)
		guest := dialAs(t, ctx, srv, "")
		joinRoom(t, ctx, guest, hs.Code, host)
		writeJSON(t, ctx, host, protocol.StartMatch{Type: protocol.TypeStartMatch})
		assert.Equal(t, protocol.CodeNotReady, decodeErr(t, expect(t, ctx, host, protocol.TypeError)).Code)
	})

	t.Run("force waives readiness but not the seat count", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		srv := roomTestServer(t)

		// Alone, force is still not a match: the seat floor holds.
		solo := dialAs(t, ctx, srv, "")
		hostRoom(t, ctx, solo)
		writeJSON(t, ctx, solo, protocol.StartMatch{Type: protocol.TypeStartMatch, Force: true})
		assert.Equal(t, protocol.CodeNotReady, decodeErr(t, expect(t, ctx, solo, protocol.TypeError)).Code)

		// Two seats, guest NOT ready: plain start refuses, force yields a
		// countdown that seats BOTH players — nobody is carved out.
		host := dialAs(t, ctx, srv, "")
		_, hs := hostRoom(t, ctx, host)
		guest := dialAs(t, ctx, srv, "")
		joinRoom(t, ctx, guest, hs.Code, host)
		writeJSON(t, ctx, host, protocol.StartMatch{Type: protocol.TypeStartMatch})
		assert.Equal(t, protocol.CodeNotReady, decodeErr(t, expect(t, ctx, host, protocol.TypeError)).Code)
		writeJSON(t, ctx, host, protocol.StartMatch{Type: protocol.TypeStartMatch, Force: true})
		for _, c := range []*websocket.Conn{host, guest} {
			var cd protocol.Countdown
			require.NoError(t, json.Unmarshal(expect(t, ctx, c, protocol.TypeCountdown), &cd))
			assert.Len(t, cd.Players, 2)
		}
	})

	t.Run("non-host cannot start", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		srv := roomTestServer(t)
		host := dialAs(t, ctx, srv, "")
		_, hs := hostRoom(t, ctx, host)
		guest := dialAs(t, ctx, srv, "")
		joinRoom(t, ctx, guest, hs.Code, host)
		writeJSON(t, ctx, guest, protocol.Ready{Type: protocol.TypeReady})
		expect(t, ctx, host, protocol.TypeRoomState)
		expect(t, ctx, guest, protocol.TypeRoomState)
		writeJSON(t, ctx, guest, protocol.StartMatch{Type: protocol.TypeStartMatch})
		assert.Equal(t, protocol.CodeForbidden, decodeErr(t, expect(t, ctx, guest, protocol.TypeError)).Code)
	})

	t.Run("happy path yields countdown", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		srv := roomTestServer(t)
		host := dialAs(t, ctx, srv, "")
		_, hs := hostRoom(t, ctx, host)
		guest := dialAs(t, ctx, srv, "")
		joinRoom(t, ctx, guest, hs.Code, host)
		writeJSON(t, ctx, guest, protocol.Ready{Type: protocol.TypeReady})
		expect(t, ctx, host, protocol.TypeRoomState)
		expect(t, ctx, guest, protocol.TypeRoomState)
		writeJSON(t, ctx, host, protocol.StartMatch{Type: protocol.TypeStartMatch})

		for _, c := range []*websocket.Conn{host, guest} {
			var cd protocol.Countdown
			require.NoError(t, json.Unmarshal(expect(t, ctx, c, protocol.TypeCountdown), &cd))
			assert.Greater(t, cd.GoAtServerMs, time.Now().UnixMilli())
			assert.GreaterOrEqual(t, cd.Seed, int64(0))
			assert.LessOrEqual(t, cd.Seed, int64(1)<<32-1)
			assert.Len(t, cd.Players, 2)
			assert.Equal(t, protocol.TextSourceSeeded, cd.Settings.TextSource.Kind)
		}
	})
}

// TestFreemodSnapshotFrozen asserts a seat's freemods are captured in the
// countdown and that a set_freemods arriving during the match is rejected.
func TestFreemodSnapshotFrozen(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	srv := roomTestServer(t)

	host := dialAs(t, ctx, srv, "")
	_, hs := hostRoom(t, ctx, host)
	guest := dialAs(t, ctx, srv, "")
	guestID, _ := joinRoom(t, ctx, guest, hs.Code, host)

	want := protocol.Freemods{Difficulty: protocol.DifficultyExpert, MinWpm: 60, Nospace: true}
	writeJSON(t, ctx, guest, protocol.SetFreemods{Type: protocol.TypeSetFreemods, Freemods: want})
	expect(t, ctx, host, protocol.TypeRoomState)
	expect(t, ctx, guest, protocol.TypeRoomState)
	writeJSON(t, ctx, guest, protocol.Ready{Type: protocol.TypeReady})
	expect(t, ctx, host, protocol.TypeRoomState)
	expect(t, ctx, guest, protocol.TypeRoomState)

	writeJSON(t, ctx, host, protocol.StartMatch{Type: protocol.TypeStartMatch})
	var cd protocol.Countdown
	require.NoError(t, json.Unmarshal(expect(t, ctx, guest, protocol.TypeCountdown), &cd))

	var frozen protocol.Freemods
	found := false
	for _, p := range cd.Players {
		if p.PlayerID == guestID {
			frozen, found = p.Freemods, true
		}
	}
	require.True(t, found)
	assert.Equal(t, want, frozen)

	// A late set_freemods during the match is rejected.
	writeJSON(t, ctx, guest, protocol.SetFreemods{Type: protocol.TypeSetFreemods,
		Freemods: protocol.Freemods{Difficulty: protocol.DifficultyMaster}})
	assert.Equal(t, protocol.CodeBadMessage, decodeErr(t, expect(t, ctx, guest, protocol.TypeError)).Code)
}

// TestKickFlow asserts the kicked client gets a Kicked frame, the seat is freed
// in the remaining room_state, and the neutral "left" system chat is broadcast.
func TestKickFlow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	srv := roomTestServer(t)

	host := dialAs(t, ctx, srv, "")
	hostID, hs := hostRoom(t, ctx, host)
	guest := dialAs(t, ctx, srv, "")
	guestID, _ := joinRoom(t, ctx, guest, hs.Code, host)

	writeJSON(t, ctx, host, protocol.Kick{Type: protocol.TypeKick, PlayerID: guestID})

	// Kicked client is notified.
	expect(t, ctx, guest, protocol.TypeKicked)

	// Remaining host sees the freed seat and a neutral system chat.
	st := decodeRoomState(t, expect(t, ctx, host, protocol.TypeRoomState))
	assert.Len(t, st.Players, 1)
	_, stillThere := playerByID(st, guestID)
	assert.False(t, stillThere)
	assert.Equal(t, hostID, st.HostPlayerID)

	var chat protocol.Chat
	require.NoError(t, json.Unmarshal(expect(t, ctx, host, protocol.TypeChat), &chat))
	assert.Equal(t, protocol.ChatFromSystem, chat.From)
	assert.Equal(t, protocol.ChatKindLeave, chat.Kind)
	assert.Contains(t, chat.Text, "left")
	assert.NotContains(t, strings.ToLower(chat.Text), "kick")
}

// TestHostTransferManual asserts transfer_host reassigns the host role.
func TestHostTransferManual(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	srv := roomTestServer(t)

	host := dialAs(t, ctx, srv, "")
	_, hs := hostRoom(t, ctx, host)
	guest := dialAs(t, ctx, srv, "")
	guestID, _ := joinRoom(t, ctx, guest, hs.Code, host)

	writeJSON(t, ctx, host, protocol.TransferHost{Type: protocol.TypeTransferHost, PlayerID: guestID})
	st := decodeRoomState(t, expect(t, ctx, guest, protocol.TypeRoomState))
	assert.Equal(t, guestID, st.HostPlayerID)
	chat := protocol.Chat{}
	require.NoError(t, json.Unmarshal(expect(t, ctx, guest, protocol.TypeChat), &chat))
	assert.Equal(t, protocol.ChatKindHostChanged, chat.Kind)
}

// TestHostTransferAuto asserts the host role passes to the earliest-joined
// remaining seat when the host leaves.
func TestHostTransferAuto(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	srv := roomTestServer(t)

	host := dialAs(t, ctx, srv, "")
	_, hs := hostRoom(t, ctx, host)
	guest := dialAs(t, ctx, srv, "")
	guestID, _ := joinRoom(t, ctx, guest, hs.Code, host)

	writeJSON(t, ctx, host, protocol.Leave{Type: protocol.TypeLeave})
	st := decodeRoomState(t, expect(t, ctx, guest, protocol.TypeRoomState))
	assert.Len(t, st.Players, 1)
	assert.Equal(t, guestID, st.HostPlayerID)
}

// TestSettingsUpdateResetsReady asserts a host settings_update applies and clears
// every seat's ready flag, and that a non-host update is forbidden.
func TestSettingsUpdateResetsReady(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	srv := roomTestServer(t)

	host := dialAs(t, ctx, srv, "")
	_, hs := hostRoom(t, ctx, host)
	guest := dialAs(t, ctx, srv, "")
	guestID, _ := joinRoom(t, ctx, guest, hs.Code, host)

	writeJSON(t, ctx, guest, protocol.Ready{Type: protocol.TypeReady})
	expect(t, ctx, host, protocol.TypeRoomState)
	expect(t, ctx, guest, protocol.TypeRoomState)

	// Non-host update is forbidden.
	writeJSON(t, ctx, guest, protocol.SettingsUpdate{Type: protocol.TypeSettingsUpdate, Settings: protocol.DefaultSettings("X")})
	assert.Equal(t, protocol.CodeForbidden, decodeErr(t, expect(t, ctx, guest, protocol.TypeError)).Code)

	// Host update applies and resets ready flags.
	ns := protocol.DefaultSettings("Words Room")
	ns.Mode = protocol.ModeWords
	ns.DurationMs = 0
	ns.WordCount = 50
	writeJSON(t, ctx, host, protocol.SettingsUpdate{Type: protocol.TypeSettingsUpdate, Settings: ns})
	st := decodeRoomState(t, expect(t, ctx, host, protocol.TypeRoomState))
	assert.Equal(t, protocol.ModeWords, st.Settings.Mode)
	assert.Equal(t, 50, st.Settings.WordCount)
	gp, ok := playerByID(st, guestID)
	require.True(t, ok)
	assert.False(t, gp.Ready, "ready flags must reset on settings change")
}

// TestChatRateLimit asserts the burst of 5 messages passes and the 6th is
// rejected with rate_limited.
func TestChatRateLimit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	srv := roomTestServer(t)

	host := dialAs(t, ctx, srv, "")
	_, hs := hostRoom(t, ctx, host)
	guest := dialAs(t, ctx, srv, "")
	joinRoom(t, ctx, guest, hs.Code, host)

	for range 6 {
		writeJSON(t, ctx, guest, protocol.ChatSend{Type: protocol.TypeChatSend, Text: "hi"})
	}

	chats, gotLimit := 0, false
	for range 7 {
		typ, data := readFrame(t, ctx, guest)
		switch typ {
		case protocol.TypeChat:
			chats++
		case protocol.TypeError:
			assert.Equal(t, protocol.CodeRateLimited, decodeErr(t, data).Code)
			gotLimit = true
		}
		if gotLimit {
			break
		}
	}
	assert.Equal(t, 5, chats, "burst of chats before the limit")
	assert.True(t, gotLimit, "6th message must be rate limited")
}

// TestChatRateLimitSurvivesRejoin is the regression for a limiter that bounded
// nothing: the chat bucket used to live on the SEAT, and a seat is minted fresh
// by every join — so leave + join_room handed the sender a full burst again and
// the sustained chat rate was unbounded. It lives on the connection now.
func TestChatRateLimitSurvivesRejoin(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	srv := roomTestServer(t)

	host := dialAs(t, ctx, srv, "")
	_, hs := hostRoom(t, ctx, host)
	guest := dialAs(t, ctx, srv, "")
	joinRoom(t, ctx, guest, hs.Code, host)

	// Spend the burst.
	for range 6 {
		writeJSON(t, ctx, guest, protocol.ChatSend{Type: protocol.TypeChatSend, Text: "hi"})
	}
	assert.Equal(t, protocol.CodeRateLimited,
		decodeErr(t, readUntil(t, ctx, guest, protocol.TypeError)).Code)

	// The old reset: drop the seat and take a new one on the same connection.
	writeJSON(t, ctx, guest, protocol.Leave{Type: protocol.TypeLeave})
	writeJSON(t, ctx, guest, protocol.JoinRoom{Type: protocol.TypeJoinRoom, Code: hs.Code})
	readUntil(t, ctx, guest, protocol.TypeRoomState)

	// A fresh seat, the same budget: still refused.
	writeJSON(t, ctx, guest, protocol.ChatSend{Type: protocol.TypeChatSend, Text: "again"})
	assert.Equal(t, protocol.CodeRateLimited,
		decodeErr(t, readUntil(t, ctx, guest, protocol.TypeError)).Code)
}

// TestCommandRateLimit asserts the per-connection command budget refuses a
// flood of room-mutating frames. `ready` is the probe: it used to be the
// cheapest broadcast amplifier on the wire (one room_state, marshalled per
// seat, per inbound frame).
func TestCommandRateLimit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	srv := roomTestServer(t)

	host := dialAs(t, ctx, srv, "")
	_, hs := hostRoom(t, ctx, host)
	guest := dialAs(t, ctx, srv, "")
	joinRoom(t, ctx, guest, hs.Code, host)

	// Well past the burst; alternating so no frame is dropped as a no-op.
	for i := range 40 {
		ready := i%2 == 0
		writeJSON(t, ctx, guest, protocol.Ready{Type: protocol.TypeReady, Ready: &ready})
	}
	assert.Equal(t, protocol.CodeRateLimited,
		decodeErr(t, readUntil(t, ctx, guest, protocol.TypeError)).Code)
}

// TestLobbyOnlyFramesAreRefusedDuringAMatch pins the two frames that used to be
// accepted mid-match. Neither decides anything once the countdown has frozen the
// roster, and both broadcast to every seat — `transfer_host` twice over.
func TestLobbyOnlyFramesAreRefusedDuringAMatch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	srv := roomTestServer(t)
	m := startMatch(t, ctx, srv, 2, 30_000)

	writeJSON(t, ctx, m.conns[1], protocol.Ready{Type: protocol.TypeReady})
	assert.Equal(t, protocol.CodeBadMessage,
		decodeErr(t, readUntil(t, ctx, m.conns[1], protocol.TypeError)).Code)

	writeJSON(t, ctx, m.conns[0], protocol.TransferHost{
		Type: protocol.TypeTransferHost, PlayerID: m.ids[1],
	})
	assert.Equal(t, protocol.CodeBadMessage,
		decodeErr(t, readUntil(t, ctx, m.conns[0], protocol.TypeError)).Code)
}

// TestChatBroadcast asserts a valid chat is broadcast to every seat with the
// sender's playerId (trimmed), and an over-long message is rejected.
func TestChatBroadcast(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	srv := roomTestServer(t)

	host := dialAs(t, ctx, srv, "")
	_, hs := hostRoom(t, ctx, host)
	guest := dialAs(t, ctx, srv, "")
	guestID, _ := joinRoom(t, ctx, guest, hs.Code, host)

	writeJSON(t, ctx, guest, protocol.ChatSend{Type: protocol.TypeChatSend, Text: "  hello world  "})
	var chat protocol.Chat
	require.NoError(t, json.Unmarshal(expect(t, ctx, host, protocol.TypeChat), &chat))
	assert.Equal(t, guestID, chat.From)
	assert.Equal(t, "hello world", chat.Text)
	assert.NotZero(t, chat.Ts)

	writeJSON(t, ctx, guest, protocol.ChatSend{Type: protocol.TypeChatSend, Text: strings.Repeat("x", 201)})
	// The guest's own broadcast of the first message is still queued; the error
	// is a distinct frame type, so skip past it.
	assert.Equal(t, protocol.CodeBadMessage, decodeErr(t, readUntil(t, ctx, guest, protocol.TypeError)).Code)
}

// TestRoomScopedRequiresRoom asserts room-scoped messages error with not_in_room
// before the sender has joined a room.
func TestRoomScopedRequiresRoom(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	srv := roomTestServer(t)

	c := dialAs(t, ctx, srv, "")
	doHello(t, ctx, c, "lonely")
	writeJSON(t, ctx, c, protocol.Ready{Type: protocol.TypeReady})
	assert.Equal(t, protocol.CodeNotInRoom, decodeErr(t, expect(t, ctx, c, protocol.TypeError)).Code)
}

// TestJoinUnknownRoom asserts joining a nonexistent code errors with
// room_not_found.
func TestJoinUnknownRoom(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	srv := roomTestServer(t)

	c := dialAs(t, ctx, srv, "")
	doHello(t, ctx, c, "seeker")
	writeJSON(t, ctx, c, protocol.JoinRoom{Type: protocol.TypeJoinRoom, Code: "ZZZZZZ"})
	assert.Equal(t, protocol.CodeRoomNotFound, decodeErr(t, expect(t, ctx, c, protocol.TypeError)).Code)
}

// TestReadyToggle asserts `ready` marks the sending seat ready and an explicit
// ready:false clears it again, each change broadcasting room_state.
func TestReadyToggle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	srv := roomTestServer(t)

	host := dialAs(t, ctx, srv, "")
	_, hs := hostRoom(t, ctx, host)
	guest := dialAs(t, ctx, srv, "")
	guestID, _ := joinRoom(t, ctx, guest, hs.Code, host)

	// Bare ready (no field) sets the flag.
	writeJSON(t, ctx, guest, protocol.Ready{Type: protocol.TypeReady})
	st := decodeRoomState(t, expect(t, ctx, guest, protocol.TypeRoomState))
	assert.True(t, playerReady(st, guestID))
	st = decodeRoomState(t, expect(t, ctx, host, protocol.TypeRoomState))
	assert.True(t, playerReady(st, guestID))

	// An explicit ready:false clears it.
	unready := false
	writeJSON(t, ctx, guest, protocol.Ready{Type: protocol.TypeReady, Ready: &unready})
	st = decodeRoomState(t, expect(t, ctx, guest, protocol.TypeRoomState))
	assert.False(t, playerReady(st, guestID))
	st = decodeRoomState(t, expect(t, ctx, host, protocol.TypeRoomState))
	assert.False(t, playerReady(st, guestID))
}
