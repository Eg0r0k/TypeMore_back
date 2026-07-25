// Command quotectl operates the quote registry against a live database.
//
//	quotectl import
//	    Publishes every corpus MANIFEST.json lists (`make import-quotes`).
//	    Idempotent: a second pass on unchanged vendored files reports every
//	    quote Unchanged and writes nothing. Changed bytes are published as a NEW
//	    revision beside the old one, which is retired rather than overwritten —
//	    published text is never edited, because old runs replay against it
//	    (docs/QUOTES.md, docs/DICTIONARIES.md).
//
// It reads the same TYPEMORE_ environment as the server, so it hashes with the
// same vendored core bundle the server and the client use.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/typemore/typemore-server/internal/platform"
	"github.com/typemore/typemore-server/internal/platform/db"
	"github.com/typemore/typemore-server/internal/quote"
	"github.com/typemore/typemore-server/internal/quote/corpus"
	quotepg "github.com/typemore/typemore-server/internal/quote/pgstore"
	"github.com/typemore/typemore-server/internal/replay"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "quotectl:", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 2 {
		return fmt.Errorf("usage: quotectl <import> [flags]")
	}
	command, args := os.Args[1], os.Args[2:]

	cfg, err := platform.LoadConfig()
	if err != nil {
		return err
	}

	switch command {
	case "import":
		fs := flag.NewFlagSet("import", flag.ExitOnError)
		lang := fs.String("lang", "", "import only this manifest row (default: all of them)")
		if err := fs.Parse(args); err != nil {
			return err
		}
		return importQuotes(cfg, *lang)

	default:
		return fmt.Errorf("unknown command %q (want import)", command)
	}
}

func importQuotes(cfg platform.Config, only string) error {
	ctx := context.Background()

	manifest, err := corpus.ReadManifest()
	if err != nil {
		return err
	}

	// The hasher is the real core bundle in goja. There is exactly one FNV-1a in
	// this system and it is the one the client runs; a Go reimplementation here
	// would be a second hash to keep in step, and a drifting text_hash makes
	// every run recorded against a quote unverifiable.
	core, err := replay.NewCore(cfg.ReplayTimeout)
	if err != nil {
		return err
	}

	pool, err := db.NewPool(ctx, cfg.DatabaseURL, cfg.DBMaxConns)
	if err != nil {
		return err
	}
	defer pool.Close()
	store := quotepg.New(pool)

	before, err := store.Count(ctx)
	if err != nil {
		return err
	}

	fmt.Printf("importing quotes from %s @ %s\n\n", manifest.Upstream.Repo, manifest.Upstream.Commit)

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "LANGUAGE\tUPSTREAM FILE\tQUOTES\tINSERTED\tSUPERSEDED\tUNCHANGED")

	var total quote.ImportStats
	started := time.Now()
	matched := 0
	for _, lang := range manifest.Languages {
		if only != "" && lang.Lang != only {
			continue
		}
		matched++

		incoming, err := corpus.Load(core, lang)
		if err != nil {
			_ = w.Flush()
			return err
		}
		stats, err := store.Import(ctx, lang.Lang, incoming)
		if err != nil {
			_ = w.Flush()
			return err
		}
		total.Add(stats)

		fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%d\t%d\n", lang.Lang, lang.File,
			stats.Total(), stats.Inserted, stats.Superseded, stats.Unchanged)
	}
	fmt.Fprintf(w, "\tTOTAL\t%d\t%d\t%d\t%d\n",
		total.Total(), total.Inserted, total.Superseded, total.Unchanged)
	if err := w.Flush(); err != nil {
		return err
	}

	if matched == 0 {
		return fmt.Errorf("no manifest row named %q", only)
	}

	after, err := store.Count(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("\n  rows in registry  %d (was %d, retired revisions included)\n", after, before)
	fmt.Printf("  elapsed           %s\n\n", time.Since(started).Round(time.Millisecond))

	switch {
	case total.Inserted == 0 && total.Superseded == 0:
		fmt.Println("unchanged — the vendored corpora are already published verbatim")
	case total.Superseded > 0:
		fmt.Printf("%d quote(s) were republished under a new revision; the previous "+
			"revisions are retired but still resolvable by id, so runs played on "+
			"them stay replayable\n", total.Superseded)
	default:
		fmt.Printf("%d quote(s) published for the first time\n", total.Inserted)
	}
	return nil
}
