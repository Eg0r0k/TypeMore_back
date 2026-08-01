-- +goose Up
--
-- The keyboard projection stamp moves off `runs` — the second half of 00019's
-- untangling. 00016 put the exactly-once flag on the runs row because "the
-- verdict transaction already holds the row lock"; true, but it also made the
-- keyboard domain the only code in the tree that WRITES another domain's table
-- (a raw UPDATE runs in internal/keyboard/pgstore, outside any queries.sql).
--
-- A presence table keeps every property the boolean had and returns the write
-- to its owner: a run_id row here means "this run's contribution is counted
-- in user_keyboard_profile"; stamping is an INSERT, unstamping (a demotion's
-- reversal) is a DELETE. The exactly-once serialization never came from the
-- column's placement — it comes from the fact that every stamp read and write
-- happens inside a verdict transaction that holds the claimed run's row lock
-- (FOR UPDATE SKIP LOCKED), and that lock is on the runs row regardless of
-- which table the stamp lives in.
--
-- Rejected alternative: a flag on user_keyboard_profile. Wrong grain — the
-- stamp is per RUN, the profile rows are per (user, key), and a reversal must
-- know whether THIS run was counted, not whether any run was.
--
-- ON DELETE CASCADE mirrors the aggregate's own lifecycle: deleting a user
-- cascades runs -> stamps exactly as it cascades user_keyboard_profile.
CREATE TABLE keyboard_projected_runs (
    run_id uuid PRIMARY KEY REFERENCES runs (id) ON DELETE CASCADE
);

INSERT INTO keyboard_projected_runs (run_id)
SELECT id FROM runs WHERE keyboard_projected;

ALTER TABLE runs DROP COLUMN keyboard_projected;

-- +goose Down
ALTER TABLE runs ADD COLUMN keyboard_projected boolean NOT NULL DEFAULT false;

UPDATE runs
SET keyboard_projected = true
WHERE id IN (SELECT run_id FROM keyboard_projected_runs);

DROP TABLE keyboard_projected_runs;
