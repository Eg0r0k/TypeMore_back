package leaderboard_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/typemore/typemore-server/internal/leaderboard"
	leaderboardpg "github.com/typemore/typemore-server/internal/leaderboard/pgstore"
	"github.com/typemore/typemore-server/internal/platform/db"
	"github.com/typemore/typemore-server/internal/platform/migrate"
)

// The Postgres testcontainer is started lazily on first use and torn down in
// TestMain, mirroring the auth and runs suites.
var (
	dbOnce      sync.Once
	dbContainer *postgres.PostgresContainer
	testDSN     string
	dbErr       error
)

func ensureDB(t *testing.T) string {
	t.Helper()
	dbOnce.Do(func() {
		ctx := context.Background()
		dbContainer, dbErr = postgres.Run(ctx, "postgres:17",
			postgres.WithDatabase("typemore"),
			postgres.WithUsername("typemore"),
			postgres.WithPassword("typemore"),
			testcontainers.WithWaitStrategy(
				wait.ForLog("database system is ready to accept connections").
					WithOccurrence(2).
					WithStartupTimeout(90*time.Second),
			),
		)
		if dbErr != nil {
			return
		}
		testDSN, dbErr = dbContainer.ConnectionString(ctx, "sslmode=disable")
		if dbErr != nil {
			return
		}
		dbErr = migrate.Up(ctx, testDSN)
	})
	require.NoError(t, dbErr, "start/migrate postgres testcontainer")
	return testDSN
}

func TestMain(m *testing.M) {
	code := m.Run()
	if dbContainer != nil {
		_ = dbContainer.Terminate(context.Background())
	}
	os.Exit(code)
}

// board is the leaderboard test harness: a real Postgres, the real projection,
// and the real HTTP surface.
//
// Runs are inserted directly rather than played through goja — the replay path
// has its own suite, and what is under test here is what happens to a board when
// a run's STATUS moves. The one place the whole chain is exercised end to end is
// internal/runs (TestAcceptedRunAppearsOnItsLeaderboard).
type board struct {
	t      *testing.T
	pool   *pgxpool.Pool
	store  *leaderboardpg.Store
	server *httptest.Server
	// asUser is the identity the /me route sees. uuid.Nil means anonymous.
	asUser uuid.UUID
}

type boardOpts struct {
	requireVerifiedEmail bool
}

func newBoard(t *testing.T, mutators ...func(*boardOpts)) *board {
	t.Helper()
	ctx := context.Background()

	opts := boardOpts{requireVerifiedEmail: false}
	for _, m := range mutators {
		m(&opts)
	}

	pool, err := db.NewPool(ctx, ensureDB(t), 5)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	_, err = pool.Exec(ctx, `TRUNCATE leaderboard_entries, bans, runs, users, quotes CASCADE`)
	require.NoError(t, err)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	store := leaderboardpg.New(pool, opts.requireVerifiedEmail)

	b := &board{t: t, pool: pool, store: store}
	svc := leaderboard.NewService(store, func(context.Context) (uuid.UUID, bool) {
		if b.asUser == uuid.Nil {
			return uuid.Nil, false
		}
		return b.asUser, true
	}, logger)

	r := chi.NewRouter()
	r.Route("/api/v1", func(r chi.Router) {
		r.Mount("/leaderboards", svc.Routes())
	})
	b.server = httptest.NewServer(r)
	t.Cleanup(b.server.Close)
	return b
}

// --- fixtures ---

// defaultSetup is a plain seeded run's setup snapshot: the shape today's client
// sends, with no mods and no textSource field at all.
const defaultSetup = `{
  "config":      {"mode":"time","durationMs":15000,"maxExtraChars":20,"difficulty":"normal","nospace":false,"minWpm":0},
  "generation":  {"mode":"time","length":0,"punctuation":false,"numbers":false,"randomCase":false,"reverse":false},
  "declaration": {"blind":false,"fading":false,"flashlight":false}
}`

// quoteSetupFor is a QUOTE run's setup snapshot, in the shape the client
// actually submits (TypeMore_front buildRunPayload): the textSource names the
// quote by id and hash and deliberately carries NO text, because the server
// re-resolves the bytes itself and never trusts the client's copy
// (docs/QUOTES.md).
func quoteSetupFor(quoteID uuid.UUID, quoteHash string) string {
	return fmt.Sprintf(`{
  "config":      {"mode":"quote","maxExtraChars":20,"difficulty":"normal","nospace":false,"minWpm":0},
  "generation":  {"mode":"quote","length":0,"punctuation":false,"numbers":false,"randomCase":false,"reverse":false,
                  "textSource":{"kind":"quote","quoteId":%q,"quoteHash":%q}},
  "declaration": {"blind":false,"fading":false,"flashlight":false}
}`, quoteID, quoteHash)
}

// quote plants one published quote and returns its id. The text is irrelevant
// to a board — what the projection needs from the registry is the id it is
// keyed by and the `source` it must attribute.
func (b *board) quote(lang, text, source string) uuid.UUID {
	b.t.Helper()
	var id uuid.UUID
	err := b.pool.QueryRow(context.Background(), `
		INSERT INTO quotes (id, lang, upstream_id, text, source, length, len_group, text_hash)
		VALUES (gen_random_uuid(), $1,
		        (SELECT coalesce(max(upstream_id), 0) + 1 FROM quotes WHERE lang = $1),
		        $2, $3, char_length($2), 1, 'deadbeef')
		RETURNING id`, lang, text, source).Scan(&id)
	require.NoError(b.t, err, "insert quote")
	return id
}

func (b *board) user(name string, verifiedEmail bool) uuid.UUID {
	b.t.Helper()
	ctx := context.Background()

	var id uuid.UUID
	err := b.pool.QueryRow(ctx,
		`INSERT INTO users (display_name) VALUES ($1) RETURNING id`, name).Scan(&id)
	require.NoError(b.t, err, "create user %q", name)

	email := fmt.Sprintf("%s@example.com", name)
	_, err = b.pool.Exec(ctx,
		`INSERT INTO auth_identities (user_id, provider, provider_subject, email, email_verified)
		 VALUES ($1, 'email', $2, $3::citext, $4)`,
		id, email, email, verifiedEmail)
	require.NoError(b.t, err, "create identity for %q", name)
	return id
}

// runSpec describes one run to plant. Zero fields take sensible defaults: a
// 15-second English time run, accepted, played "now".
type runSpec struct {
	user       uuid.UUID
	mode       string
	durationMs *int32
	wordCount  *int32
	lang       string
	status     string
	score      int64
	wpm        float64
	raw        float64
	acc        float64
	achievedAt time.Time
	setup      string
	// quote, when set, makes this a QUOTE run: mode 'quote', no duration and no
	// word count (the dimension rule 00008 enforces for them), and a setup whose
	// textSource names that quote.
	quote uuid.UUID
	// flags is written into validation.flags — an accepted run may carry them
	// (docs/REPLAY.md, "Review policy") and that must not affect eligibility.
	flags []string
}

func (s *runSpec) withDefaults() {
	if s.quote != uuid.Nil {
		s.mode = "quote"
		s.durationMs, s.wordCount = nil, nil
		if s.setup == "" {
			s.setup = quoteSetupFor(s.quote, "deadbeef")
		}
	} else {
		if s.mode == "" {
			s.mode = leaderboard.ModeTime
		}
		if s.durationMs == nil && s.wordCount == nil {
			s.durationMs = new(int32(15000))
		}
	}
	if s.lang == "" {
		s.lang = "en"
	}
	if s.status == "" {
		s.status = "accepted"
	}
	if s.wpm == 0 {
		s.wpm = 100
	}
	if s.raw == 0 {
		s.raw = s.wpm
	}
	if s.acc == 0 {
		s.acc = 1
	}
	if s.achievedAt.IsZero() {
		s.achievedAt = time.Now().UTC()
	}
	if s.setup == "" {
		s.setup = defaultSetup
	}
}

// addRun plants a run and drives it to its status exactly the way production
// does: it lands 'pending', then one status write + projection in ONE
// transaction moves it. A spec asking for 'pending' is simply never judged.
func (b *board) addRun(spec runSpec) uuid.UUID {
	b.t.Helper()
	spec.withDefaults()

	flags, err := json.Marshal(spec.flags)
	require.NoError(b.t, err)
	validation := fmt.Sprintf(`{"verdict":"valid","flags":%s,"policy":{"version":1,"suspicion":0,"threshold":1}}`, flags)

	var id uuid.UUID
	err = b.pool.QueryRow(context.Background(), `
		INSERT INTO runs (user_id, mode, duration_ms, word_count, lang, seed, dict_hash,
		                  setup, client_metrics, client_score, score_version,
		                  log, log_bytes, created_at,
		                  server_metrics, server_score, validation, bundle_sha,
		                  policy_version, validated_at)
		VALUES ($1, $2, $3, $4, $5, 1, 'testhash',
		        $6::jsonb, '{}'::jsonb, '{}'::jsonb, 2,
		        '\x1f8b'::bytea, 0, $7,
		        jsonb_build_object('wpm', $8::float8, 'raw', $9::float8, 'accuracy', $10::float8),
		        jsonb_build_object('version', 2, 'total', $11::bigint),
		        $12::jsonb, 'testbundle', 1, now())
		RETURNING id`,
		spec.user, spec.mode, spec.durationMs, spec.wordCount, spec.lang,
		spec.setup, spec.achievedAt,
		spec.wpm, spec.raw, spec.acc, spec.score, validation,
	).Scan(&id)
	require.NoError(b.t, err, "insert run")

	if spec.status != "pending" {
		b.judge(id, spec.status)
	}
	return id
}

// judge writes a run's status and projects it in one transaction — the same
// sequence internal/replay/pgstore performs around a real verdict, minus the
// goja replay that produced it.
func (b *board) judge(runID uuid.UUID, status string) {
	b.t.Helper()
	ctx := context.Background()

	tx, err := b.pool.Begin(ctx)
	require.NoError(b.t, err)
	defer func() { _ = tx.Rollback(ctx) }()

	_, err = tx.Exec(ctx, `UPDATE runs SET status = $1, validated_at = now() WHERE id = $2`, status, runID)
	require.NoError(b.t, err)
	require.NoError(b.t, b.store.ProjectRun(ctx, tx, runID))
	require.NoError(b.t, tx.Commit(ctx))
}

func (b *board) ban(userID uuid.UUID, expiresAt *time.Time) {
	b.t.Helper()
	_, err := b.pool.Exec(context.Background(),
		`INSERT INTO bans (user_id, reason, expires_at) VALUES ($1, 'testing', $2)`, userID, expiresAt)
	require.NoError(b.t, err)
}

func (b *board) unban(userID uuid.UUID) {
	b.t.Helper()
	_, err := b.pool.Exec(context.Background(), `DELETE FROM bans WHERE user_id = $1`, userID)
	require.NoError(b.t, err)
}

// --- assertions over the raw table (as opposed to the ban-filtered view) ---

// storedEntry is one row of leaderboard_entries, read WITHOUT the visibility
// view — the only way to tell "hidden from readers" from "deleted".
type storedEntry struct {
	BucketKey string
	UserID    uuid.UUID
	RunID     uuid.UUID
	Score     int64
	Grade     string
	Mods      json.RawMessage
	// QuoteSource is the snapshotted attribution: the quote's `source` on a
	// quote board, empty on a language board.
	QuoteSource string
	AchievedAt  time.Time
}

// storedColumns is spelled once so the two readers below cannot drift into
// disagreeing about what an entry is.
const storedColumns = `bucket_key, user_id, run_id, score, grade, mods,
                        coalesce(quote_source, ''), achieved_at`

func (b *board) storedEntries() []storedEntry {
	b.t.Helper()
	rows, err := b.pool.Query(context.Background(), `
		SELECT `+storedColumns+`
		FROM leaderboard_entries
		ORDER BY bucket_key, user_id`)
	require.NoError(b.t, err)
	defer rows.Close()

	var out []storedEntry
	for rows.Next() {
		var e storedEntry
		require.NoError(b.t, rows.Scan(&e.BucketKey, &e.UserID, &e.RunID, &e.Score,
			&e.Grade, &e.Mods, &e.QuoteSource, &e.AchievedAt))
		out = append(out, e)
	}
	require.NoError(b.t, rows.Err())
	return out
}

func (b *board) storedEntry(bucket leaderboard.Bucket, userID uuid.UUID) (storedEntry, bool) {
	b.t.Helper()
	var e storedEntry
	err := b.pool.QueryRow(context.Background(), `
		SELECT `+storedColumns+`
		FROM leaderboard_entries WHERE bucket_key = $1 AND user_id = $2`,
		bucket.Key(), userID).Scan(&e.BucketKey, &e.UserID, &e.RunID, &e.Score,
		&e.Grade, &e.Mods, &e.QuoteSource, &e.AchievedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return storedEntry{}, false
	}
	require.NoError(b.t, err)
	return e, true
}

// --- HTTP helpers ---

func (b *board) get(path string) *http.Response {
	b.t.Helper()
	resp, err := b.server.Client().Get(b.server.URL + path)
	require.NoError(b.t, err)
	b.t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func decodeInto[T any](t *testing.T, resp *http.Response) T {
	t.Helper()
	var v T
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&v))
	return v
}

// --- shared fixtures ---

// bucket15s is the bucket every default run lands in.
func bucket15s(t *testing.T) leaderboard.Bucket {
	t.Helper()
	b, err := leaderboard.NewBucket(leaderboard.ModeTime, new(int32(15000)), nil, "en", leaderboard.TextSourceSeeded)
	require.NoError(t, err)
	return b
}

// quoteBucket is the board one quote ranks on.
func quoteBucket(t *testing.T, quoteID uuid.UUID) leaderboard.Bucket {
	t.Helper()
	b, err := leaderboard.NewQuoteBucket(quoteID)
	require.NoError(t, err)
	return b
}

// minutesAgo is a stable, ordered clock for achievement times.
func minutesAgo(n int) time.Time {
	return time.Now().UTC().Add(-time.Duration(n) * time.Minute).Truncate(time.Microsecond)
}

// newStoreOn returns a second store over the same database with a different
// eligibility policy — the "someone changed the config and ran a rebuild" case.
func newStoreOn(t *testing.T, b *board, requireVerifiedEmail bool) *leaderboardpg.Store {
	t.Helper()
	return leaderboardpg.New(b.pool, requireVerifiedEmail)
}

func readBody(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return body
}
