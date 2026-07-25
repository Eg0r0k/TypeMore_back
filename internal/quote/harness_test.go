package quote_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/typemore/typemore-server/internal/platform/db"
	"github.com/typemore/typemore-server/internal/platform/migrate"
	"github.com/typemore/typemore-server/internal/quote"
	quotepg "github.com/typemore/typemore-server/internal/quote/pgstore"
	"github.com/typemore/typemore-server/internal/replay"
)

// The Postgres testcontainer is started lazily on first use and torn down in
// TestMain, mirroring the auth, runs and leaderboard suites.
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

// The core is expensive to build and safe to share: DictVersion serialises on
// the Core's own mutex, and these tests never run it concurrently anyway.
var (
	coreOnce sync.Once
	testCore *replay.Core
	coreErr  error
)

func ensureCore(t *testing.T) *replay.Core {
	t.Helper()
	coreOnce.Do(func() { testCore, coreErr = replay.NewCore(0) })
	require.NoError(t, coreErr, "build the vendored core bundle")
	return testCore
}

func TestMain(m *testing.M) {
	code := m.Run()
	if dbContainer != nil {
		_ = dbContainer.Terminate(context.Background())
	}
	os.Exit(code)
}

// registry is the quote test harness: a real Postgres, the real importer, and
// the real HTTP surface. Quotes are published through pgstore.Import rather
// than inserted by hand — the import IS most of what is under test, and a
// fixture that wrote rows directly would let a broken importer pass.
type registry struct {
	t      *testing.T
	pool   *pgxpool.Pool
	store  *quotepg.Store
	core   *replay.Core
	server *httptest.Server
}

func newRegistry(t *testing.T) *registry {
	t.Helper()
	ctx := context.Background()

	pool, err := db.NewPool(ctx, ensureDB(t), 5)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	_, err = pool.Exec(ctx, `TRUNCATE quotes`)
	require.NoError(t, err)

	store := quotepg.New(pool)
	svc := quote.NewService(store, slog.New(slog.NewTextHandler(io.Discard, nil)))

	r := chi.NewRouter()
	r.Route("/api/v1", func(r chi.Router) {
		r.Mount("/quotes", svc.Routes())
	})
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)

	return &registry{t: t, pool: pool, store: store, core: ensureCore(t), server: srv}
}

// --- fixtures ---

// spec describes one synthetic quote. Text is generated to the requested length
// so the schema's `length = char_length(text)` CHECK is exercised for real
// rather than sidestepped by a hand-written pair that happens to agree.
type spec struct {
	upstreamID int32
	length     int
	group      quote.LenGroup
	source     string
	// text overrides the generated body — used to publish an EDIT of a quote
	// that already exists.
	text string
}

// incoming turns specs into rows ready to publish, hashing each text through
// the real core exactly as the importer does.
func (r *registry) incoming(specs ...spec) []quote.Incoming {
	r.t.Helper()
	out := make([]quote.Incoming, len(specs))
	for i, s := range specs {
		text := s.text
		if text == "" {
			text = filler(s.upstreamID, s.length)
		}
		hash, err := quote.HashText(r.core, text)
		require.NoError(r.t, err)

		src := s.source
		if src == "" {
			src = fmt.Sprintf("fixture %d", s.upstreamID)
		}
		out[i] = quote.Incoming{
			UpstreamID: s.upstreamID,
			Text:       text,
			Source:     src,
			Length:     int32(len([]rune(text))),
			LenGroup:   s.group,
			TextHash:   hash,
		}
	}
	return out
}

// filler builds a deterministic body of exactly n characters, distinct per id.
func filler(id int32, n int) string {
	if n <= 0 {
		n = 12
	}
	seed := fmt.Sprintf("quote %d: ", id)
	if len(seed) >= n {
		return seed[:n]
	}
	return seed + strings.Repeat("x", n-len(seed))
}

func (r *registry) importLang(lang string, quotes []quote.Incoming) quote.ImportStats {
	r.t.Helper()
	stats, err := r.store.Import(context.Background(), lang, quotes)
	require.NoError(r.t, err)
	return stats
}

// --- assertions over the raw table ---

// storedQuote is one row read WITHOUT going through the domain, which is the
// only way to tell "hidden from browsing" from "gone".
type storedQuote struct {
	ID         uuid.UUID
	Lang       string
	UpstreamID int32
	Text       string
	Length     int32
	LenGroup   int16
	TextHash   string
	Superseded bool
	CreatedAt  time.Time
}

func (r *registry) storedQuotes() []storedQuote {
	r.t.Helper()
	rows, err := r.pool.Query(context.Background(), `
		SELECT id, lang, upstream_id, text, length, len_group, text_hash, superseded, created_at
		FROM quotes ORDER BY lang, upstream_id, created_at, id`)
	require.NoError(r.t, err)
	defer rows.Close()

	var out []storedQuote
	for rows.Next() {
		var q storedQuote
		require.NoError(r.t, rows.Scan(&q.ID, &q.Lang, &q.UpstreamID, &q.Text, &q.Length,
			&q.LenGroup, &q.TextHash, &q.Superseded, &q.CreatedAt))
		out = append(out, q)
	}
	require.NoError(r.t, rows.Err())
	return out
}

// --- HTTP helpers ---

func (r *registry) get(path string) *http.Response {
	r.t.Helper()
	resp, err := r.server.Client().Get(r.server.URL + path)
	require.NoError(r.t, err)
	r.t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func decodeInto[T any](t *testing.T, resp *http.Response) T {
	t.Helper()
	var v T
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&v))
	return v
}

func readBody(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return body
}
