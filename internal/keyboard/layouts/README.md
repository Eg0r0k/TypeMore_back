# Keyboard layouts — the shared data asset

`*.json` here is **data, not code**, and it is the **single source** for every
consumer of "which physical key does this symbol live on":

- the replay worker's `user_keyboard_profile` projection (char → key id, at
  verdict time);
- the profile keyboard heatmap UI (rows/cols/fingers, via
  `GET /api/v1/layouts`);
- the **anticheat bigram heuristics**, when they land: same-finger and
  same-hand transition timing reads THIS file, not a second mapping — two
  copies of "which finger types 'r'" is how the anticheat and the heatmap end
  up disagreeing about the same run.

## Shape

One document per layout:

```json
{ "name": "qwerty", "label": "QWERTY",
  "keys": [ { "id": "KeyF", "row": 2, "col": 3, "finger": "index",
              "hand": "left", "chars": ["f", "F"], "width": 1 } ] }
```

- `id` is the **physical key** in `KeyboardEvent.code` vocabulary (`KeyF`,
  `Semicolon`, `Space`). Both layouts name the SAME physical keys — qwerty's
  `f` and ЙЦУКЕН's `а` are one `KeyA` — which is what makes the projection
  layout-agnostic and is the whole upgrade path: when the projection starts
  consuming log-v2 telemetry, the log already carries these exact ids and the
  char mapping simply stops being consulted for v2 runs.
- `chars` is every symbol the key produces (shifted included). A character in
  no layout's `chars` buckets to the reserved key id **`other`** — never
  dropped, so the aggregates stay complete even for symbols the drawing has no
  key for.
- `row`/`col`/`width` are the drawing grid; `finger`/`hand` are the standard
  touch-typing assignment the anticheat heuristics will read.

## Which layout applies to a run

By the run's dictionary language: a language whose name starts with `ru` or
contains `russian` maps through **jcuken**, everything else through
**qwerty**. That rule lives in `internal/keyboard` (one place), and it is a
heuristic on purpose: the run does not carry the player's physical layout, and
until log-v2 telemetry is consumed this is the honest best guess. Aggregates
are keyed by physical key id, so both layouts fold into ONE portrait per user.

## Editing

Adding a layout is adding a file (and a rule for which languages map through
it). Changing `chars` changes how FUTURE runs project; history is corrected by
`make revalidate`, whose full pass rebuilds the projection run by run.
