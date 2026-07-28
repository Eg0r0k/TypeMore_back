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

// ProfileSeedSpec describes the profile zone's population: ONE account with a
// deep history — the 100k-run user the profile aggregates must stay fast for —
// laid over whatever background population is already in the database (the
// background is what makes "no seq scan of runs" a real assertion rather than
// a tautology on a single-user table).
type ProfileSeedSpec struct {
	// Runs is the profile user's total submissions.
	Runs int
	// Days is the history depth: created_at is spread across this many days
	// ending now, so the calendar, the streaks and the range filters all have
	// real shape to chew on.
	Days int
	// FlaggedFraction / RejectedFraction mirror the population-wide seed: the
	// metric aggregates must FILTER these out, so the fixture must contain them.
	FlaggedFraction  float64
	RejectedFraction float64
	// PBBuckets is how many leaderboard_entries rows the user holds — the PB
	// cards read. Bounded by the ranked shapes × languages, like reality.
	PBBuckets int
	// Seed makes the population reproducible.
	Seed uint64
}

// DefaultProfileSeed is the profile-zone population: a 100k-run account, two
// years deep — an order of magnitude past any real typist (100k runs is ~140
// runs a day, every day, for two years).
func DefaultProfileSeed() ProfileSeedSpec {
	return ProfileSeedSpec{
		Runs:             100_000,
		Days:             730,
		FlaggedFraction:  0.05,
		RejectedFraction: 0.01,
		PBBuckets:        36,
		Seed:             0xFACADE,
	}
}

// ProfileSeedResult reports what SeedProfileUser produced.
type ProfileSeedResult struct {
	UserID    uuid.UUID
	TotalRuns int
	Accepted  int
	Restarts  int64
	Elapsed   time.Duration
}

// SeedProfileUser writes one user with spec.Runs submissions carrying FULL
// server metric documents (durationSec, consistency, chars, spaces — the
// fields the profile aggregates actually read; the population-wide Seed writes
// only wpm/raw/accuracy because the board projection reads nothing else), plus
// the user's leaderboard_entries rows for the PB cards. COPY throughout,
// ANALYZE at the end.
func SeedProfileUser(ctx context.Context, pool *pgxpool.Pool, spec ProfileSeedSpec) (ProfileSeedResult, error) {
	started := time.Now()
	rng := rand.New(rand.NewPCG(spec.Seed, 0xD00D))
	res := ProfileSeedResult{UserID: uuid.New()}

	name := "profileperf"
	if err := copyRows(ctx, pool, "users", []string{"id", "display_name"},
		[][]any{{res.UserID, name}}); err != nil {
		return res, err
	}
	if err := copyRows(ctx, pool, "auth_identities",
		[]string{"id", "user_id", "provider", "provider_subject", "email", "email_verified"},
		[][]any{{uuid.New(), res.UserID, "email", name + "@perf.local", name + "@perf.local", true}}); err != nil {
		return res, err
	}

	runCols := []string{
		"id", "user_id", "mode", "duration_ms", "word_count", "lang", "seed",
		"dict_hash", "setup", "client_metrics", "client_score", "score_version",
		"status", "log", "log_bytes", "created_at",
		"server_metrics", "server_score", "validation", "bundle_sha",
		"policy_version", "validated_at", "restarts_since_last_submit",
	}
	setup := MustJSON(BuildSetup(SetupSpec{Mode: "time", DurationMs: 60_000}))
	clientMetrics := []byte(`{"wpm":100,"raw":100,"acc":1}`)
	clientScore := []byte(`{"version":2,"total":1000}`)
	validation := []byte(`{"verdict":"valid","flags":[],"policy":{"version":1,"suspicion":0,"threshold":1}}`)
	log := Gzip([]byte(`{"version":1,"events":[{"kind":"insert","seq":1,"t":10,"text":"a"}]}`))

	base := time.Now().UTC().Add(-time.Duration(spec.Days) * 24 * time.Hour)
	var rows [][]any
	flush := func() error {
		if len(rows) == 0 {
			return nil
		}
		if err := copyRows(ctx, pool, "runs", runCols, rows); err != nil {
			return err
		}
		res.TotalRuns += len(rows)
		rows = rows[:0]
		return nil
	}

	langs := []string{"en", "ru-RU", "german", "code_css"}
	for i := 0; i < spec.Runs; i++ {
		status := "accepted"
		switch r := rng.Float64(); {
		case r < spec.RejectedFraction:
			status = "rejected"
		case r < spec.RejectedFraction+spec.FlaggedFraction:
			status = "flagged"
		}
		shape := rankedShapes[rng.IntN(len(rankedShapes))]
		var durationMs, wordCount *int32
		dim := shape.dim
		if shape.mode == "time" {
			durationMs = &dim
		} else {
			wordCount = &dim
		}
		at := base.Add(time.Duration(rng.Int64N(int64(spec.Days) * 24 * int64(time.Hour))))
		restarts := int32(rng.IntN(6))
		res.Restarts += int64(restarts)

		var serverMetrics, serverScore []byte
		if status != "rejected" {
			wpm := 40 + rng.Float64()*120
			acc := 0.85 + rng.Float64()*0.15
			durationSec := 15 + rng.Float64()*105
			correct := int(wpm * 5 * durationSec / 60)
			serverMetrics = []byte(fmt.Sprintf(
				`{"wpm":%.6f,"raw":%.6f,"accuracy":%.6f,"consistency":%.6f,`+
					`"chars":{"correct":%d,"incorrect":%d,"extra":%d,"missed":%d},`+
					`"spaces":%d,"durationSec":%.3f}`,
				wpm, wpm+2, acc, 0.3+rng.Float64()*0.65,
				correct, rng.IntN(20), rng.IntN(6), rng.IntN(6),
				correct/6, durationSec))
			serverScore = []byte(fmt.Sprintf(`{"version":2,"total":%d}`, rng.IntN(9000)+100))
		}
		if status == "accepted" {
			res.Accepted++
		}
		rows = append(rows, []any{
			uuid.New(), res.UserID, shape.mode, durationMs, wordCount,
			langs[rng.IntN(len(langs))], int64(rng.Uint32()), "804728e8",
			setup, clientMetrics, clientScore, int16(2), status, log, int32(64),
			at, serverMetrics, serverScore, validation, "perfbundle", int16(1),
			at, restarts,
		})
		if len(rows) >= 50_000 {
			if err := flush(); err != nil {
				return res, err
			}
		}
	}
	if err := flush(); err != nil {
		return res, err
	}

	// The PB cards: one entries row per bucket, exactly the snapshot shape the
	// projection writes. Direct COPY — projecting 100k synthetic runs is zone
	// 4's business, not this fixture's.
	var pbRows [][]any
	for i := 0; i < spec.PBBuckets; i++ {
		shape := rankedShapes[i%len(rankedShapes)]
		lang := seedLangs[(i/len(rankedShapes))%len(seedLangs)]
		b := Bucket{Mode: shape.mode, Lang: lang}
		dim := shape.dim
		if shape.mode == "time" {
			b.DurationMs = &dim
		} else {
			b.WordCount = &dim
		}
		pbRows = append(pbRows, []any{
			b.Key(), res.UserID, uuid.New(), int64(rng.IntN(9000) + 100),
			40 + rng.Float64()*120, 42 + rng.Float64()*120, 0.9 + rng.Float64()*0.1,
			"S", []byte(`{"punctuation":false}`), base.Add(time.Duration(i) * 24 * time.Hour),
		})
	}
	// The entries FK references runs(id); the fixture's PB run ids are
	// synthetic, so plant matching skeleton runs first.
	var pbRunRows [][]any
	for _, row := range pbRows {
		d := int32(60_000)
		// An accepted run always carries its server metrics in production, so
		// the skeletons do too — an accepted row with NULL metrics is a shape
		// the aggregates are entitled never to see.
		m := []byte(fmt.Sprintf(
			`{"wpm":%.4f,"raw":%.4f,"accuracy":0.97,"consistency":0.8,`+
				`"chars":{"correct":300,"incorrect":6,"extra":2,"missed":1},`+
				`"spaces":58,"durationSec":60}`,
			row[4], row[5]))
		sc := []byte(fmt.Sprintf(`{"version":2,"total":%d}`, row[3]))
		pbRunRows = append(pbRunRows, []any{
			row[2], res.UserID, "time", &d, nil, "en", int64(rng.Uint32()),
			"804728e8", setup, clientMetrics, clientScore, int16(2), "accepted",
			log, int32(64), row[9], m, sc, validation, "perfbundle", int16(1),
			row[9], int32(0),
		})
	}
	if err := copyRows(ctx, pool, "runs", runCols, pbRunRows); err != nil {
		return res, err
	}
	res.TotalRuns += len(pbRunRows)
	if err := copyRows(ctx, pool, "leaderboard_entries",
		[]string{"bucket_key", "user_id", "run_id", "score", "wpm", "raw", "acc",
			"grade", "mods", "achieved_at"}, pbRows); err != nil {
		return res, err
	}

	if _, err := pool.Exec(ctx, `ANALYZE users, auth_identities, runs, leaderboard_entries`); err != nil {
		return res, fmt.Errorf("perf: analyze: %w", err)
	}
	res.Elapsed = time.Since(started)
	return res, nil
}
