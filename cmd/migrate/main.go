// Command migrate applies (or rolls back) database migrations using goose with
// the embedded SQL files.
//
// Usage:
//
//	migrate            # same as "up": apply all pending migrations
//	migrate up
//	migrate down       # roll back the most recent migration
//	migrate status
//	migrate version
//
// The database URL is read from TYPEMORE_DATABASE_URL (same default as the
// server). New migration files are created with the goose CLI, not this command
// (embedded files are read-only): `make migrate-create name=add_widgets`.
package main

import (
	"context"
	"log"
	"os"

	"github.com/typemore/typemore-server/internal/platform"
	"github.com/typemore/typemore-server/internal/platform/migrate"
)

func main() {
	command := "up"
	if len(os.Args) > 1 {
		command = os.Args[1]
	}
	args := os.Args[2:]

	cfg, err := platform.LoadConfig()
	if err != nil {
		log.Fatalf("migrate: load config: %v", err)
	}

	if err := migrate.Run(context.Background(), cfg.DatabaseURL, command, args...); err != nil {
		log.Fatalf("migrate: %v", err)
	}
	log.Printf("migrate: %q completed", command)
}
