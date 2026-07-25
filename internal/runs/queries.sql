-- Queries for the runs domain. sqlc generates type-safe Go from these into
-- internal/runs/runsdb. The gzip log blob is written once at ingestion and read
-- back only through GetRunLog (the ?log=1 detail flag) and GetPublicReplayLog
-- (the public log route); the summary queries deliberately never SELECT it.

-- name: CreateRun :one
INSERT INTO runs (
    user_id, mode, duration_ms, word_count, lang, seed, dict_hash,
    setup, client_metrics, client_score, score_version, log, log_bytes
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13
)
RETURNING id, status, created_at;

-- name: ListRunsFirst :many
-- First page of a user's runs, newest first. Keyset pagination continues via
-- ListRunsAfter using the (created_at, id) of the last row. The replay columns
-- (server_metrics/server_score/validation/validated_at) are NULL until the
-- worker has judged the run.
SELECT id, mode, duration_ms, word_count, lang, seed, dict_hash,
       setup, client_metrics, client_score, score_version, status,
       server_metrics, server_score, validation, validated_at,
       log_bytes, created_at
FROM runs
WHERE user_id = $1
ORDER BY created_at DESC, id DESC
LIMIT $2;

-- name: ListRunsAfter :many
-- Next page after the (created_at, id) cursor. The row-value comparison written
-- out longhand keeps sqlc's type inference happy and matches the composite
-- (created_at DESC, id DESC) ordering exactly.
SELECT id, mode, duration_ms, word_count, lang, seed, dict_hash,
       setup, client_metrics, client_score, score_version, status,
       server_metrics, server_score, validation, validated_at,
       log_bytes, created_at
FROM runs
WHERE user_id = $1
  AND (created_at < $2 OR (created_at = $2 AND id < $3))
ORDER BY created_at DESC, id DESC
LIMIT $4;

-- name: GetRun :one
-- One run's summary (no log payload), scoped to its owner.
SELECT id, mode, duration_ms, word_count, lang, seed, dict_hash,
       setup, client_metrics, client_score, score_version, status,
       server_metrics, server_score, validation, validated_at,
       log_bytes, created_at
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
-- Three access rules are in the WHERE clause rather than in Go, so no caller can
-- reach this data without them: the run must be ACCEPTED (a flagged, rejected
-- or unjudged run is not a public artefact), and its owner must not be banned.
-- All three failures return no row, which the handler renders as one
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
  AND NOT EXISTS (SELECT 1 FROM active_bans b WHERE b.user_id = r.user_id);

-- name: GetPublicReplayLog :one
-- The stored gzip event log of one publicly watchable run, and nothing else, so
-- the log route never re-reads the metadata it is not going to serve.
--
-- The WHERE clause is the SAME THREE RULES, spelled the same way, including the
-- join to users: eligibility for the log must be eligibility for the metadata,
-- character for character, or the two routes drift into two access matrices.
SELECT r.log
FROM runs r
         JOIN users u ON u.id = r.user_id
WHERE r.id = @run_id
  AND r.status = 'accepted'
  AND jsonb_typeof(r.server_metrics -> 'accuracy') = 'number'
  AND NOT EXISTS (SELECT 1 FROM active_bans b WHERE b.user_id = r.user_id);
