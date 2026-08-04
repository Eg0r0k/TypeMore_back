package ws

import "time"

// Per-CONNECTION rate limits for inbound frames.
//
// WHY THE CONNECTION AND NOT THE SEAT. The chat limiter used to live on the
// seat, and a seat is minted fresh by every join: leave + join_room handed the
// sender a full burst again, so the effective chat rate was unbounded and the
// rejoin itself broadcast two system messages to the room on the way. The
// budget has to sit on something the sender cannot cheaply replace. A
// connection is that: replacing it costs a new socket, a new hello and — for an
// authenticated account — the session lookup at the upgrade.
//
// A reconnect (new socket, resume token) does start a fresh budget. That is
// accepted: the cost of a full reconnect is orders of magnitude above sending a
// frame, and the alternative — keying the budget on the player id — would hand
// anyone who guesses an id the ability to exhaust someone else's.
//
// TWO BUDGETS, because the two kinds of traffic have different shapes.
//
//   - commands: everything that mutates room state or broadcasts. A human
//     presses these; 4/s sustained is already far above interactive use, and a
//     burst of 20 absorbs the legitimate flurry of settings a host applies in
//     one gesture.
//   - batches: `event_batch` during a match. The client's own cadence is one
//     batch per ~100 ms (docs/PROTOCOL.md), so 20/s sustained is a 2× ceiling
//     over a well-behaved client and never a bound a real run reaches.
//
// Neither is a security boundary on its own — the seat count, the frame caps
// and the capture caps are — but they turn "one connection can make the server
// do unbounded work per second" into a constant.
const (
	commandBurst  = 20
	commandRefill = 250 * time.Millisecond
	batchBurst    = 40
	batchRefill   = 50 * time.Millisecond
	// Chat keeps its own, tighter budget on top of the command one: a chat
	// message is seen by humans, and the rate that matters there is a reading
	// rate, not a server-cost one.
	chatBurst  = 5
	chatRefill = 400 * time.Millisecond
)

// bucket is a token bucket refilling one token per `refill`, capped at `burst`.
//
// Owned by the session's read-loop goroutine and touched from nowhere else, so
// it needs no synchronization — the same ownership rule the rest of the session
// state follows (see the field-ownership note on `session`).
type bucket struct {
	tokens float64
	last   time.Time
}

// allow consumes a token if one is available, refilling first.
//
// A zero `last` means the bucket has never been used: it starts FULL, so the
// first frame of a connection is never refused.
func (b *bucket) allow(now time.Time, burst int, refill time.Duration) bool {
	if b.last.IsZero() {
		b.tokens = float64(burst)
	} else {
		b.tokens = min(float64(burst), b.tokens+now.Sub(b.last).Seconds()/refill.Seconds())
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
