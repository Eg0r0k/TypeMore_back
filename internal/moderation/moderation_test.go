package moderation_test

import (
	"context"
	"fmt"
	"os"
	"strings"
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

	"github.com/typemore/typemore-server/internal/moderation"
	"github.com/typemore/typemore-server/internal/platform/db"
	"github.com/typemore/typemore-server/internal/platform/migrate"
)

// The predicate table.
//
// "Banned right now" is `revoked_at IS NULL AND (expires_at IS NULL OR
// expires_at > now())`, written once in the active_bans view. Every state that
// predicate distinguishes is enumerated here, because the ones that are easy to
// get wrong are the two that look banned and are not: a lapsed temporary ban
// and a revoked one both still have a row.
func TestActiveBanPredicate(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	future := time.Now().Add(72 * time.Hour)
	past := time.Now().Add(-1 * time.Hour)

	for i, tc := range []struct {
		name       string
		expiresAt  *time.Time
		revoke     bool
		restricted bool
	}{
		{"permanent", nil, false, true},
		{"expires in the future", &future, false, true},
		{"expired", &past, false, false},
		{"revoked permanent", nil, true, false},
		{"revoked temporary", &future, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			user := h.user(t, fmt.Sprintf("subject%d", i))

			// A past expiry cannot be issued through the CLI (it refuses one),
			// so it is planted directly: the point is the READ predicate, and a
			// ban that lapsed while nobody was looking is the realistic way to
			// reach this state.
			_, err := h.store.Ban(ctx, user, "note", "tester", tc.expiresAt)
			require.NoError(t, err)
			if tc.revoke {
				_, err := h.store.Unban(ctx, user)
				require.NoError(t, err)
			}

			got, err := h.store.IsRestricted(ctx, user)
			require.NoError(t, err)
			assert.Equalf(t, tc.restricted, got, "%s: restricted", tc.name)
		})
	}
}

// A temporary ban lifts ITSELF. There is no janitor, no sweep and no scheduled
// job — expiry is evaluated inside the predicate at read time, so the account
// is restricted right up to the instant and unrestricted immediately after.
func TestATemporaryBanLiftsItself(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	user := h.user(t, "temp")

	soon := time.Now().Add(1200 * time.Millisecond)
	_, err := h.store.Ban(ctx, user, "cooling off", "tester", &soon)
	require.NoError(t, err)

	restricted, err := h.store.IsRestricted(ctx, user)
	require.NoError(t, err)
	require.True(t, restricted, "the ban is not in force while it should be")

	require.Eventually(t, func() bool {
		got, err := h.store.IsRestricted(ctx, user)
		require.NoError(t, err)
		return !got
	}, 5*time.Second, 100*time.Millisecond,
		"the ban did not lapse on its own; something is sweeping instead of evaluating")

	// And nothing was deleted: the row is still there, so the history survives.
	history, err := h.store.History(ctx, user)
	require.NoError(t, err)
	require.Len(t, history, 1)
	assert.Nil(t, history[0].RevokedAt, "a lapsed ban must not look revoked")
}

// Banning an already-banned account amends the ban rather than stacking a
// second one. Two simultaneous bans would make "when does this lift" a question
// with two answers.
func TestBanIsIdempotent(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	user := h.user(t, "repeat")

	first, err := h.store.Ban(ctx, user, "first note", "alice", nil)
	require.NoError(t, err)
	require.False(t, first.Amended)

	until := time.Now().Add(24 * time.Hour)
	second, err := h.store.Ban(ctx, user, "second note", "bob", &until)
	require.NoError(t, err)
	require.True(t, second.Amended, "a re-ban must amend, not insert")
	require.NotNil(t, second.Previous)

	assert.Equal(t, first.Ban.ID, second.Ban.ID, "the amendment created a new ban row")
	assert.Equal(t, "second note", second.Ban.Reason)
	assert.Equal(t, "bob", second.Ban.IssuedBy)
	require.NotNil(t, second.Ban.ExpiresAt)
	assert.Equal(t, "first note", second.Previous.Reason, "the previous state must be reported")

	history, err := h.store.History(ctx, user)
	require.NoError(t, err)
	assert.Len(t, history, 1, "amending must not leave a second row behind")
}

// Unbanning is revocation, not deletion, and unbanning twice is not an error
// the second time — it is a statement that there was nothing to do.
func TestUnbanRevokesAndIsIdempotent(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	user := h.user(t, "revoked")

	_, err := h.store.Ban(ctx, user, "note", "tester", nil)
	require.NoError(t, err)

	ban, err := h.store.Unban(ctx, user)
	require.NoError(t, err)
	require.NotNil(t, ban.RevokedAt)

	_, err = h.store.Unban(ctx, user)
	assert.ErrorIs(t, err, moderation.ErrNotBanned)

	history, err := h.store.History(ctx, user)
	require.NoError(t, err)
	require.Len(t, history, 1, "revocation must keep the row")
	assert.NotNil(t, history[0].RevokedAt)
}

// A revoked account can be banned again, and that is a NEW row: the history is
// two bans, not one that was edited.
func TestReBanningAfterRevocationIsANewRecord(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	user := h.user(t, "again")

	first, err := h.store.Ban(ctx, user, "first", "tester", nil)
	require.NoError(t, err)
	_, err = h.store.Unban(ctx, user)
	require.NoError(t, err)

	second, err := h.store.Ban(ctx, user, "second", "tester", nil)
	require.NoError(t, err)
	assert.False(t, second.Amended, "a revoked ban must not be amended back to life")
	assert.NotEqual(t, first.Ban.ID, second.Ban.ID)

	history, err := h.store.History(ctx, user)
	require.NoError(t, err)
	assert.Len(t, history, 2, "the history must show both bans")
}

// Identifier resolution: a uuid, an email and a display name all reach the same
// account, and an ambiguous display name refuses rather than guessing.
func TestIdentifierResolution(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	user := h.user(t, "Ada")
	h.identity(t, user, "ada@example.com")

	for _, identifier := range []string{user.String(), "ada@example.com", "ADA@EXAMPLE.COM", "Ada"} {
		got, err := h.store.ResolveUser(ctx, identifier)
		require.NoErrorf(t, err, "resolving %q", identifier)
		assert.Equalf(t, user, got.ID, "resolving %q", identifier)
	}

	_, err := h.store.ResolveUser(ctx, "nobody@example.com")
	assert.ErrorIs(t, err, moderation.ErrNoSuchUser)

	// Ambiguity by display name cannot happen TODAY, and the reason is worth
	// pinning rather than assuming: users.display_name is UNIQUE, so a second
	// account cannot take the name.
	_, err = h.pool.Exec(ctx, `INSERT INTO users (display_name) VALUES ('Ada')`)
	require.Error(t, err, "display_name stopped being unique; the ambiguity path is now reachable")
	assert.Contains(t, err.Error(), "users_display_name_key")

	// The ambiguity handling in ResolveUser is therefore unreachable defence,
	// kept deliberately: it is the difference between a schema change making a
	// lookup ambiguous and a schema change making banctl silently ban whichever
	// row sorted first. Asserted directly, on the error type, since no fixture
	// can provoke it.
	var ambiguous *moderation.ErrAmbiguousUser
	require.ErrorAs(t, &moderation.ErrAmbiguousUser{
		Identifier: "Ada",
		Candidates: []moderation.User{{ID: user}, {ID: uuid.New()}},
	}, &ambiguous)
	assert.Contains(t, ambiguous.Error(), "matches 2 accounts")
}

// list --active shows what the predicate says, not what the rows say.
func TestListActiveFiltersThroughThePredicate(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	active := h.user(t, "active")
	revoked := h.user(t, "revoked")
	lapsed := h.user(t, "lapsed")

	_, err := h.store.Ban(ctx, active, "still on", "tester", nil)
	require.NoError(t, err)
	_, err = h.store.Ban(ctx, revoked, "will be lifted", "tester", nil)
	require.NoError(t, err)
	_, err = h.store.Unban(ctx, revoked)
	require.NoError(t, err)
	past := time.Now().Add(-time.Hour)
	_, err = h.store.Ban(ctx, lapsed, "already over", "tester", &past)
	require.NoError(t, err)

	onlyActive, err := h.store.List(ctx, true, 50)
	require.NoError(t, err)
	require.Len(t, onlyActive, 1, "only the permanent ban is in force")
	assert.Equal(t, active, onlyActive[0].UserID)

	all, err := h.store.List(ctx, false, 50)
	require.NoError(t, err)
	assert.Len(t, all, 3, "--all must show revoked and lapsed bans too")
}

// --- harness ---

type harness struct {
	pool  *pgxpool.Pool
	store *moderation.Store
}

// newHarness gives each test its own empty users/bans tables in the shared
// container. Truncating rather than re-migrating keeps the suite to one
// container while leaving no state between tests.
func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()
	pool, err := db.NewPool(ctx, ensureDB(t), 5)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	_, err = pool.Exec(ctx, `TRUNCATE bans, auth_identities, users CASCADE`)
	require.NoError(t, err)

	return &harness{pool: pool, store: moderation.New(pool)}
}

func (h *harness) user(t *testing.T, name string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	require.NoError(t, h.pool.QueryRow(context.Background(),
		`INSERT INTO users (display_name) VALUES ($1) RETURNING id`, name).Scan(&id))
	return id
}

func (h *harness) identity(t *testing.T, userID uuid.UUID, email string) {
	t.Helper()
	_, err := h.pool.Exec(context.Background(),
		`INSERT INTO auth_identities (user_id, provider, provider_subject, email, email_verified)
		 VALUES ($1, 'email', $2, $3, true)`, userID, strings.ToLower(email), email)
	require.NoError(t, err)
}

// The Postgres testcontainer, started lazily and shared, exactly as the auth,
// runs, quote and leaderboard suites do it.
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
