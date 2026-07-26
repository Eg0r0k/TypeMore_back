-- +goose Up
-- Rename the dictionary key `css_code` to `code_css` everywhere it is STORED
-- (docs/DICTIONARIES.md, "Naming contract").
--
-- The catalogue now enforces one naming rule: plain languages keep plain keys,
-- and every code dictionary lives in the `code_<lang>` family. `css_code` was
-- the single violation — and it violated upstream too, whose corpus has always
-- been `code_css`, which is why the vendored quote file is already named that.
--
-- THIS IS A DESTRUCTIVE ONE-TIME KEY RENAME AND IT IS LEGAL ONLY BECAUSE THERE
-- IS NO PRODUCTION DATA YET. There is no dual-read window, no compatibility
-- alias and no back-fill of the old spelling: after this migration the string
-- `css_code` does not exist in this database and any client still sending it
-- gets a language nothing matches. That trade is deliberate and it expires the
-- moment this schema carries real users' runs — from then on a key rename is a
-- new key plus a migration path, not an UPDATE.
--
-- What it does NOT touch, and why:
--
--   * `runs.setup` — the {config, generation, declaration} snapshot carries no
--     language at all (the lang is lifted into its own column at ingest), so
--     there is nothing in the jsonb to rewrite. Verified against the dev DB:
--     zero rows match '%css_code%'.
--   * `runs.dict_hash` and every hash in the replay tables — the dict_hash is
--     FNV-1a over the WORD LIST, so the rename could not have moved it. It is
--     still 55ccd317, frozen in replay.publishedHashes, and every run recorded
--     against it stays replayable. That is the whole reason this rename is an
--     UPDATE of labels and not a data migration.
--   * live room settings — a room is an in-memory actor (internal/ws/room.go)
--     and has no table. Its settings become persistent only when they are
--     frozen into `matches` at match end, which is exactly what the two
--     statements below rewrite. There is no rooms table to invent.

-- Solo runs. The bucket column the leaderboard projection reads.
UPDATE runs SET lang = 'code_css' WHERE lang = 'css_code';

-- Quote corpora. The importer keys on (lang, upstream_id), so leaving these
-- behind would make the next `make import-quotes` publish 21 duplicates under
-- the new id instead of recognising the rows it already wrote.
UPDATE quotes SET lang = 'code_css' WHERE lang = 'css_code';

-- Matches: the lifted column and the frozen settings document it was lifted
-- from. Both are rewritten, because they are two copies of one fact and a
-- migration that fixes only the column leaves the capture disagreeing with it.
UPDATE matches SET lang = 'code_css' WHERE lang = 'css_code';
UPDATE matches
   SET settings = jsonb_set(settings, '{lang}', '"code_css"')
 WHERE settings ->> 'lang' = 'css_code';

-- Leaderboard bucket keys.
--
-- A language key is "<mode>:<durationMs|wordCount>:<lang>:<textSource.kind>"
-- and it is FORMATTED IN EXACTLY ONE PLACE: leaderboard.Bucket.Key. That stays
-- true — this migration rewrites stored strings, it does not become a second
-- producer of the format. Hence the surgical shape: match the third component
-- exactly and splice the new value into it, rather than a LIKE '%css_code%'
-- replace that would also hit a language merely containing the substring.
--
-- Quote keys ("quote:<uuid>") have no third component, so split_part returns
-- the empty string and they are not selected. They carry no language by
-- design — a quote run is ranked inside its quote and nowhere else.
UPDATE leaderboard_entries
   SET bucket_key = split_part(bucket_key, ':', 1) || ':'
                 || split_part(bucket_key, ':', 2) || ':code_css:'
                 || split_part(bucket_key, ':', 4)
 WHERE split_part(bucket_key, ':', 3) = 'css_code';

-- +goose Down
-- The exact inverse. It restores a key the catalogue no longer publishes, so it
-- is only useful for stepping back past this migration with the previous binary.
UPDATE leaderboard_entries
   SET bucket_key = split_part(bucket_key, ':', 1) || ':'
                 || split_part(bucket_key, ':', 2) || ':css_code:'
                 || split_part(bucket_key, ':', 4)
 WHERE split_part(bucket_key, ':', 3) = 'code_css';

UPDATE matches
   SET settings = jsonb_set(settings, '{lang}', '"css_code"')
 WHERE settings ->> 'lang' = 'code_css';
UPDATE matches SET lang = 'css_code' WHERE lang = 'code_css';

UPDATE quotes SET lang = 'css_code' WHERE lang = 'code_css';

UPDATE runs SET lang = 'css_code' WHERE lang = 'code_css';
