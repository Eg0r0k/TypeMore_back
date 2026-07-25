//go:build load

package ws_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"runtime"
	"runtime/pprof"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"

	"github.com/typemore/typemore-server/internal/perf"
	"github.com/typemore/typemore-server/internal/protocol"
	"github.com/typemore/typemore-server/internal/ws"
)

// Zone 7 — WebSocket relay fan-out.
//
// Everything here drives REAL clients over a real httptest server, because the
// properties under measurement are transport-shaped: Room.relayEventBatch
// marshals and enqueues under the room lock, session.trySend drops rather than
// blocks, and the 256-slot outbound queue is all that stands between one
// stalled reader and everybody else's frame rate. A mocked room measures none
// of that.

// Workload shape. A 150 wpm typist emits 150*5/60 = 12.5 keystrokes per second,
// and docs/PROTOCOL.md §3 obliges the client to flush every 100 ms, so the
// average batch carries 1.25 events. loadEventsFor reproduces exactly that
// average (two events on every fourth batch, one otherwise) instead of rounding
// up to 2 and inflating the wire by 60 %.
const (
	loadSeats       = 5
	loadBatchPeriod = 100 * time.Millisecond
	loadRooms       = 50
	// loadWindow is the measured send window of the headline fan-out run. The
	// brief asks for 60 s and that is what runs: at 50 rooms it costs one minute
	// of a 45-minute suite and yields ~600 k latency samples, which is what a
	// p99 with three significant figures needs.
	loadWindow = 60 * time.Second
	// loadDrain keeps readers alive past the last write so a batch still in
	// flight counts as delivered rather than lost.
	loadDrain = 3 * time.Second
	// loadMatchMs parks the hard deadline (goAt + duration + 30 s slack) far
	// past every window below, so no match ends underneath a measurement.
	loadMatchMs = 900_000
)

// loadP99Budget is the brief's threshold of interest: past 50 ms of relay
// latency a peer's caret visibly trails their keystrokes, which is the whole
// product promise of a live race.
const loadP99Budget = 50 * time.Millisecond

// loadEpoch anchors every timestamp in this file. Intervals are taken with
// time.Since so they use Go's monotonic reading; on this Windows box that
// counter resolves to about 10 µs with occasional 0.5 ms steps, which is the
// noise floor under every latency reported below.
var loadEpoch = time.Now()

func loadNanos() int64 { return int64(time.Since(loadEpoch)) }

// loadServer starts the /ws endpoint wired like production. Every load client
// is a guest (nil identity resolver): authentication is zone 1's subject.
func loadServer(t *testing.T, store ws.MatchStore, opts ...ws.Option) *httptest.Server {
	t.Helper()
	r := chi.NewRouter()
	r.Handle("/ws", ws.NewHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil, store, opts...))
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}

// --- client harness ---------------------------------------------------------

// loadFrame is the minimal decode a measurement needs: the discriminator, the
// relaying peer, and the batchSeq marker the sender stamped into its events.
// Unknown fields are ignored, so this one shape decodes every server frame.
type loadFrame struct {
	Type     string `json:"type"`
	PlayerID string `json:"playerId"`
	Status   string `json:"status"`
	Events   []struct {
		Seq int `json:"seq"`
	} `json:"events"`
}

// loadConn is one measured WebSocket client.
type loadConn struct {
	c        *websocket.Conn
	room     *loadRoom
	seat     int
	playerID string

	// sentAt[seq] is loadNanos() at the instant this client wrote batch seq.
	// It is published atomically because the correlating read happens on a
	// different goroutine and the socket gives the race detector no
	// happens-before edge to lean on.
	sentAt  []atomic.Int64
	written atomic.Int64

	// recording gates latency sampling so a sweep point can discard its warm-up
	// without discarding the delivery accounting.
	recording atomic.Bool
	matchEnd  atomic.Bool

	// Arrival stamps (loadNanos) zone 8 uses to place the room's own progress
	// against the persistence burst running beside it.
	matchEndAt   atomic.Int64
	roomStateAt  atomic.Int64 // first room_state AFTER match_end: the room is back in the lobby
	peerFinished atomic.Int64
	chatArmed    atomic.Bool
	chatAt       atomic.Int64

	// The fields below belong to this client's reader goroutine until it exits.
	lat    []time.Duration
	got    [][]int32 // got[senderSeat][batchSeq]
	frames int64

	readErr  error
	writeErr error
	// slow marks a client that deliberately never reads (the slow-consumer
	// scenario): no reader goroutine is started for it, so nothing drains its
	// socket until the scenario inspects it by hand.
	slow bool
}

// loadRoom is one built, started room.
type loadRoom struct {
	conns   []*loadConn
	matchID string
	code    string
}

func (lr *loadRoom) seatOf(playerID string) int {
	for i, lc := range lr.conns {
		if lc.playerID == playerID {
			return i
		}
	}
	return -1
}

// loadWriteJSON marshals and writes one frame under a bounded deadline, so a
// stalled socket surfaces as an error instead of wedging the run. The deadline
// hangs off Background, not the caller's context: coder/websocket CLOSES the
// connection when a read/write context is cancelled, and these helpers must
// leave the connection usable.
func loadWriteJSON(c *websocket.Conn, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return c.Write(ctx, websocket.MessageText, b)
}

// loadReadUntil reads frames until one carries type typ, discarding the rest.
// Setup traffic (room_state, chat) interleaves in ways that exact-order
// assertions would only make brittle; the target types here are unambiguous.
func loadReadUntil(ctx context.Context, c *websocket.Conn, typ string, limit int) ([]byte, error) {
	for range limit {
		_, data, err := c.Read(ctx)
		if err != nil {
			return nil, err
		}
		var f loadFrame
		if err := json.Unmarshal(data, &f); err != nil {
			return nil, err
		}
		if f.Type == typ {
			return data, nil
		}
	}
	return nil, fmt.Errorf("no %q frame within %d frames", typ, limit)
}

// buildLoadRoom seats `seats` guests, freezes a long time-mode match, and
// returns with every connection drained to just past its countdown.
func buildLoadRoom(ctx context.Context, srv *httptest.Server, seats, maxSeq int) (*loadRoom, error) {
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
	lr := &loadRoom{}
	for i := range seats {
		c, _, err := websocket.Dial(ctx, url, nil)
		if err != nil {
			return nil, fmt.Errorf("dial seat %d: %w", i, err)
		}
		c.SetReadLimit(1 << 20)
		if err := loadWriteJSON(c, protocol.Hello{Type: protocol.TypeHello, ProtocolVersion: protocol.Version}); err != nil {
			return nil, err
		}
		data, err := loadReadUntil(ctx, c, protocol.TypeHelloOK, 4)
		if err != nil {
			return nil, err
		}
		var ok protocol.HelloOK
		if err := json.Unmarshal(data, &ok); err != nil {
			return nil, err
		}
		lc := &loadConn{c: c, room: lr, seat: i, playerID: ok.PlayerID, sentAt: make([]atomic.Int64, maxSeq+1)}
		lc.got = make([][]int32, seats)
		for s := range lc.got {
			lc.got[s] = make([]int32, maxSeq+1)
		}
		lr.conns = append(lr.conns, lc)

		if i == 0 {
			if err := loadWriteJSON(c, protocol.CreateRoom{Type: protocol.TypeCreateRoom}); err != nil {
				return nil, err
			}
			data, err := loadReadUntil(ctx, c, protocol.TypeRoomState, 4)
			if err != nil {
				return nil, err
			}
			var st protocol.RoomState
			if err := json.Unmarshal(data, &st); err != nil {
				return nil, err
			}
			lr.code = st.Code
			// The default 30 s time mode would hit its hard deadline inside the
			// measurement window, so the host stretches it before anyone joins:
			// settings_update is rejected once a match runs, and it resets every
			// ready flag, so it has to happen first.
			ns := protocol.DefaultSettings("Load")
			ns.DurationMs = loadMatchMs
			if err := loadWriteJSON(c, protocol.SettingsUpdate{Type: protocol.TypeSettingsUpdate, Settings: ns}); err != nil {
				return nil, err
			}
			if _, err := loadReadUntil(ctx, c, protocol.TypeRoomState, 8); err != nil {
				return nil, err
			}
			continue
		}
		if err := loadWriteJSON(c, protocol.JoinRoom{Type: protocol.TypeJoinRoom, Code: lr.code}); err != nil {
			return nil, err
		}
		if _, err := loadReadUntil(ctx, c, protocol.TypeRoomState, 32); err != nil {
			return nil, err
		}
	}

	// Ready up, confirming against the HOST's view after each one: `ready` is
	// processed on the sender's own read goroutine, so an unsynchronised
	// start_match can legitimately overtake it and be rejected with not_ready.
	for i := 1; i < seats; i++ {
		if err := loadWriteJSON(lr.conns[i].c, protocol.Ready{Type: protocol.TypeReady}); err != nil {
			return nil, err
		}
		if err := loadWaitReady(ctx, lr.conns[0].c, lr.conns[0].playerID, i); err != nil {
			return nil, err
		}
	}
	if err := loadWriteJSON(lr.conns[0].c, protocol.StartMatch{Type: protocol.TypeStartMatch}); err != nil {
		return nil, err
	}
	for _, lc := range lr.conns {
		data, err := loadReadUntil(ctx, lc.c, protocol.TypeCountdown, 64)
		if err != nil {
			return nil, fmt.Errorf("seat %d countdown: %w", lc.seat, err)
		}
		var cd protocol.Countdown
		if err := json.Unmarshal(data, &cd); err != nil {
			return nil, err
		}
		if lr.matchID == "" {
			lr.matchID = cd.MatchID
		} else if lr.matchID != cd.MatchID {
			return nil, errors.New("seats disagree on matchId")
		}
	}
	return lr, nil
}

// loadWaitReady reads the host's room_state stream until at least n non-host
// seats report ready.
func loadWaitReady(ctx context.Context, host *websocket.Conn, hostID string, n int) error {
	for range 64 {
		data, err := loadReadUntil(ctx, host, protocol.TypeRoomState, 64)
		if err != nil {
			return err
		}
		var st protocol.RoomState
		if err := json.Unmarshal(data, &st); err != nil {
			return err
		}
		ready := 0
		for _, p := range st.Players {
			if p.PlayerID != hostID && p.Ready {
				ready++
			}
		}
		if ready >= n {
			return nil
		}
	}
	return fmt.Errorf("host never saw %d ready seats", n)
}

// buildLoadRooms builds rooms sequentially on the test goroutine: one room is a
// dozen ordered round trips over loopback, so even 200 of them cost about a
// second, and building here keeps require out of worker goroutines.
func buildLoadRooms(t *testing.T, srv *httptest.Server, rooms, seats, maxSeq int) []*loadRoom {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	built := make([]*loadRoom, 0, rooms)
	start := time.Now()
	for i := range rooms {
		lr, err := buildLoadRoom(ctx, srv, seats, maxSeq)
		require.NoErrorf(t, err, "build room %d of %d", i, rooms)
		built = append(built, lr)
	}
	t.Logf("built %d rooms x %d seats in %s (%d goroutines live)",
		rooms, seats, time.Since(start).Round(time.Millisecond), runtime.NumGoroutine())
	return built
}

// loadSettle waits for the PREVIOUS test's teardown to finish before the next
// workload starts. A dropped seat survives its reconnect grace (15 s by
// default) with its match, AFK ticker and timers still live, so a suite that
// opens the next five hundred sockets immediately ends up measuring the run
// before it as well. It waits for the goroutine count to stop falling.
func loadSettle(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	prev := runtime.NumGoroutine()
	steady := 0
	for time.Now().Before(deadline) && steady < 4 {
		time.Sleep(500 * time.Millisecond)
		n := runtime.NumGoroutine()
		if n < prev {
			steady = 0
		} else {
			steady++
		}
		prev = n
	}
	runtime.GC()
	t.Logf("settled at %d goroutines", runtime.NumGoroutine())
}

func closeLoadRooms(rooms []*loadRoom) {
	for _, lr := range rooms {
		for _, lc := range lr.conns {
			_ = lc.c.Close(websocket.StatusNormalClosure, "")
		}
	}
}

// loadEventsFor is the 150 wpm mapping: 12.5 keystrokes/s over 100 ms batches
// is 1.25 events per batch — two events on every fourth batch, one on the rest.
func loadEventsFor(seq int) int {
	if seq%4 == 0 {
		return 2
	}
	return 1
}

// loadBatchFrame renders one event_batch by hand into buf. At 2 500 frames per
// second across the process a client-side json.Marshal would land inside the
// latency the client is trying to measure; the frame shape is fixed and tiny,
// so emitting the bytes directly keeps the measurement about the server.
func loadBatchFrame(buf []byte, matchID, playerID string, seq, events int) []byte {
	buf = append(buf[:0], `{"type":"event_batch","matchId":"`...)
	buf = append(buf, matchID...)
	buf = append(buf, `","playerId":"`...)
	buf = append(buf, playerID...)
	buf = append(buf, `","batchSeq":`...)
	buf = strconv.AppendInt(buf, int64(seq), 10)
	buf = append(buf, `,"version":1,"events":[`...)
	for i := range events {
		if i > 0 {
			buf = append(buf, ',')
		}
		// The batchSeq travels inside the opaque event body because peer_batch
		// carries only playerId and events — the relayed frame has no seq, so
		// (playerId, events[0].seq) is the only correlation key a receiver can
		// build without adding a counter to production code.
		buf = append(buf, `{"k":"insert","seq":`...)
		buf = strconv.AppendInt(buf, int64(seq), 10)
		buf = append(buf, `,"t":`...)
		buf = strconv.AppendInt(buf, int64(seq*80+i*40), 10)
		buf = append(buf, `,"ch":"x"}`...)
	}
	return append(buf, `]}`...)
}

// writeOne writes a prepared frame under its own background deadline. Deriving
// it from the run context would hand coder/websocket a cancellation that closes
// the socket, which the slow-consumer scenario needs to survive.
func (lc *loadConn) writeOne(frame []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return lc.c.Write(ctx, websocket.MessageText, frame)
}

// writeBatches paces one client's event_batch stream until stop closes.
func (lc *loadConn) writeBatches(matchID string, period time.Duration, stop <-chan struct{}) {
	tick := time.NewTicker(period)
	defer tick.Stop()
	buf := make([]byte, 0, 512)
	seq := 0
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
		}
		seq++
		if seq >= len(lc.sentAt) {
			return // the window outran the preallocated sequence space
		}
		buf = loadBatchFrame(buf, matchID, lc.playerID, seq, loadEventsFor(seq))
		lc.sentAt[seq].Store(loadNanos())
		if err := lc.writeOne(buf); err != nil {
			lc.writeErr = err
			return
		}
		lc.written.Store(int64(seq))
	}
}

// blastBatches writes n batches back to back with no pacing — the saturation
// probe used to find a single room's serialised ceiling.
func (lc *loadConn) blastBatches(matchID string, n int) {
	buf := make([]byte, 0, 512)
	for seq := 1; seq <= n && seq < len(lc.sentAt); seq++ {
		buf = loadBatchFrame(buf, matchID, lc.playerID, seq, loadEventsFor(seq))
		lc.sentAt[seq].Store(loadNanos())
		if err := lc.writeOne(buf); err != nil {
			lc.writeErr = err
			return
		}
		lc.written.Store(int64(seq))
	}
}

// blastPacedBatches writes n batches in bursts of perTick every period. It sits
// between the paced workload and the unpaced blast: fast enough to overrun a
// stalled socket, slow enough that a healthy in-process client still drains its
// own frames, so the stalled seat's loss is not confounded with harness
// backpressure.
func (lc *loadConn) blastPacedBatches(matchID string, n, perTick int, period time.Duration) {
	tick := time.NewTicker(period)
	defer tick.Stop()
	buf := make([]byte, 0, 512)
	seq := 0
	for seq < n && seq < len(lc.sentAt)-1 {
		<-tick.C
		for range perTick {
			if seq >= n || seq >= len(lc.sentAt)-1 {
				break
			}
			seq++
			buf = loadBatchFrame(buf, matchID, lc.playerID, seq, loadEventsFor(seq))
			lc.sentAt[seq].Store(loadNanos())
			if err := lc.writeOne(buf); err != nil {
				lc.writeErr = err
				return
			}
			lc.written.Store(int64(seq))
		}
	}
}

// readFrames drains this client's socket, timing every peer_batch against the
// instant its sender wrote it and counting arrivals per (sender, batchSeq).
func (lc *loadConn) readFrames(ctx context.Context) {
	for {
		_, data, err := lc.c.Read(ctx)
		if err != nil {
			lc.readErr = err
			return
		}
		now := loadNanos()
		lc.frames++
		var f loadFrame
		if err := json.Unmarshal(data, &f); err != nil {
			lc.readErr = err
			return
		}
		switch f.Type {
		case protocol.TypeMatchEnd:
			lc.matchEndAt.CompareAndSwap(0, now)
			lc.matchEnd.Store(true)
		case protocol.TypeRoomState:
			// The post-match room_state is the LAST thing endMatchLocked does,
			// still holding the room lock — so its arrival dates the moment the
			// room was free again.
			if lc.matchEnd.Load() {
				lc.roomStateAt.CompareAndSwap(0, now)
			}
		case protocol.TypeChat:
			if lc.chatArmed.Load() {
				lc.chatAt.CompareAndSwap(0, now)
			}
		case protocol.TypePeerStatus:
			if f.Status == protocol.StatusFinished {
				lc.peerFinished.Add(1)
			}
		case protocol.TypePeerBatch:
			sender := lc.room.seatOf(f.PlayerID)
			if sender < 0 || len(f.Events) == 0 {
				continue
			}
			seq := f.Events[0].Seq
			if seq <= 0 || seq >= len(lc.got[sender]) {
				continue
			}
			lc.got[sender][seq]++
			if sent := lc.room.conns[sender].sentAt[seq].Load(); sent > 0 && lc.recording.Load() {
				lc.lat = append(lc.lat, time.Duration(now-sent))
			}
		}
	}
}

// --- workload driver ---------------------------------------------------------

// relayResult is one workload run's accounting.
type relayResult struct {
	lat     []time.Duration
	sent    int64 // event_batch frames the transport accepted
	relays  int64 // peer_batches that should have arrived: sent * (seats-1)
	arrived int64
	lost    int64
	dupes   int64
	wall    time.Duration
}

// workload is a running fan-out: readers are live until stop is called, which
// is what lets a scenario keep using the connections after the send window.
type workload struct {
	rooms   []*loadRoom
	cancel  context.CancelFunc
	readers *sync.WaitGroup
	wall    time.Duration
}

// startWorkload starts a reader per responsive client and a paced writer per
// client, holds the send window open, then drains.
func startWorkload(rooms []*loadRoom, window, warmup, drain time.Duration) *workload {
	ctx, cancel := context.WithCancel(context.Background())
	w := &workload{rooms: rooms, cancel: cancel, readers: &sync.WaitGroup{}}
	for _, lr := range rooms {
		for _, lc := range lr.conns {
			if lc.slow {
				continue
			}
			w.readers.Add(1)
			go func() { defer w.readers.Done(); lc.readFrames(ctx) }()
		}
	}
	var writers sync.WaitGroup
	stop := make(chan struct{})
	start := time.Now()
	for _, lr := range rooms {
		for _, lc := range lr.conns {
			writers.Add(1)
			go func() { defer writers.Done(); lc.writeBatches(lr.matchID, loadBatchPeriod, stop) }()
		}
	}
	if warmup > 0 {
		time.Sleep(warmup)
	}
	for _, lr := range rooms {
		for _, lc := range lr.conns {
			lc.recording.Store(true)
		}
	}
	time.Sleep(window - warmup)
	close(stop)
	writers.Wait()
	w.wall = time.Since(start)
	time.Sleep(drain)
	return w
}

// stop ends the readers and reconciles what was sent against what arrived.
// Cancelling a coder/websocket read context closes the connection, so this is
// also the point at which the sockets die.
func (w *workload) stop() relayResult {
	w.cancel()
	w.readers.Wait()

	res := relayResult{wall: w.wall}
	for _, lr := range w.rooms {
		peers := int64(len(lr.conns) - 1)
		for _, sender := range lr.conns {
			n := sender.written.Load()
			res.sent += n
			res.relays += n * peers
			for _, rcv := range lr.conns {
				if rcv == sender {
					continue
				}
				for seq := int64(1); seq <= n; seq++ {
					switch got := rcv.got[sender.seat][seq]; got {
					case 0:
						res.lost++
					case 1:
						res.arrived++
					default:
						res.arrived++
						res.dupes += int64(got - 1)
					}
				}
			}
			res.lat = append(res.lat, sender.lat...)
		}
	}
	return res
}

func runRelayWorkload(rooms []*loadRoom, window, warmup, drain time.Duration) relayResult {
	return startWorkload(rooms, window, warmup, drain).stop()
}

// assertZero holds a count that must be exactly zero, in the reporting shape
// perf.Budget uses so the report table stays uniform.
func assertZero(t *testing.T, zone, workload string, measured int64, rationale string) {
	t.Helper()
	t.Logf("BUDGET %s | %s | measured %d | limit 0 | %s", zone, workload, measured, rationale)
	if measured != 0 {
		t.Errorf("BUDGET MISSED %s | %s | measured %d, expected 0", zone, workload, measured)
	}
}

// --- the headline fan-out run ------------------------------------------------

// TestLoadRelayFanout is the zone's headline: 50 rooms x 5 clients, every client
// flushing a batch every 100 ms for a full minute, measured end to end through
// real sockets.
func TestLoadRelayFanout(t *testing.T) {
	loadSettle(t)
	srv := loadServer(t, nil)
	maxSeq := int(loadWindow/loadBatchPeriod) + 64
	rooms := buildLoadRooms(t, srv, loadRooms, loadSeats, maxSeq)
	defer closeLoadRooms(rooms)

	res := runRelayWorkload(rooms, loadWindow, 0, loadDrain)
	require.NotEmpty(t, res.lat, "no latency samples: the workload never relayed anything")

	perf.Report(t, "7", "shape", fmt.Sprintf("%d rooms x %d clients, one batch per client every %s for %s",
		loadRooms, loadSeats, loadBatchPeriod, loadWindow))
	perf.Report(t, "7", "relay latency distribution", perf.Summary(res.lat))
	perf.Report(t, "7", "batches sent / relays expected / relays arrived",
		fmt.Sprintf("%d / %d / %d", res.sent, res.relays, res.arrived))

	perf.Budget{
		Zone:      "7",
		Workload:  fmt.Sprintf("relay p99, %d rooms x %d clients", loadRooms, loadSeats),
		Limit:     loadP99Budget,
		Rationale: "past 50 ms a peer's caret visibly trails their keystrokes, which is the product promise of a live race",
	}.Assert(t, perf.Percentile(res.lat, 99))

	assertZero(t, "7", "dropped peer_batches (responsive clients)", res.lost,
		"trySend may only shed frames for a stalled consumer; a client reading in real time must never lose one")
	assertZero(t, "7", "duplicated peer_batches", res.dupes,
		"the relay is lossless and exactly-once per peer (docs/MATCH.md §1)")

	cpus := runtime.NumCPU()
	batchesPerSec := float64(res.sent) / res.wall.Seconds()
	relaysPerSec := float64(res.arrived) / res.wall.Seconds()
	perf.Report(t, "7", "CPU", fmt.Sprintf("NumCPU=%d GOMAXPROCS=%d", cpus, runtime.GOMAXPROCS(0)))
	perf.Report(t, "7", "wall time", res.wall.Round(time.Millisecond))
	perf.Report(t, "7", "throughput", fmt.Sprintf("%.0f batches/s in, %.0f peer_batches/s out, %.0f relays/s/core",
		batchesPerSec, relaysPerSec, relaysPerSec/float64(cpus)))
}

// --- the capacity curve ------------------------------------------------------

// A sweep point is short on purpose: each one rebuilds every room and socket
// from scratch, and the curve's SHAPE — not a three-digit p99 per point — is
// what capacity planning asks for.
const (
	sweepWindow = 12 * time.Second
	sweepWarmup = 2 * time.Second
)

// TestLoadRelayRoomSweep walks the room count until p99 crosses 50 ms: how many
// concurrent 5-player rooms one instance carries before the relay is felt.
func TestLoadRelayRoomSweep(t *testing.T) {
	loadSettle(t)
	maxSeq := int(sweepWindow/loadBatchPeriod) + 64
	type point struct {
		rooms int
		p99   time.Duration
	}
	var curve []point
	crossed := 0
	for _, n := range []int{10, 25, 50, 100, 200} {
		// A fresh server per point, with a short grace so the previous point's
		// abandoned seats cannot linger as ticking matches beside this one.
		srv := loadServer(t, nil, ws.WithGrace(500*time.Millisecond))
		rooms := buildLoadRooms(t, srv, n, loadSeats, maxSeq)
		res := runRelayWorkload(rooms, sweepWindow, sweepWarmup, 2*time.Second)
		closeLoadRooms(rooms)
		require.NotEmptyf(t, res.lat, "%d rooms: no latency samples", n)

		p99 := perf.Percentile(res.lat, 99)
		curve = append(curve, point{rooms: n, p99: p99})
		perf.Report(t, "7", fmt.Sprintf("p99 @ %d rooms (%d clients)", n, n*loadSeats),
			fmt.Sprintf("%s | %s | lost=%d dupes=%d | %.0f relays/s",
				p99.Round(10*time.Microsecond), perf.Summary(res.lat), res.lost, res.dupes,
				float64(res.arrived)/res.wall.Seconds()))

		// Let this point's sockets and goroutines retire before the next opens
		// another thousand: a point measured on top of the previous one's
		// teardown is not a point on this curve.
		loadSettle(t)
		if p99 > loadP99Budget {
			crossed = n
			break
		}
	}
	var b strings.Builder
	for i, p := range curve {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%d rooms=%s", p.rooms, p.p99.Round(10*time.Microsecond))
	}
	perf.Report(t, "7", "p99 vs room count", b.String())
	if crossed == 0 {
		perf.Report(t, "7", "room count at which p99 crosses 50 ms",
			fmt.Sprintf(">%d (never crossed on this box)", curve[len(curve)-1].rooms))
		return
	}
	perf.Report(t, "7", "room count at which p99 crosses 50 ms", crossed)
}

// --- room-mutex contention ---------------------------------------------------

// mutexCyclesPerSecond reads the conversion the runtime uses for mutex-profile
// delays. runtime.BlockProfileRecord.Cycles is in CPU cycles and the textual
// profile header is the only place the runtime publishes the rate.
func mutexCyclesPerSecond() (float64, error) {
	var buf bytes.Buffer
	if err := pprof.Lookup("mutex").WriteTo(&buf, 1); err != nil {
		return 0, err
	}
	for line := range strings.SplitSeq(buf.String(), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "cycles/second="); ok {
			return strconv.ParseFloat(v, 64)
		}
	}
	return 0, errors.New("mutex profile carried no cycles/second header")
}

// roomLockDelay sums the mutex-profile contention charged to ws.Room stacks.
// The mutex profile attributes a wait to the stack of the goroutine that
// RELEASED the lock, so the room's own methods are the attribution point.
func roomLockDelay(hz float64) (time.Duration, int64) {
	recs := make([]runtime.BlockProfileRecord, 512)
	for {
		n, ok := runtime.MutexProfile(recs)
		if ok {
			recs = recs[:n]
			break
		}
		recs = make([]runtime.BlockProfileRecord, n+128)
	}
	var cycles, count int64
	for i := range recs {
		r := &recs[i]
		if !stackMentions(r.Stack(), "internal/ws.(*Room)") {
			continue
		}
		cycles += r.Cycles
		count += r.Count
	}
	return time.Duration(float64(cycles) / hz * float64(time.Second)), count
}

func stackMentions(pcs []uintptr, want string) bool {
	frames := runtime.CallersFrames(pcs)
	for {
		f, more := frames.Next()
		if strings.Contains(f.Function, want) {
			return true
		}
		if !more {
			return false
		}
	}
}

// TestLoadRelayLockContention measures how much of the workload waits on a
// room's mutex, two ways, because neither is sufficient alone:
//
//   - the runtime mutex profile, which reports the exact delay attributed to
//     ws.Room stacks but perturbs the run it measures (every contention event is
//     sampled), so its latency numbers are NOT the fan-out test's;
//   - a saturation probe, which drives ONE room as hard as it will go to find a
//     single room's serialised ceiling and compares production demand against
//     it. Its limit: the clients share this process's CPU with the server, so
//     the measured ceiling is a floor on the real one.
func TestLoadRelayLockContention(t *testing.T) {
	loadSettle(t)
	hzBefore, err := mutexCyclesPerSecond()
	require.NoError(t, err)

	runtime.SetMutexProfileFraction(1)
	defer runtime.SetMutexProfileFraction(0)
	beforeDelay, beforeCount := roomLockDelay(hzBefore)

	const window = 15 * time.Second
	maxSeq := int(window/loadBatchPeriod) + 64
	srv := loadServer(t, nil)
	rooms := buildLoadRooms(t, srv, loadRooms, loadSeats, maxSeq)
	res := runRelayWorkload(rooms, window, 0, 2*time.Second)
	closeLoadRooms(rooms)

	hz, err := mutexCyclesPerSecond()
	require.NoError(t, err)
	afterDelay, afterCount := roomLockDelay(hz)
	delay := afterDelay - beforeDelay
	waits := afterCount - beforeCount
	runtime.SetMutexProfileFraction(0)

	cpus := runtime.NumCPU()
	perf.Report(t, "7", "room-mutex contention (runtime mutex profile)",
		fmt.Sprintf("%s blocked across %d waits over %s wall = %.3f%% of %d-core capacity; %s per relay",
			delay.Round(time.Millisecond), waits, res.wall.Round(time.Millisecond),
			100*delay.Seconds()/(res.wall.Seconds()*float64(cpus)), cpus, perDur(delay, res.relays)))
	perf.Report(t, "7", "contention-run latency (mutex profiling ON — not comparable to the fan-out p99)",
		perf.Summary(res.lat))

	// Saturation probe: one room, one sender, no pacing.
	const blast = 20_000
	probeSrv := loadServer(t, nil)
	probe := buildLoadRooms(t, probeSrv, 1, loadSeats, blast+16)[0]
	defer closeLoadRooms([]*loadRoom{probe})

	ctx, cancel := context.WithCancel(context.Background())
	var readers sync.WaitGroup
	for _, lc := range probe.conns {
		lc.recording.Store(true)
		readers.Add(1)
		go func() { defer readers.Done(); lc.readFrames(ctx) }()
	}
	sender := probe.conns[0]
	saturationStart := time.Now()
	sender.blastBatches(probe.matchID, blast)
	waitForRelay(probe, 0, int(sender.written.Load()), 60*time.Second)
	saturated := time.Since(saturationStart)
	cancel()
	readers.Wait()

	ceiling := float64(sender.written.Load()) / saturated.Seconds()
	demand := float64(loadSeats) / loadBatchPeriod.Seconds()
	perf.Report(t, "7", "single-room serialised ceiling",
		fmt.Sprintf("%.0f batches/s (%d batches fanned out to %d peers in %s; in-process clients ⇒ a LOWER bound)",
			ceiling, sender.written.Load(), loadSeats-1, saturated.Round(time.Millisecond)))
	perf.Report(t, "7", "room-lock utilisation at the production rate",
		fmt.Sprintf("%.0f batches/s demanded per room vs %.0f/s ceiling = %.2f%%", demand, ceiling, 100*demand/ceiling))
}

// waitForRelay blocks until every peer has seen the sender's batch `seq`, or
// the deadline elapses. A write returning only means the frame is queued.
func waitForRelay(lr *loadRoom, senderSeat, seq int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		done := true
		for i, lc := range lr.conns {
			if i != senderSeat && !lc.slow && lc.got[senderSeat][seq] == 0 {
				done = false
			}
		}
		if done {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

// perDur renders a total spread over n events.
func perDur(total time.Duration, n int64) time.Duration {
	if n == 0 {
		return 0
	}
	return total / time.Duration(n)
}

// --- allocations -------------------------------------------------------------

// TestLoadRelayAllocations prices one relayed batch, and separately prices the
// thing worth pricing: relayEventBatch marshals the SAME PeerBatch value once
// per recipient, so a 5-seat room pays four identical json.Marshal calls per
// batch — under the room lock.
func TestLoadRelayAllocations(t *testing.T) {
	loadSettle(t)
	const batches = 5_000
	srv := loadServer(t, nil)
	room := buildLoadRooms(t, srv, 1, loadSeats, batches+16)[0]
	defer closeLoadRooms([]*loadRoom{room})

	ctx, cancel := context.WithCancel(context.Background())
	var readers sync.WaitGroup
	for _, lc := range room.conns {
		readers.Add(1)
		go func() { defer readers.Done(); lc.readFrames(ctx) }()
	}

	sender := room.conns[0]
	peers := int64(loadSeats - 1)
	allocBytes, allocs := perf.Delta(func() {
		sender.blastBatches(room.matchID, batches)
		waitForRelay(room, 0, int(sender.written.Load()), 60*time.Second)
	})
	cancel()
	readers.Wait()

	sent := sender.written.Load()
	require.Positive(t, sent)
	relays := sent * peers
	perf.Report(t, "7", "allocation per relayed batch (WHOLE PROCESS: server relay plus in-process client encode/decode)",
		fmt.Sprintf("%s and %d allocs over %d batches ⇒ %s / %.1f allocs per batch, %s / %.1f allocs per relay",
			perf.MiB(allocBytes), allocs, sent,
			perf.MiB(allocBytes/uint64(sent)), float64(allocs)/float64(sent),
			perf.MiB(allocBytes/uint64(relays)), float64(allocs)/float64(relays)))

	// The duplicated marshal, isolated: exactly what trySend does per recipient,
	// on the identical PeerBatch value.
	pb := protocol.PeerBatch{
		Type:     protocol.TypePeerBatch,
		PlayerID: sender.playerID,
		Events:   []json.RawMessage{json.RawMessage(`{"k":"insert","seq":1234,"t":98720,"ch":"x"}`)},
	}
	const marshals = 200_000
	mb, ma := perf.Delta(func() {
		for range marshals {
			b, err := json.Marshal(pb)
			if err != nil || len(b) == 0 {
				t.Error("marshal PeerBatch:", err)
				return
			}
		}
	})
	perMarshal := mb / marshals
	// Time it on a quiet heap: the Delta above just left 200 k marshals' worth
	// of garbage behind, and collecting that inside the timed loop would price
	// the GC rather than the marshal.
	runtime.GC()
	for range marshals / 10 {
		if _, err := json.Marshal(pb); err != nil {
			t.Fatal(err)
		}
	}
	start := time.Now()
	for range marshals {
		if _, err := json.Marshal(pb); err != nil {
			t.Fatal(err)
		}
	}
	perMarshalTime := time.Since(start) / marshals

	rate := float64(loadRooms*loadSeats) / loadBatchPeriod.Seconds() // batches/s at the headline load
	redundant := rate * float64(peers-1)                             // marshals/s that produce identical bytes
	perf.Report(t, "7", "one PeerBatch json.Marshal",
		fmt.Sprintf("%s, %.1f allocs, %s", perf.MiB(perMarshal), float64(ma)/float64(marshals), perMarshalTime))
	perf.Report(t, "7", "redundant marshals per batch in a 5-seat room",
		fmt.Sprintf("%d of %d recipients get byte-identical bytes re-marshalled ⇒ %s and %s wasted per batch",
			peers-1, peers, perf.MiB(perMarshal*uint64(peers-1)), perMarshalTime*time.Duration(peers-1)))
	perf.Report(t, "7", "redundant marshal cost at the 50-room production rate",
		fmt.Sprintf("%.0f marshals/s ⇒ %s/s of garbage and %.1f%% of one core",
			redundant, perf.MiB(uint64(redundant)*perMarshal), 100*redundant*perMarshalTime.Seconds()))
}

// BenchmarkPeerBatchMarshal prices the per-recipient marshal that
// Room.relayEventBatch repeats N-1 times per batch, under the room lock.
func BenchmarkPeerBatchMarshal(b *testing.B) {
	pb := protocol.PeerBatch{
		Type:     protocol.TypePeerBatch,
		PlayerID: "3b1e9c2f7a8d4e10b6c5a1d2e3f40506",
		Events:   []json.RawMessage{json.RawMessage(`{"k":"insert","seq":1234,"t":98720,"ch":"x"}`)},
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := json.Marshal(pb); err != nil {
			b.Fatal(err)
		}
	}
}

// --- slow consumer -----------------------------------------------------------

// slowWindow has to outrun the heartbeat teardown. The server pings every 15 s
// and waits 15 s for the pong, giving up after 2 misses, so a client that never
// reads is cancelled about 60 s after it connects; the window must be longer
// than that for the teardown to be observable from outside.
const slowWindow = 70 * time.Second

// TestLoadRelaySlowConsumer is the property trySend exists for. One client
// completes the handshake, joins, keeps typing, and NEVER reads. The room must
// not notice.
func TestLoadRelaySlowConsumer(t *testing.T) {
	loadSettle(t)
	srv := loadServer(t, nil)
	maxSeq := int(slowWindow/loadBatchPeriod) + 64
	rooms := buildLoadRooms(t, srv, 2, loadSeats, maxSeq)
	defer closeLoadRooms(rooms)

	victim, control := rooms[0], rooms[1]
	slow := victim.conns[loadSeats-1]
	slow.slow = true // startWorkload starts no reader for it

	run := startWorkload(rooms, slowWindow, 0, loadDrain)

	// The room did not deadlock and the match can still end. This happens while
	// the readers are still live, because stopping them closes the sockets.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	live := victim.conns[:loadSeats-1]
	for _, lc := range live {
		require.NoError(t, loadWriteJSON(lc.c, protocol.Finish{Type: protocol.TypeFinish, MatchID: victim.matchID}))
	}
	endedAll := true
	for _, lc := range live {
		for !lc.matchEnd.Load() {
			if ctx.Err() != nil {
				endedAll = false
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	require.True(t, endedAll, "the victim room never reached match_end: it deadlocked or the slow seat never resolved")
	perf.Report(t, "7", "slow consumer: match still ends",
		"yes — every live seat received match_end after finishing (the stalled seat resolves via its reconnect grace)")

	res := run.stop()
	perf.Report(t, "7", "slow-consumer scenario totals", fmt.Sprintf(
		"%d batches sent across both rooms in %s", res.sent, res.wall.Round(time.Millisecond)))

	var victimLat, controlLat []time.Duration
	for _, lc := range live {
		victimLat = append(victimLat, lc.lat...)
	}
	for _, lc := range control.conns {
		controlLat = append(controlLat, lc.lat...)
	}
	require.NotEmpty(t, victimLat)
	require.NotEmpty(t, controlLat)

	// Delivery accounting between HEALTHY pairs only. The stalled seat is
	// expected to lose what was relayed to it, and its own sends race its
	// teardown (a client write can land in the socket after the server's read
	// loop is already cancelled), so both directions are accounted separately.
	var victimLost, controlLost, slowSendLost, slowSendLostMinSeq int64
	for _, room := range []*loadRoom{victim, control} {
		for _, sender := range room.conns {
			n := sender.written.Load()
			for _, rcv := range room.conns {
				if rcv == sender || rcv.slow {
					continue
				}
				for seq := int64(1); seq <= n; seq++ {
					if rcv.got[sender.seat][seq] != 0 {
						continue
					}
					switch {
					case sender.slow:
						slowSendLost++
						if slowSendLostMinSeq == 0 || seq < slowSendLostMinSeq {
							slowSendLostMinSeq = seq
						}
					case room == victim:
						victimLost++
					default:
						controlLost++
					}
				}
			}
		}
	}

	victimP99 := perf.Percentile(victimLat, 99)
	controlP99 := perf.Percentile(controlLat, 99)
	perf.Report(t, "7", "slow-consumer room, 4 healthy seats", perf.Summary(victimLat))
	perf.Report(t, "7", "control room, 5 healthy seats", perf.Summary(controlLat))

	perf.Budget{
		Zone:      "7",
		Workload:  "slow consumer: healthy peers' p99 in the affected room",
		Limit:     loadP99Budget,
		Rationale: "a stalled reader must be invisible to its room, so the same 50 ms ceiling applies",
	}.Assert(t, victimP99)
	ratio := float64(victimP99) / float64(controlP99)
	perf.Report(t, "7", "slow consumer: p99 vs the control room",
		fmt.Sprintf("%s vs %s = %.2fx", victimP99.Round(10*time.Microsecond), controlP99.Round(10*time.Microsecond), ratio))
	if ratio > 2 {
		t.Errorf("BUDGET MISSED 7 | slow consumer: healthy-peer p99 degradation | %.2fx the control room, expected <=2x", ratio)
	}
	assertZero(t, "7", "slow consumer: frames lost between healthy peers", victimLost,
		"only the stalled client may lose frames; trySend must not shed anyone else's")
	perf.Report(t, "7", "control-room frames lost (no slow consumer)", controlLost)

	// The stalled seat's OWN sends, which race its teardown: a write that
	// returns only means the bytes reached the socket, and the server's read
	// loop is already gone.
	perf.Report(t, "7", "slow consumer: its own batches that never reached its peers",
		fmt.Sprintf("%d relays lost, earliest at batchSeq %d of %d written — the tail it wrote as the server tore it down",
			slowSendLost, slowSendLostMinSeq, slow.written.Load()))

	// Now the casualty's receive side.
	slowGot, slowElapsed, slowErr := drainSlowConsumer(slow)
	var owed int64
	for _, sender := range victim.conns {
		if sender != slow {
			owed += sender.written.Load()
		}
	}
	perf.Report(t, "7", "slow consumer: frames its socket still held after teardown",
		fmt.Sprintf("%d of %d peer_batches owed; read failed after %s with: %v",
			slowGot, owed, slowElapsed.Round(time.Millisecond), slowErr))
	perf.Report(t, "7", "slow consumer: batches it still managed to SEND before teardown",
		fmt.Sprintf("%d (= %s at one batch per %s, i.e. the heartbeat's 2-missed-pong deadline)",
			slow.written.Load(), time.Duration(slow.written.Load())*loadBatchPeriod, loadBatchPeriod))
	require.Error(t, slowErr, "the slow consumer's connection should have been torn down")
	require.False(t, errors.Is(slowErr, context.DeadlineExceeded),
		"the slow consumer was still alive when the drain timed out: the heartbeat never tore it down")
}

// drainSlowConsumer finally reads the stalled client's socket to exhaustion,
// reporting what survived and how the connection died.
func drainSlowConsumer(slow *loadConn) (int, time.Duration, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := time.Now()
	got := 0
	for {
		_, data, err := slow.c.Read(ctx)
		if err != nil {
			return got, time.Since(start), err
		}
		var f loadFrame
		if json.Unmarshal(data, &f) == nil && f.Type == protocol.TypePeerBatch {
			got++
		}
	}
}

// stallWindow sits comfortably inside the ~60 s heartbeat deadline so the
// stalled client is still CONNECTED when it starts reading again. That matters:
// a torn-down connection is reset by the OS with its receive buffer still full,
// which destroys the very evidence this test is after.
const stallWindow = 25 * time.Second

// TestLoadRelayStalledConsumerRecovers makes trySend's drop visible from
// outside. One seat stops reading for 25 s while its room keeps typing, then
// drains its socket: whatever is missing was shed by the non-blocking send once
// the 256-slot outbound queue filled. Nothing here touches production code —
// the arithmetic is "frames its four peers wrote" minus "frames it read".
func TestLoadRelayStalledConsumerRecovers(t *testing.T) {
	loadSettle(t)
	srv := loadServer(t, nil)
	maxSeq := int(stallWindow/loadBatchPeriod) + 64
	room := buildLoadRooms(t, srv, 1, loadSeats, maxSeq)[0]
	defer closeLoadRooms([]*loadRoom{room})

	stalled := room.conns[loadSeats-1]
	stalled.slow = true
	run := startWorkload([]*loadRoom{room}, stallWindow, 0, loadDrain)

	// Drain the stalled socket while it is still alive. Reading stops after 3 s
	// of silence; that read's expiry closes the connection, which is fine now
	// that everything the server queued has either arrived or been dropped.
	got, drained := drainStalled(stalled, 3*time.Second)
	res := run.stop()

	var owed int64
	for _, peer := range room.conns {
		if peer != stalled {
			owed += peer.written.Load()
		}
	}
	dropped := owed - int64(got)
	perf.Report(t, "7", "stalled consumer (still connected): frames delivered vs owed",
		fmt.Sprintf("%d of %d peer_batches over a %s stall; %d dropped (%.1f%%), drained in %s",
			got, owed, stallWindow, dropped, 100*float64(dropped)/float64(owed), drained.Round(time.Millisecond)))
	perf.Report(t, "7", "stalled consumer: absorbed before the first drop",
		fmt.Sprintf("%d frames — the 256-slot outbound queue plus whatever the socket buffers held", got))
	perf.Report(t, "7", "healthy peers during the stall", perf.Summary(res.lat))
	require.Positive(t, got, "the stalled client received nothing at all: the connection died instead of degrading")
	if dropped <= 0 {
		t.Logf("NOTE zone 7: at the realistic rate nothing was dropped — the outbound queue and kernel buffers "+
			"absorbed all %d frames over %s, so a stalled seat costs its room NOTHING for at least that long",
			owed, stallWindow)
	}

	// Phase 2: find where the absorption actually ends. A stalled loopback
	// socket on this box swallows 2.6 MiB before the writer blocks (measured
	// separately with a bare net.Conn), which is roughly 21 000 peer_batches —
	// far more than a realistic room can produce inside the heartbeat's 60 s.
	// So the shed path is provoked deliberately: a fresh room (fresh heartbeat
	// clock), all four healthy seats writing flat out, past the point where the
	// kernel can hide the stall. All four write so the AFK trailing rule never
	// dnf's a silent seat mid-burst.
	const perSender = 8_000
	burstRoom := buildLoadRooms(t, srv, 1, loadSeats, perSender+16)[0]
	defer closeLoadRooms([]*loadRoom{burstRoom})
	burstStalled := burstRoom.conns[loadSeats-1]
	burstStalled.slow = true // waitForRelay must not wait on a seat that never reads
	senders := burstRoom.conns[:loadSeats-1]

	ctx, cancel := context.WithCancel(context.Background())
	var readers, blasters sync.WaitGroup
	for _, lc := range senders {
		readers.Add(1)
		go func() { defer readers.Done(); lc.readFrames(ctx) }()
	}
	burstStart := time.Now()
	for _, lc := range senders {
		blasters.Add(1)
		go func() {
			defer blasters.Done()
			// 2 000 batches/s per seat: 8 000 batches/s into the room, four
			// times the room's realistic ceiling, but still a rate an actively
			// reading client keeps up with.
			lc.blastPacedBatches(burstRoom.matchID, perSender, 10, 5*time.Millisecond)
		}()
	}
	blasters.Wait()
	for _, lc := range senders {
		waitForRelay(burstRoom, lc.seat, int(lc.written.Load()), 60*time.Second)
	}
	burstWall := time.Since(burstStart)
	burstGot, burstDrained := drainStalled(burstStalled, 3*time.Second)
	cancel()
	readers.Wait()

	var burstOwed, healthyLost int64
	for _, lc := range senders {
		require.NoErrorf(t, lc.writeErr, "seat %d could not finish its burst", lc.seat)
		n := lc.written.Load()
		burstOwed += n
		for _, rcv := range senders {
			if rcv == lc {
				continue
			}
			for seq := int64(1); seq <= n; seq++ {
				if rcv.got[lc.seat][seq] == 0 {
					healthyLost++
				}
			}
		}
	}
	burstDropped := burstOwed - int64(burstGot)
	perf.Report(t, "7", "saturating burst at a stalled seat",
		fmt.Sprintf("%d of %d peer_batches survived (%d dropped, %.1f%%) — %d batches from %d seats in %s, drained in %s",
			burstGot, burstOwed, burstDropped, 100*float64(burstDropped)/float64(burstOwed),
			burstOwed, len(senders), burstWall.Round(time.Millisecond), burstDrained.Round(time.Millisecond)))
	perf.Report(t, "7", "pipeline absorption before trySend sheds",
		fmt.Sprintf("%d frames = the 256-slot outbound queue plus ~2.6 MiB of loopback socket buffer", burstGot))
	assertZero(t, "7", "saturating burst: frames lost by the HEALTHY peers", healthyLost,
		"a stalled seat must cost only itself, however hard the room is pushed")
	if burstDropped <= 0 {
		t.Errorf("BUDGET MISSED 7 | trySend shed path not observable | a stalled seat absorbed all %d frames of "+
			"an unpaced 4-seat burst, so the drop this test exists to witness never happened", burstOwed)
	}
}

// drainStalled reads a stalled client's socket until `quiet` elapses with no
// frame, counting the peer_batches that survived.
func drainStalled(lc *loadConn, quiet time.Duration) (int, time.Duration) {
	start := time.Now()
	got := 0
	for {
		ctx, cancel := context.WithTimeout(context.Background(), quiet)
		_, data, err := lc.c.Read(ctx)
		cancel()
		if err != nil {
			return got, time.Since(start)
		}
		var f loadFrame
		if json.Unmarshal(data, &f) == nil && f.Type == protocol.TypePeerBatch {
			got++
		}
	}
}
