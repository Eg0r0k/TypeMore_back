// Command leaderboardctl operates the leaderboard projection against a live
// database.
//
//	leaderboardctl rebuild
//	    Truncates leaderboard_entries and recomputes every cell from accepted
//	    runs, in one transaction. The projection is maintained incrementally by
//	    the replay worker, so a rebuild should be a no-op — that it CAN be run,
//	    and that it changes nothing when it is, is the proof that the board is a
//	    projection of Postgres rather than a second source of truth.
//
//	leaderboardctl show [-bucket KEY] [-limit N]
//	    Prints the board index, or one bucket's ranking, exactly as a reader
//	    would see it (banned players filtered, same ordering as the API).
//
// Both read the same TYPEMORE_ environment as the server, so they see the same
// eligibility policy the server projects with. See docs/LEADERBOARDS.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/typemore/typemore-server/internal/leaderboard"
	leaderboardpg "github.com/typemore/typemore-server/internal/leaderboard/pgstore"
	"github.com/typemore/typemore-server/internal/platform"
	"github.com/typemore/typemore-server/internal/platform/db"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "leaderboardctl:", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 2 {
		return fmt.Errorf("usage: leaderboardctl <rebuild|show> [flags]")
	}
	command, args := os.Args[1], os.Args[2:]

	cfg, err := platform.LoadConfig()
	if err != nil {
		return err
	}

	ctx := context.Background()
	pool, err := db.NewPool(ctx, cfg.DatabaseURL, cfg.DBMaxConns)
	if err != nil {
		return err
	}
	defer pool.Close()

	store := leaderboardpg.New(pool, cfg.LeaderboardRequireVerifiedEmail)

	switch command {
	case "rebuild":
		fs := flag.NewFlagSet("rebuild", flag.ExitOnError)
		if err := fs.Parse(args); err != nil {
			return err
		}
		return rebuild(ctx, store, cfg)

	case "show":
		fs := flag.NewFlagSet("show", flag.ExitOnError)
		bucket := fs.String("bucket", "", "one bucket key, e.g. time:15000:en:seeded (default: the index)")
		limit := fs.Int("limit", 20, "rows per board")
		if err := fs.Parse(args); err != nil {
			return err
		}
		return show(ctx, store, *bucket, int32(*limit))

	default:
		return fmt.Errorf("unknown command %q (want rebuild or show)", command)
	}
}

func rebuild(ctx context.Context, store *leaderboardpg.Store, cfg platform.Config) error {
	fmt.Printf("rebuilding leaderboards (verified email required: %t)\n",
		cfg.LeaderboardRequireVerifiedEmail)

	started := time.Now()
	stats, err := store.Rebuild(ctx)
	if err != nil {
		return err
	}

	fmt.Printf("\n  eligible cells   %d\n", stats.Cells)
	fmt.Printf("  entries before   %d\n", stats.Before)
	fmt.Printf("  entries after    %d\n", stats.After)
	fmt.Printf("  elapsed          %s\n\n", time.Since(started).Round(time.Millisecond))

	switch {
	case stats.Before == stats.After:
		fmt.Println("unchanged — the incremental projection was already correct")
	case stats.Before < stats.After:
		fmt.Printf("recovered %d entr(y|ies) the incremental projection was missing\n", stats.After-stats.Before)
	default:
		fmt.Printf("dropped %d stale entr(y|ies)\n", stats.Before-stats.After)
	}
	return nil
}

func show(ctx context.Context, store *leaderboardpg.Store, bucketKey string, limit int32) error {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	defer func() { _ = w.Flush() }()

	if bucketKey == "" {
		buckets, err := store.Buckets(ctx)
		if err != nil {
			return err
		}
		if len(buckets) == 0 {
			fmt.Println("no populated buckets")
			return nil
		}
		fmt.Fprintln(w, "BUCKET\tENTRIES")
		for i := range buckets {
			fmt.Fprintf(w, "%s\t%d\n", buckets[i].Bucket.Key(), buckets[i].Entries)
		}
		return nil
	}

	bucket, err := leaderboard.ParseBucketKey(bucketKey)
	if err != nil {
		return err
	}
	rows, err := store.Page(ctx, bucket, nil, limit)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		fmt.Printf("%s is empty\n", bucket.Key())
		return nil
	}

	fmt.Fprintf(w, "%s\n\n", bucket.Key())
	fmt.Fprintln(w, "RANK\tPLAYER\tSCORE\tWPM\tACC\tGRADE\tACHIEVED")
	for i := range rows {
		r := &rows[i]
		fmt.Fprintf(w, "%d\t%s\t%d\t%.2f\t%.4f\t%s\t%s\n",
			i+1, r.DisplayName, r.Score, r.WPM, r.Acc, r.Grade,
			r.AchievedAt.UTC().Format(time.RFC3339))
	}
	return nil
}
