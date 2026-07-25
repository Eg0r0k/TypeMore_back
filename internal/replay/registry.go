package replay

import (
	"bytes"
	"compress/gzip"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"strings"
)

// dictFS holds the dictionaries. This directory is the ONLY copy of them: the
// frontend no longer ships static/languages, it reads the catalogue and the
// hash-addressed bodies from this server.
//
//go:embed dicts/*.json
var dictFS embed.FS

const dictsDir = "dicts"

// CatalogueEntry is one row of GET /api/v1/dictionaries — the catalogue clients
// use to pick a language and to learn the hash its body is addressed by.
type CatalogueEntry struct {
	// Lang is the dictionary's code, i.e. its file name without .json. This is
	// the value that travels as `lang` in run and match payloads.
	Lang string `json:"lang"`
	// Name is the dictionary's own display name (a dictionary may be named
	// "russian_1k" while its code is "russian").
	Name string `json:"name"`
	// DictHash is the core's FNV-1a fingerprint of the word list, and the path
	// segment of the body: /static/dictionaries/{dictHash}.json.
	DictHash string `json:"dictHash"`
	// WordCount is the number of words in the list.
	WordCount int `json:"wordCount"`
	// Bytes is the exact length of the uncompressed body.
	Bytes int `json:"bytes"`
}

// entry is a seeded dictionary: its catalogue row plus the pre-built response
// bodies. Both encodings are produced once at startup — a request never
// marshals, never compresses, and never touches a file.
type entry struct {
	CatalogueEntry
	raw []byte
	gz  []byte
}

// Registry is the immutable, fully-seeded set of dictionaries. It is built once
// at startup and never mutated, so it is safe for concurrent use without a lock.
type Registry struct {
	entries []entry           // sorted by Lang (embed.FS walks in name order)
	byHash  map[string]*entry // dictHash -> body, for the static endpoint
}

// dictDoc is the on-disk dictionary shape. Only the fields the registry needs
// are modelled; the body is served verbatim, so extras (bcp47, rightToleft…)
// travel to the client untouched.
type dictDoc struct {
	Name  string   `json:"name"`
	Words []string `json:"words"`
}

// NewRegistry seeds the registry from the embedded dictionaries, computing every
// fingerprint through the goja bundle. Any problem — unparseable file, empty
// word list, or two dictionaries hashing to the same value — is a startup
// error: a half-seeded catalogue would hand clients a language they cannot
// fetch, or a hash that resolves to the wrong words.
func NewRegistry(core *Core) (*Registry, error) {
	files, err := fs.ReadDir(dictFS, dictsDir)
	if err != nil {
		return nil, fmt.Errorf("replay: read dictionaries: %w", err)
	}

	reg := &Registry{
		entries: make([]entry, 0, len(files)),
		byHash:  make(map[string]*entry, len(files)),
	}
	for _, f := range files {
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".json") {
			continue
		}
		e, err := loadDict(core, f.Name())
		if err != nil {
			return nil, err
		}
		reg.entries = append(reg.entries, e)
	}
	if len(reg.entries) == 0 {
		return nil, fmt.Errorf("replay: no dictionaries embedded from %s/", dictsDir)
	}

	// Index after the slice is final — earlier pointers would dangle on growth.
	for i := range reg.entries {
		e := &reg.entries[i]
		if prev, dup := reg.byHash[e.DictHash]; dup {
			return nil, fmt.Errorf(
				"replay: dictionaries %q and %q share dict_hash %s: content addressing requires distinct word lists",
				prev.Lang, e.Lang, e.DictHash)
		}
		reg.byHash[e.DictHash] = e
	}
	return reg, nil
}

func loadDict(core *Core, fileName string) (entry, error) {
	lang := strings.TrimSuffix(fileName, ".json")
	raw, err := dictFS.ReadFile(path.Join(dictsDir, fileName))
	if err != nil {
		return entry{}, fmt.Errorf("replay: read dictionary %q: %w", lang, err)
	}

	var doc dictDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return entry{}, fmt.Errorf("replay: parse dictionary %q: %w", lang, err)
	}
	if doc.Name == "" {
		return entry{}, fmt.Errorf("replay: dictionary %q has no name", lang)
	}
	if len(doc.Words) == 0 {
		return entry{}, fmt.Errorf("replay: dictionary %q has no words", lang)
	}

	hash, err := core.DictVersion(doc.Words)
	if err != nil {
		return entry{}, fmt.Errorf("replay: hash dictionary %q: %w", lang, err)
	}

	gz, err := gzipBytes(raw)
	if err != nil {
		return entry{}, fmt.Errorf("replay: compress dictionary %q: %w", lang, err)
	}

	return entry{
		CatalogueEntry: CatalogueEntry{
			Lang:      lang,
			Name:      doc.Name,
			DictHash:  hash,
			WordCount: len(doc.Words),
			Bytes:     len(raw),
		},
		raw: raw,
		gz:  gz,
	}, nil
}

// Catalogue returns the catalogue rows, ordered by language code.
func (r *Registry) Catalogue() []CatalogueEntry {
	out := make([]CatalogueEntry, len(r.entries))
	for i := range r.entries {
		out[i] = r.entries[i].CatalogueEntry
	}
	return out
}

// Body returns the exact stored bytes of the dictionary with the given hash.
// The slice is the registry's own — callers MUST NOT modify it.
func (r *Registry) Body(dictHash string) ([]byte, bool) {
	e, ok := r.byHash[dictHash]
	if !ok {
		return nil, false
	}
	return e.raw, true
}

// gzipBytes compresses at the highest level: it happens once per dictionary at
// startup, so trading CPU for a permanently smaller response is free.
func gzipBytes(src []byte) ([]byte, error) {
	var buf bytes.Buffer
	buf.Grow(len(src) / 2)
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	if _, err := zw.Write(src); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
