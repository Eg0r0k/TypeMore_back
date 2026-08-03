package ws_test

// One seat per account (docs/PROTOCOL.md §5).
//
// The rule is enforced in the registry, so every test here ends with
// CheckUserIndex: the behaviour on the wire can look right while the account
// index has quietly drifted, and a drifted index is the rule silently switching
// itself off for the next connection.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/typemore/typemore-server/internal/protocol"
	"github.com/typemore/typemore-server/internal/ws"
)

// closeSeatTakenOver mirrors the server's private-range close code for a
// displaced connection (internal/ws/handler.go).
const closeSeatTakenOver websocket.StatusCode = 4001

// acctServer wires /ws with an identity resolver keyed on the X-Test-Uid header
// (the account id) plus X-Test-User (the displayName), and hands back the
// handler so a test can assert the registry's internal invariants.
func acctServer(t *testing.T, store ws.MatchStore, opts ...ws.Option) (*httptest.Server, *ws.Handler) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := ws.NewHandler(logger, nil, func(req *http.Request) (string, string, bool) {
		if uid := req.Header.Get("X-Test-Uid"); uid != "" {
			return req.Header.Get("X-Test-User"), uid, true
		}
		return "", "", false
	}, store, opts...)
	r := chi.NewRouter()
	r.Handle("/ws", h)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv, h
}

// dialAcct opens a connection authenticated as the account uid with display name
// name. An empty uid is a guest.
func dialAcct(t *testing.T, ctx context.Context, srv *httptest.Server, name, uid string) *websocket.Conn {
	t.Helper()
	url := "ws" + trimScheme(srv.URL) + "/ws"
	opts := &websocket.DialOptions{HTTPHeader: http.Header{}}
	if uid != "" {
		opts.HTTPHeader.Set("X-Test-Uid", uid)
		opts.HTTPHeader.Set("X-Test-User", name)
	}
	c, _, err := websocket.Dial(ctx, url, opts)
	require.NoError(t, err, "dial %s", url)
	t.Cleanup(func() { _ = c.Close(websocket.StatusNormalClosure, "") })
	return c
}

func trimScheme(u string) string {
	return u[len("http"):]
}

// requireIndexOK asserts the account index still agrees with the seats that
// exist. Call it at a quiescent point (every frame the scenario produces already
// consumed), never mid-operation.
func requireIndexOK(t *testing.T, h *ws.Handler) {
	t.Helper()
	require.NoError(t, h.CheckUserIndex())
}

// expectDisplaced asserts a connection was displaced: the in-band
// seat_taken_over error first, then a close with code 4001. Both halves matter
// — the frame is what a client shows the person, the code is what stops it
// reconnecting straight back into the seat it just lost.
func expectDisplaced(t *testing.T, ctx context.Context, c *websocket.Conn) {
	t.Helper()
	e := decodeErr(t, expect(t, ctx, c, protocol.TypeError))
	assert.Equal(t, protocol.CodeSeatTakenOver, e.Code)

	_, _, err := c.Read(ctx)
	require.Error(t, err, "a displaced connection must be closed by the server")
	assert.Equal(t, closeSeatTakenOver, websocket.CloseStatus(err))
}

// acctHello completes the handshake and returns (playerID, resumeToken).
func acctHello(t *testing.T, ctx context.Context, c *websocket.Conn) (string, string) {
	t.Helper()
	writeJSON(t, ctx, c, protocol.Hello{Type: protocol.TypeHello, ProtocolVersion: protocol.Version})
	var ok protocol.HelloOK
	require.NoError(t, json.Unmarshal(expect(t, ctx, c, protocol.TypeHelloOK), &ok))
	return ok.PlayerID, ok.ResumeToken
}

// TestSecondConnectionTakesTheSeat is the headline case: the same account opens
// the room twice. The seat does not multiply — the newer connection inherits it
// (player id and resume token included) and the older one is displaced.
func TestSecondConnectionTakesTheSeat(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	srv, h := acctServer(t, nil)

	first := dialAcct(t, ctx, srv, "alice", "u-alice")
	firstID, firstTok := acctHello(t, ctx, first)
	writeJSON(t, ctx, first, protocol.CreateRoom{Type: protocol.TypeCreateRoom})
	room := decodeRoomState(t, expect(t, ctx, first, protocol.TypeRoomState))

	second := dialAcct(t, ctx, srv, "alice", "u-alice")
	secondID, _ := acctHello(t, ctx, second)
	require.NotEqual(t, firstID, secondID, "a fresh connection mints its own player id")

	writeJSON(t, ctx, second, protocol.JoinRoom{Type: protocol.TypeJoinRoom, Code: room.Code})

	// The takeover re-identifies the newer connection onto the existing seat: it
	// answers hello_ok a second time, carrying the SEAT's identity, then the
	// room_state every reattach ends with.
	var ok protocol.HelloOK
	require.NoError(t, json.Unmarshal(expect(t, ctx, second, protocol.TypeHelloOK), &ok))
	assert.Equal(t, firstID, ok.PlayerID, "the seat keeps its player id across a takeover")
	assert.Equal(t, firstTok, ok.ResumeToken, "and its resume token")

	st := decodeRoomState(t, expect(t, ctx, second, protocol.TypeRoomState))
	assert.Equal(t, room.Code, st.Code)
	require.Len(t, st.Players, 1, "one account, one seat")
	assert.Equal(t, firstID, st.Players[0].PlayerID)

	expectDisplaced(t, ctx, first)

	assert.Equal(t, 0, h.GraceCount(), "a takeover must not strand a claimable resume token")
	requireIndexOK(t, h)
}

// TestTakeoverKeepsTheHostRoleAndTheRoom checks the takeover is a HAND-OFF, not
// a rejoin: the seat object survives, so the host role, the join order and the
// other seats' view of the room are all untouched.
func TestTakeoverKeepsTheHostRoleAndTheRoom(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	srv, h := acctServer(t, nil)

	host := dialAcct(t, ctx, srv, "alice", "u-alice")
	hostID, _ := acctHello(t, ctx, host)
	writeJSON(t, ctx, host, protocol.CreateRoom{Type: protocol.TypeCreateRoom})
	room := decodeRoomState(t, expect(t, ctx, host, protocol.TypeRoomState))
	require.Equal(t, hostID, room.HostPlayerID)

	bob := dialAcct(t, ctx, srv, "bob", "u-bob")
	acctHello(t, ctx, bob)
	writeJSON(t, ctx, bob, protocol.JoinRoom{Type: protocol.TypeJoinRoom, Code: room.Code})
	expect(t, ctx, bob, protocol.TypeRoomState)
	expect(t, ctx, bob, protocol.TypeChat)
	expect(t, ctx, host, protocol.TypeRoomState)
	expect(t, ctx, host, protocol.TypeChat)

	second := dialAcct(t, ctx, srv, "alice", "u-alice")
	acctHello(t, ctx, second)
	writeJSON(t, ctx, second, protocol.JoinRoom{Type: protocol.TypeJoinRoom, Code: room.Code})
	expect(t, ctx, second, protocol.TypeHelloOK)
	st := decodeRoomState(t, expect(t, ctx, second, protocol.TypeRoomState))

	require.Len(t, st.Players, 2, "the takeover replaces a connection, not a seat")
	assert.Equal(t, hostID, st.HostPlayerID, "the host role rides on the seat")
	assert.Equal(t, hostID, st.Players[0].PlayerID, "and so does join order")

	expectDisplaced(t, ctx, host)

	// The room is unchanged for everybody else: bob saw nothing at all, and the
	// new connection can still act as host.
	writeJSON(t, ctx, second, protocol.SettingsUpdate{
		Type:     protocol.TypeSettingsUpdate,
		Settings: protocol.DefaultSettings("taken over"),
	})
	after := decodeRoomState(t, expect(t, ctx, bob, protocol.TypeRoomState))
	assert.Equal(t, "taken over", after.Name, "the inherited seat still holds the host role")
	requireIndexOK(t, h)
}

// TestTakeoverMidMatchKeepsTheCapture is the case the whole design exists for: a
// tab lost mid-race, its resume token gone with it (a new tab, a cleared
// storage), rejoining by room code. The seat is reclaimed, the buffered relay is
// replayed, the batch sequence continues where it stopped, and the match
// persists ONE row for the account.
func TestTakeoverMidMatchKeepsTheCapture(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store := &fakeStore{}
	// A long grace so the dropped seat is still reclaimable when the new tab
	// arrives, and a long deadline so nothing ends the match under us.
	srv, h := acctServer(t, store, ws.WithGrace(20*time.Second))

	alice := dialAcct(t, ctx, srv, "alice", "u-alice")
	aliceID, _ := acctHello(t, ctx, alice)
	writeJSON(t, ctx, alice, protocol.CreateRoom{Type: protocol.TypeCreateRoom})
	room := decodeRoomState(t, expect(t, ctx, alice, protocol.TypeRoomState))

	bob := dialAcct(t, ctx, srv, "bob", "u-bob")
	bobID, _ := acctHello(t, ctx, bob)
	writeJSON(t, ctx, bob, protocol.JoinRoom{Type: protocol.TypeJoinRoom, Code: room.Code})
	expect(t, ctx, bob, protocol.TypeRoomState)
	expect(t, ctx, bob, protocol.TypeChat)
	expect(t, ctx, alice, protocol.TypeRoomState)
	expect(t, ctx, alice, protocol.TypeChat)

	writeJSON(t, ctx, bob, protocol.Ready{Type: protocol.TypeReady})
	expect(t, ctx, alice, protocol.TypeRoomState)
	expect(t, ctx, bob, protocol.TypeRoomState)

	writeJSON(t, ctx, alice, protocol.StartMatch{Type: protocol.TypeStartMatch})
	cd := decodeCountdown(t, expect(t, ctx, alice, protocol.TypeCountdown))
	decodeCountdown(t, expect(t, ctx, bob, protocol.TypeCountdown))

	// Alice types one batch, then her tab dies without ever using its token.
	sendBatch(t, ctx, alice, cd.MatchID, aliceID, 1, []json.RawMessage{event(1)})
	pb := decodePeerBatch(t, expect(t, ctx, bob, protocol.TypePeerBatch))
	require.Equal(t, aliceID, pb.PlayerID)

	require.NoError(t, alice.Close(websocket.StatusNormalClosure, ""))
	ps := decodePeerStatus(t, expect(t, ctx, bob, protocol.TypePeerStatus))
	require.Equal(t, protocol.StatusDisconnected, ps.Status)

	// Bob keeps racing while she is away: this batch has nowhere to go but the
	// graced seat's backlog.
	sendBatch(t, ctx, bob, cd.MatchID, bobID, 1, []json.RawMessage{event(11)})

	// A brand-new tab, no resume token, joining by room code.
	revived := dialAcct(t, ctx, srv, "alice", "u-alice")
	acctHello(t, ctx, revived)
	writeJSON(t, ctx, revived, protocol.JoinRoom{Type: protocol.TypeJoinRoom, Code: room.Code})

	var ok protocol.HelloOK
	require.NoError(t, json.Unmarshal(expect(t, ctx, revived, protocol.TypeHelloOK), &ok))
	require.Equal(t, aliceID, ok.PlayerID)
	st := decodeRoomState(t, expect(t, ctx, revived, protocol.TypeRoomState))
	require.NotNil(t, st.Match, "the reclaimed seat is still in the match")
	assert.Equal(t, cd.MatchID, st.Match.MatchID)

	// The backlog is replayed before the connection goes live.
	replayed := decodePeerBatch(t, expect(t, ctx, revived, protocol.TypePeerBatch))
	assert.Equal(t, bobID, replayed.PlayerID, "the batch sent while she was away is not lost")

	ps = decodePeerStatus(t, expect(t, ctx, bob, protocol.TypePeerStatus))
	assert.Equal(t, protocol.StatusReconnected, ps.Status)

	// The capture continues: batchSeq 1 was accepted before the drop, so 2 is
	// next and 1 would be rejected.
	sendBatch(t, ctx, revived, cd.MatchID, aliceID, 1, []json.RawMessage{event(2)})
	e := decodeErr(t, expect(t, ctx, revived, protocol.TypeError))
	assert.Equal(t, protocol.CodeBadMessage, e.Code)
	assert.Contains(t, e.Message, "out of order")

	sendBatch(t, ctx, revived, cd.MatchID, aliceID, 2, []json.RawMessage{event(2)})
	pb = decodePeerBatch(t, expect(t, ctx, bob, protocol.TypePeerBatch))
	assert.Equal(t, aliceID, pb.PlayerID)

	writeJSON(t, ctx, revived, protocol.Finish{Type: protocol.TypeFinish, MatchID: cd.MatchID})
	expect(t, ctx, revived, protocol.TypePeerStatus)
	expect(t, ctx, bob, protocol.TypePeerStatus)
	writeJSON(t, ctx, bob, protocol.Finish{Type: protocol.TypeFinish, MatchID: cd.MatchID})
	expect(t, ctx, bob, protocol.TypePeerStatus)
	expect(t, ctx, revived, protocol.TypePeerStatus)
	expect(t, ctx, revived, protocol.TypeMatchEnd)
	expect(t, ctx, bob, protocol.TypeMatchEnd)

	require.Eventually(t, func() bool { return len(store.records()) == 1 }, 5*time.Second, 20*time.Millisecond)
	rec := store.records()[0]
	require.Len(t, rec.Runs, 2, "one row per seat, and the account has exactly one seat")

	var aliceRuns int
	for _, run := range rec.Runs {
		if run.UserID == "u-alice" {
			aliceRuns++
			assert.Equal(t, 2, run.BatchCount, "both batches survived the takeover")
			assert.Equal(t, protocol.StatusFinished, run.FinalStatus)
			assert.Len(t, gunzipBatches(t, run.Log), 2)
		}
	}
	assert.Equal(t, 1, aliceRuns, "the account persists exactly one match_runs row")
	requireIndexOK(t, h)
}

// TestJoinAnotherRoomOutsideAMatchMoves: away from a match a seat is worth
// nothing, so a join elsewhere is an ordinary move — the old room sees a plain
// leave, host succession included.
func TestJoinAnotherRoomOutsideAMatchMoves(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	srv, h := acctServer(t, nil)

	alice := dialAcct(t, ctx, srv, "alice", "u-alice")
	aliceID, _ := acctHello(t, ctx, alice)
	writeJSON(t, ctx, alice, protocol.CreateRoom{Type: protocol.TypeCreateRoom})
	roomA := decodeRoomState(t, expect(t, ctx, alice, protocol.TypeRoomState))

	bob := dialAcct(t, ctx, srv, "bob", "u-bob")
	bobID, _ := acctHello(t, ctx, bob)
	writeJSON(t, ctx, bob, protocol.JoinRoom{Type: protocol.TypeJoinRoom, Code: roomA.Code})
	expect(t, ctx, bob, protocol.TypeRoomState)
	expect(t, ctx, bob, protocol.TypeChat)
	expect(t, ctx, alice, protocol.TypeRoomState)
	expect(t, ctx, alice, protocol.TypeChat)

	carol := dialAcct(t, ctx, srv, "carol", "u-carol")
	acctHello(t, ctx, carol)
	writeJSON(t, ctx, carol, protocol.CreateRoom{Type: protocol.TypeCreateRoom})
	roomB := decodeRoomState(t, expect(t, ctx, carol, protocol.TypeRoomState))

	// Alice's other tab joins room B.
	other := dialAcct(t, ctx, srv, "alice", "u-alice")
	otherID, _ := acctHello(t, ctx, other)
	writeJSON(t, ctx, other, protocol.JoinRoom{Type: protocol.TypeJoinRoom, Code: roomB.Code})

	// Room A watches her leave, and the host role passes to bob — the people left
	// behind cannot tell a move from a leave, because it is one.
	left := decodeRoomState(t, expect(t, ctx, bob, protocol.TypeRoomState))
	require.Len(t, left.Players, 1)
	assert.Equal(t, bobID, left.Players[0].PlayerID)
	assert.Equal(t, bobID, left.HostPlayerID)
	expect(t, ctx, bob, protocol.TypeChat) // "alice left"
	expect(t, ctx, bob, protocol.TypeChat) // "bob is now host"

	// Room B seats her FRESH: a different room is a different seat, so no
	// hello_ok re-identification and a new player id.
	joined := decodeRoomState(t, expect(t, ctx, other, protocol.TypeRoomState))
	assert.Equal(t, roomB.Code, joined.Code)
	require.Len(t, joined.Players, 2)
	assert.Equal(t, otherID, joined.Players[1].PlayerID)
	assert.NotEqual(t, aliceID, otherID)
	expect(t, ctx, other, protocol.TypeChat)

	expectDisplaced(t, ctx, alice)
	requireIndexOK(t, h)
}

// TestCreateRoomMovesAndDropsTheEmptyRoom is the same move via create_room, with
// the old room left empty: it must be dropped, not leak as a ghost the lobby
// still counts.
func TestCreateRoomMovesAndDropsTheEmptyRoom(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	srv, h := acctServer(t, nil)

	first := dialAcct(t, ctx, srv, "alice", "u-alice")
	acctHello(t, ctx, first)
	writeJSON(t, ctx, first, protocol.CreateRoom{Type: protocol.TypeCreateRoom})
	roomA := decodeRoomState(t, expect(t, ctx, first, protocol.TypeRoomState))
	require.Equal(t, 1, h.RoomCount())

	second := dialAcct(t, ctx, srv, "alice", "u-alice")
	acctHello(t, ctx, second)
	writeJSON(t, ctx, second, protocol.CreateRoom{Type: protocol.TypeCreateRoom})
	roomB := decodeRoomState(t, expect(t, ctx, second, protocol.TypeRoomState))

	assert.NotEqual(t, roomA.Code, roomB.Code)
	assert.Equal(t, 1, h.RoomCount(), "the emptied room is dropped, not leaked")

	expectDisplaced(t, ctx, first)
	requireIndexOK(t, h)
}

// TestMoveIsRefusedMidMatch: a join aimed at another room while the account is
// racing would forfeit the race (mid-match departure is the terminal "left" and
// there is no way back in). The newer connection is refused instead, and the
// older one is left completely alone — not displaced, still playing.
func TestMoveIsRefusedMidMatch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	srv, h := acctServer(t, nil)

	alice := dialAcct(t, ctx, srv, "alice", "u-alice")
	aliceID, _ := acctHello(t, ctx, alice)
	writeJSON(t, ctx, alice, protocol.CreateRoom{Type: protocol.TypeCreateRoom})
	roomA := decodeRoomState(t, expect(t, ctx, alice, protocol.TypeRoomState))

	bob := dialAcct(t, ctx, srv, "bob", "u-bob")
	acctHello(t, ctx, bob)
	writeJSON(t, ctx, bob, protocol.JoinRoom{Type: protocol.TypeJoinRoom, Code: roomA.Code})
	expect(t, ctx, bob, protocol.TypeRoomState)
	expect(t, ctx, bob, protocol.TypeChat)
	expect(t, ctx, alice, protocol.TypeRoomState)
	expect(t, ctx, alice, protocol.TypeChat)

	writeJSON(t, ctx, bob, protocol.Ready{Type: protocol.TypeReady})
	expect(t, ctx, alice, protocol.TypeRoomState)
	expect(t, ctx, bob, protocol.TypeRoomState)
	writeJSON(t, ctx, alice, protocol.StartMatch{Type: protocol.TypeStartMatch})
	cd := decodeCountdown(t, expect(t, ctx, alice, protocol.TypeCountdown))
	expect(t, ctx, bob, protocol.TypeCountdown)

	carol := dialAcct(t, ctx, srv, "carol", "u-carol")
	acctHello(t, ctx, carol)
	writeJSON(t, ctx, carol, protocol.CreateRoom{Type: protocol.TypeCreateRoom})
	roomB := decodeRoomState(t, expect(t, ctx, carol, protocol.TypeRoomState))

	other := dialAcct(t, ctx, srv, "alice", "u-alice")
	acctHello(t, ctx, other)

	writeJSON(t, ctx, other, protocol.JoinRoom{Type: protocol.TypeJoinRoom, Code: roomB.Code})
	e := decodeErr(t, expect(t, ctx, other, protocol.TypeError))
	assert.Equal(t, protocol.CodeInMatchElsewhere, e.Code)

	writeJSON(t, ctx, other, protocol.CreateRoom{Type: protocol.TypeCreateRoom})
	e = decodeErr(t, expect(t, ctx, other, protocol.TypeError))
	assert.Equal(t, protocol.CodeInMatchElsewhere, e.Code, "create_room is the same move")

	// The racing connection was never touched: it can still relay.
	sendBatch(t, ctx, alice, cd.MatchID, aliceID, 1, []json.RawMessage{event(1)})
	pb := decodePeerBatch(t, expect(t, ctx, bob, protocol.TypePeerBatch))
	assert.Equal(t, aliceID, pb.PlayerID)

	assert.Equal(t, 2, h.RoomCount())
	requireIndexOK(t, h)

	// The SAME room is still a takeover, though: the rule is about not losing a
	// running match, not about refusing the person their own seat back.
	writeJSON(t, ctx, other, protocol.JoinRoom{Type: protocol.TypeJoinRoom, Code: roomA.Code})
	var ok protocol.HelloOK
	require.NoError(t, json.Unmarshal(expect(t, ctx, other, protocol.TypeHelloOK), &ok))
	assert.Equal(t, aliceID, ok.PlayerID)
	expect(t, ctx, other, protocol.TypeRoomState)
	expectDisplaced(t, ctx, alice)
	requireIndexOK(t, h)
}

// TestGuestsAreNotOneSeatPerAccount guards the obvious way to get this wrong:
// indexing guests too, keyed on their empty account id, would collapse every
// guest in the process into a single seat that they take turns being displaced
// from.
func TestGuestsAreNotOneSeatPerAccount(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	srv, h := acctServer(t, nil)

	host := dialAcct(t, ctx, srv, "", "")
	acctHello(t, ctx, host)
	writeJSON(t, ctx, host, protocol.CreateRoom{Type: protocol.TypeCreateRoom})
	room := decodeRoomState(t, expect(t, ctx, host, protocol.TypeRoomState))

	guest := dialAcct(t, ctx, srv, "", "")
	acctHello(t, ctx, guest)
	writeJSON(t, ctx, guest, protocol.JoinRoom{Type: protocol.TypeJoinRoom, Code: room.Code})
	st := decodeRoomState(t, expect(t, ctx, guest, protocol.TypeRoomState))
	expect(t, ctx, guest, protocol.TypeChat)
	expect(t, ctx, host, protocol.TypeRoomState)
	expect(t, ctx, host, protocol.TypeChat)

	require.Len(t, st.Players, 2, "two guests are two people")
	assert.NotEqual(t, st.Players[0].Nick, st.Players[1].Nick)
	requireIndexOK(t, h)
}

// TestResumeTokenStillReclaimsAnAccountSeat: the account index must not disturb
// the path it was built alongside. A dropped authenticated seat is still
// reclaimed by its resume token, and reclaiming it consumes the grace entry.
func TestResumeTokenStillReclaimsAnAccountSeat(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	srv, h := acctServer(t, nil, ws.WithGrace(20*time.Second))

	host := dialAcct(t, ctx, srv, "alice", "u-alice")
	hostID, hostTok := acctHello(t, ctx, host)
	writeJSON(t, ctx, host, protocol.CreateRoom{Type: protocol.TypeCreateRoom})
	room := decodeRoomState(t, expect(t, ctx, host, protocol.TypeRoomState))

	bob := dialAcct(t, ctx, srv, "bob", "u-bob")
	acctHello(t, ctx, bob)
	writeJSON(t, ctx, bob, protocol.JoinRoom{Type: protocol.TypeJoinRoom, Code: room.Code})
	expect(t, ctx, bob, protocol.TypeRoomState)
	expect(t, ctx, bob, protocol.TypeChat)
	expect(t, ctx, host, protocol.TypeRoomState)
	expect(t, ctx, host, protocol.TypeChat)

	require.NoError(t, host.Close(websocket.StatusNormalClosure, ""))
	require.Eventually(t, func() bool { return h.GraceCount() == 1 }, 5*time.Second, 10*time.Millisecond)
	requireIndexOK(t, h)

	back := dialAcct(t, ctx, srv, "alice", "u-alice")
	writeJSON(t, ctx, back, protocol.Hello{
		Type:            protocol.TypeHello,
		ProtocolVersion: protocol.Version,
		ResumeToken:     hostTok,
	})
	var ok protocol.HelloOK
	require.NoError(t, json.Unmarshal(expect(t, ctx, back, protocol.TypeHelloOK), &ok))
	assert.Equal(t, hostID, ok.PlayerID)
	st := decodeRoomState(t, expect(t, ctx, back, protocol.TypeRoomState))
	assert.Equal(t, hostID, st.HostPlayerID)

	assert.Equal(t, 0, h.GraceCount())
	requireIndexOK(t, h)
}

// TestTakeoverAfterAKickTakesAFreshSeat: once a seat is gone the account is
// seatless, index included, so the next connection is an ordinary join rather
// than a takeover of something that no longer exists.
func TestTakeoverAfterAKickTakesAFreshSeat(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	srv, h := acctServer(t, nil)

	host := dialAcct(t, ctx, srv, "host", "u-host")
	hostID, _ := acctHello(t, ctx, host)
	writeJSON(t, ctx, host, protocol.CreateRoom{Type: protocol.TypeCreateRoom})
	room := decodeRoomState(t, expect(t, ctx, host, protocol.TypeRoomState))
	require.Equal(t, hostID, room.HostPlayerID)

	alice := dialAcct(t, ctx, srv, "alice", "u-alice")
	aliceID, _ := acctHello(t, ctx, alice)
	writeJSON(t, ctx, alice, protocol.JoinRoom{Type: protocol.TypeJoinRoom, Code: room.Code})
	expect(t, ctx, alice, protocol.TypeRoomState)
	expect(t, ctx, alice, protocol.TypeChat)
	expect(t, ctx, host, protocol.TypeRoomState)
	expect(t, ctx, host, protocol.TypeChat)

	writeJSON(t, ctx, host, protocol.Kick{Type: protocol.TypeKick, PlayerID: aliceID})
	expect(t, ctx, alice, protocol.TypeKicked)
	expect(t, ctx, host, protocol.TypeRoomState)
	expect(t, ctx, host, protocol.TypeChat)
	requireIndexOK(t, h)

	// The same connection rejoins: a kicked seat leaves nothing to inherit.
	writeJSON(t, ctx, alice, protocol.JoinRoom{Type: protocol.TypeJoinRoom, Code: room.Code})
	st := decodeRoomState(t, expect(t, ctx, alice, protocol.TypeRoomState))
	expect(t, ctx, alice, protocol.TypeChat)
	expect(t, ctx, host, protocol.TypeRoomState)
	expect(t, ctx, host, protocol.TypeChat)

	require.Len(t, st.Players, 2)
	assert.Equal(t, aliceID, st.Players[1].PlayerID, "the connection keeps its own player id")
	requireIndexOK(t, h)
}

// TestFullRoomRefusalKeepsTheOldSeat: capacity is checked BEFORE the old seat is
// released, so a join into a full room costs nothing. Getting this order wrong
// loses the seat the account had and gives nothing back.
func TestFullRoomRefusalKeepsTheOldSeat(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	srv, h := acctServer(t, nil)

	// Room B, filled to capacity with guests.
	filler := dialAcct(t, ctx, srv, "", "")
	acctHello(t, ctx, filler)
	writeJSON(t, ctx, filler, protocol.CreateRoom{Type: protocol.TypeCreateRoom})
	roomB := decodeRoomState(t, expect(t, ctx, filler, protocol.TypeRoomState))
	seated := []*websocket.Conn{filler}
	for range roomCapacity - 1 {
		g := dialAcct(t, ctx, srv, "", "")
		acctHello(t, ctx, g)
		writeJSON(t, ctx, g, protocol.JoinRoom{Type: protocol.TypeJoinRoom, Code: roomB.Code})
		expect(t, ctx, g, protocol.TypeRoomState)
		expect(t, ctx, g, protocol.TypeChat)
		for _, s := range seated {
			expect(t, ctx, s, protocol.TypeRoomState)
			expect(t, ctx, s, protocol.TypeChat)
		}
		seated = append(seated, g)
	}

	// Alice sits in her own room A and tries to move into the full one.
	alice := dialAcct(t, ctx, srv, "alice", "u-alice")
	aliceID, _ := acctHello(t, ctx, alice)
	writeJSON(t, ctx, alice, protocol.CreateRoom{Type: protocol.TypeCreateRoom})
	roomA := decodeRoomState(t, expect(t, ctx, alice, protocol.TypeRoomState))

	other := dialAcct(t, ctx, srv, "alice", "u-alice")
	acctHello(t, ctx, other)
	writeJSON(t, ctx, other, protocol.JoinRoom{Type: protocol.TypeJoinRoom, Code: roomB.Code})
	e := decodeErr(t, expect(t, ctx, other, protocol.TypeError))
	assert.Equal(t, protocol.CodeRoomFull, e.Code)

	// Room A is untouched and still hers: the original connection is still live
	// and still host.
	writeJSON(t, ctx, alice, protocol.SettingsUpdate{
		Type:     protocol.TypeSettingsUpdate,
		Settings: protocol.DefaultSettings("still here"),
	})
	st := decodeRoomState(t, expect(t, ctx, alice, protocol.TypeRoomState))
	expect(t, ctx, alice, protocol.TypeChat)
	assert.Equal(t, roomA.Code, st.Code)
	assert.Equal(t, aliceID, st.HostPlayerID)
	assert.Equal(t, "still here", st.Name)

	// A room_not_found refusal is likewise free.
	writeJSON(t, ctx, other, protocol.JoinRoom{Type: protocol.TypeJoinRoom, Code: "ZZZZZZ"})
	e = decodeErr(t, expect(t, ctx, other, protocol.TypeError))
	assert.Equal(t, protocol.CodeRoomNotFound, e.Code)

	requireIndexOK(t, h)
}
