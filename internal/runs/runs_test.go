package runs_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sampleLog is a valid log-v1 envelope. The server treats events as opaque
// beyond the version/seq structural checks, so a couple of events suffice.
const sampleLog = `{"version":1,"events":[{"kind":"insert","seq":1,"t":12,"text":"t"},{"kind":"insert","seq":2,"t":96,"text":"h"},{"kind":"commit","seq":3,"t":240}]}`

type ingestResp struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

type errResp struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

type listResp struct {
	Runs []struct {
		ID        string    `json:"id"`
		Status    string    `json:"status"`
		CreatedAt time.Time `json:"createdAt"`
	} `json:"runs"`
	NextCursor string `json:"nextCursor"`
}

// validRun builds a structurally-valid POST /runs body (a time-mode run).
func validRun() map[string]any {
	return map[string]any{
		"mode":          "time",
		"durationMs":    15000,
		"lang":          "en",
		"seed":          2864901,
		"dictHash":      "a1b2c3d4",
		"setup":         json.RawMessage(`{"mode":"time","punctuation":true}`),
		"clientMetrics": json.RawMessage(`{"wpm":80,"raw":85,"acc":0.97}`),
		"clientScore":   json.RawMessage(`{"version":1,"total":1234}`),
		"scoreVersion":  1,
		"log":           json.RawMessage(sampleLog),
	}
}

// --- happy path ---

func TestIngestHappyPath(t *testing.T) {
	h := newHarness(t)
	userID := h.login("runner@example.com", "password-123", "Runner")

	resp := h.post("/api/v1/runs", validRun())
	require.Equal(t, http.StatusAccepted, resp.StatusCode)
	body := decodeInto[ingestResp](t, resp)
	require.NotEmpty(t, body.ID)
	assert.Equal(t, "pending", body.Status)

	id := uuid.MustParse(body.ID)
	ctx := context.Background()

	// The row landed, owned by the caller, status pending.
	var (
		gz       []byte
		logBytes int
		status   string
		owner    uuid.UUID
	)
	require.NoError(t, h.pool.QueryRow(ctx,
		`SELECT log, log_bytes, status, user_id FROM runs WHERE id=$1`, id).
		Scan(&gz, &logBytes, &status, &owner))
	assert.Equal(t, "pending", status)
	assert.Equal(t, userID, owner.String())

	// gzip round-trips byte-exact and log_bytes is the uncompressed size.
	assert.Equal(t, []byte(sampleLog), gunzip(t, gz), "stored gzip must round-trip byte-exact")
	assert.Equal(t, len(sampleLog), logBytes)

	// The ?log=1 detail flag streams the gunzipped log verbatim.
	logResp := h.get("/api/v1/runs/" + body.ID + "?log=1")
	require.Equal(t, http.StatusOK, logResp.StatusCode)
	assert.Equal(t, sampleLog, string(readBody(t, logResp)))

	// Plain detail omits the log payload entirely.
	detail := h.get("/api/v1/runs/" + body.ID)
	require.Equal(t, http.StatusOK, detail.StatusCode)
	raw := readBody(t, detail)
	assert.NotContains(t, string(raw), `"log"`)
	assert.Contains(t, string(raw), `"logBytes"`)
}

// --- auth required ---

func TestIngestAuthRequired(t *testing.T) {
	h := newHarness(t)
	// No login: no session cookie.
	resp := h.post("/api/v1/runs", validRun())
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	assert.Equal(t, "unauthorized", decodeInto[errResp](t, resp).Error)

	requireStatus(t, h.get("/api/v1/runs"), http.StatusUnauthorized)
}

// --- body cap ---

func TestIngestBodyTooLarge(t *testing.T) {
	h := newHarness(t)
	h.login("big@example.com", "password-123", "Big")

	// Comfortably past the 6.5 MiB cap, so MaxBytesReader trips before
	// validation runs.
	oversized := []byte(`{"setup":"` + strings.Repeat("a", 8<<20) + `"}`)
	resp := h.postRaw("/api/v1/runs", oversized)
	require.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode)
	assert.Equal(t, "payload_too_large", decodeInto[errResp](t, resp).Error)
}

// --- structural 422s with distinct codes ---

func TestIngestBadLogVersion(t *testing.T) {
	h := newHarness(t)
	h.login("badver@example.com", "password-123", "BadVer")

	body := validRun()
	// 2 became legal with the telemetry log (KnownLogVersions {1, 2}); 3 is the
	// unsupported specimen now.
	body["log"] = json.RawMessage(`{"version":3,"events":[{"seq":1}]}`)
	resp := h.post("/api/v1/runs", body)
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)
	assert.Equal(t, "unsupported_log_version", decodeInto[errResp](t, resp).Error)
}

// TestIngestLogVersions pins the version-gated telemetry grammar: a v2 log with
// well-formed down/up events is accepted; a v1 log carrying telemetry, or a v2
// telemetry event with a junk code, is structurally malformed.
func TestIngestLogVersions(t *testing.T) {
	h := newHarness(t)
	h.login("logver@example.com", "password-123", "LogVer")

	post := func(log string) *http.Response {
		body := validRun()
		body["log"] = json.RawMessage(log)
		return h.post("/api/v1/runs", body)
	}

	// A v2 run: telemetry brackets the keystroke, seq stays strictly increasing.
	resp := post(`{"version":2,"events":[
		{"kind":"down","seq":1,"t":0,"code":"KeyA"},
		{"kind":"insert","seq":2,"t":8,"text":"a"},
		{"kind":"up","seq":3,"t":40,"code":"KeyA"}]}`)
	require.Equal(t, http.StatusAccepted, resp.StatusCode)

	// The same events mislabeled v1: telemetry requires version 2.
	resp = post(`{"version":1,"events":[
		{"kind":"down","seq":1,"t":0,"code":"KeyA"},
		{"kind":"insert","seq":2,"t":8,"text":"a"}]}`)
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)
	assert.Equal(t, "malformed_log", decodeInto[errResp](t, resp).Error)

	// A v2 telemetry event whose code is not a KeyboardEvent.code shape.
	resp = post(`{"version":2,"events":[
		{"kind":"down","seq":1,"t":0,"code":"Key F!"},
		{"kind":"insert","seq":2,"t":8,"text":"a"}]}`)
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)
	assert.Equal(t, "malformed_log", decodeInto[errResp](t, resp).Error)
}

func TestIngestNonMonotonicSeq(t *testing.T) {
	h := newHarness(t)
	h.login("seq@example.com", "password-123", "Seq")

	body := validRun()
	body["log"] = json.RawMessage(`{"version":1,"events":[{"seq":1},{"seq":1}]}`)
	resp := h.post("/api/v1/runs", body)
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)
	assert.Equal(t, "non_monotonic_seq", decodeInto[errResp](t, resp).Error)
}

func TestIngestTooManyEvents(t *testing.T) {
	h := newHarness(t)
	h.login("overflow@example.com", "password-123", "Overflow")

	// One past the event cap. The events are minimal so this stays under the
	// body cap: the point is which rule fires, and the caps are ordered so the
	// event one is reachable (docs/RUNS.md, "Caps").
	var sb strings.Builder
	sb.WriteString(`{"version":1,"events":[`)
	const n = 120_001
	for i := range n {
		if i > 0 {
			sb.WriteByte(',')
		}
		fmt.Fprintf(&sb, `{"seq":%d}`, i)
	}
	sb.WriteString(`]}`)

	body := validRun()
	body["log"] = json.RawMessage(sb.String())
	resp := h.post("/api/v1/runs", body)
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)
	assert.Equal(t, "too_many_events", decodeInto[errResp](t, resp).Error)
}

func TestIngestScoreVersionMembership(t *testing.T) {
	h := newHarness(t)
	h.login("score@example.com", "password-123", "Score")

	// Every KnownScoreVersions member ({1, 2}) is accepted.
	for _, v := range []int{1, 2} {
		body := validRun()
		body["scoreVersion"] = v
		resp := h.post("/api/v1/runs", body)
		assert.Equal(t, http.StatusAccepted, resp.StatusCode, "scoreVersion %d must be accepted", v)
	}

	// A version outside the set is rejected as unsupported.
	body := validRun()
	body["scoreVersion"] = 3
	resp := h.post("/api/v1/runs", body)
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)
	assert.Equal(t, "unsupported_score_version", decodeInto[errResp](t, resp).Error)
}

func TestIngestInvalidDimensions(t *testing.T) {
	h := newHarness(t)
	h.login("dims@example.com", "password-123", "Dims")

	// Both duration and word count set: reject.
	both := validRun()
	both["wordCount"] = 50
	resp := h.post("/api/v1/runs", both)
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)
	assert.Equal(t, "invalid_dimensions", decodeInto[errResp](t, resp).Error)

	// Neither set: reject.
	neither := validRun()
	delete(neither, "durationMs")
	resp = h.post("/api/v1/runs", neither)
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)
	assert.Equal(t, "invalid_dimensions", decodeInto[errResp](t, resp).Error)

	// The seeded rule is unchanged by the quote relaxation, and it stays
	// unchanged when the seeded kind is stated EXPLICITLY rather than by
	// omission — the two spellings mean the same thing.
	explicit := validRun()
	delete(explicit, "durationMs")
	explicit["setup"] = json.RawMessage(`{"generation":{"textSource":{"kind":"seeded"}}}`)
	resp = h.post("/api/v1/runs", explicit)
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)
	assert.Equal(t, "invalid_dimensions", decodeInto[errResp](t, resp).Error)
}

// --- quote runs ---

const quoteRunID = "3f2a1b0c-9d8e-4c7b-8a6f-5e4d3c2b1a09"

// quoteRun builds a structurally-valid quote submission: a textSource by
// reference, and NEITHER dimension.
func quoteRun() map[string]any {
	body := validRun()
	delete(body, "durationMs")
	body["mode"] = "quote"
	body["dictHash"] = "e42437c7"
	body["setup"] = json.RawMessage(`{"config":{"mode":"quote"},"generation":{"mode":"quote",` +
		`"textSource":{"kind":"quote","quoteId":"` + quoteRunID + `","quoteHash":"e42437c7"}}}`)
	return body
}

func TestIngestQuoteRunCarriesNoDimension(t *testing.T) {
	h := newHarness(t)
	h.login("quote-dims@example.com", "password-123", "QuoteDims")

	requireStatus(t, h.post("/api/v1/runs", quoteRun()), http.StatusAccepted)

	// A quote's length is a property of the text, so neither number means
	// anything and both are refused.
	withWords := quoteRun()
	withWords["wordCount"] = 16
	resp := h.post("/api/v1/runs", withWords)
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)
	assert.Equal(t, "invalid_dimensions", decodeInto[errResp](t, resp).Error)

	withDuration := quoteRun()
	withDuration["durationMs"] = 15000
	resp = h.post("/api/v1/runs", withDuration)
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)
	assert.Equal(t, "invalid_dimensions", decodeInto[errResp](t, resp).Error)
}

// The text is never submitted. A payload that carries one is refused outright
// rather than accepted-and-ignored: silently dropping it would leave two
// readings of the contract, and the one where the client's copy matters is the
// one that makes a quote score meaningless.
func TestIngestQuoteRunRefusesASubmittedText(t *testing.T) {
	h := newHarness(t)
	h.login("quote-text@example.com", "password-123", "QuoteText")

	body := quoteRun()
	body["setup"] = json.RawMessage(`{"config":{"mode":"quote"},"generation":{"mode":"quote",` +
		`"textSource":{"kind":"quote","quoteId":"` + quoteRunID + `","quoteHash":"e42437c7",` +
		`"text":"Es irrt der Mensch"}}}`)
	resp := h.post("/api/v1/runs", body)
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)
	assert.Equal(t, "quote_text_submitted", decodeInto[errResp](t, resp).Error)

	// Even an empty string is a submitted text: the field must be absent.
	body["setup"] = json.RawMessage(`{"config":{"mode":"quote"},"generation":{"mode":"quote",` +
		`"textSource":{"kind":"quote","quoteId":"` + quoteRunID + `","quoteHash":"e42437c7","text":""}}}`)
	resp = h.post("/api/v1/runs", body)
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)
	assert.Equal(t, "quote_text_submitted", decodeInto[errResp](t, resp).Error)
}

// The reference itself has to be usable as a reference. Whether the quote
// EXISTS is not asked here — ingestion never consults a registry, and the
// worker flags unknown_quote — but an id nothing could ever resolve is a
// structural problem, not a corpus one.
func TestIngestQuoteRunRefusesAnUnusableReference(t *testing.T) {
	h := newHarness(t)
	h.login("quote-ref@example.com", "password-123", "QuoteRef")

	for name, textSource := range map[string]string{
		"quoteId is not a uuid": `{"kind":"quote","quoteId":"7","quoteHash":"e42437c7"}`,
		"quoteId is absent":     `{"kind":"quote","quoteHash":"e42437c7"}`,
		"quoteHash is absent":   `{"kind":"quote","quoteId":"` + quoteRunID + `"}`,
		"kind is unknown":       `{"kind":"beatmap","quoteId":"` + quoteRunID + `"}`,
	} {
		body := quoteRun()
		body["setup"] = json.RawMessage(
			`{"config":{},"generation":{"textSource":` + textSource + `}}`)
		resp := h.post("/api/v1/runs", body)
		require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode, name)
		assert.Equal(t, "invalid_text_source", decodeInto[errResp](t, resp).Error, name)
	}

	// A quote id that is well-formed but unknown is NOT an ingestion problem:
	// it lands, and the worker is what decides it cannot be judged.
	unknown := quoteRun()
	unknown["setup"] = json.RawMessage(`{"config":{},"generation":{"textSource":` +
		`{"kind":"quote","quoteId":"00000000-0000-4000-8000-000000000000","quoteHash":"deadbeef"}}}`)
	requireStatus(t, h.post("/api/v1/runs", unknown), http.StatusAccepted)
}

func TestIngestSeedOutOfRange(t *testing.T) {
	h := newHarness(t)
	h.login("seed@example.com", "password-123", "Seed")

	body := validRun()
	body["seed"] = int64(1) << 33 // > 2^32-1
	resp := h.post("/api/v1/runs", body)
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)
	assert.Equal(t, "seed_out_of_range", decodeInto[errResp](t, resp).Error)
}

// --- word-count mode lands too (exercises the other dimension) ---

func TestIngestWordMode(t *testing.T) {
	h := newHarness(t)
	h.login("words@example.com", "password-123", "Words")

	body := validRun()
	delete(body, "durationMs")
	body["mode"] = "words"
	body["wordCount"] = 50
	resp := h.post("/api/v1/runs", body)
	require.Equal(t, http.StatusAccepted, resp.StatusCode)
}

// --- rate limit ---

func TestIngestRateLimit(t *testing.T) {
	h := newHarness(t, func(o *harnessOpts) { o.runsRateEvery = time.Hour; o.runsRateBurst = 1 })
	h.login("rate@example.com", "password-123", "Rate")

	requireStatus(t, h.post("/api/v1/runs", validRun()), http.StatusAccepted)

	resp := h.post("/api/v1/runs", validRun())
	require.Equal(t, http.StatusTooManyRequests, resp.StatusCode)
	assert.Equal(t, "rate_limited", decodeInto[errResp](t, resp).Error)
}

// --- CASCADE on user deletion ---

func TestCascadeOnUserDeletion(t *testing.T) {
	h := newHarness(t)
	userID := h.login("cascade@example.com", "password-123", "Cascade")
	requireStatus(t, h.post("/api/v1/runs", validRun()), http.StatusAccepted)

	ctx := context.Background()
	uid := uuid.MustParse(userID)

	var before int
	require.NoError(t, h.pool.QueryRow(ctx, `SELECT count(*) FROM runs WHERE user_id=$1`, uid).Scan(&before))
	require.Equal(t, 1, before)

	_, err := h.pool.Exec(ctx, `DELETE FROM users WHERE id=$1`, uid)
	require.NoError(t, err)

	var after int
	require.NoError(t, h.pool.QueryRow(ctx, `SELECT count(*) FROM runs WHERE user_id=$1`, uid).Scan(&after))
	assert.Equal(t, 0, after, "deleting the user must cascade-delete their runs")
}

// --- pagination: stable keyset order, no dupes/skips ---

func TestListPaginationStable(t *testing.T) {
	h := newHarness(t)
	h.login("page@example.com", "password-123", "Pager")

	const total = 5
	posted := make(map[string]bool, total)
	for range total {
		resp := h.post("/api/v1/runs", validRun())
		require.Equal(t, http.StatusAccepted, resp.StatusCode)
		posted[decodeInto[ingestResp](t, resp).ID] = true
	}

	full := h.pageAll(1, total+10)  // single big page
	paged := h.pageAll(2, total+10) // small pages via cursor

	require.Len(t, full, total)
	assert.Equal(t, full, paged, "keyset paging must reproduce the full-scan order exactly")

	// No dupes, no skips: the paged set equals exactly what was posted.
	seen := map[string]bool{}
	for _, id := range paged {
		require.False(t, seen[id], "duplicate id across pages: %s", id)
		seen[id] = true
		require.True(t, posted[id], "unexpected id in page: %s", id)
	}
	assert.Len(t, seen, total)
}

// pageAll walks every page at the given limit and returns the ids in order.
// maxIters guards against an accidental infinite loop.
func (h *harness) pageAll(limit, maxIters int) []string {
	h.t.Helper()
	var ids []string
	cursor := ""
	for range maxIters {
		path := fmt.Sprintf("/api/v1/runs?limit=%d", limit)
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		resp := h.get(path)
		require.Equal(h.t, http.StatusOK, resp.StatusCode)
		page := decodeInto[listResp](h.t, resp)
		for _, r := range page.Runs {
			ids = append(ids, r.ID)
		}
		if page.NextCursor == "" {
			return ids
		}
		cursor = page.NextCursor
	}
	h.t.Fatalf("pagination did not terminate within %d iterations", maxIters)
	return nil
}

// --- ownership: another user's run is not found ---

func TestDetailOwnershipNotFound(t *testing.T) {
	h := newHarness(t)
	h.login("owner@example.com", "password-123", "Owner")
	resp := h.post("/api/v1/runs", validRun())
	require.Equal(t, http.StatusAccepted, resp.StatusCode)
	otherID := decodeInto[ingestResp](t, resp).ID

	// Switch to a different user.
	h.logout()
	h.login("intruder@example.com", "password-123", "Intruder")

	requireStatus(t, h.get("/api/v1/runs/"+otherID), http.StatusNotFound)
	requireStatus(t, h.get("/api/v1/runs/"+otherID+"?log=1"), http.StatusNotFound)

	// The intruder's own listing is empty.
	list := decodeInto[listResp](t, h.get("/api/v1/runs"))
	assert.Empty(t, list.Runs)
}
