-- +goose Up
--
-- The runs-list keyset cursor becomes an index SEEK (docs/PERFORMANCE.md,
-- zone 9). The profile arc put a 100k-run account on the runs feed, and the
-- zone-9 plan check caught what the old shape actually did at depth: the feed
-- orders by (created_at DESC, id DESC), but the index stopped at
-- (user_id, created_at) — so every page needed an Incremental Sort for the id
-- tiebreak, and the longhand cursor predicate was a FILTER the scan walked up
-- to rather than a start condition (66 ms at row 50 000, linear in depth).
-- Same failure shape the boards fixed in 00011, one table over.
--
-- The replacement index carries the full ordering, so the sort node vanishes;
-- the query side adds a redundant `created_at <= cursor` conjunct that the
-- planner CAN turn into a seek (the OR-form alone, it cannot). Deep pages stop
-- costing more than shallow ones.
CREATE INDEX runs_user_created_id_idx ON runs (user_id, created_at DESC, id DESC);
DROP INDEX runs_user_created_idx;

-- +goose Down
CREATE INDEX runs_user_created_idx ON runs (user_id, created_at DESC);
DROP INDEX runs_user_created_id_idx;
