package runs_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	leaderboardpg "github.com/typemore/typemore-server/internal/leaderboard/pgstore"
	"github.com/typemore/typemore-server/internal/replay"
	replaypg "github.com/typemore/typemore-server/internal/replay/pgstore"
)

// End-to-end: a real client payload goes in through the HTTP ingest path, one
// replay pass judges it, and the run comes out accepted with the server's own
// numbers — equal to the client's, because the same bundle produced both.
//
// The payload is a golden vector from internal/replay/testdata (see its
// README.md); using it here means the HTTP contract and the worker are checked
// against the same artifact the core-level tests use.

// goldenPayload loads one golden vector's POST /runs body.
func goldenPayload(t *testing.T, name string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "replay", "testdata", "vectors", name+".json"))
	require.NoError(t, err, "golden vectors missing: run `node internal/replay/testdata/generate.mjs`")

	var v struct {
		Payload map[string]any `json:"payload"`
	}
	require.NoError(t, json.Unmarshal(raw, &v))
	require.NotEmpty(t, v.Payload)
	return v.Payload
}

// replayOnce runs exactly one worker batch against the harness's database, with
// the leaderboard projector attached exactly as cmd/server attaches it — so a
// verdict and the board move together, in one transaction, here too.
func (h *harness) replayOnce(t *testing.T) {
	t.Helper()
	core, err := replay.NewCore(replay.DefaultReplayTimeout)
	require.NoError(t, err)
	reg, err := replay.NewRegistry(core)
	require.NoError(t, err)

	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	queue := replaypg.New(h.pool, leaderboardpg.New(h.pool, false))
	w := replay.NewWorker(queue, reg, replay.WorkerConfig{BatchSize: 10}, discard)
	_, err = w.RunBatch(context.Background(), core, discard)
	require.NoError(t, err)
}

func TestSubmittedRunIsReplayedAndAccepted(t *testing.T) {
	h := newHarness(t)
	h.login("replay@example.com", "correct horse battery", "replayer")

	payload := goldenPayload(t, "words-clean")
	resp := h.post("/api/v1/runs", payload)
	require.Equal(t, http.StatusAccepted, resp.StatusCode)
	ingested := decodeInto[struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}](t, resp)
	require.Equal(t, "pending", ingested.Status, "ingestion is never authoritative")

	// Nothing about the verdict is visible yet.
	before := decodeInto[map[string]any](t, h.get("/api/v1/runs/"+ingested.ID))
	assert.Equal(t, "pending", before["status"])
	assert.NotContains(t, before, "serverScore", "an unjudged run must not pretend to have server numbers")
	assert.NotContains(t, before, "validation")
	assert.NotContains(t, before, "validatedAt")

	h.replayOnce(t)

	after := decodeInto[map[string]any](t, h.get("/api/v1/runs/"+ingested.ID))
	require.Equal(t, "accepted", after["status"], "validation: %v", after["validation"])

	// The server's score is exposed and equals the client's, exactly.
	serverScore, ok := after["serverScore"].(map[string]any)
	require.True(t, ok, "serverScore missing from the summary: %v", after)
	clientScore := payload["clientScore"].(map[string]any)
	assert.Equal(t, clientScore["total"], serverScore["total"])
	assert.Equal(t, clientScore["base"], serverScore["base"])
	assert.Equal(t, clientScore["comboPeak"], serverScore["comboPeak"])

	serverMetrics, ok := after["serverMetrics"].(map[string]any)
	require.True(t, ok, "serverMetrics missing from the summary")
	clientMetrics := payload["clientMetrics"].(map[string]any)
	assert.Equal(t, clientMetrics["wpm"], serverMetrics["wpm"])
	assert.Equal(t, clientMetrics["acc"], serverMetrics["accuracy"])

	validation, ok := after["validation"].(map[string]any)
	require.True(t, ok, "validation missing from the summary")
	assert.Equal(t, "valid", validation["verdict"])
	assert.Empty(t, validation["flags"])
	assert.NotContains(t, validation, "reason")

	validatedAt, ok := after["validatedAt"].(string)
	require.True(t, ok)
	parsed, err := time.Parse(time.RFC3339Nano, validatedAt)
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now(), parsed, time.Minute)

	// The listing carries the same verdict, so a profile feed needs no extra
	// round trip per run.
	list := decodeInto[struct {
		Runs []map[string]any `json:"runs"`
	}](t, h.get("/api/v1/runs"))
	require.Len(t, list.Runs, 1)
	assert.Equal(t, "accepted", list.Runs[0]["status"])
	assert.Equal(t, serverScore["total"], list.Runs[0]["serverScore"].(map[string]any)["total"])
}

// The same path with a tampered score: ingestion still accepts the payload
// (structural validation is game-agnostic), and the worker is what catches it.
func TestSubmittedRunWithInflatedScoreIsFlagged(t *testing.T) {
	h := newHarness(t)
	h.login("cheat@example.com", "correct horse battery", "cheater")

	payload := goldenPayload(t, "words-clean")
	score := payload["clientScore"].(map[string]any)
	honest := score["total"].(float64)
	score["total"] = honest * 4
	resp := h.post("/api/v1/runs", payload)
	require.Equal(t, http.StatusAccepted, resp.StatusCode)
	ingested := decodeInto[struct {
		ID string `json:"id"`
	}](t, resp)

	h.replayOnce(t)

	after := decodeInto[map[string]any](t, h.get("/api/v1/runs/"+ingested.ID))
	require.Equal(t, "flagged", after["status"])

	validation := after["validation"].(map[string]any)
	assert.Equal(t, "valid", validation["verdict"], "the log itself is fine")
	assert.Equal(t, "score_mismatch", validation["reason"])

	divergence := validation["divergence"].(map[string]any)
	assert.Equal(t, "total", divergence["field"])
	assert.Equal(t, honest*4, divergence["client"])
	assert.Equal(t, honest, divergence["server"])

	// The client's inflated number is preserved beside the server's correction —
	// that pair is the evidence.
	assert.Equal(t, honest*4, after["clientScore"].(map[string]any)["total"])
	assert.Equal(t, honest, after["serverScore"].(map[string]any)["total"])
}
