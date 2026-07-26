// Golden-vector generator for the replay worker.
//
// HOW THESE VECTORS ARE PRODUCED (and why that matters)
// -----------------------------------------------------
// Every vector is a real POST /runs payload built by driving the VENDORED core
// bundle (../corejs/core.bundle.js) in Node — the same artifact the Go worker
// runs in goja, and the same code the browser runs. The client numbers are
// produced the way the app produces them (entities/game/model/store.ts):
//
//   * events are dispatched through `GameCore.dispatch`; a REJECTED dispatch
//     returns its seq, so the accepted log stays contiguous (store.ts insert());
//   * the live score accumulates with `scoreStep` per accepted event;
//   * clientMetrics = metricsOf(core)            → { wpm, raw, acc }
//   * clientScore   = finalizeScoreV2(base, comboPeak, metrics, mode, modMult)
//     (or scoreOfLog for the scoreVersion-1 vector, which is what a v1 client
//      submits — score.test.ts proves the live fold and the batch agree).
//
// The Go test then replays each payload through goja and demands the SAME
// numbers, bit for bit. Because the expectations come from V8 and the check runs
// in goja, a passing test proves the two engines agree on the one bundle — which
// is the entire premise of server-side replay.
//
// Regenerate with:  node internal/replay/testdata/generate.mjs
// Do that ONLY after a deliberate core-bundle update, and read the diff: a
// changed expectation means the scoring or metrics contract moved.

import { readFileSync, writeFileSync, mkdirSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'

const here = dirname(fileURLToPath(import.meta.url))
const outDir = join(here, 'vectors')

const bundleSrc = readFileSync(join(here, '..', 'corejs', 'core.bundle.js'), 'utf8')
const core = new Function(`${bundleSrc}\n;return TypeMoreCore;`)()

const {
  EVENT_LOG_VERSION,
  GameCore,
  commitEvent,
  deleteEvent,
  dictVersion,
  finalizeScoreV2,
  generateWords,
  initialScoreState,
  insertEvent,
  metricsOf,
  modMultiplierV1,
  scoreOfLog,
  scoreStep
} = core

// The dictionary every registry-backed vector plays against: a real published
// one, so the registry resolves its hash without any test-only seeding.
const LANG = 'german'
const dictDoc = JSON.parse(readFileSync(join(here, '..', 'dicts', `${LANG}.json`), 'utf8'))
const dictionary = { name: dictDoc.name, bcp47: dictDoc.bcp47 ?? LANG, words: dictDoc.words }
const DICT_HASH = dictVersion(dictionary.words)

/**
 * A code dictionary: tokens carry their own layout (`\t` head, `\n` tail) the
 * way monkeytype's code languages do. It is NOT published — `dicts/` holds no
 * code language yet — so the vector built from it carries the dictionary inline
 * and is replayed straight through the core instead of the registry-backed
 * worker. It exists to pin the separator rule: a word ending in `\n` typed its
 * own separator and must NOT also be credited a space.
 */
const codeDictionary = {
  name: 'code_javascript_vector',
  bcp47: 'en',
  words: [
    'function',
    'greet(name)',
    '{\n',
    '\tconst',
    'msg',
    '=',
    '`hi`;\n',
    '\tconsole.log(msg);\n',
    '}\n',
    'export',
    'default',
    'greet;\n'
  ]
}
const CODE_DICT_HASH = dictVersion(codeDictionary.words)

/** Deterministic jitter so regenerating produces byte-identical vectors. */
function lcg(seed) {
  let s = seed >>> 0
  return () => {
    s = (s * 1664525 + 1013904223) >>> 0
    return s / 0x100000000
  }
}

const coreConfig = (over = {}) => ({
  mode: 'words',
  durationMs: 60_000,
  maxExtraChars: 20,
  difficulty: 'normal',
  nospace: false,
  minWpm: 0,
  ...over
})

const generationConfig = (over = {}) => ({
  mode: 'words',
  length: 10,
  punctuation: false,
  numbers: false,
  randomCase: false,
  reverse: false,
  ...over
})

const noMods = { blind: false, fading: false, flashlight: false }

/**
 * A typing session that mirrors the game store's dispatch semantics exactly:
 * stamp → dispatch → on reject, hand the seq back so the log stays contiguous.
 */
function session({
  config,
  generation,
  declaration,
  seed,
  jitterSeed = 7,
  fixedInterval = 0,
  commitAtSameInstant = false,
  dict = dictionary,
  dictHash = DICT_HASH
}) {
  const generated = generateWords(dict, { seed, dictVersion: dictHash, generation })
  if (generated.isErr()) throw new Error(`generation failed: ${generated.error.message}`)
  const words = generated.value.words

  const gameCore = new GameCore({ config, words })
  const ctx = { config, words }
  const scoreState = initialScoreState()
  const rng = lcg(jitterSeed)

  let seq = 0
  let t = 0
  let rejected = 0

  // `fixedInterval` is the bot cadence: an identical gap every keystroke, which
  // is what makes zero-variance and uniform-intervals fire. Otherwise 60-150 ms
  // of jitter — human, and comfortably above the 15 ms min-interval threshold.
  const step = (override) => {
    if (override !== undefined) {
      t += override
      return
    }
    t += fixedInterval > 0 ? fixedInterval : 60 + Math.floor(rng() * 90)
  }

  const send = (make) => {
    seq += 1
    const event = make(seq, t)
    const result = gameCore.dispatch(event)
    if (result.isOk()) {
      scoreStep(scoreState, event, ctx)
      return true
    }
    seq -= 1 // store.ts: rejected events never enter the log and return their seq
    rejected += 1
    return false
  }

  return {
    words,
    core: gameCore,
    get rejected() {
      return rejected
    },
    get now() {
      return t
    },
    type(text, dtOverride) {
      step(dtOverride)
      return send((s, at) => insertEvent(s, at, text))
    },
    commit() {
      // Under a fixed cadence the commit must share the previous instant, or
      // the gap across a word boundary would be two steps and the interval
      // series would no longer be uniform (mirrors the core suite's typeAll).
      if (!commitAtSameInstant) step()
      return send((s, at) => commitEvent(s, at))
    },
    back(unit = 'char') {
      step()
      return send((s, at) => deleteEvent(s, at, unit))
    },
    tick(at) {
      t = at
      gameCore.tick(at)
    },
    finish() {
      const metrics = metricsOf(gameCore)
      const modMultiplier = modMultiplierV1({ generation, config }, declaration)
      return {
        words,
        log: { version: EVENT_LOG_VERSION, events: gameCore.events },
        metrics,
        scoreV2: finalizeScoreV2(scoreState.base, scoreState.comboPeak, metrics, config.mode, modMultiplier),
        scoreV1: scoreOfLog(gameCore.events, ctx)
      }
    }
  }
}

/** Type a word correctly, one grapheme at a time. */
function typeWord(s, word) {
  for (const ch of word) s.type(ch)
}

/**
 * The quote a fixed-text run is played on: the registry row, restated here so
 * the generator is self-contained.
 *
 * It is chosen to exercise things a "hello world" text cannot. `strebt.` and
 * `erlösen!` are punctuation the player has to type VERBATIM — nothing
 * generated it, so nothing can regenerate it either. The DOUBLE SPACE after
 * the first sentence is the real trap: `generateWords` splits the text on ' '
 * and drops the empties, so the two spaces collapse into one separator and the
 * word count is 16, not 17. And `bemüht` / `können` / `erlösen` put non-ASCII
 * through `dictVersion`, which folds over UTF-16 code units — the one place V8
 * and goja could plausibly disagree about a hash.
 */
const QUOTE_ID = '3f2a1b0c-9d8e-4c7b-8a6f-5e4d3c2b1a09'
const QUOTE_TEXT =
  'Es irrt der Mensch, solang er strebt.  Wer immer strebend sich bemüht, den können wir erlösen!'
const QUOTE_HASH = dictVersion([QUOTE_TEXT])

/**
 * The stand-in dictionary a quote run is replayed with, mirroring the Go side's
 * `Core.quoteDict`. A quote run has no dictionary: `generateWords` reads only
 * `dict.name` on the quote branch, and the empty word list is deliberate — if
 * the quote branch ever stopped firing, the seeded path would fail loudly on
 * an empty dictionary instead of producing plausible nonsense.
 */
const quoteDictionary = { name: 'quote', bcp47: 'de', words: [] }

/** The generation config of a quote run, text included (the CLIENT's copy). */
const quoteGeneration = (over = {}) => ({
  mode: 'quote',
  length: QUOTE_TEXT.split(' ').filter((w) => w.length > 0).length,
  punctuation: false,
  numbers: false,
  randomCase: false,
  reverse: false,
  textSource: { kind: 'quote', quoteId: QUOTE_ID, quoteHash: QUOTE_HASH, text: QUOTE_TEXT },
  ...over
})

/** Build the exact RUNS.md POST body (features/run-submit/model/build-payload.ts). */
function payloadOf({
  config,
  generation,
  declaration,
  seed,
  finished,
  scoreVersion,
  dict = dictionary,
  dictHash = DICT_HASH
}) {
  const score = scoreVersion === 1 ? finished.scoreV1 : finished.scoreV2
  const quote = generation.textSource?.kind === 'quote'
  return {
    mode: config.mode,
    // A quote run carries NEITHER dimension: its length is a property of the
    // text, not a number the player chose. A seeded run carries exactly one.
    ...(quote ? {} : config.mode === 'time' ? { durationMs: config.durationMs } : { wordCount: generation.length }),
    lang: dict.bcp47,
    seed,
    dictHash,
    scoreVersion,
    // buildRunPayload spreads the in-memory generation and OVERWRITES
    // textSource with the bare reference: the text is dropped at the wire
    // boundary by construction, and the server reads it back from the registry
    // by id. Reproduced here rather than asserted, so the vector is the shape
    // the client actually sends.
    setup: { config, generation: quote ? withoutQuoteText(generation) : generation, declaration },
    clientMetrics: { wpm: finished.metrics.wpm, raw: finished.metrics.raw, acc: finished.metrics.accuracy },
    clientScore: score,
    log: finished.log
  }
}

/** The submitted textSource: {kind, quoteId, quoteHash}, and nothing else. */
function withoutQuoteText(generation) {
  const { text: _dropped, ...reference } = generation.textSource
  return { ...generation, textSource: reference }
}

const vectors = []

function emit(name, description, expect, payload, extra = {}) {
  vectors.push({ name, description, expect, payload, ...extra })
}

// ── 1. words mode, flawless ──────────────────────────────────────────────────
{
  const config = coreConfig()
  const generation = generationConfig()
  const seed = 20260724
  const s = session({ config, generation, declaration: noMods, seed })
  for (let i = 0; i < generation.length; i++) {
    typeWord(s, s.words[i])
    s.commit()
  }
  const finished = s.finish()
  emit(
    'words-clean',
    'Words mode, ten words typed flawlessly at a human cadence. The baseline: accepted, no flags.',
    { status: 'accepted', verdict: 'valid', flags: [] },
    payloadOf({ config, generation, declaration: noMods, seed, finished, scoreVersion: 2 })
  )
}

// ── 2. time mode, deadline-settled ───────────────────────────────────────────
{
  const config = coreConfig({ mode: 'time', durationMs: 15_000 })
  const generation = generationConfig({ mode: 'time', length: 15 })
  const seed = 99117
  const s = session({ config, generation, declaration: noMods, seed, jitterSeed: 31 })
  // Type until shortly before the deadline: an event AT or AFTER it is a
  // "time teleport" and validateLog rejects the log (two-clock check).
  let w = 0
  while (s.now < config.durationMs - 1500 && w < s.words.length) {
    typeWord(s, s.words[w])
    s.commit()
    w += 1
  }
  // The timer worker settles the client's core at the deadline, measured from
  // the run's OWN start (startPolicy 'input' anchors t = 0 at the first event).
  s.tick(s.core.state.startedAt + config.durationMs)
  const finished = s.finish()
  if (!(finished.metrics.wpm > 0)) throw new Error('vector 2 did not settle at the deadline')
  emit(
    'time-clean',
    'Time mode (15 s) settled at the deadline by the timer worker, as the client does. Exercises the two-clock check and deadline-anchored metrics.',
    { status: 'accepted', verdict: 'valid', flags: [] },
    payloadOf({ config, generation, declaration: noMods, seed, finished, scoreVersion: 2 })
  )
}

// ── 3. text mods + declared mods (the scoreV2 multiplier path) ───────────────
{
  const config = coreConfig({ difficulty: 'expert' })
  const generation = generationConfig({ punctuation: true, numbers: true, randomCase: true })
  const declaration = { blind: true, fading: false, flashlight: false }
  const seed = 4242
  const s = session({ config, generation, declaration, seed, jitterSeed: 5 })
  for (let i = 0; i < generation.length; i++) {
    typeWord(s, s.words[i])
    s.commit()
  }
  const finished = s.finish()
  emit(
    'words-mods',
    'Punctuation + numbers + randomCase generation, expert difficulty, blind declared. The scoreV2 mod multiplier must be reproduced from the setup alone.',
    { status: 'accepted', verdict: 'valid', flags: [] },
    payloadOf({ config, generation, declaration, seed, finished, scoreVersion: 2 }),
    { modMultiplier: modMultiplierV1({ generation, config }, declaration) }
  )
}

// ── 4. rejected-dispatch backspaces (the seq-hole regression class) ──────────
{
  const config = coreConfig()
  const generation = generationConfig({ length: 6 })
  const seed = 777001
  const s = session({ config, generation, declaration: noMods, seed, jitterSeed: 11 })
  for (let i = 0; i < generation.length; i++) {
    typeWord(s, s.words[i])
    s.commit()
    // At a word boundary the previous word is committed fully correct, so the
    // reducer LOCKS backspace: both of these are rejected and must leave no
    // hole in the log's seq numbering.
    if (i < generation.length - 1) {
      s.back('char')
      s.back('word')
    }
  }
  const finished = s.finish()
  if (s.rejected === 0) throw new Error('vector 4 produced no rejected dispatches')
  emit(
    'words-rejected-backspace',
    'Every word boundary is followed by a locked backspace and a locked ctrl+backspace. Both are rejected by the reducer, never logged, and MUST NOT consume a seq — a hole here is the regression this vector exists for.',
    { status: 'accepted', verdict: 'valid', flags: [] },
    payloadOf({ config, generation, declaration: noMods, seed, finished, scoreVersion: 2 }),
    { rejectedDispatches: s.rejected }
  )
}

// ── 5. typos + corrections, scoreVersion 1 ───────────────────────────────────
{
  const config = coreConfig()
  const generation = generationConfig({ length: 8 })
  const seed = 31337
  const s = session({ config, generation, declaration: noMods, seed, jitterSeed: 23 })
  for (let i = 0; i < generation.length; i++) {
    const word = [...s.words[i]]
    for (let c = 0; c < word.length; c++) {
      // Fumble the third character of every other word, then correct it: the
      // combo breaks, accuracy drops, and the retype scores nothing.
      if (c === 2 && i % 2 === 0) {
        s.type('q')
        s.back('char')
      }
      s.type(word[c])
    }
    s.commit()
  }
  const finished = s.finish()
  if (finished.metrics.accuracy >= 1) throw new Error('vector 5 was supposed to contain typos')
  emit(
    'words-typos-v1',
    'Mistyped and corrected characters, submitted under scoreVersion 1 (scoreOfLog). Exercises acc-squared, a broken combo, and the v1 scoring route.',
    { status: 'accepted', verdict: 'valid', flags: [] },
    payloadOf({ config, generation, declaration: noMods, seed, finished, scoreVersion: 1 })
  )
}

// ── 6. one sub-15ms interval in an otherwise human run (the false positive) ──
// This is the shape that made 8 of 11 real runs get flagged under the original
// "any flag => flagged" rule: ordinary key rollover. It MUST come out accepted,
// with the flag still recorded.
{
  const config = coreConfig()
  const generation = generationConfig({ length: 10 })
  const seed = 5150
  const s = session({ config, generation, declaration: noMods, seed, jitterSeed: 17 })
  let nudged = false
  for (let i = 0; i < generation.length; i++) {
    const word = [...s.words[i]]
    for (let c = 0; c < word.length; c++) {
      // Exactly one interval under the 15 ms threshold, in the middle of the run.
      if (!nudged && i === 4 && c === 1) {
        s.type(word[c], 9)
        nudged = true
        continue
      }
      s.type(word[c])
    }
    s.commit()
  }
  const finished = s.finish()
  if (!nudged) throw new Error('vector 6 did not produce its fast interval')
  emit(
    'words-one-fast-interval',
    'A single 9 ms interval in an otherwise human run — ordinary key rollover. Raises min-interval at a tiny severity and MUST be accepted, with the flag kept for moderation.',
    { status: 'accepted', verdict: 'valid', flags: ['min-interval'] },
    payloadOf({ config, generation, declaration: noMods, seed, finished, scoreVersion: 2 })
  )
}

// ── 7. bot cadence: identical intervals end to end ───────────────────────────
// Every keystroke exactly 80 ms apart, commits sharing the previous instant, so
// the interval series has zero variance. Trips uniform-intervals AND
// zero-variance together — the bot_cadence combination rule.
{
  const config = coreConfig()
  const generation = generationConfig({ length: 12 })
  const seed = 606061
  const s = session({
    config,
    generation,
    declaration: noMods,
    seed,
    fixedInterval: 80,
    commitAtSameInstant: true
  })
  for (let i = 0; i < generation.length; i++) {
    typeWord(s, s.words[i])
    s.commit()
  }
  const finished = s.finish()
  emit(
    'words-bot-cadence',
    'Every keystroke exactly 80 ms apart with zero variance. No single flag is severe enough on its own, but uniform-intervals + zero-variance is a shape no hand produces: the bot_cadence rule must flag it.',
    { status: 'flagged', verdict: 'valid', flags: ['uniform-intervals', 'zero-variance'] },
    payloadOf({ config, generation, declaration: noMods, seed, finished, scoreVersion: 2 })
  )
}

// ── 8. nospace, and the player keeps pressing space ──────────────────────────
// In nospace the word advances on the insert that fills it; a commit event has
// no meaning. The reducer REFUSES it (kind `NospaceCommit`) instead of folding a
// no-op, because an accepted event is a logged event — and `validateLog` throws
// out an entire nospace log that contains one. This vector is the proof that a
// habitual space press cannot invalidate an honest run: every space below is
// rejected, the log comes out commit-free, and the run is accepted.
{
  const config = coreConfig({ nospace: true })
  const generation = generationConfig({ length: 8 })
  const seed = 8080808
  const s = session({ config, generation, declaration: noMods, seed, jitterSeed: 13 })
  for (let i = 0; i < generation.length; i++) {
    typeWord(s, s.words[i]) // the last grapheme auto-commits (nospace)
    s.commit() // the habitual separator press: refused, never logged
  }
  const finished = s.finish()
  if (s.rejected !== generation.length) {
    throw new Error(`vector 8: expected ${generation.length} refused commits, got ${s.rejected}`)
  }
  if (finished.log.events.some((event) => event.kind === 'commit')) {
    throw new Error('vector 8: a commit reached the log — nospace runs would be invalidated')
  }
  emit(
    'words-nospace-space-presses',
    'nospace run whose player kept tapping space after every word. Each commit is refused by the reducer, so the submitted log is commit-free and the run is accepted — the guard against an honest nospace run being thrown out by validateLog.',
    { status: 'accepted', verdict: 'valid', flags: [] },
    payloadOf({ config, generation, declaration: noMods, seed, finished, scoreVersion: 2 }),
    { rejectedDispatches: s.rejected }
  )
}

// ── 9. code dictionary: '\n' is the separator, not an extra one ──────────────
// The targets carry their own layout, so the word that ends a line ends with a
// typed '\n'. `separatorsOf` credits no phantom space for it — the numbers below
// come out of the updated core, and goja must reproduce them exactly. The
// dictionary is not published, so this vector carries it inline (see the Go
// harness: inline-dictionary vectors replay through the core, not the registry).
{
  const config = coreConfig({ maxExtraChars: 40 })
  const generation = generationConfig({ length: 9, rawTokens: true })
  const seed = 20260725
  const s = session({
    config,
    generation,
    declaration: noMods,
    seed,
    jitterSeed: 29,
    dict: codeDictionary,
    dictHash: CODE_DICT_HASH
  })
  for (let i = 0; i < generation.length; i++) {
    typeWord(s, s.words[i]) // includes the leading '\t' and the trailing '\n'
    s.commit() // Enter typed the newline above, then separates the word
  }
  const finished = s.finish()
  const lineEnders = s.words.filter((word) => word.endsWith('\n')).length
  if (lineEnders === 0) throw new Error('vector 9: the generated run has no line ends')
  // `separatorsOf`, restated: one space per committed word, minus every word that
  // ended its own line, minus the last word of a count-finished run.
  const lastEndsLine = s.words[generation.length - 1].endsWith('\n')
  const expectedSpaces = generation.length - lineEnders - (lastEndsLine ? 0 : 1)
  if (finished.metrics.spaces !== expectedSpaces) {
    throw new Error(
      `vector 9: separator accounting is off (spaces=${finished.metrics.spaces}, expected=${expectedSpaces}, lineEnders=${lineEnders})`
    )
  }
  emit(
    'code-newline-separator',
    "Raw-token (code) dictionary: '\\t' indentation and a '\\n' at the end of every line-closing word. A word that ends its own line typed its separator, so it is credited no space — the rule the metrics and the score must reproduce.",
    { status: 'accepted', verdict: 'valid', flags: [] },
    payloadOf({
      config,
      generation,
      declaration: noMods,
      seed,
      finished,
      scoreVersion: 2,
      dict: codeDictionary,
      dictHash: CODE_DICT_HASH
    }),
    {
      dictionary: { name: codeDictionary.name, bcp47: codeDictionary.bcp47, words: codeDictionary.words },
      lineEnders,
      spaces: finished.metrics.spaces
    }
  )
}

// ── 10. a quote run: fixed text, no dimension, no dictionary ─────────────────
// The words are not generated, they ARE the registry's bytes — split on ' ',
// empties dropped, no PRNG consumed. Three things this pins that no seeded
// vector can:
//
//   * the DOUBLE SPACE after the first sentence collapses, so 17 space-
//     separated fragments become 16 typeable words;
//   * `Mensch,` / `strebt.` / `bemüht,` / `erlösen!` are punctuation the player
//     must type verbatim — no `decorate()` produced them, so nothing regenerates
//     them either, and a reducer that treated them as extra characters would
//     show up immediately in accuracy;
//   * `dictVersion` folds over UTF-16 code units, and the text has three
//     non-ASCII graphemes, so the run's whole identity depends on V8 and goja
//     agreeing about them.
//
// The payload carries the quote by REFERENCE only. The text below travels in
// the vector's own `quote` field, which the Go harness loads into a fake
// registry — mirroring the server, which resolves it out of Postgres by id.
{
  const config = coreConfig({ mode: 'quote', maxExtraChars: 40 })
  const generation = quoteGeneration()
  const seed = 20260726
  const s = session({
    config,
    generation,
    declaration: noMods,
    seed,
    jitterSeed: 31,
    dict: quoteDictionary,
    dictHash: QUOTE_HASH
  })
  if (s.words.length !== 16) {
    throw new Error(`vector 10: expected 16 words from the quote, got ${s.words.length}`)
  }
  if (s.words.join(' ') === QUOTE_TEXT) {
    throw new Error('vector 10: the double space did not collapse — the vector pins nothing')
  }
  for (let i = 0; i < s.words.length; i++) {
    typeWord(s, s.words[i])
    s.commit()
  }
  const finished = s.finish()
  if (finished.metrics.accuracy !== 1) {
    throw new Error(`vector 10: the run was meant to be flawless, got acc=${finished.metrics.accuracy}`)
  }
  emit(
    'quote-fixed-text',
    'A quote run: the text comes from the registry, not the seed. Neither durationMs nor wordCount; ' +
      'dictHash is dictVersion([text]) and resolves to no dictionary; the double space collapses to one ' +
      'separator; punctuation and non-ASCII are typed verbatim. The payload carries the quote by id and ' +
      'hash only — the server reads the bytes back itself.',
    { status: 'accepted', verdict: 'valid', flags: [] },
    payloadOf({
      config,
      generation,
      declaration: noMods,
      seed,
      finished,
      scoreVersion: 2,
      dict: quoteDictionary,
      dictHash: QUOTE_HASH
    }),
    {
      quote: { id: QUOTE_ID, hash: QUOTE_HASH, text: QUOTE_TEXT },
      spaces: finished.metrics.spaces
    }
  )
}

mkdirSync(outDir, { recursive: true })
for (const vector of vectors) {
  writeFileSync(join(outDir, `${vector.name}.json`), `${JSON.stringify(vector, null, 2)}\n`, 'utf8')
  const score = vector.payload.clientScore
  console.log(
    `${vector.name.padEnd(26)} events=${String(vector.payload.log.events.length).padStart(4)} ` +
      `total=${String(score.total).padStart(6)} wpm=${vector.payload.clientMetrics.wpm.toFixed(3)} ` +
      `acc=${vector.payload.clientMetrics.acc.toFixed(4)} v${vector.payload.scoreVersion}`
  )
}
console.log(`\n${vectors.length} vectors written to ${outDir}`)
console.log(`dictionary: ${LANG} (${DICT_HASH}), ${dictionary.words.length} words`)
