-- +goose Up
--
-- The admin surface (docs/MODERATION.md, rewritten alongside this migration):
-- moderation moves from "whoever has shell on the box" (cmd/banctl, now
-- deleted) to an authenticated HTTP subtree, and this is the schema half of
-- that move.
--
-- users.role is a coarse ROLE, deliberately not a permissions table. The
-- enforcement vocabulary in code is permissions ("bans:read", "bans:write" —
-- internal/auth/permissions.go), and handlers only ever ask for a permission;
-- the role is the stored fact a role→permissions map expands at request time.
-- Storing the expansion instead (user_permissions rows) buys revocation
-- granularity nothing needs yet, at the price of the map living in data where
-- the binary that enforces it cannot version it. When a 'moderator' tier
-- arrives it is a new CHECK value and a new map entry — not a schema change
-- per capability. DEFAULT 'player': nobody is an admin by existing.
--
-- Bootstrap is promotion-only: TYPEMORE_ADMINS (emails) is walked at startup
-- and matching verified accounts are promoted. Removal from the list demotes
-- nobody — the env var is how the FIRST admin appears, not a sync source, so a
-- deploy with a trimmed list cannot silently strip a role someone is relying
-- on mid-session. Demotion is an explicit act (today: SQL; later: the admin
-- surface itself).
ALTER TABLE users
    ADD COLUMN role text NOT NULL DEFAULT 'player'
        CONSTRAINT users_role_check CHECK (role IN ('player', 'admin'));

-- Who did it, as an ACCOUNT. bans.issued_by (text) predates the admin surface
-- and stays what it always was — a free-form ops note ("who has shell"). The
-- admin surface knows the actor as a row, so it records the uuid beside the
-- note; SET NULL on deletion keeps the ban history intact when an admin
-- account is deleted, exactly as match_runs.user_id treats its players.
ALTER TABLE bans
    ADD COLUMN issued_by_user  uuid REFERENCES users (id) ON DELETE SET NULL,
    ADD COLUMN revoked_by_user uuid REFERENCES users (id) ON DELETE SET NULL;

-- +goose Down
ALTER TABLE bans
    DROP COLUMN revoked_by_user,
    DROP COLUMN issued_by_user;

ALTER TABLE users
    DROP COLUMN role;
