package runs_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"regexp"
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

	"github.com/typemore/typemore-server/internal/auth"
	authpg "github.com/typemore/typemore-server/internal/auth/pgstore"
	"github.com/typemore/typemore-server/internal/platform/db"
	"github.com/typemore/typemore-server/internal/platform/migrate"
	"github.com/typemore/typemore-server/internal/runs"
	runspg "github.com/typemore/typemore-server/internal/runs/pgstore"
)

const frontendOrigin = "http://localhost:5173"

// The Postgres testcontainer is started lazily on first use and torn down in
// TestMain, mirroring the auth suite's approach.
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

// harness wires the auth + runs domains exactly as cmd/server does (runs mounted
// behind RequireOrigin + RequireAuth), backed by a real Postgres, plus a
// cookie-jar HTTP client.
type harness struct {
	t      *testing.T
	server *httptest.Server
	client *http.Client
	mailer *recorderMailer
	pool   *pgxpool.Pool
}

type harnessOpts struct {
	runsRateEvery time.Duration
	runsRateBurst int
}

func newHarness(t *testing.T, mutators ...func(*harnessOpts)) *harness {
	t.Helper()
	ctx := context.Background()

	opts := harnessOpts{runsRateEvery: time.Millisecond, runsRateBurst: 1000}
	for _, m := range mutators {
		m(&opts)
	}

	pool, err := db.NewPool(ctx, ensureDB(t), 5)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	_, err = pool.Exec(ctx, `TRUNCATE runs, users, sessions, email_tokens, auth_identities, user_credentials RESTART IDENTITY CASCADE`)
	require.NoError(t, err)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mailer := &recorderMailer{}

	authStore := authpg.New(pool)
	authSvc := auth.NewService(authStore, authStore, mailer,
		// Effectively unlimited per-IP auth limiter so the test's own
		// register/verify/login traffic never trips it.
		auth.NewInMemoryRateLimiter(time.Millisecond, 1000),
		auth.Config{
			FrontendOrigin: frontendOrigin,
			CookieName:     "tm_session",
			CookieSecure:   false,
			SessionTTL:     time.Hour,
		}, logger)

	runsStore := runspg.New(pool)
	runsSvc := runs.NewService(runsStore,
		auth.NewInMemoryRateLimiter(opts.runsRateEvery, opts.runsRateBurst),
		func(c context.Context) (uuid.UUID, bool) {
			u, ok := auth.UserFrom(c)
			return u.ID, ok
		},
		logger)

	r := chi.NewRouter()
	r.Route("/api/v1", func(r chi.Router) {
		r.Mount("/auth", authSvc.AuthRoutes())
		r.With(authSvc.RequireAuth).Get("/me", authSvc.HandleMe)
		r.Group(func(r chi.Router) {
			r.Use(authSvc.RequireOrigin)
			r.Use(authSvc.RequireAuth)
			r.Mount("/runs", runsSvc.Routes())
		})
	})
	server := httptest.NewServer(r)
	t.Cleanup(server.Close)

	jar, _ := cookiejar.New(nil)
	client := &http.Client{
		Jar:           jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	return &harness{t: t, server: server, client: client, mailer: mailer, pool: pool}
}

// --- HTTP helpers ---

func (h *harness) post(path string, body any) *http.Response {
	h.t.Helper()
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(h.t, err)
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(http.MethodPost, h.server.URL+path, reader)
	require.NoError(h.t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", frontendOrigin)
	resp, err := h.client.Do(req)
	require.NoError(h.t, err)
	return resp
}

// postRaw sends a raw byte body (used for the oversized 413 case) with the CSRF
// Origin header.
func (h *harness) postRaw(path string, body []byte) *http.Response {
	h.t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.server.URL+path, bytes.NewReader(body))
	require.NoError(h.t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", frontendOrigin)
	resp, err := h.client.Do(req)
	require.NoError(h.t, err)
	return resp
}

func (h *harness) get(path string) *http.Response {
	h.t.Helper()
	req, err := http.NewRequest(http.MethodGet, h.server.URL+path, http.NoBody)
	require.NoError(h.t, err)
	resp, err := h.client.Do(req)
	require.NoError(h.t, err)
	return resp
}

// login registers, verifies, and logs in a fresh user, leaving a live session
// in the cookie jar. It returns the new user's id.
func (h *harness) login(email, password, name string) string {
	h.t.Helper()
	requireStatus(h.t, h.post("/api/v1/auth/register", map[string]string{
		"email": email, "password": password, "displayName": name,
	}), http.StatusOK)
	requireStatus(h.t, h.post("/api/v1/auth/verify", map[string]string{
		"token": h.mailer.lastToken(h.t),
	}), http.StatusOK)
	resp := h.post("/api/v1/auth/login", map[string]string{"email": email, "password": password})
	require.Equal(h.t, http.StatusOK, resp.StatusCode)
	var me struct {
		ID string `json:"id"`
	}
	require.NoError(h.t, json.NewDecoder(resp.Body).Decode(&me))
	_ = resp.Body.Close()
	return me.ID
}

// logout clears the current session cookie.
func (h *harness) logout() {
	h.t.Helper()
	requireStatus(h.t, h.post("/api/v1/auth/logout", nil), http.StatusOK)
}

func requireStatus(t *testing.T, resp *http.Response, want int) {
	t.Helper()
	got := resp.StatusCode
	_ = resp.Body.Close()
	require.Equal(t, want, got)
}

func decodeInto[T any](t *testing.T, resp *http.Response) T {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	var v T
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&v))
	return v
}

func readBody(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return b
}

func gunzip(t *testing.T, gz []byte) []byte {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	require.NoError(t, err)
	out, err := io.ReadAll(zr)
	require.NoError(t, err)
	require.NoError(t, zr.Close())
	return out
}

// --- recorder mailer (captures verification links) ---

type recorderMailer struct {
	mu   sync.Mutex
	msgs []auth.Mail
}

func (m *recorderMailer) Send(_ context.Context, msg auth.Mail) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.msgs = append(m.msgs, msg)
	return nil
}

var tokenRe = regexp.MustCompile(`token=(\S+)`)

func (m *recorderMailer) lastToken(t *testing.T) string {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	require.NotEmpty(t, m.msgs, "expected an email to have been sent")
	match := tokenRe.FindStringSubmatch(m.msgs[len(m.msgs)-1].Body)
	require.Len(t, match, 2, "no token in email body")
	return strings.TrimSpace(match[1])
}
