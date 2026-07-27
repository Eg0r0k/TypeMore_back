package replay

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"math"
	"path"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dop251/goja"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/typemore/typemore-server/internal/perf"
)

// The budgets this corpus is held to. They exist because the corpus stopped
// being ten small files: 439 dictionaries is 57 MB of go:embed, and every one
// of those numbers is paid on every deploy and in every test binary that builds
// a registry.
const (
	// embedWarnBytes is the ceiling on total embedded dictionary bytes. It is
	// what the import manifest sized the corpus against: over it, importing
	// stops by size descending (the largest files buy the fewest languages per
	// byte) and the deferrals are journaled in IMPORT_MANIFEST.md.
	embedWarnBytes = 60 * 1000 * 1000

	// seedBudget is the wall clock NewRegistry may spend. It is a CI budget, so
	// it is generous next to the ~3.4 s a 12-thread box measures — but it is
	// nowhere near the 31 s the serial seed cost before `seed` was
	// parallelised, which is the regression it exists to catch.
	seedBudget = 10 * time.Second
)

// sharedDicts hands every test one Core and one Registry, built once.
//
// Seeding 430 dictionaries costs ~6.4 s, and this package used to build a fresh
// registry a dozen times — 77 s of pure setup, which is what pushed the package
// past `go test`'s 10-minute panic timeout once the corpus grew. Sharing is
// safe by construction rather than by convention: a Registry is documented
// immutable after construction, and Core serialises every entry point on its
// own mutex.
//
// TestSeedingFitsTheStartupBudget deliberately does NOT use this — it is the
// one test whose subject is the cost of building a registry.
var sharedDictsOnce = sync.OnceValue(func() struct {
	core *Core
	reg  *Registry
	err  error
} {
	var out struct {
		core *Core
		reg  *Registry
		err  error
	}
	if out.core, out.err = NewCore(0); out.err != nil {
		return out
	}
	out.reg, out.err = NewRegistry(out.core)
	return out
})

func sharedDicts(t *testing.T) (*Core, *Registry) {
	t.Helper()
	got := sharedDictsOnce()
	require.NoError(t, got.err)
	return got.core, got.reg
}

// The corpus is embedded, so its size is a build-time property of the binary
// and not something anyone notices at runtime until an image is slow to pull.
func TestEmbeddedCorpusFitsTheBudget(t *testing.T) {
	files, err := fs.ReadDir(dictFS, dictsDir)
	require.NoError(t, err)

	total, count, largest, largestName := 0, 0, 0, ""
	for _, f := range files {
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".json") {
			continue
		}
		raw, err := dictFS.ReadFile(path.Join(dictsDir, f.Name()))
		require.NoError(t, err)
		total += len(raw)
		count++
		if len(raw) > largest {
			largest, largestName = len(raw), f.Name()
		}
	}

	t.Logf("%d dictionaries, %.2f MB embedded (%.0f%% of the %.0f MB budget); largest %s at %.2f MB",
		count, float64(total)/1e6, float64(total)/embedWarnBytes*100, float64(embedWarnBytes)/1e6,
		largestName, float64(largest)/1e6)
	assert.LessOrEqualf(t, total, embedWarnBytes,
		"embedded dictionaries are %.2f MB, over the %.0f MB budget: defer the largest files "+
			"by size descending and journal them in IMPORT_MANIFEST.md",
		float64(total)/1e6, float64(embedWarnBytes)/1e6)
}

// Seeding is startup latency, and startup latency at 439 dictionaries is not
// the rounding error it was at ten. A regression here is a slow deploy and a
// slow test suite long before anyone reads a profile.
func TestSeedingFitsTheStartupBudget(t *testing.T) {
	core, err := NewCore(0)
	require.NoError(t, err)

	start := time.Now()
	reg, err := NewRegistry(core)
	elapsed := time.Since(start)
	require.NoError(t, err)

	t.Logf("seeded %d dictionaries in %v (%.0f%% of the %v budget)",
		len(reg.Catalogue()), elapsed.Round(time.Millisecond),
		float64(elapsed)/float64(seedBudget)*100, seedBudget)
	assert.Lessf(t, elapsed, seedBudget,
		"NewRegistry took %v against a %v budget", elapsed.Round(time.Millisecond), seedBudget)
}

// The catalogue is one response, served on every cold client load, and it grew
// 44× with this import. Reported rather than asserted: what the right ceiling
// is depends on a product decision about paging the catalogue that has not been
// made, and a number invented here would be the one someone raises the first
// time it fails. The gzipped figure is the one that matters — it is what
// crosses the wire.
func TestCataloguePayloadSizeIsReported(t *testing.T) {
	_, reg := sharedDicts(t)

	raw, err := json.Marshal(reg.Catalogue())
	require.NoError(t, err)
	gz, err := gzipBytes(raw)
	require.NoError(t, err)

	t.Logf("catalogue: %d rows, %.1f kB raw, %.1f kB gzipped (%.1f%%), %.0f B/row raw",
		len(reg.Catalogue()), float64(len(raw))/1000, float64(len(gz))/1000,
		float64(len(gz))/float64(len(raw))*100, float64(len(raw))/float64(len(reg.Catalogue())))
}

var keyPattern = regexp.MustCompile(`^[a-z0-9_]+$`)

// The naming contract, asserted rather than trusted (docs/DICTIONARIES.md).
// A key is written into runs, match settings and bucket keys, so a stray
// character in one is expensive in exactly the places that are hard to migrate.
func TestEveryKeyMatchesTheNamingContract(t *testing.T) {
	_, reg, _ := newTestServer(t)
	for _, e := range reg.Catalogue() {
		assert.Regexpf(t, keyPattern, e.Lang, "dictionary key %q is not ^[a-z0-9_]+$", e.Lang)
	}
}

// displayNames has to hold in BOTH directions. A missing row is already a
// startup error; an extra row is the quieter half — it is how a table drifts
// into naming languages that no longer exist, and it makes "every dictionary is
// named" impossible to read off the table's length.
func TestEveryDictionaryHasADisplayNameAndViceVersa(t *testing.T) {
	files, err := fs.ReadDir(dictFS, dictsDir)
	require.NoError(t, err)

	embedded := make(map[string]bool, len(files))
	for _, f := range files {
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".json") {
			continue
		}
		lang := strings.TrimSuffix(f.Name(), ".json")
		embedded[lang] = true
		_, err := displayName(lang)
		assert.NoErrorf(t, err, "embedded dictionary %q has no display name", lang)
	}
	for lang := range displayNames {
		assert.Truef(t, embedded[lang],
			"displayNames names %q, which is not an embedded dictionary: remove the row", lang)
	}
}

// The per-language sanity gate.
//
// A malformed dictionary must fail at IMPORT, not at a player's first run. The
// registry already refuses an unparseable or nameless file, but "the JSON
// parses" is a long way from "this language is playable": the words go through
// the core's generator, the generated targets go through its metrics, and a
// corpus can be well-formed JSON and still produce nothing to type — an entry
// that is all whitespace, a word list the decorator rejects, a script whose
// graphemes the fold miscounts.
//
// So every language folds one short generated run through the goja bundle and
// three things are asserted about the result: targets are non-empty, the
// metrics that come back are finite, and the dictVersion the generator was
// handed round-trips to the dictionary's own hash. The last one is what ties
// the generated text to the catalogue row a client would have used to fetch it.
//
// It is table-driven over the real catalogue rather than a fixture list,
// because a fixture list is a thing to forget to update — the point is that
// adding a dictionary automatically adds its gate.
func TestEveryLanguageGeneratesAPlayableRun(t *testing.T) {
	core, reg := sharedDicts(t)

	setup := perf.MustJSON(perf.BuildSetup(perf.SetupSpec{
		Mode: "words", WordCount: sanityWords, DurationMs: 600_000,
	}))

	for _, e := range reg.Catalogue() {
		t.Run(e.Lang, func(t *testing.T) {
			body, ok := reg.Body(e.DictHash)
			require.True(t, ok, "catalogue advertises a hash with no body")

			// 1. The generator produces something to type.
			words := generateSanityWords(t, core, body, e.DictHash)
			require.NotEmptyf(t, words, "%s: generated no targets", e.Lang)
			for i, w := range words {
				assert.NotEmptyf(t, w, "%s: target %d is empty", e.Lang, i)
			}

			// 2. dictVersion round-trips: the hash the generator was handed is
			//    the hash of the word list it generated from, which is what
			//    ties this text to the catalogue row a client fetched it by.
			var doc dictDoc
			require.NoError(t, json.Unmarshal(body, &doc))
			roundTrip, err := core.DictVersion(doc.Words)
			require.NoError(t, err)
			assert.Equalf(t, e.DictHash, roundTrip,
				"%s: catalogue publishes %s but the word list hashes to %s",
				e.Lang, e.DictHash, roundTrip)

			// 3. A flawless run of those targets folds to a valid verdict with
			//    finite metrics. This is the whole point of the gate: it drives
			//    core.Replay, the same entry point the worker calls, so a
			//    corpus that breaks the fold fails here rather than on the
			//    first player who picks the language.
			res, err := core.Replay(t.Context(), Input{
				Seed:         sanitySeed,
				DictHash:     e.DictHash,
				DictBody:     body,
				Setup:        setup,
				Log:          typeOut(words),
				ScoreVersion: 2,
			})
			require.NoErrorf(t, err, "%s: replay errored", e.Lang)
			require.Equalf(t, "valid", res.Verdict,
				"%s: a flawless run was judged %s (%s)", e.Lang, res.Verdict, res.Reason)
			require.NotEmptyf(t, res.Metrics, "%s: a valid verdict produced no metrics", e.Lang)
			assertFiniteNumbers(t, e.Lang, "metrics", res.Metrics)
			assertFiniteNumbers(t, e.Lang, "score", res.Score)
		})
	}
}

const (
	sanityWords = 8
	sanitySeed  = 424242
)

// generateSanityWords asks the core for the targets a sanityWords-long run on
// this dictionary would present. The words come from the bundle, not from the
// file, because the decoration rules are the bundle's.
func generateSanityWords(t *testing.T, core *Core, dictBody []byte, dictHash string) []string {
	t.Helper()
	ctx := t.Context()

	dict, err := core.parseJSON(ctx, string(dictBody))
	require.NoError(t, err)
	seedCtx, err := core.parseJSON(ctx, fmt.Sprintf(
		`{"seed":%d,"dictVersion":%q,"generation":{"mode":"words","length":%d,`+
			`"punctuation":false,"numbers":false,"randomCase":false,"reverse":false}}`,
		sanitySeed, dictHash, sanityWords))
	require.NoError(t, err)

	generated, err := core.call(ctx, core.generateWords, dict, seedCtx)
	require.NoError(t, err)
	value, err := core.unwrapResult(ctx, generated)
	require.NoError(t, err)
	raw, err := core.stringifyJSON(ctx, value.(*goja.Object).Get("words"))
	require.NoError(t, err)

	var words []string
	require.NoError(t, json.Unmarshal(raw, &words))
	return words
}

// typeOut renders the event log a flawless run of these targets emits: one
// insert per grapheme, one commit per word, monotonic seq from 1 and monotonic
// time — the shape docs/RUNS.md specifies and the golden vectors carry.
func typeOut(words []string) json.RawMessage {
	events := make([]map[string]any, 0, 64)
	seq, at := 1, 0
	for _, w := range words {
		for _, r := range w {
			at += 90
			events = append(events, map[string]any{
				"kind": "insert", "seq": seq, "t": at, "text": string(r),
			})
			seq++
		}
		at += 90
		events = append(events, map[string]any{"kind": "commit", "seq": seq, "t": at})
		seq++
	}
	return perf.MustJSON(map[string]any{"version": 1, "events": events})
}

// assertFiniteNumbers walks a core-produced JSON document and fails on a number
// that is not finite, or on a null where a number belongs.
//
// The null case is the one that matters, and it is not paranoia: JSON cannot
// represent NaN or Infinity, so `JSON.stringify` renders both as `null`. A NaN
// that escaped the fold therefore reaches Go as a null, not as a number Go
// could test — which means a null in a metrics object IS the NaN detector, and
// the only one available on this side of the bridge.
func assertFiniteNumbers(t *testing.T, lang, what string, doc json.RawMessage) {
	t.Helper()
	if len(doc) == 0 {
		return
	}
	dec := json.NewDecoder(strings.NewReader(string(doc)))
	dec.UseNumber()
	var v any
	require.NoError(t, dec.Decode(&v))

	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch x := v.(type) {
		case nil:
			assert.Failf(t, "non-finite number",
				"%s: %s%s is null — JSON.stringify renders NaN and Infinity as null, "+
					"so this is a metric the fold could not compute", lang, what, path)
		case json.Number:
			f, err := x.Float64()
			assert.NoErrorf(t, err, "%s: %s%s is not a number", lang, what, path)
			assert.Falsef(t, math.IsNaN(f) || math.IsInf(f, 0),
				"%s: %s%s is %v", lang, what, path, x)
		case map[string]any:
			for k, e := range x {
				walk(path+"."+k, e)
			}
		case []any:
			for i, e := range x {
				walk(fmt.Sprintf("%s[%d]", path, i), e)
			}
		}
	}
	walk("", v)
}
