package replay

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/typemore/typemore-server/internal/perf"
	"github.com/typemore/typemore-server/internal/runlimits"
)

// THE TIMED MODE'S CEILING, DERIVED ON ITS OWN TERMS.
//
// The two dictionary gates beside this file generate
// `{"mode":"words","length":MaxWordCount}` and count the events of the resulting
// word list. What they bound is a WORD run. A timed run's worst case is
// speed × duration — an expression MaxWordCount does not appear in — and it was
// never computed anywhere: while MaxWordCount was 10 000 the word run was the
// larger of the two and the timed mode hid behind it.
//
// At MaxWordCount = 3 000 it no longer does. An hour at 250 wpm is ~75 000
// keystrokes against a 3 000-word run's ~19 000 events, so lowering the
// measuring point without deriving this one would have removed the only estimate
// that incidentally covered timed play. This test is that estimate, made
// deliberate.
//
// The model, worst case at every step:
//
//   - one STATE event per keystroke (an insert, or a commit for the separator).
//     A word run's `len(words) + Σ graphemes` is the same count expressed per
//     word; per keystroke is the form that does not need a word list.
//   - v2: three events per keystroke (down + state + up), plus one Shift PAIR
//     per shifted grapheme. The bound assumes EVERY grapheme is shifted, which
//     no real text is — 5 events per keystroke rather than 3.
//
// Both are upper bounds, so a pass here means the cap holds for any text at any
// speed up to MaxPlausibleWpm, on any dictionary, published or not.
func TestATimedRunAtMaxSpeedFitsTheEventCaps(t *testing.T) {
	keystrokes := runlimits.TimedRunKeystrokes(runlimits.MaxDurationMs, runlimits.MaxPlausibleWpm)

	v1 := keystrokes
	// Every grapheme shifted: 3 per keystroke + 2 per Shift pair.
	v2 := 5 * keystrokes

	t.Logf("a %d-minute run at %d wpm: %d keystrokes → v1 %d events (%.0f%% of %d), "+
		"v2 worst case %d events (%.0f%% of %d)",
		runlimits.MaxDurationMs/60_000, runlimits.MaxPlausibleWpm, keystrokes,
		v1, float64(v1)/float64(perf.MaxEvents)*100, perf.MaxEvents,
		v2, float64(v2)/float64(perf.MaxEventsV2)*100, perf.MaxEventsV2)

	assert.LessOrEqualf(t, v1, perf.MaxEvents,
		"the longest legal timed run does not fit the v1 event cap: %d events over %d. "+
			"Lower MaxDurationMs — the caps do not move (docs/DICTIONARIES.md)",
		v1, perf.MaxEvents)
	assert.LessOrEqualf(t, v2, perf.MaxEventsV2,
		"the longest legal timed run does not fit the v2 event cap: %d events over %d. "+
			"Lower MaxDurationMs — the caps do not move (docs/DICTIONARIES.md)",
		v2, perf.MaxEventsV2)
}

// The ceiling has to keep MEANING something. If MaxDurationMs were ever lowered
// far enough that the timed mode stopped being worth deriving, or raised far
// enough that it silently became the binding constraint with no margin, the
// number above would be doing neither of the jobs it claims.
//
// This states the margin as a property rather than leaving it in a log line: at
// least 15% of headroom under BOTH caps, the same threshold the dictionary
// import applies to a candidate corpus.
func TestTheTimedCeilingKeepsAStatedMargin(t *testing.T) {
	const minHeadroom = 0.15

	keystrokes := runlimits.TimedRunKeystrokes(runlimits.MaxDurationMs, runlimits.MaxPlausibleWpm)

	for _, c := range []struct {
		name   string
		events int
		cap    int
	}{
		{"v1", keystrokes, perf.MaxEvents},
		{"v2 (every grapheme shifted)", 5 * keystrokes, perf.MaxEventsV2},
	} {
		headroom := 1 - float64(c.events)/float64(c.cap)
		t.Logf("%s: %d / %d — %.1f%% headroom", c.name, c.events, c.cap, headroom*100)
		assert.GreaterOrEqualf(t, headroom, minHeadroom,
			"%s headroom is %.1f%%, under the %.0f%% this ceiling is supposed to carry",
			c.name, headroom*100, minHeadroom*100)
	}
}

// The saturation points, reported rather than asserted: the durations at which
// each cap would actually be reached at MaxPlausibleWpm. This is the number to
// read before changing MaxDurationMs, and it is why one hour is described as a
// product decision with margin rather than as the largest value that fits.
func TestReportTheDurationEachCapWouldSaturateAt(t *testing.T) {
	perMinute := runlimits.MaxPlausibleWpm * runlimits.CharsPerWord

	v1Minutes := float64(perf.MaxEvents) / float64(perMinute)
	v2Minutes := float64(perf.MaxEventsV2) / float64(5*perMinute)

	t.Logf("at %d wpm the caps saturate at: v1 %.0f min, v2 (all shifted) %.0f min; "+
		"MaxDurationMs is %.0f min",
		runlimits.MaxPlausibleWpm, v1Minutes, v2Minutes,
		float64(runlimits.MaxDurationMs)/60_000)

	assert.Less(t, float64(runlimits.MaxDurationMs)/60_000, v1Minutes)
	assert.Less(t, float64(runlimits.MaxDurationMs)/60_000, v2Minutes)
}
