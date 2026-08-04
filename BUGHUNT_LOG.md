# Bug hunt log

Cycle, applied to every entry: **hypothesis with a mechanism → red test →
minimal fix → mutation check**. A hypothesis that cannot be turned into a
failing test is not confirmed, and says so here with the evidence for why the
mechanism is not the one suspected. A disproved hypothesis is a result.

The starting list came from an audit dated 2026-07-29 and the refactor journal.
**The repository has moved since both.** Four of the seven were already fixed in
code — each is recorded below with the line that fixes it, because "already
fixed" is only trustworthy when somebody says which line and pins it.

---

## #1 — `lobbyEntryOf` does not know `ModeQuote`

**Hypothesis (mechanism).** `lobbyEntryOf` chose the dimension with a switch
naming `ModeTime` and `ModeWords`. `ModeQuote` matched neither arm, so both
`DurationMs` and `WordCount` stayed nil and `GET /api/v1/rooms` advertised a
quote room with no length at all — a listing that says nothing about the match
it is offering.

**Status: DISPROVED — the mechanism is gone.** `internal/ws/lobby.go:192-200`
now branches on `protocol.IsCounted(r.settings.Mode)`, the same predicate the
finish window and the per-word ceiling ask, so quote lands in the counted arm.
The comment there already records the old bug. No red test was possible: the
behaviour is correct on current code.

**Test written anyway** (`TestLobbyListsAQuoteRoomWithItsWordCount`,
`internal/ws/lobby_test.go`) — green on first run, deliberately. What is worth
protecting is not the old switch but the property: a quote room advertises its
length, and the next counted mode cannot silently fall through.

**Mutation check.** Replacing `IsCounted(...)` with `Mode == ModeWords` makes
exactly this test fail (`WordCount` nil) and nothing else.

---

## #2 — grace-token registration races the grace timer

**Hypothesis (mechanism).** `addGrace` was called after `room.disconnect`, which
had already armed the seat's grace timer, so a reconnect arriving in that window
could find no token and be handed a fresh connection while the timer went on to
expire the seat it should have reclaimed.

**Status: DISPROVED — the order is now the other way, with a re-check.**
`internal/ws/room.go:296-320`: the seat is detached under the lock, the lock is
RELEASED, `addGrace` registers the token, and only then is the lock retaken to
arm the timer — behind `if seat.disconnected && seat.sess == nil`, so a
reconnect that landed in between prevents the timer from being armed at all.
Both halves of the suspected race are closed, and by construction rather than by
timing.

No red test: the window the hypothesis needs does not exist in this code. A test
that tried to prove the absence of a race would be a timing test asserting a
negative, which proves nothing either way. **Concurrency, so: awaiting `-race`
in CI** — not claimed as proved here.

---

## #3 — match persist is fire-and-forget on `context.Background`

**Hypothesis (mechanism).** `go r.persist(snap)` with no owner meant a match
finishing as the process shut down was a goroutine nobody waited for: the
capture was lost on restart.

**Status: DISPROVED — the goroutine has an owner.** `Registry.persists` is a
`sync.WaitGroup` (`internal/ws/registry.go:47-50`), every write goes through
`reg.goPersist` (`registry.go:283`), and `WaitForPersists`
(`registry.go:294`, exposed as `Handler.WaitForPersists`, `handler.go:132`) is
what shutdown blocks on. `cmd/server/main.go` calls it after the HTTP server
stops accepting and before the pool closes — the ordering the comment there
describes.

No red test: the mechanism is absent. The existing
`internal/ws/persist_shutdown_test.go` already covers the surviving behaviour.

---

## #4 — no `defer pool.Close()` in `cmd/server`

**Status: DISPROVED — it is there.** `cmd/server/main.go:89`, registered
deliberately early so it unwinds LAST (every later defer runs first, so the
replay worker's drain and the registry's final persists still have a database
underneath them — the comment says exactly that). Nothing to test: this is a
line's presence, and a test asserting a `defer` exists would assert nothing
about behaviour.

---

## #5 — `settings_update` does not check that `quoteId` exists

**Hypothesis (mechanism).** `protocol.ValidateSettings` checks that a quote
room's `textSource.quoteId` is non-empty and (since this session) bounded in
length, but nothing checks that the id names a published quote. The relay has no
quote store and no seam to one, so it cannot.

**Status: CONFIRMED, and NOT fixed here.** The failure class is worth writing
down precisely, because it decides whether it needs fixing at all:

1. host sets a quote room with an id that resolves to nothing;
2. the server accepts it (nothing here can tell), and `start_match` sends a
   normal countdown;
3. EVERY client — host included — calls `loadMatchQuote`, gets a 404 from the
   public `GET /quotes/{id}`, and `failSetup` puts the session in `phase:
   'error'` (`entities/match/model/session-store.ts`);
4. the room shows the error panel with "could not load the quote" and stays in
   the lobby otherwise intact. No capture is written, no run is submitted, no
   state is corrupted. The match simply does not happen.

**Judgement: acceptable as a failure class, not acceptable as a silence.** It is
symmetric (nobody gets an advantage), non-destructive, and self-explaining on
screen. What it costs is a countdown everybody watched for nothing.

**Not fixed here because the minimal fix is not small**: the relay would need a
quote-existence seam (`func(ctx, id) (bool, error)` supplied by the composition
root, like `MatchStore`), which is a new interface, a new adapter and a new
failure mode of its own (what does the room do when the quote lookup itself
fails?). That is a feature, not a bug fix, and this cycle's rule is a minimal
fix for a red test. Recorded as a design decision to take deliberately.

**Reachability, for whoever picks it up:** the id only ever comes from the
client's own catalogue draw, so reaching this state takes a hand-written frame
from the host. It is a hostile-host bug, and the host can already ruin their own
room in a dozen legitimate ways.

---

## #6 — migration 00027's DO-block has never run against real data

**Hypothesis (mechanism).** Every suite migrates a FRESH database, so the
guard's `IF offenders > 0` branch — the whole point of the migration — had never
executed anywhere. A guard nobody has run is a guard nobody knows works: it
could raise on the wrong condition, fail to see duplicates it should catch, or
half-apply and leave the schema between versions.

**Status: CONFIRMED as untested; the guard itself is CORRECT.**

**Test** (`internal/platform/migrate/guard_00027_test.go`) — a fresh container
migrated to 00026, seeded with the exact history 00027 is about (one account,
two seats, one match), then asked for 00027:

- the migration is refused, and the message names the shape of the problem
  ("… group(s) with more than one row") rather than an index page;
- **nothing is half-applied**: no `match_runs_one_seat_per_user` index, and
  `goose_db_version` still says 26;
- resolving the duplicate the way the HINT describes lets it through;
- the index then refuses a second seat for the same account, by name;
- and guest seats (`user_id IS NULL`) are exempt — three, then four in one
  match, all legal, which is what the partial index is for.

**Red?** Not in the "existing code is wrong" sense — the guard behaves. It was
red in the sense that mattered: on the first run it failed for a real reason
(the seed used columns `match_runs` does not have), which is itself evidence
nobody had ever written a row against this table in a migration test.

**Mutation check.** Dropping the `HAVING count(*) > 1` clause makes the guard
fire on a clean database, and exactly the two tests in this file fail.

---

## #7 — testcontainers cold-start flake

**Status: NOT REPRODUCED, environmental.** Not a defect in this codebase: the
first container start pulls `postgres:17` and races the 90 s wait strategy. Seen
once this session in a different shape — a full `go test ./...` failed
`internal/replay` while several packages were starting containers in parallel,
and the same package passed alone (410 s). That is load, not logic.

Nothing to fix in the tree. The honest mitigations are CI-side (pre-pull the
image, cap package parallelism), and the existing `CLAUDE.md` note about telling
an environment collapse from a regression is what covers it operationally.

---

## Summary

| # | Suspect | Status |
|---|---|---|
| 1 | `lobbyEntryOf` / ModeQuote | Disproved (fixed earlier); pinned by a new test |
| 2 | grace-token race | Disproved; awaiting `-race` in CI |
| 3 | fire-and-forget persist | Disproved (WaitGroup + shutdown wait) |
| 4 | missing `defer pool.Close()` | Disproved (present) |
| 5 | unchecked `quoteId` | **Confirmed**, failure class documented, fix deferred with reasons |
| 6 | 00027 guard untested | **Confirmed untested**; now tested, guard correct |
| 7 | testcontainers flake | Not reproduced; environmental |

No production code was changed by this hunt. Two tests were added (#1, #6) and
one design decision was recorded rather than coded (#5).
