-- Bucketed score leaderboards (docs/LEADERBOARDS.md).
--
-- Reads go through leaderboard_rows (ban-filtered by construction); writes go
-- through RecomputeLeaderboardCell, which is the ONLY statement that mutates
-- the table — promotion, demotion and rebuild are all the same operation.

-- name: RunBucketCell :one
-- The cell a run belongs to, resolved WITHOUT looking at its status: a run that
-- just lost its accepted status still has to say which cell to recompute.
--
-- These are the same coordinates leaderboard_eligible_runs carries, read
-- straight off the run: which of them name the cell is Bucket's decision (a
-- quote board has only its quote), and the projection and the rebuild agree
-- because they build the bucket through the same constructor.
--
-- The quote id is resolved through `quotes` rather than lifted out of the setup
-- document, so an id that names nothing yields NULL here exactly as it does in
-- the eligible view — one row, one primary-key probe, on a query that already
-- runs once per verdict.
SELECT r.user_id, r.mode, r.duration_ms, r.word_count, r.lang,
       run_text_source_kind(r.setup)::text AS text_source_kind,
       q.id AS quote_id
FROM runs r
         LEFT JOIN quotes q ON q.id = run_quote_id(r.setup)
WHERE r.id = @run_id;

-- name: RecomputeLeaderboardCell :exec
-- Set one (bucket, player) cell to that player's best eligible run, or clear it
-- when nothing is left. This single statement covers every case:
--
--   new personal best      the ORDER BY picks the new run
--   worse run submitted    the ORDER BY keeps the old one, DO UPDATE no-ops
--   run demoted            it drops out of leaderboard_eligible_runs and the
--                          next best takes the slot
--   only entry demoted     `best` is empty, so the DELETE fires
--
-- Writing it as "recompute the cell" rather than "upsert if better" is what
-- makes demotion correct for free, and it is why a rebuild can reuse this
-- statement verbatim instead of reimplementing the rule.
--
-- Siblings are matched on the bucket COMPONENTS, never by parsing bucket_key:
-- the key's format has exactly one producer, and it is in Go.
--
-- quote_id is the first component and it decides which of the others count. A
-- quote board has no mode, no size, no language and no text-source dimension —
-- the quote IS the dimension (SCORING_CONCEPT §6) — so for a quote cell the
-- language coordinates are not merely equal-to-something, they are NOT ASKED.
-- That is what makes the two exclusions structural rather than remembered:
--
--   language cell  quote_id IS NULL      → a quote run cannot be a sibling
--   quote cell     quote_id = <that one> → a seeded run cannot be a sibling
--
-- and both hold for the projection AND the rebuild, because this is the only
-- statement either of them writes through.
--
-- The quote's attribution is resolved ONCE, by the LEFT JOIN below, for the
-- single row that won the cell: not per candidate run, and never per read. On a
-- language board b.quote_id is NULL, the index probe matches nothing and the
-- column stays NULL, which is why no language board pays for the feature. LEFT
-- rather than inner because of THAT case, not because the quote might be
-- missing: a language cell has no quote id at all, and an inner join would drop
-- every language board's row. On a quote cell the row is guaranteed — the
-- eligible view resolves quote_id through `quotes`, so a candidate exists only
-- if its quote does, which is what makes `source` non-optional on a quote board
-- rather than merely usually present.
--
WITH best AS (
    SELECT e.*
    FROM leaderboard_eligible_runs e
    WHERE e.user_id = @user_id
      AND e.quote_id IS NOT DISTINCT FROM sqlc.narg(quote_id)::uuid
      AND (e.quote_id IS NOT NULL
           OR (e.mode = @mode
               AND e.duration_ms IS NOT DISTINCT FROM sqlc.narg(duration_ms)::int
               AND e.word_count IS NOT DISTINCT FROM sqlc.narg(word_count)::int
               AND e.lang = @lang
               AND e.text_source_kind = @text_source_kind))
      -- The economic barrier against throwaway accounts. Off by config, and
      -- flipping it needs a rebuild to take effect on existing runs.
      --
      -- Note `ai.user_id = @user_id`, NOT `= e.user_id`. The two are the same
      -- value — `e.user_id = @user_id` is asserted five lines above — but
      -- correlating on the column makes this a SubPlan the executor runs once
      -- per CANDIDATE RUN instead of an InitPlan it runs once per statement.
      -- Measured (docs/PERFORMANCE.md, zone 4): a six-run cell 34.6 ms → 8.0 ms,
      -- and a player with 100 000 runs in one bucket 1 m 30 s → 458 ms, which is
      -- the gate costing nothing at all. This projection runs inside the replay
      -- worker's verdict transaction, holding its row locks, so the difference
      -- is not academic.
      AND (NOT @require_verified_email::boolean
           OR EXISTS (SELECT 1 FROM auth_identities ai
                      WHERE ai.user_id = @user_id AND ai.email_verified))
    ORDER BY e.score DESC, e.achieved_at ASC, e.run_id ASC
    LIMIT 1
),
cleared AS (
    DELETE FROM leaderboard_entries le
    WHERE le.bucket_key = @bucket_key
      AND le.user_id = @user_id
      AND NOT EXISTS (SELECT 1 FROM best)
)
INSERT INTO leaderboard_entries
    (bucket_key, user_id, run_id, score, wpm, raw, acc, grade, mods, achieved_at,
     quote_source)
SELECT @bucket_key, b.user_id, b.run_id, b.score, b.wpm, b.raw, b.acc,
       run_grade(b.acc), b.mods, b.achieved_at, q.source
FROM best b
         LEFT JOIN quotes q ON q.id = b.quote_id
ON CONFLICT (bucket_key, user_id) DO UPDATE
    SET run_id      = EXCLUDED.run_id,
        score       = EXCLUDED.score,
        wpm         = EXCLUDED.wpm,
        raw         = EXCLUDED.raw,
        acc         = EXCLUDED.acc,
        grade       = EXCLUDED.grade,
        mods        = EXCLUDED.mods,
        achieved_at = EXCLUDED.achieved_at,
        quote_source = EXCLUDED.quote_source
    -- An unchanged best is left physically alone.
    WHERE leaderboard_entries.run_id <> EXCLUDED.run_id;

-- name: EnumerateLeaderboardCells :many
-- Every (player, bucket) cell that currently has an eligible run. The rebuild
-- walks this and calls RecomputeLeaderboardCell on each, so the rebuilt table is
-- produced by the same statement that maintains it incrementally.
--
-- These are candidate COORDINATES, not board identities: two rows can name the
-- same quote board (the language coordinates ride along and a quote board
-- ignores them). Collapsing coordinates into the board they name is Bucket.Key's
-- job and happens in Go, where the key has its one producer — Rebuild walks
-- each DISTINCT bucket once.
SELECT DISTINCT e.user_id, e.mode, e.duration_ms, e.word_count, e.lang,
                e.text_source_kind, e.quote_id
FROM leaderboard_eligible_runs e
WHERE (NOT @require_verified_email::boolean
       OR EXISTS (SELECT 1 FROM auth_identities ai
                  WHERE ai.user_id = e.user_id AND ai.email_verified))
ORDER BY e.user_id, e.mode, e.duration_ms, e.word_count, e.lang,
         e.text_source_kind, e.quote_id;

-- name: ClearLeaderboard :exec
-- Rebuild step one. TRUNCATE is transactional in Postgres, so a failed rebuild
-- leaves the old board untouched rather than an empty one.
TRUNCATE leaderboard_entries;

-- name: CountLeaderboardEntries :one
SELECT count(*) FROM leaderboard_entries;

-- name: ListLeaderboardBuckets :many
-- The board index. Counts are ban-filtered like every other read, so a bucket
-- whose only player is banned reports zero rather than one.
--
-- Quote boards are in here alongside the language ones, and they are the reason
-- this result is no longer bounded by the schema — see docs/LEADERBOARDS.md.
SELECT bucket_key, count(*)::bigint AS entries
FROM leaderboard_ranked
GROUP BY bucket_key
ORDER BY bucket_key;

-- name: ListLeaderboardPageFirst :many
-- Page one: best score first, earliest achievement first on a tie, user_id as
-- the final tiebreak so the order is total (and therefore pageable).
--
-- The order is expressed over sort_key, which packs (score, achieved_at) into
-- one descending integer — see db/migrations/00011. That is what makes the
-- continuation below a btree start condition instead of a filter.
SELECT user_id, display_name, run_id, score, wpm, raw, acc, grade, mods,
       achieved_at, quote_source
FROM leaderboard_rows
WHERE bucket_key = @bucket_key
ORDER BY sort_key DESC, achieved_at ASC, user_id ASC
LIMIT @row_limit;

-- name: ListLeaderboardPageAfter :many
-- The keyset continuation, as a genuine start condition.
--
-- `sort_key <= cursor` is the part the index can descend to: it lands the scan
-- ON the cursor row instead of at rank 1. The second clause is what excludes
-- the cursor row itself and orders the players who share its exact sort_key —
-- the same score in the same second, ordered by who got there first to the
-- microsecond — and it can only remove rows the scan has
-- already reached, so it is a Filter over a handful of ties rather than over
-- everything above the cursor. Measured on a 20 000-row bucket at depth 10 000:
-- Rows Removed by Filter 1, 53 buffer hits, against 50 092 and 39 482 for the
-- OR-of-conjunctions this replaced.
SELECT user_id, display_name, run_id, score, wpm, raw, acc, grade, mods,
       achieved_at, quote_source
FROM leaderboard_rows
WHERE bucket_key = @bucket_key
  AND sort_key <= leaderboard_sort_key(@score, @achieved_at)
  AND (sort_key < leaderboard_sort_key(@score, @achieved_at)
       OR achieved_at > @achieved_at::timestamptz
       OR (achieved_at = @achieved_at::timestamptz AND user_id > @user_id::uuid))
ORDER BY sort_key DESC, achieved_at ASC, user_id ASC
LIMIT @row_limit;

-- name: CountLeaderboardAbove :one
-- How many visible entries outrank this position. Rank = that + 1, which is what
-- makes the rank on a cursor page exact instead of a guess carried in the token.
--
-- It reads leaderboard_ranked, NOT leaderboard_rows: a count needs the ban
-- filter and does not need a display name, and at depth the planner was
-- hash-joining the whole 120 000-row users table into a query that selects no
-- columns (docs/PERFORMANCE.md zone 3, finding 2). leaderboard_sort_idx covers
-- leaderboard_ranked completely, so this is an index-only scan.
SELECT count(*)::bigint
FROM leaderboard_ranked
WHERE bucket_key = @bucket_key
  AND sort_key >= leaderboard_sort_key(@score, @achieved_at)
  AND (sort_key > leaderboard_sort_key(@score, @achieved_at)
       OR achieved_at < @achieved_at::timestamptz
       OR (achieved_at = @achieved_at::timestamptz AND user_id < @user_id::uuid));

-- name: GetLeaderboardEntry :one
-- One player's entry in one bucket. A banned player has none (the view), which
-- is exactly the 204 the /me endpoint wants.
SELECT user_id, display_name, run_id, score, wpm, raw, acc, grade, mods,
       achieved_at, quote_source
FROM leaderboard_rows
WHERE bucket_key = @bucket_key AND user_id = @user_id;
