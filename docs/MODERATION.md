# TypeMore Moderation — bans v1

What a ban does, what it deliberately does not do, and how to issue one.

## The scope, first

A ban stops a player's runs being **counted** and hides their **leaderboard**
entries. That is all of it.

| | |
|---|---|
| `POST /api/v1/runs` | **403 `account_restricted`**, and the run is not stored |
| Leaderboard reads | their entries are hidden, everywhere, including the board index |
| Public replay endpoints | **404** for a banned owner's runs |
| `GET /api/v1/me` | gains `restricted: true` |
| **Login, sessions** | **untouched** |
| **Rooms, matches, chat** | **untouched** |
| Reading boards, watching other people's replays | **untouched** |

A banned player can still sign in and still play a casual match against a
friend. `TestABannedUserStillLogsInAndKeepsItsSession` is the test that keeps
that true — if somebody decides a ban should block login, they have to delete
that test and say so out loud.

## The predicate, defined once

```sql
CREATE VIEW active_bans AS
SELECT user_id FROM bans
WHERE revoked_at IS NULL
  AND (expires_at IS NULL OR expires_at > now());
```

**Every** reader in the server goes through this view: `leaderboard_ranked` (and
therefore `leaderboard_rows`), both public replay reads in
`internal/runs/queries.sql`, the run-submission gate and the `/me` flag. Nothing
repeats the expression, which is why adding `revoked_at` to it in
`db/migrations/00012` made all five revocation-aware without any of them being
edited.

Two consequences worth stating plainly, because both are things people expect to
need code and do not:

- **A temporary ban needs no janitor.** Expiry is evaluated at read time, so the
  ban lifts itself the second it lapses. There is no sweep, no scheduled job and
  nothing to fall behind.
- **An unban is instant and needs no rebuild.** Board entries are *filtered*, not
  deleted, so restoring a player is one predicate flipping. `TestBoardEntriesHideAndRestoreWithNoRebuild`
  walks the whole cycle without recomputing anything.

## The reason is internal, always

`bans.reason` is `NOT NULL` and it is a **moderation note**. It is never sent to
the player and no endpoint returns it. The banner a restricted player sees says
«аккаунт ограничен» and nothing else — no reason, no expiry, no issuer, no
appeal form.

That is a deliberate product decision, not an oversight:

- a reason shown to the player is a reason that has to be defensible to the
  player, which turns every ban into a negotiation;
- an expiry shown to the player tells a cheater exactly when to come back; and
- the interfaces enforce it. `moderation.Restrictions` — the seam both the
  submission gate and `/me` are behind — returns **one boolean**. There is no
  reason in scope for a handler to leak, and `TestMeReportsRestrictedAndNothingMore`
  greps the wire for the note to make sure.

## The admin surface

Bans are issued over an authenticated HTTP subtree, `/api/v1/admin`, by
accounts holding the admin role.

**This section reverses a recorded decision, and says so out loud.** v1 issued
bans by CLI only (`cmd/banctl`, now deleted) on the argument that no admin
endpoints keeps an authentication bug's blast radius away from moderation. The
argument was sound and the mechanism was not: the CLI needed a Go toolchain
and direct database reach, neither of which a deployed stand has — moderation
that cannot be exercised where the players are is a design property with no
referent. The blast-radius concern is answered structurally now, five ways:

1. **Permissions, not role checks.** Enforcement speaks capabilities —
   `bans:read`, `bans:write` (`internal/auth/permissions.go`) — and routes ask
   for a permission via `RequirePermission`. `users.role` (00023) is only the
   stored fact a role→permissions map expands, per request, from the database:
   demotion takes effect on the target's next request, no session invalidation
   required. The map is code, versioned with the binary that enforces it; a
   future `moderator` tier is a map entry granting a subset, not a sweep over
   handlers.
2. **The subtree is invisible.** A permission miss answers **404**,
   byte-identical to an unknown route — not 401, not 403 — and the subtree is
   mounted behind `OptionalAuth`, so the anonymous prober and the logged-in
   player get the same nothing. The permission check runs before even the
   Origin (CSRF) check: nobody without the permission learns anything,
   whatever they send.
3. **No admins, no surface.** `TYPEMORE_ADMINS` empty (the default) means the
   subtree is not mounted at all. A self-hosted stand that never configures an
   admin has no admin routes, not admin routes nobody can pass.
4. **Bootstrap is promotion-only.** At startup, accounts owning a **verified**
   identity on a configured email are promoted. Removal from the list demotes
   nobody — the env var is how the first admin appears, not a sync source.
5. **Every act is audited** — as an account (`issued_by_user` /
   `revoked_by_user`, below) and as a structured log line carrying actor and
   target.

`GET /me` carries the caller's expanded `permissions` array (omitted for a
plain player) — the client renders the admin UI from capabilities, never from
the role.

### Endpoints

All under `/api/v1/admin`; reads behind `bans:read`, mutations behind
`bans:write` plus the Origin check.

| | |
|---|---|
| `GET /bans?active=&limit=` | Bans newest first; `active=0` includes revoked and lapsed |
| `GET /users/{identifier}/bans` | Resolution + the account's full ban history + `restricted` now |
| `POST /bans` `{user, reason, until?}` | Issue or amend; the response is a **diff** (below) |
| `DELETE /users/{userID}/ban` | Revoke; idempotent, answers `{revoked: false}` when there was nothing to do |

`{identifier}` / `user` is a **display name, a uuid, or an email**. Resolution
tries them in that order of certainty — uuid, then email, then display name —
so pasting an id can never match somebody's chosen nickname. An identifier
matching more than one account is a **409 carrying the candidates**; it never
picks. One matching nothing is the subtree's usual 404. The unban target is a
uuid only: a revocation is precise or it is not issued, and the resolution
endpoint is one GET away.

`reason` is required. A ban with no note is one nobody can review later.

`until` takes either a duration (`72h`) or an RFC3339 instant, because both are
natural: `72h` is what a moderator thinks and an instant is what a script
computes. Omitting it is a **permanent** ban — an explicit omission rather than
a magic zero. An instant in the past is a 400: it would mint a ban already
lapsed.

### Idempotency, and why the response is a diff

Banning an already-banned account **amends** the existing ban rather than
stacking a second one; two simultaneous bans would make "when does this lift" a
question with two answers. So `POST /bans` answers with what *changed*:

```json
{"user": {...}, "ban": {...}, "amended": true, "previous": {"expiresAt": null, ...}}
```

An admin who re-submitted by accident needs to see `"amended": true` with an
identical `previous`; one extending an expiry needs to see what moved. "ok"
would be true in both cases and useful in neither.

`DELETE …/ban` on an unbanned account is likewise not an error — it answers
`{"revoked": false}`, because DELETE run twice must mean the same thing as
DELETE run once.

The `reason` and the issuer are visible on this surface. That does not bend
"the reason is internal, always": the rule is about the banned player's wire,
and this subtree is exactly the internal audience the note is kept for.

## The record

```
bans(id, user_id, reason, issued_by, issued_by_user, issued_at,
     expires_at, revoked_at, revoked_by_user)
```

- **`id`** — a ban has its own identity, so an account can have a **history**.
  It used to be keyed by `user_id`, which allowed exactly one ban ever.
- **`issued_by`** is **text** — the human-readable note (the admin's display
  name; historically an ops handle from the CLI era).
- **`issued_by_user` / `revoked_by_user`** (00023) are the actors as
  **accounts**, the columns an audit can join on. `SET NULL` on deletion keeps
  the record intact when an admin account goes away; NULL also covers acts
  with no account behind them (tests, one-off tooling).
- **`revoked_at`** — revocation is a fact with a time. Deleting the row would
  make an unban indistinguishable from a ban that never happened.

Re-banning after a revocation is a **new row**, so an account's history reads
as a history rather than as a single mutable verdict.

## What v1 does not have

Recorded so the absences are decisions rather than gaps:

- **No appeal mechanics.** The banner is a statement, not a form.
- **No admin UI in this repo.** The API above is the contract; the panel is the
  frontend's to build from `/me`'s `permissions`.
- **No shadow ban.** The 403 is honest; see `apiErrRestricted`.
- **No uniqueness constraint on "one active ban per user".** It cannot be a
  partial unique index, because the predicate contains `now()` and index
  predicates must be immutable. The admin surface is the only writer and it
  amends rather than inserts, so the invariant holds in practice; two admins racing on the
  same account is the case that could break it, and it would produce a duplicate
  ban rather than a missing one.
- **No IP or device bans.** Account scope only.

Two of these have since been answered elsewhere rather than here: player
**reports** are the signal that reaches a moderator ([`REPORTS.md`](REPORTS.md)),
and a bad **quote** now has an action to point at — withdrawal — which is a
separate surface with its own permission, because a ban and a quote are not the
same kind of thing.

## Tests

| What | Where |
|---|---|
| Predicate table: permanent / future expiry / past expiry / revoked | `internal/moderation` |
| A temporary ban lifts itself, with no janitor | `internal/moderation` |
| `ban` amends rather than stacks; `unban` revokes rather than deletes | `internal/moderation` |
| Identifier resolution, and refusing an ambiguous one | `internal/moderation` |
| The admin surface: amend diff, idempotent revoke, 409 candidates, audit actors | `internal/moderation` (`admin_http_test.go`) |
| Bootstrap promotes verified emails only, idempotently; permission miss is 404 for everyone; role read per request; `/me` permissions | `internal/auth` (`permissions_test.go`) |
| 403 on submit, and the run not stored | `internal/runs` |
| Submission resumes when a ban lapses | `internal/runs` |
| `/me` carries the flag and nothing else | `internal/runs` |
| The untouched scope: login, sessions, board reads | `internal/runs` |
| Boards hide then restore, with no rebuild | `internal/leaderboard` |
| A revoked ban stops hiding immediately | `internal/leaderboard` |

## Related

- [`LEADERBOARDS.md`](LEADERBOARDS.md) — why ban filtering is on the read side
- [`RUNS.md`](RUNS.md) — the submission path the 403 sits on
- `db/migrations/00006_leaderboards.sql` — where `bans` and `active_bans` began
- `db/migrations/00012_bans_v1.sql` — the shape they have now
- `db/migrations/00023_admin.sql` — the role column and the audit actors


## Overruling a run's verdict

`POST /api/v1/admin/runs/{id}/status` (`runs:override`), with `{"status":
"accepted"|"flagged"|"rejected", "reason": "…"}`.

**Why this exists.** The replay worker decides every run's status, and it used to
be the only writer of it. That is right while a verdict is arithmetic — a seed
that does not reproduce, a log the reducer refused — and wrong the moment one is
a JUDGEMENT. `superhuman-burst` is a judgement: it fires on speed, speed is a
continuum with real people at the top of it, and `leaderboard_eligible_runs`
selects on `status = 'accepted'`, so a wrongly flagged run does not annoy a
player — it removes their result from the board.

A check whose false positives cannot be undone has to be set where the blast
radius allows rather than where the evidence says. This endpoint is what lets the
speed ceiling be set honestly (docs/REPLAY.md, `BURST_CEILING`).

Four things happen in ONE transaction: the run is locked and re-read, the status
is written, the decision is appended to `run_status_overrides`, and the
leaderboard projection is brought back in line. A rollback takes all four. The
alternative — status now, projection later — is a window in which a reinstated
run is `accepted` and absent from the board it was reinstated onto.

**A decision sticks.** The revalidation claim skips any run with an override
row, so the next core release does not recompute the worker's answer over the
operator's. Without that the endpoint would be a suggestion.

**What it does NOT touch:** the verdict, the server's numbers, the client's. An
override disagrees with what the evidence was taken to MEAN; rewriting the
evidence would leave a decision with no case behind it.

`pending` is refused on both sides of the move. A pending run has no verdict to
disagree with, and moving one there would hand it back to the worker as new work
— which is `revalidate`, not an override. An override that changes nothing is
refused too: the trail is a list of decisions, and a row that moves nothing later
reads as one.

`reason` is required by the schema as well as the handler.

## The review queue

`GET /api/v1/admin/runs/review?minSuspicion=0.1&limit=50` (`runs:review`).

Judged runs at or above a suspicion floor, worst first. **It lists `accepted`
runs too, and that is the point of it.** A run that crossed the review threshold
is already `flagged` and easy to find; the ones worth a human's attention are
those carrying real suspicion that did NOT cross it — which is exactly where
`sustained_superhuman` now leaves a superhuman speed, since that rule no longer
routes on its own (docs/REPLAY.md). A queue showing only `flagged` would list the
cases the machine was already sure about and hide the ones it wanted help with.

The default floor is `0.1`. Not zero: every judged run carries a suspicion, most
of them a rounding error from key rollover. Over the 127 honest runs of the
2026-08-03 export the highest is 0.1776 and the mean is 0.0031, so 0.1 is where
the queue starts being short enough to read.

`GET /api/v1/admin/runs/{id}/overrides` (`runs:review`) is one run's decision
history, newest first.

**The two permissions are separate on purpose.** `runs:review` reads the queue;
`runs:override` acts on it. A run's status decides whether it holds a leaderboard
slot, so putting a result back on the board is the most consequential thing the
admin surface can do — a role that triages should be able to do so without also
being able to do that.
