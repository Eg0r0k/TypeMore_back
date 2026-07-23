package ws_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"sort"
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

// newTestServer starts a real HTTP server (random port) wired exactly like
// production: the /ws WebSocket endpoint served by ws.Handler. The logger is
// discarded to keep test output clean.
func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := chi.NewRouter()
	r.Handle("/ws", ws.NewHandler(logger, nil))
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}

// dial opens a real WebSocket client to the test server's /ws endpoint.
func dial(t *testing.T, ctx context.Context, srv *httptest.Server) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
	c, _, err := websocket.Dial(ctx, url, nil)
	require.NoError(t, err, "dial %s", url)
	t.Cleanup(func() { _ = c.Close(websocket.StatusNormalClosure, "") })
	return c
}

// writeJSON sends a value as a JSON text frame.
func writeJSON(t *testing.T, ctx context.Context, c *websocket.Conn, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	require.NoError(t, c.Write(ctx, websocket.MessageText, b))
}

// readFrame reads one frame and returns its discriminator plus raw bytes.
func readFrame(t *testing.T, ctx context.Context, c *websocket.Conn) (string, []byte) {
	t.Helper()
	typ, data, err := c.Read(ctx)
	require.NoError(t, err)
	require.Equal(t, websocket.MessageText, typ)
	var env protocol.Envelope
	require.NoError(t, json.Unmarshal(data, &env))
	return env.Type, data
}

// doHello performs the hello handshake and returns the assigned player id.
func doHello(t *testing.T, ctx context.Context, c *websocket.Conn, nick string) string {
	t.Helper()
	writeJSON(t, ctx, c, protocol.Hello{Type: protocol.TypeHello, ProtocolVersion: protocol.Version, Nick: nick})
	typ, data := readFrame(t, ctx, c)
	require.Equal(t, protocol.TypeHelloOK, typ)
	var ok protocol.HelloOK
	require.NoError(t, json.Unmarshal(data, &ok))
	assert.Equal(t, protocol.Version, ok.ServerVersion)
	assert.NotEmpty(t, ok.PlayerID)
	return ok.PlayerID
}

// TestHelloThenNTP is the headline integration test: a real client completes the
// hello handshake, then runs five NTP exchanges. It asserts the pong echo
// semantics and that the client-side offset computation recovers a synthetic
// clock skew within tolerance.
func TestHelloThenNTP(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	srv := newTestServer(t)
	c := dial(t, ctx, srv)

	doHello(t, ctx, c, "neo")

	// Simulate a client whose clock is 5s ahead of the server. A correct offset
	// computation must recover ~ -5000ms (server = client + offset).
	const skewMs = 5000
	clientNow := func() int64 { return time.Now().UnixMilli() + skewMs }

	const pairs = 5
	type sample struct{ offset, rtt int64 }
	samples := make([]sample, 0, pairs)

	for range pairs {
		t0 := clientNow()
		writeJSON(t, ctx, c, protocol.NTPPing{Type: protocol.TypeNTPPing, T0: t0})

		typ, data := readFrame(t, ctx, c)
		t3 := clientNow()
		require.Equal(t, protocol.TypeNTPPong, typ)

		var pong protocol.NTPPong
		require.NoError(t, json.Unmarshal(data, &pong))

		// Echo semantics: t0 returned unchanged, and t1 <= t2 (receive precedes
		// send on the server).
		assert.Equal(t, t0, pong.T0, "t0 must be echoed unchanged")
		assert.LessOrEqual(t, pong.T1, pong.T2, "t1 (recv) must not be after t2 (send)")

		offset := ((pong.T1 - pong.T0) + (pong.T2 - t3)) / 2
		rtt := (t3 - t0) - (pong.T2 - pong.T1)
		samples = append(samples, sample{offset: offset, rtt: rtt})
	}

	// Client procedure from docs/PROTOCOL.md: discard pairs whose RTT exceeds 3x
	// the minimum observed RTT, then take the median offset.
	minRTT := samples[0].rtt
	for _, s := range samples {
		if s.rtt < minRTT {
			minRTT = s.rtt
		}
	}
	offsets := make([]int64, 0, len(samples))
	for _, s := range samples {
		if s.rtt <= 3*minRTT {
			offsets = append(offsets, s.offset)
		}
	}
	require.NotEmpty(t, offsets)
	sort.Slice(offsets, func(i, j int) bool { return offsets[i] < offsets[j] })
	median := offsets[len(offsets)/2]

	// The recovered offset should be ~ -skew. Tolerance absorbs loopback RTT and
	// the millisecond quantization of the timestamps.
	const tolerance = 100
	assert.InDelta(t, -skewMs, median, tolerance,
		"median offset %d should recover synthetic skew %d", median, -skewMs)
}

// TestVersionMismatchCloses asserts a bad protocolVersion yields a
// version_mismatch error AND that the server then closes the connection.
func TestVersionMismatchCloses(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	srv := newTestServer(t)
	c := dial(t, ctx, srv)

	writeJSON(t, ctx, c, protocol.Hello{
		Type:            protocol.TypeHello,
		ProtocolVersion: protocol.Version + 999,
		Nick:            "neo",
	})

	typ, data := readFrame(t, ctx, c)
	require.Equal(t, protocol.TypeError, typ)
	var e protocol.Error
	require.NoError(t, json.Unmarshal(data, &e))
	assert.Equal(t, protocol.CodeVersionMismatch, e.Code)

	// The next read must fail: the server closes after sending the error.
	_, _, err := c.Read(ctx)
	require.Error(t, err)
	assert.Equal(t, websocket.StatusPolicyViolation, websocket.CloseStatus(err))
}

// TestProtocolViolations covers the in-band (non-closing) error cases in a
// table: each sends a frame and expects a bad_message error while the
// connection stays usable.
func TestProtocolViolations(t *testing.T) {
	tests := []struct {
		name        string
		helloFirst  bool // complete hello before the offending frame
		frame       any
		rawFrame    string // used instead of frame when non-empty
		wantMessage string
	}{
		{
			name:        "message before hello",
			frame:       protocol.NTPPing{Type: protocol.TypeNTPPing, T0: 1},
			wantMessage: "hello must be the first message",
		},
		{
			name:        "unknown type after hello",
			helloFirst:  true,
			rawFrame:    `{"type":"totally_made_up"}`,
			wantMessage: "unsupported message type",
		},
		{
			name:        "second hello after hello",
			helloFirst:  true,
			frame:       protocol.Hello{Type: protocol.TypeHello, ProtocolVersion: protocol.Version, Nick: "again"},
			wantMessage: "hello already completed",
		},
		{
			name:        "invalid nick",
			frame:       protocol.Hello{Type: protocol.TypeHello, ProtocolVersion: protocol.Version, Nick: ""},
			wantMessage: "nick must be 1-16 characters",
		},
		{
			name:        "not json",
			rawFrame:    `this is not json`,
			wantMessage: "not valid JSON",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			srv := newTestServer(t)
			c := dial(t, ctx, srv)

			if tc.helloFirst {
				doHello(t, ctx, c, "neo")
			}

			if tc.rawFrame != "" {
				require.NoError(t, c.Write(ctx, websocket.MessageText, []byte(tc.rawFrame)))
			} else {
				writeJSON(t, ctx, c, tc.frame)
			}

			typ, data := readFrame(t, ctx, c)
			require.Equal(t, protocol.TypeError, typ)
			var e protocol.Error
			require.NoError(t, json.Unmarshal(data, &e))
			assert.Equal(t, protocol.CodeBadMessage, e.Code)
			assert.Contains(t, e.Message, tc.wantMessage)

			// Connection stays open: a subsequent valid hello must still work.
			// (Skip when a hello was already completed, since a second hello is
			// itself an error.)
			if !tc.helloFirst {
				doHello(t, ctx, c, "second")
			}
		})
	}
}
