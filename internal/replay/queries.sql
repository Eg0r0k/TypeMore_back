-- Replay worker queue + revalidation (docs/REPLAY.md). The claim and the
-- decision are executed inside the SAME transaction: the claim takes the row
-- locks, the decision writes the verdict, and the commit releases both together.

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

-- name: ClaimStalePolicyRuns :many
-- The revalidation scan: runs already judged, but by rules or by CODE that are
-- no longer current. Two independent reasons to re-judge, because bundle_sha and
-- policy_version answer different questions (docs/REPLAY.md, "Policy
-- versioning"):
--
--   policy_version behind (or NULL, i.e. judged before the policy existed)
--       the rules that turned the numbers into a status have moved
--   bundle_sha not the current one
--       the code that produced the NUMBERS has moved, so the numbers on the row
--       are the old bundle's and may disagree with what the client now computes
--
-- Keying on policy_version alone left the second class stranded: a re-vendored
-- bundle would flag honest runs as metric_mismatch and `make revalidate` would
-- refuse to look at them, because their policy_version was already current.
--
-- IS DISTINCT FROM, not <>, so a row judged before bundle_sha was recorded
-- (NULL) is claimed rather than skipped by three-valued logic.
--
-- Same locking discipline as the queue, so a revalidation pass and the worker
-- can run at the same time without either seeing the other's rows. Uses
-- runs_stale_policy_idx.
SELECT id, seed, dict_hash, score_version, setup, client_metrics, client_score,
       log, attempts
FROM runs
WHERE status <> 'pending'
  AND (policy_version IS NULL
       OR policy_version < @policy_version
       OR bundle_sha IS DISTINCT FROM @bundle_sha::text)
ORDER BY created_at
FOR UPDATE SKIP LOCKED
LIMIT @row_limit;

-- name: ApplyReplayDecision :exec
-- Record one verdict. client_metrics / client_score are deliberately untouched:
-- the client's numbers and the server's sit side by side forever, because the
-- pair IS the evidence a mismatch is judged on. bundle_sha and policy_version
-- together say which code and which rules produced this row.
UPDATE runs
SET status         = @status,
    server_metrics = @server_metrics,
    server_score   = @server_score,
    validation     = @validation,
    bundle_sha     = @bundle_sha,
    policy_version = @policy_version,
    attempts       = @attempts,
    last_error     = NULLIF(@last_error::text, ''),
    validated_at   = now()
WHERE id = @id;

-- name: ListRunsForCalibration :many
-- Read-only sample for `make calibrate`: everything the decision needs, plus the
-- status the run currently carries so a dry run can report what would change.
-- No locking, no ordering surprises — oldest first, bounded by the caller.
SELECT id, seed, dict_hash, score_version, setup, client_metrics, client_score,
       log, attempts, status, policy_version, created_at
FROM runs
WHERE status <> 'pending'
ORDER BY created_at
LIMIT @row_limit;
