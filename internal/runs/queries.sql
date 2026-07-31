-- Queries for the runs domain. sqlc generates type-safe Go from these into
-- internal/runs/runsdb. The gzip log blob is written once at ingestion and read
-- back only through GetRunLog (the ?log=1 detail flag) and GetPublicReplayLog
-- (the public log route); the summary queries deliberately never SELECT it.

-- name: CreateRun :one
INSERT INTO runs (
    user_id, mode, duration_ms, word_count, lang, seed, dict_hash,
    setup, client_metrics, client_score, score_version, log, log_bytes,
    restarts_since_last_submit
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14
)
RETURNING id, status, created_at;

-- name: ListRunsFirst :many
-- First page of a user's runs, newest first. Keyset pagination continues via
-- ListRunsAfter using the (created_at, id) of the last row. The replay columns
-- (server_metrics/server_score/validation/validated_at) are NULL until the
-- worker has judged the run.
--
-- The trailing `derived` document carries the profile table's cells, lifted
-- server-side so the client renders rows without parsing the whole
-- setup/metrics documents: the SQL grade of the SERVER accuracy (run_grade —
-- the fenced mirror of the core's gradeOf), the consistency and chars slices
-- of server_metrics (absent until judged), and the run_quote_id / run_mods
-- selections of the setup snapshot the leaderboard projection already uses,
-- plus run_adopted_from — the seeded-repeat marker, which is what lets a row
-- render "saved, not counted" without the client re-deriving eligibility.
--
-- Every cell is a pure function of columns this table already holds. There is
-- deliberately NO join here, to quotes or to anything else: this is the profile
-- page's hot query with pinned plans on a 100k-run account (docs/PERFORMANCE.md
-- zone 9), and a quote's own text and length band are already served, cached by
-- id, by GET /quotes/{id} — the same call the quote board's heading makes.
-- One jsonb document rather than five columns so nullability stays a property
-- of the data (jsonb_strip_nulls) instead of five hand-annotated scan types.
-- Additive: every pre-existing column is untouched.
SELECT id, mode, duration_ms, word_count, lang, seed, dict_hash,
       setup, client_metrics, client_score, score_version, status,
       server_metrics, server_score, validation, validated_at,
       log_bytes, restarts_since_last_submit, created_at,
       (jsonb_strip_nulls(jsonb_build_object(
           'grade',       run_grade((server_metrics ->> 'accuracy')::numeric),
           'consistency', (server_metrics ->> 'consistency')::float8,
           'chars',       server_metrics -> 'chars',
           'quoteId',     run_quote_id(setup),
           'adoptedFromRunId', run_adopted_from(setup),
           'mods',        run_mods(setup))))::jsonb AS derived
FROM runs
WHERE user_id = $1
ORDER BY created_at DESC, id DESC
LIMIT $2;

-- name: ListRunsAfter :many
-- Next page after the (created_at, id) cursor. The row-value comparison written
-- out longhand keeps sqlc's type inference happy and matches the composite
-- (created_at DESC, id DESC) ordering exactly. The redundant-looking
-- `created_at <= $2` conjunct is the SEEK: the planner cannot derive a start
-- condition from the OR form alone, so without it the cursor is a filter the
-- index scan walks up to — linear in depth, measured at 66 ms by row 50 000 on
-- the zone-9 fixture. With it, the scan starts at the cursor and the OR only
-- trims the tie boundary (migration 00015 carries the matching 3-column
-- index, which is also what retires the id-tiebreak Incremental Sort).
SELECT id, mode, duration_ms, word_count, lang, seed, dict_hash,
       setup, client_metrics, client_score, score_version, status,
       server_metrics, server_score, validation, validated_at,
       log_bytes, restarts_since_last_submit, created_at,
       (jsonb_strip_nulls(jsonb_build_object(
           'grade',       run_grade((server_metrics ->> 'accuracy')::numeric),
           'consistency', (server_metrics ->> 'consistency')::float8,
           'chars',       server_metrics -> 'chars',
           'quoteId',     run_quote_id(setup),
           'adoptedFromRunId', run_adopted_from(setup),
           'mods',        run_mods(setup))))::jsonb AS derived
FROM runs
WHERE user_id = $1
  AND created_at <= $2
  AND (created_at < $2 OR (created_at = $2 AND id < $3))
ORDER BY created_at DESC, id DESC
LIMIT $4;

-- name: GetRun :one
-- One run's summary (no log payload), scoped to its owner.
SELECT id, mode, duration_ms, word_count, lang, seed, dict_hash,
       setup, client_metrics, client_score, score_version, status,
       server_metrics, server_score, validation, validated_at,
       log_bytes, restarts_since_last_submit, created_at,
       (jsonb_strip_nulls(jsonb_build_object(
           'grade',       run_grade((server_metrics ->> 'accuracy')::numeric),
           'consistency', (server_metrics ->> 'consistency')::float8,
           'chars',       server_metrics -> 'chars',
           'quoteId',     run_quote_id(setup),
           'adoptedFromRunId', run_adopted_from(setup),
           'mods',        run_mods(setup))))::jsonb AS derived
FROM runs
WHERE id = $1 AND user_id = $2;

-- name: GetRunLog :one
-- The gzip log blob for one run, scoped to its owner (the ?log=1 detail flag).
SELECT log FROM runs
WHERE id = $1 AND user_id = $2;

-- name: GetPublicReplay :one
-- Everything needed to describe someone else's run: the setup and the seed to
-- regenerate the text, the dict_hash to verify the dictionary that regeneration
-- is fed, and the server's own verdict numbers. The log is NOT here — it is a
-- separate route (GetPublicReplayLog) so the stored gzip blob can go straight
-- to the socket instead of being gunzipped into a JSON envelope
-- (docs/PERFORMANCE.md, zone 6).
--
-- seed and dict_hash are the same two columns the owner's authenticated summary
-- already exposes (docs/RUNS.md); the word list is regenerated on the client
-- from them, so the server never ships a word list it would then have to keep
-- in sync with the core's generator.
--
-- Four access rules are in the WHERE clause rather than in Go, so no caller can
-- reach this data without them: the run must be ACCEPTED (a flagged, rejected
-- or unjudged run is not a public artefact), its owner must not be banned, and
-- — since public profiles — its owner's profile must be open OR the run must
-- hold a leaderboard slot. The last disjunct is the boundary between profile
-- privacy and the boards (docs/PROFILE.md, "Public profiles"): closing a
-- profile hides the aggregated history page, never a result its owner put into
-- a public ranking, so a board row's replay keeps working whatever the switch
-- says. All failures return no row, which the handler renders as one
-- indistinguishable 404 — a leaderboard must not leak who is under review.
SELECT r.setup, r.server_metrics, r.server_score,
       run_grade((r.server_metrics ->> 'accuracy')::numeric)::text AS grade,
       r.mode, r.duration_ms, r.word_count, r.lang, r.seed, r.dict_hash,
       r.created_at, u.display_name
FROM runs r
         JOIN users u ON u.id = r.user_id
WHERE r.id = @run_id
  AND r.status = 'accepted'
  AND jsonb_typeof(r.server_metrics -> 'accuracy') = 'number'
  AND NOT EXISTS (SELECT 1 FROM active_bans b WHERE b.user_id = r.user_id)
  AND (u.profile_public OR EXISTS (SELECT 1 FROM leaderboard_entries e WHERE e.run_id = r.id));

-- name: GetPublicReplayLog :one
-- The stored gzip event log of one publicly watchable run, and nothing else, so
-- the log route never re-reads the metadata it is not going to serve.
--
-- The WHERE clause is the SAME FOUR RULES, spelled the same way, including the
-- join to users: eligibility for the log must be eligibility for the metadata,
-- character for character, or the two routes drift into two access matrices.
SELECT r.log
FROM runs r
         JOIN users u ON u.id = r.user_id
WHERE r.id = @run_id
  AND r.status = 'accepted'
  AND jsonb_typeof(r.server_metrics -> 'accuracy') = 'number'
  AND NOT EXISTS (SELECT 1 FROM active_bans b WHERE b.user_id = r.user_id)
  AND (u.profile_public OR EXISTS (SELECT 1 FROM leaderboard_entries e WHERE e.run_id = r.id));
