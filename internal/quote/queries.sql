-- The quote registry (docs/QUOTES.md).
--
-- Reads are public and text-frugal: the browse queries never SELECT `text`, so
-- the corpus cannot be walked out of the database through the paging endpoint
-- even by a handler bug — the column is not in the result set to leak.
--
-- Writes happen only in `make import-quotes`. Nothing here UPDATEs `text`:
-- publishing a corrected quote inserts a new revision and retires the old one,
-- because every stored run resolves its bytes through the row it was played on.

-- name: ListQuotes :many
-- One page of published quote metadata, in the browse ordering.
--
-- (lang, len_group, id) is the quotes_browse_idx column list, so the index
-- supplies the order and the page is an ordered index scan rather than a sort.
-- The trailing id is a primary key, which makes the ordering TOTAL — the
-- property keyset pagination needs, and the reason a row inserted before the
-- cursor cannot shuffle anything the walk has already returned.
--
-- The optional filters are written as `$n IS NULL OR col = $n` rather than as
-- separate query variants. With a bound parameter Postgres folds the IS NULL
-- away in a custom plan and uses the column as a scan key; under a generic plan
-- it degrades to a filter over the partial index, which on a 2 286-row corpus
-- is an index-only scan measured in microseconds. Six near-identical queries
-- would buy nothing but six converters to keep in step.
--
-- `withdrawn_at IS NULL` joins `NOT superseded` as the OTHER half of "published"
-- (00025). Both are in quotes_browse_idx's partial predicate, so neither costs a
-- filter; they are separate columns because they have separate owners — the
-- importer moves one, a moderator moves the other.
SELECT id, lang, upstream_id, source, length, len_group, text_hash, created_at
FROM quotes
WHERE NOT superseded
  AND withdrawn_at IS NULL
  AND (sqlc.narg(lang)::text IS NULL OR lang = sqlc.narg(lang)::text)
  AND (sqlc.narg(len_group)::smallint IS NULL OR len_group = sqlc.narg(len_group)::smallint)
  AND (NOT @after_cursor::boolean
       OR (lang, len_group, id) > (@after_lang::text, @after_group::smallint, @after_id::uuid))
ORDER BY lang, len_group, id
LIMIT @row_limit;

-- name: PickRandomQuote :one
-- One published quote, drawn uniformly from the rows the filter admits.
--
-- Done in SQL, and deliberately NOT `ORDER BY random() LIMIT 1`: that sorts the
-- whole filtered corpus to throw all of it away but one row. This counts the
-- candidates and then jumps to one offset among them, both reads served by
-- quotes_browse_idx as INDEX-ONLY scans — `id` is in the index and the partial
-- predicate already excludes superseded rows, so neither read touches the heap
-- and there is no Sort node anywhere in the plan.
--
-- NOT MATERIALIZED is load-bearing. `candidates` is referenced twice, which by
-- default makes Postgres build a tuplestore and scan it twice; inlining it
-- turns both references back into index-only scans and measured 3.9 ms -> 2.2 ms
-- on english, the largest corpus (docs/QUOTES.md, "Random selection"). Written
-- once rather than as two copies of the predicate so the filter cannot drift.
--
-- It is O(candidates) rather than O(log n): the count is what forces a full
-- pass. That is the price of an EXACTLY uniform draw, and it is the right trade
-- here — see docs/QUOTES.md for the index-probe alternative and why a
-- gap-weighted draw was not worth an extra index.
WITH candidates AS NOT MATERIALIZED (
    SELECT id
    FROM quotes
    WHERE NOT superseded
      AND withdrawn_at IS NULL
      AND (sqlc.narg(lang)::text IS NULL OR lang = sqlc.narg(lang)::text)
      AND (sqlc.narg(len_group)::smallint IS NULL OR len_group = sqlc.narg(len_group)::smallint)
),
pick AS (
    SELECT id FROM candidates
    OFFSET (SELECT floor(random() * count(*))::bigint FROM candidates)
    LIMIT 1
)
SELECT q.id, q.lang, q.upstream_id, q.text, q.source, q.length, q.len_group,
       q.text_hash, q.superseded, q.created_at
FROM quotes q
         JOIN pick ON pick.id = q.id;

-- name: GetQuote :one
-- One quote by id, text included, SUPERSEDED AND WITHDRAWN REVISIONS INCLUDED.
-- This is the only read that crosses that line, and it has to: a run recorded
-- against a retired or withdrawn quote must resolve its exact bytes forever or
-- it stops being replayable — the same guarantee a frozen dict_hash gives
-- seeded runs. Withdrawal is a discovery rule, never a resolution rule (00025).
--
-- Both states ride along on the row so a client can say "this quote is no
-- longer offered" instead of silently linking to something the browse endpoint
-- will never return again.
SELECT id, lang, upstream_id, text, source, length, len_group, text_hash,
       superseded, (withdrawn_at IS NOT NULL)::boolean AS withdrawn, created_at
FROM quotes
WHERE id = @id;

-- --- import (make import-quotes) ---

-- name: FindQuoteRevision :one
-- The revision of (lang, upstream_id) that already carries these exact bytes.
-- Keyed on the hash rather than on the text so the comparison is an index
-- lookup on quotes_revision_idx instead of a full text equality.
SELECT id, superseded
FROM quotes
WHERE lang = @lang AND upstream_id = @upstream_id AND text_hash = @text_hash;

-- name: InsertQuoteRevision :one
-- Publish new bytes under an existing or new (lang, upstream_id). The id is
-- generated here rather than in Go so the whole import is one round trip per
-- statement and the database owns row identity, as everywhere else.
INSERT INTO quotes (id, lang, upstream_id, text, source, length, len_group, text_hash)
VALUES (gen_random_uuid(), @lang, @upstream_id, @text, @source, @length, @len_group, @text_hash)
RETURNING id;

-- name: SupersedeOtherRevisions :execrows
-- Retire every OTHER published revision of this key. Note what it does not do:
-- it never touches `text`, so the retired row still serves the exact bytes its
-- runs were played on. The rowcount is what tells the importer it superseded
-- something rather than published a first revision.
UPDATE quotes
SET superseded = true
WHERE lang = @lang AND upstream_id = @upstream_id AND id <> @keep_id AND NOT superseded;

-- name: RepublishQuoteRevision :execrows
-- Un-retire a revision whose bytes upstream has reverted to. Also not a text
-- edit: it only moves which revision of the key is the current one, and the
-- rowcount is 0 whenever the revision was already current — which is what keeps
-- an unchanged re-import reporting unchanged.
UPDATE quotes
SET superseded = false
WHERE id = @id AND superseded;

-- name: CountQuotes :one
SELECT count(*) FROM quotes;

-- --- moderation (the admin surface, docs/REPORTS.md) ---

-- name: WithdrawQuote :one
-- Take a quote out of circulation. Idempotent in the useful direction: a quote
-- already withdrawn keeps its ORIGINAL withdrawal — the first moderator's
-- timestamp, actor and reason are the record, and a second call must not
-- rewrite who decided what and when. The returned row is what the handler
-- diffs to answer "did this call change anything".
UPDATE quotes
SET withdrawn_at     = coalesce(withdrawn_at, now()),
    withdrawn_by     = CASE WHEN withdrawn_at IS NULL THEN @actor::uuid ELSE withdrawn_by END,
    withdrawn_reason = CASE WHEN withdrawn_at IS NULL THEN @reason::text ELSE withdrawn_reason END
WHERE id = @id
RETURNING id, withdrawn_at, withdrawn_by, withdrawn_reason,
          -- Whether THIS call is the one that withdrew it. The comparison is
          -- against the transaction clock, so it is true only for the row this
          -- statement just changed.
          (withdrawn_at = now()) AS newly_withdrawn;

-- name: RestoreQuote :execrows
-- Put a withdrawn quote back into circulation. The rowcount is 0 when there was
-- nothing to restore, which is how the handler answers idempotently rather than
-- inventing a 404 for a quote that exists and was simply never withdrawn.
UPDATE quotes
SET withdrawn_at = NULL, withdrawn_by = NULL, withdrawn_reason = NULL
WHERE id = @id AND withdrawn_at IS NOT NULL;

-- name: GetQuoteWithdrawal :one
-- The moderation view of one quote: identity plus the withdrawal record. Text
-- included — a moderator deciding whether to withdraw a quote has to read it,
-- and this route is behind a permission gate rather than on the public surface.
SELECT id, lang, upstream_id, text, source, length, len_group, superseded,
       withdrawn_at, withdrawn_by, withdrawn_reason, created_at
FROM quotes
WHERE id = @id;

-- name: ListWithdrawnQuoteIDs :many
-- Every withdrawn quote, id only. The leaderboard's board index reads this to
-- drop those boards from its listing; it is a separate query rather than a join
-- because rebuilding a bucket key in SQL would make a second producer of a
-- format that has exactly one (internal/leaderboard/bucket.go).
--
-- Served index-only from quotes_withdrawn_idx over a set that is normally
-- empty, which is what makes it affordable on a public read path.
SELECT id FROM quotes WHERE withdrawn_at IS NOT NULL;
