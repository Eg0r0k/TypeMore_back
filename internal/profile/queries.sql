-- Profile aggregates (docs/PROFILE.md): on-demand SQL over `runs`, computed at
-- request time, cached nowhere. Every query here is scoped to ONE user and must
-- stay index-driven off runs_user_id_created_at_idx (or, for the PB cards,
-- leaderboard_entries_user_idx) — the 100k-run perf suite pins the plans.
--
-- Two aggregation rules, stated once (and asserted by tests):
--   * METRIC aggregates (wpm/raw/acc/consistency, estimated words) read
--     ACCEPTED runs only — the server-verified numbers in server_metrics,
--     which since 00019 live on the run_verdicts satellite (1:1 by run_id;
--     LEFT JOIN where the base set includes unjudged runs, INNER where
--     status = 'accepted' already implies the verdict row exists). Every join
--     carries the redundant-looking `v.user_id = r.user_id`: it is what lets
--     the planner propagate the $1 equality onto run_verdicts and drive BOTH
--     sides through player-scoped indexes (run_verdicts_user_idx) instead of
--     hashing the whole verdict table — the zone-9 plan pins are the test.
--   * COUNTERS read every submitted run regardless of verdict (a flagged run
--     was still played), and tests_started additionally folds in the
--     client-reported restart counts: started = count(*) + sum(restarts).

-- name: GetProfileUser :one
SELECT display_name, created_at FROM users WHERE id = $1;

-- name: GetProfileCounts :one
-- Counters + the two derived totals. time_typing_ms and estimated_words read
-- server_metrics (accepted only): durationSec is the deadline-pinned duration
-- the core measured, and the words estimate is the raw character volume
-- (correct+incorrect+extra+spaces) over the conventional 5 chars/word.
SELECT count(*)::bigint                                            AS tests_completed,
       coalesce(sum(restarts_since_last_submit), 0)::bigint        AS restarts,
       coalesce(sum((v.server_metrics ->> 'durationSec')::float8 * 1000)
                FILTER (WHERE r.status = 'accepted'), 0)::bigint     AS time_typing_ms,
       coalesce(sum(((v.server_metrics -> 'chars' ->> 'correct')::float8
                   + (v.server_metrics -> 'chars' ->> 'incorrect')::float8
                   + (v.server_metrics -> 'chars' ->> 'extra')::float8
                   + (v.server_metrics ->> 'spaces')::float8) / 5)
                FILTER (WHERE r.status = 'accepted'), 0)::float8     AS estimated_words
FROM runs r
         LEFT JOIN run_verdicts v ON v.run_id = r.id AND v.user_id = r.user_id
WHERE r.user_id = $1;

-- name: GetProfileMetricStats :one
-- highest / average for the four profile metrics, over accepted runs.
SELECT coalesce(max((v.server_metrics ->> 'wpm')::float8), 0)::float8         AS wpm_highest,
       coalesce(avg((v.server_metrics ->> 'wpm')::float8), 0)::float8         AS wpm_average,
       coalesce(max((v.server_metrics ->> 'raw')::float8), 0)::float8         AS raw_highest,
       coalesce(avg((v.server_metrics ->> 'raw')::float8), 0)::float8         AS raw_average,
       coalesce(max((v.server_metrics ->> 'accuracy')::float8), 0)::float8    AS acc_highest,
       coalesce(avg((v.server_metrics ->> 'accuracy')::float8), 0)::float8    AS acc_average,
       coalesce(max((v.server_metrics ->> 'consistency')::float8), 0)::float8 AS consistency_highest,
       coalesce(avg((v.server_metrics ->> 'consistency')::float8), 0)::float8 AS consistency_average
FROM runs r
         JOIN run_verdicts v ON v.run_id = r.id AND v.user_id = r.user_id
WHERE r.user_id = $1 AND r.status = 'accepted';

-- name: GetProfileLast10 :one
-- The same four metrics averaged over the LAST 10 accepted runs by date. The
-- inner query walks (user_id, created_at DESC) newest-first and stops after
-- ten accepted rows — no sort node, the index provides the order.
SELECT coalesce(avg(t.wpm), 0)::float8         AS wpm_average,
       coalesce(avg(t.raw), 0)::float8         AS raw_average,
       coalesce(avg(t.acc), 0)::float8         AS acc_average,
       coalesce(avg(t.consistency), 0)::float8 AS consistency_average
FROM (SELECT (v.server_metrics ->> 'wpm')::float8         AS wpm,
             (v.server_metrics ->> 'raw')::float8         AS raw,
             (v.server_metrics ->> 'accuracy')::float8    AS acc,
             (v.server_metrics ->> 'consistency')::float8 AS consistency
      FROM runs r
               JOIN run_verdicts v ON v.run_id = r.id AND v.user_id = r.user_id
      WHERE r.user_id = $1 AND r.status = 'accepted'
      ORDER BY r.created_at DESC, r.id DESC
      LIMIT 10) t;

-- name: GetProfileStreaks :one
-- Consecutive-day streaks over the days the user PLAYED (runs-based — logins
-- are not tracked, by design; docs/PROFILE.md). Classic gaps-and-islands: a
-- run of consecutive days shares one anchor (day - row_number). `current` is
-- the island still alive relative to the caller-supplied "today" (UTC): its
-- last day is today or yesterday — yesterday keeps the streak alive until the
-- end of today. At most one island can satisfy that predicate, so the FILTER
-- picks the island, not a maximum over several.
WITH days AS (SELECT DISTINCT (created_at AT TIME ZONE 'UTC')::date AS day
              FROM runs WHERE user_id = $1),
     islands AS (SELECT day,
                        day - (row_number() OVER (ORDER BY day))::int AS anchor
                 FROM days),
     lengths AS (SELECT count(*)::int AS len, max(day) AS last_day
                 FROM islands GROUP BY anchor)
SELECT coalesce(max(len), 0)::int                                        AS best,
       coalesce(max(len) FILTER (WHERE last_day >= sqlc.arg(today)::date - 1),
                0)::int                                                   AS current
FROM lengths;

-- name: GetProfileLanguages :one
-- Per-language play counts over every submitted run, as one jsonb document —
-- ordered most-played first.
SELECT (coalesce(jsonb_agg(jsonb_build_object('lang', lang, 'tests', tests)
                           ORDER BY tests DESC, lang), '[]'::jsonb))::jsonb AS languages
FROM (SELECT lang, count(*)::int AS tests
      FROM runs WHERE user_id = $1
      GROUP BY lang) t;

-- name: GetProfileActivity :many
-- Daily buckets for the activity calendar: every submitted run counts as a
-- test that day; the time column reads the server-verified durations that
-- exist (accepted runs). Days with no runs are absent — the calendar renders
-- the gaps itself.
SELECT (r.created_at AT TIME ZONE 'UTC')::date                        AS day,
       count(*)::int                                                AS tests,
       coalesce(sum((v.server_metrics ->> 'durationSec')::float8 * 1000)
                FILTER (WHERE r.status = 'accepted'), 0)::bigint      AS time_ms
FROM runs r
         LEFT JOIN run_verdicts v ON v.run_id = r.id AND v.user_id = r.user_id
WHERE r.user_id = $1 AND r.created_at >= $2
GROUP BY 1
ORDER BY 1;

-- name: GetProfileHistogram :many
-- Tests per 10-wpm bucket over accepted runs; `bucket` is the lower bound
-- (60 covers [60, 70)).
SELECT (floor((v.server_metrics ->> 'wpm')::float8 / 10) * 10)::int AS bucket,
       count(*)::int                                                AS tests
FROM runs r
         JOIN run_verdicts v ON v.run_id = r.id AND v.user_id = r.user_id
WHERE r.user_id = $1 AND r.status = 'accepted'
GROUP BY 1
ORDER BY 1;

-- name: GetProfileTimeseries :many
-- Per-day chart series inside [from, to): time typing (accepted, verified
-- durations), and the day's average wpm / accuracy over accepted runs.
SELECT (r.created_at AT TIME ZONE 'UTC')::date                        AS day,
       coalesce(sum((v.server_metrics ->> 'durationSec')::float8 * 1000)
                FILTER (WHERE r.status = 'accepted'), 0)::bigint      AS time_ms,
       coalesce(avg((v.server_metrics ->> 'wpm')::float8)
                FILTER (WHERE r.status = 'accepted'), 0)::float8      AS avg_wpm,
       coalesce(avg((v.server_metrics ->> 'accuracy')::float8)
                FILTER (WHERE r.status = 'accepted'), 0)::float8      AS avg_acc
FROM runs r
         LEFT JOIN run_verdicts v ON v.run_id = r.id AND v.user_id = r.user_id
WHERE r.user_id = $1 AND r.created_at >= $2 AND r.created_at < $3
GROUP BY 1
ORDER BY 1;

-- name: GetProfileWpmPerHour :one
-- The header stat "speed change per hour spent typing": an ordinary
-- least-squares slope (regr_slope) of y = a day's average wpm over x = the
-- cumulative hours of accepted typing inside the range at that day's end. The
-- points are the DAY buckets, not the raw runs, for two reasons that agree:
-- the chart the stat headlines is a daily series (a trend line through
-- different points than the chart plots would be a different claim), and a
-- windowed running sum over 100k raw rows is a sort that spills to disk at
-- scale, while ≤366 day points sort in a page (the zone-9 plan check is what
-- caught it). NULL (fewer than two days, or zero variance in x) collapses to
-- 0 — the "no trend yet" answer.
--
-- `raw` is MATERIALIZED on purpose: it narrows each tuple to (day, wpm, secs)
-- BEFORE the grouping sort. Without the fence the planner flattens the CTE
-- and sorts rows still carrying the whole server_metrics document — ~200
-- bytes each — which at 100k accepted runs is exactly the disk-spilling sort
-- the day-bucket design exists to avoid (the zone-9 spill check caught that
-- too, when 00019 moved the metrics behind a join).
WITH raw AS MATERIALIZED (
        SELECT (r.created_at AT TIME ZONE 'UTC')::date        AS day,
               (v.server_metrics ->> 'wpm')::float8         AS wpm,
               (v.server_metrics ->> 'durationSec')::float8 AS secs
        FROM runs r
                 JOIN run_verdicts v ON v.run_id = r.id AND v.user_id = r.user_id
        WHERE r.user_id = $1 AND r.status = 'accepted'
          AND r.created_at >= $2 AND r.created_at < $3),
     days AS (SELECT day, avg(wpm) AS wpm, sum(secs) AS secs
              FROM raw
              GROUP BY 1),
     pts AS (SELECT wpm, sum(secs) OVER (ORDER BY day) / 3600.0 AS hours
             FROM days)
SELECT coalesce(regr_slope(wpm, hours), 0)::float8 AS wpm_per_hour
FROM pts;

-- name: GetProfilePBs :many
-- The PB cards: leaderboard_entries IS the per-bucket personal-best store (one
-- row per player per bucket, projected from accepted runs), so this is a read,
-- not a computation. Own-profile view — deliberately the raw entries table,
-- not the ban-filtered leaderboard_rows: a player's own bests are their own
-- data, and this surface is session-scoped to its owner.
SELECT bucket_key, run_id, score, wpm::float8 AS wpm, raw::float8 AS raw,
       acc::float8 AS acc, grade, mods, quote_source, achieved_at
FROM leaderboard_entries
WHERE user_id = $1
ORDER BY achieved_at DESC;

-- name: GetProfileKeyboard :many
-- The keyboard heatmap read: the projection's rows, verbatim — aggregates the
-- worker maintains at verdict time, never derived from logs at request time.
SELECT key_id, presses, errors, interval_sum_ms, interval_count
FROM user_keyboard_profile
WHERE user_id = $1
ORDER BY key_id;

-- name: GetPublicProfileUser :one
-- The public surface's name resolution (docs/PROFILE.md, "Public profiles"):
-- display_name is citext UNIQUE, so the lookup is case-insensitive exactly the
-- way registration's uniqueness is. Nothing else about the account rides along
-- — the public payloads are explicit allowlists, and this row is where the
-- allowlist for the header STOPS.
SELECT id, display_name, created_at, profile_public, keyboard_public
FROM users
WHERE display_name = $1;

-- name: GetPublicProfileRunsFirst :many
-- First page of a profile's PUBLIC run history. The WHERE clause is the board
-- surface's visibility rule, spelled the same way as the public replay pair in
-- internal/runs/queries.sql: only ACCEPTED runs, and none at all while the
-- owner is banned (active_bans is THE ban/expiry predicate — docs/MODERATION.md).
-- Whether the profile answers a stranger at all is the handler's 403, decided
-- before this query runs; per-run visibility is decided here, in SQL, so no
-- new code path can reach a hidden run.
--
-- The column list is an ALLOWLIST, deliberately narrower than the owner's own
-- feed: no client-reported numbers, no validation trail, no setup snapshot, no
-- restart counter. server_metrics/server_score and the derived cells are
-- exactly what the public replay route already serves per run.
SELECT r.id, r.mode, r.duration_ms, r.word_count, r.lang,
       v.server_metrics, v.server_score, r.created_at,
       (jsonb_strip_nulls(jsonb_build_object(
           'grade',       run_grade((v.server_metrics ->> 'accuracy')::numeric),
           'consistency', (v.server_metrics ->> 'consistency')::float8,
           'chars',       v.server_metrics -> 'chars',
           'quoteId',     run_quote_id(r.setup),
           'adoptedFromRunId', run_adopted_from(r.setup),
           'mods',        run_mods(r.setup))))::jsonb AS derived
FROM runs r
         JOIN run_verdicts v ON v.run_id = r.id AND v.user_id = r.user_id
WHERE r.user_id = $1
  AND r.status = 'accepted'
  AND NOT EXISTS (SELECT 1 FROM active_bans b WHERE b.user_id = r.user_id)
ORDER BY r.created_at DESC, r.id DESC
LIMIT $2;

-- name: GetPublicProfileRunsAfter :many
-- Next public-history page after the (created_at, id) cursor — the same
-- longhand row-value comparison, with the redundant-looking `<=` SEEK conjunct,
-- as ListRunsAfter in internal/runs/queries.sql, and for the same measured
-- reason (migration 00015's 3-column index provides the start condition).
SELECT r.id, r.mode, r.duration_ms, r.word_count, r.lang,
       v.server_metrics, v.server_score, r.created_at,
       (jsonb_strip_nulls(jsonb_build_object(
           'grade',       run_grade((v.server_metrics ->> 'accuracy')::numeric),
           'consistency', (v.server_metrics ->> 'consistency')::float8,
           'chars',       v.server_metrics -> 'chars',
           'quoteId',     run_quote_id(r.setup),
           'adoptedFromRunId', run_adopted_from(r.setup),
           'mods',        run_mods(r.setup))))::jsonb AS derived
FROM runs r
         JOIN run_verdicts v ON v.run_id = r.id AND v.user_id = r.user_id
WHERE r.user_id = $1
  AND r.status = 'accepted'
  AND NOT EXISTS (SELECT 1 FROM active_bans b WHERE b.user_id = r.user_id)
  AND r.created_at <= $2
  AND (r.created_at < $2 OR (r.created_at = $2 AND r.id < $3))
ORDER BY r.created_at DESC, r.id DESC
LIMIT $4;

-- name: GetPublicProfilePBs :many
-- The PB cards as a STRANGER may see them: the same entries read as
-- GetProfilePBs, but through the ban predicate the boards read through. The
-- session-scoped read deliberately skips it (a player's own bests are their
-- own data); a public surface showing a banned player's records would leak
-- what every board hides. leaderboard_rows is not used here only because the
-- view does not carry quote_source; the predicate is the same active_bans
-- object, so the two cannot drift.
SELECT bucket_key, run_id, score, wpm::float8 AS wpm, raw::float8 AS raw,
       acc::float8 AS acc, grade, mods, quote_source, achieved_at
FROM leaderboard_entries e
WHERE e.user_id = $1
  AND NOT EXISTS (SELECT 1 FROM active_bans b WHERE b.user_id = e.user_id)
ORDER BY achieved_at DESC;

-- name: GetProfileDominantLang :one
-- The user's most-played dictionary language — the heatmap's default layout
-- comes from it. Ties break alphabetically so the answer is stable.
SELECT lang
FROM runs
WHERE user_id = $1
GROUP BY lang
ORDER BY count(*) DESC, lang
LIMIT 1;

-- name: SearchUsersByName :many
-- Player search (docs/PROFILE.md, "Search"). Two parameters carry one user
-- input, on purpose:
--
--   * @pattern is the input lowercased AND escaped for LIKE. '_' is both a
--     legal display_name character (the 00001 CHECK allows [a-zA-Z0-9_.-])
--     and a LIKE wildcard, so an unescaped search for "foo_bar" would also
--     match "fooXbar" — a wrong result, not merely a loose one.
--   * @needle is the same input lowercased but NOT escaped. It feeds only the
--     exact-match ranking term, which compares strings rather than patterns
--     and would be broken by the escaping the pattern needs.
--
-- Ranking is spelled out rather than delegated to pg_trgm's similarity():
-- exact match, then prefix, then shortest name, then alphabetical. similarity()
-- would order results by a number the searcher cannot predict, and it is noisy
-- over 3-20 character handles; this ordering is one a user can reconstruct by
-- looking at it. The alphabetical tail is what makes two identical requests
-- return the same page — length alone leaves ties, and a search box that
-- reshuffles under the cursor reads as broken.
--
-- Banned accounts are absent: active_bans is the same predicate every
-- leaderboard and the public run history read through (docs/MODERATION.md).
-- Search is a discovery surface, so it hides exactly what the boards hide,
-- while /users/{name} keeps answering 200 for a banned name — the header route
-- has never been the thing that hides a ban.
--
-- CLOSED profiles ARE returned. A closed profile is still ranked on the boards
-- under its own name and its header still answers 200, so making it unfindable
-- by that name would contradict "closed is a state, not a 404" (00018) without
-- concealing anything the boards do not already publish. The row carries
-- profile_public so the client can render the closed state instead of
-- promising a page that will 403.
SELECT u.display_name, u.created_at, u.profile_public
FROM users u
WHERE lower(u.display_name::text) LIKE '%' || @pattern::text || '%'
  AND NOT EXISTS (SELECT 1 FROM active_bans b WHERE b.user_id = u.id)
ORDER BY (lower(u.display_name::text) = @needle::text) DESC,
         (lower(u.display_name::text) LIKE @pattern::text || '%') DESC,
         char_length(u.display_name::text),
         u.display_name
LIMIT @lim;

-- name: GetPublicProfileIdentity :one
-- The profile's IDENTITY half for one account (00029): what the owner wrote
-- about themselves. Read as a SEPARATE statement from GetPublicProfileUser
-- rather than by widening it, because the two answer different questions and
-- are gated differently: the header row decides whether the page exists at all
-- (and answers for a CLOSED profile), while this is profile content and is only
-- served once the profile gate has passed.
SELECT bio, keyboard
FROM users
WHERE id = $1;

-- name: ListShowcaseBadges :many
-- One account's badge SHOWCASE, in the order its owner arranged.
--
-- The live predicate is `revoked_at IS NULL` and nothing else. In particular it
-- is NOT "has a display_order": a revoked badge keeps whatever position it had,
-- because revocation is an operator's act and must not quietly rewrite a
-- showcase the owner arranged — so the filter has to be the revocation, and a
-- reader that trusted display_order alone would keep rendering a badge that was
-- taken away. `display_order IS NOT NULL` is the owner's own "show this" half.
--
-- badge_code breaks ties: display_order is not unique (00029 explains why), and
-- an ordering that can permute between two identical reads renders as a
-- showcase that shuffles under the cursor.
SELECT badge_code, display_order
FROM user_badges
WHERE user_id = $1
  AND revoked_at IS NULL
  AND display_order IS NOT NULL
ORDER BY display_order, badge_code;

-- name: ListGrantedBadges :many
-- Every badge the account HOLDS, shown or not — what the settings screen offers
-- as the pool to arrange, and what the showcase write validates against. Same
-- live predicate, without the owner's visibility half.
SELECT badge_code, granted_at, display_order
FROM user_badges
WHERE user_id = $1
  AND revoked_at IS NULL
ORDER BY granted_at, badge_code;

-- name: ListUserLinks :many
-- One account's social links. `kind` orders them so the profile header renders
-- the same sequence on every load.
SELECT kind, handle
FROM user_links
WHERE user_id = $1
ORDER BY kind;

-- name: UpdateProfileIdentity :exec
-- The bio/keyboard half of PATCH /me/profile. Both are resolved against the
-- current row by the handler before this runs, so this writes the pair the
-- caller will be shown; NULL clears (00029: NULL is "unset", '' is impossible).
UPDATE users
SET bio      = sqlc.narg(bio),
    keyboard = sqlc.narg(keyboard),
    updated_at = now()
WHERE id = @user_id;

-- name: UpsertUserLink :exec
-- Set one link. The natural key IS the primary key, so "change my GitHub" is an
-- upsert on (user_id, kind) rather than a delete-then-insert that would open a
-- window where the link does not exist.
INSERT INTO user_links (user_id, kind, handle)
VALUES (@user_id, @kind, @handle)
ON CONFLICT (user_id, kind) DO UPDATE SET handle = EXCLUDED.handle;

-- name: DeleteUserLink :exec
-- Clearing a link is deleting its row: an empty handle is not a state
-- user_links can hold (the per-kind CHECKs forbid it), and storing one would
-- make "has no GitHub" and "has a broken GitHub" the same row.
DELETE FROM user_links WHERE user_id = @user_id AND kind = @kind;

-- name: ClearBadgeShowcase :exec
-- Take every live badge out of the showcase. The showcase write is
-- "clear, then place the chosen ones", inside one transaction: the alternative
-- — diffing the request against what is stored — would have to decide what an
-- absent code means, and the answer ("hide it") is exactly what clearing does.
UPDATE user_badges
SET display_order = NULL
WHERE user_id = @user_id AND revoked_at IS NULL;

-- name: PlaceBadgeInShowcase :exec
-- Give one badge its position. The `revoked_at IS NULL` predicate is what makes
-- this refuse a revoked badge: the handler validates the code against the live
-- grants first, but the write must not depend on that check still being true —
-- a revocation racing a showcase save would otherwise put a taken-away badge
-- back on a public page. A no-op here is the honest outcome of that race.
UPDATE user_badges
SET display_order = @display_order
WHERE user_id = @user_id AND badge_code = @badge_code AND revoked_at IS NULL;
