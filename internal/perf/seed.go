package perf

import (
	"context"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SeedSpec describes a synthetic population of players and runs.
//
// # Why these volumes
//
// The defaults below are deliberately past anything this game will see for
// years. A benchmark that passes at the volume you expect tells you the system
// works today; one that passes at ten times that tells you which wall you will
// hit and when. Concretely:
//
//   - 1 000 000 accepted runs is roughly a year of a busy typing site
//     (~2 700 runs/day sustained across all players).
//   - The HOT bucket holds 100 000+ distinct players. monkeytype's largest
//     English 60s board is of that order, and it is the single query most likely
//     to be linked publicly, so it is the one that must not degrade.
//   - 500 buckets is every ranked shape (6) across ~80 languages, which is more
//     languages than the dictionary registry will plausibly carry.
//
// The generator writes through COPY, so a million rows lands in seconds rather
// than minutes; a seed slow enough to discourage running the suite is a seed
// nobody runs.
type SeedSpec struct {
	// Users is how many accounts to create. Each holds runs across several
	// buckets, as a real player does.
	Users int
	// HotBucketEntries is how many DISTINCT players hold an entry in the single
	// hottest bucket — the /me rank curve's worst case.
	HotBucketEntries int
	// RunsPerHotUser is how many accepted runs each hot-bucket player has there.
	// More than one matters: the projection has to pick a best from a set, and a
	// player with one run never exercises that.
	RunsPerHotUser int
	// ColdBuckets is how many additional buckets to spread runs across.
	ColdBuckets int
	// EntriesPerColdBucket is the population of each of those.
	EntriesPerColdBucket int
	// UnverifiedFraction is the share of accounts without a verified email —
	// eligible to play, not eligible to rank (docs/LEADERBOARDS.md).
	UnverifiedFraction float64
	// BannedFraction is the share of accounts under an active ban: present in
	// the table, filtered from every read.
	BannedFraction float64
	// FlaggedFraction / RejectedFraction are the shares of runs that are NOT
	// accepted, so the eligible view has something to filter.
	FlaggedFraction  float64
	RejectedFraction float64
	// Seed makes the population reproducible.
	Seed uint64
}

// DefaultSeed is the zone 3/4 population: ~1M runs, a 100k-player hot bucket.
func DefaultSeed() SeedSpec {
	return SeedSpec{
		Users:                120_000,
		HotBucketEntries:     100_000,
		RunsPerHotUser:       6,
		ColdBuckets:          499,
		EntriesPerColdBucket: 700,
		UnverifiedFraction:   0.08,
		BannedFraction:       0.002,
		FlaggedFraction:      0.05,
		RejectedFraction:     0.01,
		Seed:                 0xC0FFEE,
	}
}

// SmallSeed is a proportionally identical population two orders of magnitude
// smaller, for the -short path and for the rank curve's 1k/10k points.
func SmallSeed() SeedSpec {
	s := DefaultSeed()
	s.Users = 1_500
	s.HotBucketEntries = 1_000
	s.RunsPerHotUser = 3
	s.ColdBuckets = 20
	s.EntriesPerColdBucket = 25
	return s
}

// Bucket names one board in the seeded population.
type Bucket struct {
	Mode       string
	DurationMs *int32
	WordCount  *int32
	Lang       string
}

// HotBucket is the bucket every "hot" run lands in: 60-second English, the shape
// a public leaderboard link points at.
func HotBucket() Bucket {
	d := int32(60_000)
	return Bucket{Mode: "time", DurationMs: &d, Lang: "en"}
}

// Key formats the bucket key. It MUST match leaderboard.Bucket.Key; the perf
// package deliberately does not import the leaderboard domain (the thing under
// measurement should not be a dependency of the measuring tape), so
// TestSeedBucketKeysMatchTheDomain pins the two together.
func (b Bucket) Key() string {
	dim := int32(0)
	if b.DurationMs != nil {
		dim = *b.DurationMs
	} else if b.WordCount != nil {
		dim = *b.WordCount
	}
	return fmt.Sprintf("%s:%d:%s:seeded", b.Mode, dim, b.Lang)
}

// SeedResult reports what a seed produced, so a benchmark can address the data
// it was given rather than rediscovering it.
type SeedResult struct {
	Hot           Bucket
	ColdBuckets   []Bucket
	Users         []uuid.UUID
	HotUsers      []uuid.UUID
	BannedUsers   []uuid.UUID
	UnverifiedIDs []uuid.UUID
	TotalRuns     int
	Elapsed       time.Duration
}

// rankedShapes are the (mode, dimension) pairs the eligible view admits.
var rankedShapes = []struct {
	mode string
	dim  int32
}{
	{"time", 15_000}, {"time", 30_000}, {"time", 60_000},
	{"words", 25}, {"words", 50}, {"words", 100},
}

var seedLangs = []string{
	"en", "ru-RU", "german", "code_css", "arabian", "chinese", "japanese",
	"russian_empire", "traditional_chinese", "es", "fr", "it", "pt", "pl",
	"nl", "sv", "no", "fi", "da", "cs", "tr", "uk", "el", "he", "hi", "ko",
}

// Seed populates users, identities, bans and runs, then leaves the leaderboard
// EMPTY — projecting it is the thing zone 4 measures. Call ProjectAll or the
// rebuild command afterwards for the zone 3 read benchmarks.
//
// Everything is written with COPY inside one transaction: a million single-row
// INSERTs would take longer than the benchmarks they feed.
func Seed(ctx context.Context, pool *pgxpool.Pool, spec SeedSpec) (SeedResult, error) {
	started := time.Now()
	rng := rand.New(rand.NewPCG(spec.Seed, 0xA11CE))

	res := SeedResult{Hot: HotBucket()}

	// --- users + identities ---
	users := make([]uuid.UUID, spec.Users)
	userRows := make([][]any, spec.Users)
	identRows := make([][]any, spec.Users)
	for i := range users {
		users[i] = uuid.New()
		name := fmt.Sprintf("perf%07d", i)
		verified := rng.Float64() >= spec.UnverifiedFraction
		userRows[i] = []any{users[i], name}
		identRows[i] = []any{uuid.New(), users[i], "email", name + "@perf.local", name + "@perf.local", verified}
		if !verified {
			res.UnverifiedIDs = append(res.UnverifiedIDs, users[i])
		}
	}
	res.Users = users

	if err := copyRows(ctx, pool, "users", []string{"id", "display_name"}, userRows); err != nil {
		return res, err
	}
	if err := copyRows(ctx, pool, "auth_identities",
		[]string{"id", "user_id", "provider", "provider_subject", "email", "email_verified"},
		identRows); err != nil {
		return res, err
	}

	// --- bans ---
	var banRows [][]any
	for _, u := range users {
		if rng.Float64() < spec.BannedFraction {
			banRows = append(banRows, []any{u, "perf fixture", nil})
			res.BannedUsers = append(res.BannedUsers, u)
		}
	}
	if len(banRows) > 0 {
		if err := copyRows(ctx, pool, "bans",
			[]string{"user_id", "reason", "expires_at"}, banRows); err != nil {
			return res, err
		}
	}

	// --- cold buckets ---
	res.ColdBuckets = make([]Bucket, 0, spec.ColdBuckets)
	for i := range spec.ColdBuckets {
		shape := rankedShapes[i%len(rankedShapes)]
		lang := seedLangs[(i/len(rankedShapes))%len(seedLangs)]
		if i >= len(rankedShapes)*len(seedLangs) {
			// Past the natural combinations, synthesise more languages so the
			// catalogue benchmark really sees ColdBuckets distinct boards.
			lang = fmt.Sprintf("lang%03d", i)
		}
		b := Bucket{Mode: shape.mode, Lang: lang}
		dim := shape.dim
		if shape.mode == "time" {
			b.DurationMs = &dim
		} else {
			b.WordCount = &dim
		}
		if b.Key() == res.Hot.Key() {
			continue // the hot bucket is seeded separately, at scale
		}
		res.ColdBuckets = append(res.ColdBuckets, b)
	}

	// --- runs ---
	runCols := []string{
		"id", "user_id", "mode", "duration_ms", "word_count", "lang", "seed",
		"dict_hash", "setup", "client_metrics", "client_score", "score_version",
		"status", "log", "log_bytes", "created_at",
		"server_metrics", "server_score", "validation", "bundle_sha",
		"policy_version", "validated_at",
	}

	setup := MustJSON(BuildSetup(SetupSpec{Mode: "time", DurationMs: 60_000}))
	clientMetrics := []byte(`{"wpm":100,"raw":100,"acc":1}`)
	clientScore := []byte(`{"version":2,"total":1000}`)
	validation := []byte(`{"verdict":"valid","flags":[],"policy":{"version":1,"suspicion":0,"threshold":1}}`)
	// A tiny but genuine gzip blob: zones 3 and 4 never decompress it, and a
	// realistic 8 KB blob per row would make the fixture 8 GB.
	log := Gzip([]byte(`{"version":1,"events":[{"kind":"insert","seq":1,"t":10,"text":"a"}]}`))

	base := time.Now().UTC().Add(-365 * 24 * time.Hour)
	var runRows [][]any
	flush := func() error {
		if len(runRows) == 0 {
			return nil
		}
		if err := copyRows(ctx, pool, "runs", runCols, runRows); err != nil {
			return err
		}
		res.TotalRuns += len(runRows)
		runRows = runRows[:0]
		return nil
	}

	addRun := func(user uuid.UUID, b Bucket, score int64, at time.Time) error {
		status := "accepted"
		switch r := rng.Float64(); {
		case r < spec.RejectedFraction:
			status = "rejected"
		case r < spec.RejectedFraction+spec.FlaggedFraction:
			status = "flagged"
		}
		acc := 0.85 + rng.Float64()*0.15
		wpm := 40 + rng.Float64()*120
		runRows = append(runRows, []any{
			uuid.New(), user, b.Mode, b.DurationMs, b.WordCount, b.Lang,
			int64(rng.Uint32()), "804728e8", setup, clientMetrics, clientScore,
			int16(2), status, log, int32(64), at,
			[]byte(fmt.Sprintf(`{"wpm":%.6f,"raw":%.6f,"accuracy":%.6f}`, wpm, wpm+1, acc)),
			[]byte(fmt.Sprintf(`{"version":2,"total":%d}`, score)),
			validation, "perfbundle", int16(1), at,
		})
		if len(runRows) >= 50_000 {
			return flush()
		}
		return nil
	}

	// Hot bucket: HotBucketEntries distinct players × RunsPerHotUser runs each.
	res.HotUsers = make([]uuid.UUID, 0, spec.HotBucketEntries)
	for i := range spec.HotBucketEntries {
		u := users[i%len(users)]
		res.HotUsers = append(res.HotUsers, u)
		for range spec.RunsPerHotUser {
			at := base.Add(time.Duration(rng.Int64N(365*24)) * time.Hour)
			if err := addRun(u, res.Hot, int64(rng.IntN(9000)+100), at); err != nil {
				return res, err
			}
		}
	}

	// Cold buckets.
	for _, b := range res.ColdBuckets {
		for range spec.EntriesPerColdBucket {
			u := users[rng.IntN(len(users))]
			at := base.Add(time.Duration(rng.Int64N(365*24)) * time.Hour)
			if err := addRun(u, b, int64(rng.IntN(9000)+100), at); err != nil {
				return res, err
			}
		}
	}
	if err := flush(); err != nil {
		return res, err
	}

	if _, err := pool.Exec(ctx, `ANALYZE users, auth_identities, bans, runs`); err != nil {
		return res, fmt.Errorf("perf: analyze: %w", err)
	}

	res.Elapsed = time.Since(started)
	return res, nil
}

// copyRows streams rows in with COPY.
func copyRows(ctx context.Context, pool *pgxpool.Pool, table string, cols []string, rows [][]any) error {
	_, err := pool.CopyFrom(ctx, pgx.Identifier{table}, cols, pgx.CopyFromRows(rows))
	if err != nil {
		return fmt.Errorf("perf: copy into %s: %w", table, err)
	}
	return nil
}

// Truncate empties every table the seeder writes, so a suite can re-seed.
func Truncate(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, `TRUNCATE leaderboard_entries, bans, runs, users CASCADE`)
	if err != nil {
		return fmt.Errorf("perf: truncate: %w", err)
	}
	return nil
}
