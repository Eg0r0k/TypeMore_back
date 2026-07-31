# TypeMore Profile

The player's own statistics surface: summary counters, the activity calendar,
the wpm histogram, the daily time/speed series with its trend stat, and the
personal-best cards. Served by `internal/profile`; consumed by the frontend's
`/profile` page.

Two decisions define the phase:

- **Everything is on-demand SQL over `runs`.** Nothing is projected, nothing is
  cached — the profile is always exactly what the runs table says right now.
  Every query is scoped to one user and pinned to the per-user indexes by plan
  assertions (`docs/PERFORMANCE.md`, profile zone); the day a measured load
  says otherwise, the first move is a covering index, not a cache.
- **Session-scoped for the owner; a separate, explicitly-gated public surface
  for everyone else.** Every `/api/v1/profile/*` route requires a session and
  answers about the caller, exactly as in v1. Public profiles arrived as the
  deliberate flag v1 promised: `GET /api/v1/users/{name}/…` (see "Public
  profiles" below), gated by two per-account switches enforced **on the
  server**. The keyboard heatmap kept its promise too: **private-by-default
  with its own opt-in** — per-key timing aggregates are effectively biometric
  (see Privacy).

## Endpoints

All under `/api/v1/profile`, all requiring a session cookie; anonymous callers
get `401`. Errors are `{"error":"<code>","message":"..."}`.

| Method | Path | Purpose |
|---|---|---|
| GET | `/api/v1/profile/summary` | Identity, counters, metric groups, streaks, language counts |
| GET | `/api/v1/profile/activity?days=366` | Daily `{date, tests, timeMs}` buckets for the calendar |
| GET | `/api/v1/profile/histogram` | Accepted-test counts per 10-wpm bucket |
| GET | `/api/v1/profile/timeseries?from=&to=` | Per-day `{timeTypingMs, avgWpm, avgAcc}` + the trend stat |
| GET | `/api/v1/profile/pbs` | Best leaderboard entry per bucket the caller holds |
| GET | `/api/v1/profile/keyboard` | The keyboard heatmap's per-key aggregates |

`GET /api/v1/layouts` (public, cacheable) serves the layouts asset itself —
geometry, fingers, and the symbol lists — for the heatmap's drawing.

### Aggregation rules (stated once, asserted by tests)

- **Metric aggregates** (wpm / raw / acc / consistency, estimated words, the
  histogram, the timeseries averages, the trend stat) read **accepted runs
  only** — the server-verified numbers in `server_metrics`. Acc and consistency
  travel as `[0, 1]` fractions, formatted as % at the display edge.
- **Counters** read every submitted run regardless of verdict (a flagged run
  was still played), and `testsStarted` additionally folds in the
  client-reported restart counts:
  `testsStarted = count(runs) + Σ restartsSinceLastSubmit`
  ([`RUNS.md`](RUNS.md), `restartsSinceLastSubmit`).
- **Day buckets are UTC** everywhere (activity, timeseries, streaks).

### `GET /profile/summary`

```json
{
  "displayName": "boardsmoke", "joined": "2026-07-20T10:00:00Z",
  "testsStarted": 7, "testsCompleted": 2, "restartsPerCompleted": 2.5,
  "timeTypingMs": 33000, "estimatedWordsTyped": 96,
  "wpm":         { "highest": 113.07, "average": 108.4, "averageLast10": 108.4 },
  "raw":         { "highest": 118.2,  "average": 112.1, "averageLast10": 112.1 },
  "acc":         { "highest": 1,      "average": 0.967, "averageLast10": 0.967 },
  "consistency": { "highest": 0.83,   "average": 0.79,  "averageLast10": 0.79 },
  "streak": { "current": 1, "best": 4 },
  "languages": [ { "lang": "german", "tests": 2 } ]
}
```

- `averageLast10` is the mean over the **last 10 accepted runs by date** — the
  index walks newest-first and stops after ten, no sort.
- `timeTypingMs` is `Σ server_metrics.durationSec × 1000` over accepted runs:
  the deadline-pinned duration the core itself measured. Unjudged/flagged runs
  contribute nothing — the profile only counts time the server verified.
- `estimatedWordsTyped` is the raw character volume over the conventional five
  characters per word: `Σ (correct + incorrect + extra + spaces) / 5`,
  accepted runs, rounded once at the end.
- `restartsPerCompleted` is `Σ restarts / testsCompleted`, `0` on a fresh
  account rather than a division error.
- **Streaks** are consecutive **days played** (a day counts iff at least one
  run was submitted that UTC day — logins are not tracked, by design).
  `current` is the length of the island whose last day is **today or
  yesterday** (yesterday keeps a streak alive until the end of today); `best`
  is the longest island ever. Gaps-and-islands in one SQL query.
- `languages` counts every submitted run per dictionary language, most-played
  first.

### `GET /profile/activity?days=N`

Daily buckets of the last `N` UTC days (default and cap **366** — a year plus
the leap-day column), today included. Only populated days are returned; the
calendar renders its own gaps. `tests` counts every submitted run; `timeMs`
sums the verified durations (accepted runs).

```json
{ "days": [ { "date": "2026-07-28", "tests": 2, "timeMs": 33000 } ] }
```

### `GET /profile/histogram`

Accepted-run counts per 10-wpm bucket of the **server's** wpm; `wpm` is the
bucket's lower bound (60 covers [60, 70)); empty buckets are absent.

```json
{ "buckets": [ { "wpm": 100, "tests": 1 }, { "wpm": 110, "tests": 1 } ] }
```

### `GET /profile/timeseries?from=&to=`

Per-day chart series inside `[from, to)`. Bounds are RFC3339 instants or plain
`YYYY-MM-DD` dates (UTC midnight); a date-only `to` is taken **inclusively** —
its whole day is inside the window, because that is what every range picker
means by an end date. `from` defaults to the beginning of time, `to` to now.

```json
{
  "days": [ { "date": "2026-07-28", "timeTypingMs": 33000,
              "avgWpm": 108.4, "avgAcc": 0.967 } ],
  "wpmPerHour": 3.2
}
```

**`wpmPerHour` — "speed change per hour spent typing" — the method.** For each
accepted run in the range, take `y` = that run's server wpm and `x` = the
cumulative hours of accepted typing inside the range measured at that run (its
own duration included; a running sum in index order, so no sort). The stat is
the ordinary least-squares slope of `y` over `x` — Postgres `regr_slope` —
i.e. "over this range, each hour of typing moved my speed by X wpm". Fewer
than two points, or zero variance in `x`, reads `0`: no trend yet. Computed
server-side so every client shows the same number.

### `GET /profile/pbs`

The caller's row from `leaderboard_entries` for every bucket they hold — the
entries table already **is** the per-bucket personal-best store (one row per
player per bucket, the projection keeps it their best eligible run), so the
cards cost **zero new computation**; `00014` adds the `user_id` index that
makes reading them one index scan. Rows are decorated through
`leaderboard.ParseBucketKey` (the key format's one parser) exactly the way the
board index decorates buckets — a language board's fields absent on a quote PB
and vice versa, `source` never absent on a quote PB:

```json
{ "pbs": [
  { "bucket": "time:15000:german:seeded", "mode": "time", "durationMs": 15000,
    "lang": "german", "textSource": "seeded",
    "runId": "…", "score": 1645, "wpm": 103.2, "raw": 103.2, "acc": 1,
    "grade": "SS", "mods": { "…": "…" }, "achievedAt": "…" },
  { "bucket": "quote:1f5f1f2c-…", "quoteId": "1f5f1f2c-…",
    "source": "Johann Wolfgang von Goethe", "runId": "…", "score": 1406,
    "wpm": 112.8, "raw": 112.8, "acc": 1, "grade": "SS", "mods": { "…": "…" },
    "achievedAt": "…" }
] }
```

Deliberately read from the raw entries table, not the ban-filtered
`leaderboard_rows`: these are the caller's own bests on a session-scoped
surface, and hiding a player's own history from them serves nobody.

### `GET /profile/keyboard`

The keyboard heatmap's data: per PHYSICAL key (`KeyboardEvent.code`
vocabulary), lifetime press count, error rate and mean inter-key interval.

```json
{ "layout": "qwerty",
  "keys": [ { "keyId": "KeyF", "count": 5321, "errorRate": 0.021,
              "avgIntervalMs": 152.4, "intervals": 4980 } ] }
```

- `layout` is the DEFAULT the UI should open on: the layout of the caller's
  most-played dictionary language (`qwerty` for a fresh account). The toggle
  is free — aggregates are keyed by physical key, so both layouts render the
  same portrait.
- `intervals` is the observation count behind `avgIntervalMs`, so the UI can
  refuse to color a key from three presses instead of faking confidence.
- The rows come from **`user_keyboard_profile`** (migration `00016`): per-user
  per-key aggregates maintained **incrementally inside the replay worker's
  verdict transaction**, from per-character observations the core extracts
  while the log is already parsed in goja (`charObservationsOf`,
  `shared/core/keyboard.ts`). Aggregates NEVER require replaying a log at
  request time — that is the projection's entire reason to exist.
- Characters map onto keys through the **layouts asset**
  (`internal/keyboard/layouts/` — data, not code, and the single source for
  every consumer; its README names the anticheat bigram heuristics as the
  next one). A run's language picks the layout (ru → ЙЦУКЕН, else qwerty);
  unmapped characters bucket to the reserved key `other`, never disappear.
- **Exactly-once accounting**: a run's contribution is added when its verdict
  lands accepted and the `runs.keyboard_projected` stamp is off, and reversed
  (floored at zero) when a stamped run is re-judged to anything else. So
  `make revalidate` — which already re-replays every stale run — IS the
  backfill mechanism: after a bundle change it walks all history, and the
  projection fills in exactly once per accepted run.
- **Upgrade path**: key ids are already `KeyboardEvent.code`s. When the
  projection starts consuming log-v2 telemetry, a v2 run's observations group
  by the codes the log carries and the char mapping stops being consulted for
  v2 runs; v1 runs keep the char basis forever. Nothing about the table moves.
- **Privacy**: own profile only, like everything here — and when public
  profiles arrive, the keyboard heatmap stays **private-by-default with its
  own opt-in**: per-key error and timing profiles are effectively biometric.

## The runs list, extended for the profile table

`GET /api/v1/runs` summaries carry derived cells so the profile's table
renders rows without parsing the setup/metrics documents — **additive**, every
pre-existing field untouched ([`RUNS.md`](RUNS.md)):

| Field | Present | Source |
|---|---|---|
| `grade` | judged runs | `run_grade` of the SERVER accuracy (the fenced SQL mirror of the core's `gradeOf`) |
| `consistency` | judged runs | `server_metrics.consistency`, `[0, 1]` |
| `chars` | judged runs | `server_metrics.chars` (correct/incorrect/extra/missed) |
| `quoteId` | quote runs | `run_quote_id(setup)` — the profile table's quote link |
| `mods` | always | `run_mods(setup)` — the same selection the boards render |

## Public profiles

`GET /api/v1/users/{name}/…` — another player's profile, by display name
(citext UNIQUE, so the lookup is case-insensitive exactly like the name's
uniqueness; guests have no accounts and therefore no profiles). Mounted behind
`OptionalAuth`: no session required, but an owner with a cookie is recognised.
Privacy is enforced **here, on the server** — a frontend hiding sections over
an API that still answers would be privacy theatre, and the e2e suite talks
HTTP past any frontend to keep that true.

Two per-account switches on `users` (migration `00018`):

| Column | Default | Gates |
|---|---|---|
| `profile_public` | **true** | The whole data surface below |
| `keyboard_public` | **false** | The keyboard portrait, additionally |

Owner surface: the switches ride on `GET /api/v1/me` and move via
`PATCH /api/v1/me/settings` (RequireOrigin + RequireAuth, partial body —
`{"profilePublic"?, "keyboardPublic"?}`), answering with the same user view
`/me` serves. The owner's session-scoped `/api/v1/profile/*` routes are
untouched by either switch: **the owner always sees their own profile whole**,
and the public paths also answer the owner (the preview case).

| Route | Closed profile, stranger/anon | Open profile, stranger/anon |
|---|---|---|
| `GET /users/{name}` | **200** `{name, joined, public:false}` | 200 `{name, joined, public:true}` |
| `…/summary` `…/activity` `…/histogram` `…/timeseries` `…/pbs` `…/runs` | **403 `profile_closed`** | 200 |
| `…/portrait` | 403 `profile_closed` | 200 iff `keyboard_public`, else **403 `portrait_closed`** |

An unknown name is a plain **404 `not_found`** — names are already public on
every board, so there is no enumeration story to blur it for. A closed profile
is deliberately **not** a 404: the page exists, "closed" is its state.

What the public routes serve is the **same aggregation code** the session
routes serve (shared `serve*` helpers — WHO may ask is each route's gate; WHAT
a summary is must not fork), with three deliberate differences:

- **`…/runs` is an allowlist**, narrower than the owner's feed: only ACCEPTED
  runs, and per row only the server's verdict numbers plus the derived display
  cells — no `clientMetrics`/`clientScore`, no `validation`, no `setup`, no
  restart counter, no log size. `TestPublicRunPayloadIsAnAllowlist` snapshots
  the exact key set; a new field is a deliberate disclosure that updates the
  snapshot in the same commit that argues why. Keyset pagination as the
  owner's feed.
- **`…/pbs` reads through the boards' ban predicate** (`active_bans`), unlike
  the session PBs read: a public surface must not show what every board hides.
  Likewise a banned owner's `…/runs` history is empty — the same predicate the
  public replay routes enforce. **Summary aggregates are NOT rebuilt under
  bans** (existing semantics, kept deliberately): a banned owner's counters
  keep answering, because going quiet would leak the ban through a side door
  the boards already refuse to leak through.
- **`…/portrait`** is served only when (`keyboard_public` OR owner) and the
  profile is open — see Privacy below for why the switch exists at all.

**The boundary with the boards — the line that must not move.** Profile
privacy does not touch the leaderboards. A closed profile stays ranked under
its name, its row stays clickable, and the run holding a board slot stays
publicly watchable: the public replay pair's predicate
(`internal/runs/queries.sql`) is extended with
`profile_public OR run holds a board slot`, so closing a profile hides the
**aggregated history page** — never a result its owner put into a public
ranking. A closed profile's **non-board** runs do become unwatchable (they
were only reachable through the history page privacy just closed), as the same
indistinguishable 404 as everything else unwatchable.
`TestClosedProfileKeepsItsBoardRowAndItsBoardReplay` is the pin.

## Privacy

Every `/api/v1/profile/*` route answers about the session's user and no route
accepts a user id — unchanged from v1. The public surface above is the
deliberate flag v1's documentation promised, with the promised shape:

- the profile switch is per-account; the default is **open** (product
  decision — the flag shipped together with the surface it gates, so no
  account existed with an expectation of privacy the default would betray);
- the **keyboard heatmap has its own opt-in, off by default** — per-key error
  rates and inter-key timings are effectively biometric (they identify and
  profile a person's motor behaviour), so they never ship on a public surface
  unless their owner turned them on themselves; a closed profile hides the
  portrait regardless of that opt-in.

## Performance posture

Aggregates run at request time, cache nothing, and are pinned by the 100k-run
perf suite (`docs/PERFORMANCE.md`, profile zone): each endpoint's plan must be
driven by `runs`' `(user_id, created_at)` index (or
`leaderboard_entries_user_idx` for the PBs) — no seq scan of `runs`, no
external sort — and hold its measured budget. The runs-list page itself stays
keyset-paginated, exactly as before.
