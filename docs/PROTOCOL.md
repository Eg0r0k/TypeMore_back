# TypeMore Realtime Protocol — v1

Status: **v1 (draft, this phase implements `hello` + NTP only).**
This document is the contract shared **verbatim** between the server
(`typemore-server`) and the frontend repository. It is standalone: it does not
reference any Go or TypeScript type. Where the server's realtime behaviour is
defined, it is defined here.

---

## 1. Transport

- **WebSocket**, endpoint `GET /ws` (upgrade).
- **JSON text frames only.** Binary frames are rejected with a `bad_message`
  error. One JSON value per frame.
- **Protocol version is `1` (numeric).** It is announced by the client in its
  `hello` and is a plain integer, not a string.
- The server speaks exactly one protocol version and **never translates between
  versions**. A client announcing a different version is rejected (see
  `version_mismatch`) and disconnected.

### Frame shape

Every frame — both directions — is a JSON object with a `type` string
discriminator plus that message's payload fields:

```json
{ "type": "<message type>", "...": "payload fields" }
```

Unknown message types are answered with a `bad_message` error and the connection
stays open (forward-compatibility: a newer client may send a type this server
does not implement yet).

### Timestamps and units

All wall-clock timestamps on the wire (`t0`, `t1`, `t2`, `goAtServerMs`) are
**integer milliseconds since the Unix epoch (UTC)**. `t1`/`t2` are read from the
**server** clock; `t0` is read from the **client** clock. The client's `t3`
(defined below) never travels on the wire — it is a client-local reading.

---

## 2. Connection lifecycle

1. Client opens the WebSocket.
2. Client **MUST** send `hello` as its **first** frame. Any other frame before a
   successful `hello` is answered with `bad_message` (connection stays open; the
   client may then send a valid `hello`).
3. Server validates the protocol version and nick:
   - version mismatch ⇒ `error` with code `version_mismatch`, then the server
     **closes** the connection;
   - invalid nick ⇒ `error` with code `bad_message` (connection stays open, may
     retry);
   - otherwise ⇒ `hello_ok`.
4. After `hello_ok` the client may send `ntp_ping` (this phase) and, in later
   phases, the room/match messages.

### Heartbeat

The server uses WebSocket **ping/pong** as a liveness probe: it pings after
**15 s** of connection idleness. **2 missed** pongs ⇒ the peer is considered
disconnected and the disconnect grace window (below) begins. WebSocket
ping/pong is handled by the transport layer, not by application `type` frames;
clients do not implement it manually (browsers answer pings automatically).

*(Heartbeat is specified now; enforced from the relay phase.)*

---

## 3. Client → server messages

### `hello`

First frame of every connection. `nick` is the guest display identity, **1–16
characters** (counted in Unicode code points, not bytes).

`resumeToken` is **optional**: on a mid-match reconnect the client re-sends its
hello with the `resumeToken` it was given in the previous `hello_ok`, and the
server restores its seat and replays the buffered backlog (see §6). Omit it on a
fresh connection. Logged-in users are additionally identified by their session
cookie (from the auth layer), which survives independently of this token.

```json
{ "type": "hello", "protocolVersion": 1, "nick": "Neo" }
```

```json
{ "type": "hello", "protocolVersion": 1, "nick": "Neo", "resumeToken": "b3f1…(64 hex chars)" }
```

### `ntp_ping`

A clock-synchronisation ping. `t0` is the client's clock reading at send time.

```json
{ "type": "ntp_ping", "t0": 1737645123456 }
```

### `create_room`

Requests a new room. Payload is finalised in the relay phase.

```json
{ "type": "create_room" }
```

### `join_room`

Joins an existing room by its code (see §5 for the code format).

```json
{ "type": "join_room", "code": "K7GQ2M" }
```

### `ready`

Marks the sending player ready to start.

```json
{ "type": "ready" }
```

### `event_batch`

Relays a batch of the frontend's **log-v1 `GameEvent`** objects. In this phase
(and until the relay phase) these events are **opaque** to the server — it does
not parse or validate their contents beyond the batch envelope; structural
validation lands in the relay phase.

- `matchId` — the match this batch belongs to.
- `playerId` — the sender (as issued in `hello_ok`).
- `batchSeq` — a **monotonic, per-player** batch counter starting at `1`. It lets
  the server detect gaps and duplicates when replaying the backlog after a
  reconnect; the transport preserves order, `batchSeq` makes loss/duplication
  detectable.
- `version` — the **event-log format** version (log-v1 ⇒ `1`).
- `events` — the ordered array of opaque event objects.

**Batching contract (client obligation):** the client flushes a batch every
**≤ 100 ms** or every **16 events**, whichever comes first. Batches are
**strictly sequence-ordered** — the server relays and appends them in the order
received per player and never reorders.

```json
{
  "type": "event_batch",
  "matchId": "m_9f3a",
  "playerId": "3b1e...c4",
  "batchSeq": 7,
  "version": 1,
  "events": [
    { "k": "insert", "seq": 1, "t": 12, "ch": "t" },
    { "k": "insert", "seq": 2, "t": 96, "ch": "h" },
    { "k": "commit", "seq": 3, "t": 240 }
  ]
}
```

> The `events` objects above are illustrative of the frontend's log-v1 shape;
> the server treats them as opaque this phase. Their canonical schema lives in
> the frontend repository's core (`insert` / `delete` / `commit` / `replace`).

### `leave`

Voluntarily leaves the current room.

```json
{ "type": "leave" }
```

---

## 4. Server → client messages

### `hello_ok`

Acknowledges a valid `hello`. `playerId` is the server-issued opaque identity
for this connection; `serverVersion` echoes the protocol version the server
speaks (always equal to the client's, since a mismatch would have been rejected).
`resumeToken` is a fresh **256-bit random** secret (64 hex chars): the client
stores it and presents it in a later `hello` to reclaim its seat after a
disconnect (see §6). It is a capability, not the `playerId` — the `playerId` may
be shown to peers, the `resumeToken` never is.

```json
{ "type": "hello_ok", "playerId": "3b1e9c2f7a8d4e10b6c5a1d2e3f40506", "serverVersion": 1, "resumeToken": "b3f1...c9" }
```

### `error`

Reports a problem. `code` is one of the fixed values below; `message` is a
human-readable explanation (not for programmatic use).

```json
{ "type": "error", "code": "bad_message", "message": "hello must be the first message" }
```

| `code`             | Meaning                                                        | Closes connection? |
|--------------------|---------------------------------------------------------------|--------------------|
| `version_mismatch` | Client protocol version ≠ server's                            | **Yes** (after the error frame is sent) |
| `bad_message`      | Malformed frame, wrong order, unknown/unsupported type, bad nick | No |
| `room_not_found`   | `join_room` code has no room                                   | No |
| `room_full`        | Room already at capacity (5)                                   | No |
| `not_in_room`      | Room-scoped message sent while not in a room                   | No |
| `internal`         | Unexpected server error                                        | No |

### `ntp_pong`

Answers an `ntp_ping`.

- `t0` — the client's `t0`, **echoed unchanged**.
- `t1` — the server clock at the moment it **received** the ping.
- `t2` — the server clock at the moment it **sent** the pong.

```json
{ "type": "ntp_pong", "t0": 1737645123456, "t1": 1737645123500, "t2": 1737645123501 }
```

#### Client clock-offset procedure (frontend implementer, read this)

The countdown (`goAtServerMs`) is expressed in the **server** clock. To schedule
the local 3-2-1 the client must know its offset from the server clock. Compute
it like NTP:

1. Send **at least 5** `ntp_ping` / `ntp_pong` pairs **before any countdown**.
2. For each pair, let `t3` be the client clock reading at the moment the
   `ntp_pong` **arrives**. Then:

   ```
   offset = ((t1 − t0) + (t2 − t3)) / 2
   rtt    = (t3 − t0) − (t2 − t1)
   ```

   `offset` is `serverClock − clientClock` (add it to a client time to get
   server time; subtract it from a server time to get client time).
3. **Discard** any pair whose `rtt` exceeds **3× the minimum observed `rtt`**
   (these are jittered outliers).
4. Use the **median** `offset` of the surviving pairs.

To convert a countdown: `localGoTime = goAtServerMs − offset`.

### `room_state`

Full snapshot of a room, sent on any change. `settings` is opaque this phase
(finalised in the relay phase).

```json
{
  "type": "room_state",
  "code": "K7GQ2M",
  "players": [
    { "playerId": "3b1e...c4", "nick": "Neo", "ready": true },
    { "playerId": "8a2f...91", "nick": "Trinity", "ready": false }
  ],
  "settings": { }
}
```

### `countdown`

Announces the shared match start. All clients convert `goAtServerMs` via their
NTP offset and schedule the local 3-2-1; the shared **t=0** (the "go" instant)
is identical for everyone.

- `goAtServerMs` — server-clock instant of t=0.
- `seed` — the generation seed: an **integer in `[0, 2³²−1]`** (mulberry32 is a
  32-bit PRNG; this range fits a JSON number with room to spare — no 2⁵³ issue).
- `dictHash` — FNV-1a dictionary fingerprint the match runs against; a client
  missing this dictionary version downloads the static asset before "go".
- `lang` — language code of the dictionary.
- `config` — the match generation/mods configuration; opaque this phase.

```json
{
  "type": "countdown",
  "goAtServerMs": 1737645130000,
  "seed": 2864901,
  "dictHash": "a1b2c3d4",
  "lang": "en",
  "config": { }
}
```

### `peer_batch`

Relays another player's events to this client, **order preserved per player**.
The relay is **lossless**. `events` is opaque this phase (same shape as
`event_batch.events`).

```json
{ "type": "peer_batch", "playerId": "8a2f...91", "events": [ { "k": "insert", "seq": 1 } ] }
```

### `peer_status`

Reports a peer's lifecycle transition.

- `status` is one of: `joined`, `left`, `disconnected`, `reconnected`,
  `finished`, `dnf`.

```json
{ "type": "peer_status", "playerId": "8a2f...91", "status": "disconnected" }
```

---

## 5. Rooms

- **Codes** are **6 characters**, drawn from a **human-safe alphabet** that
  excludes the ambiguous glyphs `0`, `O`, `1`, and `I`.
- A room holds **at most 5 players**.
- A room **dies when empty**.

*(Rooms are specified now; implemented in the relay phase.)*

---

## 6. Server obligations (specified now, implemented in the relay phase)

These are part of the v1 contract but are **not** implemented in this phase.
They are recorded here so the frontend can rely on them.

- **Inbound timestamping.** Every `event_batch` is stamped with its server
  arrival time (`recvServerMs`) and appended to the per-player authoritative log
  for the match.
- **Wall-clock plausibility.** A run's final event time must fall within
  `[go, lastBatchRecv + RTT tolerance]`. Violations are **flagged, not rejected
  mid-match** — the match continues and the flag is resolved out of band.
- **Disconnect policy.** On a mid-match WebSocket drop the server **keeps the
  seat for a 15 s grace window** and **buffers the peer-relay backlog**. A
  reconnect (a `hello` presenting the same `resumeToken`) **replays the
  backlog** and resumes. Grace expiry ⇒ the peer is broadcast as `dnf` and the
  match continues for the others. Spectator-side ghosts **freeze during the gap
  and fast-forward on catch-up** (the client's jitter buffer absorbs this).
- **Heartbeat.** WebSocket ping/pong at 15 s idle; 2 missed pongs ⇒ considered
  disconnected (grace window starts).

---

## 7. Open questions

Gaps in the source specification, surfaced here rather than decided unilaterally.
Items marked **RESOLVED** carry the owner's decision (already reflected above);
the rest still need a decision before the relay phase implements them.

1. **Reconnect token transport — RESOLVED.** `hello_ok` issues a `resumeToken`
   (256-bit random, 64 hex chars); a reconnect is `hello {resumeToken}`. The
   token is a secret capability distinct from the peer-visible `playerId`, so a
   guessed `playerId` cannot hijack a seat. Logged-in users are additionally
   covered by their auth session cookie. (See §3 `hello` / §4 `hello_ok`.)
2. **`event_batch` sequence field — RESOLVED.** The envelope carries an explicit
   `batchSeq` (monotonic, per-player, starting at 1). Transport order is
   preserved by WebSocket/TCP; `batchSeq` exists so gaps and duplicates are
   detectable during backlog replay after a reconnect. (See §3 `event_batch`.)
3. **`seed` encoding — RESOLVED.** `seed` is an integer in `[0, 2³²−1]`
   (mulberry32 is 32-bit; safely within JSON's 2⁵³ integer range). (See §4
   `countdown`.)
4. **`event_batch.version` semantics.** Confirmed as the **event-log** format
   version (log-v1 ⇒ 1), *not* the protocol version. Flagged only because the
   name collides conceptually with `protocolVersion`; the two version numbers
   evolve independently.
5. **Room-code case sensitivity.** The 6-char human-safe alphabet excludes
   `0/O/1/I`; is the code case-sensitive, and does `join_room` normalise case
   (e.g. upper-case the input) before lookup? (Human-safe codes usually imply
   case-insensitive entry.)
6. **Room message acknowledgements.** What does the server send in response to
   `create_room` / `join_room` / `ready` / `leave` on success — always a fresh
   `room_state` broadcast, or a targeted ack too? Assumed `room_state`, not
   stated.
7. **Max frame size.** No inbound frame-size limit is specified. The server
   currently caps a single frame at 1 MiB; the relay phase's `event_batch`
   sizing (≤16 events/batch) should confirm this ceiling is comfortable.
8. **`ntp_pong.t2` precision.** `t2` is captured just before the frame is queued
   for the socket writer, not at the exact byte-on-wire instant; the queue is
   effectively empty for the tiny NTP frames, so the gap is sub-millisecond, but
   it is a known, bounded approximation the offset math already tolerates via
   the RTT filter.
