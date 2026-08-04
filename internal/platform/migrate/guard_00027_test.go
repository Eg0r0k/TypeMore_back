package migrate_test

// Migration 00027's DO-block guard, applied to a database that actually holds
// the rows it is about.
//
// WHY THIS TEST EXISTS. Every other suite migrates a FRESH database, so the
// guard has only ever run against zero rows: the branch that fires has never
// executed anywhere. That branch is the whole point of the migration — it turns
// "could not create unique index … duplicate key value" into a message telling
// an operator which matches to look at and why they cannot simply delete them.
// A guard nobody has run is a guard nobody knows works.
//
// The test migrates to 00026, plants the violating rows, and then asks for
// 00027 — which is exactly the sequence a real deployment would hit.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/typemore/typemore-server/internal/platform/db"
	"github.com/typemore/typemore-server/internal/platform/migrate"
)

// seedMatch inserts the parent match row match_runs references.
func seedMatch(t *testing.T, pool *pgxpool.Pool, matchID string) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO matches (id, room_code, name, settings, freemods, seed,
		                     dict_hash, lang, go_at, ended_at)
		VALUES ($1, 'AAAAAA', 'guarded', '{}'::jsonb, '[]'::jsonb, 1,
		        'deadbeef', 'en', now(), now())
		ON CONFLICT (id) DO NOTHING`, matchID)
	require.NoError(t, err, "seed matches")
}

// seedMatchRun inserts one capture row for (match, user) with the columns 00003
// declares. userID nil is a GUEST seat — the case the partial index exempts.
func seedMatchRun(t *testing.T, pool *pgxpool.Pool, matchID string, userID *uuid.UUID, playerID string) {
	t.Helper()
	seedMatch(t, pool, matchID)
	_, err := pool.Exec(context.Background(), `
		INSERT INTO match_runs (match_id, player_id, nick, user_id, freemods, log, batch_count, final_status)
		VALUES ($1, $2, $3, $4, '{}'::jsonb, '8b'::bytea, 0, 'finished')`,
		matchID, playerID, "seat-"+playerID, userID)
	require.NoError(t, err, "seed match_runs")
}

func TestMigration00027RefusesAMatchAnAccountPlayedAgainstItself(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	dsn := freshDB(t)
	pool, err := db.NewPool(ctx, dsn, 3)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	// Stop one short of the migration under test.
	// `up-to` rather than a new exported helper: Run already takes any goose
	// command, so testing this needs nothing added to the production package.
	require.NoError(t, migrate.Run(ctx, dsn, "up-to", "26"), "migrate to 00026")

	// The exact history 00027 is about: one account, two seats, one match.
	matchID := "m_selfrace"
	user := uuid.New()
	_, err = pool.Exec(ctx, `INSERT INTO users (id, display_name) VALUES ($1, $2)`, user, "selfracer")
	require.NoError(t, err)
	seedMatchRun(t, pool, matchID, &user, "p1")
	seedMatchRun(t, pool, matchID, &user, "p2")

	// The guard must fire, and it must say what the operator has to decide.
	err = migrate.Up(ctx, dsn)
	require.Error(t, err, "00027 must refuse a database that already violates its rule")
	assert.Contains(t, err.Error(), "more than one row",
		"the refusal names the shape of the problem, not an index page")

	// Nothing was half-applied: the index is absent and 00027 is not recorded.
	var indexes int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_indexes WHERE indexname = 'match_runs_one_seat_per_user'`).
		Scan(&indexes))
	assert.Zero(t, indexes, "a refused migration leaves no index behind")

	var version int64
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT max(version_id) FROM goose_db_version WHERE is_applied`).Scan(&version))
	assert.EqualValues(t, 26, version, "the schema stays where it was")

	// Resolve the duplicate the way the hint describes — a human choosing which
	// capture is real — and the migration goes through.
	_, err = pool.Exec(ctx, `DELETE FROM match_runs WHERE match_id = $1 AND player_id = 'p2'`, matchID)
	require.NoError(t, err)
	require.NoError(t, migrate.Up(ctx, dsn), "with the history resolved, 00027 applies")

	// And the index it creates now enforces the rule going forward.
	seedMatchRun(t, pool, "m_second", &user, "p3")
	_, err = pool.Exec(ctx, `
		INSERT INTO match_runs (match_id, player_id, nick, user_id, freemods, log, batch_count, final_status)
		VALUES ('m_second', 'p4', 'dupe', $1, '{}'::jsonb, '8b'::bytea, 0, 'finished')`, user)
	require.Error(t, err, "the index refuses a second seat for the same account")
	assert.True(t, strings.Contains(err.Error(), "match_runs_one_seat_per_user"),
		"and it is THAT index that refuses it: %v", err)
}

// A guest has no account, so any number of guest seats in one match is legal —
// the index is partial for exactly this reason, and a plain UNIQUE would have
// let only one guest per match through.
func TestMigration00027LeavesGuestSeatsAlone(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	dsn := freshDB(t)
	pool, err := db.NewPool(ctx, dsn, 3)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	require.NoError(t, migrate.Run(ctx, dsn, "up-to", "26"))

	for _, player := range []string{"g1", "g2", "g3"} {
		seedMatchRun(t, pool, "m_guests", nil, player)
	}

	require.NoError(t, migrate.Up(ctx, dsn), "guest seats are not a violation")

	// Still true after the index exists.
	_, err = pool.Exec(ctx, `
		INSERT INTO match_runs (match_id, player_id, nick, user_id, freemods, log, batch_count, final_status)
		VALUES ('m_guests', 'g4', 'g4', NULL, '{}'::jsonb, '8b'::bytea, 0, 'finished')`)
	assert.NoError(t, err, "a fourth guest in the same match is still legal")
}
