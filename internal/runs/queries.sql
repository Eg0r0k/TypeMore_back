-- Queries for the runs domain. sqlc generates type-safe Go from these into
-- internal/runs/runsdb. The gzip log blob is written once at ingestion and read
-- back only through GetRunLog (the ?log=1 detail flag); the summary queries
-- deliberately never SELECT it.

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
-- ListRunsAfter using the (created_at, id) of the last row.
SELECT id, mode, duration_ms, word_count, lang, seed, dict_hash,
       setup, client_metrics, client_score, score_version, status,
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
       log_bytes, created_at
FROM runs
WHERE id = $1 AND user_id = $2;

-- name: GetRunLog :one
-- The gzip log blob for one run, scoped to its owner (the ?log=1 detail flag).
SELECT log FROM runs
WHERE id = $1 AND user_id = $2;
