# TypeMore Server — Capacity

One page: what each layer costs, what that implies as a ceiling, and how many
concurrent players that is.

**No new measurements.** Every number here is distilled from
[`PERFORMANCE.md`](PERFORMANCE.md), which says where it came from and which test
re-runs it. Where a figure is derived rather than measured it is marked
**[DERIVED]** and its arithmetic is shown. If a number here and a number there
ever disagree, PERFORMANCE.md is right.

## The assumptions, stated once

Concurrency figures depend entirely on what a "player" is assumed to do, so the
conversions below use exactly these and nothing else:

| | | why |
|---|---|---|
| **Runs per player per minute** | **1** | A 60 s run plus a results screen, back to back, with no pauses. A player doing 15 s runs continuously is 4× this and is called out where it matters. |
| **Batches per second per match seat** | **10** | PROTOCOL.md §3 obliges a flush every ≤100 ms. |
| **Events per batch** | **1.25** | 150 wpm × 5 chars ÷ 60 = 12.5 keystrokes/s over 10 flushes. Rounding to 2 would inflate the wire by 60%. |
| **Seats per match** | **5** | The relay fixture's room size. |
| **Host** | one box, 6 cores / 12 threads, Postgres alongside | The measurement rig. A separate database host moves several rows and none of the conclusions. |

Two of these are load-bearing: a **run** costs ingestion + one replay + one
projection, and a **match seat** costs 10 relays/s of fan-out. They are
independent workloads and they saturate on different resources.

## The table

| Layer | Workload | Measured | Implied ceiling | Concurrent players | What that means |
|---|---|---|---|---|---|
| **Auth** | one argon2id hash | 24.8 ms, 19.9 MB | gate of 8 → **~110 logins/s**, 463 MiB peak | — | Logins are a burst at the door, not steady state. Throughput is **flat from 4-way to 200-way**, so slots past `GOMAXPROCS` are memory held hostage. |
| **Ingestion** | POST /runs at the cap | 87 ms p50 / 99 ms p99 | ~11 req/s per in-flight slot; 20 concurrent = 149.6 MiB | **[DERIVED]** ~660 players at 1 run/min | 70% of the request is JSON-parsing the body **twice**. |
| **Replay** | realistic 60 s run (483 ev) | 76 ms p99, **5.8 runs/s per goroutine** | `REPLAY_CONCURRENCY` 4 → **~23 runs/s** | **~1 400 players** at 1 run/min | The steady-state bottleneck, and the first thing to saturate. |
| **Replay** | worst legal run (code_css 10k) | ~8.1 s against a 45 s timeout | 5.6× margin | — | One such run occupies a quarter of the worker pool for 8 s. |
| **Projection** | per verdict, typical player | +2.75 ms | negligible next to 173 ms of replay | — | 1.5% of a verdict. Was 12% before the zone 4 fix. |
| **Boards** | page 1 **and** any deep page | **2–3 ms p99** | thousands/s; not a constraint | — | Depth-independent since `sort_key` (zone 3). A page is one index range scan. |
| **Boards** | `/me` rank at 100 k entries | **120 ms p99** | ~8/s per connection | — | **O(rank).** The one read that scales with board size. |
| **Boards** | catalogue, 499 buckets / 427 k entries | 133 ms p99 | scales with **entries**, not buckets | — | ~1.2 s at 4 M entries **[DERIVED]**. |
| **Replay watch** | public replay, 20 concurrent | 149 ms p50, 80.6 MiB | rate-limited to 30/IP | — | Passthrough gzip; the database is 0.1 ms of it. |
| **Relay** | 50 rooms × 5 seats | p99 **13.56 ms**, 9 993 peer_batch/s | **833 relays/s per core** | — | 27% of its 50 ms budget. |
| **Relay** | the crossing point | p99 **47.75–54.13 ms** | **≈200 rooms / 1 000 clients / ~40 000 relays/s** | **1 000 players in matches** | The number everything multi-instance is measured against. |
| **Match end** | 20 simultaneous | 101 ms, unrelated p99 4.3× worse for ~100 ms | pool of 10 connections | — | The burst is **queueing, not work**: `SaveMatch` is 8 ms alone, 39 ms at 20-way. |
| **Rebuild** | 948 600 runs → 427 099 cells | **7–11 min**, and it is **downtime** | offline only | — | `TRUNCATE` holds `ACCESS EXCLUSIVE` for the whole run. |

## What saturates first

**The replay worker, at roughly 1 400 concurrent players.** [DERIVED] 5.8 runs/s
per goroutine × `REPLAY_CONCURRENCY` 4 = 23 runs/s; at one run per player per
minute that is 23 × 60 = 1 380 players. Nothing else is close:

1. **Replay — ~1 400 players.** CPU-bound in goja, and the only knob is
   `REPLAY_CONCURRENCY`, which competes with the argon2id gate for the same
   memory budget. A player doing 15 s runs instead of 60 s ones counts as four,
   so **~350 sprinters** saturate the same pool.
2. **Relay — ~1 000 players in matches.** Measured, not derived, and the two
   ceilings are close enough that a box hosting both hits them together. The
   1 000 clients shared the box with the server, so server-only capacity is
   higher; 1 000 is the honest figure for one box running both.
3. **Ingestion — ~660 players [DERIVED]**, but only against the *body cap*: a
   6.5 MiB submission is a 10 000-word run, and nobody plays those back to back.
   At realistic sizes ingestion is well clear of replay.
4. **`/me` at 120 ms** is the first *read* to hurt, and it hurts per request
   rather than per player.

**What does not saturate:** board pages (depth-independent, 2–3 ms), the
dictionary catalogue (43 kB, served from memory), replay watching (rate-limited
per IP), and projection (1.5% of a verdict).

## The one hard threshold

**A second instance is needed at ≈200 rooms / 1 000 concurrent match clients /
~40 000 relays per second.** That is where relay p99 crosses its 50 ms budget —
measured twice, 47.75 ms and 54.13 ms, straddling it, so 200 is the crossing and
not a point safely under it. Delivery stayed lossless at every point up to and
past it; what degrades is latency, not correctness.

That number is also the **Redis threshold**, because the two are the same
question. Room state lives in one process's memory today, so a second instance
cannot serve the same room — which makes "add an instance" and "move room state
to Redis" one decision, taken at one number. Below it, a second instance buys
nothing that a bigger box does not; above it, no box helps, because the limit is
scheduler and CPU on fan-out rather than any single lock (the room mutex is
**0.067%** of machine capacity, measured two ways).

## Reading this before an incident

- **A slow board is not a capacity problem.** Pages are 2–3 ms at any depth. If
  a board is slow, look at `/me` (O(rank)) or the catalogue (O(entries)).
- **A backing-up replay queue is the expected first symptom of load.** It is the
  lowest ceiling and it degrades as latency-to-verdict, not as errors: runs
  still land, they just take longer to be judged.
- **Match-end bursts are self-limiting and brief** — ~100 ms of 4× latency on
  unrelated queries, caused by 20 goroutines wanting 10 connections.
- **Never run `rebuild-leaderboards` on a live board.** It is 7–11 minutes of
  `GET /api/v1/leaderboards/*` returning nothing.

## Related

- [`PERFORMANCE.md`](PERFORMANCE.md) — every number above, with its method
- [`LEADERBOARDS.md`](LEADERBOARDS.md) — why the rebuild is per-cell
- [`PROTOCOL.md`](PROTOCOL.md) — the 100 ms flush obligation the relay load models
- [`RUNS.md`](RUNS.md) — the ingestion caps
