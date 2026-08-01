package auth_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/typemore/typemore-server/internal/auth"
	"github.com/typemore/typemore-server/internal/auth/pgstore"
	"github.com/typemore/typemore-server/internal/platform/db"
	"github.com/typemore/typemore-server/internal/platform/migrate"
)

const frontendOrigin = "http://localhost:5173"

// The Postgres testcontainer is started lazily on first use by ensureDB, so
// pure unit tests in this package do not require Docker. TestMain only handles
// teardown of whatever ensureDB started.
var (
	dbOnce      sync.Once
	dbContainer *postgres.PostgresContainer
	testDSN     string
	dbErr       error
)

// ensureDB starts (once) the Postgres container and applies migrations,
// returning the DSN. Integration tests call it; failures fail the calling test.
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

// TestMain runs the suite and terminates the container if one was started.
func TestMain(m *testing.M) {
	code := m.Run()
	if dbContainer != nil {
		_ = dbContainer.Terminate(context.Background())
	}
	os.Exit(code)
}

// --- test harness ---

type harness struct {
	t      *testing.T
	server *httptest.Server
	client *http.Client
	mailer *recorderMailer
	pool   *pgxpool.Pool
	store  *pgstore.Store
	svc    *auth.Service
}

type serverOpts struct {
	providers map[string]auth.ProviderCredentials
	rateEvery time.Duration
	rateBurst int
	// captcha is nil by default — the disabled mode every other test in this
	// package exercises implicitly.
	captcha auth.CaptchaVerifier
}

// newHarness truncates the database, builds the auth service exactly as main
// wires it (routes at /api/v1/auth and /api/v1/me), and starts an httptest
// server plus a cookie-jar client that does not auto-follow redirects.
func newHarness(t *testing.T, mutators ...func(*serverOpts)) *harness {
	t.Helper()
	ctx := context.Background()

	opts := serverOpts{rateEvery: time.Millisecond, rateBurst: 1000}
	for _, m := range mutators {
		m(&opts)
	}

	pool, err := db.NewPool(ctx, ensureDB(t), 5)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	// Clean slate for this test.
	_, err = pool.Exec(ctx, `TRUNCATE users, sessions, email_tokens, auth_identities, user_credentials RESTART IDENTITY CASCADE`)
	require.NoError(t, err)

	store := pgstore.New(pool)
	mailer := &recorderMailer{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	svc := auth.NewService(store, store, mailer,
		auth.NewInMemoryRateLimiter(opts.rateEvery, opts.rateBurst),
		opts.captcha,
		auth.Config{
			FrontendOrigin:    frontendOrigin,
			CookieName:        "tm_session",
			CookieSecure:      false,
			SessionTTL:        time.Hour,
			OAuthRedirectBase: "http://server.test",
			Providers:         opts.providers,
		}, logger)

	r := chi.NewRouter()
	r.Route("/api/v1", func(r chi.Router) {
		r.Mount("/auth", svc.AuthRoutes())
		r.With(svc.RequireAuth).Get("/me", svc.HandleMe)
		// A probe behind the permission gate, wired exactly as main.go mounts
		// the admin subtree (OptionalAuth, then RequirePermission): the
		// permissions tests assert the 404-invisibility contract against it.
		r.With(svc.OptionalAuth).
			Handle("/permissions-probe",
				svc.RequirePermission(auth.PermBansRead)(
					http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						w.WriteHeader(http.StatusOK)
					})))
	})
	server := httptest.NewServer(r)
	t.Cleanup(server.Close)

	jar, _ := cookiejar.New(nil)
	client := &http.Client{
		Jar: jar,
		// Do not follow redirects: OAuth flows end in a redirect we want to inspect.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	return &harness{
		t:      t,
		server: server,
		client: client,
		mailer: mailer,
		pool:   pool,
		store:  store,
		svc:    svc,
	}
}

// post sends a JSON POST with the required Origin header (CSRF). body may be nil.
func (h *harness) post(path string, body any) *http.Response {
	h.t.Helper()
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(h.t, err)
		reader = strings.NewReader(string(b))
	}
	req, err := http.NewRequest(http.MethodPost, h.server.URL+path, reader)
	require.NoError(h.t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", frontendOrigin)
	resp, err := h.client.Do(req)
	require.NoError(h.t, err)
	return resp
}

// get sends a GET (no Origin needed for safe methods).
func (h *harness) get(path string) *http.Response {
	h.t.Helper()
	req, err := http.NewRequest(http.MethodGet, h.server.URL+path, http.NoBody)
	require.NoError(h.t, err)
	resp, err := h.client.Do(req)
	require.NoError(h.t, err)
	return resp
}

// readBody reads and closes a response body.
func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return string(b)
}

// --- recorder mailer ---

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

// lastToken returns the token embedded in the most recent email's link.
func (m *recorderMailer) lastToken(t *testing.T) string {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	require.NotEmpty(t, m.msgs, "expected an email to have been sent")
	match := tokenRe.FindStringSubmatch(m.msgs[len(m.msgs)-1].Body)
	require.Len(t, match, 2, "no token found in email body")
	tok, err := url.QueryUnescape(match[1])
	require.NoError(t, err)
	return tok
}

func (m *recorderMailer) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.msgs)
}
