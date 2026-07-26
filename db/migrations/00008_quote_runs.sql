-- +goose Up
-- Quote runs: relax the dimension rule for fixed-text runs, and ONLY for them
-- (docs/RUNS.md, "Dimensions"; docs/QUOTES.md).
--
-- `runs_one_dimension` has been a strict XOR since 00002: exactly one of
-- duration_ms / word_count, because a seeded run is either "type for 30
-- seconds" or "type 25 words" and there is no third answer. A quote run is that
-- third answer. Its length is a property of the TEXT, not of the session: the
-- player did not choose 25 words, they chose a quote, and the same quote is the
-- same length for everyone who types it. A word_count column here would be a
-- second, unverified copy of something `quotes.length` already knows, and a
-- duration_ms would describe a deadline the run does not have.
--
-- So the rule becomes CONDITIONAL, not relaxed:
--
--   quote  -> both columns NULL, mandatory
--   seeded -> exactly one, unchanged
--
-- Why conditional rather than simply dropped, or widened to "at most one":
-- either of those would weaken the seeded case too, and the seeded case is the
-- overwhelming majority of rows. A seeded run that arrives with both dimensions
-- set, or with neither, is a client that does not know what it played — that is
-- worth catching at the schema level, and it stays caught here. Relaxing a
-- constraint for one shape is not a reason to stop enforcing it for the others.
--
-- run_text_source_kind() (00006) is the single definition of where a run's text
-- came from, and it already coalesces an ABSENT textSource to 'seeded' — which
-- is exactly what every row written before quotes existed carries, and what
-- keeps this migration from having to backfill anything. It is IMMUTABLE, so it
-- is usable in a CHECK; the ADD CONSTRAINT below validates the existing table,
-- and every existing row is seeded with one dimension, so it passes unchanged.
--
-- The service validates the same rule up front (internal/runs/validate.go,
-- validateDimensions) so a client gets a 422 with a code rather than a 500 from
-- a constraint violation. This is the schema-level guarantee behind it.

ALTER TABLE runs DROP CONSTRAINT runs_one_dimension;

ALTER TABLE runs ADD CONSTRAINT runs_one_dimension CHECK (
    CASE run_text_source_kind(setup)
        WHEN 'quote' THEN duration_ms IS NULL AND word_count IS NULL
        ELSE (duration_ms IS NULL) <> (word_count IS NULL)
    END
);

-- +goose Down
-- Restoring the strict XOR would fail on any quote run already stored, which is
-- the honest behaviour: the down path is for a deployment that has not yet
-- accepted one. Delete the quote runs first if you mean it.
ALTER TABLE runs DROP CONSTRAINT runs_one_dimension;

ALTER TABLE runs ADD CONSTRAINT runs_one_dimension CHECK ((duration_ms IS NULL) <> (word_count IS NULL));
