# Replay golden vectors

`vectors/*.json` are real `POST /runs` payloads plus the verdict the worker must
reach for each. They are the drift tripwire between the browser and the server.

## How they are produced

`generate.mjs` drives the **vendored bundle** (`../corejs/core.bundle.js`) in
Node — the same artifact the Go worker runs inside goja, and the same code the
browser runs. It reproduces the client's finish path exactly as
`entities/game/model/store.ts` does it:

- events go through `GameCore.dispatch`; a **rejected** dispatch hands its `seq`
  back, so the accepted log stays contiguous (the seq-hole regression class);
- the live score accumulates with `scoreStep` per accepted event;
- `clientMetrics` = `metricsOf(core)` → `{ wpm, raw, acc }`;
- `clientScore` = `finalizeScoreV2(base, comboPeak, metrics, mode, modMultiplier)`
  — or `scoreOfLog` for the `scoreVersion: 1` vector, which is what a v1 client
  submits.

The expectations therefore come out of **V8**, and the Go test reproduces them in
**goja**. A passing `TestGoldenVectorsReplayBitExact` is evidence that the two
engines agree on the one bundle, which is the whole premise of server-side
replay. Nothing is compared with an epsilon.

Every vector plays against the published `german` dictionary
(`../dicts/german.json`, `dict_hash` `804728e8`), so the registry resolves it
with no test-only seeding.

## What each vector covers

| Vector | Covers |
|---|---|
| `words-clean` | The baseline: ten words, flawless, human cadence |
| `time-clean` | Time mode settled at the deadline by the timer worker — the two-clock check and deadline-anchored metrics |
| `words-mods` | punctuation + numbers + randomCase generation, expert difficulty, blind declared: the scoreV2 mod multiplier, derived from the setup alone |
| `words-rejected-backspace` | Locked backspace and ctrl+backspace at every word boundary — rejected, never logged, and **must not** consume a seq |
| `words-typos-v1` | Typos and corrections (acc < 1, broken combo) submitted under `scoreVersion: 1` |
| `words-one-fast-interval` | One 9 ms interval in an otherwise human run — ordinary key rollover. Raises `min-interval` at a tiny severity and **must be accepted**, with the flag kept. The false-positive case the review policy exists for. |
| `words-bot-cadence` | Every keystroke exactly 80 ms apart, zero variance. No single flag is severe enough alone, but `uniform-intervals` + `zero-variance` is a shape no hand makes — **must be flagged** by the `bot_cadence` rule. |

The last two are synthetic in their *timing* only: the log, the metrics and the
score are still produced by driving the real core, so their flags and severities
are the core's own output rather than numbers invented for a test. They pin the
two ends of the review boundary (docs/REPLAY.md, "Review policy").

`TestGoldenVectorsCoverTheContractSurface` asserts that this table stays true, so
a regeneration cannot quietly drop a case and leave the suite green.

## Regenerating

```sh
node internal/replay/testdata/generate.mjs
```

Do this **only** after a deliberate `make core-bundle` update, and **read the
diff**. A changed expectation is not noise: it means the scoring or metrics
contract moved, and every already-accepted run was judged by the old one. See
the bundle-update procedure in `docs/REPLAY.md`.
