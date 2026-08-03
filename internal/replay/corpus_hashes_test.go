package replay

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The dictionary catalogue's hashes, pinned one line per dictionary.
//
// A dictHash is not an internal detail. It is the ADDRESS a dictionary body is
// served at (`/static/dictionaries/{dictHash}.json`), the drift guard every
// stored run carries, and — through `makeSeedContext` — an input to the check
// that decides whether a historical run can still be replayed at all. Published
// bytes are frozen forever (CLAUDE.md), so a dictionary whose hash moves is a
// dictionary whose entire run history becomes unreplayable. That invariant had
// no test: the registry computes the hash from the file at startup, so file and
// hash always agree with each other and nothing was ever compared against
// yesterday.
//
// This is that comparison. The golden was generated from the bundle vendored at
// the time it was written, so it also answers a question no other test in this
// package asks: does a NEW core bundle still fingerprint the corpus the way the
// old one did? `expandEllipsis` (words.ts) is the change that made the question
// urgent — it rewrites tokens for typeability, and if it had run before the
// digest instead of after it, eleven dictionaries would have silently changed
// address and taken their runs with them.
//
// Regenerate deliberately, never reflexively:
//
//	TYPEMORE_REGENERATE_CORPUS_HASHES=1 go test ./internal/replay/ -run TestDictionaryHashesAreFrozen
//
// and treat the diff as the review. A line that moves is either a dictionary
// that was intentionally re-vendored (its runs are gone — say so in the commit)
// or a core change that was not supposed to be observable.
const dictHashGolden = "testdata/corpus-hashes/dictionaries.tsv"

const regenerateEnv = "TYPEMORE_REGENERATE_CORPUS_HASHES"

func TestDictionaryHashesAreFrozen(t *testing.T) {
	_, reg := sharedDicts(t)

	catalogue := reg.Catalogue()
	require.NotEmpty(t, catalogue)

	lines := make([]string, 0, len(catalogue))
	for _, entry := range catalogue {
		lines = append(lines, fmt.Sprintf("%s\t%s\t%d", entry.Lang, entry.DictHash, entry.WordCount))
	}
	sort.Strings(lines)

	if os.Getenv(regenerateEnv) != "" {
		writeGolden(t, dictHashGolden, lines)
		t.Logf("regenerated %s with %d dictionaries", dictHashGolden, len(lines))
		return
	}

	want := readGolden(t, dictHashGolden)
	require.Equal(t, len(want), len(lines),
		"the catalogue gained or lost a dictionary; regenerate the golden deliberately")

	// Compared line by line rather than as one blob: a whole-file diff of 430
	// rows tells an operator that "something moved", and the only thing worth
	// knowing here is WHICH dictionary and whether its runs are now stranded.
	for i := range want {
		require.Equal(t, want[i], lines[i], "dictionary hash moved at row %d", i)
	}
	t.Logf("%d dictionary hashes unchanged", len(lines))
}

func writeGolden(t *testing.T, rel string, lines []string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(rel), 0o755))
	require.NoError(t, os.WriteFile(rel, []byte(strings.Join(lines, "\n")+"\n"), 0o644))
}

func readGolden(t *testing.T, rel string) []string {
	t.Helper()
	f, err := os.Open(rel)
	require.NoError(t, err, "golden missing — generate it with %s=1", regenerateEnv)
	defer func() { _ = f.Close() }()

	var out []string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		out = append(out, line)
	}
	require.NoError(t, scanner.Err())
	return out
}
