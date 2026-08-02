# TypeMore Reports

Player reports: the **signal** half of moderation, next to bans
([`MODERATION.md`](MODERATION.md)), which are the **action** half.

One queue covers everything a player can complain about. v1 knows three
subjects — a player, a quote, a run — and adding a fourth is a bounded,
described change rather than a redesign.

## The rule the whole design turns on

**A report records a decision. It never performs one.**

Resolving a queue item writes a verdict, its author, its time and a note. It
does not ban anybody and does not withdraw anything. The act a verdict refers to
happens on the surface that owns it: `POST /admin/bans` for a player,
`POST /admin/quotes/{id}/withdrawal` for a quote.

That is not fastidiousness about clicks. It is what keeps a ban to **one**
birthplace, with one set of invariants and one audit trail — and it is what
keeps this domain from having to call into the quote domain, which the layering
rules forbid. The separation is load-bearing in both directions.

The corollary is a rule for adding subject types: **a subject with no action is
a queue item with no button.** Do not add one until the thing a moderator would
do about it exists.

## The table

One table, typed foreign keys — `db/migrations/00026_reports.sql`.

The rejected alternative is the usual `subject_type` + a bare `subject_id`. It
needs no migration to gain a type and pays for that with no referential
integrity at all: a report could name a quote that never existed, a deleted
subject would leave a row pointing at nothing, and only Go would stand between
the queue and rubbish. Here the database refuses both, and the cost is one
column, one branch in each `CHECK`, and one partial unique index per new type.

Two constraints, two different jobs:

| Constraint | Refuses |
|---|---|
| `reports_subject_exactly_one` | A row that is not **well-formed**: more or fewer than one subject column set, or a `subject_type` that disagrees with which one it is |
| `reports_reason_matches_subject` | A row that is not **meaningful**: `typo` is a thing a quote can have and a player cannot |

Plus `reports_no_self_report`, `reports_comment_length`, and
`reports_resolution_complete` (`status = 'open'` and `resolved_at IS NULL` move
together — `resolved_by` is deliberately outside that connective, being
`ON DELETE SET NULL` like a ban's actor, so deleting a moderator cannot make a
historical row violate a constraint).

**The reason vocabulary lives in SQL**, unlike the permission map, which is
versioned with the binary that enforces it. A reason is stored forever and the
queue is filtered by it, so a typo'd reason string would be an unfilterable row
in a history nobody can retroactively fix. The Go copy in `reports.go` exists
only to turn a bad request into a good error message instead of a constraint
violation rendered as a 500.

| Subject | Reasons |
|---|---|
| `user` | `offensive_name`, `impersonation`, `cheating`, `other` |
| `quote` | `typo`, `wrong_language`, `offensive`, `other` |
| `run` | `cheating`, `impossible_score`, `other` |

## The queue is per subject

Forty complaints about one quote are **one** decision. The queue groups by
subject, ordered by pressure (how many people complained) and then by age, so
the loudest thing is first and nothing starves behind it. Each item carries the
distinct reasons given — "12 reports, all `offensive`" and "12 reports, six
different reasons" are different situations — and a **snapshot** of the subject
as it stands now (the display name, the quote's text and whether it is already
withdrawn, the run's owner and status), resolved in the same query so triage
costs one round trip and does not require opening every item.

Resolving closes the whole group in one statement, so a crash cannot leave a
subject half-decided.

## Endpoints

| | |
|---|---|
| `POST /api/v1/reports` | File one. `{subject:{type,id}, reason, comment?}`. Session + Origin required |
| `GET /api/v1/admin/reports?type=&limit=` | The grouped queue — `reports:read` |
| `GET /api/v1/admin/reports/{type}/{id}` | Every report on one subject, open and closed — `reports:read` |
| `POST /api/v1/admin/reports/{type}/{id}/resolve` | `{verdict: actioned\|dismissed, note?}` — `reports:write` |

Filing answers **201** for a new report and **200** for one the caller already
has open on that subject — a double-tapped button is not a client error, and the
partial unique indexes make the deduplication race-free rather than
check-then-insert. A resolved report does **not** block a new one: a repeat
offence is a new incident, which is why those indexes are partial on
`status = 'open'`.

Reporting something that does not exist is a `404`, caught by the foreign keys
rather than by a lookup first — that would be both a race and a second round
trip for an answer the `INSERT` already has.

### Who may file

Authenticated players in good standing. A banned account is refused with the
same `restricted` gate that stops their runs counting. Anonymous filing is not
supported: it cannot be deduplicated, cannot be rate-limited beyond an IP, and
cannot be weighed later by who filed it.

Two different limits, because they bound different things:

- `TYPEMORE_REPORT_RATE_*` — a per-**account** token bucket (default: burst 5,
  one token a minute) bounding how *fast* reports arrive.
- `maxOpenReportsPerUser` (20) — how much of the queue one person may occupy at
  once. No refill rate washes this away; resolving their earlier reports is what
  frees the budget.

Reporter identities are visible to moderators. Twelve reports from one friend
group and twelve from strangers are different evidence.

## The action a report points at

Bans already existed. Quotes had **no** admin write surface at all — the corpus
was written only by `make import-quotes` — so the report queue would have had
nothing to do about a bad quote. `db/migrations/00025_quote_withdrawal.sql`
adds it.

### Why `withdrawn_at` is a new column and not `superseded`

Both end with "this quote stops being served", but they have different owners:

- `superseded` is **versioning**, owned by the importer. It means "a newer
  revision of this `(lang, upstream_id)` exists", and the importer both sets
  **and clears** it — `RepublishQuoteRevision` un-retires a revision whose bytes
  upstream reverted to. A moderator writing `superseded = true` would have their
  decision silently undone by the next import, weeks later.
- `withdrawn_at` is **moderation**, owned by the admin surface. Nothing in the
  import path reads or writes it.

`TestWithdrawalSurvivesAReimport` is the pin. If it ever passes against a reused
column, this decision has been quietly reverted.

### What withdrawal does and does not do

It removes the quote from **discovery** and never from **resolution by id**:

| Surface | Withdrawn quote |
|---|---|
| `GET /quotes` (browse), `GET /quotes/random` | absent |
| The leaderboard **board index** | its board is absent |
| `GET /quotes/{id}` | **answers**, with `withdrawn: true` |
| `GET /leaderboards/quote:{id}` (direct link) | **answers**, entries intact |
| `GET /runs/{id}/replay` on a run played on it | **answers** |

Every run ever played on a quote must keep replaying against the exact bytes it
was played on — the same frozen-bytes rule that keeps a retired revision
fetchable. A moderator can stop a quote being handed out; nobody can make
history unreplayable, and nobody can un-earn a result somebody already played
for.

The browse index's partial predicate was **rebuilt** to carry
`withdrawn_at IS NULL` alongside `NOT superseded`. Left alone, the new condition
would have become a post-index filter needing the heap, which is exactly the
property `PickRandomQuote`'s index-only scans exist for (3.9 ms → 2.2 ms,
[`QUOTES.md`](QUOTES.md)).

### The board index, and why the filter is in Go

A withdrawn quote's board leaves the index only. The filter runs in the
leaderboard **service**, over a set of withdrawn ids supplied by the composition
root (`WithdrawnQuotesFunc`), not in the board index's SQL. Excluding those rows
in SQL would mean rebuilding a bucket key from a quote id — `'quote:' || q.id` —
making a **second producer** of a format this codebase deliberately keeps to one
(`Bucket.Key` / `ParseBucketKey`). Nothing would keep the two spellings in step,
and the failure would be silent: the index would simply stop hiding withdrawn
boards.

### Quote moderation endpoints

| | |
|---|---|
| `GET /api/v1/admin/quotes/{id}` | The quote with its withdrawal record, text included — `reports:read` |
| `POST /api/v1/admin/quotes/{id}/withdrawal` | `{reason}`, required — `quotes:write` |
| `DELETE /api/v1/admin/quotes/{id}/withdrawal` | Restore; idempotent — `quotes:write` |

Withdrawing twice keeps the **first** decision (its time, actor and reason);
the response's `changed` says whether this call moved anything, the same diff
shape the ban surface answers with. A `reason` is required for the same reason a
ban's is: a decision with no note is one nobody can review later.

There is no publish, no edit, no delete on this surface, and no permission
grants one. A quote's bytes, language and hash are as immutable from here as
from the public API.

## Adding a subject type

1. **Migration**: one `subject_<x>_id` column with its foreign key, one branch
   in each of the two `CHECK`s, one partial unique index.
2. **Go**: one `SubjectType` constant, one entry in `reasonsBySubject`, one case
   in `Subject.Columns` and its inverse `subjectOf`.
3. **Queue query**: the column in the `GROUP BY`, and a `LEFT JOIN` for its
   snapshot.
4. **The action must already exist.** See the top of this document.

## Permissions

`reports:read`, `reports:write` and `quotes:write` join `bans:read` /
`bans:write` in `rolePermissions` (`internal/auth/permissions.go`) and ride to
the client on `/me`. `reports:write` is deliberately *not* the permission that
bans or withdraws: resolving records a verdict, so a role could one day triage
the queue without being able to punish.

The admin subtree answers **404**, not 403, to anyone without the permission —
see [`MODERATION.md`](MODERATION.md), "The admin surface".

The three admin subtrees share one prefix. `/admin/reports` and `/admin/quotes`
are sibling mounts and the ban surface is mounted **last**, on `/`, because it
owns paths at the root of `/admin`. chi resolves a static segment ahead of the
catch-all a root mount installs; `TestAdminSubtreesCoexist` is what stops that
ordering being lost.

## What v1 does not have

Recorded so the absences are decisions rather than gaps:

- **No auto-action on a threshold.** N reports never hide a subject by
  themselves — that hands a brigade a lever on anybody.
- **No feedback to the reporter.** Filing is a statement, not a ticket; the
  same posture as bans having no appeal form.
- **No assignment or in-review state.** One admin role, and a `claimed_by`
  nobody reads is a field that goes stale.
- **No reporter reputation.** The data to build it accrues (every dismissal is
  recorded against its reporter), but nothing reads it yet.
- **No run invalidation.** Confirming a cheating report means banning the owner,
  which removes them from every board through the predicate that already
  exists. A per-run "annul" would make a second writer of `runs.status` beside
  the replay worker, and would need the board slot recomputed.

## Tests

| What | Where |
|---|---|
| The schema refuses malformed rows: wrong discriminator, two subjects, none, a foreign reason, self-report, closed-without-timestamp, unknown status | `internal/moderation` (`reports_test.go`) |
| Queue grouping, counts, distinct reasons, snapshot, type filter | `internal/moderation` |
| Repeat filing is idempotent; a different reporter is a real second signal | `internal/moderation` |
| Resolve closes the whole group; resolving twice is a no-op; a resolved report does not block a new one | `internal/moderation` |
| Withdrawal survives a re-import; leaves discovery; stays resolvable by id; is idempotent and keeps the first record | `internal/quote` (`withdrawal_test.go`) |
| The full loop over HTTP: file → queue → withdraw → resolve → history | `internal/runs` (`reports_e2e_test.go`) |
| A withdrawn quote's board leaves the index only; direct link, entries and replay survive | `internal/runs` |
| Filing needs a session; a banned account is refused | `internal/runs` |
| The three admin subtrees coexist under one prefix | `internal/runs` |

## Related

- [`MODERATION.md`](MODERATION.md) — bans: the action a user report points at
- [`QUOTES.md`](QUOTES.md) — the corpus, its immutability rule and `superseded`
- [`LEADERBOARDS.md`](LEADERBOARDS.md) — the board index and the bucket key
