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

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/typemore/typemore-server/internal/keyboard"
	keyboardpg "github.com/typemore/typemore-server/internal/keyboard/pgstore"
	leaderboardpg "github.com/typemore/typemore-server/internal/leaderboard/pgstore"
	"github.com/typemore/typemore-server/internal/quote"
	quotepg "github.com/typemore/typemore-server/internal/quote/pgstore"
	"github.com/typemore/typemore-server/internal/replay"
	replaypg "github.com/typemore/typemore-server/internal/replay/pgstore"
	"github.com/typemore/typemore-server/internal/replay/policy"
	"github.com/typemore/typemore-server/internal/replay/policy/policytest"
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
	return goldenVector(t, name).Payload
}

// goldenVectorFile is the part of a vector this suite reads: the submission,
// and — for a quote vector — the registry row it was played against. The two
// are separate on purpose: the text never travels in the payload, so the test
// has to plant it in the database exactly as the importer would.
type goldenVectorFile struct {
	Payload map[string]any `json:"payload"`
	Quote   *struct {
		ID   uuid.UUID `json:"id"`
		Hash string    `json:"hash"`
		Text string    `json:"text"`
	} `json:"quote"`
}

func goldenVector(t *testing.T, name string) goldenVectorFile {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "replay", "testdata", "vectors", name+".json"))
	require.NoError(t, err, "golden vectors missing: run `node internal/replay/testdata/generate.mjs`")

	var v goldenVectorFile
	require.NoError(t, json.Unmarshal(raw, &v))
	require.NotEmpty(t, v.Payload)
	return v
}

// replayOnce runs exactly one worker batch against the harness's database, with
// the leaderboard projector and the quote registry attached exactly as
// cmd/server attaches them — so a verdict and the board move together, in one
// transaction, and a quote run resolves its text out of real Postgres.
func (h *harness) replayOnce(t *testing.T) {
	t.Helper()
	core, err := replay.NewCore(replay.DefaultReplayTimeout)
	require.NoError(t, err)
	reg, err := replay.NewRegistry(core)
	require.NoError(t, err)

	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	layouts, err := keyboard.Load()
	require.NoError(t, err)
	queue := replaypg.New(h.pool, leaderboardpg.New(h.pool, false)).
		WithKeyboard(keyboardpg.New(layouts))
	w := replay.NewWorker(queue, reg, quote.ReplayResolver{Store: quotepg.New(h.pool)},
		replay.WorkerConfig{BatchSize: 10, Decider: fakeDecider(t)}, discard)
	_, err = w.RunBatch(context.Background(), core, discard)
	require.NoError(t, err)
}

// publishQuote plants one quote row the way `make import-quotes` would. The
// caller supplies the hash, so a test can stage the case where the registry's
// bytes and the run's claim have drifted apart.
func (h *harness) publishQuote(t *testing.T, id uuid.UUID, text, hash string, superseded bool) {
	t.Helper()
	_, err := h.pool.Exec(context.Background(),
		`INSERT INTO quotes (id, lang, upstream_id, text, source, length, len_group, text_hash, superseded)
		 VALUES ($1, 'german', $2, $3, 'golden vector', char_length($3), 1, $4, $5)`,
		id, int32(id.ID()%100_000), text, hash, superseded)
	require.NoError(t, err)
}

// supersedeQuote retires a published revision the way the importer does: it
// flips the flag and NEVER touches the text, which is the whole reason a run
// recorded against the retired row keeps resolving its own bytes.
func (h *harness) supersedeQuote(t *testing.T, id uuid.UUID) {
	t.Helper()
	tag, err := h.pool.Exec(context.Background(),
		`UPDATE quotes SET superseded = true WHERE id = $1`, id)
	require.NoError(t, err)
	require.EqualValues(t, 1, tag.RowsAffected(), "no such published quote")
}

// stalePolicyVersion backdates every run's recorded policy version so the next
// revalidate pass claims it. Without this, revalidate is correctly a no-op —
// it only re-judges what the current policy or bundle has moved past — and a
// test that wants to see a run judged AGAIN has to say so.
func (h *harness) stalePolicyVersion(t *testing.T) {
	t.Helper()
	_, err := h.pool.Exec(context.Background(), `UPDATE runs SET policy_version = 0`)
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

// --- quote runs -------------------------------------------------------------

// The whole fixed-text path, end to end: a quote is published, a run played on
// it is submitted by reference only, and the worker resolves the bytes back out
// of Postgres to judge it. The payload never carried the text; the server's
// numbers still match the client's exactly, because both came out of the same
// bundle over the same bytes.
func TestSubmittedQuoteRunIsReplayedAndAccepted(t *testing.T) {
	h := newHarness(t)
	h.login("quote-replay@example.com", "correct horse battery", "quoter")

	v := goldenVector(t, "quote-fixed-text")
	require.NotNil(t, v.Quote, "the quote vector must carry its registry row")
	h.publishQuote(t, v.Quote.ID, v.Quote.Text, v.Quote.Hash, false)

	// The submission is a reference, not a copy. If this ever stops holding,
	// the rest of the test is measuring the wrong thing.
	textSource := v.Payload["setup"].(map[string]any)["generation"].(map[string]any)["textSource"].(map[string]any)
	require.NotContains(t, textSource, "text", "the payload must not carry the quote text")
	require.NotContains(t, v.Payload, "durationMs")
	require.NotContains(t, v.Payload, "wordCount")

	resp := h.post("/api/v1/runs", v.Payload)
	require.Equal(t, http.StatusAccepted, resp.StatusCode)
	ingested := decodeInto[struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}](t, resp)
	require.Equal(t, "pending", ingested.Status)

	h.replayOnce(t)

	after := decodeInto[map[string]any](t, h.get("/api/v1/runs/"+ingested.ID))
	require.Equal(t, "accepted", after["status"], "validation: %v", after["validation"])

	clientScore := v.Payload["clientScore"].(map[string]any)
	serverScore := after["serverScore"].(map[string]any)
	assert.Equal(t, clientScore["total"], serverScore["total"])
	assert.Equal(t, clientScore["base"], serverScore["base"])

	clientMetrics := v.Payload["clientMetrics"].(map[string]any)
	serverMetrics := after["serverMetrics"].(map[string]any)
	assert.Equal(t, clientMetrics["wpm"], serverMetrics["wpm"])
	assert.Equal(t, clientMetrics["acc"], serverMetrics["accuracy"])

	validation := after["validation"].(map[string]any)
	assert.Equal(t, "valid", validation["verdict"])
	assert.NotContains(t, validation, "reason")
}

// A run played on a revision that has since been RETIRED must still replay.
// That is the entire reason /quotes/{id} serves superseded rows: supersession
// inserts a new revision beside the old one and never edits it, so the old
// bytes — and every score recorded against them — stay resolvable forever.
func TestQuoteRunOnASupersededRevisionStillReplays(t *testing.T) {
	h := newHarness(t)
	h.login("quote-superseded@example.com", "correct horse battery", "retired")

	v := goldenVector(t, "quote-fixed-text")
	// The revision the run was played on, now retired…
	h.publishQuote(t, v.Quote.ID, v.Quote.Text, v.Quote.Hash, true)
	// …and the revision that replaced it, under the same upstream key. It is
	// what /quotes and /quotes/random now serve, and it is NOT what this run
	// resolves: resolution is by id, so the newer row is a distractor.
	h.publishQuote(t, uuid.New(), v.Quote.Text+" (überarbeitet)", "beefcafe", false)

	resp := h.post("/api/v1/runs", v.Payload)
	require.Equal(t, http.StatusAccepted, resp.StatusCode)
	id := decodeInto[struct {
		ID string `json:"id"`
	}](t, resp).ID

	h.replayOnce(t)

	after := decodeInto[map[string]any](t, h.get("/api/v1/runs/"+id))
	assert.Equal(t, "accepted", after["status"],
		"a retired revision stopped replaying: validation %v", after["validation"])
}

// The registry holds different bytes than the run claims. The server cannot
// judge the run — but it also cannot prove anything about the player, and the
// likelier explanation is that the corpus moved. Flagged, explicitly NOT
// rejected, and retryable by `make revalidate` once the registry agrees again.
func TestQuoteRunWithAMismatchedHashIsFlaggedNotRejected(t *testing.T) {
	h := newHarness(t)
	h.login("quote-mismatch@example.com", "correct horse battery", "drifted")

	v := goldenVector(t, "quote-fixed-text")
	// Same id, different bytes — what an in-place edit of a published quote
	// would look like from the worker's side.
	h.publishQuote(t, v.Quote.ID, "Ganz andere Worte, gleiche Kennung.", "0badcafe", false)

	resp := h.post("/api/v1/runs", v.Payload)
	require.Equal(t, http.StatusAccepted, resp.StatusCode)
	id := decodeInto[struct {
		ID string `json:"id"`
	}](t, resp).ID

	h.replayOnce(t)

	after := decodeInto[map[string]any](t, h.get("/api/v1/runs/"+id))
	require.Equal(t, "flagged", after["status"])
	require.NotEqual(t, "rejected", after["status"],
		"a corpus that moved is not proof the run was bad")

	validation := after["validation"].(map[string]any)
	assert.Equal(t, "unknown_quote", validation["reason"])
	assert.Equal(t, "error", validation["verdict"],
		"the core never had an opinion — the server could not assemble the run")
	assert.NotContains(t, after, "serverScore",
		"a run that was not judged must not carry server numbers")
}

// An id the registry has never heard of: same verdict, same reasoning. A node
// restored from a stale dump must not start rejecting honest runs.
func TestQuoteRunOnAnUnknownQuoteIsFlaggedNotRejected(t *testing.T) {
	h := newHarness(t)
	h.login("quote-unknown@example.com", "correct horse battery", "stranger")

	// Nothing published at all.
	v := goldenVector(t, "quote-fixed-text")
	resp := h.post("/api/v1/runs", v.Payload)
	require.Equal(t, http.StatusAccepted, resp.StatusCode,
		"ingestion is game-agnostic: it never consults the registry")
	id := decodeInto[struct {
		ID string `json:"id"`
	}](t, resp).ID

	h.replayOnce(t)

	after := decodeInto[map[string]any](t, h.get("/api/v1/runs/"+id))
	require.Equal(t, "flagged", after["status"])
	require.NotEqual(t, "rejected", after["status"])
	assert.Equal(t, "unknown_quote", after["validation"].(map[string]any)["reason"])
}

// revalidateOnce drives one bundle/policy-aware revalidate batch under the
// given judge, keyboard projector attached — the same pass `make revalidate`
// runs, which is also the keyboard projection's backfill mechanism.
func (h *harness) revalidateOnce(t *testing.T, judge policy.Judge) {
	t.Helper()
	decider, err := replay.NewDecider(judge)
	require.NoError(t, err)
	core, err := replay.NewCore(replay.DefaultReplayTimeout)
	require.NoError(t, err)
	reg, err := replay.NewRegistry(core)
	require.NoError(t, err)
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	layouts, err := keyboard.Load()
	require.NoError(t, err)
	queue := replaypg.New(h.pool, leaderboardpg.New(h.pool, false)).
		WithKeyboard(keyboardpg.New(layouts))
	w := replay.NewWorker(queue, reg, quote.ReplayResolver{Store: quotepg.New(h.pool)},
		replay.WorkerConfig{BatchSize: 50, Decider: decider}, discard)
	for {
		n, err := w.RevalidateBatch(context.Background(), core, discard)
		require.NoError(t, err)
		if n == 0 {
			return
		}
	}
}

// fakeDecider binds the deterministic fake judge. These end-to-end tests are
// about ingest, replay and the projections around them, not about the shipped
// review policy — which is behind a build tag they must not depend on.
func fakeDecider(t *testing.T) replay.Decider {
	t.Helper()
	d, err := replay.NewDecider(policytest.NewFake())
	require.NoError(t, err)
	return d
}
