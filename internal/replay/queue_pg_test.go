package replay_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/typemore/typemore-server/internal/platform/db"
	"github.com/typemore/typemore-server/internal/platform/migrate"
	"github.com/typemore/typemore-server/internal/replay"
	replaypg "github.com/typemore/typemore-server/internal/replay/pgstore"
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

func newPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := db.NewPool(context.Background(), ensureDB(t), 12)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	// Every test owns the whole queue, so start from an empty one.
	_, err = pool.Exec(context.Background(), `TRUNCATE runs CASCADE`)
	require.NoError(t, err)
	return pool
}

// --- golden-vector payloads (shared with the in-package tests) --------------

type vectorFile struct {
	Name string `json:"name"`
	// Expect is the verdict the vector was generated with — the same fields the
	// in-package suite asserts, so a test here judges against the vector's own
	// recorded expectation rather than a second copy of it.
	Expect struct {
		Status  string   `json:"status"`
		Verdict string   `json:"verdict"`
		Flags   []string `json:"flags"`
	} `json:"expect"`
	Payload struct {
		Mode          string          `json:"mode"`
		DurationMs    *int32          `json:"durationMs"`
		WordCount     *int32          `json:"wordCount"`
		Lang          string          `json:"lang"`
		Seed          int64           `json:"seed"`
		DictHash      string          `json:"dictHash"`
		ScoreVersion  int16           `json:"scoreVersion"`
		Setup         json.RawMessage `json:"setup"`
		ClientMetrics json.RawMessage `json:"clientMetrics"`
		ClientScore   json.RawMessage `json:"clientScore"`
		Log           json.RawMessage `json:"log"`
	} `json:"payload"`
}

func loadVector(t *testing.T, name string) vectorFile {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "vectors", name+".json"))
	require.NoError(t, err)
	var v vectorFile
	require.NoError(t, json.Unmarshal(raw, &v))
	return v
}

func gzipBytes(t *testing.T, raw []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, err := zw.Write(raw)
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	return buf.Bytes()
}

// seedUser inserts an account directly: these tests exercise the queue, not
// registration.
func seedUser(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := pool.QueryRow(context.Background(),
		`INSERT INTO users (display_name) VALUES ($1) RETURNING id`,
		fmt.Sprintf("queue_%s", uuid.NewString()[:8])).Scan(&id)
	require.NoError(t, err)
	return id
}

// insertPending lands one pending run built from a golden vector, exactly as
// the ingest path would.
func insertPending(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID, v vectorFile) uuid.UUID {
	t.Helper()
	return insertPendingLog(t, pool, userID, v, v.Payload.Log)
}

// insertPendingLog is insertPending with the event log substituted — for a
// fixture that varies the LOG (its wire version, its telemetry) while keeping
// the rest of the submission identical.
func insertPendingLog(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID, v vectorFile, log json.RawMessage) uuid.UUID {
	t.Helper()
	p := v.Payload
	var durationMs, wordCount *int32
	if p.Mode == "time" {
		var cfg struct {
			Config struct {
				DurationMs int32 `json:"durationMs"`
			} `json:"config"`
		}
		require.NoError(t, json.Unmarshal(p.Setup, &cfg))
		durationMs = &cfg.Config.DurationMs
	} else {
		var gen struct {
			Generation struct {
				Length int32 `json:"length"`
			} `json:"generation"`
		}
		require.NoError(t, json.Unmarshal(p.Setup, &gen))
		wordCount = &gen.Generation.Length
	}

	logJSON, err := json.Marshal(log)
	require.NoError(t, err)
	gz := gzipBytes(t, logJSON)

	var id uuid.UUID
	err = pool.QueryRow(context.Background(), `
		INSERT INTO runs (user_id, mode, duration_ms, word_count, lang, seed, dict_hash,
		                  setup, client_metrics, client_score, score_version, log, log_bytes)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13) RETURNING id`,
		userID, p.Mode, durationMs, wordCount, p.Lang, p.Seed, p.DictHash,
		p.Setup, p.ClientMetrics, p.ClientScore, p.ScoreVersion, gz, len(logJSON)).Scan(&id)
	require.NoError(t, err)
	return id
}

type runRow struct {
	Status        string
	ServerMetrics []byte
	ServerScore   []byte
	Validation    []byte
	BundleSha     *string
	PolicyVersion *int16
	ValidatedAt   *time.Time
	Attempts      int16
	LastError     *string
}

func fetchRun(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) runRow {
	t.Helper()
	var r runRow
	err := pool.QueryRow(context.Background(), `
		SELECT status, server_metrics, server_score, validation, bundle_sha,
		       policy_version, validated_at, attempts, last_error
		FROM runs WHERE id = $1`, id).
		Scan(&r.Status, &r.ServerMetrics, &r.ServerScore, &r.Validation, &r.BundleSha,
			&r.PolicyVersion, &r.ValidatedAt, &r.Attempts, &r.LastError)
	require.NoError(t, err)
	return r
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// pgQuotes is the quote registry the queue tests run against. Every fixture
// here is a SEEDED run, so the resolver is never reached — an empty map is the
// honest wiring, and a quote run appearing in this suite would be flagged
// unknown_quote rather than silently judged against nothing.
type pgQuotes struct{}

func (pgQuotes) ResolveQuote(context.Context, uuid.UUID) (string, string, bool, error) {
	return "", "", false, nil
}

func newTestWorker(t *testing.T, q replay.Queue, cfg replay.WorkerConfig) *replay.Worker {
	t.Helper()
	core, err := replay.NewCore(replay.DefaultReplayTimeout)
	require.NoError(t, err)
	reg, err := replay.NewRegistry(core)
	require.NoError(t, err)
	return replay.NewWorker(q, reg, pgQuotes{}, cfg, discardLogger())
}

// --- the queue --------------------------------------------------------------

// One pass over a real queue: the row leaves 'pending' with the server's numbers
// and the bundle that produced them recorded beside it.
func TestQueueProcessesAPendingRun(t *testing.T) {
	pool := newPool(t)
	user := seedUser(t, pool)
	v := loadVector(t, "words-clean")
	id := insertPending(t, pool, user, v)

	w := newTestWorker(t, replaypg.New(pool, nil), replay.WorkerConfig{BatchSize: 10})
	core, err := replay.NewCore(replay.DefaultReplayTimeout)
	require.NoError(t, err)
	n, err := w.RunBatch(context.Background(), core, discardLogger())
	require.NoError(t, err)
	require.Equal(t, 1, n)

	row := fetchRun(t, pool, id)
	assert.Equal(t, replay.StatusAccepted, row.Status, "validation: %s", row.Validation)
	require.NotNil(t, row.BundleSha)
	assert.Equal(t, replay.BundleSHA(), *row.BundleSha)
	require.NotNil(t, row.ValidatedAt)
	assert.Zero(t, row.Attempts)
	assert.Nil(t, row.LastError)

	// The server's numbers survived the jsonb round trip unchanged.
	var serverScore, clientScore map[string]any
	require.NoError(t, json.Unmarshal(row.ServerScore, &serverScore))
	require.NoError(t, json.Unmarshal(v.Payload.ClientScore, &clientScore))
	assert.Equal(t, clientScore["total"], serverScore["total"])

	// A judged run is never claimed again.
	n, err = w.RunBatch(context.Background(), core, discardLogger())
	require.NoError(t, err)
	assert.Zero(t, n)
}

// countingQueue wraps a real Queue and records which runs were handed to
// `decide`. Shared between the two workers, it is the direct evidence for
// "no run processed twice".
type countingQueue struct {
	inner replay.Queue
	mu    *sync.Mutex
	seen  map[uuid.UUID]int
}

func (q countingQueue) ProcessBatch(ctx context.Context, limit int32, decide func(context.Context, replay.PendingRun) replay.Decision) (int, error) {
	return q.inner.ProcessBatch(ctx, limit, func(ctx context.Context, run replay.PendingRun) replay.Decision {
		q.mu.Lock()
		q.seen[run.ID]++
		q.mu.Unlock()
		return decide(ctx, run)
	})
}

func (q countingQueue) ProcessStalePolicyBatch(ctx context.Context, policyVersion int16, bundleSHA string, limit int32, decide func(context.Context, replay.PendingRun) replay.Decision) (int, error) {
	return q.inner.ProcessStalePolicyBatch(ctx, policyVersion, bundleSHA, limit, func(ctx context.Context, run replay.PendingRun) replay.Decision {
		q.mu.Lock()
		q.seen[run.ID]++
		q.mu.Unlock()
		return decide(ctx, run)
	})
}

// Two workers, one queue, no coordination beyond FOR UPDATE SKIP LOCKED: every
// run must be judged exactly once. Run this under -race.
func TestTwoWorkersNeverProcessTheSameRunTwice(t *testing.T) {
	pool := newPool(t)
	user := seedUser(t, pool)

	vectors := []vectorFile{
		loadVector(t, "words-clean"),
		loadVector(t, "time-clean"),
		loadVector(t, "words-mods"),
		loadVector(t, "words-rejected-backspace"),
		loadVector(t, "words-typos-v1"),
	}
	const copies = 8
	ids := make([]uuid.UUID, 0, len(vectors)*copies)
	for range copies {
		for _, v := range vectors {
			ids = append(ids, insertPending(t, pool, user, v))
		}
	}

	var mu sync.Mutex
	seen := make(map[uuid.UUID]int, len(ids))
	// A small batch relative to the backlog guarantees the two workers really
	// interleave instead of one draining everything in a single claim.
	cfg := replay.WorkerConfig{BatchSize: 3}

	var wg sync.WaitGroup
	for range 2 {
		q := countingQueue{inner: replaypg.New(pool, nil), mu: &mu, seen: seen}
		w := newTestWorker(t, q, cfg)
		core, err := replay.NewCore(replay.DefaultReplayTimeout)
		require.NoError(t, err)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				n, err := w.RunBatch(context.Background(), core, discardLogger())
				if err != nil || n == 0 {
					assert.NoError(t, err)
					return
				}
			}
		}()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, seen, len(ids), "every run must have been claimed")
	for id, times := range seen {
		assert.Equal(t, 1, times, "run %s was processed %d times", id, times)
	}

	var pending int
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT count(*) FROM runs WHERE status = 'pending'`).Scan(&pending))
	assert.Zero(t, pending, "the queue must be drained")

	var judged int
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT count(*) FROM runs WHERE status = 'accepted' AND validated_at IS NOT NULL`).Scan(&judged))
	assert.Equal(t, len(ids), judged)
}

// A verdict is committed with the claim, so a worker shutting down mid-poll
// never loses the batch it already finished — and never re-does it.
func TestVerdictsCommitWithTheClaim(t *testing.T) {
	pool := newPool(t)
	user := seedUser(t, pool)
	for range 4 {
		insertPending(t, pool, user, loadVector(t, "words-clean"))
	}

	ctx, cancel := context.WithCancel(context.Background())
	w := newTestWorker(t, replaypg.New(pool, nil), replay.WorkerConfig{
		BatchSize:    2,
		PollInterval: 10 * time.Millisecond,
	})

	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	require.Eventually(t, func() bool {
		var pending int
		if err := pool.QueryRow(context.Background(),
			`SELECT count(*) FROM runs WHERE status = 'pending'`).Scan(&pending); err != nil {
			return false
		}
		return pending == 0
	}, 30*time.Second, 50*time.Millisecond, "the worker loop should drain the queue")

	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(30 * time.Second):
		t.Fatal("worker did not stop after cancellation")
	}

	var accepted int
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT count(*) FROM runs WHERE status = 'accepted'`).Scan(&accepted))
	assert.Equal(t, 4, accepted)
}

// --- revalidation -------------------------------------------------------------

// `make revalidate` walks runs judged under an older policy forward, and is
// idempotent: the second pass finds nothing because the first set
// policy_version.
func TestRevalidateIsBoundedAndIdempotent(t *testing.T) {
	pool := newPool(t)
	user := seedUser(t, pool)

	ids := []uuid.UUID{
		insertPending(t, pool, user, loadVector(t, "words-clean")),
		insertPending(t, pool, user, loadVector(t, "words-one-fast-interval")),
		insertPending(t, pool, user, loadVector(t, "words-bot-cadence")),
	}

	w := newTestWorker(t, replaypg.New(pool, nil), replay.WorkerConfig{BatchSize: 10})
	core, err := replay.NewCore(replay.DefaultReplayTimeout)
	require.NoError(t, err)

	// Judge them normally first.
	n, err := w.RunBatch(context.Background(), core, discardLogger())
	require.NoError(t, err)
	require.Equal(t, len(ids), n)

	// Freshly judged runs are already at the current policy, so there is
	// nothing stale to walk forward.
	n, err = w.RevalidateBatch(context.Background(), core, discardLogger())
	require.NoError(t, err)
	assert.Zero(t, n, "a just-judged run must not be re-judged")

	// Simulate rows judged by an older policy (and, for one of them, by no
	// policy at all — the pre-policy NULL).
	_, err = pool.Exec(context.Background(),
		`UPDATE runs SET policy_version = 0 WHERE id = ANY($1)`, ids[:2])
	require.NoError(t, err)
	_, err = pool.Exec(context.Background(),
		`UPDATE runs SET policy_version = NULL WHERE id = $1`, ids[2])
	require.NoError(t, err)

	n, err = w.RevalidateBatch(context.Background(), core, discardLogger())
	require.NoError(t, err)
	assert.Equal(t, len(ids), n, "every stale run should be claimed")

	// Idempotent: nothing left.
	n, err = w.RevalidateBatch(context.Background(), core, discardLogger())
	require.NoError(t, err)
	assert.Zero(t, n)

	for _, id := range ids {
		row := fetchRun(t, pool, id)
		require.NotNil(t, row.PolicyVersion, "run %s has no policy_version", id)
		assert.Equal(t, replay.CurrentPolicyVersion, *row.PolicyVersion)
	}
}

// The other half of the claim: a run judged by a DIFFERENT BUNDLE is stale even
// when its policy_version is already current.
//
// This is the case that used to be stranded. Re-vendoring the core changes the
// numbers, the client's fresh numbers stop matching the stored ones, the run
// comes back flagged metric_mismatch — and a policy-only claim would refuse to
// look at it forever, because the rules had not moved. Fourteen real runs sat
// in exactly that state (docs/PERFORMANCE.md, "the vendored bundle is stale").
func TestRevalidateClaimsRunsJudgedByAnotherBundle(t *testing.T) {
	pool := newPool(t)
	user := seedUser(t, pool)
	ctx := context.Background()

	ids := []uuid.UUID{
		insertPending(t, pool, user, loadVector(t, "words-clean")),
		insertPending(t, pool, user, loadVector(t, "time-clean")),
	}

	w := newTestWorker(t, replaypg.New(pool, nil), replay.WorkerConfig{BatchSize: 10})
	core, err := replay.NewCore(replay.DefaultReplayTimeout)
	require.NoError(t, err)

	n, err := w.RunBatch(ctx, core, discardLogger())
	require.NoError(t, err)
	require.Equal(t, len(ids), n)

	// Current policy, current bundle: nothing to do.
	n, err = w.RevalidateBatch(ctx, core, discardLogger())
	require.NoError(t, err)
	require.Zero(t, n)

	// One row judged by an older bundle, one by a bundle that was never
	// recorded at all — three-valued logic would skip the NULL under `<>`,
	// which is why the claim says IS DISTINCT FROM.
	_, err = pool.Exec(ctx, `UPDATE runs SET bundle_sha = 'deadbeef' WHERE id = $1`, ids[0])
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `UPDATE runs SET bundle_sha = NULL WHERE id = $1`, ids[1])
	require.NoError(t, err)

	// policy_version is deliberately left current on both, so the ONLY thing
	// that can claim them is the bundle arm.
	for _, id := range ids {
		row := fetchRun(t, pool, id)
		require.NotNil(t, row.PolicyVersion)
		require.Equal(t, replay.CurrentPolicyVersion, *row.PolicyVersion)
	}

	n, err = w.RevalidateBatch(ctx, core, discardLogger())
	require.NoError(t, err)
	assert.Equal(t, len(ids), n, "a run judged by another bundle is stale, policy or no policy")

	// Applying the decision writes bundle_sha as well as policy_version, so the
	// second pass is empty — the property that makes `make revalidate` safe to
	// run twice, now that it has two reasons to claim.
	n, err = w.RevalidateBatch(ctx, core, discardLogger())
	require.NoError(t, err)
	assert.Zero(t, n, "a re-judged run must stop matching BOTH arms of the claim")

	for _, id := range ids {
		row := fetchRun(t, pool, id)
		require.NotNil(t, row.BundleSha, "run %s has no bundle_sha", id)
		assert.Equal(t, replay.BundleSHA(), *row.BundleSha)
	}
}

// The backfill contract of the consistency/chars re-vendor: `make revalidate`
// walks history forward without moving a single verdict — the only change a
// backfilled row may show is its server_metrics carrying the enriched fields
// (consistency redefined onto [0, 1]; the chars breakdown). This test plants
// exactly the historical shape — runs judged by an older bundle whose stored
// consistency is the legacy per-word curve on [0, 100] — and asserts the
// verdict columns byte for byte while the metric document is corrected.
func TestRevalidateBackfillKeepsVerdictsAndEnrichesMetrics(t *testing.T) {
	pool := newPool(t)
	user := seedUser(t, pool)
	ctx := context.Background()

	ids := []uuid.UUID{
		insertPending(t, pool, user, loadVector(t, "words-clean")),
		insertPending(t, pool, user, loadVector(t, "words-consistency-chars")),
		// A flagged verdict that must survive the backfill unmoved too.
		insertPending(t, pool, user, loadVector(t, "words-bot-cadence")),
	}

	w := newTestWorker(t, replaypg.New(pool, nil), replay.WorkerConfig{BatchSize: 10})
	core, err := replay.NewCore(replay.DefaultReplayTimeout)
	require.NoError(t, err)
	n, err := w.RunBatch(ctx, core, discardLogger())
	require.NoError(t, err)
	require.Equal(t, len(ids), n)

	type charCounts struct {
		Correct   int `json:"correct"`
		Incorrect int `json:"incorrect"`
		Extra     int `json:"extra"`
		Missed    int `json:"missed"`
	}
	type metricsDoc struct {
		Wpm         float64     `json:"wpm"`
		Raw         float64     `json:"raw"`
		Accuracy    float64     `json:"accuracy"`
		Consistency *float64    `json:"consistency"`
		Chars       *charCounts `json:"chars"`
	}

	before := map[uuid.UUID]runRow{}
	for _, id := range ids {
		row := fetchRun(t, pool, id)
		before[id] = row
		// Rewrite the row into the historical shape: an older bundle's sha, and
		// the legacy consistency scale — so the backfill has something to fix.
		var m map[string]any
		require.NoError(t, json.Unmarshal(row.ServerMetrics, &m))
		m["consistency"] = 73.5
		legacy, err := json.Marshal(m)
		require.NoError(t, err)
		_, err = pool.Exec(ctx,
			`UPDATE runs SET bundle_sha = 'deadbeef', server_metrics = $2 WHERE id = $1`, id, legacy)
		require.NoError(t, err)
	}

	n, err = w.RevalidateBatch(ctx, core, discardLogger())
	require.NoError(t, err)
	require.Equal(t, len(ids), n, "every run judged by the old bundle backfills")

	for _, id := range ids {
		was := before[id]
		row := fetchRun(t, pool, id)

		// Verdicts must not change: not the status, not the verdict document,
		// not the score it was compared on.
		assert.Equal(t, was.Status, row.Status, "backfill moved a status")
		assert.JSONEq(t, string(was.Validation), string(row.Validation),
			"backfill moved the validation document")
		assert.JSONEq(t, string(was.ServerScore), string(row.ServerScore),
			"backfill moved the server score")

		// Metrics may only be enriched: wpm/raw/accuracy and chars identical to
		// the last bit; consistency back on the [0, 1] definition.
		var wasM, nowM metricsDoc
		require.NoError(t, json.Unmarshal(was.ServerMetrics, &wasM))
		require.NoError(t, json.Unmarshal(row.ServerMetrics, &nowM))
		assert.Equal(t, wasM.Wpm, nowM.Wpm, "backfill moved wpm")
		assert.Equal(t, wasM.Raw, nowM.Raw, "backfill moved raw")
		assert.Equal(t, wasM.Accuracy, nowM.Accuracy, "backfill moved accuracy")
		require.NotNil(t, nowM.Chars, "backfilled metrics lost the chars breakdown")
		assert.Equal(t, wasM.Chars, nowM.Chars, "backfill moved the chars breakdown")
		require.NotNil(t, nowM.Consistency, "backfilled metrics lost consistency")
		assert.GreaterOrEqual(t, *nowM.Consistency, 0.0)
		assert.LessOrEqual(t, *nowM.Consistency, 1.0)

		require.NotNil(t, row.BundleSha)
		assert.Equal(t, replay.BundleSHA(), *row.BundleSha)
	}
}

// The policy decides the status, and revalidation is what applies a policy
// change to history: the same three runs land accepted / accepted / flagged.
func TestRevalidateAppliesTheCurrentPolicy(t *testing.T) {
	pool := newPool(t)
	user := seedUser(t, pool)

	clean := insertPending(t, pool, user, loadVector(t, "words-clean"))
	weak := insertPending(t, pool, user, loadVector(t, "words-one-fast-interval"))
	bot := insertPending(t, pool, user, loadVector(t, "words-bot-cadence"))

	w := newTestWorker(t, replaypg.New(pool, nil), replay.WorkerConfig{BatchSize: 10})
	core, err := replay.NewCore(replay.DefaultReplayTimeout)
	require.NoError(t, err)

	// Pretend an older policy flagged everything that raised a flag — which is
	// exactly what the pre-policy rule did.
	_, err = w.RunBatch(context.Background(), core, discardLogger())
	require.NoError(t, err)
	_, err = pool.Exec(context.Background(),
		`UPDATE runs SET status = 'flagged', policy_version = NULL`)
	require.NoError(t, err)

	n, err := w.RevalidateBatch(context.Background(), core, discardLogger())
	require.NoError(t, err)
	require.Equal(t, 3, n)

	assert.Equal(t, replay.StatusAccepted, fetchRun(t, pool, clean).Status,
		"a clean run must come back from review")
	assert.Equal(t, replay.StatusAccepted, fetchRun(t, pool, weak).Status,
		"one rollover interval must come back from review")
	assert.Equal(t, replay.StatusFlagged, fetchRun(t, pool, bot).Status,
		"the bot-shaped run must stay in review")

	// The accepted run kept its flag and its arithmetic: moderation can still
	// see why it was close.
	var validation []byte
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT validation FROM runs WHERE id = $1`, weak).Scan(&validation))
	var doc struct {
		Flags  []map[string]any `json:"flags"`
		Policy struct {
			Version   int16   `json:"version"`
			Suspicion float64 `json:"suspicion"`
			Threshold float64 `json:"threshold"`
		} `json:"policy"`
	}
	require.NoError(t, json.Unmarshal(validation, &doc))
	assert.Len(t, doc.Flags, 1, "the flag must survive acceptance")
	assert.Equal(t, replay.CurrentPolicyVersion, doc.Policy.Version)
	assert.Positive(t, doc.Policy.Suspicion)
	assert.Less(t, doc.Policy.Suspicion, doc.Policy.Threshold)
}

// Revalidation must never touch the worker's own queue: a pending run belongs
// to the worker, not to a re-judge.
func TestRevalidateIgnoresPendingRuns(t *testing.T) {
	pool := newPool(t)
	user := seedUser(t, pool)
	id := insertPending(t, pool, user, loadVector(t, "words-clean"))

	w := newTestWorker(t, replaypg.New(pool, nil), replay.WorkerConfig{BatchSize: 10})
	core, err := replay.NewCore(replay.DefaultReplayTimeout)
	require.NoError(t, err)

	n, err := w.RevalidateBatch(context.Background(), core, discardLogger())
	require.NoError(t, err)
	assert.Zero(t, n, "a pending run must be left for the worker")
	assert.Equal(t, replay.StatusPending, fetchRun(t, pool, id).Status)
}
