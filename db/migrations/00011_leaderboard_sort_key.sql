-- +goose Up
--
-- Honest keyset pagination for the boards (docs/PERFORMANCE.md zone 3).
--
-- The problem this fixes, measured. The ranking order is
-- (score DESC, achieved_at ASC, user_id ASC) — mixed directions — so the
-- continuation could only be spelled as an OR of three conjunctions. A btree
-- can start a scan from a row comparison but not from an OR-of-conjunctions, so
-- every page re-walked the bucket from rank 1 and threw away everything before
-- the cursor. Page 1 and page 1001 had the IDENTICAL plan and differed only in
-- what they discarded: 0 rows removed by filter and 53 buffer hits against
-- 50 092 rows removed and 39 482 buffer hits. Keyset paging exists so depth
-- costs nothing; as implemented, depth cost 18×.
--
-- The fix is to make the whole ordering single-direction by packing it into one
-- signed integer, so a btree can be told where to start:
--
--     sort_key = (score << 32) - floor(epoch(achieved_at))
--
-- Higher score dominates because it occupies the high bits. Within one score,
-- SUBTRACTING the epoch makes an earlier run sort higher — which is
-- `achieved_at ASC`, the tiebreak the spec asks for, with no second direction
-- to reconcile. Ordering by (sort_key DESC, user_id ASC) is therefore exactly
-- (score DESC, achieved_at ASC, user_id ASC), and
-- TestSortKeyOrderingEqualsTheLegacyOrdering asserts that on random data rather
-- than leaving it as an argument.
--
-- Why the score has to be bounded, and why loudly. The packing only works while
-- the two fields do not collide: the epoch must fit in the low 32 bits (it does
-- until 2106) and the score must not overflow bigint when shifted. score < 2^24
-- gives 2^24 << 32 = 2^56, seven bits of headroom under 2^63, and it is ~10×
-- the largest score this system has produced (~1.6M). A CHECK is the right
-- place for it because the failure it prevents is silent: a score at 2^31 would
-- shift into the sign bit and rank a player LAST instead of first. A run that
-- somehow scored that high must fail its insert and be looked at, not be
-- quietly mis-ranked.
ALTER TABLE leaderboard_entries
    ADD CONSTRAINT leaderboard_entries_score_packable
        CHECK (score >= 0 AND score < 16777216);

-- The epoch is taken by subtracting a fully-qualified UTC literal rather than
-- with `extract(epoch from achieved_at)`. The latter is STABLE, not IMMUTABLE
-- (it depends on the session time zone for a timestamptz), and Postgres refuses
-- a generated column built on it. Subtracting a timestamptz from a timestamptz
-- yields an interval, and the epoch of an interval is a pure function of it.
ALTER TABLE leaderboard_entries
    ADD COLUMN sort_key bigint
        GENERATED ALWAYS AS (
            (score << 32)
                - floor(extract(epoch from (achieved_at - '1970-01-01 00:00:00+00'::timestamptz)))::bigint
            ) STORED;

-- The ranking index, now over the packed key.
--
-- achieved_at is still in it, AFTER sort_key, and that is not redundancy. The
-- packing floors the epoch to whole SECONDS, so two runs with the same score in
-- the same second collapse to one sort_key — and the schema's tie rule is that
-- "ties are broken by who got there first", to the microsecond the column
-- stores. Dropping achieved_at here would have re-ordered those players by
-- user_id, which is a silent rank change for anyone who shares a score and a
-- second with somebody else. Measured on a 400-row fixture built to collide:
-- 56 rows would have moved.
--
-- It costs nothing that matters. sort_key is still the column the scan descends
-- on, so the continuation is still a start condition; achieved_at and user_id
-- only order rows the scan has already reached, inside a single sort_key. And
-- keeping them here is what lets a count of better positions stay index-only.
CREATE INDEX leaderboard_sort_idx
    ON leaderboard_entries (bucket_key, sort_key DESC, achieved_at ASC, user_id ASC);

-- The old index is what the old three-way predicate needed and nothing reads it
-- any more. Leaving it would be write cost on every projection for no read.
DROP INDEX leaderboard_rank_idx;

-- Splitting the read view, which is docs/PERFORMANCE.md zone 3 finding 2.
--
-- `/me` is a count of better positions. It needs the ban filter and it does not
-- need a display name, but leaderboard_rows carries the users join, so at depth
-- the planner hash-joined the entire 120 000-row users table into a query that
-- counts rows and selects no columns. Splitting the view lets the count read a
-- relation that leaderboard_sort_idx covers completely — an index-only scan.
--
-- The "there is no other way to reach the table" property is preserved: the ban
-- filter is still the only door, it now simply has two handles on it.
DROP VIEW leaderboard_rows;

CREATE VIEW leaderboard_ranked AS
SELECT e.bucket_key,
       e.user_id,
       e.run_id,
       e.score,
       e.wpm,
       e.raw,
       e.acc,
       e.grade,
       e.mods,
       e.achieved_at,
       e.quote_source,
       e.sort_key
FROM leaderboard_entries e
WHERE NOT EXISTS (SELECT 1 FROM active_bans b WHERE b.user_id = e.user_id);

CREATE VIEW leaderboard_rows AS
SELECT r.bucket_key,
       r.user_id,
       u.display_name,
       r.run_id,
       r.score,
       r.wpm,
       r.raw,
       r.acc,
       r.grade,
       r.mods,
       r.achieved_at,
       r.quote_source,
       r.sort_key
FROM leaderboard_ranked r
         JOIN users u ON u.id = r.user_id;

-- +goose StatementBegin
-- The cursor, in the packed coordinate. A client's cursor is still
-- (score, achieved_at, user_id) — the wire format does not change — so the
-- server packs it here, with exactly the expression the generated column uses.
-- Two spellings of one packing is how a start condition comes to disagree with
-- the order it is starting in, so there is one function and the column is
-- defined by it in the comment above.
CREATE FUNCTION leaderboard_sort_key(score bigint, achieved_at timestamptz)
    RETURNS bigint
    LANGUAGE sql
    IMMUTABLE
    PARALLEL SAFE
AS $$
SELECT (score << 32)
           - floor(extract(epoch from (achieved_at - '1970-01-01 00:00:00+00'::timestamptz)))::bigint
$$;
-- +goose StatementEnd

-- +goose Down
DROP VIEW leaderboard_rows;
DROP VIEW leaderboard_ranked;
DROP FUNCTION leaderboard_sort_key(bigint, timestamptz);

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

CREATE INDEX leaderboard_rank_idx
    ON leaderboard_entries (bucket_key, score DESC, achieved_at ASC, user_id ASC);
DROP INDEX leaderboard_sort_idx;
ALTER TABLE leaderboard_entries DROP COLUMN sort_key;
ALTER TABLE leaderboard_entries DROP CONSTRAINT leaderboard_entries_score_packable;
