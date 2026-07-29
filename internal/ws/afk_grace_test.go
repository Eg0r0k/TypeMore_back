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

// Two clocks used to run on a dropped seat at once. The reconnect grace window
// is 15 s and the trailing-AFK rule is 15 s, and the AFK sweep did not exempt a
// disconnected seat — so a drop was resolved at lastBatch + 15 s by the sweep,
// never later than the grace it was supposed to be sitting inside, and earlier
// by however long the connection had been failing before the socket died. The
// reconnect window existed in the constant and nowhere else.
//
// Now the grace window belongs to the grace timer alone. These tests pin both
// halves of that: a seat that comes back keeps racing, and a seat that does not
// come back ends exactly where it used to.

// TestGracedSeatIsNotSweptAndKeepsRacing is the reconnect the grace window is
// for. The trailing rule is set well BELOW the grace window, so under the old
// behaviour the sweep would reach the dropped seat with seconds of grace still
// on the clock.
func TestGracedSeatIsNotSweptAndKeepsRacing(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	// Share rule out of reach (2.0 is unreachable) so this is about trailing
	// alone: 2 s of silence dnf's a seat, and the grace window is three times
	// that. The production numbers are equal; making them unequal here is what
	// lets the test tell the two clocks apart.
	srv := relayServer(t, &fakeStore{}, ws.WithGrace(6*time.Second), ws.WithAfkKick(2, 300, 2000))

	m := startMatch(t, ctx, srv, 2, 30_000)
	host, guest := m.conns[0], m.conns[1]
	hostID, guestID, guestTok := m.ids[0], m.ids[1], m.tokens[1]
	waitForGo(t, m)

	// Both seats type once, so each has a real lastBatch to be silent since.
	sendBatch(t, ctx, guest, m.matchID, guestID, 1, []json.RawMessage{event(1)})
	expect(t, ctx, host, protocol.TypePeerBatch)
	sendBatch(t, ctx, host, m.matchID, hostID, 1, []json.RawMessage{event(2)})
	expect(t, ctx, guest, protocol.TypePeerBatch)

	// The guest drops at what the docs call the tenth second.
	require.NoError(t, guest.Close(websocket.StatusNormalClosure, "drop"))
	ps := decodePeerStatus(t, readUntil(t, ctx, host, protocol.TypePeerStatus))
	require.Equal(t, protocol.StatusDisconnected, ps.Status)
	require.Equal(t, guestID, ps.PlayerID)

	// The host keeps typing for the rest of the test: the trailing rule applies
	// to it too, and a dnf'd host would end the match out from under the seat
	// this test is about.
	stopHost := keepTyping(host, m.matchID, hostID, 1)
	defer stopHost()

	// Three seconds away — three sweep ticks, and half again the trailing rule,
	// with half the grace window still unspent.
	time.Sleep(3 * time.Second)

	// Back inside the grace window, on the same resume token.
	guest2 := dialAs(t, ctx, srv, "")
	writeJSON(t, ctx, guest2, protocol.Hello{
		Type: protocol.TypeHello, ProtocolVersion: protocol.Version, ResumeToken: guestTok})
	var ok protocol.HelloOK
	require.NoError(t, json.Unmarshal(expect(t, ctx, guest2, protocol.TypeHelloOK), &ok))
	require.Equal(t, guestID, ok.PlayerID, "the resume token must return the same seat")

	// Deliberately do NOT type yet. A sweep tick lands inside this wait, and it
	// is the tick that decides whether the reconnect meant anything: the seat
	// has been silent on the wire for over four seconds, twice the trailing
	// rule. Coming back restarted that clock, so what the sweep sees is 1.5 s of
	// silence, inside the budget. Without the restart the outage would be spent
	// retroactively and this tick would dnf a player who is sitting there
	// typing.
	time.Sleep(1500 * time.Millisecond)

	// The seat is still racing: its next batch is accepted and relayed. A dnf'd
	// sender would be answered with bad_message and the host would never see it.
	sendBatch(t, ctx, guest2, m.matchID, guestID, 2, []json.RawMessage{event(99)})
	frame, skipped := readUntilCollecting(t, ctx, host, protocol.TypePeerBatch)
	assert.Equal(t, guestID, decodePeerBatch(t, frame).PlayerID)

	// And nothing along the way dnf'd it — not the sweep during the outage, not
	// the tick after the reconnect. This is the assertion that fails against the
	// old behaviour, twice over.
	for _, raw := range skipped {
		if frameType(t, raw) != protocol.TypePeerStatus {
			continue
		}
		ps := decodePeerStatus(t, raw)
		assert.NotEqual(t, protocol.StatusDNF, ps.Status,
			"seat %s was dnf'd across a drop it reconnected from", ps.PlayerID)
	}
}

// TestUnresumedDropStillDnfsAtGraceExpiry is the other half: nothing changes for
// a player who does not come back. The seat is dnf'd, the match ends, and the
// capture records the dnf — the same outcome as before, reached through the
// grace timer instead of the sweep.
func TestUnresumedDropStillDnfsAtGraceExpiry(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	// Both clocks live, and the sweep is the faster one — exactly the shape
	// that used to make the sweep win.
	store := &fakeStore{}
	srv := relayServer(t, store, ws.WithGrace(2*time.Second), ws.WithAfkKick(2, 300, 1000))

	m := startMatch(t, ctx, srv, 2, 30_000)
	host, guest := m.conns[0], m.conns[1]
	hostID, guestID := m.ids[0], m.ids[1]
	waitForGo(t, m)

	sendBatch(t, ctx, guest, m.matchID, guestID, 1, []json.RawMessage{event(1)})
	expect(t, ctx, host, protocol.TypePeerBatch)

	require.NoError(t, guest.Close(websocket.StatusNormalClosure, "drop"))
	// The host has sent nothing yet, so its pump starts the batch sequence at 1.
	stopHost := keepTyping(host, m.matchID, hostID, 0)
	ps := decodePeerStatus(t, readUntil(t, ctx, host, protocol.TypePeerStatus))
	require.Equal(t, protocol.StatusDisconnected, ps.Status)

	// Grace expires with no resume: dnf, as it always was.
	ps = decodePeerStatus(t, readUntil(t, ctx, host, protocol.TypePeerStatus))
	assert.Equal(t, protocol.StatusDNF, ps.Status)
	assert.Equal(t, guestID, ps.PlayerID)

	stopHost()
	writeJSON(t, ctx, host, protocol.Finish{Type: protocol.TypeFinish, MatchID: m.matchID})
	require.Eventually(t, func() bool { return len(store.records()) == 1 },
		10*time.Second, 20*time.Millisecond)

	byID := map[string]string{}
	for _, run := range store.records()[0].Runs {
		byID[run.PlayerID] = run.FinalStatus
	}
	assert.Equal(t, protocol.StatusDNF, byID[guestID], "an unresumed drop is still a dnf")
	assert.Equal(t, protocol.StatusFinished, byID[hostID])
}

// keepTyping sends a batch from a seat every 250 ms until the returned stop is
// called. These tests shrink the trailing rule to a second, which applies to
// EVERY racing seat — so the seat that is meant to survive has to keep producing
// batches while the test waits on the seat that is not.
//
// fromSeq is the last batchSeq the seat has already sent; the pump continues
// from fromSeq+1, because the room requires the sequence to be contiguous.
//
// It runs off the test goroutine, so it reports nothing: a write that fails just
// ends the pump, and the assertions the test actually makes will notice.
func keepTyping(c *websocket.Conn, matchID, playerID string, fromSeq int) (stop func()) {
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		seq := fromSeq
		for {
			select {
			case <-done:
				return
			case <-time.After(250 * time.Millisecond):
				seq++
				b, err := json.Marshal(protocol.EventBatch{
					Type: protocol.TypeEventBatch, MatchID: matchID, PlayerID: playerID,
					BatchSeq: seq, Version: 1, Events: []json.RawMessage{event(seq)},
				})
				if err != nil {
					return
				}
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				err = c.Write(ctx, websocket.MessageText, b)
				cancel()
				if err != nil {
					return
				}
			}
		}
	}()
	return func() {
		close(done)
		<-finished
	}
}

// waitForGo blocks until the match's countdown has reached t=0. Before it, every
// seat's lastBatch is the go time itself and the sweep cannot fire, so a test
// that means to exercise the sweep has to be past it.
func waitForGo(t *testing.T, m match) {
	t.Helper()
	require.NotZero(t, m.goAtMs, "the harness did not record the countdown's go time")
	if wait := time.Until(time.UnixMilli(m.goAtMs)); wait > 0 {
		time.Sleep(wait)
	}
}

// readUntilCollecting is readUntil that also hands back the frames it skipped,
// so a test can assert about what did NOT arrive on the way.
//
// It carries its own short deadline. The interesting failure here is a frame
// that never comes — a batch from a seat the sweep dnf'd is answered with
// bad_message and never reaches the peer — and waiting out the whole test
// context for that reports it as a stalled read instead of as what it is.
func readUntilCollecting(t *testing.T, ctx context.Context, c *websocket.Conn, typ string) ([]byte, [][]byte) {
	t.Helper()
	deadline, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var skipped [][]byte
	for range 30 {
		_, data, err := c.Read(deadline)
		if err != nil {
			t.Fatalf("no %q frame arrived (%v); saw %s", typ, err, frameTypes(t, skipped))
		}
		if frameType(t, data) == typ {
			return data, skipped
		}
		skipped = append(skipped, data)
	}
	t.Fatalf("did not receive a %q frame; saw %s", typ, frameTypes(t, skipped))
	return nil, nil
}

func frameTypes(t *testing.T, frames [][]byte) []string {
	t.Helper()
	out := make([]string, 0, len(frames))
	for _, f := range frames {
		out = append(out, frameType(t, f))
	}
	return out
}

func frameType(t *testing.T, raw []byte) string {
	t.Helper()
	var env struct {
		Type string `json:"type"`
	}
	require.NoError(t, json.Unmarshal(raw, &env))
	return env.Type
}
