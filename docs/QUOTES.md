# TypeMore Quotes

Fixed-text runs (SCORING_CONCEPT §6, ARCHITECTURE.md §4.5). A quote is the
direct osu!-beatmap analogue: everyone types the **same bytes**, so a score only
means something next to other scores on the same text.

That is the whole difference from a seeded run. A seeded run's text is
regenerated from `seed` + dictionary and is effectively infinite; a quote is a
finite, memorisable artefact. Quotes therefore rank **per quote and never
globally** — they are excluded from the score buckets by
`leaderboard_eligible_runs` (`textSource.kind = 'seeded'`, see
[`LEADERBOARDS.md`](LEADERBOARDS.md)) — and, being artefacts, they are
**published, not edited**.

```mermaid
flowchart LR
  U["monkeytype @ e113dff1<br/>frontend/static/quotes"] -->|vendored| V["internal/quote/quotes/*.json<br/>+ MANIFEST.json"]
  V -->|make import-quotes<br/>text_hash via goja| Q[(quotes)]
  Q --> A["GET /api/v1/quotes…"]
```

## Endpoints

All three are **public** — no session, no `Origin` check, not even an optional
one. A guest picking a text to type has no account yet, exactly as with the
dictionary catalogue.

| Method | Path | Returns |
|---|---|---|
| GET | `/api/v1/quotes?lang=&group=&cursor=&limit=` | A page of quote **metadata**. Never the text. |
| GET | `/api/v1/quotes/random?lang=&group=` | **One** quote **with** its text. Starting a quote run is this call. |
| GET | `/api/v1/quotes/{id}` | One quote with its text, **including retired revisions**. |

### `GET /api/v1/quotes`

```json
{
  "quotes": [
    {
      "id": "1f5f1f2c-6f0f-4d5a-9f0a-3f2a1b0c9d8e",
      "lang": "german",
      "upstreamId": 42,
      "source": "Johann Wolfgang von Goethe",
      "length": 187,
      "lenGroup": "medium",
      "textHash": "8b1cf30a"
    }
  ],
  "nextCursor": "Z2VybWFuOjE6MWY1ZjFmMmMt…"
}
```

| Field | Meaning |
|---|---|
| `id` | The quote's server id. The only handle that resolves its text, and what a run/board row records. |
| `lang` | **Our** language id — the same id the dictionary catalogue uses. |
| `upstreamId` | The quote's id inside its upstream corpus. Stable across re-imports; **not** unique on its own (see *Immutability*). |
| `source` | Upstream's attribution, verbatim. |
| `length` | Characters in the text (`char_length`, enforced by a CHECK). |
| `lenGroup` | `short` \| `medium` \| `long` \| `thicc` — the band, not the ordinal. |
| `textHash` | FNV-1a of the text, computed by the vendored core bundle. |

`nextCursor` is absent on the last page.

Query parameters: `lang` and `group` are optional filters; an unknown `lang` is
an empty page, an unknown `group` is **`400`**, never a silently dropped filter.
`limit` defaults to 50 and is clamped to `[1, 200]`.

#### Why there is no `text` here

This endpoint is walkable end to end with a cursor. Putting bodies on it would
make "list the corpus" and "download the corpus in one pass" the same request,
and the corpus is the one asset a competitor could lift wholesale. Text is
reachable exactly one quote at a time: through `/random`, where the *server*
chooses, or through `/{id}`, which you need the id for.

It is enforced in SQL, not only in the handler — `ListQuotes` does not select
the column, so there is no body in the result set for a view struct to leak.

#### Pagination

Keyset, over the total order `(lang, len_group, id)` — which is exactly
`quotes_browse_idx`, so a page is an ordered index scan rather than a sort, and
the trailing primary key makes the order total. The cursor is an opaque
base64url token of that triple.

Total order plus keyset means the walk is **stable under concurrent writes**: a
row inserted before your position cannot shift anything you have already seen,
and one inserted after it simply shows up later. An `OFFSET` pager gets exactly
this case wrong. `TestPaginationIsStableUnderAConcurrentInsert` plants such a
row mid-walk.

### `GET /api/v1/quotes/random`

```json
{
  "id": "1f5f1f2c-…", "lang": "german", "upstreamId": 42,
  "source": "Johann Wolfgang von Goethe",
  "length": 187, "lenGroup": "medium", "textHash": "8b1cf30a",
  "text": "Es ist nicht genug, zu wissen, man muß auch anwenden …",
  "superseded": false
}
```

Same fields as the index plus `text` and `superseded`. Retired revisions are
never drawn — nobody should start a run on a text that has already been
replaced — so `superseded` is always `false` here; it is in the shape so the
single-quote responses are one shape.

`404` when the filter matches nothing (`?lang=german&group=thicc` on a corpus
with no thicc german), rather than an empty body a client has to special-case.

#### Random selection

The draw is done **in SQL** and is exactly uniform. `PickRandomQuote` counts the
candidates and jumps to one offset among them; both reads are served by
`quotes_browse_idx` as index-only scans. On `english`, the largest corpus at
6 488 quotes:

```
Nested Loop (actual time=2.162..2.164 rows=1 loops=1)
  CTE pick
    ->  Limit (actual time=2.129..2.130 rows=1 loops=1)
          InitPlan 1
            ->  Aggregate (actual time=1.405..1.406 rows=1 loops=1)
                  ->  Index Only Scan using quotes_browse_idx on quotes (rows=6488)
                        Index Cond: (lang = 'english'::text)
                        Heap Fetches: 0
          ->  Index Only Scan using quotes_browse_idx on quotes quotes_1 (rows=5273)
                Index Cond: (lang = 'english'::text)
                Heap Fetches: 0
  ->  Index Scan using quotes_pkey on quotes q (rows=1)
Execution Time: 2.250 ms
```

Three things to read off that plan:

- **No Sort node.** `ORDER BY random() LIMIT 1` — the obvious spelling — sorts
  the entire filtered corpus to throw all but one row away. This does not.
- **`Heap Fetches: 0`.** `id` is in the index and the index is partial on
  `NOT superseded`, so the candidate gather never touches the table. Only the
  single winning row is read, by primary key.
- **`NOT MATERIALIZED` is load-bearing.** The `candidates` CTE is referenced
  twice; by default Postgres builds a tuplestore and scans it twice, which
  measured 3.9 ms. Inlining it puts both references back on the index: 2.2 ms.

It is honestly **O(candidates)**, not O(log n): the count is what forces a full
pass over the language's index range. The O(log n) alternative — store nothing
extra, probe with `id >= gen_random_uuid()` and take the first row — was
rejected on two counts: it needs the leading index columns all equality-bound
(so it breaks the moment `group` is omitted, or needs a second index to cover
that case), and it is *gap-weighted* rather than uniform, i.e. a quote whose id
happens to follow a large gap gets drawn more often. Two milliseconds once per
run start is not worth buying a biased draw and an extra index.

### `GET /api/v1/quotes/{id}`

The same body as `/random`, and the **only** read that serves retired
revisions. It must: a run stores the quote id it was played on, and the replay
worker and the per-quote boards resolve the text back through it. A retired
quote that stopped being fetchable would make every run played on it
unwatchable — the exact failure a frozen `dict_hash` exists to prevent
([`DICTIONARIES.md`](DICTIONARIES.md)).

`404` for an unknown id **and** for a malformed one: both mean "this link
resolves to no quote", and distinguishing them only tells a scraper which of its
guesses were well-formed.

## Immutability: published text is never edited

**Once a quote has been served, its bytes must never change.**

The reasoning is the dictionary doctrine, one level down. A seeded run stores
its `dict_hash` and regenerates its words; a quote run stores its quote id and
*is* those bytes. Editing a published quote in place would:

- change what every past score on it was scored against, silently — the
  leaderboard for that quote would be comparing runs on two different texts; and
- break replay, because the log's keystrokes no longer line up with the text.

So the import never issues `UPDATE … SET text`. For each `(lang, upstream_id)`:

| Upstream says | The registry does | Reported as |
|---|---|---|
| nothing published under that key | `INSERT` | *inserted* |
| the same bytes as the published revision | nothing at all | *unchanged* |
| **different** bytes | `INSERT` a new row **beside** the old one, and set `superseded = true` on the previous revision(s) | *superseded* |

`(lang, upstream_id, text_hash)` is unique, which is what turns "same bytes" into
a no-op the schema guarantees rather than something the importer has to remember.
`(lang, upstream_id)` deliberately is **not** unique: that is where the new
revision goes.

A superseded row keeps its text forever. It is excluded from `/quotes` and
`/quotes/random` — nobody starts a new run on it — and stays permanently
resolvable through `/quotes/{id}`. There is no delete path, and there must not be.

## Length groups are **per corpus**

Every corpus carries its own `groups` array, and they are **not** all the same:

| Corpus | short | medium | long | thicc |
|---|---|---|---|---|
| everything else | 0–100 | 101–300 | 301–600 | 601+ |
| **chinese** | **0–30** | **31–80** | **81–200** | **201+** |

Which is obvious once you look at the text: a Chinese character carries about as
much as an English word, so 100 characters of chinese is not a short quote, it is
a long one. Upstream got this right; a server that hard-coded one threshold
table would quietly file the whole chinese corpus one or two bands too low.

The importer therefore reads **each file's own** `groups` array. The manifest
records the same numbers so a re-vendor that changed them fails loudly instead of
shifting every `len_group` under a corpus whose text did not change at all.

`TestChineseThresholdsDifferFromTheRest` is the tripwire, and it is deliberately
two-sided: the tables must differ *and* the majority table must produce a
different band for real vendored quotes. Today that is 3 of 330 chinese quotes
(the corpus's longest is 63 characters, so its own thresholds only ever separate
short from medium) — a small number, and the honest one. A test that demanded a
big one would be asserting something the data does not say.

Current distribution, for orientation:

| Corpus | short | medium | long | thicc |
|---|---|---|---|---|
| english | 926 | 3786 | 1589 | 187 |
| russian | 235 | 436 | 283 | 209 |
| french | 71 | 538 | 483 | 15 |
| german | 155 | 291 | 84 | 30 |
| chinese | 327 | 3 | — | — |
| arabian | 38 | 44 | 7 | — |
| code_python | 7 | 35 | 20 | 14 |
| code_javascript | 4 | 33 | 10 | — |
| css_code | 4 | 11 | 5 | 1 |

## `text_hash` comes from the vendored bundle

`text_hash = core.DictVersion([]string{quote.Text})` — the FNV-1a the **client**
computes, because it is literally the same code running in goja
(`internal/replay/corejs`). There is no Go FNV-1a in this system and there must
never be one.

Two details worth knowing:

- **A one-element slice is the point.** The core joins words with NUL before
  folding them, so one element makes the join a no-op and the digest is exactly
  `fnv1a(text)` — in the *same convention* `dict_hash` already uses, which means
  a quote hash and a dictionary hash are comparable artefacts rather than two
  unrelated 8-hex strings.
- **No new goja binding was added.** The existing `dictVersion` export is
  sufficient. A second hashing entry point would be a second hash to keep in
  step, and a drifting hash silently invalidates every run recorded against it —
  the failure `DICTIONARIES.md` spends a section on.

Note that `text_hash` folds over UTF-16 code units (it is JS) while `length`
counts characters (it is `char_length`). The two agree for everything in the BMP,
and the importer **refuses** a corpus where they diverge rather than storing a
number that means one thing on one side of the wire — see
`corpus.Load`.

## Provenance

Vendored from [monkeytype](https://github.com/monkeytypegame/monkeytype),
`frontend/static/quotes`, at commit
**`e113dff1cfc27cc624f47ac9899d6e287c3fc33f`**. The files sit in
`internal/quote/quotes/` in their **upstream shape**, unmodified, so a diff
against upstream is a plain file diff.

`MANIFEST.json` beside them is the import source of truth. It exists because a
directory listing is not enough:

- upstream's file names are not our language ids (`arabic` → `arabian`,
  `chinese_simplified` → `chinese`, `code_css` → `css_code`);
- it records each corpus's expected quote count and thresholds, so the vendored
  copy can be *checked* rather than trusted; and
- it documents what is deliberately absent.

| Language | Upstream file | Quotes |
|---|---|---|
| `english` | `english.json` | 6488 |
| `russian` | `russian.json` | 1163 |
| `french` | `french.json` | 1107 |
| `german` | `german.json` | 560 |
| `chinese` | `chinese_simplified.json` | 330 |
| `arabian` | `arabic.json` | 89 |
| `code_python` | `code_python.json` | 76 |
| `code_javascript` | `code_javascript.json` | 47 |
| `css_code` | `code_css.json` | 21 |

**9 languages, 9 881 quotes.** Every one of them except `code_python` and
`code_javascript` also has a served dictionary, so a player can do seeded and
quote runs in the same language; those two are quote-only, added explicitly.

### Excluded

The rule is "a quote language must be a language we serve". These three are
served dictionaries with no upstream counterpart:

| Language | Why |
|---|---|
| `japanese` | Served dictionary, but upstream publishes no japanese quote corpus. |
| `traditional_chinese` | Served dictionary, but upstream publishes no `chinese_traditional` corpus. |
| `russian_empire` | Served dictionary (pre-reform orthography); no upstream counterpart. |

`MANIFEST.json`'s own `excluded` array is the authority;
`TestExcludedLanguagesMatchTheDoc` fails if this table drifts from it.

## Re-importing

```
make import-quotes                 # every manifest row
make import-quotes lang=german     # one of them
```

Reads `TYPEMORE_DATABASE_URL` and `TYPEMORE_REPLAY_TIMEOUT` exactly as the
server does, and hashes with the same vendored bundle. Each language is one
transaction: a corpus that fails halfway leaves the previous revision intact
rather than a half-swapped one.

The report is per language and totalled:

```
LANGUAGE         UPSTREAM FILE            QUOTES  INSERTED  SUPERSEDED  UNCHANGED
english          english.json             6488    0         0           6488
…
                 TOTAL                    9881    0         0           9881

unchanged — the vendored corpora are already published verbatim
```

**Running it twice must report all-unchanged the second time.** That is the
observable form of the immutability rule: if a second pass reports anything but
zeros in the first two columns, the import is rewriting published text.
`TestImportIsIdempotent` asserts it on both the row count and on `created_at`
not moving, because an upsert that rewrote every row would leave the counts
identical and still have churned the table.

### Adding a language

1. Drop the upstream file into `internal/quote/quotes/` **verbatim**.
2. Add a `languages` row to `MANIFEST.json`: our `lang` id, the `file`, its
   `quotes` count, its own `groups` array, and a one-line `why`. Copying the
   thresholds from a neighbouring row without looking is the bug this file
   exists to prevent.
3. If it was in `excluded`, remove it from there and update the table above.
4. `make import-quotes lang=<id>`, then run it again and confirm all-unchanged.

Nothing else. There is no code to touch and no hash to write down.

### Changing a language

Re-vendor the file at a new upstream commit and update `MANIFEST.json`'s
`upstream.commit` and the affected counts. Changed texts publish as new
revisions and retire the old ones — which is a *deliberate* event, not a
routine one: every run played on a retired quote keeps replaying against its own
bytes, but the per-quote board for that text now splits across two quote ids.
Read the `superseded` column of the report before you deploy it.

Removing a language from the manifest stops it being re-imported; it does **not**
delete its rows, and it must not. There is no path in this system that deletes a
published quote.

## Related

- `SCORING_CONCEPT.md` §6 — quotes as beatmaps, why they are unranked globally
- `ARCHITECTURE.md` §4.5 — seeds, dictionaries and the immutability doctrine
- [`DICTIONARIES.md`](DICTIONARIES.md) — the frozen `dict_hash` this mirrors
- [`LEADERBOARDS.md`](LEADERBOARDS.md) — why quote runs never reach a score bucket
- `internal/replay/corejs/README.md` — the vendored bundle `text_hash` comes from
