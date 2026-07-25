-- +goose Up
-- The quote registry: fixed-text runs (SCORING_CONCEPT §6, docs/QUOTES.md).
--
-- A quote is the osu!-beatmap analogue — everyone types the SAME bytes, so a
-- quote is a *published artefact*, not a row someone edits. That single fact
-- shapes the whole table: `superseded` instead of UPDATE, a `text_hash` that
-- travels with runs, and a uniqueness rule that lets a corrected text land
-- BESIDE its predecessor rather than on top of it. It is the same doctrine a
-- frozen `dict_hash` gets in docs/DICTIONARIES.md, for the same reason: a run
-- must replay forever against the exact bytes it was played on.
--
-- Rows arrive only through `make import-quotes`, driven by
-- internal/quote/quotes/MANIFEST.json. Nothing in the request path writes here.
CREATE TABLE quotes (
    id          uuid PRIMARY KEY,

    -- OUR language id — the same id the dictionary catalogue serves, so a quote
    -- and a seeded run in the same language agree on what that language is
    -- called. Upstream's own file names differ for several of them; the
    -- manifest owns that mapping and this column only ever sees the result.
    lang        text        NOT NULL,
    -- The quote's id WITHIN its upstream corpus. (lang, upstream_id) is the
    -- stable identity a re-import keys on; it is deliberately NOT unique on its
    -- own, because a corrected text is a new row under the same key.
    upstream_id int         NOT NULL,

    text        text        NOT NULL,
    source      text        NOT NULL,
    -- Character count. Upstream ships it alongside the text and the len_group
    -- thresholds are expressed in it, so it is stored rather than derived: a
    -- query that filters on length must not have to call char_length() per row.
    length      int         NOT NULL,
    -- 0=short 1=medium 2=long 3=thicc, assigned from THAT CORPUS'S OWN `groups`
    -- array — the thresholds are per-corpus and chinese genuinely differs, so
    -- the number is imported, never recomputed from a constant table here.
    len_group   smallint    NOT NULL,

    -- FNV-1a of the text, computed by the VENDORED CORE BUNDLE running in goja
    -- (replay.Core.DictVersion over a one-element slice), never by a Go or SQL
    -- reimplementation. One hashing implementation in the system is the whole
    -- point: a drifting hash silently invalidates every run recorded against it.
    text_hash   text        NOT NULL,

    -- Retired by a later revision of the same (lang, upstream_id). Excluded
    -- from browsing and from random selection; ALWAYS still fetchable by id,
    -- because every run ever played on it resolves its text through that id.
    superseded  boolean     NOT NULL DEFAULT false,
    created_at  timestamptz NOT NULL DEFAULT now(),

    -- `length` is what clients page and filter on and what the importer copies
    -- from upstream. If the two ever disagree, the corpus is corrupt and every
    -- length-derived decision (len_group, star rating later) is wrong — so it
    -- is a constraint, not a test. char_length() counts characters, which is
    -- what upstream's `length` means; note this is NOT identical to the UTF-16
    -- code-unit count the core hashes over, and only the hash cares about that.
    CONSTRAINT quotes_length_matches_text CHECK (length = char_length(text)),
    -- An out-of-range group is an importer bug that would otherwise surface as
    -- a quote nobody can filter to. Four groups is the upstream contract.
    CONSTRAINT quotes_len_group_range CHECK (len_group BETWEEN 0 AND 3),
    -- An empty quote is not a run anybody can play, and it would hash to the
    -- empty digest for every language at once.
    CONSTRAINT quotes_text_not_empty CHECK (length > 0)
);

-- The import key. Re-importing the SAME bytes must be a no-op, which this turns
-- into a plain ON CONFLICT rather than a read-then-write race; re-importing
-- CHANGED bytes is a different text_hash, so it inserts instead of colliding —
-- which is exactly the supersede path, enforced by the schema rather than by
-- the importer remembering to.
CREATE UNIQUE INDEX quotes_revision_idx ON quotes (lang, upstream_id, text_hash);

-- The public read path: browse a language (optionally one length group) and
-- pick a random quote out of it. Partial on `NOT superseded` because both
-- public endpoints exclude retired revisions — the retired rows are dead weight
-- in this index and are reached by primary key only.
--
-- `id` is appended to the spec's (lang, len_group) so the ordering is TOTAL:
-- keyset pagination needs a unique final tiebreak, and having it in the index
-- keeps the page an ordered index scan instead of a sort. The random pick reads
-- the same range as an index-only scan (docs/QUOTES.md, "Random selection").
CREATE INDEX quotes_browse_idx ON quotes (lang, len_group, id) WHERE NOT superseded;

-- +goose Down
DROP INDEX quotes_browse_idx;
DROP INDEX quotes_revision_idx;
DROP TABLE quotes;
