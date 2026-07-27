// Command banctl issues and revokes account bans against a live database.
//
//	banctl ban <user> --reason "..." [--until 72h | RFC3339] [--by NAME]
//	    Puts the account under restriction. Idempotent: re-running it on an
//	    already-banned account AMENDS the reason and expiry rather than stacking
//	    a second ban, and says which fields moved.
//
//	banctl unban <user>
//	    Revokes the active ban. The row is kept with a revoked_at, so the
//	    history survives.
//
//	banctl list [--active | --all] [--limit N]
//	    Bans newest first. --active is the default.
//
//	banctl show <user>
//	    Every ban this account has ever had, and whether it is restricted now.
//
// <user> is a display name, a uuid, or an email. Resolution tries uuid, then
// email, then display name — the unambiguous forms first — and refuses rather
// than guessing when a name matches more than one account.
//
// There is deliberately NO admin HTTP surface. A ban is issued by whoever has
// shell on the server, which keeps the blast radius of an authentication bug
// away from moderation entirely. See docs/MODERATION.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/user"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/typemore/typemore-server/internal/moderation"
	"github.com/typemore/typemore-server/internal/platform"
	"github.com/typemore/typemore-server/internal/platform/db"
)

func main() {
	if err := run(); err != nil {
		var ambiguous *moderation.ErrAmbiguousUser
		if errors.As(err, &ambiguous) {
			fmt.Fprintf(os.Stderr, "banctl: %v\n", err)
			for _, c := range ambiguous.Candidates {
				fmt.Fprintf(os.Stderr, "  %s  %s\n", c.ID, c.DisplayName)
			}
			fmt.Fprintln(os.Stderr, "re-run with the uuid of the account you mean")
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, "banctl:", err)
		os.Exit(1)
	}
}

const usage = "usage: banctl <ban|unban|list|show> [args]"

func run() error {
	if len(os.Args) < 2 {
		return errors.New(usage)
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
	store := moderation.New(pool)

	switch command {
	case "ban":
		return banCmd(ctx, store, args)
	case "unban":
		return unbanCmd(ctx, store, args)
	case "list":
		return listCmd(ctx, store, args)
	case "show":
		return showCmd(ctx, store, args)
	default:
		return fmt.Errorf("unknown command %q (%s)", command, usage)
	}
}

func banCmd(ctx context.Context, store *moderation.Store, args []string) error {
	fs := flag.NewFlagSet("ban", flag.ExitOnError)
	reason := fs.String("reason", "", "internal moderation note (required; never shown to the player)")
	until := fs.String("until", "", "when it lifts: a duration like 72h, or an RFC3339 instant. Omit for permanent")
	by := fs.String("by", "", "who issued it (default: the OS user)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: banctl ban <user> --reason \"...\" [--until 72h]")
	}
	if strings.TrimSpace(*reason) == "" {
		return errors.New("--reason is required: a ban with no note is one nobody can review later")
	}
	expiresAt, err := parseUntil(*until)
	if err != nil {
		return err
	}

	acct, err := store.ResolveUser(ctx, fs.Arg(0))
	if err != nil {
		return err
	}
	res, err := store.Ban(ctx, acct.ID, *reason, issuer(*by), expiresAt)
	if err != nil {
		return err
	}

	if !res.Amended {
		fmt.Printf("banned %s (%s)\n", acct.DisplayName, acct.ID)
		fmt.Printf("  reason   %s\n", res.Ban.Reason)
		fmt.Printf("  until    %s\n", untilText(res.Ban.ExpiresAt))
		fmt.Printf("  issuedBy %s\n", res.Ban.IssuedBy)
		return nil
	}

	// Already banned: print what CHANGED, not "ok". An operator who re-ran the
	// command by accident needs to see that nothing moved, and one who meant to
	// extend an expiry needs to see that it did.
	fmt.Printf("%s (%s) was already banned; amended the existing ban\n", acct.DisplayName, acct.ID)
	changed := false
	if res.Previous.Reason != res.Ban.Reason {
		fmt.Printf("  reason   %s -> %s\n", res.Previous.Reason, res.Ban.Reason)
		changed = true
	}
	if untilText(res.Previous.ExpiresAt) != untilText(res.Ban.ExpiresAt) {
		fmt.Printf("  until    %s -> %s\n", untilText(res.Previous.ExpiresAt), untilText(res.Ban.ExpiresAt))
		changed = true
	}
	if res.Previous.IssuedBy != res.Ban.IssuedBy {
		fmt.Printf("  issuedBy %s -> %s\n", res.Previous.IssuedBy, res.Ban.IssuedBy)
		changed = true
	}
	if !changed {
		fmt.Println("  nothing changed")
	}
	return nil
}

func unbanCmd(ctx context.Context, store *moderation.Store, args []string) error {
	fs := flag.NewFlagSet("unban", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: banctl unban <user>")
	}
	acct, err := store.ResolveUser(ctx, fs.Arg(0))
	if err != nil {
		return err
	}
	ban, err := store.Unban(ctx, acct.ID)
	if errors.Is(err, moderation.ErrNotBanned) {
		fmt.Printf("%s (%s) is not banned; nothing to do\n", acct.DisplayName, acct.ID)
		return nil
	}
	if err != nil {
		return err
	}
	fmt.Printf("unbanned %s (%s)\n", acct.DisplayName, acct.ID)
	fmt.Printf("  the ban issued %s is revoked; their leaderboard entries are visible again\n",
		ban.IssuedAt.UTC().Format(time.RFC3339))
	return nil
}

func listCmd(ctx context.Context, store *moderation.Store, args []string) error {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	all := fs.Bool("all", false, "include revoked and expired bans")
	active := fs.Bool("active", false, "only bans in force (the default)")
	limit := fs.Int("limit", 50, "rows")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *all && *active {
		return errors.New("--active and --all are opposites; pass at most one")
	}
	bans, err := store.List(ctx, !*all, int32(*limit))
	if err != nil {
		return err
	}
	if len(bans) == 0 {
		if *all {
			fmt.Println("no bans have ever been issued")
		} else {
			fmt.Println("no active bans")
		}
		return nil
	}
	printBans(bans)
	return nil
}

func showCmd(ctx context.Context, store *moderation.Store, args []string) error {
	fs := flag.NewFlagSet("show", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: banctl show <user>")
	}
	acct, err := store.ResolveUser(ctx, fs.Arg(0))
	if err != nil {
		return err
	}
	restricted, err := store.IsRestricted(ctx, acct.ID)
	if err != nil {
		return err
	}
	bans, err := store.History(ctx, acct.ID)
	if err != nil {
		return err
	}

	state := "not restricted"
	if restricted {
		state = "RESTRICTED"
	}
	fmt.Printf("%s (%s) — %s\n\n", acct.DisplayName, acct.ID, state)
	if len(bans) == 0 {
		fmt.Println("no bans on record")
		return nil
	}
	printBans(bans)
	return nil
}

func printBans(bans []moderation.Ban) {
	now := time.Now()
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	defer func() { _ = w.Flush() }()
	fmt.Fprintln(w, "STATE\tPLAYER\tISSUED\tUNTIL\tBY\tREASON")
	for i := range bans {
		b := &bans[i]
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			stateOf(*b, now), b.DisplayName,
			b.IssuedAt.UTC().Format(time.RFC3339), untilText(b.ExpiresAt),
			b.IssuedBy, b.Reason)
	}
}

func stateOf(b moderation.Ban, now time.Time) string {
	switch {
	case b.RevokedAt != nil:
		return "revoked"
	case b.ExpiresAt != nil && !b.ExpiresAt.After(now):
		return "expired"
	default:
		return "active"
	}
}

func untilText(t *time.Time) string {
	if t == nil {
		return "permanent"
	}
	return t.UTC().Format(time.RFC3339)
}

// parseUntil accepts a duration ("72h") or an absolute instant (RFC3339).
//
// Both spellings exist because both are natural: "72h" is what a moderator
// thinks, and an RFC3339 instant is what a script computes. An empty value is
// a permanent ban, which is why it is an explicit omission rather than a magic
// duration of zero.
func parseUntil(v string) (*time.Time, error) {
	if strings.TrimSpace(v) == "" {
		return nil, nil
	}
	if d, err := time.ParseDuration(v); err == nil {
		if d <= 0 {
			return nil, fmt.Errorf("--until %q is not in the future", v)
		}
		at := time.Now().UTC().Add(d)
		return &at, nil
	}
	at, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return nil, fmt.Errorf("--until %q is neither a duration (72h) nor an RFC3339 instant", v)
	}
	if !at.After(time.Now()) {
		return nil, fmt.Errorf("--until %q is not in the future", v)
	}
	at = at.UTC()
	return &at, nil
}

// issuer records WHO ran the command. It is free text and it is not
// authenticated — anyone who can run banctl already has shell on the box, so
// this is an audit note, not an access control.
func issuer(flagValue string) string {
	if s := strings.TrimSpace(flagValue); s != "" {
		return s
	}
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return "unknown"
}
