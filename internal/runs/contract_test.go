package runs_test

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The frontend's contract-drift snapshot (src/features/run-submit, guarded by
// build-payload.test.ts) mirrors the RUNS.md POST body field-for-field. These
// fixtures reproduce that exact shape server-side so a rename on either end —
// a struct json tag here, a payload key there — trips a test rather than
// silently dropping a field.
const (
	feLog     = `{"version":1,"events":[{"kind":"insert","seq":1,"t":12,"text":"t"}]}`
	feSetup   = `{"config":{"mode":"time","durationMs":15000,"maxExtraChars":20,"difficulty":"normal","nospace":false,"minWpm":0},"generation":{"mode":"time","length":15,"punctuation":false,"numbers":false,"randomCase":false,"reverse":false},"declaration":{"blind":false,"fading":false,"flashlight":false}}`
	feMetrics = `{"wpm":80,"raw":85,"acc":0.97}`
	feScore   = `{"version":2,"total":1234,"base":1300,"comboPeak":50,"accMultiplier":0.94,"timeBonus":null,"modMultiplier":1}`
)

// frontendRun mirrors buildRunPayload's time-mode output exactly.
func frontendRun() map[string]any {
	return map[string]any{
		"mode":          "time",
		"durationMs":    15000,
		"lang":          "en",
		"seed":          2864901,
		"dictHash":      "a1b2c3d4",
		"scoreVersion":  2,
		"setup":         json.RawMessage(feSetup),
		"clientMetrics": json.RawMessage(feMetrics),
		"clientScore":   json.RawMessage(feScore),
		"log":           json.RawMessage(feLog),
	}
}

// TestIngestFrontendPayloadContract is the server-side counterpart of the
// frontend payload snapshot test: it posts the exact documented POST /runs body
// and asserts every request field is consumed and persisted verbatim. A drift
// in any top-level json tag would surface as a missing/wrong column (or a 4xx),
// and the opaque snapshots must round-trip byte-equivalent.
func TestIngestFrontendPayloadContract(t *testing.T) {
	h := newHarness(t)
	h.login("contract@example.com", "password-123", "Contract")

	body := frontendRun()

	// The top-level key set matches RUNS.md's documented request fields exactly
	// (the same guard as the frontend's Object.keys snapshot).
	gotKeys := make([]string, 0, len(body))
	for k := range body {
		gotKeys = append(gotKeys, k)
	}
	sort.Strings(gotKeys)
	assert.Equal(t, []string{
		"clientMetrics", "clientScore", "dictHash", "durationMs", "lang",
		"log", "mode", "scoreVersion", "seed", "setup",
	}, gotKeys, "fixture must carry exactly the documented POST /runs fields")

	resp := h.post("/api/v1/runs", body)
	require.Equal(t, http.StatusAccepted, resp.StatusCode)
	id := uuid.MustParse(decodeInto[ingestResp](t, resp).ID)

	// Every bucket field landed in its indexed column — proves the top-level
	// json tags match field-for-field.
	var (
		mode         string
		durationMs   *int32
		wordCount    *int32
		lang         string
		seed         int64
		dictHash     string
		scoreVersion int16
		setup        []byte
		metrics      []byte
		score        []byte
		gz           []byte
	)
	require.NoError(t, h.pool.QueryRow(context.Background(),
		`SELECT mode, duration_ms, word_count, lang, seed, dict_hash,
		        score_version, setup, client_metrics, client_score, log
		   FROM runs WHERE id=$1`, id).
		Scan(&mode, &durationMs, &wordCount, &lang, &seed, &dictHash,
			&scoreVersion, &setup, &metrics, &score, &gz))

	assert.Equal(t, "time", mode)
	require.NotNil(t, durationMs)
	assert.Equal(t, int32(15000), *durationMs)
	assert.Nil(t, wordCount, "time-mode run must leave word_count NULL")
	assert.Equal(t, "en", lang)
	assert.Equal(t, int64(2864901), seed)
	assert.Equal(t, "a1b2c3d4", dictHash)
	assert.Equal(t, int16(2), scoreVersion, "submitted scoreVersion must be stored, not a constant")

	// Opaque snapshots round-trip as equivalent JSON (jsonb re-serializes).
	assert.JSONEq(t, feSetup, string(setup))
	assert.JSONEq(t, feMetrics, string(metrics))
	assert.JSONEq(t, feScore, string(score))

	// The log blob round-trips byte-for-byte.
	assert.Equal(t, []byte(feLog), gunzip(t, gz))
}
