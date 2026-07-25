// Package quotes embeds the vendored upstream quote corpora and their manifest
// so `make import-quotes` needs nothing but the binary.
//
// It is its own package for one reason: the server reads quotes out of
// Postgres and never opens these files, so linking a megabyte of JSON into it
// would be pure weight. Only the import command imports internal/quote/corpus,
// which is the only importer of this package.
//
// MANIFEST.json is the source of truth for what gets imported — the importer
// reads it, never a directory listing, so adding a corpus is a manifest entry
// plus its file rather than "whatever happens to be in here".
package quotes

import "embed"

// FS holds MANIFEST.json and every vendored corpus beside it.
//
//go:embed *.json
var FS embed.FS
