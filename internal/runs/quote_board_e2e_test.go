package runs_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/typemore/typemore-server/internal/replay/policy/policytest"
)

// bucketIndexRow is one row of GET /api/v1/leaderboards. Both board shapes share
// the endpoint, and a quote board's language fields are ABSENT rather than
// empty — the pointers are what tell the two apart on the wire.
type bucketIndexRow struct {
	Bucket     string     `json:"bucket"`
	QuoteID    *uuid.UUID `json:"quoteId"`
	Mode       string     `json:"mode"`
	DurationMs *int32     `json:"durationMs"`
	WordCount  *int32     `json:"wordCount"`
	Lang       string     `json:"lang"`
	TextSource string     `json:"textSource"`
	Entries    int64      `json:"entries"`
}

type boardPageRow struct {
	Rank   int64   `json:"rank"`
	UserID string  `json:"userId"`
	Score  int64   `json:"score"`
	WPM    float64 `json:"wpm"`
	Acc    float64 `json:"acc"`
	Source string  `json:"source"`
	RunID  string  `json:"runId"`
}

func bucketIndex(t *testing.T, h *harness) []bucketIndexRow {
	t.Helper()
	return decodeInto[struct {
		Buckets []bucketIndexRow `json:"buckets"`
	}](t, h.get("/api/v1/leaderboards")).Buckets
}

func boardPage(t *testing.T, h *harness, bucket string) []boardPageRow {
	t.Helper()
	resp := h.get("/api/v1/leaderboards/" + bucket)
	require.Equal(t, http.StatusOK, resp.StatusCode, "bucket %q", bucket)
	return decodeInto[struct {
		Entries []boardPageRow `json:"entries"`
	}](t, resp).Entries
}

// STAGE 6 — the whole quote path through the real stack, in one test.
//
// A quote is published, a real client payload is POSTed by reference only, the
// real worker resolves the bytes out of Postgres and judges the run through
// goja, and the run lands on THAT QUOTE'S board with the server's own numbers —
// and on no other board anywhere in the catalogue.
//
// internal/leaderboard covers the projection exhaustively against planted rows;
// what this covers is that the chain is wired at all: ingest → worker → verdict
// transaction → projection → the two read endpoints.
func TestQuoteRunIsJudgedAndLandsOnItsOwnBoard(t *testing.T) {
	h := newHarness(t)
	userID := h.login("quote-board@example.com", "correct horse battery", "quoter")

	v := goldenVector(t, "quote-fixed-text")
	require.NotNil(t, v.Quote)
	h.publishQuote(t, v.Quote.ID, v.Quote.Text, v.Quote.Hash, false)

	// A second, unrelated quote nobody plays. Its board must never appear: the
	// catalogue lists boards that HOLD a row, not quotes that exist.
	other := uuid.New()
	h.publishQuote(t, other, "Ein zweites Zitat, das niemand tippt.", "cafe0001", false)

	ingested := decodeInto[struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}](t, h.post("/api/v1/runs", v.Payload))
	require.Equal(t, "pending", ingested.Status)
	require.Empty(t, bucketIndex(t, h), "a pending run ranks nowhere")

	h.replayOnce(t)

	after := decodeInto[map[string]any](t, h.get("/api/v1/runs/"+ingested.ID))
	require.Equal(t, "accepted", after["status"], "validation: %v", after["validation"])

	// It is in the player's own history, with the quote lifted into the derived
	// cells — the handle the history row turns into a link and a text.
	list := decodeInto[struct {
		Runs []map[string]any `json:"runs"`
	}](t, h.get("/api/v1/runs"))
	require.Len(t, list.Runs, 1)
	assert.Equal(t, v.Quote.ID.String(), list.Runs[0]["quoteId"])
	assert.NotContains(t, list.Runs[0], "adoptedFromRunId",
		"a quote run drawn fresh is not a seeded repeat")

	// Exactly ONE board exists, and it is this quote's.
	index := bucketIndex(t, h)
	require.Len(t, index, 1, "a quote run must mint exactly one board: its own")
	board := index[0]
	assert.Equal(t, "quote:"+v.Quote.ID.String(), board.Bucket)
	require.NotNil(t, board.QuoteID)
	assert.Equal(t, v.Quote.ID, *board.QuoteID)
	assert.EqualValues(t, 1, board.Entries)
	// A quote board has none of a language board's dimensions — the quote IS the
	// dimension, and any second component could only repeat what the id fixes.
	assert.Empty(t, board.Mode)
	assert.Empty(t, board.Lang)
	assert.Empty(t, board.TextSource)
	assert.Nil(t, board.DurationMs)
	assert.Nil(t, board.WordCount)

	// The row carries the SERVER's numbers and the quote's attribution.
	rows := boardPage(t, h, board.Bucket)
	require.Len(t, rows, 1)
	serverScore := after["serverScore"].(map[string]any)
	serverMetrics := after["serverMetrics"].(map[string]any)
	assert.EqualValues(t, 1, rows[0].Rank)
	assert.Equal(t, userID, rows[0].UserID)
	assert.Equal(t, ingested.ID, rows[0].RunID)
	assert.EqualValues(t, serverScore["total"], float64(rows[0].Score))
	assert.InDelta(t, serverMetrics["wpm"].(float64), rows[0].WPM, 1e-9)
	assert.NotEmpty(t, rows[0].Source, "a quote is someone's words; the attribution is not optional")

	// And on NO other board. Asserted over another real quote's board and over
	// the language board the run would most plausibly have fallen into.
	assert.Empty(t, boardPage(t, h, "quote:"+other.String()),
		"a run on one quote must not appear on another quote's board")
	for _, key := range []string{
		"time:15000:" + v.Payload["lang"].(string) + ":seeded",
		"words:50:" + v.Payload["lang"].(string) + ":seeded",
	} {
		assert.Empty(t, boardPage(t, h, key),
			"a quote run must not reach a language board (%s)", key)
	}
}

// The other half of Stage 6: the corpus moves AFTER the run has been judged and
// ranked. Retiring a revision is not an edit — the row keeps its bytes — so the
// run stays judged against the version it was played on and stays on its board.
//
// `make revalidate` is the path that would notice otherwise: it re-judges runs
// behind a new bundle or policy, so it re-resolves the quote. This drives that
// same re-judgement explicitly rather than assuming the first verdict sticks
// because nothing looked again.
func TestASupersededQuoteLeavesAnAlreadyRankedRunAlone(t *testing.T) {
	h := newHarness(t)
	h.login("quote-supersede-board@example.com", "correct horse battery", "steadfast")

	v := goldenVector(t, "quote-fixed-text")
	h.publishQuote(t, v.Quote.ID, v.Quote.Text, v.Quote.Hash, false)

	ingested := decodeInto[struct {
		ID string `json:"id"`
	}](t, h.post("/api/v1/runs", v.Payload))
	h.replayOnce(t)

	bucket := "quote:" + v.Quote.ID.String()
	before := boardPage(t, h, bucket)
	require.Len(t, before, 1)

	// The typo fix lands the only way the registry allows: the played revision
	// is retired IN PLACE (text untouched) and the correction is inserted beside
	// it under a NEW id.
	h.supersedeQuote(t, v.Quote.ID)
	corrected := uuid.New()
	h.publishQuote(t, corrected, v.Quote.Text+" (korrigiert)", "beefcafe", false)

	// Re-judge the run for real. Revalidate only claims runs the current policy
	// or bundle has moved past, so the version is backdated first — otherwise
	// this pass is correctly a no-op and the test would be asserting that the
	// first verdict merely stuck because nothing looked again.
	h.stalePolicyVersion(t)
	h.revalidateOnce(t, policytest.NewFake())

	after := decodeInto[map[string]any](t, h.get("/api/v1/runs/"+ingested.ID))
	assert.Equal(t, "accepted", after["status"],
		"a retired revision stopped resolving: validation %v", after["validation"])

	assert.Equal(t, before, boardPage(t, h, bucket),
		"correcting a quote must not disturb the board of the version people played")

	// The correction is a DIFFERENT board — empty, because nobody has typed it.
	assert.Empty(t, boardPage(t, h, "quote:"+corrected.String()))

	index := bucketIndex(t, h)
	require.Len(t, index, 1, "the corrected quote is not a board until someone plays it")
	assert.Equal(t, bucket, index[0].Bucket)
}

// STAGE 3.5 through the real stack: a run whose TEXT was adopted from another
// run is saved, judged, and visible in history — and holds no board slot.
//
// The two runs are the SAME payload but for `setup.adoptedFromRunId`, so what
// the board is reacting to cannot be anything else: same words, same seed, same
// log, same score. Only one of them ranks.
func TestAnAdoptedRunIsSavedListedAndRankedNowhere(t *testing.T) {
	h := newHarness(t)
	h.login("adopted-e2e@example.com", "correct horse battery", "copier")

	payload := goldenPayload(t, "time-clean")
	require.EqualValues(t, 15000, payload["durationMs"], "the fixture must sit in a ranked bucket")

	original := decodeInto[struct {
		ID string `json:"id"`
	}](t, h.post("/api/v1/runs", payload))
	h.replayOnce(t)

	index := bucketIndex(t, h)
	require.Len(t, index, 1)
	bucket := index[0].Bucket
	require.Len(t, boardPage(t, h, bucket), 1)

	// The same run again, this time declaring where its text came from.
	repeat := goldenPayload(t, "time-clean")
	setup := repeat["setup"].(map[string]any)
	setup["adoptedFromRunId"] = original.ID
	adopted := decodeInto[struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}](t, h.post("/api/v1/runs", repeat))
	require.Equal(t, "pending", adopted.Status, "a seeded repeat is accepted like any other run")

	h.replayOnce(t)

	after := decodeInto[map[string]any](t, h.get("/api/v1/runs/"+adopted.ID))
	assert.Equal(t, "accepted", after["status"],
		"a seeded repeat is judged, not refused: validation %v", after["validation"])
	assert.NotNil(t, after["serverScore"], "it is scored like any other run — it just does not compete")

	// Visible in history, marked.
	list := decodeInto[struct {
		Runs []map[string]any `json:"runs"`
	}](t, h.get("/api/v1/runs"))
	require.Len(t, list.Runs, 2)
	byID := map[string]map[string]any{}
	for _, row := range list.Runs {
		byID[row["id"].(string)] = row
	}
	assert.Equal(t, original.ID, byID[adopted.ID]["adoptedFromRunId"])
	assert.NotContains(t, byID[original.ID], "adoptedFromRunId")

	// And ranked nowhere: the board still holds the ORIGINAL run, not the copy,
	// and no second board appeared.
	rows := boardPage(t, h, bucket)
	require.Len(t, rows, 1, "one slot per player per board, and the copy did not take it")
	assert.Equal(t, original.ID, rows[0].RunID)
	assert.Len(t, bucketIndex(t, h), 1)
}

// The same statement on a QUOTE board, because the exclusion is a property of
// the run and must not depend on which board shape it would have landed in.
func TestAnAdoptedQuoteRunIsSavedAndRankedNowhere(t *testing.T) {
	h := newHarness(t)
	h.login("adopted-quote-e2e@example.com", "correct horse battery", "quotecopier")

	v := goldenVector(t, "quote-fixed-text")
	h.publishQuote(t, v.Quote.ID, v.Quote.Text, v.Quote.Hash, false)

	repeat := map[string]any{}
	raw, err := json.Marshal(v.Payload)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &repeat))
	repeat["setup"].(map[string]any)["adoptedFromRunId"] = uuid.New().String()

	adopted := decodeInto[struct {
		ID string `json:"id"`
	}](t, h.post("/api/v1/runs", repeat))
	h.replayOnce(t)

	after := decodeInto[map[string]any](t, h.get("/api/v1/runs/"+adopted.ID))
	require.Equal(t, "accepted", after["status"], "validation: %v", after["validation"])

	assert.Empty(t, bucketIndex(t, h),
		"a seeded repeat mints no board, not even its quote's")
	assert.Empty(t, boardPage(t, h, "quote:"+v.Quote.ID.String()))
}
