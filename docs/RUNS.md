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
| GET  | `/api/v1/runs/{id}/replay` | **none** | One **accepted** run's playback metadata — setup, seed, dictHash, server numbers, grade, display name |
| GET  | `/api/v1/runs/{id}/replay/log` | **none** | The same run's EventLog, as the stored gzip bytes (`Content-Encoding: gzip`) |

The last two are the spectator surface behind a leaderboard row and the only
public routes here: they serve accepted runs whose owner is not banned, and
answer an indistinguishable `404` for everything else (pending, flagged,
rejected, banned owner, nonexistent). They carry the verdict's result but never
its reasoning — no `validation`, no client-reported numbers. Both draw on one
per-IP token bucket, because an event log is the heaviest payload the server
serves and neither route needs a session. Full shape and access matrix:
[`LEADERBOARDS.md`](LEADERBOARDS.md).

The owner-only `?log=1` path is unchanged and remains the only way to reach a
run that is pending, flagged or rejected.

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
  "restartsSinceLastSubmit": 3,
  "setup":         { "...": "full CoreConfig+GenerationConfig snapshot" },
  "clientMetrics": { "wpm": 80, "raw": 85, "acc": 0.97 },
  "clientScore":   { "version": 2, "total": 1234 },
  "log": { "version": 1, "events": [ { "kind": "insert", "seq": 1, "t": 12, "text": "t" } ] }
}
```

Exactly one of `durationMs` / `wordCount` is set (time vs. word modes) — unless
the run is a **quote run**, which carries neither; see *Text source* below. The
response is `202 { "id": "<uuid>", "status": "pending" }`.

### `restartsSinceLastSubmit`

How many tests the player started and abandoned since their **previous
submission**. A restarted run is never submitted — it has no log, no score, no
row — so without this count the profile's "tests started" could only equal
"tests completed", which is a lie for anyone who restarts. The client counts
restarts locally and reports the number on the NEXT run it submits; profile
aggregation reads `tests_started = count(completed) + sum(restarts)`.

Optional (a client that predates the field submits none, meaning 0), integer in
`[0, 10 000]` — out of range is `422 invalid_restarts`, enforced twice
(validator + the `runs_restarts_range` CHECK, migration `00013`). The count is
client-reported and **unverifiable by design**: a restarted run leaves nothing
to replay, so — like the declared view-only mods — it is accepted on trust. It
feeds profile statistics only; nothing ranked reads it. Every row written
before the column exists carries `0`, the only claim history can make.

### Text source: seeded vs. quote

A run's text either comes from its **seed** — regenerated from
`seed` + dictionary, effectively infinite — or from a **published quote**, a
fixed text everyone types the same bytes of ([`QUOTES.md`](QUOTES.md)). The
snapshot says which, at `setup.generation.textSource`:

```json
"textSource": {
  "kind": "quote",
  "quoteId": "3f2a1b0c-9d8e-4c7b-8a6f-5e4d3c2b1a09",
  "quoteHash": "e42437c7"
}
```

| Field | Rule |
|---|---|
| `kind` | `"seeded"` or `"quote"`. **Absent means `seeded`.** Anything else is `422 invalid_text_source`. |
| `quoteId` | Required for `quote`, and must parse as a UUID. It is the *only* handle that resolves the text. `422 invalid_text_source` otherwise. |
| `quoteHash` | Required for `quote`, ≤ 64 chars. It is `dictVersion([text])` — the same value the registry stores as `quotes.text_hash`. |
| `text` | **Must be absent.** A payload that carries one is `422 quote_text_submitted`. |

**The text is never submitted.** The client sends the reference; the server
re-reads the bytes from the registry by id and replays against those. This is
not a size optimisation — it is what makes a quote score mean anything. If the
client's copy of the text were what the run was judged against, "type this
quote" would be "type whatever you say the quote was", and two scores on the
same `quoteId` would not be comparable.

Which is also why a submitted `text` is **refused** rather than ignored.
Ignoring it would leave two plausible readings of the contract, and the wrong
one is the one that makes the whole feature meaningless. A client that sends it
is either confused or trying something; both deserve an answer, not silence.
`buildRunPayload` on the frontend drops the field at the wire boundary by
construction, so this rule only ever fires on a hand-rolled payload.

An **absent** `textSource` is `seeded`, and that is load-bearing: it is what let
quotes land without bumping `EVENT_LOG_VERSION`, and it is what every row
written before quotes existed carries. `{"kind":"seeded"}` and no `textSource`
at all mean exactly the same thing everywhere — validator, `run_text_source_kind()`
in SQL, and the replay worker.

#### Dimensions are conditional on the text source

| Text source | Rule |
|---|---|
| `seeded` | **Exactly one** of `durationMs` (1…3 600 000) / `wordCount` (1…10 000) |
| `quote` | **Neither** |

A quote run has no dimension because neither number would mean anything. Its
length is a property of the **text**, not of the session: the player did not
choose 25 words or 30 seconds, they chose a quote, and the same quote is the
same length for everyone who types it. A `wordCount` here would be a second,
unverified copy of something `quotes.length` already knows; a `durationMs` would
describe a deadline the run does not have.

The seeded rule is **unchanged** — still a strict XOR, both-or-neither still
`422 invalid_dimensions`. Relaxing the constraint for one shape is not a reason
to stop enforcing it for the others, and seeded runs are the overwhelming
majority. Both halves are enforced twice: in the validator, and by the
`runs_one_dimension` CHECK (migration `00008`), which switches on
`run_text_source_kind(setup)`.

#### `dictHash` on a quote run

It is `dictVersion([text])` — i.e. the same 8-hex value as `quoteHash`, because
the core derives a quote run's `SeedContext.dictVersion` from the text rather
than from a word list. Two consequences worth stating:

- **It resolves to no published dictionary,** and must not be looked up in one.
  The replay worker branches on the text source *before* it reaches the
  dictionary registry; a shared lookup would flag every quote run `unknown_dict`
  before it ever reached the quote resolver.
- **The server does not trust it.** Everything a quote run is judged against is
  re-derived from `quoteId`. A client that got `dictHash` wrong cannot break a
  run whose text the server already resolved.

### List response

```json
{
  "runs": [ { "id": "...", "mode": "time", "durationMs": 15000, "lang": "en",
              "seed": 2864901, "dictHash": "a1b2c3d4",
              "setup": {…}, "clientMetrics": {…}, "clientScore": {…},
              "scoreVersion": 1, "status": "pending", "logBytes": 512,
              "restartsSinceLastSubmit": 0,
              "createdAt": "2026-07-23T21:00:00Z" } ],
  "nextCursor": "<opaque>"
}
```

`nextCursor` is an opaque keyset token over `(created_at, id)`; pass it back as
`?cursor=`. It is absent on the last page. `limit` defaults to 20, clamped to
100. The log payload is never included in summaries.

Summaries additionally carry the profile table's **derived cells** — `grade`,
`consistency`, `chars` (absent until judged), `quoteId` (quote runs only) and
`mods` (always) — lifted in SQL from the documents the row already stores, so
the profile page renders without parsing them. Additive; see
[`PROFILE.md`](PROFILE.md).

## Structural validation (this phase only)

Validation is **fast and game-agnostic** — it never replays the log, recomputes
anything, or consults a dictionary. Malformed JSON is `400 bad_request`; an
oversized body is `413`; a well-formed body that breaks a structural rule is
`422` with a distinct code.

| Check | Limit / rule | Failure |
|---|---|---|
| Raw body size | ≤ 25 MiB (transport; sized for log v2) | `413 payload_too_large` |
| `scoreVersion` | one of `KnownScoreVersions` = `{1, 2}` | `422 unsupported_score_version` |
| `log.version` | one of `KnownLogVersions` = `{1, 2}` | `422 unsupported_log_version` |
| v1 log bytes | ≤ 6.5 MiB (the pre-telemetry envelope, post-decode) | `422 log_too_large` |
| Event count | 1 … 120 000 (v1) / 1 … 480 000 (v2) | `422 empty_log` / `422 too_many_events` |
| Telemetry grammar | `down`/`up` only in v2; `code` is 1–32 chars of `[A-Za-z0-9]` | `422 malformed_log` |
| `seq` | strictly increasing, non-negative start (cheap linear scan) | `422 non_monotonic_seq` |
| `seed` | integer in `[0, 2³²−1]` (mulberry32) | `422 seed_out_of_range` |
| `textSource` | `kind` ∈ {`seeded`, `quote`}; a `quote` needs a UUID `quoteId` and a `quoteHash` | `422 invalid_text_source` |
| `textSource.text` | must be absent | `422 quote_text_submitted` |
| Dimensions | seeded: exactly one of `durationMs` (1…3 600 000) / `wordCount` (1…10 000). quote: neither | `422 invalid_dimensions` |
| `restartsSinceLastSubmit` | optional; integer in `[0, 10 000]` when present | `422 invalid_restarts` |
| `mode` / `lang` / `dictHash` | present, ≤ 32/32/64 chars | `400 bad_request` |
| `setup` / `clientMetrics` / `clientScore` / `log` | present | `400 bad_request` |

`scoreVersion` is checked against `KnownScoreVersions` (the exported allow-list
in `internal/runs`, currently `{1, 2}`) — the single source of truth shared by
this rule and the validator, so widening it is a one-line change. The current
client ships score formula **v2**; **v1** stays accepted for older builds.

`textSource` is the **only** part of the opaque `setup` snapshot this layer
parses, and it parses no further than it must: the kind decides which dimension
rule applies, and the dimension columns are indexed columns this layer owns.
Whether the quote *exists* is not asked here — ingestion never consults a
registry, exactly as it never consults the dictionary one. The worker resolves
the id and flags `unknown_quote` if it cannot.

The gzip log round-trips **byte-for-byte**: `log` is captured as raw bytes,
gzip-compressed (stdlib, best-speed), and stored; `log_bytes` records the
uncompressed size. `GET …?log=1` returns exactly the bytes that were sent.

### Caps: why 6.5 MiB and 120 000 (and 25 MiB and 480 000), and what they cost

The pair used to be **2 MiB / 50 000 events**, and the two could not both be
obeyed. 50 000 events marshal to ~2.5 MiB, so `MaxBytesReader` bounded every
submission at **39 915** events and the documented event cap was a phantom
nothing could ever reach. Worse, the documented game was not playable at all:

| Full `MaxWordCount` (10 000-word) run | Events | Body |
|---|---|---|
| german, plain | 79 116 | 3.87 MiB |
| german, punctuation | 81 430 | 3.99 MiB |
| russian_empire, punctuation | 80 852 | 4.02 MiB |
| **code_css, punctuation** (worst of the nine) | **108 274** | **5.35 MiB** |

Measured against the re-vendored bundle, real `generateWords` output, one insert
per grapheme plus one commit per word. Every one of those exceeded both old
caps. `MaxWordCount = 10 000` was a contract ingestion refused.

So the caps were **raised, not lowered**. Lowering the event cap to ~40 000 to
match reality would have been the cheaper edit, but it would silently delete a
mode the docs promise — "the server shrank the game to fit its own interpreter"
is precisely the failure [`PERFORMANCE.md`](PERFORMANCE.md) zone 2 already calls
out about the replay timeout. The old numbers were sized around an interpreter
cost that no longer exists: pre-fix, 79 394 events would have cost roughly three
minutes in goja and been indefensible; post-fix (zone 2's 21.6× re-measure) the
same fixture is seconds.

Two rules to keep in mind before tuning either number again:

- **The two caps are coupled, so headroom must be given to both.** A text mod
  lengthens *tokens*, which raises the event count as well as the byte count —
  punctuation alone adds 2 314 events to a german run. An earlier draft of this
  change gave +16 % to the body cap and +0.8 % to the event cap, which is not
  headroom; it is one cap moving. (`numbers` is not the risk: a digit run
  *replaces* a word and shortens the text.)
- **The caps are ordered on purpose.** The **event cap is operative** — it is
  what a well-formed log runs into, and it bounds the replay worker's cost,
  which scales with event count. The **body cap sits above it** and no longer
  bounds a well-formed log at all; it exists to catch a payload that is fat for
  another reason (a paste, an IME commit: one insert carrying many graphemes).
  A body cap that bounds a well-formed log is doing the event cap's job badly.
  `TestMaxLegalPayloadSitsAtTheCaps` pins that ordering, and
  `TestEveryPublishedDictionaryCanPlayAFullLengthRun` pins that a 10 000-word
  run fits on **every** published dictionary — the check whose absence let the
  caps be sized against german and quietly exclude code_css.

**The cost, unburied.** 6.5 MiB is 3.25× the old ingest envelope, and zone 5
measured the body being JSON-parsed **twice** on this path. With
`REPLAY_CONCURRENCY` at 4 that is a worst case around 4 × 2 × 6.5 MiB ≈ **52 MiB
of request bodies alone**, competing for the same budget as the argon2id gate,
which is itself sized as a memory ceiling (zone 1). This is now by a wide margin
the largest single allocation on the ingest path, and it is the next thing to
measure — not a raise that came for free.

### Log v2 (keystroke telemetry): the re-derived caps

A v2 capture brackets every physical keystroke with its `down`/`up` pair
(`docs` in the frontend: game-architecture.md, "Event protocol"), so a v2 log
is **exactly 3× its state events** plus one Shift pair per shifted grapheme in
the no-hold worst case. Re-measured by the same generator-driven guard test
that sized the v1 caps
(`TestEveryPublishedDictionaryCanPlayAFullLengthRunUnderLogV2`), over every
published dictionary, both punctuation arms:

| Worst full-length v2 run | Total events | Modeled log bytes |
|---|---|---|
| **code_abap_1k** (worst of all published) | **417 710** | **~21.5 MiB** |

Hence **480 000 events** for a v2 log (~15 % headroom, the discipline that
sized 120 000) and a **25 MiB transport cap** (the version is unknowable before
the body is parsed, so the transport must admit the largest grammar). Two
things deliberately did NOT widen:

- a **v1 log** is bounded post-decode at the old envelope (`log_too_large`
  above): raising the transport cap for v2 must not quietly hand v1 clients a
  4× bigger log;
- the **event cap stays operative** for both grammars — the guard test asserts
  a body at either event cap fits under its size bound, and
  `TestLoadReplayMaxRunV2Telemetry` re-checked the replay-timeout margin with a
  real interleaved maximum run: worst sample 9.8 s against the 45 s interrupt
  budget (22 %), median 9.5 s against the 22.5 s engineering margin (42 %);
  scaled to the worst dictionary's 417 710 events that is ~17 s — inside both.

Watch item: the **v1** event cap is nearly exhausted — league_of_legends now
measures 118 943 events (99 % of 120 000). The next published dictionary that
crosses it will fail the guard test, which is that test doing its job.

**Storage note.** Telemetry is anticheat/analytics **input**: this phase only
collects and structurally validates it (the `unpaired-keyup` flag is
bookkeeping at weight 0.00). The keyboard-portrait projection and the
hold/overlap plausibility heuristics are LATER phases consuming this data —
nothing reads `code` semantically today, and the gzip log round-trips
byte-for-byte exactly as before.

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
  duration_ms   int  NULL │ seeded: exactly one non-null; quote: both null
  word_count    int  NULL │ (runs_one_dimension CHECK, conditional since 00008)
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
  restarts_since_last_submit int NOT NULL DEFAULT 0  CHECK 0 … 10 000  -- see above
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
| `validation` | `{verdict, reason?, flags[], policy{}, divergence?}` |
| `validatedAt` | When the verdict was written |

`clientMetrics` / `clientScore` are never overwritten: the pair is the evidence
a mismatch is judged on. The pipeline, the decision table, the review policy and
the queue design are in [`REPLAY.md`](REPLAY.md).

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
    "policy": { "version": 1, "suspicion": 0, "threshold": 1 },
    "divergence": { "field": "total", "client": 5640, "server": 1410 }
  },
  "validatedAt": "2026-07-24T21:00:03Z"
}
```

An **accepted** run can still carry `flags` and a non-zero
`policy.suspicion` — a weak plausibility signal is recorded, not acted on
(REPLAY.md, "Review policy"). Treat `status` as the verdict and everything
under `validation` as moderation detail: the client should render the former
and ignore the latter.

An accepted run of a **ranked shape** also lands on its leaderboard, in the same
transaction that wrote the verdict ([`LEADERBOARDS.md`](LEADERBOARDS.md)).
Nothing about that is visible on the run summary: the board is a projection, and
`runs` stays the source of truth.

A **quote run** is judged the same way, with one extra step in front of it: the
worker resolves `textSource.quoteId` against the quote registry (superseded
revisions included), checks the stored `text_hash` against the run's
`quoteHash`, and hands the resolved bytes to the core. The client's copy of the
text is never involved — it was refused at ingestion, and the resolution happens
once per run rather than once per `generateWords` call. An unknown id, or a hash
that disagrees, is `flagged` with reason `unknown_quote` — **never** `rejected`,
for the same reason `unknown_dict` is not: rejection is for a run we can prove
is bad, and this is a run we cannot currently judge. The corpus is the likelier
thing to have moved.

## Deliberately deferred

Still out of scope for both ingestion and the worker:

- **Anti-cheat beyond the core's own plausibility flags** — cross-run
  heuristics, fingerprint correlation, shadow-ban (BACKEND.md §11).
- **The admin review queue** over `flagged` runs, and the handle that issues a
  ban (the `bans` table and its read-side filter already exist).
- **TP / profile rating** — SCORING_CONCEPT §5, its own phase.
- **Rejecting an unknown `dictHash` at ingestion.** Ingestion still treats it as
  an opaque string; the worker resolves it against the registry
  ([`DICTIONARIES.md`](DICTIONARIES.md)) and flags `unknown_dict`.
- **Rejecting an unknown `quoteId` at ingestion**, for exactly the same reason:
  ingestion checks that the reference is *usable*, never that it *resolves*. The
  worker flags `unknown_quote` ([`QUOTES.md`](QUOTES.md)).

The `(status, created_at) WHERE status='pending'` index is the worker's queue
scan; the stored `setup` snapshot is what goja replays against.
