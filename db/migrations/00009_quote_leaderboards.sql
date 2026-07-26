-- +goose Up
-- Per-quote leaderboards (SCORING_CONCEPT §6, docs/LEADERBOARDS.md, docs/QUOTES.md).
--
-- A quote is a fixed map: everyone types the same bytes, so a quote score means
-- something next to other scores ON THAT QUOTE and nothing at all next to a
-- seeded one. SCORING_CONCEPT §6 gives three reasons and they are not stylistic:
-- a finite corpus is memorisable (burst above real skill), quotes run 8–100+
-- words so they do not fit the ranked sizes at all, and a global rating fed by
-- quotes becomes a search for easy maps. Policy: RANKED WITHIN THE QUOTE,
-- UNRANKED GLOBALLY.
--
-- That policy is enforced HERE, in leaderboard_eligible_runs, and nowhere else.
-- The projection, the rebuild and every read go through this view (00006), so
-- putting the rule in the view is what makes it impossible for the three of them
-- to drift into three different answers — and it is why a bug in Go can at worst
-- recompute the WRONG cell, never file a run into a board the view would not
-- have put it in.

-- +goose StatementBegin
-- The quote a run was played on, or NULL for a seeded run.
--
-- The client submits setup.generation.textSource = {kind, quoteId, quoteHash}
-- and deliberately NOT the text (docs/QUOTES.md); the id is the only handle, and
-- this is where SQL picks it up.
--
-- The uuid regex is a guard, not decoration: this view is evaluated inside the
-- replay worker's verdict transaction, and `'nonsense'::uuid` raises rather than
-- returning NULL — one hand-edited setup document would abort the batch that
-- wrote it. It is the same reasoning as the jsonb_typeof guards below. A run
-- that claims a quote it cannot name resolves to NULL here and is then ranked
-- NOWHERE: it fails the quote branch (no quote) and the seeded branch (kind is
-- not 'seeded'), which is the safe direction to fail in.
CREATE FUNCTION run_quote_id(setup jsonb) RETURNS uuid
    LANGUAGE sql IMMUTABLE STRICT PARALLEL SAFE AS $$
    SELECT CASE
        WHEN setup -> 'generation' -> 'textSource' ->> 'kind' = 'quote'
         AND setup -> 'generation' -> 'textSource' ->> 'quoteId' ~
             '^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$'
        THEN (setup -> 'generation' -> 'textSource' ->> 'quoteId')::uuid
    END
$$;
-- +goose StatementEnd

-- The quote's attribution, snapshotted onto the row that shows it. NULL on every
-- language board, where there is no quote to attribute.
--
-- It is a column rather than a join in leaderboard_rows because the reads must
-- not pay for it: a board page is one index range scan plus the display-name
-- join (docs/PERFORMANCE.md zone 3), and adding a second join to serve a field
-- that is NULL for every seeded row would charge every language board for a
-- feature it does not have. The value is fetched ONCE, by the projection, for
-- the one run that wins the cell — see RecomputeLeaderboardCell.
--
-- Snapshotting it is safe in a way that snapshotting most things is not: a
-- published quote is never edited (docs/QUOTES.md, "Immutability"), so this
-- string cannot go stale behind the board.
ALTER TABLE leaderboard_entries ADD COLUMN quote_source text;

-- Both views are replaced, not patched: leaderboard_eligible_runs gains a
-- column and changes what several existing ones mean, and CREATE OR REPLACE
-- cannot express the Down side of that symmetrically.
DROP VIEW leaderboard_rows;
DROP VIEW leaderboard_eligible_runs;

-- Which runs may hold a slot, and which cell each one is a candidate for.
--
-- `quote_id` is the new coordinate and the whole of the quote policy. It is
-- non-NULL for exactly the runs that were played on a quote, and it is what
-- RecomputeLeaderboardCell matches on, so:
--
--   * a quote run can never enter a language board — a language cell asks for
--     `quote_id IS NULL` and a quote run does not answer to it; and
--   * a seeded run can never enter a quote board — a quote cell asks for one
--     specific quote id and a seeded run has none.
--
-- Neither exclusion is a WHERE clause someone has to remember to repeat: there
-- is one statement that writes leaderboard_entries and it matches on this
-- column, so both directions hold for the projection, for the rebuild and for
-- anything else that is ever taught to read this view.
--
-- TP (SCORING_CONCEPT §5) does not exist yet, and this is the hook it must use
-- when it does: the runs it may count are exactly `WHERE quote_id IS NULL`.
-- Quotes are out of TP for the same three reasons they are out of the global
-- boards — memorisation, length variance, cherry-picking easy maps — and the
-- column is here, named, so that arriving TP has to say something about quotes
-- rather than inherit them by writing `SELECT … FROM leaderboard_eligible_runs`
-- and not noticing.
--
-- Still not filtered here, unchanged from 00006:
--   * bans           — enforced at READ time (leaderboard_rows).
--   * verified email — deployment policy, passed per query by the caller. Both
--                      apply to quote boards exactly as they do to language
--                      ones, precisely because both sit outside this view.
CREATE VIEW leaderboard_eligible_runs AS
SELECT r.id      AS run_id,
       r.user_id AS user_id,
       r.mode,
       r.duration_ms,
       r.word_count,
       r.lang,
       run_text_source_kind(r.setup)              AS text_source_kind,
       q.id                                       AS quote_id,
       (r.server_score ->> 'total')::bigint       AS score,
       (r.server_metrics ->> 'wpm')::numeric      AS wpm,
       (r.server_metrics ->> 'raw')::numeric      AS raw,
       (r.server_metrics ->> 'accuracy')::numeric AS acc,
       run_mods(r.setup)                          AS mods,
       r.created_at                               AS achieved_at
FROM runs r
         -- LATERAL, so run_quote_id runs once per row rather than once per
         -- reference to it.
         LEFT JOIN LATERAL (SELECT run_quote_id(r.setup) AS quote_id) c ON true
         -- The quote id a board is keyed by is a RESOLVED one. Taking it from
         -- `quotes` rather than from the setup document is what makes "this
         -- names a real quote" part of the coordinate instead of a separate
         -- check somebody has to repeat: an id that resolves to nothing is
         -- simply not a quote coordinate, and the run falls through to the
         -- seeded branch below, which it also fails. Ranked nowhere.
         --
         -- RunBucketCell resolves it through the same join, which is what keeps
         -- the per-verdict path and the rebuild agreeing about which cell a run
         -- belongs to (TestRebuildReproducesIncrementalMaintenance plants an
         -- accepted run naming a quote that is not in the registry, precisely
         -- to hold that agreement).
         LEFT JOIN quotes q ON q.id = c.quote_id
WHERE r.status = 'accepted'
  -- Flags do NOT disqualify: policy v1 accepts runs that raised a weak
  -- plausibility signal, and that acceptance is the verdict (docs/REPLAY.md).
  AND CASE WHEN q.id IS NOT NULL
      -- A quote run is ranked on its quote, at whatever length that quote is.
      -- There is no shape test, because the ranked sizes are meaningless
      -- against a corpus that runs 8 to 100+ words (SCORING_CONCEPT §6) — and
      -- a quote run carries neither duration_ms nor word_count to test.
      THEN true
      -- A seeded run is ranked in its language board, and only at a ranked
      -- shape. 10min is deliberately absent: sitting out ten minutes is
      -- endurance, and the sample is too small to rank (SCORING_CONCEPT §4).
      -- `= 'seeded'` is also what keeps out a run that CLAIMS a quote it cannot
      -- name: either the id was unparseable or it resolves to no row, so the
      -- run lands in this branch, and its text source is not 'seeded'.
      ELSE run_text_source_kind(r.setup) = 'seeded'
           AND ((r.mode = 'time'  AND r.duration_ms IN (15000, 30000, 60000))
             OR (r.mode = 'words' AND r.word_count  IN (25, 50, 100)))
      END
  -- The jsonb_typeof guards are not paranoia: this view is evaluated inside the
  -- replay worker's transaction, and a malformed verdict document must not be
  -- able to abort the batch that wrote it.
  AND jsonb_typeof(r.server_score -> 'total') = 'number'
  AND jsonb_typeof(r.server_metrics -> 'wpm') = 'number'
  AND jsonb_typeof(r.server_metrics -> 'raw') = 'number'
  AND jsonb_typeof(r.server_metrics -> 'accuracy') = 'number';

-- What a reader may see. EVERY read goes through this view, which is how ban
-- filtering stays impossible to forget: there is no other way to reach the
-- entries table from a query. quote_source rides along as a plain column, so a
-- quote board page costs exactly what a language board page costs.
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
       e.achieved_at,
       e.quote_source
FROM leaderboard_entries e
         JOIN users u ON u.id = e.user_id
WHERE NOT EXISTS (SELECT 1 FROM active_bans b WHERE b.user_id = e.user_id);

-- +goose Down
DROP VIEW leaderboard_rows;
DROP VIEW leaderboard_eligible_runs;
ALTER TABLE leaderboard_entries DROP COLUMN quote_source;
DROP FUNCTION run_quote_id(jsonb);

CREATE VIEW leaderboard_eligible_runs AS
SELECT r.id      AS run_id,
       r.user_id AS user_id,
       r.mode,
       r.duration_ms,
       r.word_count,
       r.lang,
       run_text_source_kind(r.setup)              AS text_source_kind,
       (r.server_score ->> 'total')::bigint       AS score,
       (r.server_metrics ->> 'wpm')::numeric      AS wpm,
       (r.server_metrics ->> 'raw')::numeric      AS raw,
       (r.server_metrics ->> 'accuracy')::numeric AS acc,
       run_mods(r.setup)                          AS mods,
       r.created_at                               AS achieved_at
FROM runs r
WHERE r.status = 'accepted'
  AND run_text_source_kind(r.setup) = 'seeded'
  AND ((r.mode = 'time'  AND r.duration_ms IN (15000, 30000, 60000))
    OR (r.mode = 'words' AND r.word_count  IN (25, 50, 100)))
  AND jsonb_typeof(r.server_score -> 'total') = 'number'
  AND jsonb_typeof(r.server_metrics -> 'wpm') = 'number'
  AND jsonb_typeof(r.server_metrics -> 'raw') = 'number'
  AND jsonb_typeof(r.server_metrics -> 'accuracy') = 'number';

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
