-- Replay worker queue + revalidation (docs/REPLAY.md). The claim and the
-- decision are executed inside the SAME transaction: the claim takes the row
-- locks, the decision writes the verdict, and the commit releases both together.
--
-- Since 00019 the verdict payload lives in run_verdicts (1:1 with runs, row
-- exists <=> judged); runs keeps the lifecycle (status) and the queue's retry
-- bookkeeping (attempts, last_error). A decision is therefore two statements —
-- UpsertRunVerdict + ApplyRunOutcome — issued back to back in the claim's
-- transaction, which is the same atomicity the old single UPDATE had.

-- name: ClaimPendingRuns :many
-- The queue scan. FOR UPDATE SKIP LOCKED lets N workers share one queue with no
-- broker and no 'processing' status: a row another worker already holds is
-- stepped over, and a worker that dies rolls its rows straight back to
-- claimable. Oldest first, so nothing starves. Uses runs_pending_idx.
--
-- created_at rides along because the judgement depends on it: the canary
-- detectors are armed per run against the canary epoch (docs/REPLAY.md), and a
-- run created before the canary-rendering client shipped must be judged exactly
-- as it was. It is an already-selected column of the same row, so carrying it
-- costs nothing.
SELECT id, seed, dict_hash, score_version, setup, client_metrics, client_score,
       log, attempts, created_at
FROM runs
WHERE status = 'pending'
ORDER BY created_at
FOR UPDATE SKIP LOCKED
LIMIT $1;

-- name: ClaimStalePolicyRuns :many
-- The revalidation scan: runs already judged, but by rules or by CODE that are
-- no longer current. The join IS the "judged" predicate — a verdict row exists
-- exactly for judged runs, so this claim walks the verdict table where it used
-- to walk the status <> 'pending' partial index. Two independent reasons to
-- re-judge, because bundle_sha and policy_version answer different questions
-- (docs/REPLAY.md, "Policy versioning"):
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
-- FOR UPDATE OF r, v: both halves of the run are locked, with the same SKIP
-- LOCKED discipline as the queue, so a revalidation pass and the worker can run
-- at the same time without either seeing the other's rows (their claims are
-- disjoint anyway — a pending run has no verdict row to join).
SELECT r.id, r.seed, r.dict_hash, r.score_version, r.setup, r.client_metrics,
       r.client_score, r.log, r.attempts, r.created_at
FROM runs r
         JOIN run_verdicts v ON v.run_id = r.id
WHERE v.policy_version IS NULL
   OR v.policy_version < @policy_version
   OR v.bundle_sha IS DISTINCT FROM @bundle_sha::text
ORDER BY r.created_at
FOR UPDATE OF r, v SKIP LOCKED
LIMIT @row_limit;

-- name: UpsertRunVerdict :exec
-- Record one verdict's payload. client_metrics / client_score on runs are
-- deliberately untouched: the client's numbers and the server's sit side by
-- side forever, because the pair IS the evidence a mismatch is judged on.
-- bundle_sha and policy_version together say which code and which rules
-- produced this row. The upsert is what makes revalidation a rewrite of the
-- SAME verdict identity rather than a second one.
--
-- user_id is sourced from the runs row right here (INSERT..SELECT), so the
-- snapshot column cannot be miswritten by a caller and the Decision type
-- never needs to carry it. On conflict it is left alone: it is immutable.
INSERT INTO run_verdicts (run_id, user_id, server_metrics, server_score,
                          validation, bundle_sha, policy_version, validated_at)
SELECT r.id, r.user_id, @server_metrics::jsonb, @server_score::jsonb,
       @validation::jsonb, @bundle_sha, @policy_version, now()
FROM runs r
WHERE r.id = @run_id
ON CONFLICT (run_id) DO UPDATE SET
    server_metrics = excluded.server_metrics,
    server_score   = excluded.server_score,
    validation     = excluded.validation,
    bundle_sha     = excluded.bundle_sha,
    policy_version = excluded.policy_version,
    validated_at   = now();

-- name: ApplyRunOutcome :exec
-- The lifecycle half of the same decision: status transition plus the queue's
-- retry bookkeeping. Always executed in the same transaction as
-- UpsertRunVerdict; the invariant "status <> 'pending' <=> a verdict row
-- exists" is exactly the pair of these two statements committing together.
UPDATE runs
SET status     = @status,
    attempts   = @attempts,
    last_error = NULLIF(@last_error::text, '')
WHERE id = @id;

-- name: ListRunsForCalibration :many
-- Read-only sample for `make calibrate`: everything the decision needs, plus
-- the status and policy_version the run currently carries so a dry run can
-- report what would change. The verdict join doubles as the "judged only"
-- filter. No locking, no ordering surprises — oldest first, bounded by the
-- caller.
SELECT r.id, r.seed, r.dict_hash, r.score_version, r.setup, r.client_metrics,
       r.client_score, r.log, r.attempts, r.status, v.policy_version,
       r.created_at
FROM runs r
         JOIN run_verdicts v ON v.run_id = r.id
ORDER BY r.created_at
LIMIT @row_limit;
