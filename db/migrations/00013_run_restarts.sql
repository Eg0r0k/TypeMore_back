-- +goose Up
--
-- restartsSinceLastSubmit (docs/RUNS.md): how many times the player restarted
-- the test between their previous submission and this one. Restarted runs are
-- never submitted — they have no log, no score, no row — so without this count
-- the profile's "tests started" would be a lie equal to "tests completed". The
-- client counts locally and reports the number with the NEXT submitted run;
-- profile aggregation reads tests_started = count(completed) + sum(restarts).
--
-- Structural bounds mirror ingestion's cap (0..10 000): the count is
-- client-reported and unverifiable — like the declared view-only mods, it is
-- accepted on trust — so the cap is there to bound abuse, not to judge. The
-- DEFAULT 0 is also what every pre-existing row means: the column arrived after
-- those runs landed, and zero restarts is the only claim history can make.
ALTER TABLE runs ADD COLUMN restarts_since_last_submit integer NOT NULL DEFAULT 0
    CONSTRAINT runs_restarts_range CHECK (
        restarts_since_last_submit >= 0 AND restarts_since_last_submit <= 10000
    );

-- +goose Down
ALTER TABLE runs DROP COLUMN restarts_since_last_submit;
