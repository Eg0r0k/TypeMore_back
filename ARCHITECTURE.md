# TypeMore — System Architecture

TypeMore is a typing game that combines monkeytype-style typing tests with an
osu!-inspired scoring and rating layer: mods act as score multipliers, text
difficulty is rated like beatmaps, and a profile rating (TP) aggregates the
player's best runs. This document describes the full system: client core,
server, data flow, anti-cheat, and the determinism contract that ties them
together.

---

## 1. System overview

```
┌──────────────────────────── Browser ────────────────────────────┐
│  Vue 3 UI (windowed render, Shadow DOM words, transforms)       │
│      │ reads state per keystroke                                │
│  Pinia store factory ── input adapter (1 grapheme / insert)     │
│      │ events                                                   │
│  GameCore (pure sync reducer, TS)   Worker timer (delta-only)   │
│      │ seeded words (mulberry32 + FNV-1a dict hash)             │
│      └── append-only event log (insert/delete/commit/replace)   │
└───────────────┬─────────────────────────────────────────────────┘
                │ POST /runs (compressed event log)
┌───────────────▼──────────────── Server (Go, single binary) ─────┐
│  HTTP (chi) / WebSocket (coder/websocket)                       │
│  auth │ runs │ wordgen │ leaderboard │ rating │ admin │ match   │
│                │ enqueue                                        │
│  river job queue (Postgres-backed)                              │
│                │                                                │
│  replay worker: goja runs the SAME core bundle → recompute      │
│  metrics + score (versioned) → anti-cheat heuristics → status   │
│                │                                                │
│  PostgreSQL (source of truth, immutable logs)                   │
│  Redis (leaderboard ZSETs, sessions, rate limits — rebuildable) │
└─────────────────────────────────────────────────────────────────┘
```

Architectural style: **modular monolith**. One Go binary, Postgres, Redis.
No microservices; domain packages are isolated so that a future extraction
(realistically only `match`) stays cheap.

---

## 2. Client core

The game engine lives in `src/shared/core` (FSD-aligned, framework-free
TypeScript):

- **Event protocol** — append-only log of `insert / delete / commit / replace`
  events with branded `Seq` / `Ms` types (log format v1). The log is the single
  source of truth for a run; all metrics are derivable from it.
- **GameCore** — a pure, synchronous, per-instance reducer
  (`reduce / settle / foldLog`). Time enters only through events; the core never
  reads clocks. Multiple instances coexist (local player + `GhostCore`
  opponents in multiplayer).
- **Deterministic generation** — words are pre-generated from a seed via
  mulberry32; the dictionary is fingerprinted with an FNV-1a hash. Same seed +
  same dictionary ⇒ identical text everywhere.
- **Worker timer** — authoritative ticks come from a Web Worker with a
  delta-only time-base contract (the worker never emits absolute timestamps),
  ideal-grid cadence, and deadline-pinned finish for frozen tabs.
- **Rendering** — windowed word rendering (window + recycle), Shadow DOM word
  container (closed in production) as anti-cheat layer 1, transform-based caret
  / line-jump / scroll-tape. Perf gate: Playwright asserts bounded DOM node
  count and ≤2 DOM updates per keystroke at 10k words.

The core is deliberately pure and portable — this is what makes server-side
replay possible.

---

## 3. Determinism contract (client ↔ server)

Anti-cheat and scoring both rest on one invariant:

> **Replaying a run's event log on the server reproduces the exact same final
> state and metrics as the client computed.**

To guarantee this without maintaining two implementations, the server executes
the **same compiled JS bundle of the core** inside [goja](https://github.com/dop251/goja)
(a pure-Go JavaScript interpreter). Zero drift by construction. Replay is
asynchronous (queued), so interpreter overhead is irrelevant at current scale.

**Escape hatch** (only if validation throughput ever becomes a bottleneck):
port the core to native Go, gated by **golden vectors** — JSON fixtures
generated from the TypeScript test suite (`seed + event log → expected final
state + metrics`) that both implementations must satisfy in CI. The TypeScript
core remains the canonical client implementation either way.

**Rejected:** compiling a Go core to WASM for the browser. It would require
rewriting both the core and the entire client integration, adds a permanent
bundle-size tax (Go runtime) and a chatty JS↔WASM boundary on the per-keystroke
render path, and provides nothing beyond what goja / golden vectors already
give.

---

## 4. Server

### 4.1 Package layout

```
cmd/server/main.go       — composition root, manual DI
internal/
  platform/              — db (pgx), redis, config, middleware, slog; imports no domain
  auth/                  — OAuth (GitHub, Google), opaque sessions in Redis
  runs/                  — run ingestion, immutable log storage
  replay/                — goja + core bundle, validation worker
  score/                 — scoreV1 / tpV1: versioned pure functions
  leaderboard/           — buckets, Redis ZSET read-model, rebuild
  rating/                — TP: top-100 with decay, per-bucket dedup
  wordgen/               — seed issuance, dict hashes, star rating, daily challenge
  anticheat/             — heuristics on top of replay, flags, shadow-ban
  admin/                 — flagged-run review
  match/                 — (future) rooms, WS relay of input events
```

Layering rules: `platform` knows no domains; domains never import each other
directly (interfaces are declared at the consumer); transport handlers are
thin.

### 4.2 Run lifecycle

1. Client finishes a run → `POST /runs` with the compressed event log and its
   locally computed preview result.
2. Server: structural checks (size, format) → log stored in Postgres as an
   **immutable blob**, run status `pending` → job enqueued (river). Client gets
   an immediate response with the preview.
3. Replay worker: goja replay → metrics and score recomputed with the
   versioned formula → compared against client-reported numbers → anti-cheat
   heuristics (inter-key interval distribution, implausibly uniform timing,
   impossible speeds) → status `accepted` or `flagged`.
4. On `accepted`: bucket leaderboard ZSET updated; profile TP incrementally
   recomputed.

### 4.3 Storage

- **PostgreSQL is the source of truth.** Key tables: `users`, `runs`
  (bucket fields: mode / duration / word count / language; mods bitmask; seed;
  dict_hash; score, wpm, acc, tp; `score_version`, `tp_version`; status;
  compressed log; timestamps), `daily_challenges`, `flags`.
- **Redis is a rebuildable read-model**: leaderboard ZSETs per bucket,
  sessions, rate-limit counters. Any ZSET can be dropped and rebuilt from
  Postgres.
- Event logs are immutable and kept indefinitely; they migrate to S3-compatible
  object storage when volume demands it.

### 4.4 Formula versioning

Score and TP formulas are versioned pure functions (`scoreV1`, `tpV1`). Every
run stores the formula versions used. A rebalance is a nightly batch: replay or
recompute from stored logs, bump versions, rebuild leaderboards — the same
mechanism as osu! pp reworks.

### 4.5 Seeds & dictionary distribution

The server never generates or streams words. Per run, only a **seed** travels
over the network; the client generates the text locally
(mulberry32 + dictionary). Streaming words mid-run is explicitly rejected: it
would break log self-sufficiency (and with it replay validation), put network
latency on the hot path, and kill offline play.

Dictionaries are **immutable, versioned static assets**:

- Served as static files with a content hash in the filename
  (`english-1k.<hash>.json`) and `Cache-Control: immutable` — downloaded once
  per version, cacheable on a CDN. Zero per-run traffic.
- The FNV-1a `dict_hash` (already computed by the core) is the version
  identifier. The server keeps a **dictionary registry** and stores **every
  version ever published** — event logs are immutable and kept forever, so
  every historical run must remain replayable against its own dictionary.
- A ranked run is accepted only if its `dict_hash` exists in the registry; the
  replay worker loads the matching dictionary version for goja.
- Updating a dictionary = publishing a new file with a new hash; old runs keep
  validating against theirs.
- Multiplayer: the server sends `seed + dict_hash` to all match participants;
  a client missing that dictionary version downloads the static asset before
  the match starts, then everything runs locally.

**Quotes** are the one mode with fixed (non-seeded) text and are treated like
osu! beatmaps: a server-side **quote registry** (`quote_id + text + hash`,
every version kept forever for replay) mirrors the dictionary registry; the
replay worker loads the text by `quote_id` instead of generating from a seed.
The core's `GenerationConfig` gains a text-source abstraction
(`seeded | fixed`). Quotes have **per-quote leaderboards** with their own star
rating but are excluded from global score buckets and TP (memorizable finite
corpus, length variance, cherry-picking) — see `SCORING_CONCEPT.md`.

Daily challenge = one seed + fixed mod set per day (occasionally a quote),
with its own leaderboard.

### 4.6 Auth & abuse

- OAuth (GitHub, Google) via `golang.org/x/oauth2`; opaque session tokens in
  httpOnly cookies; sessions in Redis. No JWT (single service; revocation
  matters more than statelessness). No stored passwords.
- Rate limiting (Redis, per-user and per-IP) on `POST /runs` and auth
  endpoints.
- Flagged runs go to an admin review queue; shadow-ban supported.

---

## 5. Scoring & rating (summary)

Full design lives in `SCORING_CONCEPT.md`. The architectural constraints it
imposes:

- Client score is **display-only**; the server-side replay result is
  authoritative.
- Scores are comparable only within a bucket
  (`mode × duration/word count × language`); WPM leaderboards exist in
  parallel.
- TP (profile rating) is a **separate formula** from score — decoupled so
  either can be rebalanced independently — computed as a decayed sum of the
  top-100 runs with per-bucket deduplication.
- Text difficulty (star rating) is computed deterministically from the seed,
  per language.

---

## 6. Multiplayer (planned)

Up to 5 players over WebSocket. The network layer is a **relay of input
events**: each client runs opponents as `GhostCore` instances fed by relayed
logs; the server validates post-hoc via the same replay pipeline. This keeps
the server out of the real-time simulation loop. If authoritative live
simulation is ever required, that is the trigger for the native Go core port
(Section 3) — not WASM.

Planned flow: transport → match store → UI, living in `internal/match` within
the same binary until scale forces extraction.

### 6.1 Match timing model

> This section consolidates the shared client↔server timing design into the
> server architecture doc so it is self-contained. The **wire-level** contract
> (exact frames and field names) lives in [`docs/PROTOCOL.md`](docs/PROTOCOL.md);
> this is the *why* behind it.

A multiplayer match needs every client to hit **the same `t=0` ("go") instant**
despite each running on its own unsynchronised wall clock. The design keeps the
server out of the real-time loop (per §6) while still coordinating a shared
start:

- **Server clock is the reference.** All shared instants (notably the countdown
  `goAtServerMs`) are expressed in the server's wall clock. The server never
  waits on clients to align; it just publishes an instant.
- **Clients estimate their offset via an NTP-style handshake.** Before any
  countdown a client exchanges ≥5 `ntp_ping`/`ntp_pong` pairs. Each pong carries
  `t0` (client send, echoed), `t1` (server receive), `t2` (server send); the
  client reads `t3` (its own receive) locally. Per pair,
  `offset = ((t1−t0) + (t2−t3)) / 2` and `rtt = (t3−t0) − (t2−t1)`. The client
  discards pairs whose RTT exceeds 3× the minimum observed RTT and takes the
  **median** offset — a jitter-robust estimate of `serverClock − clientClock`.
- **Countdown converts locally.** On `countdown{goAtServerMs, seed, dictHash,
  lang, config}`, each client computes `localGoTime = goAtServerMs − offset` and
  schedules its own 3-2-1 to fire at that local time. Because every client
  subtracts its own offset from the *same* server instant, all the local "go"s
  coincide on the real timeline.
- **The core stays clock-free.** Per §2, `GameCore` takes time only through
  events; the timing model lives entirely in the transport/scheduling layer, so
  determinism and server-side replay are unaffected.

Timestamps on the wire are integer milliseconds since the Unix epoch (UTC).
Post-`go`, gameplay events flow as the relayed `event_batch`/`peer_batch`
stream; timing plausibility of a finished run is checked post-hoc by the replay
pipeline, not enforced live.

---

## 7. Non-goals

- Microservices, Kubernetes, message brokers — not before they are forced.
- Cross-language difficulty normalization — per-language buckets instead.
- Client-trusted results of any kind.
- JWT-based auth.

## 8. Build order

Each stage ships as a working product:

1. Auth + run ingestion with log storage (no validation yet)
2. Bucketed score / WPM leaderboards
3. Replay worker (goja) + `scoreV1`
4. Anti-cheat heuristics + flags + admin review
5. Daily challenge
6. TP rating
7. Match (multiplayer)
