-- +goose Up
--
-- Two integrity facts the match capture has relied on by convention since
-- 00003, promoted into the schema.
--
-- One seat per player per match. The relay persists a match once, at match
-- end, one match_runs row per seat — but nothing structural said so, and a
-- persist bug (a retry, a double end-of-match) would have written a second
-- capture for the same seat. Every future consumer of this table would then
-- double-count silently: a match replay worker would judge one player twice,
-- a history feed would show the race twice. The constraint makes the second
-- write an ERROR instead.
--
-- Deliberately NO ON CONFLICT in the persist path (internal/ws/wspg): the
-- persist runs once at match end, so a duplicate seat is not a benign retry to
-- absorb — it is a relay bug to surface. Silent idempotency here would trade a
-- loud crash log for a quiet lie about how many times the persist ran.
--
-- The paired index: a UNIQUE constraint on (match_id, player_id) is backed by
-- an index whose match_id prefix serves exactly the scans match_runs_match_idx
-- existed for (persist/read one match's runs together), so the old index is
-- retired rather than duplicated.
ALTER TABLE match_runs
    ADD CONSTRAINT match_runs_one_seat_per_player UNIQUE (match_id, player_id);
DROP INDEX match_runs_match_idx;

-- A match cannot end before it starts. go_at and ended_at are both NOT NULL —
-- a match is only persisted once it has both — so the plain comparison has no
-- NULL corner. Equality is allowed: a zero-duration race is degenerate but
-- representable (everyone left at go), while a negative one can only be a
-- clock or capture bug and deserves to fail the write that carries it.
ALTER TABLE matches
    ADD CONSTRAINT matches_ended_after_go CHECK (ended_at >= go_at);

-- +goose Down
ALTER TABLE matches DROP CONSTRAINT matches_ended_after_go;

CREATE INDEX match_runs_match_idx ON match_runs (match_id);
ALTER TABLE match_runs DROP CONSTRAINT match_runs_one_seat_per_player;
