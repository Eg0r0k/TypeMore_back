-- Profile aggregates (docs/PROFILE.md): on-demand SQL over `runs`, computed at
-- request time, cached nowhere. Every query here is scoped to ONE user and must
-- stay index-driven off runs_user_id_created_at_idx (or, for the PB cards,
-- leaderboard_entries_user_idx) — the 100k-run perf suite pins the plans.
--
-- Two aggregation rules, stated once (and asserted by tests):
--   * METRIC aggregates (wpm/raw/acc/consistency, estimated words) read
--     ACCEPTED runs only — the server-verified numbers in server_metrics.
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
       coalesce(sum((server_metrics ->> 'durationSec')::float8 * 1000)
                FILTER (WHERE status = 'accepted'), 0)::bigint     AS time_typing_ms,
       coalesce(sum(((server_metrics -> 'chars' ->> 'correct')::float8
                   + (server_metrics -> 'chars' ->> 'incorrect')::float8
                   + (server_metrics -> 'chars' ->> 'extra')::float8
                   + (server_metrics ->> 'spaces')::float8) / 5)
                FILTER (WHERE status = 'accepted'), 0)::float8     AS estimated_words
FROM runs
WHERE user_id = $1;

-- name: GetProfileMetricStats :one
-- highest / average for the four profile metrics, over accepted runs.
SELECT coalesce(max((server_metrics ->> 'wpm')::float8), 0)::float8         AS wpm_highest,
       coalesce(avg((server_metrics ->> 'wpm')::float8), 0)::float8         AS wpm_average,
       coalesce(max((server_metrics ->> 'raw')::float8), 0)::float8         AS raw_highest,
       coalesce(avg((server_metrics ->> 'raw')::float8), 0)::float8         AS raw_average,
       coalesce(max((server_metrics ->> 'accuracy')::float8), 0)::float8    AS acc_highest,
       coalesce(avg((server_metrics ->> 'accuracy')::float8), 0)::float8    AS acc_average,
       coalesce(max((server_metrics ->> 'consistency')::float8), 0)::float8 AS consistency_highest,
       coalesce(avg((server_metrics ->> 'consistency')::float8), 0)::float8 AS consistency_average
FROM runs
WHERE user_id = $1 AND status = 'accepted';

-- name: GetProfileLast10 :one
-- The same four metrics averaged over the LAST 10 accepted runs by date. The
-- inner query walks (user_id, created_at DESC) newest-first and stops after
-- ten accepted rows — no sort node, the index provides the order.
SELECT coalesce(avg(t.wpm), 0)::float8         AS wpm_average,
       coalesce(avg(t.raw), 0)::float8         AS raw_average,
       coalesce(avg(t.acc), 0)::float8         AS acc_average,
       coalesce(avg(t.consistency), 0)::float8 AS consistency_average
FROM (SELECT (server_metrics ->> 'wpm')::float8         AS wpm,
             (server_metrics ->> 'raw')::float8         AS raw,
             (server_metrics ->> 'accuracy')::float8    AS acc,
             (server_metrics ->> 'consistency')::float8 AS consistency
      FROM runs
      WHERE user_id = $1 AND status = 'accepted'
      ORDER BY created_at DESC, id DESC
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
SELECT (created_at AT TIME ZONE 'UTC')::date                        AS day,
       count(*)::int                                                AS tests,
       coalesce(sum((server_metrics ->> 'durationSec')::float8 * 1000)
                FILTER (WHERE status = 'accepted'), 0)::bigint      AS time_ms
FROM runs
WHERE user_id = $1 AND created_at >= $2
GROUP BY 1
ORDER BY 1;

-- name: GetProfileHistogram :many
-- Tests per 10-wpm bucket over accepted runs; `bucket` is the lower bound
-- (60 covers [60, 70)).
SELECT (floor((server_metrics ->> 'wpm')::float8 / 10) * 10)::int AS bucket,
       count(*)::int                                              AS tests
FROM runs
WHERE user_id = $1 AND status = 'accepted'
GROUP BY 1
ORDER BY 1;

-- name: GetProfileTimeseries :many
-- Per-day chart series inside [from, to): time typing (accepted, verified
-- durations), and the day's average wpm / accuracy over accepted runs.
SELECT (created_at AT TIME ZONE 'UTC')::date                        AS day,
       coalesce(sum((server_metrics ->> 'durationSec')::float8 * 1000)
                FILTER (WHERE status = 'accepted'), 0)::bigint      AS time_ms,
       coalesce(avg((server_metrics ->> 'wpm')::float8)
                FILTER (WHERE status = 'accepted'), 0)::float8      AS avg_wpm,
       coalesce(avg((server_metrics ->> 'accuracy')::float8)
                FILTER (WHERE status = 'accepted'), 0)::float8      AS avg_acc
FROM runs
WHERE user_id = $1 AND created_at >= $2 AND created_at < $3
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
WITH days AS (SELECT (created_at AT TIME ZONE 'UTC')::date AS day,
                     avg((server_metrics ->> 'wpm')::float8)          AS wpm,
                     sum((server_metrics ->> 'durationSec')::float8)  AS secs
              FROM runs
              WHERE user_id = $1 AND status = 'accepted'
                AND created_at >= $2 AND created_at < $3
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

-- name: GetProfileDominantLang :one
-- The user's most-played dictionary language — the heatmap's default layout
-- comes from it. Ties break alphabetically so the answer is stable.
SELECT lang
FROM runs
WHERE user_id = $1
GROUP BY lang
ORDER BY count(*) DESC, lang
LIMIT 1;
