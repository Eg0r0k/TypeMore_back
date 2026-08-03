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
// Lock ordering is always Registry.mu THEN Room.mu THEN Registry.usersMu —
// create/join hold the registry lock while seating (which takes the room lock,
// which takes the account index lock), and removeIfEmpty does the same, so a
// concurrent join can never race a room out from under a caller. usersMu is a
// LEAF: nothing is ever acquired while it is held, which is what lets a seat
// removal deep inside the room lock (a kick, a dnf, the match-end reap) keep the
// index honest without inverting the order above it.
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

	// usersMu guards users. It is separate from mu on purpose: seats disappear
	// under a ROOM lock (kick, grace expiry, the match-end reap) and taking mu
	// there would invert the documented order, while leaving those paths
	// unindexed would let a stale entry outlive the seat it names.
	usersMu sync.Mutex
	// users is the account -> seat index that makes "one seat per account" a
	// fact rather than a hope (docs/PROTOCOL.md §5). Only authenticated seats
	// appear: a guest has no account id, and keying guests off the empty string
	// would collapse every guest in the process into one entry and let the first
	// guest's seat be taken over by the next one.
	users map[string]seatRef
}

// seatRef locates one seat: the room that owns it and the seat itself. The room
// is needed because the index answers "where is this account sitting", and the
// seat pointer is what makes deregistration safe — an entry is only dropped when
// it still names the seat being removed, so a stale removal cannot delete the
// newer seat that replaced it.
type seatRef struct {
	room *Room
	seat *seat
}

// graceEntry links a live resume token to the room and seat awaiting reconnect.
type graceEntry struct {
	token string
	room  *Room
	seat  *seat
}

// seatOutcome is the registry's whole decision about one create_room/join_room,
// taken under reg.mu so no two connections of the same account can both be told
// "yes". Exactly one of room / errCode is set.
//
// reclaim non-nil means the account already had a seat in this room and the
// caller must re-attach to THAT seat rather than expect a fresh one; the seat has
// already been parked in the disconnected state for it.
//
// Displacing the older connection is NOT reported here: it happens under the
// room lock, at the moment the seat changes hands, because that is the only
// ordering that cannot race the displaced connection's own teardown (see
// Room.displaceLocked).
type seatOutcome struct {
	room    *Room
	reclaim *seat
	errCode string
}

// NewRegistry builds an empty room registry. store (may be nil) receives each
// finished match's capture.
func NewRegistry(log *slog.Logger, store MatchStore) *Registry {
	return &Registry{
		log:    log,
		store:  store,
		timing: defaultTiming(),
		rooms:  make(map[string]*Room),
		users:  make(map[string]seatRef),
	}
}

// create opens a new room with host seated as its first (host) seat.
//
// A create_room is by definition a room the account is not already in, so an
// existing seat is always a MOVE: it is released here (freeing it before the new
// room exists, which is safe because a create cannot fail for capacity) and its
// connection is handed back for the caller to displace. The one refusal is a
// racing mid-match seat elsewhere — see Room.releaseSeat.
func (reg *Registry) create(host *session) seatOutcome {
	reg.mu.Lock()
	defer reg.mu.Unlock()

	if cur, st, held := reg.seatOfUser(host.userID); held {
		if !cur.releaseSeat(st) {
			return seatOutcome{errCode: protocol.CodeInMatchElsewhere}
		}
		reg.removeIfEmptyLocked(cur.code)
	}

	code := reg.freeCodeLocked()
	room := newRoom(code, reg, reg.log, reg.store)
	reg.rooms[code] = room
	room.seat(host, true)
	return seatOutcome{room: room}
}

// join seats joiner in the room named by code (matched case-insensitively).
//
// The whole account-seat decision happens under reg.mu, which is what makes it
// race-free: every seat in the process is created here or in create, so an
// account that is seatless at the top of this function cannot acquire a seat
// before the bottom of it.
//
// The order of the three checks is load-bearing:
//
//  1. the room must exist — a move must not cost the old seat if the
//     destination is a typo;
//  2. an existing seat in THIS room is a reclaim, and capacity never enters into
//     it (the seat is already counted);
//  3. capacity is checked BEFORE the old seat is released, so a failed join into
//     a full room leaves the account exactly where it was. Capacity cannot
//     change under us: seats are only ever added here and in create, both under
//     reg.mu, and a concurrent removal only makes more room.
func (reg *Registry) join(code string, joiner *session) seatOutcome {
	code = normalizeCode(code)

	reg.mu.Lock()
	defer reg.mu.Unlock()

	room := reg.rooms[code]
	if room == nil {
		return seatOutcome{errCode: protocol.CodeRoomNotFound}
	}

	cur, st, held := reg.seatOfUser(joiner.userID)
	if held && cur == room {
		room.detachSeat(st)
		return seatOutcome{room: room, reclaim: st}
	}
	if room.full() {
		return seatOutcome{errCode: protocol.CodeRoomFull}
	}

	if held {
		if !cur.releaseSeat(st) {
			return seatOutcome{errCode: protocol.CodeInMatchElsewhere}
		}
		reg.removeIfEmptyLocked(cur.code)
	}
	if !room.seat(joiner, false) {
		// Unreachable: capacity was checked above under this same lock. Report it
		// rather than return a room the caller was never seated in.
		return seatOutcome{errCode: protocol.CodeRoomFull}
	}
	return seatOutcome{room: room}
}

// indexSeat records st as the seat held by its account. Guests (no account id)
// are not indexed. Called from Room.seat under the room lock.
func (reg *Registry) indexSeat(room *Room, st *seat) {
	if st.userID == "" {
		return
	}
	reg.usersMu.Lock()
	defer reg.usersMu.Unlock()
	reg.users[st.userID] = seatRef{room: room, seat: st}
}

// unindexSeat drops st's account entry, but only if the index still names THIS
// seat: a takeover into another room writes the new entry before the old seat is
// removed, and an unconditional delete there would erase the seat that is
// actually live. Called from Room.removeSeatLocked, the single choke point every
// seat removal passes through.
func (reg *Registry) unindexSeat(st *seat) {
	if st.userID == "" {
		return
	}
	reg.usersMu.Lock()
	defer reg.usersMu.Unlock()
	if ref, ok := reg.users[st.userID]; ok && ref.seat == st {
		delete(reg.users, st.userID)
	}
}

// seatOfUser reports the room and seat currently held by userID. An empty
// userID (a guest) never matches.
func (reg *Registry) seatOfUser(userID string) (*Room, *seat, bool) {
	if userID == "" {
		return nil, nil, false
	}
	reg.usersMu.Lock()
	defer reg.usersMu.Unlock()
	ref, ok := reg.users[userID]
	if !ok {
		return nil, nil, false
	}
	return ref.room, ref.seat, true
}

// removeIfEmpty drops the room named by code if it currently has no seats. It is
// called after any departure; the emptiness re-check under the registry lock
// means a join that arrived in the meantime keeps the room alive.
func (reg *Registry) removeIfEmpty(code string) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	reg.removeIfEmptyLocked(code)
}

// removeIfEmptyLocked is removeIfEmpty for callers already holding reg.mu (the
// seat-move paths in create/join, which must not re-enter the registry lock).
func (reg *Registry) removeIfEmptyLocked(code string) {
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
