package runs_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The whole chain, once, through the real components: a real client payload is
// POSTed, the real replay worker judges it through goja, and the run appears on
// its board with the server's own numbers.
//
// internal/leaderboard tests the projection exhaustively against planted runs;
// this is the test that would fail if the projector were never wired into the
// worker's transaction, or wired to the wrong bucket.
func TestAcceptedRunAppearsOnItsLeaderboard(t *testing.T) {
	h := newHarness(t)
	userID := h.login("board@example.com", "correct horse battery", "boarder")

	// time-clean is a 15s run, which IS a ranked shape. words-clean is 10 words,
	// which is NOT (ranked word counts are 25/50/100), so it can never rank —
	// TestUnrankedShapeNeverRanks below pins that.
	payload := goldenPayload(t, "time-clean")
	require.EqualValues(t, 15000, payload["durationMs"], "the fixture must sit in a ranked bucket")

	resp := h.post("/api/v1/runs", payload)
	require.Equal(t, http.StatusAccepted, resp.StatusCode)
	ingested := decodeInto[struct {
		ID string `json:"id"`
	}](t, resp)

	// Nothing is on any board while the run is pending: the projection follows
	// the verdict, and there is no verdict yet.
	empty := decodeInto[struct {
		Buckets []map[string]any `json:"buckets"`
	}](t, h.get("/api/v1/leaderboards"))
	require.Empty(t, empty.Buckets, "a pending run must not rank")

	h.replayOnce(t)

	after := decodeInto[map[string]any](t, h.get("/api/v1/runs/"+ingested.ID))
	require.Equal(t, "accepted", after["status"], "validation: %v", after["validation"])

	// The board index now knows the bucket the run was played in.
	index := decodeInto[struct {
		Buckets []struct {
			Bucket     string `json:"bucket"`
			Mode       string `json:"mode"`
			DurationMs *int32 `json:"durationMs"`
			Lang       string `json:"lang"`
			TextSource string `json:"textSource"`
			Entries    int64  `json:"entries"`
		} `json:"buckets"`
	}](t, h.get("/api/v1/leaderboards"))
	require.Len(t, index.Buckets, 1)
	bucket := index.Buckets[0]
	assert.Equal(t, "time:15000:german:seeded", bucket.Bucket)
	assert.Equal(t, "time", bucket.Mode)
	require.NotNil(t, bucket.DurationMs)
	assert.EqualValues(t, 15000, *bucket.DurationMs)
	assert.Equal(t, "seeded", bucket.TextSource,
		"a run with no textSource in its setup is seeded text")
	assert.EqualValues(t, 1, bucket.Entries)

	// And the row carries the SERVER's numbers, not the client's report.
	page := decodeInto[struct {
		Entries []struct {
			Rank        int64           `json:"rank"`
			UserID      string          `json:"userId"`
			DisplayName string          `json:"displayName"`
			Score       int64           `json:"score"`
			WPM         float64         `json:"wpm"`
			Acc         float64         `json:"acc"`
			Grade       string          `json:"grade"`
			Mods        json.RawMessage `json:"mods"`
			RunID       string          `json:"runId"`
		} `json:"entries"`
	}](t, h.get("/api/v1/leaderboards/"+bucket.Bucket))
	require.Len(t, page.Entries, 1)
	row := page.Entries[0]

	serverScore := after["serverScore"].(map[string]any)
	serverMetrics := after["serverMetrics"].(map[string]any)
	assert.EqualValues(t, 1, row.Rank)
	assert.Equal(t, userID, row.UserID)
	assert.Equal(t, "boarder", row.DisplayName)
	assert.Equal(t, ingested.ID, row.RunID)
	assert.EqualValues(t, serverScore["total"], float64(row.Score))
	assert.InDelta(t, serverMetrics["wpm"].(float64), row.WPM, 1e-9)
	assert.InDelta(t, serverMetrics["accuracy"].(float64), row.Acc, 1e-9)
	assert.Equal(t, "SS", row.Grade, "a flawless run grades SS")

	// The mods object reflects what was actually played, lifted from the setup.
	setup := payload["setup"].(map[string]any)
	generation := setup["generation"].(map[string]any)
	var mods map[string]any
	require.NoError(t, json.Unmarshal(row.Mods, &mods))
	assert.Equal(t, generation["punctuation"], mods["punctuation"])
	assert.Equal(t, generation["numbers"], mods["numbers"])
	assert.Equal(t, setup["config"].(map[string]any)["difficulty"], mods["difficulty"])

	// The player's own rank, through the authenticated route.
	me := decodeInto[struct {
		Entry struct {
			Rank int64 `json:"rank"`
		} `json:"entry"`
	}](t, h.get("/api/v1/leaderboards/"+bucket.Bucket+"/me"))
	assert.EqualValues(t, 1, me.Entry.Rank)
}

// A run of an unranked shape is a perfectly good run — accepted, stored,
// watchable — it just never reaches a board. words-clean is 10 words, and the
// ranked word counts are 25/50/100 (SCORING_CONCEPT §4).
func TestUnrankedShapeNeverRanks(t *testing.T) {
	h := newHarness(t)
	h.login("unranked@example.com", "correct horse battery", "unranked")

	payload := goldenPayload(t, "words-clean")
	require.EqualValues(t, 10, payload["wordCount"], "the fixture must be an unranked shape")

	ingested := decodeInto[struct {
		ID string `json:"id"`
	}](t, h.post("/api/v1/runs", payload))
	h.replayOnce(t)

	after := decodeInto[map[string]any](t, h.get("/api/v1/runs/"+ingested.ID))
	require.Equal(t, "accepted", after["status"], "an unranked shape is still a valid run")

	index := decodeInto[struct {
		Buckets []map[string]any `json:"buckets"`
	}](t, h.get("/api/v1/leaderboards"))
	assert.Empty(t, index.Buckets, "10 words is not a ranked bucket")

	// And it is still publicly watchable — ranking and visibility are separate
	// questions.
	assert.Equal(t, http.StatusOK, h.get("/api/v1/runs/"+ingested.ID+"/replay").StatusCode)
}

// The public replay endpoint is the other half of a watchable board: the row
// carries a run id, and anyone may fetch that run's setup, log and numbers.
func TestPublicReplayServesAnAcceptedRun(t *testing.T) {
	h := newHarness(t)
	h.login("watch@example.com", "correct horse battery", "watched")

	payload := goldenPayload(t, "words-clean")
	ingested := decodeInto[struct {
		ID string `json:"id"`
	}](t, h.post("/api/v1/runs", payload))
	h.replayOnce(t)

	// Anonymous: a fresh client with no cookie jar entry at all.
	h.logout()

	resp := h.get("/api/v1/runs/" + ingested.ID + "/replay")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	view := decodeInto[struct {
		RunID         string          `json:"runId"`
		DisplayName   string          `json:"displayName"`
		Mode          string          `json:"mode"`
		WordCount     *int32          `json:"wordCount"`
		Lang          string          `json:"lang"`
		Setup         json.RawMessage `json:"setup"`
		ServerMetrics map[string]any  `json:"serverMetrics"`
		ServerScore   map[string]any  `json:"serverScore"`
		Grade         string          `json:"grade"`
		Log           json.RawMessage `json:"log"`
	}](t, resp)

	assert.Equal(t, ingested.ID, view.RunID)
	assert.Equal(t, "watched", view.DisplayName)
	assert.Equal(t, "words", view.Mode)
	require.NotNil(t, view.WordCount)
	assert.EqualValues(t, 10, *view.WordCount)
	assert.Equal(t, "german", view.Lang)
	assert.Equal(t, "SS", view.Grade)

	// The setup is what regenerates the text, and the log is the run itself:
	// both must be complete enough to replay client-side without another call.
	assert.JSONEq(t, mustJSON(t, payload["setup"]), string(view.Setup))
	assert.JSONEq(t, mustJSON(t, payload["log"]), string(view.Log),
		"the log must round-trip byte-for-byte through gzip storage")

	// The server's numbers ride along so a viewer sees the authoritative result.
	assert.Equal(t, payload["clientScore"].(map[string]any)["total"], view.ServerScore["total"])
	assert.Equal(t, payload["clientMetrics"].(map[string]any)["wpm"], view.ServerMetrics["wpm"])

	// It carries nothing about HOW the run was judged: a spectator gets the
	// result, not the moderation trail.
	raw := decodeInto[map[string]any](t, h.get("/api/v1/runs/"+ingested.ID+"/replay"))
	assert.NotContains(t, raw, "validation")
	assert.NotContains(t, raw, "clientScore")
	assert.NotContains(t, raw, "clientMetrics")
}

// The access matrix. Everything that is not a public, accepted, unbanned run is
// the same 404 — a spectator must not be able to tell "under review" from
// "never existed".
func TestPublicReplayAccessMatrix(t *testing.T) {
	cases := []struct {
		name   string
		status string
		banned bool
		want   int
	}{
		{"accepted", "accepted", false, http.StatusOK},
		{"flagged", "flagged", false, http.StatusNotFound},
		{"rejected", "rejected", false, http.StatusNotFound},
		{"pending", "pending", false, http.StatusNotFound},
		{"accepted but the owner is banned", "accepted", true, http.StatusNotFound},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			userID := h.login("matrix@example.com", "correct horse battery", "matrixed")

			ingested := decodeInto[struct {
				ID string `json:"id"`
			}](t, h.post("/api/v1/runs", goldenPayload(t, "words-clean")))
			if tc.status != "pending" {
				h.replayOnce(t)
				h.setRunStatus(ingested.ID, tc.status)
			}
			if tc.banned {
				h.ban(userID)
			}

			// Even the OWNER gets the public answer here: the endpoint is the
			// spectator surface, and their own run stays reachable through the
			// authenticated ?log=1 path instead.
			resp := h.get("/api/v1/runs/" + ingested.ID + "/replay")
			assert.Equal(t, tc.want, resp.StatusCode)
		})
	}

	t.Run("nonexistent run", func(t *testing.T) {
		h := newHarness(t)
		resp := h.get("/api/v1/runs/00000000-0000-0000-0000-000000000000/replay")
		assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	})

	t.Run("not even a uuid", func(t *testing.T) {
		h := newHarness(t)
		resp := h.get("/api/v1/runs/nonsense/replay")
		assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	})
}

// A banned player's runs vanish from the public surfaces without being deleted,
// and come back the moment the ban is lifted.
func TestBanHidesTheBoardRowAndTheReplay(t *testing.T) {
	h := newHarness(t)
	userID := h.login("banned@example.com", "correct horse battery", "bannedone")

	ingested := decodeInto[struct {
		ID string `json:"id"`
	}](t, h.post("/api/v1/runs", goldenPayload(t, "time-clean")))
	h.replayOnce(t)

	const bucket = "time:15000:german:seeded"
	require.Len(t, h.boardEntries(bucket), 1)
	require.Equal(t, http.StatusOK, h.get("/api/v1/runs/"+ingested.ID+"/replay").StatusCode)

	h.ban(userID)

	assert.Empty(t, h.boardEntries(bucket), "a banned player must leave every board")
	assert.Equal(t, http.StatusNotFound, h.get("/api/v1/runs/"+ingested.ID+"/replay").StatusCode)

	// The projection was never touched, so unbanning is instant.
	h.unban(userID)
	assert.Len(t, h.boardEntries(bucket), 1)
	assert.Equal(t, http.StatusOK, h.get("/api/v1/runs/"+ingested.ID+"/replay").StatusCode)
}

// A demotion applied to an already-ranked run must take its board slot with it,
// through the same transaction that wrote the new status.
func TestDemotionThroughTheWorkerLeavesTheBoard(t *testing.T) {
	h := newHarness(t)
	h.login("demoted@example.com", "correct horse battery", "demotee")

	ingested := decodeInto[struct {
		ID string `json:"id"`
	}](t, h.post("/api/v1/runs", goldenPayload(t, "time-clean")))
	h.replayOnce(t)

	const bucket = "time:15000:german:seeded"
	require.Len(t, h.boardEntries(bucket), 1)

	h.setRunStatus(ingested.ID, "flagged")

	assert.Empty(t, h.boardEntries(bucket))
	empty := decodeInto[struct {
		Buckets []map[string]any `json:"buckets"`
	}](t, h.get("/api/v1/leaderboards"))
	assert.Empty(t, empty.Buckets, "an emptied bucket disappears from the index")
}

// --- helpers ---

// setRunStatus performs a moderator-style status change: the status write and
// the projection in ONE transaction, which is the contract every writer of
// runs.status has to honour.
func (h *harness) setRunStatus(runID, status string) {
	h.t.Helper()
	ctx := context.Background()

	tx, err := h.pool.Begin(ctx)
	require.NoError(h.t, err)
	defer func() { _ = tx.Rollback(ctx) }()

	_, err = tx.Exec(ctx, `UPDATE runs SET status = $1 WHERE id = $2`, status, runID)
	require.NoError(h.t, err)
	require.NoError(h.t, h.board.ProjectRun(ctx, tx, uuid.MustParse(runID)))
	require.NoError(h.t, tx.Commit(ctx))
}

func (h *harness) ban(userID string) {
	h.t.Helper()
	_, err := h.pool.Exec(context.Background(),
		`INSERT INTO bans (user_id, reason) VALUES ($1, 'testing')`, userID)
	require.NoError(h.t, err)
}

func (h *harness) unban(userID string) {
	h.t.Helper()
	_, err := h.pool.Exec(context.Background(), `DELETE FROM bans WHERE user_id = $1`, userID)
	require.NoError(h.t, err)
}

// boardEntries reads one bucket's visible rows through the public endpoint.
func (h *harness) boardEntries(bucket string) []map[string]any {
	h.t.Helper()
	page := decodeInto[struct {
		Entries []map[string]any `json:"entries"`
	}](h.t, h.get("/api/v1/leaderboards/"+bucket))
	return page.Entries
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return string(b)
}
