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
	Name    string `json:"name"`
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

	logJSON, err := json.Marshal(p.Log)
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
	ValidatedAt   *time.Time
	Attempts      int16
	LastError     *string
}

func fetchRun(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) runRow {
	t.Helper()
	var r runRow
	err := pool.QueryRow(context.Background(), `
		SELECT status, server_metrics, server_score, validation, bundle_sha,
		       validated_at, attempts, last_error
		FROM runs WHERE id = $1`, id).
		Scan(&r.Status, &r.ServerMetrics, &r.ServerScore, &r.Validation, &r.BundleSha,
			&r.ValidatedAt, &r.Attempts, &r.LastError)
	require.NoError(t, err)
	return r
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newTestWorker(t *testing.T, q replay.Queue, cfg replay.WorkerConfig) *replay.Worker {
	t.Helper()
	core, err := replay.NewCore(replay.DefaultReplayTimeout)
	require.NoError(t, err)
	reg, err := replay.NewRegistry(core)
	require.NoError(t, err)
	return replay.NewWorker(q, reg, cfg, discardLogger())
}

// --- the queue --------------------------------------------------------------

// One pass over a real queue: the row leaves 'pending' with the server's numbers
// and the bundle that produced them recorded beside it.
func TestQueueProcessesAPendingRun(t *testing.T) {
	pool := newPool(t)
	user := seedUser(t, pool)
	v := loadVector(t, "words-clean")
	id := insertPending(t, pool, user, v)

	w := newTestWorker(t, replaypg.New(pool), replay.WorkerConfig{BatchSize: 10})
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
		q := countingQueue{inner: replaypg.New(pool), mu: &mu, seen: seen}
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
	w := newTestWorker(t, replaypg.New(pool), replay.WorkerConfig{
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
