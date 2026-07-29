package ws

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"log/slog"
	"strings"
	"sync"

	"github.com/typemore/typemore-server/internal/protocol"
)

// roomCapacity is the maximum number of seats in a room (docs/PROTOCOL.md §5).
const roomCapacity = 5

// roomCodeLen is the length of a room code.
const roomCodeLen = 6

// roomCodeAlphabet is the human-safe alphabet for room codes: 32 glyphs with the
// ambiguous 0/O/1/I removed (docs/PROTOCOL.md §5). Its length being a power of
// two means a uniform byte maps to a character without modulo bias.
const roomCodeAlphabet = "23456789ABCDEFGHJKLMNPQRSTUVWXYZ"

// Registry is the process-wide set of live rooms keyed by code. It is the single
// owner of the room map: rooms are created here and a room is dropped (via
// removeIfEmpty) once its last seat leaves. Safe for concurrent use.
//
// Lock ordering is always Registry.mu THEN Room.mu — Create/Join hold the
// registry lock while seating (which takes the room lock), and removeIfEmpty
// does the same, so a concurrent join can never race a room out from under a
// caller.
type Registry struct {
	log    *slog.Logger
	store  MatchStore
	timing matchTiming
	mu     sync.Mutex
	rooms  map[string]*Room
	// graces holds the seats currently in their reconnect grace window, keyed by
	// their (secret) resume token. It is a slice, not a map, so the token
	// comparison on reconnect is a constant-time compare against each candidate.
	graces []graceEntry
	// persists tracks the in-flight match-capture writes. They are started off
	// the room lock (the write must not hold up a room's return to the lobby)
	// and they outlive the room that started it, so something process-wide has
	// to own them or a shutdown races them.
	persists sync.WaitGroup
}

// graceEntry links a live resume token to the room and seat awaiting reconnect.
type graceEntry struct {
	token string
	room  *Room
	seat  *seat
}

// NewRegistry builds an empty room registry. store (may be nil) receives each
// finished match's capture.
func NewRegistry(log *slog.Logger, store MatchStore) *Registry {
	return &Registry{log: log, store: store, timing: defaultTiming(), rooms: make(map[string]*Room)}
}

// Create opens a new room with host seated as its first (host) seat and returns
// it. The caller records the returned room on its session.
func (reg *Registry) Create(host *session) *Room {
	reg.mu.Lock()
	defer reg.mu.Unlock()

	code := reg.freeCodeLocked()
	room := newRoom(code, reg, reg.log, reg.store)
	reg.rooms[code] = room
	room.seat(host, true)
	return room
}

// Join seats joiner in the room named by code (matched case-insensitively). It
// returns the room and an empty error code on success, or a nil room and a
// protocol error code (room_not_found / room_full) on failure.
func (reg *Registry) Join(code string, joiner *session) (*Room, string) {
	code = normalizeCode(code)

	reg.mu.Lock()
	defer reg.mu.Unlock()

	room := reg.rooms[code]
	if room == nil {
		return nil, protocol.CodeRoomNotFound
	}
	if !room.seat(joiner, false) {
		return nil, protocol.CodeRoomFull
	}
	return room, ""
}

// removeIfEmpty drops the room named by code if it currently has no seats. It is
// called after any departure; the emptiness re-check under the registry lock
// means a join that arrived in the meantime keeps the room alive.
func (reg *Registry) removeIfEmpty(code string) {
	reg.mu.Lock()
	defer reg.mu.Unlock()

	if room := reg.rooms[code]; room != nil && room.empty() {
		delete(reg.rooms, code)
	}
}

// freeCodeLocked returns a code not currently in use. Caller holds reg.mu.
func (reg *Registry) freeCodeLocked() string {
	for {
		code := newRoomCode()
		if _, exists := reg.rooms[code]; !exists {
			return code
		}
	}
}

// newRoomCode mints a random room code from the human-safe alphabet.
func newRoomCode() string {
	b := make([]byte, roomCodeLen)
	if _, err := rand.Read(b); err != nil {
		panic("ws: crypto/rand failed: " + err.Error())
	}
	for i := range b {
		b[i] = roomCodeAlphabet[int(b[i])%len(roomCodeAlphabet)]
	}
	return string(b)
}

// normalizeCode canonicalizes a client-supplied code: trimmed and upper-cased so
// entry is case-insensitive (codes are stored upper-case).
func normalizeCode(code string) string {
	return strings.ToUpper(strings.TrimSpace(code))
}

// goPersist runs a finished match's capture write on its own goroutine, owned by
// the registry. The write is deliberately off the room lock and deliberately on
// a background context with its own timeout: a match that ended as the process
// is shutting down is exactly the capture worth keeping, so cancelling it with
// the server context would throw away the thing it exists to save. What was
// missing is the other half — something to wait on, so the process does not exit
// out from under a half-written match.
func (reg *Registry) goPersist(write func()) {
	reg.persists.Add(1)
	go func() {
		defer reg.persists.Done()
		write()
	}()
}

// WaitForPersists blocks until every in-flight match-capture write has finished,
// or until ctx is done. It reports whether they all completed; a false means the
// deadline arrived first and some capture may be missing, which is worth a log
// line rather than silence.
func (reg *Registry) WaitForPersists(ctx context.Context) bool {
	done := make(chan struct{})
	go func() {
		reg.persists.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	}
}

// addGrace registers a seat's resume token so a reconnect can find its room and
// seat during the grace window. Called off the room lock after disconnect().
func (reg *Registry) addGrace(token string, room *Room, seat *seat) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	reg.graces = append(reg.graces, graceEntry{token: token, room: room, seat: seat})
}

// claimGrace looks up and removes the grace entry matching token, comparing in
// constant time so a wrong token cannot be probed by timing. It returns ok=false
// when no entry matches.
func (reg *Registry) claimGrace(token string) (*Room, *seat, bool) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	tok := []byte(token)
	for i, e := range reg.graces {
		if subtle.ConstantTimeCompare([]byte(e.token), tok) == 1 {
			room, seat := e.room, e.seat
			reg.graces = append(reg.graces[:i], reg.graces[i+1:]...)
			return room, seat, true
		}
	}
	return nil, nil, false
}

// removeGrace drops the grace entry for token if still present (e.g. on grace
// expiry). It is a no-op when a reconnect already claimed it.
func (reg *Registry) removeGrace(token string) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	for i, e := range reg.graces {
		if e.token == token {
			reg.graces = append(reg.graces[:i], reg.graces[i+1:]...)
			return
		}
	}
}
