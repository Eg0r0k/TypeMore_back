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

## Issuing a ban

Bans are issued by a **server CLI only**. There are no admin endpoints and no
admin UI, which keeps the blast radius of an authentication bug away from
moderation entirely: to ban somebody you need shell on the box.

```sh
make ban user=ada reason="repeated macro use" until=72h
make unban user=ada
make bans                 # active only
make bans all=1           # including revoked and expired
make ban-show user=ada    # every ban this account has ever had
```

or directly:

```sh
go run ./cmd/banctl ban <user> --reason "..." [--until 72h | RFC3339] [--by NAME]
go run ./cmd/banctl unban <user>
go run ./cmd/banctl list [--active | --all] [--limit N]
go run ./cmd/banctl show <user>
```

`<user>` is a **display name, a uuid, or an email**. Resolution tries them in
that order of certainty — uuid, then email, then display name — so pasting an id
can never match somebody's chosen nickname. An identifier that matches more than
one account prints the candidates and refuses; it never picks.

`--reason` is required. A ban with no note is one nobody can review later.

`--until` takes either a duration (`72h`) or an RFC3339 instant, because both are
natural: `72h` is what a moderator thinks and an instant is what a script
computes. Omitting it is a **permanent** ban — an explicit omission rather than a
magic zero.

### Idempotency, and why the output is a diff

Banning an already-banned account **amends** the existing ban rather than
stacking a second one; two simultaneous bans would make "when does this lift" a
question with two answers. So `ban` prints what *changed*:

```
ada (a1b2…) was already banned; amended the existing ban
  until    permanent -> 2026-07-30T09:00:00Z
```

An operator who re-ran the command by accident needs to see `nothing changed`;
one extending an expiry needs to see that it moved. "ok" would be true in both
cases and useful in neither.

`unban` on an unbanned account is likewise not an error — it says there was
nothing to do.

## The record

```
bans(id, user_id, reason, issued_by, issued_at, expires_at, revoked_at)
```

- **`id`** — a ban has its own identity, so an account can have a **history**.
  It used to be keyed by `user_id`, which allowed exactly one ban ever.
- **`issued_by`** is **text**, not a user id. Bans are issued by whoever has
  shell on the box; the field is a name, an ops handle or a ticket id, defaulted
  to the OS user. It is an audit note and **not** an access control — anyone who
  can run `banctl` already has the database.
- **`revoked_at`** — revocation is a fact with a time. Deleting the row would
  make an unban indistinguishable from a ban that never happened.

Re-banning after a revocation is a **new row**, so `banctl show` reads as a
history rather than as a single mutable verdict.

## What v1 does not have

Recorded so the absences are decisions rather than gaps:

- **No appeal mechanics.** The banner is a statement, not a form.
- **No admin API or UI**, deliberately (above).
- **No shadow ban.** The 403 is honest; see `apiErrRestricted`.
- **No uniqueness constraint on "one active ban per user".** It cannot be a
  partial unique index, because the predicate contains `now()` and index
  predicates must be immutable. `banctl` is the only writer and it amends rather
  than inserts, so the invariant holds in practice; two operators racing on the
  same account is the case that could break it, and it would produce a duplicate
  ban rather than a missing one.
- **No IP or device bans.** Account scope only.

## Tests

| What | Where |
|---|---|
| Predicate table: permanent / future expiry / past expiry / revoked | `internal/moderation` |
| A temporary ban lifts itself, with no janitor | `internal/moderation` |
| `ban` amends rather than stacks; `unban` revokes rather than deletes | `internal/moderation` |
| Identifier resolution, and refusing an ambiguous one | `internal/moderation` |
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
