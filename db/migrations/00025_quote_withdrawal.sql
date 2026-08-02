-- +goose Up
-- Quote withdrawal (docs/REPORTS.md, "The action a report points at"): a
-- moderator's power to take a bad quote out of circulation.
--
-- This is a NEW column and deliberately NOT a reuse of `superseded`, even
-- though both end with "this quote stops being served". They are owned by
-- different writers and mean different things:
--
--   * superseded is VERSIONING, owned by `make import-quotes`. It means "a
--     newer revision of this (lang, upstream_id) exists". The importer both
--     sets and CLEARS it — RepublishQuoteRevision un-retires a revision whose
--     bytes upstream reverted to — so a moderator writing `superseded = true`
--     would have their decision silently undone by the next import. That is
--     the whole reason this column exists.
--   * withdrawn_at is MODERATION, owned by the admin surface. Nothing in the
--     import path reads or writes it, so a withdrawal survives every re-import.
--
-- What withdrawal does NOT do is make the quote unresolvable. It removes the
-- quote from DISCOVERY (browsing, random selection, the board index) and never
-- from RESOLUTION by id: every run ever played on this quote must keep
-- replaying against the exact bytes it was played on, which is the same frozen
-- -bytes rule that makes a retired revision still fetchable (00007's header,
-- GetQuote's comment). A moderator can stop a quote being handed out; nobody
-- can make history unreplayable.
ALTER TABLE quotes
    ADD COLUMN withdrawn_at     timestamptz,
    ADD COLUMN withdrawn_by     uuid REFERENCES users (id) ON DELETE SET NULL,
    ADD COLUMN withdrawn_reason text,
    -- A withdrawal with no note is one nobody can review later — the same rule
    -- the ban surface enforces on `reason`. withdrawn_by is deliberately NOT in
    -- this connective: it is ON DELETE SET NULL like a ban's actor columns, and
    -- deleting a moderator's account must not break a CHECK on a historical row.
    ADD CONSTRAINT quotes_withdrawal_complete
        CHECK ((withdrawn_at IS NULL) = (withdrawn_reason IS NULL));

-- The browse index is REBUILT rather than left alone. Its partial predicate has
-- to carry the new condition too: with only `NOT superseded` in the index,
-- `withdrawn_at IS NULL` becomes a post-index filter that must visit the heap,
-- which costs exactly the property the index exists for. PickRandomQuote's two
-- reads are INDEX-ONLY scans by design (measured 3.9 ms -> 2.2 ms on english,
-- docs/QUOTES.md "Random selection"), and a filter needing the heap would take
-- that back without failing any test.
--
-- The predicate stays immutable — `withdrawn_at IS NULL` is a column test, not
-- a now() comparison — so it is legal in an index predicate, unlike the ban
-- expiry rule that forced active_bans to be a view (00012).
DROP INDEX quotes_browse_idx;
CREATE INDEX quotes_browse_idx ON quotes (lang, len_group, id)
    WHERE NOT superseded AND withdrawn_at IS NULL;

-- The board index asks "which quotes are withdrawn" on every request, to drop
-- their boards from the listing. This partial index makes that an index-only
-- scan over a set that is normally EMPTY — the answer costs nothing precisely
-- because withdrawal is rare, and it stays cheap as the corpus grows because
-- the index holds withdrawn rows only.
CREATE INDEX quotes_withdrawn_idx ON quotes (id) WHERE withdrawn_at IS NOT NULL;

-- +goose Down
DROP INDEX quotes_withdrawn_idx;
DROP INDEX quotes_browse_idx;
CREATE INDEX quotes_browse_idx ON quotes (lang, len_group, id) WHERE NOT superseded;
ALTER TABLE quotes
    DROP CONSTRAINT quotes_withdrawal_complete,
    DROP COLUMN withdrawn_reason,
    DROP COLUMN withdrawn_by,
    DROP COLUMN withdrawn_at;
