-- +goose Up
--
-- Ban management v1 (docs/MODERATION.md).
--
-- The bans table is not a stub — 00006 created it real, with the read-side
-- filter, on the reasoning that "a leaderboard without ban filtering is not
-- something you can safely retrofit, while the admin surface that ISSUES bans
-- can wait". This is that surface arriving, and it needs three things the
-- original shape did not have.

-- 1. A ban has an identity of its own, so a user can have a HISTORY.
--
-- user_id was the primary key, which made a ban a property of the account and
-- allowed exactly one, forever. Revoking then re-banning has to be two rows or
-- the first one is destroyed, and "what happened to this account" is a question
-- moderation will be asked.
ALTER TABLE bans DROP CONSTRAINT bans_pkey;
ALTER TABLE bans ADD COLUMN id uuid NOT NULL DEFAULT gen_random_uuid();
ALTER TABLE bans ADD PRIMARY KEY (id);

-- The read path looks bans up BY USER, and now more than one row may match.
CREATE INDEX bans_user_idx ON bans (user_id);

-- 2. issued_by is an operator, not an account.
--
-- Bans are issued by a server CLI, by whoever has shell on the box. Modelling
-- that as a uuid REFERENCES users made the issuer a player, which it is not —
-- it forced a real account to exist for every moderator and it silently NULLed
-- the issuer if that account was ever deleted. It is free text: a name, an
-- ops handle, a ticket id. Existing rows keep whatever uuid they carried, as
-- its text, because throwing away provenance is worse than an ugly string.
ALTER TABLE bans DROP CONSTRAINT bans_issued_by_fkey;
ALTER TABLE bans ALTER COLUMN issued_by TYPE text USING issued_by::text;

-- 3. Revocation is a fact with a time, not a deletion.
--
-- Deleting the row would make an unban indistinguishable from a ban that never
-- happened. NULL means in force.
ALTER TABLE bans ADD COLUMN revoked_at timestamptz;

COMMENT ON COLUMN bans.reason IS
    'Internal moderation note. NEVER exposed to the player: the banner they see '
        'is deliberately opaque (docs/MODERATION.md).';

-- +goose StatementBegin
-- The single definition of "banned right now", extended for revocation.
--
-- This view is the ONE place the predicate is written, and everything that
-- filters on bans already reads it: leaderboard_ranked (and therefore
-- leaderboard_rows), and both public replay reads in internal/runs/queries.sql.
-- Adding revocation here is what makes those three expiry- and
-- revocation-aware without any of them being edited.
--
-- Temporary bans need no janitor for exactly this reason. Expiry is evaluated
-- at read time, so a temp ban lifts itself the second it lapses, and an unban
-- restores a player's board entries instantly with no rebuild — the entries
-- were never deleted, only filtered.
CREATE OR REPLACE VIEW active_bans AS
SELECT user_id
FROM bans
WHERE revoked_at IS NULL
  AND (expires_at IS NULL OR expires_at > now());
-- +goose StatementEnd

-- +goose Down
CREATE OR REPLACE VIEW active_bans AS
SELECT user_id
FROM bans
WHERE expires_at IS NULL OR expires_at > now();

ALTER TABLE bans DROP COLUMN revoked_at;
ALTER TABLE bans ALTER COLUMN issued_by TYPE uuid USING issued_by::uuid;
ALTER TABLE bans ADD CONSTRAINT bans_issued_by_fkey
    FOREIGN KEY (issued_by) REFERENCES users (id) ON DELETE SET NULL;
DROP INDEX bans_user_idx;
ALTER TABLE bans DROP CONSTRAINT bans_pkey;
ALTER TABLE bans DROP COLUMN id;
ALTER TABLE bans ADD PRIMARY KEY (user_id);
