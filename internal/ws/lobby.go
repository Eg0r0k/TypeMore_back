package ws

// Public lobby discovery: GET /api/v1/rooms.
//
// This is HTTP, and it is deliberately OUTSIDE the WebSocket protocol.
// Discovery is a stateless read — "what is open right now" — while the protocol
// is a session: a client that has not picked a room yet has nothing to hold a
// session about, and making it upgrade, `hello`, and hold a socket open just to
// see a list would be a connection per browser tab on the lobby screen. The
// list is therefore a plain cacheable GET, mounted beside the other public
// routes. See docs/PROTOCOL.md §5.

import (
	"encoding/json"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/typemore/typemore-server/internal/protocol"
)

// RateLimiter decides whether an action keyed by a string (a client IP here)
// may proceed. Consumer-declared, exactly as internal/runs declares its own, so
// the transport layer imports no sibling domain; the composition root passes
// the shared in-memory token bucket the auth and runs surfaces already use.
type RateLimiter interface {
	// Allow reports whether the action for key is permitted right now.
	Allow(key string) bool
}

// lobbyResponse is the envelope of GET /api/v1/rooms. It is an object with a
// `rooms` key rather than a bare array, matching the `{ "buckets": … }` /
// `{ "quotes": … }` shape the rest of the API uses.
type lobbyResponse struct {
	Rooms []roomView `json:"rooms"`
}

// lobbyError mirrors the sibling domains' client-facing error shape (a stable
// machine code in "error", a human message in "message").
type lobbyError struct {
	Code    string `json:"error"`
	Message string `json:"message"`
}

// roomView is one listed room. It carries only what a lobby card needs: no
// player identities, no host id, no dictionary fingerprint — a public list is
// not a window into a room's occupants.
type roomView struct {
	Code        string           `json:"code"`
	Name        string           `json:"name"`
	PlayerCount int              `json:"playerCount"`
	MaxPlayers  int              `json:"maxPlayers"`
	InMatch     bool             `json:"inMatch"`
	Settings    roomSettingsView `json:"settings"`
}

// roomSettingsView is the match-shape subset of protocol.Settings. DurationMs
// and WordCount are MUTUALLY EXCLUSIVE — pointers so that exactly the one the
// mode uses is present and the other is absent, never a misleading zero. Same
// treatment the leaderboard catalogue gives its bucket dimension.
type roomSettingsView struct {
	Mode       string `json:"mode"`
	DurationMs *int   `json:"durationMs,omitempty"`
	WordCount  *int   `json:"wordCount,omitempty"`
	Lang       string `json:"lang"`
}

// lobbyEntry is one room copied out from under that room's own lock: a plain
// value with no pointer back into live room state, so the sort and the JSON
// encoding that follow touch no shared memory and hold no lock. created is the
// ordering key and never reaches the wire.
type lobbyEntry struct {
	view    roomView
	created time.Time
}

// LobbyHandler returns the handler for GET /api/v1/rooms: the public list of
// open rooms. It is public and sessionless — a guest browsing for a game has no
// account and no socket yet.
//
// limiter throttles per client IP; a nil limiter disables throttling, which is
// only ever right in a test.
func (h *Handler) LobbyHandler(limiter RateLimiter) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if limiter != nil && !limiter.Allow(lobbyClientIP(r)) {
			h.writeLobbyJSON(w, http.StatusTooManyRequests, lobbyError{
				Code:    "rate_limited",
				Message: "too many room-list requests; slow down and try again shortly",
			})
			return
		}

		entries := h.reg.openRooms()
		// Always an array, never null: an empty lobby is `{"rooms":[]}`.
		rooms := make([]roomView, len(entries))
		for i := range entries {
			rooms[i] = entries[i].view
		}
		h.writeLobbyJSON(w, http.StatusOK, lobbyResponse{Rooms: rooms})
	})
}

// openRooms snapshots the publicly listed rooms, ordered for the lobby list.
//
// THE LOCKING IS THE POINT OF THIS FUNCTION. The documented order is
// Registry.mu THEN Room.mu (see the Registry doc comment), and the cheapest way
// to honour it is to never hold both:
//
//  1. take reg.mu just long enough to copy the room POINTERS into a local
//     slice, then release it — the registry is free again before any room lock
//     is touched, so a create/join/removeIfEmpty never waits on this walk;
//  2. take each Room.mu in turn, one at a time, and copy that room into a plain
//     value; the lock is released before the next room is visited, so no lock
//     is ever held across all rooms;
//  3. sort and encode with nothing held at all.
//
// A room torn down mid-walk is harmless: the *Room copied in step 1 stays a
// valid object (the registry only drops the map entry), so step 2 observes its
// final, self-consistent state under its own lock — an emptied room, which is
// filtered out. There is no window in which a half-built room can be published
// and none in which the walk can panic.
func (reg *Registry) openRooms() []lobbyEntry {
	reg.mu.Lock()
	rooms := make([]*Room, 0, len(reg.rooms))
	for _, room := range reg.rooms {
		rooms = append(rooms, room)
	}
	reg.mu.Unlock()

	out := make([]lobbyEntry, 0, len(rooms))
	for _, room := range rooms {
		if entry, ok := room.lobbyEntryOf(); ok {
			out = append(out, entry)
		}
	}

	slices.SortFunc(out, func(a, b lobbyEntry) int {
		// Fullest first — the lobby's job is showing where the people are.
		if d := b.view.PlayerCount - a.view.PlayerCount; d != 0 {
			return d
		}
		// Then oldest first, so a room opened a second ago cannot displace one
		// that has been waiting.
		if d := a.created.Compare(b.created); d != 0 {
			return d
		}
		// Then the code, which makes the order TOTAL: two rooms created inside
		// the same clock tick would otherwise be ordered by map iteration and
		// visibly swap places between two polls of an idle lobby.
		return strings.Compare(a.view.Code, b.view.Code)
	})
	return out
}

// lobbyEntryOf snapshots this room for the public list under the room lock,
// reporting ok=false when it must not be listed:
//
//   - private rooms, in any state — a code is the only way in, by design;
//   - seatless rooms, which are rooms mid-teardown (the last seat left between
//     the registry walk and this lock) and are about to vanish from the
//     registry anyway.
func (r *Room) lobbyEntryOf() (lobbyEntry, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.settings.Visibility != protocol.VisibilityOpen || len(r.seats) == 0 {
		return lobbyEntry{}, false
	}

	view := roomView{
		Code:        r.code,
		Name:        r.settings.Name,
		PlayerCount: len(r.seats),
		MaxPlayers:  roomCapacity,
		InMatch:     r.inMatch,
		Settings: roomSettingsView{
			Mode: r.settings.Mode,
			Lang: r.settings.Lang,
		},
	}
	// Exactly one of the two dimensions, chosen by the mode (docs/PROTOCOL.md §5).
	//
	// The counted arm asks IsCounted rather than naming the modes, which is the
	// same question the finish window, the AFK share rule and the per-word
	// ceiling ask. Naming them is what went wrong here: the switch listed time
	// and words, quote fell through both, and a quote room appeared in the public
	// list carrying NEITHER dimension — a listing that says nothing about the
	// length of the match it is advertising. The next counted mode would have
	// done it again.
	switch {
	case r.settings.Mode == protocol.ModeTime:
		duration := r.settings.DurationMs
		view.Settings.DurationMs = &duration
	case protocol.IsCounted(r.settings.Mode):
		words := r.settings.WordCount
		view.Settings.WordCount = &words
	}
	return lobbyEntry{view: view, created: r.createdAt}, true
}

func (h *Handler) writeLobbyJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		h.log.Error("encode room list", "err", err)
	}
}

// lobbyClientIP is the rate-limit key: the peer address only. A forwarded
// header is caller-controlled, so trusting it would make the limit a suggestion
// — the same reasoning (and the same two lines) as the auth and runs domains.
func lobbyClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
