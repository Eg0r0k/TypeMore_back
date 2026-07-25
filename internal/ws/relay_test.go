package ws_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/typemore/typemore-server/internal/protocol"
	"github.com/typemore/typemore-server/internal/ws"
)

// fakeStore is an in-memory ws.MatchStore that records everything saved so a
// relay test can assert the persisted capture without a database.
type fakeStore struct {
	mu    sync.Mutex
	saved []ws.MatchRecord
}

func (f *fakeStore) SaveMatch(_ context.Context, m ws.MatchRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saved = append(f.saved, m)
	return nil
}

func (f *fakeStore) records() []ws.MatchRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ws.MatchRecord(nil), f.saved...)
}

// relayServer wires /ws with the given store and options, resolving identity from
// the X-Test-User (displayName) and X-Test-Uid (account id) headers.
func relayServer(t *testing.T, store ws.MatchStore, opts ...ws.Option) *httptest.Server {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := chi.NewRouter()
	r.Handle("/ws", ws.NewHandler(logger, nil, func(req *http.Request) (string, string, bool) {
		if name := req.Header.Get("X-Test-User"); name != "" {
			return name, req.Header.Get("X-Test-Uid"), true
		}
		return "", "", false
	}, store, opts...))
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}

// relayHello completes the hello handshake and returns the player id and resume
// token (both from hello_ok).
func relayHello(t *testing.T, ctx context.Context, c *websocket.Conn) (string, string) {
	t.Helper()
	writeJSON(t, ctx, c, protocol.Hello{Type: protocol.TypeHello, ProtocolVersion: protocol.Version})
	var ok protocol.HelloOK
	require.NoError(t, json.Unmarshal(expect(t, ctx, c, protocol.TypeHelloOK), &ok))
	require.NotEmpty(t, ok.PlayerID)
	require.NotEmpty(t, ok.ResumeToken)
	return ok.PlayerID, ok.ResumeToken
}

// event builds an opaque log-v1-ish event as raw JSON.
func event(seq int) json.RawMessage {
	return json.RawMessage(`{"k":"insert","seq":` + strconv.Itoa(seq) + `,"ch":"x"}`)
}

// sendBatch sends one event_batch from c.
func sendBatch(t *testing.T, ctx context.Context, c *websocket.Conn, matchID, playerID string, seq int, events []json.RawMessage) {
	t.Helper()
	writeJSON(t, ctx, c, protocol.EventBatch{
		Type:     protocol.TypeEventBatch,
		MatchID:  matchID,
		PlayerID: playerID,
		BatchSeq: seq,
		Version:  1,
		Events:   events,
	})
}

func decodePeerBatch(t *testing.T, data []byte) protocol.PeerBatch {
	t.Helper()
	var pb protocol.PeerBatch
	require.NoError(t, json.Unmarshal(data, &pb))
	return pb
}

func decodePeerStatus(t *testing.T, data []byte) protocol.PeerStatus {
	t.Helper()
	var ps protocol.PeerStatus
	require.NoError(t, json.Unmarshal(data, &ps))
	return ps
}

func decodeCountdown(t *testing.T, data []byte) protocol.Countdown {
	t.Helper()
	var cd protocol.Countdown
	require.NoError(t, json.Unmarshal(data, &cd))
	return cd
}

// gunzipBatches decompresses a persisted capture log into its CapturedBatch list.
func gunzipBatches(t *testing.T, b []byte) []ws.CapturedBatch {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	require.NoError(t, err)
	defer zr.Close()
	raw, err := io.ReadAll(zr)
	require.NoError(t, err)
	var cb []ws.CapturedBatch
	require.NoError(t, json.Unmarshal(raw, &cb))
	return cb
}

// match holds the state a started match test needs.
type match struct {
	conns   []*websocket.Conn
	ids     []string
	tokens  []string
	matchID string
	seed    int64
}

// startMatch builds an n-player room (player 0 is host) and starts a match,
// returning the drained connections and the frozen countdown fields. All
// connections' queues are empty on return. A non-empty settingsDur overrides the
// room's time-mode durationMs (applied host-side before ready/start).
func startMatch(t *testing.T, ctx context.Context, srv *httptest.Server, n, settingsDur int) match {
	t.Helper()
	var ns *protocol.Settings
	if settingsDur > 0 {
		s := protocol.DefaultSettings("Relay")
		s.DurationMs = settingsDur
		ns = &s
	}
	return startMatchWith(t, ctx, srv, n, ns)
}

// startMatchWith is startMatch with full control over the room settings applied
// before ready/start (nil keeps the room defaults).
func startMatchWith(t *testing.T, ctx context.Context, srv *httptest.Server, n int, ns *protocol.Settings) match {
	t.Helper()
	require.GreaterOrEqual(t, n, 2)

	host := dialAs(t, ctx, srv, "")
	hostID, hostTok := relayHello(t, ctx, host)
	writeJSON(t, ctx, host, protocol.CreateRoom{Type: protocol.TypeCreateRoom})
	hs := decodeRoomState(t, expect(t, ctx, host, protocol.TypeRoomState))

	m := match{conns: []*websocket.Conn{host}, ids: []string{hostID}, tokens: []string{hostTok}}
	seated := []*websocket.Conn{host}
	for i := 1; i < n; i++ {
		g := dialAs(t, ctx, srv, "")
		gid, gtok := relayHello(t, ctx, g)
		writeJSON(t, ctx, g, protocol.JoinRoom{Type: protocol.TypeJoinRoom, Code: hs.Code})
		expect(t, ctx, g, protocol.TypeRoomState)
		expect(t, ctx, g, protocol.TypeChat)
		for _, s := range seated {
			expect(t, ctx, s, protocol.TypeRoomState)
			expect(t, ctx, s, protocol.TypeChat)
		}
		m.conns = append(m.conns, g)
		m.ids = append(m.ids, gid)
		m.tokens = append(m.tokens, gtok)
		seated = append(seated, g)
	}

	if ns != nil {
		writeJSON(t, ctx, host, protocol.SettingsUpdate{Type: protocol.TypeSettingsUpdate, Settings: *ns})
		// settings_update broadcasts room_state + a settings_changed chat to all.
		for _, c := range m.conns {
			expect(t, ctx, c, protocol.TypeRoomState)
			expect(t, ctx, c, protocol.TypeChat)
		}
	}

	// Non-host seats ready; each ready broadcasts room_state to all.
	for i := 1; i < n; i++ {
		writeJSON(t, ctx, m.conns[i], protocol.Ready{Type: protocol.TypeReady})
		for _, c := range m.conns {
			expect(t, ctx, c, protocol.TypeRoomState)
		}
	}

	writeJSON(t, ctx, host, protocol.StartMatch{Type: protocol.TypeStartMatch})
	for i, c := range m.conns {
		cd := decodeCountdown(t, expect(t, ctx, c, protocol.TypeCountdown))
		require.NotEmpty(t, cd.MatchID)
		if i == 0 {
			m.matchID = cd.MatchID
			m.seed = cd.Seed
		} else {
			require.Equal(t, m.matchID, cd.MatchID, "every seat gets the same matchId")
			require.Equal(t, m.seed, cd.Seed, "every seat gets the same seed")
		}
	}
	return m
}

// TestRelayTwoPlayerMatch runs a full 2-player match: countdown fields, lossless
// per-player-ordered cross-relay, finish, and the persisted capture with stamps.
func TestRelayTwoPlayerMatch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	store := &fakeStore{}
	srv := relayServer(t, store)

	m := startMatch(t, ctx, srv, 2, 0)
	host, guest := m.conns[0], m.conns[1]

	// Countdown froze the seed in range and both freemods.
	assert.GreaterOrEqual(t, m.seed, int64(0))
	assert.LessOrEqual(t, m.seed, int64(1)<<32-1)

	// Host relays 3 ordered batches; guest must receive them in order, lossless.
	for seq := 1; seq <= 3; seq++ {
		sendBatch(t, ctx, host, m.matchID, m.ids[0], seq, []json.RawMessage{event(seq)})
	}
	for seq := 1; seq <= 3; seq++ {
		pb := decodePeerBatch(t, expect(t, ctx, guest, protocol.TypePeerBatch))
		assert.Equal(t, m.ids[0], pb.PlayerID)
		require.Len(t, pb.Events, 1)
		assert.JSONEq(t, string(event(seq)), string(pb.Events[0]))
	}

	// Guest relays 2 batches back.
	for seq := 1; seq <= 2; seq++ {
		sendBatch(t, ctx, guest, m.matchID, m.ids[1], seq, []json.RawMessage{event(seq)})
	}
	for seq := 1; seq <= 2; seq++ {
		pb := decodePeerBatch(t, expect(t, ctx, host, protocol.TypePeerBatch))
		assert.Equal(t, m.ids[1], pb.PlayerID)
	}

	// Both finish; each sees the other's peer_status finished.
	writeJSON(t, ctx, host, protocol.Finish{Type: protocol.TypeFinish, MatchID: m.matchID})
	ps := decodePeerStatus(t, readUntil(t, ctx, guest, protocol.TypePeerStatus))
	assert.Equal(t, protocol.StatusFinished, ps.Status)
	assert.Equal(t, m.ids[0], ps.PlayerID)

	writeJSON(t, ctx, guest, protocol.Finish{Type: protocol.TypeFinish, MatchID: m.matchID})
	ps = decodePeerStatus(t, readUntil(t, ctx, host, protocol.TypePeerStatus))
	assert.Equal(t, protocol.StatusFinished, ps.Status)

	// The match persists once, with both runs and server-stamped captures.
	require.Eventually(t, func() bool { return len(store.records()) == 1 }, 5*time.Second, 20*time.Millisecond)
	rec := store.records()[0]
	assert.Equal(t, m.matchID, rec.ID)
	assert.Equal(t, m.seed, rec.Seed)
	require.Len(t, rec.Runs, 2)

	byID := map[string]ws.MatchRunRecord{}
	for _, run := range rec.Runs {
		byID[run.PlayerID] = run
	}
	hostRun := byID[m.ids[0]]
	assert.Equal(t, protocol.StatusFinished, hostRun.FinalStatus)
	assert.Equal(t, 3, hostRun.BatchCount)
	hb := gunzipBatches(t, hostRun.Log)
	require.Len(t, hb, 3)
	for i, cb := range hb {
		assert.Equal(t, i+1, cb.BatchSeq)
		assert.NotZero(t, cb.RecvServerMs, "each captured batch carries a server recv stamp")
	}
	assert.Equal(t, 2, byID[m.ids[1]].BatchCount)
}

// TestRelayFivePlayerMatch verifies all-to-all relay among 5 seats and a single
// persisted match with 5 runs.
func TestRelayFivePlayerMatch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	store := &fakeStore{}
	srv := relayServer(t, store)

	m := startMatch(t, ctx, srv, 5, 0)

	// Each seat relays one batch; every other seat receives exactly that peer.
	for i, c := range m.conns {
		sendBatch(t, ctx, c, m.matchID, m.ids[i], 1, []json.RawMessage{event(1)})
	}
	for i, c := range m.conns {
		got := map[string]bool{}
		for range len(m.conns) - 1 {
			pb := decodePeerBatch(t, expect(t, ctx, c, protocol.TypePeerBatch))
			got[pb.PlayerID] = true
		}
		for j, id := range m.ids {
			if j == i {
				assert.NotContains(t, got, id, "no self-relay")
			} else {
				assert.Contains(t, got, id, "seat %d must receive peer %d", i, j)
			}
		}
	}

	for i, c := range m.conns {
		sendBatch(t, ctx, c, m.matchID, m.ids[i], 2, []json.RawMessage{event(2)})
	}
	for i, c := range m.conns {
		writeJSON(t, ctx, c, protocol.Finish{Type: protocol.TypeFinish, MatchID: m.matchID})
		_ = i
	}

	require.Eventually(t, func() bool { return len(store.records()) == 1 }, 5*time.Second, 20*time.Millisecond)
	rec := store.records()[0]
	require.Len(t, rec.Runs, 5)
	for _, run := range rec.Runs {
		assert.Equal(t, protocol.StatusFinished, run.FinalStatus)
		assert.Equal(t, 2, run.BatchCount)
	}
}

// TestReconnectResume drops a seat mid-match, reconnects with its resume token,
// and asserts the buffered backlog replays exactly once in order and batchSeq
// continues across the gap.
func TestReconnectResume(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	store := &fakeStore{}
	srv := relayServer(t, store)

	m := startMatch(t, ctx, srv, 2, 0)
	host, guest := m.conns[0], m.conns[1]
	hostID, guestID, guestTok := m.ids[0], m.ids[1], m.tokens[1]

	// Establish guest's batchSeq at 1, then drop guest.
	sendBatch(t, ctx, guest, m.matchID, guestID, 1, []json.RawMessage{event(1)})
	decodePeerBatch(t, expect(t, ctx, host, protocol.TypePeerBatch))
	require.NoError(t, guest.Close(websocket.StatusNormalClosure, "drop"))

	// Host observes the disconnect, then relays two batches that must buffer.
	ps := decodePeerStatus(t, readUntil(t, ctx, host, protocol.TypePeerStatus))
	assert.Equal(t, protocol.StatusDisconnected, ps.Status)
	assert.Equal(t, guestID, ps.PlayerID)
	sendBatch(t, ctx, host, m.matchID, hostID, 1, []json.RawMessage{event(10)})
	sendBatch(t, ctx, host, m.matchID, hostID, 2, []json.RawMessage{event(11)})

	// Reconnect with the resume token: hello_ok, then the backlog in order.
	guest2 := dialAs(t, ctx, srv, "")
	writeJSON(t, ctx, guest2, protocol.Hello{Type: protocol.TypeHello, ProtocolVersion: protocol.Version, ResumeToken: guestTok})
	var ok protocol.HelloOK
	require.NoError(t, json.Unmarshal(expect(t, ctx, guest2, protocol.TypeHelloOK), &ok))
	assert.Equal(t, guestID, ok.PlayerID, "resume keeps the same player id")

	// The resumer receives a fresh room_state right after hello_ok, then the backlog.
	expect(t, ctx, guest2, protocol.TypeRoomState)

	first := decodePeerBatch(t, expect(t, ctx, guest2, protocol.TypePeerBatch))
	require.Len(t, first.Events, 1)
	assert.JSONEq(t, string(event(10)), string(first.Events[0]))
	second := decodePeerBatch(t, expect(t, ctx, guest2, protocol.TypePeerBatch))
	assert.JSONEq(t, string(event(11)), string(second.Events[0]))

	// Host is told the peer reconnected.
	ps = decodePeerStatus(t, readUntil(t, ctx, host, protocol.TypePeerStatus))
	assert.Equal(t, protocol.StatusReconnected, ps.Status)

	// batchSeq continuity: guest's next accepted seq is 2 (it sent 1 pre-drop).
	sendBatch(t, ctx, guest2, m.matchID, guestID, 2, []json.RawMessage{event(2)})
	pb := decodePeerBatch(t, expect(t, ctx, host, protocol.TypePeerBatch))
	assert.Equal(t, guestID, pb.PlayerID)
	assert.JSONEq(t, string(event(2)), string(pb.Events[0]))

	// Finish and confirm the guest's capture spans the gap (2 batches).
	writeJSON(t, ctx, host, protocol.Finish{Type: protocol.TypeFinish, MatchID: m.matchID})
	writeJSON(t, ctx, guest2, protocol.Finish{Type: protocol.TypeFinish, MatchID: m.matchID})
	require.Eventually(t, func() bool { return len(store.records()) == 1 }, 5*time.Second, 20*time.Millisecond)
	rec := store.records()[0]
	for _, run := range rec.Runs {
		if run.PlayerID == guestID {
			assert.Equal(t, protocol.StatusFinished, run.FinalStatus)
			assert.Equal(t, 2, run.BatchCount)
		}
	}
}

// TestRoomStateNamesRunningMatch asserts room_state carries the running match's
// identity, and only while it runs. This is what a page-reloaded client sees: the
// seat survived on its resume token but the tab's game state did not, so without
// this field the client cannot tell it holds a seat in a match it can no longer
// play — and every other player waits out the deadline for a run that will never
// arrive.
func TestRoomStateNamesRunningMatch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	srv := relayServer(t, &fakeStore{})

	m := startMatch(t, ctx, srv, 2, 0)
	host, guest := m.conns[0], m.conns[1]

	// The reload: the tab's socket dies and a brand-new one reclaims the seat.
	require.NoError(t, guest.Close(websocket.StatusNormalClosure, "reload"))
	ps := decodePeerStatus(t, readUntil(t, ctx, host, protocol.TypePeerStatus))
	require.Equal(t, protocol.StatusDisconnected, ps.Status)

	guest2, _, rs := resumeSeat(t, ctx, srv, m.tokens[1], m.ids[1])
	require.NotNil(t, rs.Match, "a resumer mid-match must learn the match exists")
	assert.Equal(t, m.matchID, rs.Match.MatchID)
	assert.Positive(t, rs.Match.GoAtServerMs)

	// The client's answer to that is a forfeit: it abandons a run it can no
	// longer produce, so the seat is dnf'd (never a phantom finisher) and the
	// match ends at once instead of stalling until the hard deadline.
	writeJSON(t, ctx, guest2, protocol.Finish{Type: protocol.TypeFinish, MatchID: m.matchID, Forfeit: true})
	// The resume itself announced `reconnected` to the host first.
	ps = decodePeerStatus(t, readUntil(t, ctx, host, protocol.TypePeerStatus))
	require.Equal(t, protocol.StatusReconnected, ps.Status)
	ps = decodePeerStatus(t, readUntil(t, ctx, host, protocol.TypePeerStatus))
	assert.Equal(t, protocol.StatusDNF, ps.Status, "a forfeit is a dnf, not a finish")
	assert.Equal(t, m.ids[1], ps.PlayerID)
	writeJSON(t, ctx, host, protocol.Finish{Type: protocol.TypeFinish, MatchID: m.matchID})
	me := decodeMatchEnd(t, readUntil(t, ctx, host, protocol.TypeMatchEnd))
	assert.Equal(t, protocol.ReasonAllFinished, me.Reason)
	byID := resultsByID(t, me, 2)
	assert.Equal(t, protocol.StatusDNF, byID[m.ids[1]].Status)
	assert.Nil(t, byID[m.ids[1]].FinishedAtMs)
	// Post-match the field is gone: the seat is back in a plain lobby.
	after := decodeRoomState(t, readUntil(t, ctx, host, protocol.TypeRoomState))
	assert.Nil(t, after.Match, "room_state names no match once the match has ended")
}

// TestGraceExpiryDNF asserts a seat that never reconnects is dnf'd after the
// grace window and the match ends once the remaining seat finishes.
func TestGraceExpiryDNF(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	store := &fakeStore{}
	srv := relayServer(t, store, ws.WithGrace(300*time.Millisecond))

	m := startMatch(t, ctx, srv, 2, 0)
	host, guest := m.conns[0], m.conns[1]
	guestID := m.ids[1]

	require.NoError(t, guest.Close(websocket.StatusNormalClosure, "drop"))
	ps := decodePeerStatus(t, readUntil(t, ctx, host, protocol.TypePeerStatus))
	assert.Equal(t, protocol.StatusDisconnected, ps.Status)

	// Grace elapses -> dnf.
	ps = decodePeerStatus(t, readUntil(t, ctx, host, protocol.TypePeerStatus))
	assert.Equal(t, protocol.StatusDNF, ps.Status)
	assert.Equal(t, guestID, ps.PlayerID)

	// Host finishes -> match ends and persists.
	writeJSON(t, ctx, host, protocol.Finish{Type: protocol.TypeFinish, MatchID: m.matchID})
	require.Eventually(t, func() bool { return len(store.records()) == 1 }, 5*time.Second, 20*time.Millisecond)
	rec := store.records()[0]
	byID := map[string]string{}
	for _, run := range rec.Runs {
		byID[run.PlayerID] = run.FinalStatus
	}
	assert.Equal(t, protocol.StatusDNF, byID[guestID])
	assert.Equal(t, protocol.StatusFinished, byID[m.ids[0]])
}

// TestHardDeadlineDNF asserts an unfinished match is force-ended at the hard
// deadline with every remaining seat dnf'd.
func TestHardDeadlineDNF(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	store := &fakeStore{}
	srv := relayServer(t, store, ws.WithDeadlineSlack(200))

	// Short time-mode duration so the deadline (goAt + dur + slack) fires soon.
	m := startMatch(t, ctx, srv, 2, 300)

	// Nobody finishes; both seats are dnf'd at the deadline.
	for _, c := range m.conns {
		ps := decodePeerStatus(t, readUntil(t, ctx, c, protocol.TypePeerStatus))
		assert.Equal(t, protocol.StatusDNF, ps.Status)
	}
	require.Eventually(t, func() bool { return len(store.records()) == 1 }, 5*time.Second, 20*time.Millisecond)
	rec := store.records()[0]
	require.Len(t, rec.Runs, 2)
	for _, run := range rec.Runs {
		assert.Equal(t, protocol.StatusDNF, run.FinalStatus)
	}
}

// TestRematchNewSeed asserts a second match after one completes gets a fresh
// matchId and a new seed (rematch = re-ready then start).
func TestRematchNewSeed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	store := &fakeStore{}
	srv := relayServer(t, store)

	m := startMatch(t, ctx, srv, 2, 0)
	host, guest := m.conns[0], m.conns[1]

	// Finish the first match.
	writeJSON(t, ctx, host, protocol.Finish{Type: protocol.TypeFinish, MatchID: m.matchID})
	writeJSON(t, ctx, guest, protocol.Finish{Type: protocol.TypeFinish, MatchID: m.matchID})
	// Match end resets ready and broadcasts a fresh room_state to both.
	for _, c := range m.conns {
		st := decodeRoomState(t, readUntil(t, ctx, c, protocol.TypeRoomState))
		require.False(t, playerReady(st, m.ids[1]), "ready resets at match end")
	}

	// Re-ready and start again: new countdown, new matchId, new seed.
	writeJSON(t, ctx, guest, protocol.Ready{Type: protocol.TypeReady})
	for _, c := range m.conns {
		expect(t, ctx, c, protocol.TypeRoomState)
	}
	writeJSON(t, ctx, host, protocol.StartMatch{Type: protocol.TypeStartMatch})
	cd2 := decodeCountdown(t, readUntil(t, ctx, host, protocol.TypeCountdown))
	assert.NotEqual(t, m.matchID, cd2.MatchID, "rematch gets a new matchId")
	assert.NotEqual(t, m.seed, cd2.Seed, "rematch gets a new seed")
}

func playerReady(st protocol.RoomState, id string) bool {
	p, ok := playerByID(st, id)
	return ok && p.Ready
}

// TestUnknownResumeTokenDegradesToFresh asserts a hello with an unknown resume
// token is NOT an error: the server issues a fresh identity (the grace window
// simply elapsed — rejecting would brick every stale-token client).
func TestUnknownResumeTokenDegradesToFresh(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	srv := relayServer(t, &fakeStore{})

	c := dialAs(t, ctx, srv, "")
	writeJSON(t, ctx, c, protocol.Hello{Type: protocol.TypeHello, ProtocolVersion: protocol.Version, ResumeToken: strings.Repeat("a", 64)})
	var ok protocol.HelloOK
	require.NoError(t, json.Unmarshal(expect(t, ctx, c, protocol.TypeHelloOK), &ok))
	assert.NotEmpty(t, ok.PlayerID)
	assert.NotEqual(t, strings.Repeat("a", 64), ok.ResumeToken, "a fresh token is issued")
}

// TestBatchSeqGapAndDup asserts a gap or duplicate batchSeq is rejected with
// bad_message and the connection stays usable.
func TestBatchSeqGapAndDup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	srv := relayServer(t, &fakeStore{})

	m := startMatch(t, ctx, srv, 2, 0)
	host, guest := m.conns[0], m.conns[1]

	sendBatch(t, ctx, host, m.matchID, m.ids[0], 1, []json.RawMessage{event(1)})
	decodePeerBatch(t, expect(t, ctx, guest, protocol.TypePeerBatch))

	// Gap: skip 2, send 3.
	sendBatch(t, ctx, host, m.matchID, m.ids[0], 3, []json.RawMessage{event(3)})
	assert.Equal(t, protocol.CodeBadMessage, decodeErr(t, expect(t, ctx, host, protocol.TypeError)).Code)

	// Connection stays: the correct next seq (2) is accepted.
	sendBatch(t, ctx, host, m.matchID, m.ids[0], 2, []json.RawMessage{event(2)})
	decodePeerBatch(t, expect(t, ctx, guest, protocol.TypePeerBatch))

	// Duplicate: replay seq 2 (lastSeq is now 2, expects 3).
	sendBatch(t, ctx, host, m.matchID, m.ids[0], 2, []json.RawMessage{event(2)})
	assert.Equal(t, protocol.CodeBadMessage, decodeErr(t, expect(t, ctx, host, protocol.TypeError)).Code)

	// Empty events are rejected too.
	sendBatch(t, ctx, host, m.matchID, m.ids[0], 3, []json.RawMessage{})
	assert.Equal(t, protocol.CodeBadMessage, decodeErr(t, expect(t, ctx, host, protocol.TypeError)).Code)
}

// TestEventBatchFrameCap asserts an event_batch over 1 MiB is dropped with
// bad_message while the connection stays open.
func TestEventBatchFrameCap(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	srv := relayServer(t, &fakeStore{})

	m := startMatch(t, ctx, srv, 2, 0)
	host, guest := m.conns[0], m.conns[1]

	big := json.RawMessage(`"` + strings.Repeat("x", 1_500_000) + `"`)
	sendBatch(t, ctx, host, m.matchID, m.ids[0], 1, []json.RawMessage{big})
	assert.Equal(t, protocol.CodeBadMessage, decodeErr(t, readUntil(t, ctx, host, protocol.TypeError)).Code)

	// Connection stays open: a normal batch still relays.
	sendBatch(t, ctx, host, m.matchID, m.ids[0], 1, []json.RawMessage{event(1)})
	pb := decodePeerBatch(t, expect(t, ctx, guest, protocol.TypePeerBatch))
	assert.Equal(t, m.ids[0], pb.PlayerID)
}

// lobbyRoom builds a two-seat lobby room (index 0 is host) and returns the
// drained connections, player ids, resume tokens, and the room code.
func lobbyRoom(t *testing.T, ctx context.Context, srv *httptest.Server) ([2]*websocket.Conn, [2]string, [2]string, string) {
	t.Helper()
	host := dialAs(t, ctx, srv, "")
	hostID, hostTok := relayHello(t, ctx, host)
	writeJSON(t, ctx, host, protocol.CreateRoom{Type: protocol.TypeCreateRoom})
	hs := decodeRoomState(t, expect(t, ctx, host, protocol.TypeRoomState))

	guest := dialAs(t, ctx, srv, "")
	guestID, guestTok := relayHello(t, ctx, guest)
	writeJSON(t, ctx, guest, protocol.JoinRoom{Type: protocol.TypeJoinRoom, Code: hs.Code})
	expect(t, ctx, guest, protocol.TypeRoomState)
	expect(t, ctx, guest, protocol.TypeChat)
	expect(t, ctx, host, protocol.TypeRoomState)
	expect(t, ctx, host, protocol.TypeChat)

	return [2]*websocket.Conn{host, guest}, [2]string{hostID, guestID}, [2]string{hostTok, guestTok}, hs.Code
}

// resumeSeat reconnects with a resume token, retrying until the server has
// processed the drop and registered the grace entry (a lobby drop is silent on
// the wire, so there is no frame to synchronize on). An unknown token degrades
// to a FRESH identity (never an error), so an attempt that raced the drop is
// detected by the wrong playerId and redialed. Returns the new connection, the
// hello_ok, and the fresh room_state the server always sends a resumer.
func resumeSeat(t *testing.T, ctx context.Context, srv *httptest.Server, token, wantID string) (*websocket.Conn, protocol.HelloOK, protocol.RoomState) {
	t.Helper()
	for {
		c := dialAs(t, ctx, srv, "")
		writeJSON(t, ctx, c, protocol.Hello{Type: protocol.TypeHello, ProtocolVersion: protocol.Version, ResumeToken: token})
		gotType, data := readFrame(t, ctx, c)
		require.Equal(t, protocol.TypeHelloOK, gotType)
		var ok protocol.HelloOK
		require.NoError(t, json.Unmarshal(data, &ok))
		if ok.PlayerID != wantID {
			// Fresh identity: the drop was not processed yet — retry on a new dial.
			require.NoError(t, c.Close(websocket.StatusNormalClosure, "retry"))
			time.Sleep(20 * time.Millisecond)
			continue
		}
		st := decodeRoomState(t, expect(t, ctx, c, protocol.TypeRoomState))
		return c, ok, st
	}
}

// TestLobbyDropResume drops a lobby seat's connection and resumes within grace:
// the seat is reclaimed with the same playerId, nick, and ready flag, the
// resumer receives a fresh room_state after hello_ok, and the other seat saw
// no leave — the drop is silent on the wire.
func TestLobbyDropResume(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	srv := relayServer(t, &fakeStore{})

	conns, ids, toks, _ := lobbyRoom(t, ctx, srv)
	host, guest := conns[0], conns[1]
	guestID, guestTok := ids[1], toks[1]

	// Guest readies up so the flag can be checked across the resume; the
	// room_state also carries the nick to compare after the reclaim.
	writeJSON(t, ctx, guest, protocol.Ready{Type: protocol.TypeReady})
	pre := decodeRoomState(t, expect(t, ctx, guest, protocol.TypeRoomState))
	expect(t, ctx, host, protocol.TypeRoomState)
	preSeat, found := playerByID(pre, guestID)
	require.True(t, found)

	require.NoError(t, guest.Close(websocket.StatusNormalClosure, "reload"))

	guest2, ok, rs := resumeSeat(t, ctx, srv, guestTok, guestID)
	assert.Equal(t, guestID, ok.PlayerID, "resume keeps the player id")
	require.Len(t, rs.Players, 2, "the seat never left the room")
	p, found := playerByID(rs, guestID)
	require.True(t, found)
	assert.Equal(t, preSeat.Nick, p.Nick, "resume keeps the nick")
	assert.True(t, p.Ready, "ready flag survives the drop")

	// The gap was silent for the host: its next frame is the chat the resumed
	// guest sends now — not a leave chat or room_state from the drop/resume.
	writeJSON(t, ctx, guest2, protocol.ChatSend{Type: protocol.TypeChatSend, Text: "back"})
	var chat protocol.Chat
	require.NoError(t, json.Unmarshal(expect(t, ctx, host, protocol.TypeChat), &chat))
	assert.Equal(t, guestID, chat.From)
}

// TestLobbyGraceExpiryLeave asserts a lobby seat that never resumes departs
// through the normal leave flow once the grace elapses: room_state without the
// seat, a system leave chat, and a dead resume token.
func TestLobbyGraceExpiryLeave(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	srv := relayServer(t, &fakeStore{}, ws.WithGrace(300*time.Millisecond))

	conns, ids, toks, _ := lobbyRoom(t, ctx, srv)
	host, guest := conns[0], conns[1]
	guestID, guestTok := ids[1], toks[1]

	require.NoError(t, guest.Close(websocket.StatusNormalClosure, "drop"))

	// Grace elapses -> the leave flow: room_state without the seat, leave chat.
	st := decodeRoomState(t, expect(t, ctx, host, protocol.TypeRoomState))
	require.Len(t, st.Players, 1)
	_, found := playerByID(st, guestID)
	assert.False(t, found)
	var chat protocol.Chat
	require.NoError(t, json.Unmarshal(expect(t, ctx, host, protocol.TypeChat), &chat))
	assert.Equal(t, protocol.ChatFromSystem, chat.From)
	assert.Equal(t, protocol.ChatKindLeave, chat.Kind)

	// The expired token no longer reclaims anything — the hello degrades to a
	// fresh identity instead of an error.
	c := dialAs(t, ctx, srv, "")
	writeJSON(t, ctx, c, protocol.Hello{Type: protocol.TypeHello, ProtocolVersion: protocol.Version, ResumeToken: guestTok})
	var fresh protocol.HelloOK
	require.NoError(t, json.Unmarshal(expect(t, ctx, c, protocol.TypeHelloOK), &fresh))
	assert.NotEqual(t, guestID, fresh.PlayerID, "expired token yields a fresh identity")
}

// TestSoloHostReloadKeepsRoom asserts a room whose only seat is graced survives
// the gap: the solo host drops, resumes, and finds the same room with its host
// role intact.
func TestSoloHostReloadKeepsRoom(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	srv := relayServer(t, &fakeStore{})

	host := dialAs(t, ctx, srv, "")
	hostID, hostTok := relayHello(t, ctx, host)
	writeJSON(t, ctx, host, protocol.CreateRoom{Type: protocol.TypeCreateRoom})
	hs := decodeRoomState(t, expect(t, ctx, host, protocol.TypeRoomState))

	require.NoError(t, host.Close(websocket.StatusNormalClosure, "reload"))

	_, ok, rs := resumeSeat(t, ctx, srv, hostTok, hostID)
	assert.Equal(t, hostID, ok.PlayerID)
	assert.Equal(t, hs.Code, rs.Code, "the room with only a graced seat survived")
	assert.Equal(t, hostID, rs.HostPlayerID, "host keeps the role across the resume")
	require.Len(t, rs.Players, 1)
}

// TestHostDropNoSuccession asserts the host role does NOT pass while the host
// is graced: the host drops, the guest sees nothing, and the resumed host is
// still the host.
func TestHostDropNoSuccession(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	srv := relayServer(t, &fakeStore{})

	conns, ids, toks, _ := lobbyRoom(t, ctx, srv)
	host, guest := conns[0], conns[1]
	hostID, hostTok := ids[0], toks[0]

	require.NoError(t, host.Close(websocket.StatusNormalClosure, "reload"))

	host2, _, rs := resumeSeat(t, ctx, srv, hostTok, hostID)
	assert.Equal(t, hostID, rs.HostPlayerID, "no succession while graced")

	// The guest saw neither the drop nor the resume: its next frame is the
	// resumed host's chat.
	writeJSON(t, ctx, host2, protocol.ChatSend{Type: protocol.TypeChatSend, Text: "still host"})
	var chat protocol.Chat
	require.NoError(t, json.Unmarshal(expect(t, ctx, guest, protocol.TypeChat), &chat))
	assert.Equal(t, hostID, chat.From)
}
