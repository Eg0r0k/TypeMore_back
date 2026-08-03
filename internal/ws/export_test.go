package ws

// Test-only surface. This file is compiled into the test binary only, which is
// how the external ws_test package (it dials real sockets against a real
// httptest server, so it has to live outside) can still assert an INTERNAL
// invariant without exporting anything into the production API.

import "fmt"

// CheckUserIndex verifies the registry's account -> seat index against the seats
// that actually exist, and is the assertion every one-seat-per-account test ends
// with. It checks the two halves of the rule separately, because they fail
// differently:
//
//   - no account holds two seats anywhere in the process. This is the rule
//     itself; a leak here means a create/join path found a way around the
//     registry decision.
//   - the index and the seats agree exactly, in both directions. A missing entry
//     makes the next connection of that account seat itself a SECOND time (the
//     rule silently stops applying); a surplus entry points at a seat that no
//     longer exists, and the next connection of that account is offered a
//     takeover of nothing.
//
// It walks the registry the way lobby.go does — room pointers copied under
// reg.mu, then each room under its own lock, one at a time — so it never holds
// two locks and cannot itself deadlock against the code it is checking. That
// also means it is only meaningful at a QUIESCENT moment; call it between
// operations, not during one.
func (h *Handler) CheckUserIndex() error {
	reg := h.reg

	reg.mu.Lock()
	rooms := make([]*Room, 0, len(reg.rooms))
	for _, room := range reg.rooms {
		rooms = append(rooms, room)
	}
	reg.mu.Unlock()

	live := make(map[string]seatRef)
	for _, room := range rooms {
		room.mu.Lock()
		seats := append([]*seat(nil), room.seats...)
		room.mu.Unlock()

		for _, st := range seats {
			if st.userID == "" {
				continue
			}
			if prev, dup := live[st.userID]; dup {
				return fmt.Errorf("account %q holds two seats: %s/%s and %s/%s",
					st.userID, prev.room.code, prev.seat.playerID, room.code, st.playerID)
			}
			live[st.userID] = seatRef{room: room, seat: st}
		}
	}

	reg.usersMu.Lock()
	indexed := make(map[string]seatRef, len(reg.users))
	for k, v := range reg.users {
		indexed[k] = v
	}
	reg.usersMu.Unlock()

	for userID, want := range live {
		got, ok := indexed[userID]
		if !ok {
			return fmt.Errorf("account %q sits in %s/%s but is not indexed",
				userID, want.room.code, want.seat.playerID)
		}
		if got.seat != want.seat || got.room != want.room {
			return fmt.Errorf("account %q is indexed at %s/%s but sits in %s/%s",
				userID, got.room.code, got.seat.playerID, want.room.code, want.seat.playerID)
		}
	}
	for userID, got := range indexed {
		if _, ok := live[userID]; !ok {
			return fmt.Errorf("account %q is indexed at %s/%s but holds no seat",
				userID, got.room.code, got.seat.playerID)
		}
	}
	return nil
}

// RoomCount reports how many rooms the registry currently holds. Tests use it to
// assert that a room a moving account emptied was actually dropped.
func (h *Handler) RoomCount() int {
	h.reg.mu.Lock()
	defer h.reg.mu.Unlock()
	return len(h.reg.rooms)
}

// GraceCount reports how many resume tokens are currently claimable. It is how
// the takeover tests catch a stranded grace entry: taking a seat over by ACCOUNT
// bypasses claimGrace, so the entry has to be dropped explicitly or it outlives
// every timer that would have cleaned it up.
func (h *Handler) GraceCount() int {
	h.reg.mu.Lock()
	defer h.reg.mu.Unlock()
	return len(h.reg.graces)
}
