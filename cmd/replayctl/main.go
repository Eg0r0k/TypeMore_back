// Command replayctl operates the replay review policy against a live database.
//
//	replayctl calibrate [-limit N] [-top N]
//	    Re-validates stored runs through the current bundle and policy and prints
//	    what it FINDS — per-flag firing rates, a suspicion histogram, the worst
//	    offenders, and the status changes the current policy would make. It writes
//	    nothing. This is the command to run before touching a weight.
//
//	replayctl revalidate [-limit N] [-batch N]
//	    Re-judges runs whose policy_version is behind the current one and writes
//	    the result. Bounded by -limit, idempotent by construction: applying a
//	    decision sets policy_version, so a second pass finds nothing.
//
// Both read the same TYPEMORE_ environment as the server, so they judge with
// exactly the deployment's policy — weight overrides included. See
// docs/REPLAY.md, "Review policy".
package main

import (
	"cmp"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	leaderboardpg "github.com/typemore/typemore-server/internal/leaderboard/pgstore"
	"github.com/typemore/typemore-server/internal/platform"
	"github.com/typemore/typemore-server/internal/platform/db"
	"github.com/typemore/typemore-server/internal/replay"
	replaypg "github.com/typemore/typemore-server/internal/replay/pgstore"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "replayctl:", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 2 {
		return fmt.Errorf("usage: replayctl <calibrate|revalidate> [flags]")
	}
	command, args := os.Args[1], os.Args[2:]

	cfg, err := platform.LoadConfig()
	if err != nil {
		return err
	}
	// Resolved before anything touches the database: a typo in a weight
	// override must stop the tool, not silently judge with a default.
	policy, err := replay.DefaultPolicy().WithOverrides(
		cfg.ReplayFlagWeights, cfg.ReplayReviewThreshold, cfg.ReplaySustainedBurstSec)
	if err != nil {
		return err
	}

	ctx := context.Background()
	pool, err := db.NewPool(ctx, cfg.DatabaseURL, cfg.DBMaxConns)
	if err != nil {
		return err
	}
	defer pool.Close()

	switch command {
	case "calibrate":
		fs := flag.NewFlagSet("calibrate", flag.ExitOnError)
		limit := fs.Int("limit", 5000, "max runs to examine")
		top := fs.Int("top", 10, "how many worst offenders to detail")
		if err := fs.Parse(args); err != nil {
			return err
		}
		return calibrate(ctx, pool, policy, cfg, int32(*limit), *top)

	case "revalidate":
		fs := flag.NewFlagSet("revalidate", flag.ExitOnError)
		limit := fs.Int("limit", 5000, "max runs to re-judge in this pass")
		batch := fs.Int("batch", 50, "runs per transaction")
		if err := fs.Parse(args); err != nil {
			return err
		}
		return revalidate(ctx, pool, policy, cfg, *limit, int32(*batch))

	default:
		return fmt.Errorf("unknown command %q (want calibrate or revalidate)", command)
	}
}

// auditDoc is a read-only view of the `validation` jsonb the worker writes.
type auditDoc struct {
	Verdict string        `json:"verdict"`
	Reason  string        `json:"reason"`
	Flags   []replay.Flag `json:"flags"`
	Policy  *auditPolicy  `json:"policy"`
}

type auditPolicy struct {
	Version   int16    `json:"version"`
	Suspicion float64  `json:"suspicion"`
	Threshold float64  `json:"threshold"`
	Rules     []string `json:"rules"`
}

// --- calibrate ---------------------------------------------------------------

type outcome struct {
	run       replay.CalibrationRun
	status    string
	suspicion float64
	doc       auditDoc
}

func calibrate(ctx context.Context, pool *pgxpool.Pool, policy replay.Policy, cfg platform.Config, limit int32, top int) error {
	core, err := replay.NewCore(cfg.ReplayTimeout)
	if err != nil {
		return err
	}
	reg, err := replay.NewRegistry(core)
	if err != nil {
		return err
	}
	// nil projector: calibration judges runs to report what WOULD change and
	// must not touch a read model while doing it.
	rows, err := replaypg.New(pool, nil).ListForCalibration(ctx, limit)
	if err != nil {
		return err
	}

	printPolicy(policy)
	if len(rows) == 0 {
		fmt.Println("\nno judged runs to calibrate against")
		return nil
	}

	outcomes := make([]outcome, 0, len(rows))
	flagRuns := map[string]int{}
	flagSeverity := map[string][]float64{}
	ruleRuns := map[string]int{}
	transitions := map[string]int{}
	before := map[string]int{}
	after := map[string]int{}

	for i := range rows {
		// The exact path the worker takes — a report from a different code path
		// would be a fiction.
		d := replay.Judge(ctx, core, reg, policy, rows[i].PendingRun)

		var doc auditDoc
		_ = json.Unmarshal(d.Validation, &doc)
		for _, f := range doc.Flags {
			flagRuns[f.Code]++
			flagSeverity[f.Code] = append(flagSeverity[f.Code], f.Score)
		}
		var suspicion float64
		if doc.Policy != nil {
			suspicion = doc.Policy.Suspicion
			for _, r := range doc.Policy.Rules {
				ruleRuns[r]++
			}
		}
		before[rows[i].Status]++
		after[d.Status]++
		if rows[i].Status != d.Status {
			transitions[rows[i].Status+" -> "+d.Status]++
		}
		outcomes = append(outcomes, outcome{run: rows[i], status: d.Status, suspicion: suspicion, doc: doc})
	}

	fmt.Printf("\nexamined %d judged run(s), bundle %s\n", len(rows), replay.BundleSHA()[:12])

	fmt.Println("\nflag firing rate")
	fmt.Printf("  %-22s %5s %7s %8s %8s %8s %8s\n", "flag", "runs", "rate", "min sev", "mean", "max sev", "weight")
	codes := slices.Sorted(maps.Keys(flagRuns))
	sort.SliceStable(codes, func(i, j int) bool { return flagRuns[codes[i]] > flagRuns[codes[j]] })
	if len(codes) == 0 {
		fmt.Println("  (no flags raised)")
	}
	for _, code := range codes {
		sev := flagSeverity[code]
		lo, hi, sum := sev[0], sev[0], 0.0
		for _, v := range sev {
			lo, hi, sum = min(lo, v), max(hi, v), sum+v
		}
		mean := sum / float64(len(sev))
		fmt.Printf("  %-22s %5d %6.1f%% %8.4f %8.4f %8.4f %8.2f  (max contribution %.4f)\n",
			code, flagRuns[code], 100*float64(flagRuns[code])/float64(len(rows)),
			lo, mean, hi, policy.Weights[code], hi*policy.Weights[code])
	}

	fmt.Println("\ncombination rules fired")
	if len(ruleRuns) == 0 {
		fmt.Println("  (none)")
	}
	for _, r := range slices.Sorted(maps.Keys(ruleRuns)) {
		fmt.Printf("  %-24s %4d\n", r, ruleRuns[r])
	}

	fmt.Printf("\nsuspicion histogram (review threshold %.2f)\n", policy.ReviewThreshold)
	for _, b := range histogram(outcomes, policy.ReviewThreshold) {
		fmt.Printf("  %-22s %4d %s\n", b.label, b.count, strings.Repeat("#", b.count))
	}

	slices.SortFunc(outcomes, func(a, b outcome) int { return cmp.Compare(b.suspicion, a.suspicion) })
	shown := min(top, len(outcomes))
	fmt.Printf("\ntop %d by suspicion\n", shown)
	for i := range outcomes[:shown] {
		o := &outcomes[i]
		reason := o.doc.Reason
		if reason == "" {
			reason = "-"
		}
		fmt.Printf("  %s susp=%-8.4f %-8s -> %-8s %-20s %s\n",
			o.run.ID, o.suspicion, o.run.Status, o.status, reason, flagSummary(o.doc.Flags))
	}

	fmt.Println("\nstatus under this policy")
	for _, s := range []string{"accepted", "flagged", "rejected"} {
		fmt.Printf("  %-9s stored %3d  ->  policy %3d\n", s, before[s], after[s])
	}
	if len(transitions) == 0 {
		fmt.Println("\nnothing would change — every judged run already matches this policy")
	} else {
		fmt.Println("\ntransitions `make revalidate` would apply")
		for _, t := range slices.Sorted(maps.Keys(transitions)) {
			fmt.Printf("  %-26s %4d\n", t, transitions[t])
		}
	}
	fmt.Println("\nread-only: nothing was written")
	return nil
}

type bucket struct {
	label string
	count int
}

// histogram bins runs by suspicion. The bins are deliberately fine near zero:
// that is where the false-positive question lives.
func histogram(outcomes []outcome, threshold float64) []bucket {
	edges := []struct {
		label string
		hi    float64
	}{
		{"0 (no flags)", 0},
		{"(0, 0.01]", 0.01},
		{"(0.01, 0.05]", 0.05},
		{"(0.05, 0.25]", 0.25},
		{"(0.25, 0.50]", 0.50},
		{"(0.50, threshold)", threshold},
	}
	out := make([]bucket, 0, len(edges)+1)
	var lo float64
	for i, e := range edges {
		n := 0
		for j := range outcomes {
			o := &outcomes[j]
			switch {
			case i == 0:
				if o.suspicion == 0 {
					n++
				}
			case o.suspicion > lo && o.suspicion <= e.hi && o.suspicion < threshold:
				n++
			}
		}
		out = append(out, bucket{label: e.label, count: n})
		lo = e.hi
	}
	n := 0
	for i := range outcomes {
		if outcomes[i].suspicion >= threshold {
			n++
		}
	}
	return append(out, bucket{label: ">= threshold", count: n})
}

func flagSummary(flags []replay.Flag) string {
	if len(flags) == 0 {
		return "-"
	}
	parts := make([]string, 0, len(flags))
	for _, f := range flags {
		parts = append(parts, fmt.Sprintf("%s=%.3f", f.Code, f.Score))
	}
	return strings.Join(parts, " ")
}

// --- revalidate --------------------------------------------------------------

func revalidate(ctx context.Context, pool *pgxpool.Pool, policy replay.Policy, cfg platform.Config, limit int, batch int32) error {
	core, err := replay.NewCore(cfg.ReplayTimeout)
	if err != nil {
		return err
	}
	reg, err := replay.NewRegistry(core)
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	// Revalidation moves real verdicts, so it carries the same leaderboard
	// projector the server does: a run demoted here leaves its board in the same
	// transaction (docs/LEADERBOARDS.md, "Maintenance").
	board := leaderboardpg.New(pool, cfg.LeaderboardRequireVerifiedEmail)
	worker := replay.NewWorker(replaypg.New(pool, board), reg, replay.WorkerConfig{
		BatchSize:     batch,
		ReplayTimeout: cfg.ReplayTimeout,
		Policy:        policy,
	}, logger)

	printPolicy(policy)
	fmt.Printf("\nre-judging runs with policy_version < %d (limit %d, batch %d)\n\n",
		policy.Version, limit, batch)

	started := time.Now()
	total := 0
	for total < limit {
		n, err := worker.RevalidateBatch(ctx, core, logger)
		if err != nil {
			return err
		}
		if n == 0 {
			break
		}
		total += n
	}
	fmt.Printf("\nrevalidated %d run(s) in %s\n", total, time.Since(started).Round(time.Millisecond))
	if total == 0 {
		fmt.Println("nothing was stale: every judged run is already at the current policy")
	}
	return nil
}

func printPolicy(p replay.Policy) {
	fmt.Printf("policy v%d   review threshold %.2f   sustained-burst floor %.0fs   bundle %s\n",
		p.Version, p.ReviewThreshold, p.SustainedBurstSec, replay.BundleSHA()[:12])
	parts := make([]string, 0, len(p.Weights))
	for _, code := range slices.Sorted(maps.Keys(p.Weights)) {
		parts = append(parts, fmt.Sprintf("%s=%.2f", code, p.Weights[code]))
	}
	fmt.Println("weights: " + strings.Join(parts, "  "))
}
