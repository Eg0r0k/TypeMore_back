// Package migrations embeds the goose SQL migration files so they travel inside
// the binary. This lets both the migrate command and the integration tests apply
// the schema without shipping the .sql files alongside them.
package migrations

import "embed"

// FS holds every migration file in this directory. goose is pointed at it via
// goose.SetBaseFS.
//
//go:embed *.sql
var FS embed.FS
