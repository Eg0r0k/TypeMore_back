-- +goose Up
-- Flag policy versioning (docs/REPLAY.md, "Review policy").
--
-- bundle_sha answers "which code computed these numbers". policy_version answers
-- the other half: "which rules turned those numbers into a status". They move
-- independently — a weights/threshold change re-judges runs without touching a
-- single metric — so a verdict is only reproducible with both recorded.
--
-- NULL means the run was judged before the policy existed (the original
-- "any flag => flagged" rule). `make revalidate` is what walks those forward.

ALTER TABLE runs
    ADD COLUMN policy_version smallint;

-- The revalidation scan: judged runs whose policy is behind the current one.
-- Partial on `status <> 'pending'` because pending runs belong to the worker's
-- own queue, not to a re-judge.
CREATE INDEX runs_stale_policy_idx ON runs (policy_version, created_at)
    WHERE status <> 'pending';

-- +goose Down
DROP INDEX runs_stale_policy_idx;
ALTER TABLE runs DROP COLUMN policy_version;
