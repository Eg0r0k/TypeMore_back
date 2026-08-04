-- Moderation: issuing, revoking and reading bans.
--
-- "Banned right now" is defined ONCE, in the active_bans view
-- (db/migrations/00012). Nothing in this file repeats
-- `revoked_at IS NULL AND (expires_at IS NULL OR expires_at > now())`; every
-- query that needs it joins the view. That is what makes a temporary ban lift
-- itself and an unban restore a player instantly, everywhere, with no rebuild
-- and no janitor — there is one predicate and every reader is behind it.

-- name: ResolveUserByID :one
SELECT u.id, u.display_name
FROM users u
WHERE u.id = @id;

-- name: ResolveUserByDisplayName :many
-- Many, not one: display names are not unique, and a moderator who typed an
-- ambiguous one must be told rather than have the tool pick.
SELECT u.id, u.display_name
FROM users u
WHERE u.display_name = @display_name
ORDER BY u.id
LIMIT 10;

-- name: ResolveUserByEmail :many
-- An account may hold several identities; the email is the one a moderator has
-- in hand from a report.
SELECT DISTINCT u.id, u.display_name
FROM users u
         JOIN auth_identities ai ON ai.user_id = u.id
WHERE lower(ai.email) = lower(@email)
ORDER BY u.id
LIMIT 10;

-- name: ActiveBanFor :one
-- The one ban currently in force for a user, if any.
SELECT b.id, b.user_id, b.reason, b.issued_by, b.issued_at, b.expires_at, b.revoked_at
FROM bans b
WHERE b.user_id = @user_id
  AND EXISTS (SELECT 1 FROM active_bans a WHERE a.user_id = b.user_id)
  AND b.revoked_at IS NULL
  AND (b.expires_at IS NULL OR b.expires_at > now())
ORDER BY b.issued_at DESC
LIMIT 1;

-- name: InsertBan :one
-- issued_by is the human-readable note; issued_by_user is the actor as an
-- ACCOUNT (00023) — the admin surface records both, and the uuid is the one
-- an audit can join on.
INSERT INTO bans (user_id, reason, issued_by, issued_by_user, expires_at)
VALUES (@user_id, @reason, @issued_by, @issued_by_user, @expires_at)
RETURNING id, user_id, reason, issued_by, issued_at, expires_at, revoked_at;

-- name: UpdateBan :one
-- Re-banning an already-banned user amends the ban in place rather than
-- stacking a second one: two simultaneous bans on one account would make
-- "when does this lift" a question with two answers. The amendment takes the
-- amending actor: the row records who last shaped the restriction in force.
UPDATE bans
SET reason         = @reason,
    issued_by      = @issued_by,
    issued_by_user = @issued_by_user,
    expires_at     = @expires_at,
    issued_at      = now()
WHERE id = @id
RETURNING id, user_id, reason, issued_by, issued_at, expires_at, revoked_at;

-- name: RevokeBan :one
UPDATE bans
SET revoked_at      = now(),
    revoked_by_user = @revoked_by_user
WHERE id = @id AND revoked_at IS NULL
RETURNING id, user_id, reason, issued_by, issued_at, expires_at, revoked_at;

-- name: ListBans :many
-- Every ban, newest first, with the display name and whether it is in force.
-- `only_active` filters through the view rather than re-deriving the predicate.
SELECT b.id, b.user_id, u.display_name, b.reason, b.issued_by, b.issued_at,
       b.expires_at, b.revoked_at,
       EXISTS (SELECT 1
               FROM active_bans a
               WHERE a.user_id = b.user_id) AS user_restricted
FROM bans b
         JOIN users u ON u.id = b.user_id
WHERE NOT @only_active::boolean
   OR (b.revoked_at IS NULL AND (b.expires_at IS NULL OR b.expires_at > now()))
ORDER BY b.issued_at DESC
LIMIT @row_limit;

-- name: ListBansForUser :many
SELECT b.id, b.user_id, u.display_name, b.reason, b.issued_by, b.issued_at,
       b.expires_at, b.revoked_at,
       EXISTS (SELECT 1
               FROM active_bans a
               WHERE a.user_id = b.user_id) AS user_restricted
FROM bans b
         JOIN users u ON u.id = b.user_id
WHERE b.user_id = @user_id
ORDER BY b.issued_at DESC;

-- name: IsRestricted :one
-- The whole of what the rest of the server asks about a ban: yes or no, right
-- now. No reason, no expiry — the player-facing banner is deliberately opaque
-- and there is nothing here for a handler to leak.
SELECT EXISTS (SELECT 1 FROM active_bans a WHERE a.user_id = @user_id);

-- --- reports (docs/REPORTS.md) ---

-- name: CreateReport :one
-- File one report. ON CONFLICT DO NOTHING against the three partial unique
-- indexes makes a repeat submission a no-op rather than a duplicate row; the
-- caller reads the existing report separately and answers 200 either way, so a
-- double-tapped button is not an error the player has to understand.
--
-- The subject arrives as three nullable parameters of which the caller sets
-- exactly one. That shape is checked by the database (reports_subject_exactly
-- _one), so a service bug here is a failed INSERT rather than a malformed row.
INSERT INTO reports (subject_type, subject_user_id, subject_quote_id, subject_run_id,
                     reporter_id, reason, comment)
VALUES (@subject_type, sqlc.narg(subject_user_id), sqlc.narg(subject_quote_id),
        sqlc.narg(subject_run_id), @reporter_id, @reason, sqlc.narg(comment))
ON CONFLICT DO NOTHING
RETURNING id, subject_type, reporter_id, reason, comment, status, created_at;

-- name: FindOpenReport :one
-- The caller's existing OPEN report on one subject — what a repeat submission
-- answers with. The three IS NOT DISTINCT FROM comparisons let one query serve
-- every subject type: the two NULL parameters match the two NULL columns.
SELECT id, subject_type, reporter_id, reason, comment, status, created_at
FROM reports
WHERE reporter_id = @reporter_id
  AND status = 'open'
  AND subject_type = @subject_type
  AND subject_user_id IS NOT DISTINCT FROM sqlc.narg(subject_user_id)::uuid
  AND subject_quote_id IS NOT DISTINCT FROM sqlc.narg(subject_quote_id)::uuid
  AND subject_run_id IS NOT DISTINCT FROM sqlc.narg(subject_run_id)::uuid;

-- name: ListReportQueue :many
-- The moderator's queue: one row per SUBJECT, not per report. Forty complaints
-- about one quote are one thing to decide, and a queue that lists them forty
-- times is a queue nobody can work.
--
-- Ordered by pressure (how many people complained) and then by age, so the
-- loudest thing is first and nothing starves behind it. The reason list comes
-- back aggregated because "12 reports, all 'offensive'" and "12 reports, all
-- different" are different situations and the queue should show which it is.
SELECT r.subject_type,
       r.subject_user_id,
       r.subject_quote_id,
       r.subject_run_id,
       count(*)::bigint                            AS open_reports,
       min(r.created_at)::timestamptz              AS first_reported,
       max(r.created_at)::timestamptz              AS last_reported,
       array_agg(DISTINCT r.reason ORDER BY r.reason)::text[] AS reasons,
       -- The subject's own identity, resolved here so the queue is ONE round
       -- trip. Exactly one of these is non-null on any row, matching the
       -- subject columns above.
       u.display_name                              AS user_name,
       q.text                                      AS quote_text,
       q.lang                                      AS quote_lang,
       (q.withdrawn_at IS NOT NULL)::boolean       AS quote_withdrawn,
       run_owner.display_name                      AS run_owner_name,
       run.status                                  AS run_status
FROM reports r
         LEFT JOIN users u ON u.id = r.subject_user_id
         LEFT JOIN quotes q ON q.id = r.subject_quote_id
         LEFT JOIN runs run ON run.id = r.subject_run_id
         LEFT JOIN users run_owner ON run_owner.id = run.user_id
WHERE r.status = 'open'
  AND (sqlc.narg(subject_type)::text IS NULL OR r.subject_type = sqlc.narg(subject_type)::text)
GROUP BY r.subject_type, r.subject_user_id, r.subject_quote_id, r.subject_run_id,
         u.display_name, q.text, q.lang, q.withdrawn_at,
         run_owner.display_name, run.status
ORDER BY count(*) DESC, min(r.created_at)
LIMIT @row_limit;

-- name: ListReportsForSubject :many
-- Every report on one subject, open and closed, newest first — the detail view
-- behind a queue row. Reporter names are resolved here: a moderator judging
-- whether twelve reports are a real signal or one brigade needs to see who
-- filed them.
SELECT r.id, r.reason, r.comment, r.status, r.created_at,
       r.resolved_at, r.resolution_note,
       reporter.display_name AS reporter_name,
       resolver.display_name AS resolver_name
FROM reports r
         JOIN users reporter ON reporter.id = r.reporter_id
         LEFT JOIN users resolver ON resolver.id = r.resolved_by
WHERE r.subject_type = @subject_type
  AND r.subject_user_id IS NOT DISTINCT FROM sqlc.narg(subject_user_id)::uuid
  AND r.subject_quote_id IS NOT DISTINCT FROM sqlc.narg(subject_quote_id)::uuid
  AND r.subject_run_id IS NOT DISTINCT FROM sqlc.narg(subject_run_id)::uuid
ORDER BY r.created_at DESC
LIMIT @row_limit;

-- name: ResolveSubjectReports :execrows
-- Close EVERY open report on one subject in a single statement. The moderator
-- decided about the subject, not about each complaint, so the whole group moves
-- at once and cannot be left half-resolved by a crash between two updates.
--
-- The rowcount is the answer to "was there anything to resolve", which is what
-- makes a second identical call idempotent rather than a 404.
UPDATE reports
SET status          = @status,
    resolved_at     = now(),
    resolved_by     = @resolver::uuid,
    resolution_note = sqlc.narg(note)
WHERE status = 'open'
  AND subject_type = @subject_type
  AND subject_user_id IS NOT DISTINCT FROM sqlc.narg(subject_user_id)::uuid
  AND subject_quote_id IS NOT DISTINCT FROM sqlc.narg(subject_quote_id)::uuid
  AND subject_run_id IS NOT DISTINCT FROM sqlc.narg(subject_run_id)::uuid;

-- name: CountOpenReportsBy :one
-- How many open reports this player currently has outstanding. A cheap ceiling
-- on breadth that the per-IP token bucket cannot express: the limiter caps the
-- RATE of filing, this caps how much of the queue one person can occupy at once.
SELECT count(*)::bigint FROM reports WHERE reporter_id = @reporter_id AND status = 'open';

-- name: GrantBadge :one
-- Grant a badge (00029). IDEMPOTENT by construction: the partial unique index
-- covers exactly the live grants, so a second grant of a badge the account
-- already holds conflicts and updates nothing — and still RETURNS the row, so
-- the admin surface can answer "they have it, since this date, from that admin"
-- rather than an error the operator has to interpret.
--
-- display_order is deliberately untouched on conflict: re-granting must not
-- rearrange a showcase its owner arranged, and a fresh grant starts hidden
-- (NULL) so a badge never appears on somebody's public page without them
-- putting it there.
INSERT INTO user_badges (user_id, badge_code, granted_by)
VALUES (@user_id, @badge_code, sqlc.narg(granted_by))
ON CONFLICT (user_id, badge_code) WHERE revoked_at IS NULL
    DO UPDATE SET badge_code = user_badges.badge_code
RETURNING id, badge_code, granted_at, granted_by, display_order;

-- name: RevokeBadge :one
-- Soft-revoke the live grant of one badge. Idempotent in the same shape a
-- ban's revocation is: the predicate only matches a LIVE grant, so running it
-- twice revokes once, and the caller distinguishes the two by whether a row
-- came back — never by an error.
UPDATE user_badges
SET revoked_at = now(), revoked_by = sqlc.narg(revoked_by)
WHERE user_id = @user_id AND badge_code = @badge_code AND revoked_at IS NULL
RETURNING id, badge_code, granted_at, revoked_at;

-- name: ListBadgesOfUser :many
-- One account's badge history for the admin surface: live grants AND revoked
-- ones. Revocations are shown because "why did they used to have that" is the
-- question the soft revoke exists to answer, and because an operator about to
-- re-grant wants to see it was taken away before.
SELECT b.badge_code,
       b.granted_at,
       g.display_name AS granted_by_name,
       b.revoked_at,
       r.display_name AS revoked_by_name,
       b.display_order
FROM user_badges b
         LEFT JOIN users g ON g.id = b.granted_by
         LEFT JOIN users r ON r.id = b.revoked_by
WHERE b.user_id = $1
ORDER BY b.granted_at DESC;

-- name: ListHoldersOfBadge :many
-- Who currently holds badge X. Live grants only: this answers "who has it",
-- and the revocation history of an account is that account's own listing.
SELECT b.user_id, u.display_name, b.granted_at
FROM user_badges b
         JOIN users u ON u.id = b.user_id
WHERE b.badge_code = @badge_code AND b.revoked_at IS NULL
ORDER BY b.granted_at DESC
LIMIT @lim;
