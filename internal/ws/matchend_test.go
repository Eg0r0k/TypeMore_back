package ws_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/typemore/typemore-server/internal/protocol"
	"github.com/typemore/typemore-server/internal/ws"
)

func decodeMatchEnd(t *testing.T, data []byte) protocol.MatchEnd {
	t.Helper()
	var me protocol.MatchEnd
	require.NoError(t, json.Unmarshal(data, &me))
	return me
}

// wordsSettings builds a valid words-mode room configuration.
func wordsSettings(wordCount int) *protocol.Settings {
	s := protocol.DefaultSettings("Relay")
	s.Mode = protocol.ModeWords
	s.WordCount = wordCount
	s.DurationMs = 0
	return &s
}

// resultsByID indexes a match_end's results by playerId.
func resultsByID(t *testing.T, me protocol.MatchEnd, wantLen int) map[string]protocol.MatchResult {
	t.Helper()
	require.Len(t, me.Results, wantLen, "results must cover the full frozen roster")
	byID := map[string]protocol.MatchResult{}
	for _, res := range me.Results {
		byID[res.PlayerID] = res
	}
	return byID
}

// TestMatchEndAllFinished asserts a completed match broadcasts match_end to
// every seat exactly once — after the final peer_status and before the
// post-match room_state — with reason all_finished and per-seat statuses,
// finishedAtMs ordering, and accepted batch/event counts.
func TestMatchEndAllFinished(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	srv := relayServer(t, &fakeStore{})

	m := startMatch(t, ctx, srv, 2, 0)
	host, guest := m.conns[0], m.conns[1]

	// Host relays 3 batches carrying 4 events total; guest stays silent (0/0).
	sendBatch(t, ctx, host, m.matchID, m.ids[0], 1, []json.RawMessage{event(1)})
	sendBatch(t, ctx, host, m.matchID, m.ids[0], 2, []json.RawMessage{event(2), event(3)})
	sendBatch(t, ctx, host, m.matchID, m.ids[0], 3, []json.RawMessage{event(4)})
	for range 3 {
		expect(t, ctx, guest, protocol.TypePeerBatch)
	}

	before := time.Now().UnixMilli()
	writeJSON(t, ctx, host, protocol.Finish{Type: protocol.TypeFinish, MatchID: m.matchID})
	ps := decodePeerStatus(t, expect(t, ctx, guest, protocol.TypePeerStatus))
	assert.Equal(t, protocol.StatusFinished, ps.Status)
	expectOwnFinished(t, ctx, host, m.ids[0])

	writeJSON(t, ctx, guest, protocol.Finish{Type: protocol.TypeFinish, MatchID: m.matchID})
	expectOwnFinished(t, ctx, guest, m.ids[1])

	// Emission point: the final peer_status, then match_end, then room_state.
	ps = decodePeerStatus(t, expect(t, ctx, host, protocol.TypePeerStatus))
	assert.Equal(t, protocol.StatusFinished, ps.Status)
	assert.Equal(t, m.ids[1], ps.PlayerID)
	me := decodeMatchEnd(t, expect(t, ctx, host, protocol.TypeMatchEnd))
	expect(t, ctx, host, protocol.TypeRoomState)
	after := time.Now().UnixMilli()

	assert.Equal(t, m.matchID, me.MatchID)
	assert.Equal(t, protocol.ReasonAllFinished, me.Reason)
	byID := resultsByID(t, me, 2)

	hostRes, guestRes := byID[m.ids[0]], byID[m.ids[1]]
	assert.Equal(t, protocol.StatusFinished, hostRes.Status)
	assert.Equal(t, 3, hostRes.BatchCount)
	assert.Equal(t, 4, hostRes.EventCount)
	require.NotNil(t, hostRes.FinishedAtMs)
	assert.Equal(t, protocol.StatusFinished, guestRes.Status)
	assert.Equal(t, 0, guestRes.BatchCount, "a silent player counts 0 batches")
	assert.Equal(t, 0, guestRes.EventCount, "a silent player counts 0 events")
	require.NotNil(t, guestRes.FinishedAtMs)
	assert.GreaterOrEqual(t, *hostRes.FinishedAtMs, before)
	assert.LessOrEqual(t, *hostRes.FinishedAtMs, *guestRes.FinishedAtMs, "host finished first")
	assert.LessOrEqual(t, *guestRes.FinishedAtMs, after)

	// The guest receives the same frame exactly once, then the room_state.
	guestMe := decodeMatchEnd(t, expect(t, ctx, guest, protocol.TypeMatchEnd))
	expect(t, ctx, guest, protocol.TypeRoomState)
	assert.Equal(t, me.MatchID, guestMe.MatchID)
	assert.Equal(t, protocol.ReasonAllFinished, guestMe.Reason)
}

// TestMatchEndDeadlineReason asserts the hard-deadline path emits match_end
// with reason deadline, every seat dnf, and no finishedAtMs.
func TestMatchEndDeadlineReason(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	srv := relayServer(t, &fakeStore{}, ws.WithDeadlineSlack(200))

	m := startMatch(t, ctx, srv, 2, 300)

	for _, c := range m.conns {
		ps := decodePeerStatus(t, expect(t, ctx, c, protocol.TypePeerStatus))
		assert.Equal(t, protocol.StatusDNF, ps.Status)
		me := decodeMatchEnd(t, expect(t, ctx, c, protocol.TypeMatchEnd))
		expect(t, ctx, c, protocol.TypeRoomState)

		assert.Equal(t, m.matchID, me.MatchID)
		assert.Equal(t, protocol.ReasonDeadline, me.Reason)
		byID := resultsByID(t, me, 2)
		for _, id := range m.ids {
			assert.Equal(t, protocol.StatusDNF, byID[id].Status)
			assert.Nil(t, byID[id].FinishedAtMs, "finishedAtMs is present only for finished")
		}
	}
}

// TestWordsAfkShareDNF asserts the words-mode AFK rule dnf's a racing seat whose
// idle SHARE of the elapsed match window crosses the threshold, ending the match
// with reason all_finished once every seat is terminal, and that the published
// afkMs/afkShare explain the verdict.
func TestWordsAfkShareDNF(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	// Trailing rule far out of reach: this test is about the SHARE rule alone.
	srv := relayServer(t, &fakeStore{}, ws.WithAfkKick(0.6, 300, 60_000))

	m := startMatchWith(t, ctx, srv, 2, wordsSettings(10))
	host, guest := m.conns[0], m.conns[1]

	// Host produces a batch and finishes; the guest never types at all, so every
	// bucket of its window is idle (share 1.0) and the sweep dnf's it.
	sendBatch(t, ctx, host, m.matchID, m.ids[0], 1, []json.RawMessage{event(1)})
	expect(t, ctx, guest, protocol.TypePeerBatch)
	writeJSON(t, ctx, host, protocol.Finish{Type: protocol.TypeFinish, MatchID: m.matchID})
	ps := decodePeerStatus(t, expect(t, ctx, guest, protocol.TypePeerStatus))
	assert.Equal(t, protocol.StatusFinished, ps.Status)
	expectOwnFinished(t, ctx, host, m.ids[0])

	ps = decodePeerStatus(t, expect(t, ctx, host, protocol.TypePeerStatus))
	assert.Equal(t, protocol.StatusDNF, ps.Status)
	assert.Equal(t, m.ids[1], ps.PlayerID)
	me := decodeMatchEnd(t, expect(t, ctx, host, protocol.TypeMatchEnd))
	expect(t, ctx, host, protocol.TypeRoomState)

	assert.Equal(t, protocol.ReasonAllFinished, me.Reason, "AFK dnfs end the match as all_finished")
	byID := resultsByID(t, me, 2)
	assert.Equal(t, protocol.StatusFinished, byID[m.ids[0]].Status)
	assert.Equal(t, 1, byID[m.ids[0]].BatchCount)
	assert.Equal(t, protocol.StatusDNF, byID[m.ids[1]].Status)
	assert.Nil(t, byID[m.ids[1]].FinishedAtMs)

	// The verdict is auditable: the silent seat is 100% idle over its window,
	// the finisher typed inside its (sub-second to ~1 s) window.
	assert.Equal(t, 1.0, byID[m.ids[1]].AfkShare, "a seat that never typed is fully AFK")
	assert.Positive(t, byID[m.ids[1]].AfkMs)
	assert.Less(t, byID[m.ids[0]].AfkShare, 0.6, "the finisher is below the kick threshold")

	// The dnf'd-but-connected seat still receives the frame directly.
	guestMe := decodeMatchEnd(t, expect(t, ctx, guest, protocol.TypeMatchEnd))
	assert.Equal(t, protocol.ReasonAllFinished, guestMe.Reason)
}

// TestWordsAfkShareSparesPause asserts the share rule does NOT punish a pause:
// two seats that keep typing around a ~1.5 s think are never dnf'd, where the
// old fixed idle timer would have killed anyone quiet for its window.
func TestWordsAfkShareSparesPause(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	// Warm-up 300 ms so the share rule is live almost immediately; the trailing
	// rule is parked at 60 s so only the share rule can speak here.
	srv := relayServer(t, &fakeStore{}, ws.WithAfkKick(0.6, 300, 60_000))

	m := startMatchWith(t, ctx, srv, 2, wordsSettings(10))
	host, guest := m.conns[0], m.conns[1]

	// Both seats type, pause ~1.5 s (idle share peaks at ~0.5), then type again.
	for seq := 1; seq <= 2; seq++ {
		sendBatch(t, ctx, host, m.matchID, m.ids[0], seq, []json.RawMessage{event(seq)})
		expect(t, ctx, guest, protocol.TypePeerBatch)
		sendBatch(t, ctx, guest, m.matchID, m.ids[1], seq, []json.RawMessage{event(seq)})
		expect(t, ctx, host, protocol.TypePeerBatch)
		time.Sleep(1500 * time.Millisecond)
	}

	// Still active participants: a dnf'd seat's batch would be rejected with
	// bad_message and never reach the peer.
	sendBatch(t, ctx, guest, m.matchID, m.ids[1], 3, []json.RawMessage{event(3)})
	pb := decodePeerBatch(t, expect(t, ctx, host, protocol.TypePeerBatch))
	assert.Equal(t, m.ids[1], pb.PlayerID, "a paused-but-typing seat is never AFK-dnf'd")
}

// TestAfkTrailingDNFInTimeMode asserts the trailing rule — the one a player
// feels: go silent long enough and you are out, in TIME mode too, where the
// share rule deliberately never fires. This is the case the share rule alone
// missed: a player who raced for a while and then walked away keeps a low idle
// share for a long time, yet is plainly not at the keyboard.
func TestAfkTrailingDNFInTimeMode(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	// Share rule out of reach (threshold 2.0 is unreachable); trailing at 1.5 s.
	srv := relayServer(t, &fakeStore{}, ws.WithAfkKick(2, 300, 1500))

	m := startMatch(t, ctx, srv, 2, 0) // time mode, 30 s duration
	host, guest := m.conns[0], m.conns[1]

	// The guest races for a moment, then stops dead.
	sendBatch(t, ctx, guest, m.matchID, m.ids[1], 1, []json.RawMessage{event(1)})
	expect(t, ctx, host, protocol.TypePeerBatch)

	// The host keeps typing, so only the silent seat may be dnf'd.
	hostSeq := 0
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		hostSeq++
		sendBatch(t, ctx, host, m.matchID, m.ids[0], hostSeq, []json.RawMessage{event(hostSeq)})
		expect(t, ctx, guest, protocol.TypePeerBatch)
		time.Sleep(300 * time.Millisecond)
	}

	ps := decodePeerStatus(t, readUntil(t, ctx, host, protocol.TypePeerStatus))
	assert.Equal(t, protocol.StatusDNF, ps.Status, "silence dnf's a seat in time mode too")
	assert.Equal(t, m.ids[1], ps.PlayerID)

	// The seat that never stopped typing is untouched: its next batch is still
	// accepted and relayed (a dnf'd sender would get bad_message instead).
	hostSeq++
	sendBatch(t, ctx, host, m.matchID, m.ids[0], hostSeq, []json.RawMessage{event(hostSeq)})
	pb := decodePeerBatch(t, readUntil(t, ctx, guest, protocol.TypePeerBatch))
	assert.Equal(t, m.ids[0], pb.PlayerID, "a typing seat is never AFK-dnf'd")
}

// TestFinishWindowDNF asserts the first finish of a words-mode match opens the
// finish window and its close dnf's every still-racing seat, ending the match
// with reason finish_window.
func TestFinishWindowDNF(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	srv := relayServer(t, &fakeStore{}, ws.WithFinishWindow(300*time.Millisecond))

	m := startMatchWith(t, ctx, srv, 2, wordsSettings(10))
	host, guest := m.conns[0], m.conns[1]

	// The slow guest produced a batch (2 events), so the idle rule leaves it
	// alone; only the finish window can dnf it.
	sendBatch(t, ctx, guest, m.matchID, m.ids[1], 1, []json.RawMessage{event(1), event(2)})
	expect(t, ctx, host, protocol.TypePeerBatch)

	writeJSON(t, ctx, host, protocol.Finish{Type: protocol.TypeFinish, MatchID: m.matchID})
	expectOwnFinished(t, ctx, host, m.ids[0])

	ps := decodePeerStatus(t, expect(t, ctx, host, protocol.TypePeerStatus))
	assert.Equal(t, protocol.StatusDNF, ps.Status)
	assert.Equal(t, m.ids[1], ps.PlayerID)
	me := decodeMatchEnd(t, expect(t, ctx, host, protocol.TypeMatchEnd))
	expect(t, ctx, host, protocol.TypeRoomState)

	assert.Equal(t, m.matchID, me.MatchID)
	assert.Equal(t, protocol.ReasonFinishWindow, me.Reason)
	byID := resultsByID(t, me, 2)
	assert.Equal(t, protocol.StatusFinished, byID[m.ids[0]].Status)
	require.NotNil(t, byID[m.ids[0]].FinishedAtMs)
	assert.Equal(t, protocol.StatusDNF, byID[m.ids[1]].Status)
	assert.Nil(t, byID[m.ids[1]].FinishedAtMs)
	assert.Equal(t, 1, byID[m.ids[1]].BatchCount)
	assert.Equal(t, 2, byID[m.ids[1]].EventCount)

	// The straggler saw the host finish, then the match end.
	ps = decodePeerStatus(t, expect(t, ctx, guest, protocol.TypePeerStatus))
	assert.Equal(t, protocol.StatusFinished, ps.Status)
	guestMe := decodeMatchEnd(t, expect(t, ctx, guest, protocol.TypeMatchEnd))
	assert.Equal(t, protocol.ReasonFinishWindow, guestMe.Reason)
}

// TestMatchEndInBacklogOnResume asserts a graced seat's backlog contains the
// match_end of a match that ended during its grace window, and the backlog is
// replayed (peer batches first, match_end last) when the seat resumes.
func TestMatchEndInBacklogOnResume(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	srv := relayServer(t, &fakeStore{}, ws.WithDeadlineSlack(200))

	// Short duration + long (default) grace: the match deadline fires while
	// the dropped guest is still graced.
	m := startMatch(t, ctx, srv, 2, 300)
	host, guest := m.conns[0], m.conns[1]

	require.NoError(t, guest.Close(websocket.StatusNormalClosure, "drop"))
	ps := decodePeerStatus(t, readUntil(t, ctx, host, protocol.TypePeerStatus))
	assert.Equal(t, protocol.StatusDisconnected, ps.Status)

	// This batch buffers into the graced guest's backlog; the host finishes.
	sendBatch(t, ctx, host, m.matchID, m.ids[0], 1, []json.RawMessage{event(1)})
	writeJSON(t, ctx, host, protocol.Finish{Type: protocol.TypeFinish, MatchID: m.matchID})
	expectOwnFinished(t, ctx, host, m.ids[0])

	// Deadline dnf's the graced guest and ends the match.
	ps = decodePeerStatus(t, expect(t, ctx, host, protocol.TypePeerStatus))
	assert.Equal(t, protocol.StatusDNF, ps.Status)
	assert.Equal(t, m.ids[1], ps.PlayerID)
	me := decodeMatchEnd(t, expect(t, ctx, host, protocol.TypeMatchEnd))
	assert.Equal(t, protocol.ReasonDeadline, me.Reason)

	// The resumer replays its backlog after hello_ok + room_state: the buffered
	// peer_batch first, the match_end last.
	guest2, ok, _ := resumeSeat(t, ctx, srv, m.tokens[1], m.ids[1])
	assert.Equal(t, m.ids[1], ok.PlayerID)
	pb := decodePeerBatch(t, expect(t, ctx, guest2, protocol.TypePeerBatch))
	assert.Equal(t, m.ids[0], pb.PlayerID)
	guestMe := decodeMatchEnd(t, expect(t, ctx, guest2, protocol.TypeMatchEnd))
	assert.Equal(t, m.matchID, guestMe.MatchID)
	assert.Equal(t, protocol.ReasonDeadline, guestMe.Reason)
	byID := resultsByID(t, guestMe, 2)
	assert.Equal(t, protocol.StatusFinished, byID[m.ids[0]].Status)
	assert.Equal(t, protocol.StatusDNF, byID[m.ids[1]].Status)
}

// TestTimeModeNoAfkShareRule asserts the SHARE rule never touches a time-mode
// match (stopping early is a legitimate way to end a timed run), while the
// trailing rule — the one that does apply there — is held out of reach here.
func TestTimeModeNoAfkShareRule(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	srv := relayServer(t, &fakeStore{},
		ws.WithAfkKick(0.1, 200, 60_000),
		ws.WithFinishWindow(200*time.Millisecond))

	m := startMatch(t, ctx, srv, 2, 0) // time mode, 30 s duration
	host, guest := m.conns[0], m.conns[1]

	// First finish: if the finish window wrongly armed, it would fire 200 ms
	// later; if the idle rule wrongly armed, the silent guest would be dnf'd
	// 200 ms after "go" (go is countdownLead = 3 s after start).
	writeJSON(t, ctx, host, protocol.Finish{Type: protocol.TypeFinish, MatchID: m.matchID})
	ps := decodePeerStatus(t, expect(t, ctx, guest, protocol.TypePeerStatus))
	assert.Equal(t, protocol.StatusFinished, ps.Status)
	// The finisher gets its own `finished` too (§ peer_status), so the host has
	// a frame waiting before anything the guest sends can reach it.
	own := decodePeerStatus(t, expect(t, ctx, host, protocol.TypePeerStatus))
	assert.Equal(t, protocol.StatusFinished, own.Status)
	assert.Equal(t, m.ids[0], own.PlayerID)

	time.Sleep(3600 * time.Millisecond) // well past go + both shrunk windows

	// The guest is still an active participant: its batch is accepted and
	// relayed (a dnf'd seat would be rejected and nothing would reach host).
	sendBatch(t, ctx, guest, m.matchID, m.ids[1], 1, []json.RawMessage{event(1)})
	pb := decodePeerBatch(t, expect(t, ctx, host, protocol.TypePeerBatch))
	assert.Equal(t, m.ids[1], pb.PlayerID)

	writeJSON(t, ctx, guest, protocol.Finish{Type: protocol.TypeFinish, MatchID: m.matchID})
	ps = decodePeerStatus(t, expect(t, ctx, host, protocol.TypePeerStatus))
	assert.Equal(t, protocol.StatusFinished, ps.Status)
	assert.Equal(t, m.ids[1], ps.PlayerID)
	me := decodeMatchEnd(t, expect(t, ctx, host, protocol.TypeMatchEnd))
	assert.Equal(t, protocol.ReasonAllFinished, me.Reason)
	byID := resultsByID(t, me, 2)
	assert.Equal(t, protocol.StatusFinished, byID[m.ids[0]].Status)
	assert.Equal(t, protocol.StatusFinished, byID[m.ids[1]].Status, "no words-mode AFK dnf in time mode")
}

// expectOwnFinished consumes the `finished` a seat now receives about ITSELF.
//
// peer_status `finished` is broadcast to every seat including the finisher
// (docs/PROTOCOL.md), so a connection that has just sent `finish` has one frame
// waiting before anything else can reach it. Consuming it explicitly, rather
// than skipping frames until the interesting one, is what keeps these tests
// able to notice an extra or missing broadcast.
func expectOwnFinished(t *testing.T, ctx context.Context, c *websocket.Conn, playerID string) {
	t.Helper()
	ps := decodePeerStatus(t, expect(t, ctx, c, protocol.TypePeerStatus))
	assert.Equal(t, protocol.StatusFinished, ps.Status)
	assert.Equal(t, playerID, ps.PlayerID, "a finisher must receive its OWN finished")
}
