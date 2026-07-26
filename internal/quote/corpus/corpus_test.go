package corpus_test

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/typemore/typemore-server/internal/quote"
	"github.com/typemore/typemore-server/internal/quote/corpus"
	"github.com/typemore/typemore-server/internal/replay"
)

// stubHasher stands in for the core wherever a test is about the CORPUS rather
// than about hashing. Hashing 9 881 quotes through goja takes seconds and
// proves nothing about thresholds; TestTextHashComesFromTheCoreBundle drives
// the real bundle instead.
type stubHasher struct{}

func (stubHasher) DictVersion(words []string) (string, error) {
	return fmt.Sprintf("%08x", len(strings.Join(words, "\x00"))), nil
}

func manifest(t *testing.T) corpus.Manifest {
	t.Helper()
	m, err := corpus.ReadManifest()
	require.NoError(t, err)
	return m
}

// Every manifest row must load: the file exists, declares the upstream language
// the row points at, holds the recorded number of quotes, and carries the
// recorded thresholds. This is the tripwire for a re-vendor that pulled a
// different commit than the manifest claims.
func TestEveryManifestRowLoads(t *testing.T) {
	m := manifest(t)
	require.NotEmpty(t, m.Upstream.Commit, "provenance must pin a commit")

	total := 0
	for _, lang := range m.Languages {
		t.Run(lang.Lang, func(t *testing.T) {
			got, err := corpus.Load(stubHasher{}, lang)
			require.NoError(t, err)
			assert.Len(t, got, lang.Quotes)
			total += len(got)
		})
	}
	t.Logf("%d languages, %d quotes", len(m.Languages), total)
}

// A quote's len_group must come from ITS OWN corpus's thresholds. The
// expectation here is recomputed from the vendored file rather than from a
// table in the test, so the test cannot drift into asserting the same wrong
// constant the importer would use.
func TestLenGroupsComeFromEachCorpusOwnThresholds(t *testing.T) {
	m := manifest(t)
	for _, lang := range m.Languages {
		t.Run(lang.Lang, func(t *testing.T) {
			got, err := corpus.Load(stubHasher{}, lang)
			require.NoError(t, err)

			for i := range got {
				q := &got[i]
				want := bandOf(t, lang.Groups, int(q.Length))
				require.Equal(t, want, q.LenGroup,
					"%s #%d (length %d) with thresholds %v", lang.Lang, q.UpstreamID, q.Length, lang.Groups)
			}
		})
	}
}

// The witness. chinese publishes 30/80/200 where every other corpus publishes
// 100/300/600, so an importer with ONE global threshold table is not a style
// problem — it puts chinese quotes in the wrong band.
//
// The assertion is two-sided on purpose: the thresholds differ AND applying the
// majority table to chinese produces a DIFFERENT band for real quotes in the
// vendored file. Without the second half the test would still pass on an
// importer that read the right numbers and then ignored them.
//
// The count is small — chinese's longest quote is 63 characters, so its own
// thresholds only ever separate short from medium and the two tables disagree
// about a handful of rows. That is the honest size of the effect; a test that
// demanded a big number would be asserting something the corpus does not say.
// Every disagreeing quote is named, so a failure points at data rather than a
// ratio.
func TestChineseThresholdsDifferFromTheRest(t *testing.T) {
	m := manifest(t)

	var chinese, other corpus.Language
	for _, lang := range m.Languages {
		switch lang.Lang {
		case "chinese":
			chinese = lang
		case "russian":
			other = lang
		}
	}
	require.NotEmpty(t, chinese.File, "the manifest must still carry chinese")
	require.NotEmpty(t, other.File, "the manifest must still carry russian")
	require.NotEqual(t, other.Groups, chinese.Groups,
		"chinese is the witness for per-corpus thresholds; if upstream has "+
			"unified them, this test needs a new witness, not deleting")

	got, err := corpus.Load(stubHasher{}, chinese)
	require.NoError(t, err)

	var misfiled []string
	for i := range got {
		q := &got[i]
		if global := bandOf(t, other.Groups, int(q.Length)); global != q.LenGroup {
			misfiled = append(misfiled, fmt.Sprintf("#%d (length %d): own=%s global=%s",
				q.UpstreamID, q.Length, q.LenGroup, global))
		}
	}
	require.NotEmpty(t, misfiled,
		"the two threshold tables must disagree about at least one vendored chinese "+
			"quote, or this test cannot catch a global threshold table")
	t.Logf("a single global threshold table would misfile %d of %d chinese quotes:\n  %s",
		len(misfiled), len(got), strings.Join(misfiled, "\n  "))

	// And the disagreement is exactly the one the thresholds describe: quotes
	// past chinese's own 30-character boundary but inside the common table's
	// 100-character one.
	for i := range got {
		q := &got[i]
		if int(q.Length) > chinese.Groups[0][1] && int(q.Length) <= other.Groups[0][1] {
			require.NotEqual(t, quote.LenShort, q.LenGroup,
				"chinese #%d is %d characters — past chinese's own short/medium "+
					"boundary of %d — so it must NOT be short",
				q.UpstreamID, q.Length, chinese.Groups[0][1])
		}
	}
}

// The stored `length` is the character count of the stored text. The database
// enforces it with a CHECK; this catches it before the import even opens a
// connection, and names the offending quote.
func TestLengthAgreesWithTheText(t *testing.T) {
	m := manifest(t)
	for _, lang := range m.Languages {
		t.Run(lang.Lang, func(t *testing.T) {
			got, err := corpus.Load(stubHasher{}, lang)
			require.NoError(t, err)
			for i := range got {
				q := &got[i]
				require.EqualValues(t, len([]rune(q.Text)), q.Length,
					"%s #%d", lang.Lang, q.UpstreamID)
			}
		})
	}
}

// text_hash must come out of the VENDORED BUNDLE, not out of a Go FNV-1a. This
// drives the real core: if someone ever reimplements the hash in Go, this test
// keeps passing only for as long as the reimplementation agrees — and the day
// it drifts, every quote's hash is wrong at once.
func TestTextHashComesFromTheCoreBundle(t *testing.T) {
	core, err := replay.NewCore(0)
	require.NoError(t, err)

	m := manifest(t)
	// One small corpus is enough: what is under test is the wiring, not 9 881
	// repetitions of it.
	var css corpus.Language
	for _, lang := range m.Languages {
		if lang.Lang == "code_css" {
			css = lang
		}
	}
	require.NotEmpty(t, css.File)

	got, err := corpus.Load(core, css)
	require.NoError(t, err)
	require.NotEmpty(t, got)

	for i := range got {
		want, err := core.DictVersion([]string{got[i].Text})
		require.NoError(t, err)
		require.Equal(t, want, got[i].TextHash, "code_css #%d", got[i].UpstreamID)
		require.Len(t, got[i].TextHash, 8, "the core's digest is 8 hex characters")
	}
}

// The manifest documents what is NOT imported and why. docs/QUOTES.md reprints
// that list, so this pins the two together: an excluded language that gets
// vendored, or one that gets added, has to move in both places or CI says so.
func TestExcludedLanguagesMatchTheDoc(t *testing.T) {
	m := manifest(t)
	require.NotEmpty(t, m.Excluded)

	raw, err := os.ReadFile("../../../docs/QUOTES.md")
	require.NoError(t, err)
	doc := string(raw)

	for _, ex := range m.Excluded {
		assert.Contains(t, doc, "`"+ex.Lang+"`",
			"docs/QUOTES.md must list the excluded language %q", ex.Lang)
	}
	for _, lang := range m.Languages {
		for _, ex := range m.Excluded {
			require.NotEqual(t, lang.Lang, ex.Lang,
				"%s is both imported and excluded", lang.Lang)
		}
	}
}

// bandOf is the test's own reading of a thresholds array — written separately
// from the importer's so the two have to agree rather than share a bug.
func bandOf(t *testing.T, groups [][2]int, length int) quote.LenGroup {
	t.Helper()
	for i, g := range groups {
		if length >= g[0] && length <= g[1] {
			return quote.LenGroup(i)
		}
	}
	t.Fatalf("length %d falls outside %v", length, groups)
	return 0
}
