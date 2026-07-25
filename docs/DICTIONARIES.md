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
  { "lang": "german",  "name": "german",     "dictHash": "804728e8", "wordCount": 197,  "bytes": 3003  },
  { "lang": "russian", "name": "russian_1k", "dictHash": "f5aacfd2", "wordCount": 1003, "bytes": 20411 }
]
```

| Field | Meaning |
|---|---|
| `lang` | The language **code** — the file's basename, and the value that travels as `lang` in run/match payloads |
| `name` | The dictionary's own display name; it may differ from the code (`russian` → `russian_1k`) |
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

Seeding is strict: an unparseable file, a nameless or wordless dictionary, or two
dictionaries hashing to the same value is a **startup failure**. A half-seeded
catalogue would advertise a language whose body 404s, or a hash that resolves to
the wrong words.

## Adding a language

1. Drop `internal/replay/dicts/<lang>.json` in place:

   ```json
   { "name": "esperanto", "bcp47": "eo", "words": ["saluton", "mondo", "..."] }
   ```

   `name` and `words` are required. `bcp47`, `rightToleft`, and anything else you
   add ride along untouched — the body is served verbatim.

2. Restart the server. The registry picks the file up, computes its `dictHash`
   through the goja bundle, pre-compresses it, and the catalogue lists it. There
   is no hash to write down and no code to touch. (The dictionaries are embedded
   in the binary, so "restart" means rebuild the binary/image — `make build` or
   `docker compose build app`.)

3. Add the new `lang → dictHash` pair to `publishedHashes` in
   `internal/replay/dictionaries_test.go`. That map is what freezes it (below).

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

## Related

- `ARCHITECTURE.md` §4.5 — seeds & dictionary distribution
- `BACKEND.md` §5 — seed-based generation, why the server never streams words
- `docs/RUNS.md` — `dictHash` in the run payload
- `docs/PROTOCOL.md` §5 — `dictHash` in frozen room settings
- `internal/replay/corejs/README.md` — the vendored bundle and how to rebuild it
