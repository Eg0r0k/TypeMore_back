# TypeMore Dictionaries

Word lists live **here**, in the server, and nowhere else. The frontend ships no
`public/static/languages` any more: it reads a catalogue, resolves a language
code to a content hash, and fetches the body from a hash-addressed URL.

Two reasons the server owns them:

1. **The replay worker needs them.** Validating a run means regenerating its text
   from `seed` + dictionary. The registry that serves clients is the same one
   goja will read from — one source, no chance of the validator and the player
   disagreeing about what "russian" contained.
2. **Immutability is enforceable.** A URL that is a hash of its own content
   cannot go stale, so it can be cached forever, and a run recorded against
   `dict_hash` can always be resolved back to the exact words it was generated
   from (`ARCHITECTURE.md` §4.5).

Files: `internal/replay/dicts/*.json`, compiled into the binary with `go:embed`.

## The corpus

| | |
|---|---|
| Dictionaries | **430** |
| Embedded bytes | **57.15 MB** (budget 60 MB) |
| Distinct base languages | 220 |
| Size variants (`_1k` … `_250k`) | 214 |
| `code_*` dictionaries | 63 |
| Right-to-left | 22 |
| Catalogue payload | 43.3 kB raw, 9.7 kB gzipped |
| Startup seeding | ~6.4 s (budget 10 s) |

**`internal/replay/dicts/IMPORT_MANIFEST.md` is the authority**, one row per
upstream file: its canonical key, display name, byte size, word count, and
whether it was imported or skipped with the reason. The per-language rows are
deliberately not reprinted here — at 430 languages a table in prose is a second
copy to keep in step, and the manifest is the one the import was actually run
from.

Four budgets are asserted rather than assumed, in `corpus_test.go`: total
embedded bytes, `NewRegistry` wall time, one short generated run per language
folded through the goja bundle, and `displayNames` holding in both directions.

**Provenance.** The word lists are vendored from
[monkeytype](https://github.com/monkeytypegame/monkeytype)'s
`frontend/static/languages`, normalised: LF endings (enforced by
`.gitattributes`, because the body is served byte-for-byte and its `bytes` is
published in the catalogue), and upstream-only metadata dropped — `english`
arrived carrying `noLazyMode` and `orderedByFrequency`, which mean nothing here
and would have been served to every client forever. Four fields are kept:
`name`, `words`, `bcp47` where upstream supplies one, and `rightToleft` for the
22 RTL corpora. Nothing is invented: a language with no upstream `bcp47` does
not get one, which is why its runs and board buckets are keyed by the language
code (`german`, `code_css`) rather than a tag.

The RTL flag is the one field whose *spelling* is normalised. Upstream now
writes `rightToLeft`; the corpus and the frontend's `DictionaryBodySchema` both
write `rightToleft`, and serving two names for one flag would have silently
dropped text direction on every newly imported RTL language.

**A dictionary must be able to play the documented game.** `MaxWordCount` is
10 000 words and the ingestion event cap is 120 000 (`docs/RUNS.md`), and
whether a full-length run fits under that cap is a property of the dictionary —
one insert per grapheme plus one commit per word, so a corpus of long tokens
costs more events for the same word count. Ten upstream files are over it
(`english_legal` needs 360 619 events) and were **not** imported.
`TestEveryPublishedDictionaryCanPlayAFullLengthRun` is what enforces that, and
the answer when it fails is to leave the dictionary out, never to raise the
cap.

Adding one is a **vendored file plus a binary rebuild** — see "Adding a
language" below. There is no runtime loading path and deliberately so: a
dictionary the server could pick up without a deploy is a dictionary whose hash
nobody froze.

## Naming contract

**This is binding for every language added from here on.** A dictionary key is
not a label you pick per file; it is an identifier that gets written into runs,
match settings and leaderboard bucket keys, and once written it is expensive to
change. So there is one rule and it has two clauses:

- a **plain language** gets a **plain key**: `english`, `french`, `russian`,
  `russian_empire`, `german`, `japanese`, `arabian`, `chinese`,
  `traditional_chinese`;
- a **code dictionary** gets a `code_<lang>` key — the family, never a variant
  of it: `code_css`, `code_javascript`, `code_python`, `code_go`.

`code_<lang>` and not `<lang>_code` because a common prefix sorts and filters:
the catalogue is ordered by key, so every code dictionary lands together, and
"is this a code language?" is a prefix test rather than a list somebody has to
maintain. Upstream (monkeytype) uses the same shape, which is the second reason:
a vendored corpus whose id already matches needs no mapping row.

`css_code` was the one key that broke both clauses, and it broke them
expensively — the vendored quote corpus is `code_css.json`, so the manifest
carried a rename purely to disagree with itself. It is now `code_css`
everywhere. Its `dict_hash` did not move (55ccd317): the hash is FNV-1a over the
WORD LIST, so no key can reach it, and
`TestRenamingADictionaryKeyDoesNotMoveItsHash` asserts exactly that — which is
what made the rename safe for runs already recorded against it.

### `lang` travels; `name` is shown

These are two different things and conflating them is what put a raw key in
front of a user.

- **`lang` is the key.** It is the only identifier that moves: it is submitted
  with a run, frozen into match settings, and formatted into the bucket key by
  `leaderboard.Bucket.Key`. A client stores it, sends it back, and compares it.
  A client must **never render it**.
- **`name` is the display name**, and the **server owns it**. It comes from
  `displayNames` in `internal/replay/registry.go`, an explicit table — `code_css`
  → "CSS (code)", `arabian` → "Arabic", `russian_empire` →
  "Russian (pre-reform)", `chinese` → "Chinese (simplified)". It is explicit
  because a display name cannot be computed: title-casing the key yields
  "Code Css" and "Russian Empire", which is the same wrong answer as showing the
  key, only better disguised.

A dictionary whose key has no row in that table is a **startup failure**, not a
fallback to the key. Falling back is not a lesser failure mode — it is the
original bug, a catalogue that looks healthy while offering the user an id.

## Endpoints

Both are **public** — no session, no `Origin` check. Dictionaries are static
assets that guests need too. Both answer with the configured CORS origin
(`TYPEMORE_FRONTEND_ORIGIN`).

| Method | Path | Purpose |
|---|---|---|
| GET | `/api/v1/dictionaries` | The catalogue: which languages exist and where their bodies live |
| GET | `/static/dictionaries/{dictHash}.json` | One dictionary body, immutable |

### `GET /api/v1/dictionaries`

```json
[
  { "lang": "german",   "name": "German",     "dictHash": "804728e8", "wordCount": 197,  "bytes": 3003  },
  { "lang": "russian",  "name": "Russian",    "dictHash": "f5aacfd2", "wordCount": 1003, "bytes": 20411 },
  { "lang": "code_css", "name": "CSS (code)", "dictHash": "55ccd317", "wordCount": 57,   "bytes": 1279  }
]
```

| Field | Meaning |
|---|---|
| `lang` | The canonical **key** — the file's basename. The only thing that travels: run submissions, match settings and leaderboard bucket keys all carry it. Clients index on it and **never render it** |
| `name` | The **human display name**, owned by this server (`displayNames` in `internal/replay/registry.go`). Presentation data, never a key: it is what a picker shows, and it is not derivable from `lang` |
| `dictHash` | FNV-1a fingerprint of the word list, and the address of the body |
| `wordCount` | Number of words in the list |
| `bytes` | Exact length of the uncompressed body |

Ordered by `lang`. The catalogue is **not** immutable — publishing a language
changes it — so it is served `Cache-Control: public, max-age=60` with an ETag;
revalidation is a bodyless `304`.

### `GET /static/dictionaries/{dictHash}.json`

Returns the dictionary file **verbatim** — the same bytes the registry loaded,
including fields the server does not model (`bcp47`, `rightToleft`, …).

```
Cache-Control: public, max-age=31536000, immutable
ETag: "804728e8"
Vary: Accept-Encoding
```

An unknown hash is `404 {"error":"not_found", ...}`. Never a redirect to "the
current" version of that language: a run recorded against a retired dictionary
must fail loudly, not silently replay against different words.

### Client flow

```
GET /api/v1/dictionaries        →  pick { lang: "russian", dictHash: "f5aacfd2" }
GET /static/dictionaries/f5aacfd2.json
```

Two hops, but only the first is ever repeated: the body URL is immutable, so the
browser (and any CDN in front of it) keeps it indefinitely. The frontend does the
same lookup in `src/shared/lib/helpers/json-files.ts`.

## Caching contract

| | Catalogue | Body |
|---|---|---|
| `Cache-Control` | `public, max-age=60` | `public, max-age=31536000, immutable` |
| `ETag` | digest of the rendered JSON | the `dictHash` itself |
| `304` on `If-None-Match` | yes | yes |
| Safe to put on a CDN | yes | yes, forever |

**Content is addressed BY HASH, so caching forever is safe by construction.** A
new version of a dictionary is a new hash — therefore a new URL — never a mutated
file. Nothing at a published URL is ever rewritten.

## Serving model

Everything is built once, at startup, and a request only copies bytes:

- Files are read from the embedded FS **once**; there is no runtime file access.
- Each fingerprint is computed by calling `dictVersion` in the **vendored core
  bundle** running in goja (`internal/replay/corejs`). The server never
  reimplements FNV-1a in Go — a drifting hash would silently invalidate every
  stored run. See `internal/replay/corejs/README.md`.
- Each body is gzipped once at `BestCompression`; **both encodings are kept** and
  the request picks one from `Accept-Encoding`. No per-request marshalling, no
  per-request compression.
- The catalogue JSON is marshalled and gzipped once, the same way.

Seeding is strict: an unparseable file, a nameless or wordless dictionary, a
language with **no row in `displayNames`**, or two dictionaries hashing to the
same value is a **startup failure**. A half-seeded catalogue would advertise a
language whose body 404s, a hash that resolves to the wrong words, or a row a
picker can only render as a raw key.

## Adding a language

1. Pick the key by the naming contract above — plain language, plain key; code
   dictionary, `code_<lang>`. The key is the file's basename and it is what will
   be written into runs and bucket keys, so it is the one decision here that is
   expensive to revisit.

2. Drop `internal/replay/dicts/<lang>.json` in place:

   ```json
   { "name": "esperanto", "bcp47": "eo", "words": ["saluton", "mondo", "..."] }
   ```

   `name` and `words` are required. `bcp47`, `rightToleft`, and anything else you
   add ride along untouched — the body is served verbatim. Note that the file's
   `name` is the core bundle's `dictName`, an internal corpus label — it is
   **not** the catalogue's display name and is never shown to anyone.

3. Add the display name to `displayNames` in `internal/replay/registry.go`. This
   is not optional and there is no fallback: without it, the registry refuses to
   build and the server does not start.

4. Restart the server. The registry picks the file up, computes its `dictHash`
   through the goja bundle, pre-compresses it, and the catalogue lists it. There
   is no hash to write down. (The dictionaries are embedded in the binary, so
   "restart" means rebuild the binary/image — `make build` or
   `docker compose build app`.)

5. Add the new `lang → dictHash` pair to `publishedHashes` in
   `internal/replay/dictionaries_test.go`. That map is what freezes it (below).

6. Run the package's tests. Three gates apply to the new file without being
   asked for: `TestEveryLanguageGeneratesAPlayableRun` folds one short run on it
   through the bundle (non-empty targets, a valid verdict, finite metrics, a
   `dictVersion` that round-trips), `TestEveryPublishedDictionaryCanPlayAFullLengthRun`
   checks a 10 000-word run still fits the ingestion caps, and
   `TestEmbeddedCorpusFitsTheBudget` checks the corpus still fits in 60 MB. A
   malformed word list is meant to fail here rather than on the first player who
   picks the language.

Removing a language is the same breaking change as editing one — see below.

## A published `dict_hash` is immutable forever

**Once a dictionary has been served, its word list must never change.**

Every run stores the `dict_hash` it was generated from, and the replay worker
regenerates the text from `seed` + that dictionary. Editing a shipped word
list — adding a word, fixing a typo, reordering — changes the hash, so:

- the old hash resolves to nothing, and **every run recorded against it becomes
  unreplayable**; and
- the leaderboard entries derived from those runs can never be recomputed.

So a dictionary file in `internal/replay/dicts/` is append-only in the sense that
matters: you may add **new** files, you may not rewrite existing ones, and you may
not delete one that has been published.

To change a language's contents, publish it as a **new** dictionary — a new file,
a new code (`german_2k`, `english_1k_v2`) — and let clients migrate. The old hash
keeps resolving; old runs keep replaying.

`TestPublishedHashesAreImmutable` is the tripwire: it pins every published
`lang → dictHash` pair and fails if one moves or disappears. When it fails,
**revert the dictionary edit — never update the golden value.**

### A key rename is a different animal

Renaming a language's **key** does not touch its hash, because the hash never
saw the key: `dictVersion` is FNV-1a over the word list and nothing else. So
`css_code` → `code_css` left 55ccd317 exactly where it was, every run recorded
against it still resolves to the same bytes, and
`TestRenamingADictionaryKeyDoesNotMoveItsHash` asserts that from the words alone
rather than leaving it as an argument.

What a rename *does* touch is every place the key was **stored**: `runs.lang`,
`quotes.lang`, `matches.lang` and the frozen `matches.settings`, and the third
component of `leaderboard_entries.bucket_key`. `db/migrations/00010` rewrites
all five in one transaction — and it is a one-time destructive rewrite with no
dual-read window, which is legal only because there is no production data yet.
Once there is, a key rename is a new key plus a migration path, and the naming
contract above exists so it never has to be either.

## Related

- `ARCHITECTURE.md` §4.5 — seeds & dictionary distribution
- `BACKEND.md` §5 — seed-based generation, why the server never streams words
- `docs/RUNS.md` — `dictHash` in the run payload
- `docs/PROTOCOL.md` §5 — `dictHash` in frozen room settings
- `internal/replay/corejs/README.md` — the vendored bundle and how to rebuild it
