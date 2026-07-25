// Package quote is the fixed-text run domain: the published corpus everyone
// types the same bytes of, and how a reader browses, draws and resolves one.
// See SCORING_CONCEPT §6 and docs/QUOTES.md.
//
// # A published artefact, not a mutable row
//
// A quote is the osu!-beatmap analogue: the text IS the map, and a score only
// means something next to other scores on the same bytes. So a quote is never
// edited. A corrected upstream text is a NEW row under the same
// (lang, upstream_id), and the previous row is marked superseded — still
// fetchable by id forever, because every run played on it regenerates its text
// from that id. This is the same rule a published dict_hash lives under
// (docs/DICTIONARIES.md); the reason is identical, and so is the failure mode
// when it is broken.
//
// # One hashing implementation
//
// text_hash is replay.Core.DictVersion over a ONE-ELEMENT slice — the FNV-1a
// the vendored core bundle computes, reached through the binding that already
// exists. The NUL join the core applies between words is a no-op on one
// element, so the digest is exactly fnv1a(text) in the same convention
// dict_hash uses. There is no Go FNV-1a in this package and there must never
// be one: see HashText.
//
// # Layering
//
// Like the other domains, quote declares its dependencies as consumer-side
// interfaces (Store for reads, DictVersioner for hashing) and imports no
// sibling domain. The vendored corpus and the importer live one level down in
// quote/quotes and quote/corpus so that the server binary, which reads quotes
// out of Postgres, never links a megabyte of JSON it will not open.
package quote
