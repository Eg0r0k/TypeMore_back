-- +goose Up
--
-- Profile read model (docs/PROFILE.md). The profile aggregates are on-demand
-- SQL over `runs` — nothing is projected, nothing is cached — so the only
-- schema this phase needs is an index for the one table that could not answer
-- its profile question through an existing one:
--
-- leaderboard_entries is keyed (bucket_key, user_id): perfect for "who holds a
-- slot on this board", useless for "which slots does this user hold" — the PB
-- cards' question. The entries table IS the PB store (one row per player per
-- bucket, their best eligible run, maintained by the projection), so the cards
-- cost zero new computation; this index is what makes reading them one index
-- scan instead of a walk of every board.
CREATE INDEX leaderboard_entries_user_idx ON leaderboard_entries (user_id);

-- +goose Down
DROP INDEX leaderboard_entries_user_idx;
