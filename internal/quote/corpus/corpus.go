// Package corpus reads the vendored upstream quote corpora into rows the
// importer can publish. It is the only consumer of internal/quote/quotes, which
// keeps the megabytes of embedded JSON out of the server binary.
//
// MANIFEST.json is the source of truth, never a directory listing: it maps each
// upstream file to OUR language id (upstream calls them arabic and
// chinese_simplified where we serve arabian and chinese), records the count and
// the length thresholds we expect that file to carry, and documents which
// languages are deliberately absent. Adding a language is a manifest row plus
// its file — a stray JSON in the directory imports nothing.
package corpus

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/typemore/typemore-server/internal/quote"
	"github.com/typemore/typemore-server/internal/quote/quotes"
)

// ManifestFile is the manifest's name inside the embedded corpus directory.
const ManifestFile = "MANIFEST.json"

// Manifest is internal/quote/quotes/MANIFEST.json.
type Manifest struct {
	Upstream  Upstream   `json:"upstream"`
	Languages []Language `json:"languages"`
	Excluded  []Excluded `json:"excluded"`
}

// Upstream is the provenance of every vendored file: which repository, and the
// exact commit they were taken from. Pinned because "the latest monkeytype" is
// not a thing anyone can re-derive a corpus from two years later.
type Upstream struct {
	Repo   string `json:"repo"`
	Commit string `json:"commit"`
	Path   string `json:"path"`
}

// Language is one manifest row: an upstream file published under our language id.
type Language struct {
	// Lang is OUR language id — the dictionary catalogue's id, not upstream's
	// file name.
	Lang string `json:"lang"`
	// File is the vendored file beside MANIFEST.json.
	File string `json:"file"`
	// Quotes is the count we expect that file to hold.
	Quotes int `json:"quotes"`
	// Groups is the corpus's OWN short/medium/long/thicc thresholds, recorded
	// here so a test can assert the vendored copy still matches what we read.
	Groups [][2]int `json:"groups"`
	Why    string   `json:"why"`
}

// Excluded is one upstream language we deliberately do not import, with the
// reason. Kept in the manifest so "why is there no japanese?" has an answer that
// does not depend on anyone remembering.
type Excluded struct {
	Lang           string `json:"lang"`
	UpstreamQuotes int    `json:"upstream_quotes,omitempty"`
	Why            string `json:"why"`
}

// corpusFile is the vendored upstream shape, verbatim. Only the fields we
// publish are modelled; upstream may add more and they simply ride along unread.
type corpusFile struct {
	Language string   `json:"language"`
	Groups   [][2]int `json:"groups"`
	Quotes   []struct {
		Text   string `json:"text"`
		Source string `json:"source"`
		Length int    `json:"length"`
		ID     int32  `json:"id"`
	} `json:"quotes"`
}

// ReadManifest parses the embedded manifest.
func ReadManifest() (Manifest, error) {
	raw, err := quotes.FS.ReadFile(ManifestFile)
	if err != nil {
		return Manifest{}, fmt.Errorf("quote/corpus: read %s: %w", ManifestFile, err)
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return Manifest{}, fmt.Errorf("quote/corpus: parse %s: %w", ManifestFile, err)
	}
	if len(m.Languages) == 0 {
		return Manifest{}, fmt.Errorf("quote/corpus: %s lists no languages", ManifestFile)
	}
	return m, nil
}

// Load reads one manifest row's corpus and returns its quotes ready to publish,
// in upstream order, each already carrying its length group and its text hash.
//
// Every check below is a hard failure rather than a skipped row. A half-imported
// language is worse than none: the corpus is what a score is compared against,
// so "most of russian" is a leaderboard whose population depends on which
// quotes happened to parse.
func Load(dv quote.DictVersioner, lang Language) ([]quote.Incoming, error) {
	raw, err := quotes.FS.ReadFile(lang.File)
	if err != nil {
		return nil, fmt.Errorf("quote/corpus: read %s: %w", lang.File, err)
	}
	var doc corpusFile
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("quote/corpus: parse %s: %w", lang.File, err)
	}

	// The manifest claims this file IS that upstream corpus. Upstream names the
	// corpus inside the file too, so the mapping row can be checked rather than
	// trusted — this is what catches a copy-pasted `file` pointing at the wrong
	// language, which would otherwise publish german quotes as arabian.
	if want := strings.TrimSuffix(lang.File, ".json"); doc.Language != want {
		return nil, fmt.Errorf("quote/corpus: %s declares language %q, want %q",
			lang.File, doc.Language, want)
	}
	if len(doc.Quotes) != lang.Quotes {
		return nil, fmt.Errorf("quote/corpus: %s holds %d quotes, manifest says %d",
			lang.File, len(doc.Quotes), lang.Quotes)
	}
	if err := sameGroups(doc.Groups, lang.Groups); err != nil {
		return nil, fmt.Errorf("quote/corpus: %s: %w", lang.File, err)
	}

	out := make([]quote.Incoming, 0, len(doc.Quotes))
	seen := make(map[int32]struct{}, len(doc.Quotes))
	for i := range doc.Quotes {
		q := &doc.Quotes[i]
		if _, dup := seen[q.ID]; dup {
			return nil, fmt.Errorf("quote/corpus: %s has two quotes with id %d", lang.File, q.ID)
		}
		seen[q.ID] = struct{}{}

		// Upstream's `length` is JS String.length — UTF-16 code units — and its
		// thresholds are expressed in it. Postgres stores char_length, which
		// counts characters, and the two agree for everything in the BMP. A
		// corpus where they diverge (an astral character: emoji, rare CJK) is
		// one where "length" means two different things on the two sides of the
		// wire, so it stops the import instead of quietly storing a number the
		// CHECK constraint would reject anyway.
		if runes := utf8.RuneCountInString(q.Text); runes != q.Length {
			return nil, fmt.Errorf(
				"quote/corpus: %s #%d: upstream length %d but %d characters — "+
					"the text contains characters outside the BMP and the two length "+
					"conventions have diverged",
				lang.File, q.ID, q.Length, runes)
		}

		group, err := groupOf(doc.Groups, q.Length)
		if err != nil {
			return nil, fmt.Errorf("quote/corpus: %s #%d: %w", lang.File, q.ID, err)
		}
		hash, err := quote.HashText(dv, q.Text)
		if err != nil {
			return nil, fmt.Errorf("quote/corpus: %s #%d: hash: %w", lang.File, q.ID, err)
		}

		out = append(out, quote.Incoming{
			UpstreamID: q.ID,
			Text:       q.Text,
			Source:     q.Source,
			Length:     int32(q.Length),
			LenGroup:   group,
			TextHash:   hash,
		})
	}
	return out, nil
}

// groupOf assigns a length band from THIS corpus's own thresholds.
//
// The bounds are a parameter, never a constant: chinese uses 30/80/200 where
// every other corpus uses 100/300/600, so a global threshold table would
// silently file 320 of chinese's 330 quotes under the wrong band. That is
// exactly the bug TestChineseThresholdsDifferFromTheRest exists to catch.
func groupOf(groups [][2]int, length int) (quote.LenGroup, error) {
	for i, g := range groups {
		if length >= g[0] && length <= g[1] {
			return quote.LenGroup(i), nil
		}
	}
	return 0, fmt.Errorf("length %d falls outside every group in %v", length, groups)
}

// sameGroups checks the vendored file still carries the thresholds the manifest
// recorded. If upstream moves a boundary, the re-import supersedes nothing (the
// text is unchanged) but every len_group silently shifts — so the divergence has
// to be a failure, not a diff nobody reads.
func sameGroups(got, want [][2]int) error {
	if len(want) == 0 {
		return fmt.Errorf("manifest records no groups")
	}
	if len(got) != len(want) {
		return fmt.Errorf("file has %d groups, manifest records %d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			return fmt.Errorf("group %d is %v, manifest records %v", i, got[i], want[i])
		}
	}
	if len(got) != int(quote.LenGroupCount) {
		return fmt.Errorf("file has %d groups, the schema stores %d",
			len(got), quote.LenGroupCount)
	}
	return nil
}
