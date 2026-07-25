# TypeMore Run Ingestion

Phase 3 of the server: accepting finished typing tests. A client that finishes a
run POSTs its **event log** plus the numbers it computed locally; the server
validates the payload **structurally only**, stores the log as an immutable gzip
blob with status `pending`, and answers immediately. Nothing here is
authoritative — the client keeps showing its own preview result until the
**replay worker** recomputes it and sets the real status
([`REPLAY.md`](REPLAY.md)). See BACKEND.md §3–4 and
[`ARCHITECTURE.md`](../ARCHITECTURE.md) §4.2.

Guests play entirely client-side; ingestion requires a session (auth phase).

## Endpoints

All under `/api/v1`. Like the auth surface, POST requires an `Origin` header
equal to `TYPEMORE_FRONTEND_ORIGIN` (CSRF) and a valid session cookie. Errors are
`{"error":"<code>","message":"..."}`.

| Method | Path | Auth | Purpose |
|---|---|---|---|
| POST | `/api/v1/runs` | session | Ingest a finished run → `202 {id, status:"pending"}` |
| GET  | `/api/v1/runs?cursor=&limit=` | session | List own runs, newest-first, keyset-paginated (no log) |
| GET  | `/api/v1/runs/{id}` | session | One own run's summary (no log) |
| GET  | `/api/v1/runs/{id}?log=1` | session | Stream the gunzipped EventLog JSON (for the replay feature) |

### POST body

The bucket fields are lifted top-level to populate indexed columns; `setup`,
`log`, `clientMetrics`, and `clientScore` are opaque JSON stored verbatim.

```json
{
  "mode": "time",
  "durationMs": 15000,
  "lang": "en",
  "seed": 2864901,
  "dictHash": "a1b2c3d4",
  "scoreVersion": 2,
  "setup":         { "...": "full CoreConfig+GenerationConfig snapshot" },
  "clientMetrics": { "wpm": 80, "raw": 85, "acc": 0.97 },
  "clientScore":   { "version": 2, "total": 1234 },
  "log": { "version": 1, "events": [ { "kind": "insert", "seq": 1, "t": 12, "text": "t" } ] }
}
```

Exactly one of `durationMs` / `wordCount` is set (time vs. word modes). The
response is `202 { "id": "<uuid>", "status": "pending" }`.

### List response

```json
{
  "runs": [ { "id": "...", "mode": "time", "durationMs": 15000, "lang": "en",
              "seed": 2864901, "dictHash": "a1b2c3d4",
              "setup": {…}, "clientMetrics": {…}, "clientScore": {…},
              "scoreVersion": 1, "status": "pending", "logBytes": 512,
              "createdAt": "2026-07-23T21:00:00Z" } ],
  "nextCursor": "<opaque>"
}
```

`nextCursor` is an opaque keyset token over `(created_at, id)`; pass it back as
`?cursor=`. It is absent on the last page. `limit` defaults to 20, clamped to
100. The log payload is never included in summaries.

## Structural validation (this phase only)

Validation is **fast and game-agnostic** — it never replays the log, recomputes
anything, or consults a dictionary. Malformed JSON is `400 bad_request`; an
oversized body is `413`; a well-formed body that breaks a structural rule is
`422` with a distinct code.

| Check | Limit / rule | Failure |
|---|---|---|
| Raw body size | ≤ 2 MB | `413 payload_too_large` |
| `scoreVersion` | one of `KnownScoreVersions` = `{1, 2}` | `422 unsupported_score_version` |
| `log.version` | must equal `1` | `422 unsupported_log_version` |
| Event count | 1 … 50 000 | `422 empty_log` / `422 too_many_events` |
| `seq` | strictly increasing, non-negative start (cheap linear scan) | `422 non_monotonic_seq` |
| `seed` | integer in `[0, 2³²−1]` (mulberry32) | `422 seed_out_of_range` |
| Dimensions | exactly one of `durationMs` (1…3 600 000) / `wordCount` (1…10 000) | `422 invalid_dimensions` |
| `mode` / `lang` / `dictHash` | present, ≤ 32/32/64 chars | `400 bad_request` |
| `setup` / `clientMetrics` / `clientScore` / `log` | present | `400 bad_request` |

`scoreVersion` is checked against `KnownScoreVersions` (the exported allow-list
in `internal/runs`, currently `{1, 2}`) — the single source of truth shared by
this rule and the validator, so widening it is a one-line change. The current
client ships score formula **v2**; **v1** stays accepted for older builds.

The gzip log round-trips **byte-for-byte**: `log` is captured as raw bytes,
gzip-compressed (stdlib, best-speed), and stored; `log_bytes` records the
uncompressed size. `GET …?log=1` returns exactly the bytes that were sent.

### Note on `seq`: structural vs. deep

The server checks only that seq is **strictly increasing from a non-negative
start** — enough to reject duplicates and reordering cheaply. The frontend core
emits seq **contiguous from 1** (`events.ts` / `validate.ts`), and the full
"no gaps, monotonic time, reduce-legal" check is part of the replay worker's
deep validation, not this phase. The structural rule accepts every real client
log while staying game-agnostic.

## Rate limiting

`POST /runs` is rate-limited **per user** with a generous token bucket (a typing
session is many runs): default burst 120, one token refilled every 30 s
(≈ 120 runs/hour sustained), tunable via `TYPEMORE_RUNS_RATE_BURST` /
`TYPEMORE_RUNS_RATE_EVERY`. It reuses the auth limiter machinery behind the
`RateLimiter` interface; swapping to Redis later changes only the composition
root. Listing/detail are not rate-limited.

## Schema

```
runs
  id            uuid pk
  user_id       uuid fk→users ON DELETE CASCADE
  mode          text
  duration_ms   int  NULL │ exactly one non-null (runs_one_dimension CHECK)
  word_count    int  NULL │
  lang          text
  seed          bigint         CHECK 0 … 2³²−1
  dict_hash     text
  setup         jsonb          -- CoreConfig+GenerationConfig snapshot (opaque)
  client_metrics jsonb         -- reported wpm/raw/acc (display-only)
  client_score  jsonb          -- reported ScoreResult (display-only)
  score_version smallint
  status        text DEFAULT 'pending'  CHECK ∈ {pending,accepted,flagged,rejected}
  log           bytea          -- gzip of the raw EventLog JSON (immutable)
  log_bytes     int            -- uncompressed size
  created_at    timestamptz
  idx(user_id, created_at DESC)                    -- own-runs feed
  idx(status, created_at) WHERE status='pending'   -- future worker queue scan
```

Everything cascades from `users`, so account deletion removes a user's runs.

## Environment

| Variable | Default | Meaning |
|---|---|---|
| `TYPEMORE_RUNS_RATE_EVERY` | `30s` | Per-user token-bucket refill interval |
| `TYPEMORE_RUNS_RATE_BURST` | `120` | Per-user token-bucket size |

## The verdict (replay worker)

A landed run is `pending` until the worker replays it. Once judged, the summary
endpoints carry the server's own numbers alongside the client's — all four
fields are **absent** until then, so "not replayed yet" is distinguishable from
"replayed and empty":

| Field | Meaning |
|---|---|
| `status` | `pending` → `accepted` / `flagged` / `rejected` |
| `serverMetrics` | The core's recomputed `Metrics` |
| `serverScore` | The core's recomputed `ScoreResult` |
| `validation` | `{verdict, reason?, flags[], divergence?}` |
| `validatedAt` | When the verdict was written |

`clientMetrics` / `clientScore` are never overwritten: the pair is the evidence
a mismatch is judged on. The pipeline, the decision table, and the queue design
are in [`REPLAY.md`](REPLAY.md).

```json
{
  "id": "...", "status": "flagged",
  "clientScore": { "version": 2, "total": 5640, "...": "..." },
  "serverScore": { "version": 2, "total": 1410, "...": "..." },
  "serverMetrics": { "wpm": 113.07, "raw": 113.07, "accuracy": 1, "...": "..." },
  "validation": {
    "verdict": "valid",
    "reason": "score_mismatch",
    "flags": [],
    "divergence": { "field": "total", "client": 5640, "server": 1410 }
  },
  "validatedAt": "2026-07-24T21:00:03Z"
}
```

## Deliberately deferred

Still out of scope for both ingestion and the worker:

- **Anti-cheat beyond the core's own plausibility flags** — cross-run
  heuristics, fingerprint correlation, shadow-ban (BACKEND.md §11).
- **The admin review queue** over `flagged` runs.
- **Leaderboards / TP**: an `accepted` run does not yet update any read model
  (SCORING_CONCEPT §4–5).
- **Rejecting an unknown `dictHash` at ingestion.** Ingestion still treats it as
  an opaque string; the worker resolves it against the registry
  ([`DICTIONARIES.md`](DICTIONARIES.md)) and flags `unknown_dict`.

The `(status, created_at) WHERE status='pending'` index is the worker's queue
scan; the stored `setup` snapshot is what goja replays against.
