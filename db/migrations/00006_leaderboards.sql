-- +goose Up
-- Bucketed score leaderboards (SCORING_CONCEPT §4, docs/LEADERBOARDS.md).
--
-- The board is a PROJECTION of accepted runs, not a second source of truth:
-- every row here can be recomputed from `runs` alone (`make rebuild-leaderboards`).
-- BACKEND.md §0/§10 pencilled in a Redis ZSET for this read model; it is
-- deliberately not used yet — see docs/LEADERBOARDS.md, "Why no Redis".
--
-- Everything below that a query is allowed to depend on is defined ONCE:
--   run_grade()                 the letter grade of an accuracy
--   run_text_source_kind()      where a run's text came from
--   run_mods()                  the mod flags a run was played under
--   active_bans                 which bans are in force right now
--   leaderboard_eligible_runs   which runs may hold a board slot
--   leaderboard_rows            which entries are visible to a reader
-- The projection, the rebuild and every read go through these, so "eligible"
-- and "visible" cannot drift apart between code paths.

-- Moderation bans (BACKEND.md §11). Only the table and the read-side filter
-- land in this phase: a leaderboard without ban filtering is not something you
-- can safely retrofit, while the admin surface that ISSUES bans can wait.
-- A banned player keeps their entries (see leaderboard_rows) so an unban is
-- instant and needs no rebuild.
CREATE TABLE bans (
    user_id    uuid PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    reason     text        NOT NULL,
    -- The moderator who issued it. NULL once that account is deleted: losing
    -- the issuer must never drop the ban itself.
    issued_by  uuid        REFERENCES users (id) ON DELETE SET NULL,
    issued_at  timestamptz NOT NULL DEFAULT now(),
    -- NULL = permanent.
    expires_at timestamptz
);

-- The single definition of "banned right now". Every read path filters through
-- this view rather than repeating the expiry predicate.
CREATE VIEW active_bans AS
SELECT user_id
FROM bans
WHERE expires_at IS NULL OR expires_at > now();

-- +goose StatementBegin
-- Letter grade by accuracy (SCORING_CONCEPT §4). This mirrors the core's
-- gradeOf() in shared/core/score.ts, and it is the ONE place the server states
-- the mapping: the projection, the rebuild and the public replay endpoint all
-- call it. The duplication with the TS core is fenced by
-- TestGradeMatchesTheCore, which drives the real vendored bundle in goja and
-- compares it against this function across the threshold boundaries — move a
-- threshold in the core and CI goes red instead of the boards going quietly
-- wrong.
CREATE FUNCTION run_grade(acc numeric) RETURNS text
    LANGUAGE sql IMMUTABLE STRICT PARALLEL SAFE AS $$
    SELECT CASE
        WHEN acc >= 1    THEN 'SS'
        WHEN acc >= 0.98 THEN 'S'
        WHEN acc >= 0.95 THEN 'A'
        WHEN acc >= 0.9  THEN 'B'
        ELSE 'C'
    END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- Where a run's text came from. Today's client generates every text from the
-- seed and sends no textSource at all, so an absent field means 'seeded';
-- BACKEND.md §8's seeded|fixed abstraction will populate it when quotes land.
-- The default is safe rather than trusting: a run whose text did not come from
-- the seed cannot survive replay in the first place, because the worker
-- regenerates the words from seed + dictionary before judging the log.
CREATE FUNCTION run_text_source_kind(setup jsonb) RETURNS text
    LANGUAGE sql IMMUTABLE STRICT PARALLEL SAFE AS $$
    SELECT coalesce(setup -> 'generation' -> 'textSource' ->> 'kind', 'seeded')
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- The mod flags a run was played under, lifted out of the setup snapshot into a
-- flat object a leaderboard row can render without parsing the whole config.
--
-- This is field SELECTION, not scoring: the multipliers and which combination
-- counts as what stay in the core (shared/core/mods.ts), and the score column
-- already has them folded in. TestModsProjectionCoversEveryCoreMod asserts this
-- list still covers every mod the core knows about.
CREATE FUNCTION run_mods(setup jsonb) RETURNS jsonb
    LANGUAGE sql IMMUTABLE STRICT PARALLEL SAFE AS $$
    SELECT jsonb_build_object(
        'punctuation', coalesce(setup -> 'generation' -> 'punctuation', 'false'::jsonb),
        'numbers',     coalesce(setup -> 'generation' -> 'numbers',     'false'::jsonb),
        'randomCase',  coalesce(setup -> 'generation' -> 'randomCase',  'false'::jsonb),
        'reverse',     coalesce(setup -> 'generation' -> 'reverse',     'false'::jsonb),
        'nospace',     coalesce(setup -> 'config'     -> 'nospace',     'false'::jsonb),
        'difficulty',  coalesce(setup -> 'config'     -> 'difficulty',  '"normal"'::jsonb),
        'minWpm',      coalesce(setup -> 'config'     -> 'minWpm',      '0'::jsonb),
        'blind',       coalesce(setup -> 'declaration' -> 'blind',      'false'::jsonb),
        'fading',      coalesce(setup -> 'declaration' -> 'fading',     'false'::jsonb),
        'flashlight',  coalesce(setup -> 'declaration' -> 'flashlight', 'false'::jsonb))
$$;
-- +goose StatementEnd

-- One row per player per bucket: their best eligible run there.
--
-- The columns are a SNAPSHOT of that run, not a join to it. A board page is one
-- index range scan plus the display-name join, and a run row that later loses
-- its accepted status is removed by the projection rather than silently
-- changing what the board shows.
CREATE TABLE leaderboard_entries (
    -- "<mode>:<durationMs|wordCount>:<lang>:<textSource.kind>", formatted by
    -- exactly one function server-side (leaderboard.Bucket.Key).
    bucket_key  text        NOT NULL,
    user_id     uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    run_id      uuid        NOT NULL REFERENCES runs (id) ON DELETE CASCADE,

    -- The server's own numbers, never the client's.
    score       bigint      NOT NULL,
    wpm         numeric     NOT NULL,
    raw         numeric     NOT NULL,
    acc         numeric     NOT NULL,
    grade       text        NOT NULL,
    -- run_mods(setup) of the entry's run.
    mods        jsonb       NOT NULL,
    -- When the run was played (runs.created_at), NOT when it was projected:
    -- ties are broken by who got there first.
    achieved_at timestamptz NOT NULL,

    PRIMARY KEY (bucket_key, user_id)
);

-- The ranking scan: one bucket, best first, earliest first on a tie. user_id is
-- appended to the spec's (bucket_key, score DESC, achieved_at ASC) so the
-- ordering is TOTAL — keyset pagination needs a unique final tiebreak, and
-- having it in the index keeps the page scan ordered rather than sorted.
CREATE INDEX leaderboard_rank_idx
    ON leaderboard_entries (bucket_key, score DESC, achieved_at ASC, user_id ASC);

-- A run can hold at most one slot anywhere. It is implied (a run belongs to one
-- player and one bucket), so the index is here to make a projection bug fail
-- loudly instead of duplicating a player across boards.
CREATE UNIQUE INDEX leaderboard_entries_run_idx ON leaderboard_entries (run_id);

-- Which runs may hold a slot. The projection and the rebuild both read this, so
-- there is one answer to "is this run eligible" (docs/LEADERBOARDS.md).
--
-- Not filtered here, on purpose:
--   * bans          — enforced at READ time (leaderboard_rows) so an unban is
--                     instant and needs no rebuild.
--   * verified email — a deployment policy, passed per query by the caller.
--
-- The jsonb_typeof guards are not paranoia: this view is evaluated inside the
-- replay worker's transaction, and a malformed verdict document must not be
-- able to abort the batch that wrote it.
CREATE VIEW leaderboard_eligible_runs AS
SELECT r.id      AS run_id,
       r.user_id AS user_id,
       r.mode,
       r.duration_ms,
       r.word_count,
       r.lang,
       run_text_source_kind(r.setup)            AS text_source_kind,
       (r.server_score ->> 'total')::bigint     AS score,
       (r.server_metrics ->> 'wpm')::numeric    AS wpm,
       (r.server_metrics ->> 'raw')::numeric    AS raw,
       (r.server_metrics ->> 'accuracy')::numeric AS acc,
       run_mods(r.setup)                        AS mods,
       r.created_at                             AS achieved_at
FROM runs r
WHERE r.status = 'accepted'
  -- Flags do NOT disqualify: policy v1 accepts runs that raised a weak
  -- plausibility signal, and that acceptance is the verdict (docs/REPLAY.md).
  AND run_text_source_kind(r.setup) = 'seeded'
  -- Ranked shapes only (SCORING_CONCEPT §4). 10min is deliberately absent:
  -- sitting out ten minutes is endurance, and the sample is too small to rank.
  AND ((r.mode = 'time'  AND r.duration_ms IN (15000, 30000, 60000))
    OR (r.mode = 'words' AND r.word_count  IN (25, 50, 100)))
  AND jsonb_typeof(r.server_score -> 'total') = 'number'
  AND jsonb_typeof(r.server_metrics -> 'wpm') = 'number'
  AND jsonb_typeof(r.server_metrics -> 'raw') = 'number'
  AND jsonb_typeof(r.server_metrics -> 'accuracy') = 'number';

-- What a reader may see. EVERY read goes through this view, which is how ban
-- filtering stays impossible to forget: there is no other way to reach the
-- entries table from a query.
CREATE VIEW leaderboard_rows AS
SELECT e.bucket_key,
       e.user_id,
       u.display_name,
       e.run_id,
       e.score,
       e.wpm,
       e.raw,
       e.acc,
       e.grade,
       e.mods,
       e.achieved_at
FROM leaderboard_entries e
         JOIN users u ON u.id = e.user_id
WHERE NOT EXISTS (SELECT 1 FROM active_bans b WHERE b.user_id = e.user_id);

-- +goose Down
DROP VIEW leaderboard_rows;
DROP VIEW leaderboard_eligible_runs;
DROP TABLE leaderboard_entries;
DROP FUNCTION run_mods(jsonb);
DROP FUNCTION run_text_source_kind(jsonb);
DROP FUNCTION run_grade(numeric);
DROP VIEW active_bans;
DROP TABLE bans;
