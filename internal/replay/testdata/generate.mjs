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

// The dictionary every vector plays against: a real published one, so the
// registry resolves its hash without any test-only seeding.
const LANG = 'german'
const dictDoc = JSON.parse(readFileSync(join(here, '..', 'dicts', `${LANG}.json`), 'utf8'))
const dictionary = { name: dictDoc.name, bcp47: dictDoc.bcp47 ?? LANG, words: dictDoc.words }
const DICT_HASH = dictVersion(dictionary.words)

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
function session({ config, generation, declaration, seed, jitterSeed = 7 }) {
  const generated = generateWords(dictionary, { seed, dictVersion: DICT_HASH, generation })
  if (generated.isErr()) throw new Error(`generation failed: ${generated.error.message}`)
  const words = generated.value.words

  const gameCore = new GameCore({ config, words })
  const ctx = { config, words }
  const scoreState = initialScoreState()
  const rng = lcg(jitterSeed)

  let seq = 0
  let t = 0
  let rejected = 0

  const step = () => {
    // 60–150 ms between keystrokes: human, and comfortably above the
    // min-interval plausibility threshold (15 ms).
    t += 60 + Math.floor(rng() * 90)
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
    type(text) {
      step()
      return send((s, at) => insertEvent(s, at, text))
    },
    commit() {
      step()
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

/** Build the exact RUNS.md POST body (features/run-submit/model/build-payload.ts). */
function payloadOf({ config, generation, declaration, seed, finished, scoreVersion }) {
  const score = scoreVersion === 1 ? finished.scoreV1 : finished.scoreV2
  return {
    mode: config.mode,
    ...(config.mode === 'time' ? { durationMs: config.durationMs } : { wordCount: generation.length }),
    lang: dictionary.bcp47,
    seed,
    dictHash: DICT_HASH,
    scoreVersion,
    setup: { config, generation, declaration },
    clientMetrics: { wpm: finished.metrics.wpm, raw: finished.metrics.raw, acc: finished.metrics.accuracy },
    clientScore: score,
    log: finished.log
  }
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
