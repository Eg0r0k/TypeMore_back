-- +goose Up
--
-- A human's decision about one run's status, and the record that a human made it.
--
-- WHY THIS EXISTS. Until now the replay worker was the only writer of
-- `runs.status`, and its verdict was final in the strongest sense: there was no
-- way to disagree with it. That is fine while every verdict is arithmetic
-- (a seed mismatch, a refused log) and wrong the moment one is a JUDGEMENT.
-- `superhuman-burst` fires on speed, speed is a continuum with real people at
-- the top of it, and `leaderboard_eligible_runs` selects on `status =
-- 'accepted'` — so a wrongly flagged run does not annoy a player, it removes
-- their result from the board with no way back. A check whose false positives
-- are unrecoverable has to be set so timidly that it catches nothing.
--
-- This table is the way back. It is also the reason the thresholds above it can
-- be set where the evidence says rather than where the blast radius allows.
--
-- APPEND-ONLY, ONE ROW PER DECISION. A moderation trail that overwrites itself
-- answers "what is the status now" and not "who decided, when, and why" — and
-- the second question is the one asked when a decision is disputed. `run_id` is
-- therefore not unique; the current decision is the newest row.
CREATE TABLE run_status_overrides (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id     uuid NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    -- Both sides of the move, so the row is readable without reconstructing
    -- what the worker had decided at the time. from_status is what the row held
    -- when the operator acted; a later revalidate cannot change it retroactively
    -- because a run with an override is never re-judged (see below).
    -- `text` with a CHECK, mirroring runs.status (00002) rather than inventing an
    -- enum the rest of the schema does not use.
    from_status text NOT NULL CHECK (from_status IN ('accepted', 'flagged', 'rejected')),
    -- 'pending' is absent from BOTH sides on purpose. A pending run has not been
    -- judged yet, so there is no verdict to disagree with, and moving a run back
    -- to pending would hand it to the worker as new work — which is what
    -- `revalidate` is for and is not an override.
    to_status   text NOT NULL CHECK (to_status IN ('accepted', 'flagged', 'rejected')),
    -- Required, and deliberately not nullable. An override with no stated reason
    -- is indistinguishable from a mistake six months later, and this is the one
    -- place in the system where a human overrules the evidence.
    reason      text NOT NULL CHECK (length(btrim(reason)) > 0),
    decided_by  uuid NOT NULL REFERENCES users (id),
    decided_at  timestamptz NOT NULL DEFAULT now(),
    -- An override that changes nothing is a note, not a decision.
    CONSTRAINT run_status_overrides_moves CHECK (from_status <> to_status)
);

-- The claim predicate's index: "has this run ever been decided by a human".
-- Every revalidation batch asks it once per candidate row.
CREATE INDEX run_status_overrides_run_idx ON run_status_overrides (run_id);

-- The audit read: one operator's decisions, newest first.
CREATE INDEX run_status_overrides_decided_idx ON run_status_overrides (decided_by, decided_at DESC);

-- +goose Down
DROP TABLE run_status_overrides;
