package quote

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// ErrNotFound is returned by the store when no quote matches — an unknown id,
// or a filter combination the corpus has nothing for.
var ErrNotFound = errors.New("quote: not found")

// LenGroup is a quote's length band. The numbers are the stored `len_group`
// column; the names are what the API speaks, because "short" survives a
// threshold change and "0" does not.
//
// The BOUNDS are deliberately absent from this type. They are per-corpus —
// chinese uses 30/80/200 where every other corpus uses 100/300/600 — so they
// live in each vendored file's own `groups` array and are read at import time.
// A threshold table in Go would be a bug that only shows up on one language.
type LenGroup int16

// The four bands upstream publishes for every corpus.
const (
	LenShort LenGroup = iota
	LenMedium
	LenLong
	LenThicc
	// LenGroupCount bounds the valid range; the DB has the same CHECK, and the
	// importer asserts every vendored corpus publishes exactly this many bands.
	LenGroupCount = 4
)

var lenGroupNames = [LenGroupCount]string{"short", "medium", "long", "thicc"}

// String renders the group as the name the API uses.
func (g LenGroup) String() string {
	if g < 0 || int(g) >= LenGroupCount {
		return fmt.Sprintf("LenGroup(%d)", int16(g))
	}
	return lenGroupNames[g]
}

// Valid reports whether g names one of the four published bands.
func (g LenGroup) Valid() bool { return g >= 0 && int(g) < LenGroupCount }

// ParseLenGroup resolves a `group=` query value. Only the four names are
// accepted: the ordinals are a storage detail, and letting "2" through would
// freeze the numbering into the public contract.
func ParseLenGroup(s string) (LenGroup, error) {
	for i, name := range lenGroupNames {
		if s == name {
			return LenGroup(i), nil
		}
	}
	return 0, fmt.Errorf("quote: unknown length group %q", s)
}

// Meta is one quote WITHOUT its text — everything a client needs to list,
// filter and link to a quote, and nothing it needs to type one.
//
// The absence of Text is load-bearing, not an oversight: /quotes is a browsable
// index, and an index that carries the bodies is a one-pass corpus scrape.
type Meta struct {
	ID         uuid.UUID
	Lang       string
	UpstreamID int32
	Source     string
	Length     int32
	LenGroup   LenGroup
	TextHash   string
	Superseded bool
	CreatedAt  time.Time
}

// Quote is a full quote, text included. Only the two single-quote endpoints
// return one: starting a run, and resolving the text a stored run was played on.
type Quote struct {
	Meta
	Text string
}

// Filter narrows a browse or a draw. Both fields are optional; a zero Filter
// means the whole published corpus.
type Filter struct {
	// Lang is OUR language id, empty for "any".
	Lang string
	// Group is a length band, nil for "any".
	Group *LenGroup
}

// Cursor is a keyset position in the browse ordering: the (lang, len_group, id)
// of the last row a page returned. The triple is the index's own order and its
// final component is a primary key, so the ordering is total — which is what
// makes the walk stable while rows are being inserted around it.
type Cursor struct {
	Lang  string
	Group LenGroup
	ID    uuid.UUID
}

// Store is the quote read model, declared here at the consumer.
//
// Reads only. The corpus is written exactly once per revision by the importer
// (`make import-quotes`), which reaches the write side through its own
// interface in the pgstore package — the HTTP surface has no business being
// able to publish a quote.
type Store interface {
	// Page returns up to limit published quotes' metadata in (lang, len_group,
	// id) order, continuing after the given position when non-nil. Superseded
	// revisions are excluded.
	Page(ctx context.Context, f Filter, after *Cursor, limit int32) ([]Meta, error)
	// Random returns one published quote, text included, drawn uniformly from
	// the rows the filter admits. ErrNotFound when the filter admits none.
	Random(ctx context.Context, f Filter) (Quote, error)
	// ByID returns one quote, text included, INCLUDING superseded revisions: a
	// run recorded against a retired quote must stay watchable forever.
	ByID(ctx context.Context, id uuid.UUID) (Quote, error)
}

// --- the replay side of the domain ---

// ReplayResolver adapts this read model to the narrow resolver the replay
// worker declares at its own consumer end (`replay.QuoteResolver`).
//
// It exists so the dependency runs ONE way. `internal/replay` owns the goja
// bundle that this package hashes text with; if it also imported this package
// to read a quote back, the two would be mutually dependent for no reason. So
// the seam speaks primitives only — id in, bytes and digest out — exactly as
// DictVersioner below speaks primitives in the other direction.
//
// The (value, ok, err) shape mirrors `replay.Registry.Body`, and the split
// matters: a quote that does not exist is a run the server cannot judge yet
// (ok=false, flagged), while a database that will not answer is a transient
// failure to retry (err). Collapsing them would turn an outage into a wave of
// permanent verdicts.
type ReplayResolver struct{ Store Store }

// ResolveQuote returns the stored text and text_hash of one quote, superseded
// revisions included — a run played on a since-retired revision must replay
// forever, which is the whole reason ByID serves them.
func (r ReplayResolver) ResolveQuote(ctx context.Context, id uuid.UUID) (text, textHash string, ok bool, err error) {
	q, err := r.Store.ByID(ctx, id)
	switch {
	case errors.Is(err, ErrNotFound):
		return "", "", false, nil
	case err != nil:
		return "", "", false, err
	}
	return q.Text, q.TextHash, true, nil
}

// --- the import side of the domain ---

// DictVersioner is the corpus-digest hasher: satisfied by *replay.Core, which
// runs the vendored bundle's dictVersion() in goja. Declared here so this
// package can hash without importing a sibling domain, and narrow enough that
// nobody is tempted to satisfy it with a fresh Go FNV-1a.
type DictVersioner interface {
	DictVersion(words []string) (string, error)
}

// HashText fingerprints one quote's text — the value stored in `text_hash`.
//
// It is DictVersion over a single-element slice, on purpose. The core joins
// words with NUL before folding them, so one element makes the join a no-op and
// the result is exactly fnv1a(text) in the same digest convention `dict_hash`
// already uses. That is why there is no new goja binding for this and must not
// be one: the existing export is sufficient, and a second hashing entry point
// is a second hash to keep in step.
func HashText(dv DictVersioner, text string) (string, error) {
	return dv.DictVersion([]string{text})
}

// Incoming is one quote as the importer presents it: upstream's own fields plus
// the two values this system derives (LenGroup from the corpus's own thresholds,
// TextHash from the core). The language is not on the row because a whole
// language is published in one transaction.
type Incoming struct {
	UpstreamID int32
	Text       string
	Source     string
	Length     int32
	LenGroup   LenGroup
	TextHash   string
}

// ImportStats is what publishing one language did. The three counts partition
// the language's quotes exactly, which is what makes "run it twice, the second
// pass is all Unchanged" a meaningful assertion rather than a vibe.
type ImportStats struct {
	// Inserted: no revision existed for that (lang, upstream_id).
	Inserted int
	// Superseded: a DIFFERENT text was published there; the new bytes landed as
	// a new row and the old one was retired.
	Superseded int
	// Unchanged: the published revision already carries these exact bytes.
	Unchanged int
}

// Total is the number of quotes the pass considered.
func (s ImportStats) Total() int { return s.Inserted + s.Superseded + s.Unchanged }

// Add accumulates another language's counts.
func (s *ImportStats) Add(o ImportStats) {
	s.Inserted += o.Inserted
	s.Superseded += o.Superseded
	s.Unchanged += o.Unchanged
}
