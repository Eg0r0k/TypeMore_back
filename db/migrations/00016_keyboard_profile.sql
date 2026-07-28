-- +goose Up
--
-- The keyboard heatmap projection (docs/PROFILE.md, "Keyboard"). Per user, per
-- PHYSICAL key (KeyboardEvent.code vocabulary — the layouts asset maps typed
-- characters onto key ids, and 'other' buckets what no layout knows): press
-- count, error count, and the inter-key interval sum/count whose quotient is
-- the mean. Updated INCREMENTALLY inside the replay worker's verdict
-- transaction from the observations goja extracts while the log is already
-- parsed — aggregates never require replaying a log at request time, which is
-- the point of them existing.
CREATE TABLE user_keyboard_profile (
    user_id         uuid             NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    key_id          text             NOT NULL,
    presses         bigint           NOT NULL DEFAULT 0 CHECK (presses >= 0),
    errors          bigint           NOT NULL DEFAULT 0 CHECK (errors >= 0),
    interval_sum_ms double precision NOT NULL DEFAULT 0,
    interval_count  bigint           NOT NULL DEFAULT 0 CHECK (interval_count >= 0),
    PRIMARY KEY (user_id, key_id)
);

-- Exactly-once accounting for the incremental update: a run's contribution is
-- ADDED when a verdict lands accepted and the stamp is off, and REVERSED when
-- a stamped run is re-judged to anything else — so revalidate can walk the
-- whole history (its claim already re-replays every stale run) without ever
-- counting a run twice. The flag lives on runs because it is a fact about the
-- RUN's relationship to the projection, and the verdict transaction already
-- holds the row lock.
ALTER TABLE runs ADD COLUMN keyboard_projected boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE runs DROP COLUMN keyboard_projected;
DROP TABLE user_keyboard_profile;
