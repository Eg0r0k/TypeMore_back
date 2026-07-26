package ws_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/typemore/typemore-server/internal/protocol"
	"github.com/typemore/typemore-server/internal/ws"
)

// lobbyPath is the public discovery endpoint, mounted exactly as production
// mounts it: a plain HTTP GET beside the other public routes, NOT under /ws.
const lobbyPath = "/api/v1/rooms"

// countingLimiter is a fixed-budget stand-in for the shared token bucket: the
// bucket's own refill behaviour is the auth package's test, what matters here
// is that the endpoint consults a limiter at all and renders its refusal.
type countingLimiter struct {
	remaining atomic.Int64
}

func (l *countingLimiter) Allow(string) bool { return l.remaining.Add(-1) >= 0 }

// lobbyTestServer wires /ws and the public room list onto one server, so a test
// can churn rooms over the protocol and read the list over HTTP. limiter may be
// nil (unthrottled).
func lobbyTestServer(t *testing.T, limiter ws.RateLimiter) *httptest.Server {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	// A short grace makes a dropped connection actually destroy its room inside
	// a test's lifetime, which the churn test needs.
	h := ws.NewHandler(logger, nil, func(req *http.Request) (string, string, bool) {
		if name := req.Header.Get("X-Test-User"); name != "" {
			return name, "", true
		}
		return "", "", false
	}, nil, ws.WithGrace(10*time.Millisecond))

	r := chi.NewRouter()
	r.Handle("/ws", h)
	r.Method(http.MethodGet, lobbyPath, h.LobbyHandler(limiter))
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}

// listedRoom mirrors one entry of the wire contract. The two dimensions are
// pointers on purpose: the test must be able to tell "absent" from "zero".
type listedRoom struct {
	Code        string `json:"code"`
	Name        string `json:"name"`
	PlayerCount int    `json:"playerCount"`
	MaxPlayers  int    `json:"maxPlayers"`
	InMatch     bool   `json:"inMatch"`
	Settings    struct {
		Mode       string `json:"mode"`
		DurationMs *int   `json:"durationMs"`
		WordCount  *int   `json:"wordCount"`
		Lang       string `json:"lang"`
	} `json:"settings"`
}

// getLobby performs the GET and returns the decoded rooms plus the status.
func getLobby(ctx context.Context, srv *httptest.Server) ([]listedRoom, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+lobbyPath, http.NoBody)
	if err != nil {
		return nil, 0, err
	}
	res, err := srv.Client().Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = res.Body.Close() }()

	var body struct {
		Rooms []listedRoom `json:"rooms"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		return nil, res.StatusCode, err
	}
	return body.Rooms, res.StatusCode, nil
}

// fetchLobby is getLobby for the happy path, asserting the envelope shape.
func fetchLobby(t *testing.T, ctx context.Context, srv *httptest.Server) []listedRoom {
	t.Helper()
	rooms, status, err := getLobby(ctx, srv)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)
	require.NotNil(t, rooms, "the envelope carries an array, never null")
	return rooms
}

// publish flips the caller's room to visibility "open" (rooms are created
// private) and names it, optionally mutating the rest of the settings. It
// consumes the host's own room_state + settings chat.
func publish(t *testing.T, ctx context.Context, c *websocket.Conn, st protocol.RoomState,
	name string, mutate func(*protocol.Settings)) {
	t.Helper()
	s := st.Settings
	s.Visibility = protocol.VisibilityOpen
	s.Name = name
	if mutate != nil {
		mutate(&s)
	}
	writeJSON(t, ctx, c, protocol.SettingsUpdate{Type: protocol.TypeSettingsUpdate, Settings: s})
	expect(t, ctx, c, protocol.TypeRoomState)
	expect(t, ctx, c, protocol.TypeChat)
}

func codesOf(rooms []listedRoom) []string {
	out := make([]string, len(rooms))
	for i, r := range rooms {
		out[i] = r.Code
	}
	return out
}

// TestLobbyListsOnlyOpenRooms is the visibility rule: a private room is
// unreachable without its code and must never surface, in any state.
func TestLobbyListsOnlyOpenRooms(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	srv := lobbyTestServer(t, nil)

	// An empty registry answers with an array, not null.
	assert.Empty(t, fetchLobby(t, ctx, srv))

	openHost := dialAs(t, ctx, srv, "")
	_, openState := hostRoom(t, ctx, openHost)
	publish(t, ctx, openHost, openState, "friday night", nil)

	// The second room keeps the default visibility, which is "private".
	privateHost := dialAs(t, ctx, srv, "")
	_, privateState := hostRoom(t, ctx, privateHost)
	require.Equal(t, protocol.VisibilityPrivate, privateState.Visibility,
		"a fresh room must start private, or this test proves nothing")

	rooms := fetchLobby(t, ctx, srv)
	require.Len(t, rooms, 1, "only the open room is discoverable")
	assert.Equal(t, openState.Code, rooms[0].Code)
	assert.Equal(t, "friday night", rooms[0].Name)
	assert.Equal(t, 1, rooms[0].PlayerCount)
	assert.Equal(t, roomCapacity, rooms[0].MaxPlayers)
	assert.False(t, rooms[0].InMatch)

	// Going private again removes it, and no state leaks in the meantime.
	publish(t, ctx, openHost, openState, "friday night", func(s *protocol.Settings) {
		s.Visibility = protocol.VisibilityPrivate
	})
	assert.Empty(t, fetchLobby(t, ctx, srv), "a room that goes private leaves the list")
}

// TestLobbyOrdering pins the documented order: playerCount descending, then
// creation order ascending.
func TestLobbyOrdering(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	srv := lobbyTestServer(t, nil)

	// Created oldest-first, so first.Code precedes second.Code precedes busy.Code
	// in creation order regardless of what the codes happen to be.
	firstHost := dialAs(t, ctx, srv, "")
	_, first := hostRoom(t, ctx, firstHost)
	publish(t, ctx, firstHost, first, "first", nil)

	secondHost := dialAs(t, ctx, srv, "")
	_, second := hostRoom(t, ctx, secondHost)
	publish(t, ctx, secondHost, second, "second", nil)

	busyHost := dialAs(t, ctx, srv, "")
	_, busy := hostRoom(t, ctx, busyHost)
	publish(t, ctx, busyHost, busy, "busy", nil)
	for range 2 {
		joiner := dialAs(t, ctx, srv, "")
		joinRoom(t, ctx, joiner, busy.Code, busyHost)
	}

	rooms := fetchLobby(t, ctx, srv)
	require.Len(t, rooms, 3)
	assert.Equal(t, []string{busy.Code, first.Code, second.Code}, codesOf(rooms),
		"fullest first, then oldest first")
	assert.Equal(t, 3, rooms[0].PlayerCount)

	// The two single-seat rooms are the interesting case: the order between them
	// must not depend on map iteration, so repeated polls of an idle lobby must
	// return byte-identical ordering.
	for range 5 {
		assert.Equal(t, []string{busy.Code, first.Code, second.Code}, codesOf(fetchLobby(t, ctx, srv)),
			"an idle lobby must not shuffle between polls")
	}
}

// TestLobbyModeDimensionsAreMutuallyExclusive asserts the settings projection:
// exactly the dimension the mode uses is present, never both and never a
// misleading zero for the other.
func TestLobbyModeDimensionsAreMutuallyExclusive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	srv := lobbyTestServer(t, nil)

	timeHost := dialAs(t, ctx, srv, "")
	_, timed := hostRoom(t, ctx, timeHost)
	publish(t, ctx, timeHost, timed, "clock", func(s *protocol.Settings) {
		s.Mode, s.DurationMs, s.WordCount, s.Lang = protocol.ModeTime, 30000, 0, "english"
	})

	wordsHost := dialAs(t, ctx, srv, "")
	_, worded := hostRoom(t, ctx, wordsHost)
	publish(t, ctx, wordsHost, worded, "words", func(s *protocol.Settings) {
		s.Mode, s.DurationMs, s.WordCount, s.Lang = protocol.ModeWords, 0, 50, "russian"
	})

	byCode := map[string]listedRoom{}
	for _, r := range fetchLobby(t, ctx, srv) {
		byCode[r.Code] = r
	}
	require.Len(t, byCode, 2)

	gotTime := byCode[timed.Code]
	assert.Equal(t, protocol.ModeTime, gotTime.Settings.Mode)
	require.NotNil(t, gotTime.Settings.DurationMs)
	assert.Equal(t, 30000, *gotTime.Settings.DurationMs)
	assert.Nil(t, gotTime.Settings.WordCount, "a time room must not report a word count")
	assert.Equal(t, "english", gotTime.Settings.Lang)

	gotWords := byCode[worded.Code]
	assert.Equal(t, protocol.ModeWords, gotWords.Settings.Mode)
	require.NotNil(t, gotWords.Settings.WordCount)
	assert.Equal(t, 50, *gotWords.Settings.WordCount)
	assert.Nil(t, gotWords.Settings.DurationMs, "a words room must not report a duration")
	assert.Equal(t, "russian", gotWords.Settings.Lang)
}

// TestLobbyRateLimited proves the endpoint is behind the shared limiter and
// renders a refusal in the standard error shape.
func TestLobbyRateLimited(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	limiter := &countingLimiter{}
	limiter.remaining.Store(1)
	srv := lobbyTestServer(t, limiter)

	_, status, err := getLobby(ctx, srv)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+lobbyPath, http.NoBody)
	require.NoError(t, err)
	res, err := srv.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = res.Body.Close() }()

	assert.Equal(t, http.StatusTooManyRequests, res.StatusCode)
	var apiErr struct {
		Code    string `json:"error"`
		Message string `json:"message"`
	}
	require.NoError(t, json.NewDecoder(res.Body).Decode(&apiErr))
	assert.Equal(t, "rate_limited", apiErr.Code)
	assert.NotEmpty(t, apiErr.Message)
}

// TestLobbyUnderConcurrentRoomChurn is the reason this endpoint is shaped the
// way it is. Rooms are created, published, joined, left and destroyed by
// parallel goroutines over real WebSocket connections while other goroutines
// hammer the HTTP list. Under -race this covers the registry-lock / room-lock
// interleaving the snapshot walk depends on; every response is checked for
// internal consistency, which is how a half-built or torn-down room would show
// up if the walk ever published one.
func TestLobbyUnderConcurrentRoomChurn(t *testing.T) {
	if testing.Short() {
		t.Skip("churn test runs for a couple of seconds")
	}
	srv := lobbyTestServer(t, nil)
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"

	const (
		churners = 6
		pollers  = 4
		runFor   = 2 * time.Second
	)
	deadline := time.Now().Add(runFor)
	var wg sync.WaitGroup
	var polls, cycles, widest atomic.Int64

	// One churn cycle: open a room, publish it, seat a joiner, then tear the
	// whole thing down — half by an explicit `leave`, half by dropping the
	// connection, so both destruction paths race the walk.
	cycle := func(t *testing.T, dropRude bool) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		host, _, err := websocket.Dial(ctx, wsURL, nil)
		if err != nil {
			t.Error("dial host:", err)
			return
		}
		defer func() { _ = host.Close(websocket.StatusNormalClosure, "") }()

		if _, _, err = readOne(ctx, host, protocol.Hello{
			Type: protocol.TypeHello, ProtocolVersion: protocol.Version}); err != nil {
			t.Error("hello:", err)
			return
		}
		_, raw, err := readOne(ctx, host, protocol.CreateRoom{Type: protocol.TypeCreateRoom})
		if err != nil {
			t.Error("create_room:", err)
			return
		}
		var state protocol.RoomState
		if err = json.Unmarshal(raw, &state); err != nil {
			t.Error("decode room_state:", err)
			return
		}

		// Publish it. From here on the host's inbound frames are ignored: the
		// server's outbound queue is bounded and non-blocking, so an unread
		// connection is a slow consumer, not a stall.
		settings := state.Settings
		settings.Visibility = protocol.VisibilityOpen
		if err = writeOne(ctx, host, protocol.SettingsUpdate{
			Type: protocol.TypeSettingsUpdate, Settings: settings}); err != nil {
			t.Error("settings_update:", err)
			return
		}

		joiner, _, err := websocket.Dial(ctx, wsURL, nil)
		if err != nil {
			t.Error("dial joiner:", err)
			return
		}
		defer func() { _ = joiner.Close(websocket.StatusNormalClosure, "") }()
		if _, _, err = readOne(ctx, joiner, protocol.Hello{
			Type: protocol.TypeHello, ProtocolVersion: protocol.Version}); err != nil {
			t.Error("joiner hello:", err)
			return
		}
		if err = writeOne(ctx, joiner, protocol.JoinRoom{
			Type: protocol.TypeJoinRoom, Code: state.Code}); err != nil {
			t.Error("join_room:", err)
			return
		}

		if dropRude {
			// Both ends vanish; the seats survive only the (10ms) grace, then
			// the room is reaped out from under any walk in flight.
			return
		}
		if err = writeOne(ctx, joiner, protocol.Leave{Type: protocol.TypeLeave}); err != nil {
			t.Error("joiner leave:", err)
			return
		}
		if err = writeOne(ctx, host, protocol.Leave{Type: protocol.TypeLeave}); err != nil {
			t.Error("host leave:", err)
			return
		}
		cycles.Add(1)
	}

	for i := range churners {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; time.Now().Before(deadline); n++ {
				cycle(t, (i+n)%2 == 0)
			}
		}()
	}

	for range pollers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			for time.Now().Before(deadline) {
				rooms, status, err := getLobby(ctx, srv)
				if err != nil {
					t.Error("GET rooms:", err)
					return
				}
				if status != http.StatusOK {
					t.Errorf("GET rooms: status %d", status)
					return
				}
				assertLobbyConsistent(t, rooms)
				polls.Add(1)
				for n := int64(len(rooms)); ; {
					if cur := widest.Load(); n <= cur || widest.CompareAndSwap(cur, n) {
						break
					}
				}
			}
		}()
	}

	wg.Wait()
	t.Logf("churn: %d cycles, %d polls, widest list %d rooms", cycles.Load(), polls.Load(), widest.Load())
	require.Positive(t, polls.Load(), "the pollers must have observed something")
	require.Positive(t, cycles.Load(), "the churners must have completed cycles")
	// Without this the whole test could pass against a permanently empty list.
	require.Positive(t, widest.Load(), "the pollers must have seen live rooms, not an always-empty lobby")
}

// assertLobbyConsistent checks every invariant a listed room must satisfy. A
// room snapshotted mid-teardown, or one read without its own lock held, would
// break one of these: a zero player count, a count above capacity, a duplicated
// code, a broken ordering, or a settings pair that is neither of the two modes.
func assertLobbyConsistent(t *testing.T, rooms []listedRoom) {
	t.Helper()
	seen := make(map[string]struct{}, len(rooms))
	for i, r := range rooms {
		if _, dup := seen[r.Code]; dup {
			t.Errorf("room %s listed twice", r.Code)
		}
		seen[r.Code] = struct{}{}

		if len(r.Code) != 6 {
			t.Errorf("room %q: code is not 6 characters", r.Code)
		}
		if r.Name == "" {
			t.Errorf("room %s: empty name", r.Code)
		}
		if r.PlayerCount < 1 || r.PlayerCount > r.MaxPlayers {
			t.Errorf("room %s: playerCount %d outside [1, %d]", r.Code, r.PlayerCount, r.MaxPlayers)
		}
		if r.MaxPlayers != roomCapacity {
			t.Errorf("room %s: maxPlayers %d", r.Code, r.MaxPlayers)
		}
		if (r.Settings.DurationMs == nil) == (r.Settings.WordCount == nil) {
			t.Errorf("room %s: durationMs/wordCount are not mutually exclusive", r.Code)
		}
		if r.Settings.Lang == "" {
			t.Errorf("room %s: empty lang", r.Code)
		}
		if i > 0 && rooms[i-1].PlayerCount < r.PlayerCount {
			t.Errorf("ordering broken at %d: %d before %d", i, rooms[i-1].PlayerCount, r.PlayerCount)
		}
	}
}

// writeOne sends one JSON frame. Unlike the testify helpers it returns an error
// instead of failing, so it is safe to call from a non-test goroutine.
func writeOne(ctx context.Context, c *websocket.Conn, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.Write(ctx, websocket.MessageText, b)
}

// readOne sends one frame and returns the single frame that answers it.
func readOne(ctx context.Context, c *websocket.Conn, v any) (string, []byte, error) {
	if err := writeOne(ctx, c, v); err != nil {
		return "", nil, err
	}
	_, data, err := c.Read(ctx)
	if err != nil {
		return "", nil, err
	}
	var env protocol.Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return "", nil, err
	}
	return env.Type, data, nil
}
