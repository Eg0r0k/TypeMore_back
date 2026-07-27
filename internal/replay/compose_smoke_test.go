package replay

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/typemore/typemore-server/internal/perf"
)

// Compose smoke for the mass import.
//
// Every other gate in this package checks one layer: the registry seeds, the
// catalogue names, the generator produces words, the frozen hashes hold. This
// one checks that a NEWLY imported language composes all the way through — a
// run is generated from its dictionary, typed, validated, scored, and comes
// back `accepted` from the same worker path a real submission takes.
//
// Three seeded languages are spot-checked and they are chosen to be three
// different shapes of new, because the import produced three different shapes:
// a plain language, a `_1k` size variant (the naming clause that says a variant
// keeps a plain key), and a `code_*` dictionary (the clause that says code
// dictionaries live in one family). A fourth case is a quote run on a
// newly-imported corpus, which takes the other text path entirely — no
// dictionary, no seeded regeneration, the bytes resolved by id.
//
// On the client numbers. The payload's `clientMetrics`/`clientScore` are taken
// from a first pass through the core rather than from a recorded browser
// session, so this test cannot catch a client/server scoring divergence — the
// golden vectors in testdata/ exist for that and are produced by running the
// real core in Node. What it does catch, and what the import could plausibly
// break, is everything else on the path: a dictionary whose words the generator
// refuses, a script whose graphemes the validator counts differently from the
// log that typed them, a corpus that scores to something the policy rejects.
func TestNewlyImportedLanguagesComposeEndToEnd(t *testing.T) {
	core, reg := sharedDicts(t)

	// Not the ten that were already published — the point is the new ones.
	for _, tc := range []struct {
		lang string
		why  string
	}{
		{"polish", "a plain language on a Latin script with diacritics"},
		{"italian_1k", "a size variant: the naming clause that keeps a plain key"},
		{"code_rust", "a code dictionary: raw tokens, no prose"},
		{"hebrew", "right-to-left, to prove RTL is shipped and not merely listed"},
	} {
		t.Run(tc.lang, func(t *testing.T) {
			var entry CatalogueEntry
			for _, e := range reg.Catalogue() {
				if e.Lang == tc.lang {
					entry = e
				}
			}
			require.Equalf(t, tc.lang, entry.Lang, "%s is not in the catalogue", tc.lang)
			body, ok := reg.Body(entry.DictHash)
			require.True(t, ok)

			words := generateSanityWords(t, core, body, entry.DictHash)
			require.NotEmpty(t, words)

			setup := perf.MustJSON(perf.BuildSetup(perf.SetupSpec{
				Mode: "words", WordCount: sanityWords, DurationMs: 600_000,
			}))
			log := typeOut(words)

			// First pass: what the core says this run is worth. A real client
			// computes these from the same bundle in the browser.
			first, err := core.Replay(context.Background(), Input{
				Seed: sanitySeed, DictHash: entry.DictHash, DictBody: body,
				Setup: setup, Log: log, ScoreVersion: 2,
			})
			require.NoError(t, err)
			require.Equalf(t, "valid", first.Verdict, "%s: %s", tc.lang, first.Reason)

			d := judgeOne(t, PendingRun{
				ID:            uuid.New(),
				Seed:          sanitySeed,
				DictHash:      entry.DictHash,
				ScoreVersion:  2,
				Setup:         setup,
				ClientMetrics: asClientMetrics(t, first.Metrics),
				ClientScore:   first.Score,
				Log:           gzipJSON(t, log),
			})

			assert.Equalf(t, "accepted", d.Status,
				"%s (%s) was not accepted: %s", tc.lang, tc.why, string(d.Validation))
			assert.Equal(t, "valid", audit(t, d).Verdict)
			assert.NotEmptyf(t, d.ServerMetrics, "%s: accepted with no server metrics", tc.lang)
			assert.NotEmptyf(t, d.ServerScore, "%s: accepted with no server score", tc.lang)
		})
	}
}

// The other text path: a quote run on a corpus the quote import just added.
//
// A quote run carries no dictionary and no dimension — its text IS the bytes
// the server resolves by id, and `leaderboard_eligible_runs.quote_id` is what
// keeps it off every language board and on its own `quote:<id>` one
// (docs/LEADERBOARDS.md). What is asserted here is the half this package owns:
// the run is accepted, and it is accepted having been judged against the
// registry's bytes rather than anything the submission carried. The projection
// onto the board itself is internal/leaderboard's, and is covered there.
func TestANewLanguageQuoteRunIsAccepted(t *testing.T) {
	core, _ := sharedDicts(t)

	// A real quote from the newly vendored polish corpus, with the digest the
	// registry stores for it — computed here through the same bundle the
	// importer uses, which is the point: one hash convention, one code path.
	const text = "Nie ma nic bardziej praktycznego niz dobra teoria."
	hash, err := core.DictVersion([]string{text})
	require.NoError(t, err)

	id := uuid.New()
	words := splitOnSpaces(text)

	// `textSource` lives inside `generation`, not beside it: the generation
	// config is what decides where the text comes from, and the golden quote
	// vector carries it there.
	setup := perf.MustJSON(map[string]any{
		"config": map[string]any{
			"mode": "quote", "durationMs": 600_000, "maxExtraChars": 40,
			"difficulty": "normal", "nospace": false, "minWpm": 0,
		},
		"generation": map[string]any{
			"mode": "quote", "length": len(words), "punctuation": false,
			"numbers": false, "randomCase": false, "reverse": false,
			"textSource": map[string]any{
				"kind": "quote", "quoteId": id.String(), "quoteHash": hash,
			},
		},
		"declaration": map[string]any{"blind": false, "fading": false, "flashlight": false},
	})
	log := typeOut(words)

	quote := &QuoteText{Text: text, Hash: hash}
	first, err := core.Replay(context.Background(), Input{
		Seed: sanitySeed, Quote: quote, Setup: setup, Log: log, ScoreVersion: 2,
	})
	require.NoError(t, err)
	require.Equalf(t, "valid", first.Verdict, "quote run: %s", first.Reason)

	d := judgeOneWithQuotes(t, PendingRun{
		ID:            uuid.New(),
		Seed:          sanitySeed,
		ScoreVersion:  2,
		Setup:         setup,
		ClientMetrics: asClientMetrics(t, first.Metrics),
		ClientScore:   first.Score,
		Log:           gzipJSON(t, log),
	}, fakeQuotes{id: *quote})

	assert.Equal(t, "accepted", d.Status, string(d.Validation))
	assert.Equal(t, "valid", audit(t, d).Verdict)
	assert.NotEmpty(t, d.ServerScore)

	// The submitted hash is never what the run is judged against: the worker
	// resolves the text by id and uses the REGISTRY's digest. Proven by handing
	// it a registry whose bytes differ — the run must stop being valid.
	other := &QuoteText{Text: text + " I jeszcze jedno zdanie.", Hash: hash}
	bad := judgeOneWithQuotes(t, PendingRun{
		ID: uuid.New(), Seed: sanitySeed, ScoreVersion: 2, Setup: setup,
		ClientMetrics: asClientMetrics(t, first.Metrics), ClientScore: first.Score,
		Log: gzipJSON(t, log),
	}, fakeQuotes{id: *other})
	assert.NotEqual(t, "accepted", bad.Status,
		"the worker accepted a run judged against different bytes than it was typed on")
}

// asClientMetrics narrows the core's Metrics object to the three numbers a
// client actually submits.
//
// The rename is the whole reason this helper exists: the core calls the third
// one `accuracy` and the wire calls it `acc` (compareMetrics in decide.go says
// so in a comment). Passing the core's object through verbatim produces a
// payload whose `acc` is absent, which the comparison reads as a client/server
// divergence of null against 1 — a metric_mismatch that is entirely an artefact
// of the test, and one that would have been easy to "fix" by loosening the
// comparison.
func asClientMetrics(t *testing.T, serverMetrics json.RawMessage) json.RawMessage {
	t.Helper()
	var m struct {
		WPM      float64 `json:"wpm"`
		Raw      float64 `json:"raw"`
		Accuracy float64 `json:"accuracy"`
	}
	require.NoError(t, json.Unmarshal(serverMetrics, &m))
	return perf.MustJSON(map[string]any{"wpm": m.WPM, "raw": m.Raw, "acc": m.Accuracy})
}

// splitOnSpaces mirrors what generateWords does to a quote's text: the targets
// are the space-separated tokens, and nothing else about the string matters.
func splitOnSpaces(text string) []string {
	var out []string
	start := -1
	for i, r := range text {
		if r == ' ' {
			if start >= 0 {
				out = append(out, text[start:i])
				start = -1
			}
			continue
		}
		if start < 0 {
			start = i
		}
	}
	if start >= 0 {
		out = append(out, text[start:])
	}
	return out
}
