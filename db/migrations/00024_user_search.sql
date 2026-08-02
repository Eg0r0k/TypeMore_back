-- +goose Up
-- Player search (docs/PROFILE.md, "Search"): GET /api/v1/users?q= matches a
-- SUBSTRING of display_name, not a prefix. Handles carry their identity in the
-- middle at least as often as at the front — streaming prefixes (TTV_, xX_),
-- clan suffixes, digits substituted mid-word — so a prefix-only search fails
-- to find a player the searcher knows exists, which is the worst failure mode
-- a search box has. The exact-name lookup (GetPublicProfileUser) is unchanged
-- and still goes through citext's UNIQUE index; this is a second access path,
-- not a replacement.
--
-- Substring matching needs pg_trgm. A btree cannot answer LIKE '%x%' at all,
-- whatever the opclass, so there is no version of this feature that reuses the
-- uniqueness index. The index is built on the EXPRESSION lower(display_name
-- ::text) rather than on the column, for two reasons:
--
--   * gin_trgm_ops is defined over text. Indexing citext directly would rest
--     on a binary-coercibility argument, and an index the planner silently
--     declines to use is worse than no index at all — it looks like the
--     feature is fast right up until production data arrives.
--   * lower() on both sides makes case-insensitivity an explicit property of
--     the SEARCH, independent of what citext's comparison does. Uniqueness and
--     findability are then free to have different rules without one quietly
--     redefining the other.
--
-- The handler REQUIRES q to be at least 3 characters. That is not merely an
-- abuse limit: a trigram index cannot serve a pattern shorter than 3
-- characters, and the planner falls back to a sequential scan over users —
-- exactly the query an anonymous endpoint must not be able to ask for. The
-- rule forbids nothing real, because a display_name shorter than 3 characters
-- is already impossible under the CHECK constraint in 00001.
CREATE EXTENSION IF NOT EXISTS pg_trgm;

CREATE INDEX users_display_name_trgm_idx
    ON users USING gin (lower(display_name::text) gin_trgm_ops);

-- +goose Down
DROP INDEX IF EXISTS users_display_name_trgm_idx;
-- The extension leaves with the only index that uses it. Plain DROP, never
-- CASCADE: if something else has come to depend on pg_trgm by the time this is
-- rolled back, the drop must fail loudly rather than silently take that
-- dependency's index down with it.
DROP EXTENSION IF EXISTS pg_trgm;
