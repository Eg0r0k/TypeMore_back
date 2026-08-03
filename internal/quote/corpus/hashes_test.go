package corpus_test

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/typemore/typemore-server/internal/quote/corpus"
	"github.com/typemore/typemore-server/internal/replay"
)

// Every vendored quote's text_hash, pinned one line per quote.
//
// TestTextHashComesFromTheCoreBundle proves the WIRING — that the importer's
// hash is the core's hash — over one small corpus. This proves the VALUE, over
// all of them, and it is a different question with a different consequence.
//
// A quote's (id, text_hash) pair is its identity forever: the replay worker
// resolves a run's quote by id and refuses it unless the registry's text_hash
// matches the `quoteHash` the run recorded (docs/REPLAY.md step 1), and a quote
// run's `dictVersion` IS that hash. Published bytes are frozen (CLAUDE.md), so
// a hash that moves does not invalidate a quote — it invalidates every run ever
// played on it, and the run cannot be re-judged because the text it was
// generated from no longer answers to the name it recorded.
//
// The hash is taken over the RAW text, which is what makes this checkable at
// all: the tokenizer may rewrite what a player TYPES (`expandEllipsis` in
// words.ts turns `…` into `...` for the thirty-four quotes that carry one)
// without touching what the corpus IS. That ordering is the whole design, and
// this file is where it stops being a claim. If the rewrite ever moves ahead of
// the digest, 15 817 lines move with it.
//
// Regenerate deliberately, never reflexively:
//
//	TYPEMORE_REGENERATE_CORPUS_HASHES=1 go test ./internal/quote/corpus/ -run TestEveryQuoteHashIsFrozen
//
// The diff is the review, and a moved line needs an answer to "what happens to
// the runs on that quote" before it is committed.
const quoteHashGolden = "testdata/quote-hashes.tsv"

const regenerateEnv = "TYPEMORE_REGENERATE_CORPUS_HASHES"

func TestEveryQuoteHashIsFrozen(t *testing.T) {
	if testing.Short() {
		t.Skip("hashes 15k quotes through the goja bundle")
	}
	core, err := replay.NewCore(0)
	require.NoError(t, err)

	m := manifest(t)
	// Manifest order, not sorted: it is the order the importer walks and the
	// order a re-vendor edits, so a diff of this file lines up with a diff of
	// MANIFEST.json instead of scattering across it.
	var lines []string
	for _, lang := range m.Languages {
		loaded, err := corpus.Load(core, lang)
		require.NoError(t, err, "load %s", lang.Lang)
		for i := range loaded {
			lines = append(lines, fmt.Sprintf("%s\t%d\t%s",
				lang.Lang, loaded[i].UpstreamID, loaded[i].TextHash))
		}
	}
	require.NotEmpty(t, lines)

	if os.Getenv(regenerateEnv) != "" {
		require.NoError(t, os.MkdirAll(filepath.Dir(quoteHashGolden), 0o755))
		require.NoError(t, os.WriteFile(quoteHashGolden,
			[]byte(strings.Join(lines, "\n")+"\n"), 0o644))
		t.Logf("regenerated %s with %d quotes across %d languages",
			quoteHashGolden, len(lines), len(m.Languages))
		return
	}

	f, err := os.Open(quoteHashGolden)
	require.NoError(t, err, "golden missing — generate it with %s=1", regenerateEnv)
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var want []string
	for scanner.Scan() {
		if line := scanner.Text(); line != "" {
			want = append(want, line)
		}
	}
	require.NoError(t, scanner.Err())

	require.Equal(t, len(want), len(lines),
		"the corpus gained or lost quotes; regenerate the golden deliberately")
	// Line by line: "one of 15 817 hashes moved" is only actionable once it
	// names the language and the upstream id, because that is what a moderator
	// needs to look up the runs that were played on it.
	for i := range want {
		require.Equal(t, want[i], lines[i], "quote hash moved at row %d", i)
	}
	t.Logf("%d quote hashes unchanged across %d languages", len(lines), len(m.Languages))
}
