-- +goose Up
-- Seeded repeats: a run whose TEXT was taken from another run is saved, is
-- visible in its player's history, and is ranked nowhere.
--
-- The rule is about the ORIGIN OF THE TEXT and about nothing else. It is NOT
-- "there was a ghost on screen", and the difference is the whole point:
--
--   * A pace caret running at your PB's speed over a FRESHLY GENERATED text is
--     an ordinary run. You have never seen those words. Losing to the caret,
--     on wpm or on score, changes nothing — the run goes to history, to its
--     board, to the PB and (when it exists) to TP, exactly like a run played
--     with nothing on screen at all. The opponent is decoration.
--   * A run that ADOPTED another run's setup — the "race this run" flow, which
--     applies the target run's seed and word list wholesale — is a different
--     thing. The text is knowable in advance and therefore learnable, so it
--     cannot hold a board slot, a personal best, or a rating point.
--
-- Gating on the opponent would get both of those backwards. Gating on the
-- origin gets both right, and it keeps working for any future surface that
-- shows a caret, because a caret is not what is being asked about.
--
-- NOTE FOR WHOEVER WRITES TP. Migration 00009's header tells you to count
-- `WHERE quote_id IS NULL`. That instruction is DEAD — a quote run now earns TP
-- exactly as a seeded run does, with no exception and no coefficient (see
-- docs/LEADERBOARDS.md, "TP does not exist yet"). The runs TP may count are the
-- whole of leaderboard_eligible_runs and nothing less, which is also how it
-- inherits the exclusion below for free. 00009 is left as written because an
-- applied migration is history, not documentation.
--
-- The claim is the CLIENT's, and it is only ever used to withhold credit. That
-- is the safe direction for an unverifiable field: lying about it can cost a
-- player their own board slot and can never win them one. (The opposite field —
-- "this run is fresh, honest" — would be worth forging, which is exactly why
-- the flag is spelled as the burden rather than as the permission.)

-- +goose StatementBegin
-- The run this run's text was taken from, or NULL when the text was generated
-- fresh. NULL is therefore the default for every row ever written, which is what
-- lets this land without a backfill and without a wire version.
--
-- The uuid regex is the same guard run_quote_id carries and for the same reason:
-- this function is evaluated inside the replay worker's verdict transaction, and
-- `'nonsense'::uuid` raises rather than returning NULL — one hand-edited setup
-- document would abort the batch that wrote it. A malformed value resolves to
-- NULL here, i.e. the run is treated as fresh; ingestion refuses the same
-- document up front (422 invalid_adopted_from), so a row can only carry a
-- malformed value if it was written around the API.
CREATE FUNCTION run_adopted_from(setup jsonb) RETURNS uuid
    LANGUAGE sql IMMUTABLE STRICT PARALLEL SAFE AS $$
    SELECT CASE
        WHEN setup ->> 'adoptedFromRunId' ~
             '^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$'
        THEN (setup ->> 'adoptedFromRunId')::uuid
    END
$$;
-- +goose StatementEnd

-- The view is replaced rather than patched: it gains a WHERE clause, and
-- CREATE OR REPLACE cannot express the Down side of that symmetrically.
--
-- Placed HERE, in leaderboard_eligible_runs, for the reason 00009 gives about
-- the quote rule: the projection, the rebuild and every read go through this one
-- view, so a run that is not eligible cannot be filed by one path and skipped by
-- another. It also means the exclusion is a property of the RUN and not of the
-- board shape — a seeded repeat and a quote repeat are refused by the same line.
DROP VIEW leaderboard_eligible_runs;

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
         LEFT JOIN LATERAL (SELECT run_quote_id(r.setup) AS quote_id) c ON true
         LEFT JOIN quotes q ON q.id = c.quote_id
WHERE r.status = 'accepted'
  -- A seeded repeat holds no slot on any board, of either shape. It is still
  -- accepted, still judged, still stored, still listed in the player's own
  -- history — it simply does not compete. TP, when it exists, must read this
  -- same view (or repeat this same predicate) for the same reason.
  AND run_adopted_from(r.setup) IS NULL
  AND CASE WHEN q.id IS NOT NULL
      THEN true
      ELSE run_text_source_kind(r.setup) = 'seeded'
           AND ((r.mode = 'time'  AND r.duration_ms IN (15000, 30000, 60000))
             OR (r.mode = 'words' AND r.word_count  IN (25, 50, 100)))
      END
  AND jsonb_typeof(r.server_score -> 'total') = 'number'
  AND jsonb_typeof(r.server_metrics -> 'wpm') = 'number'
  AND jsonb_typeof(r.server_metrics -> 'raw') = 'number'
  AND jsonb_typeof(r.server_metrics -> 'accuracy') = 'number';

-- +goose Down
DROP VIEW leaderboard_eligible_runs;

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
         LEFT JOIN LATERAL (SELECT run_quote_id(r.setup) AS quote_id) c ON true
         LEFT JOIN quotes q ON q.id = c.quote_id
WHERE r.status = 'accepted'
  AND CASE WHEN q.id IS NOT NULL
      THEN true
      ELSE run_text_source_kind(r.setup) = 'seeded'
           AND ((r.mode = 'time'  AND r.duration_ms IN (15000, 30000, 60000))
             OR (r.mode = 'words' AND r.word_count  IN (25, 50, 100)))
      END
  AND jsonb_typeof(r.server_score -> 'total') = 'number'
  AND jsonb_typeof(r.server_metrics -> 'wpm') = 'number'
  AND jsonb_typeof(r.server_metrics -> 'raw') = 'number'
  AND jsonb_typeof(r.server_metrics -> 'accuracy') = 'number';

DROP FUNCTION run_adopted_from(jsonb);
