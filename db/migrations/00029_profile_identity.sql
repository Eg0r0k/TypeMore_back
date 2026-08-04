-- +goose Up
-- The profile's IDENTITY half: what a player says about themselves (bio, the
-- board they type on, where else to find them) and what the operators say about
-- them (badges). Everything here is public-surface data; nothing here gates
-- anything or feeds a score.

-- --- 1. Two free-text fields on the account itself -------------------------
--
-- On `users` rather than in a `user_profiles` side table: both are single-valued
-- per account, both are read on the very page that already reads `users` for the
-- name and the privacy switches, and a 1:1 table would buy a join for every
-- profile read and nothing else. (A side table earns its keep when the columns
-- are optional AND wide AND rarely read; 250 + 100 characters are none of those.)
--
-- NULL and empty string are deliberately NOT the same thing here: NULL is "never
-- set", '' is impossible — the CHECKs forbid it — so a client never has to guess
-- whether an empty bio is a cleared one or an unset one. Clearing sends NULL.
--
-- The length caps are the SAME numbers the API enforces, stated in both places
-- on purpose: the handler's copy produces a decent error message, and this one
-- is what makes the cap true for rows that arrive by any path the handler is not
-- on. char_length counts CHARACTERS, matching the API's rune count — a byte cap
-- would silently give a Cyrillic bio half the room an ASCII one gets.
ALTER TABLE users
    ADD COLUMN bio      text CHECK (bio IS NULL OR char_length(bio) BETWEEN 1 AND 250),
    ADD COLUMN keyboard text CHECK (keyboard IS NULL OR char_length(keyboard) BETWEEN 1 AND 100);

-- --- 2. Badges -------------------------------------------------------------
--
-- WHY THERE IS NO `badges` TABLE, and why badge_code has no foreign key.
--
-- A badge's DEFINITION — its name, its description, its icon, its colour — lives
-- in the frontend registry (`src/entities/badge/registry.ts`) and nowhere else,
-- exactly as the role→permission map lives in Go (internal/auth/permissions.go,
-- 00023) rather than in a table. The reasoning is the same: a definition that is
-- rendered by one client and never queried by the server is presentation, and
-- presentation versioned with the binary that renders it cannot drift from it.
-- A `badges` table would be a second home for the same fact, kept in step by
-- nobody, and it would make adding a badge a migration instead of a commit.
--
-- So this table stores GRANTS, not badges: which account holds which code, since
-- when, on whose authority. `badge_code` is therefore a value, not a reference —
-- there is no parent row for it to reference. The server validates it against
-- the code list it knows (internal/profile/badges.go) before inserting, so an
-- unknown code cannot be granted; a code RETIRED from the registry later still
-- reads back from history, which is the behaviour a grant record wants.
--
-- This is not a 3NF violation: nothing in this table is functionally determined
-- by badge_code, because nothing about the badge itself is stored here at all.
CREATE TABLE user_badges (
    id      uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,

    -- Bounded and shaped so a typo is a refusal rather than a row nobody can
    -- render: the registry's codes are lower_snake_case identifiers.
    badge_code text NOT NULL
        CHECK (badge_code ~ '^[a-z][a-z0-9_]{1,39}$'),

    granted_at timestamptz NOT NULL DEFAULT now(),
    -- ON DELETE SET NULL, like a ban's actor columns (00023): the grant outlives
    -- the admin account that made it, and NULL also covers grants with no
    -- account behind them (tests, one-off tooling).
    granted_by uuid REFERENCES users (id) ON DELETE SET NULL,

    -- Revocation is SOFT and is a fact with a time, for the same reason an
    -- unban is: deleting the row would make a revoked badge indistinguishable
    -- from one that was never granted, and "why did they used to have that"
    -- is exactly the question an audit trail exists to answer.
    revoked_at timestamptz,
    revoked_by uuid REFERENCES users (id) ON DELETE SET NULL,

    -- The SHOWCASE position, chosen by the badge's owner — not by the operator
    -- who granted it. NULL means "held but not shown", which is why this is
    -- nullable rather than defaulted: a fresh grant must not silently rearrange
    -- a showcase its owner has already arranged.
    --
    -- Deliberately NOT unique per user. Enforcing a gapless, collision-free
    -- ordering in SQL would make every reorder a multi-statement dance around a
    -- unique index (or a deferred constraint), and the read is one ORDER BY over
    -- at most a handful of rows: a duplicate position is a cosmetic tie broken
    -- by badge_code, not a corruption. The API writes the whole showcase at once
    -- and assigns 0..n-1, so ties do not arise through the supported path.
    display_order integer CHECK (display_order IS NULL OR display_order >= 0),

    -- An actor with no act is not a record of anything. The converse is NOT
    -- required: `revoked_by` is SET NULL on account deletion, so a revocation
    -- whose admin account is gone must stay a legal row.
    CONSTRAINT user_badges_revoker_implies_revocation CHECK (
        revoked_by IS NULL OR revoked_at IS NOT NULL)
);

-- One LIVE grant per (account, code). Partial on `revoked_at IS NULL`, so a
-- badge can be granted again after a revocation — that is a new grant with its
-- own timestamp and its own actor, and the history keeps both. It cannot be a
-- plain unique constraint for the same reason `active_bans` cannot be a partial
-- index over now(): what "live" means is a predicate, and this is the half of it
-- that is immutable.
CREATE UNIQUE INDEX user_badges_live_uniq ON user_badges (user_id, badge_code)
    WHERE revoked_at IS NULL;

-- The public read: one account's SHOWCASE, in order. Partial on exactly the
-- rows that read serves — live grants the owner chose to show — so the index
-- holds the working set rather than every revocation ever recorded.
CREATE INDEX user_badges_showcase_idx ON user_badges (user_id, display_order)
    WHERE revoked_at IS NULL AND display_order IS NOT NULL;

-- The admin read: who holds badge X. Cheap to keep, and the reason the
-- moderation surface can answer that question at all.
CREATE INDEX user_badges_by_code_idx ON user_badges (badge_code, granted_at DESC)
    WHERE revoked_at IS NULL;

-- --- 3. Social links -------------------------------------------------------
--
-- A ROW PER LINK, not three columns on `users`. `github_handle, youtube_handle,
-- twitch_handle` would be a repeating group in the 1NF sense — the same fact
-- ("this account is also known as X on service Y") spelled three times, where
-- adding a fourth service is a migration and asking "who links a Twitch" is a
-- query over a column that may not exist yet.
--
-- (user_id, kind) is the natural key and IS the primary key: a link is
-- identified by whose it is and which service it names, there is never a second
-- row for the same pair, and a surrogate id would only add a column nothing
-- would ever look a row up by.
--
-- WE STORE THE HANDLE, NEVER A URL. A stored URL is a stored redirect: it can
-- point anywhere, including `javascript:` and a phishing host wearing a
-- lookalike domain, and every renderer would then have to re-validate what the
-- writer already accepted. A handle is an opaque identifier that the RENDERER
-- turns into a link by pasting it onto a fixed prefix it owns, so the set of
-- hosts this product can ever link to is a list in the source, not a column in
-- the database.
--
-- The per-kind patterns below are that rule made enforceable. They are POSIX
-- regexes (no lookahead), each the platform's own handle grammar reduced to what
-- can be stated in one: a colon, a slash, a dot-dot or a space cannot appear in
-- ANY of them, which is what makes "a full URL was pasted in" a constraint
-- violation rather than a broken link on a public page.
CREATE TABLE user_links (
    user_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    kind    text NOT NULL CHECK (kind IN ('github', 'youtube', 'twitch')),
    handle  text NOT NULL,

    PRIMARY KEY (user_id, kind),

    CONSTRAINT user_links_handle_matches_kind CHECK (
        CASE kind
            -- GitHub: alphanumerics and single hyphens, 1-39 characters, never
            -- leading or trailing a hyphen. The "no double hyphen" half of
            -- their rule is not expressible without lookahead, so it is not
            -- claimed here — a `foo--bar` handle is a link that 404s, not a
            -- link that goes somewhere else.
            WHEN 'github' THEN handle ~ '^[A-Za-z0-9]([A-Za-z0-9-]{0,37}[A-Za-z0-9])?$'
            -- YouTube handles (the @name form, stored WITHOUT the @): letters,
            -- digits, underscores, hyphens and dots, 3-30 characters.
            WHEN 'youtube' THEN handle ~ '^[A-Za-z0-9._-]{3,30}$'
            -- Twitch logins: letters, digits and underscores, 4-25 characters.
            WHEN 'twitch' THEN handle ~ '^[A-Za-z0-9_]{4,25}$'
            END)
);

-- +goose Down
DROP TABLE user_links;
DROP TABLE user_badges;
ALTER TABLE users
    DROP COLUMN keyboard,
    DROP COLUMN bio;
