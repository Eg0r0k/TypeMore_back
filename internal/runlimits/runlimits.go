// Package runlimits owns the two hard bounds on how long a single run may be:
// how many words a word run may ask for, and how long a timed run may last.
//
// WHY A PACKAGE OF TWO CONSTANTS. The numbers were previously written out three
// times — the ingest validator (internal/runs), the bucket-key bound
// (internal/leaderboard) and the load fixtures (internal/perf) — with nothing
// but a comment claiming the copies agreed. They MUST agree: a bucket key that
// can describe a run ingestion refuses is a board nothing can reach, and a load
// fixture built at a size ingestion refuses measures a run the server will never
// see. Three literals that must be equal and no test that says so is the same
// shape as the AFK thresholds before contract/match-timings.json.
//
// It is a leaf package on purpose, alongside internal/runstatus: domains do not
// import each other, so a value all three need lives below all three rather than
// inside whichever one happened to define it first.
//
// Both bounds are HARD and UNCONDITIONAL. They are refusals at ingest, not
// judgements — nothing here is behind policy.Judge or the anticheat build tag,
// because a self-hosted open build that accepted runs the product does not allow
// would be a hole straight into the database (docs/SELF_HOST.md).
package runlimits

// MaxWordCount is the largest word count a run may declare.
//
// WHERE THE NUMBER COMES FROM. It is the measuring point of the two dictionary
// gates (internal/replay: TestEveryPublishedDictionaryCanPlayAFullLengthRun and
// its v2 twin), which generate a full-length run on EVERY published dictionary
// and require it to fit under the ingestion event caps. At 10 000 words the
// worst published dictionary — league_of_legends — measured 118 943 events
// against a 120 000 cap: 99% used, 1 057 events of headroom, i.e. one slightly
// wordier import away from making a documented mode unplayable.
//
// 3 000 is the product's answer to that, and it is a lowering of the MEASURING
// POINT, not of a cap. The event caps themselves never move (docs/DICTIONARIES.md
// explains why: they are sized by what the server can replay, and raising one
// invalidates the sizing of everything under it). Lowering how long a run may be
// is the other legal way to make the same inequality hold, and it is the one
// that buys headroom instead of spending it.
//
// IRREVERSIBLE AFTER A PUBLICATION. Lowering this admits dictionaries that were
// previously rejected. Once such a dictionary is published its dict_hash is
// frozen forever and every run on it must stay replayable, so the number can
// never be raised back without making those runs unreplayable at the sizes the
// higher number would allow. Raise it only before publishing anything that
// depends on it having been low.
const MaxWordCount = 3_000

// MaxDurationMs is the longest a timed run may last: one hour.
//
// WHERE THE NUMBER COMES FROM — and it is NOT the dictionary gates. Those
// generate `{"mode":"words","length":MaxWordCount}` and derive their event count
// from the resulting word list, so what they bound is a WORD run. A timed run's
// worst case is speed × duration, an expression MaxWordCount does not appear in
// at all: at 250 wpm an hour is ~75 000 keystrokes, which is 25× the 3 000-word
// run the gates measure. While MaxWordCount was 10 000 the word run happened to
// be the larger of the two and the timed mode hid behind it; at 3 000 it no
// longer does, so the ceiling below is derived on its own terms and pinned by
// its own test (TestATimedRunAtMaxSpeedFitsTheEventCaps).
//
// The derivation, worst case throughout:
//
//	keystrokes = MaxPlausibleWpm × 5 chars/word × 60 min      = 75 000
//	v1 events  = one state event per keystroke                = 75 000  (63% of 120 000)
//	v2 events  = 3 per keystroke + one Shift PAIR per shifted
//	             grapheme; the bound assumes EVERY grapheme
//	             shifted, which no real text is             = 375 000  (78% of 480 000)
//
// Inverted, the caps saturate at ~96 min (v1) and ~77 min (v2 all-shifted). One
// hour is the tighter of those with ~22% to spare, and with a realistic shifted
// fraction the v2 figure drops to roughly half the cap.
//
// One hour is a PRODUCT decision taken with margin, not the largest number that
// would fit. Nobody types for an hour; the limit exists so that "how long can a
// run be" has an answer at all, and the margin exists so that answer does not
// have to move when a dictionary or a log version does.
const MaxDurationMs = 3_600_000

// MaxPlausibleWpm is the typing speed the timed-mode ceiling is derived against.
//
// It is deliberately far above any sustained human rate — the fastest recorded
// sustained English typing sits near 210 wpm and an hour at that pace has never
// been done — because it is used to prove a bound holds, not to predict what a
// player will do. An estimate that is generous in the direction of MORE events
// is the only kind that makes the conclusion safe.
//
// It is NOT an anti-cheat threshold and must never become one: nothing rejects a
// run for exceeding it. Plausibility of a SPEED is the replay policy's business
// (internal/replay/policy, anticheat build only); this is arithmetic about caps.
const MaxPlausibleWpm = 250

// CharsPerWord is the wpm metric's own definition of a word: five characters,
// the separating space included. Same constant the pace caret uses on the client
// (features/test/pace) and the same one metricsOf folds with in the core.
const CharsPerWord = 5

// TimedRunKeystrokes reports the worst-case keystroke count of a timed run of
// durationMs played at wpm — the input to both event models above. Exported so
// the ceiling's test and the load fixtures compute it the same way instead of
// each open-coding the arithmetic.
func TimedRunKeystrokes(durationMs int64, wpm int) int {
	return int(durationMs * int64(wpm) * CharsPerWord / 60_000)
}
