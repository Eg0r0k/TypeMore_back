-- +goose Up
-- Replay-worker verdict columns (BACKEND.md §3, docs/REPLAY.md). The worker
-- recomputes every pending run through the vendored core bundle in goja and
-- records what it found: the server's own metrics/score, the validation report,
-- and which bundle produced them.
--
-- Nothing here is nullable-by-accident: every column is NULL for a run that has
-- not been replayed yet, and a decision writes all of them at once. The
-- client-reported numbers (client_metrics / client_score) are never overwritten
-- — the pair is the evidence a mismatch is judged on.

ALTER TABLE runs
    -- Server-recomputed Metrics (wpm/raw/accuracy/consistency/chars/…), exactly
    -- as validateLog returned them. Opaque to Go: stored verbatim.
    ADD COLUMN server_metrics jsonb,
    -- Server-recomputed ScoreResult, routed by the run's score_version.
    ADD COLUMN server_score   jsonb,
    -- {verdict, reason, flags[]} — the validation report plus the decision
    -- reason. `verdict` is the core's 'valid'/'invalid' or the server-side
    -- 'error' when the run could not be replayed at all.
    ADD COLUMN validation     jsonb,
    -- SHA-256 of the core bundle that produced the numbers above. A rebalance
    -- or a bundle upgrade can therefore find exactly which runs were judged by
    -- which code.
    ADD COLUMN bundle_sha     text,
    ADD COLUMN validated_at   timestamptz,
    -- Replay attempts so far. Only a *failed* replay (timeout / JS error)
    -- increments it; a run that reached a verdict never comes back.
    ADD COLUMN attempts       smallint NOT NULL DEFAULT 0,
    -- Last replay failure, for operator triage. NULL once a run succeeds.
    ADD COLUMN last_error     text;

-- The queue scan is unchanged: runs_pending_idx (status, created_at) WHERE
-- status = 'pending' from 00002. The worker claims from it with
-- FOR UPDATE SKIP LOCKED (see docs/REPLAY.md for why not river).

-- +goose Down
ALTER TABLE runs
    DROP COLUMN server_metrics,
    DROP COLUMN server_score,
    DROP COLUMN validation,
    DROP COLUMN bundle_sha,
    DROP COLUMN validated_at,
    DROP COLUMN attempts,
    DROP COLUMN last_error;
