package wspg_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/typemore/typemore-server/internal/platform/db"
	"github.com/typemore/typemore-server/internal/platform/migrate"
	"github.com/typemore/typemore-server/internal/ws"
	"github.com/typemore/typemore-server/internal/ws/wspg"
)

// The Postgres testcontainer is started lazily on first use, mirroring the auth
// and runs suites; TestMain only tears down whatever ensureDB started.
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

func gzipJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	require.NoError(t, err)
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, err = zw.Write(raw)
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	return buf.Bytes()
}

// TestSaveMatchRoundTrip persists a match with one authed run and one guest run,
// then reads the rows back: header fields, the NULL vs FK user_id, and the gzip
// capture (with server recv stamps) intact.
func TestSaveMatchRoundTrip(t *testing.T) {
	ctx := context.Background()
	pool, err := db.NewPool(ctx, ensureDB(t), 5)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	_, err = pool.Exec(ctx, `TRUNCATE matches, match_runs, users RESTART IDENTITY CASCADE`)
	require.NoError(t, err)

	var uid string
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO users (display_name) VALUES ('Neo') RETURNING id::text`).Scan(&uid))

	batches := []ws.CapturedBatch{
		{BatchSeq: 1, RecvServerMs: 111, Events: []json.RawMessage{json.RawMessage(`{"k":"insert","seq":1}`)}},
		{BatchSeq: 2, RecvServerMs: 222, Events: []json.RawMessage{json.RawMessage(`{"k":"commit","seq":2}`)}},
	}
	store := wspg.New(pool)
	rec := ws.MatchRecord{
		ID:       "m_roundtrip01",
		RoomCode: "ABC234",
		Name:     "Round Trip",
		Settings: json.RawMessage(`{"mode":"time","durationMs":30000,"textSource":{"kind":"seeded"}}`),
		Freemods: json.RawMessage(`[{"playerId":"p1","freemods":{"difficulty":"normal","minWpm":0,"nospace":false}}]`),
		Seed:     4294967295,
		DictHash: "en-default",
		Lang:     "en",
		GoAt:     time.Now().Add(-time.Minute).UTC().Truncate(time.Millisecond),
		EndedAt:  time.Now().UTC().Truncate(time.Millisecond),
		Runs: []ws.MatchRunRecord{
			{
				PlayerID:    "p1",
				Nick:        "Neo",
				UserID:      uid,
				Freemods:    json.RawMessage(`{"difficulty":"normal","minWpm":0,"nospace":false}`),
				Log:         gzipJSON(t, batches),
				BatchCount:  2,
				FinalStatus: "finished",
			},
			{
				PlayerID:    "p2",
				Nick:        "Guest-1234",
				UserID:      "",
				Freemods:    json.RawMessage(`{"difficulty":"expert","minWpm":60,"nospace":true}`),
				Log:         gzipJSON(t, []ws.CapturedBatch{}),
				BatchCount:  0,
				FinalStatus: "dnf",
			},
		},
	}
	require.NoError(t, store.SaveMatch(ctx, rec))

	// Match header round-trips.
	var (
		roomCode, lang, dictHash string
		seed                     int64
		settings, freemods       []byte
	)
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT room_code, lang, dict_hash, seed, settings, freemods FROM matches WHERE id = $1`, rec.ID).
		Scan(&roomCode, &lang, &dictHash, &seed, &settings, &freemods))
	assert.Equal(t, "ABC234", roomCode)
	assert.Equal(t, "en", lang)
	assert.Equal(t, int64(4294967295), seed)
	assert.JSONEq(t, string(rec.Settings), string(settings))
	assert.JSONEq(t, string(rec.Freemods), string(freemods))

	// Runs round-trip: the authed run keeps its user_id, the guest run is NULL,
	// and the gzip capture decodes to the same stamped batches.
	rows, err := pool.Query(ctx,
		`SELECT player_id, nick, user_id::text, log, batch_count, final_status
		 FROM match_runs WHERE match_id = $1 ORDER BY player_id`, rec.ID)
	require.NoError(t, err)
	defer rows.Close()

	type runRow struct {
		playerID, nick, status string
		userID                 *string
		log                    []byte
		batchCount             int
	}
	var got []runRow
	for rows.Next() {
		var r runRow
		require.NoError(t, rows.Scan(&r.playerID, &r.nick, &r.userID, &r.log, &r.batchCount, &r.status))
		got = append(got, r)
	}
	require.NoError(t, rows.Err())
	require.Len(t, got, 2)

	require.Equal(t, "p1", got[0].playerID)
	require.NotNil(t, got[0].userID)
	assert.Equal(t, uid, *got[0].userID)
	assert.Equal(t, "finished", got[0].status)
	assert.Equal(t, 2, got[0].batchCount)

	zr, err := gzip.NewReader(bytes.NewReader(got[0].log))
	require.NoError(t, err)
	raw, err := io.ReadAll(zr)
	require.NoError(t, err)
	var decoded []ws.CapturedBatch
	require.NoError(t, json.Unmarshal(raw, &decoded))
	require.Len(t, decoded, 2)
	assert.Equal(t, int64(111), decoded[0].RecvServerMs)
	assert.Equal(t, 2, decoded[1].BatchSeq)

	require.Equal(t, "p2", got[1].playerID)
	assert.Nil(t, got[1].userID, "guest run has NULL user_id")
	assert.Equal(t, "dnf", got[1].status)
}
