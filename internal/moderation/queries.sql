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
