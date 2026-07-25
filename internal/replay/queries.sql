-- Replay worker queue (docs/REPLAY.md). Two statements, both executed inside the
-- SAME transaction: the claim takes the row locks, the decision writes the
-- verdict, and the commit releases both together.

-- name: ClaimPendingRuns :many
-- The queue scan. FOR UPDATE SKIP LOCKED lets N workers share one queue with no
-- broker and no 'processing' status: a row another worker already holds is
-- stepped over, and a worker that dies rolls its rows straight back to
-- claimable. Oldest first, so nothing starves. Uses runs_pending_idx.
SELECT id, seed, dict_hash, score_version, setup, client_metrics, client_score,
       log, attempts
FROM runs
WHERE status = 'pending'
ORDER BY created_at
FOR UPDATE SKIP LOCKED
LIMIT $1;

-- name: ApplyReplayDecision :exec
-- Record one verdict. client_metrics / client_score are deliberately untouched:
-- the client's numbers and the server's sit side by side forever, because the
-- pair IS the evidence a mismatch is judged on.
UPDATE runs
SET status         = @status,
    server_metrics = @server_metrics,
    server_score   = @server_score,
    validation     = @validation,
    bundle_sha     = @bundle_sha,
    attempts       = @attempts,
    last_error     = NULLIF(@last_error::text, ''),
    validated_at   = now()
WHERE id = @id;
