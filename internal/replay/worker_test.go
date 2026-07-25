package replay

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- golden vectors ---------------------------------------------------------

// vector is one testdata file: a real POST /runs payload plus what the worker
// must decide about it. See testdata/README.md for how they are produced.
type vector struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Expect      struct {
		Status  string `json:"status"`
		Verdict string `json:"verdict"`
	} `json:"expect"`
	Payload            vectorPayload `json:"payload"`
	RejectedDispatches int           `json:"rejectedDispatches"`
}

type vectorPayload struct {
	Mode          string          `json:"mode"`
	Lang          string          `json:"lang"`
	Seed          int64           `json:"seed"`
	DictHash      string          `json:"dictHash"`
	ScoreVersion  int16           `json:"scoreVersion"`
	Setup         json.RawMessage `json:"setup"`
	ClientMetrics json.RawMessage `json:"clientMetrics"`
	ClientScore   json.RawMessage `json:"clientScore"`
	Log           json.RawMessage `json:"log"`
}

func loadVectors(t *testing.T) []vector {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("testdata", "vectors", "*.json"))
	require.NoError(t, err)
	require.NotEmpty(t, paths, "no golden vectors: run `node internal/replay/testdata/generate.mjs`")

	out := make([]vector, 0, len(paths))
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		require.NoError(t, err)
		var v vector
		require.NoError(t, json.Unmarshal(raw, &v), p)
		out = append(out, v)
	}
	return out
}

func gzipJSON(t *testing.T, raw json.RawMessage) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, err := zw.Write(raw)
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	return buf.Bytes()
}

func (v vector) pendingRun(t *testing.T) PendingRun {
	t.Helper()
	return PendingRun{
		ID:            uuid.New(),
		Seed:          v.Payload.Seed,
		DictHash:      v.Payload.DictHash,
		ScoreVersion:  v.Payload.ScoreVersion,
		Setup:         v.Payload.Setup,
		ClientMetrics: v.Payload.ClientMetrics,
		ClientScore:   v.Payload.ClientScore,
		Log:           gzipJSON(t, v.Payload.Log),
	}
}

// --- test rig ---------------------------------------------------------------

// fakeQueue is an in-memory Queue: it hands out the runs it was given and keeps
// every decision, so worker behaviour can be asserted without a database.
type fakeQueue struct {
	mu        sync.Mutex
	pending   []PendingRun
	decisions map[uuid.UUID]Decision
	batches   int
}

func newFakeQueue(runs ...PendingRun) *fakeQueue {
	return &fakeQueue{pending: runs, decisions: make(map[uuid.UUID]Decision)}
}

func (q *fakeQueue) ProcessBatch(ctx context.Context, limit int32, decide func(context.Context, PendingRun) Decision) (int, error) {
	q.mu.Lock()
	q.batches++
	n := min(int(limit), len(q.pending))
	batch := q.pending[:n]
	q.pending = q.pending[n:]
	q.mu.Unlock()

	for i := range batch {
		run := &batch[i]
		d := decide(ctx, *run)
		q.mu.Lock()
		q.decisions[run.ID] = d
		q.mu.Unlock()
	}
	return n, nil
}

func (q *fakeQueue) decision(t *testing.T, id uuid.UUID) Decision {
	t.Helper()
	q.mu.Lock()
	defer q.mu.Unlock()
	d, ok := q.decisions[id]
	require.True(t, ok, "no decision recorded for run %s", id)
	return d
}

func testWorker(t *testing.T, q Queue) (*Worker, *Core) {
	t.Helper()
	core, err := NewCore(DefaultReplayTimeout)
	require.NoError(t, err)
	reg, err := NewRegistry(core)
	require.NoError(t, err)
	w := NewWorker(q, reg, WorkerConfig{BatchSize: 50}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return w, core
}

// judgeOne runs a single run through the worker's decision path.
func judgeOne(t *testing.T, run PendingRun) Decision {
	t.Helper()
	q := newFakeQueue(run)
	w, _ := testWorker(t, q)
	n, err := w.RunBatch(context.Background(), mustCore(t, DefaultReplayTimeout), slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	require.Equal(t, 1, n)
	return q.decision(t, run.ID)
}

func mustCore(t *testing.T, timeout time.Duration) *Core {
	t.Helper()
	core, err := NewCore(timeout)
	require.NoError(t, err)
	return core
}

// numbers pulls the comparable numeric fields out of a ScoreResult / Metrics
// object so a mismatch names the field instead of dumping two JSON blobs.
func numbers(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	require.NotEmpty(t, raw)
	var m map[string]any
	require.NoError(t, json.Unmarshal(raw, &m))
	return m
}

// --- the contract: goja reproduces V8, exactly -------------------------------

// Every golden vector was scored by the SAME bundle running in Node. Replaying
// it in goja must produce identical numbers — not close, identical. An epsilon
// here would hide precisely the drift this worker exists to detect.
func TestGoldenVectorsReplayBitExact(t *testing.T) {
	for _, v := range loadVectors(t) {
		t.Run(v.Name, func(t *testing.T) {
			run := v.pendingRun(t)
			d := judgeOne(t, run)

			require.Equal(t, v.Expect.Status, d.Status, "validation: %s", d.Validation)
			assert.Equal(t, bundleSHA, d.BundleSHA)
			assert.Empty(t, d.LastError)
			assert.Zero(t, d.Attempts)

			var doc validationDoc
			require.NoError(t, json.Unmarshal(d.Validation, &doc))
			assert.Equal(t, v.Expect.Verdict, doc.Verdict)
			assert.Empty(t, doc.Reason)
			assert.Nil(t, doc.Divergence)

			// Score: every numeric field, compared exactly.
			client := numbers(t, v.Payload.ClientScore)
			server := numbers(t, d.ServerScore)
			for field, want := range client {
				assert.Equal(t, want, server[field], "score.%s", field)
			}
			assert.Equal(t, len(client), len(server), "score has extra/missing fields: %s", d.ServerScore)

			// Metrics: the three the client reports, compared exactly. The
			// server's object carries more (consistency, chars, spaces…), which
			// the client never sends.
			clientMetrics := numbers(t, v.Payload.ClientMetrics)
			serverMetrics := numbers(t, d.ServerMetrics)
			assert.Equal(t, clientMetrics["wpm"], serverMetrics["wpm"], "metrics.wpm")
			assert.Equal(t, clientMetrics["raw"], serverMetrics["raw"], "metrics.raw")
			assert.Equal(t, clientMetrics["acc"], serverMetrics["accuracy"], "metrics.accuracy")
		})
	}
}

// The vector set has to keep covering what it was built to cover; a silent
// regeneration that drops the interesting cases would leave the suite green and
// blind.
func TestGoldenVectorsCoverTheContractSurface(t *testing.T) {
	vectors := loadVectors(t)
	require.GreaterOrEqual(t, len(vectors), 3, "the brief asks for 3-5 real logs")

	var sawTime, sawWords, sawMods, sawRejectedBackspace, sawV1, sawV2, sawTypos bool
	for _, v := range vectors {
		switch v.Payload.Mode {
		case "time":
			sawTime = true
		case "words":
			sawWords = true
		}
		switch v.Payload.ScoreVersion {
		case 1:
			sawV1 = true
		case 2:
			sawV2 = true
		}
		var setup struct {
			Generation struct {
				Punctuation bool `json:"punctuation"`
				Numbers     bool `json:"numbers"`
				RandomCase  bool `json:"randomCase"`
			} `json:"generation"`
		}
		require.NoError(t, json.Unmarshal(v.Payload.Setup, &setup))
		if setup.Generation.Punctuation || setup.Generation.Numbers || setup.Generation.RandomCase {
			sawMods = true
		}
		if v.RejectedDispatches > 0 {
			sawRejectedBackspace = true
		}
		if acc := numbers(t, v.Payload.ClientMetrics)["acc"]; acc != float64(1) {
			sawTypos = true
		}
	}
	assert.True(t, sawTime, "no time-mode vector")
	assert.True(t, sawWords, "no words-mode vector")
	assert.True(t, sawMods, "no text-mods vector")
	assert.True(t, sawRejectedBackspace, "no rejected-dispatch (seq-hole) vector")
	assert.True(t, sawV1, "no scoreVersion 1 vector")
	assert.True(t, sawV2, "no scoreVersion 2 vector")
	assert.True(t, sawTypos, "no imperfect-accuracy vector")
}

// --- tampering --------------------------------------------------------------

func firstVector(t *testing.T, name string) vector {
	t.Helper()
	vectors := loadVectors(t)
	for i := range vectors {
		if vectors[i].Name == name {
			return vectors[i]
		}
	}
	t.Fatalf("golden vector %q not found", name)
	return vector{}
}

// Editing a single event breaks the log's own consistency, and the reducer says
// so: rejected, with the core's reason recorded verbatim.
func TestEditedEventIsRejected(t *testing.T) {
	v := firstVector(t, "words-clean")

	// Punch a hole in the seq numbering — the exact shape a hand-edited log has.
	var log struct {
		Version int               `json:"version"`
		Events  []json.RawMessage `json:"events"`
	}
	require.NoError(t, json.Unmarshal(v.Payload.Log, &log))
	require.Greater(t, len(log.Events), 10)
	log.Events = append(log.Events[:5], log.Events[6:]...)
	edited, err := json.Marshal(log)
	require.NoError(t, err)
	v.Payload.Log = edited

	d := judgeOne(t, v.pendingRun(t))
	require.Equal(t, StatusRejected, d.Status)
	assert.Nil(t, d.ServerScore, "a rejected log's numbers are meaningless and must not be stored")
	assert.Nil(t, d.ServerMetrics)

	var doc validationDoc
	require.NoError(t, json.Unmarshal(d.Validation, &doc))
	assert.Equal(t, verdictInvalid, doc.Verdict)
	assert.Contains(t, doc.Reason, "seq", "the core's own reason must survive: %s", doc.Reason)
}

// Replaying a valid log but with an inflated client score is the headline case:
// flagged, both numbers kept, nothing rejected.
func TestInflatedClientScoreIsFlagged(t *testing.T) {
	v := firstVector(t, "words-clean")

	var score map[string]any
	require.NoError(t, json.Unmarshal(v.Payload.ClientScore, &score))
	honest := score["total"].(float64)
	score["total"] = honest * 10
	tampered, err := json.Marshal(score)
	require.NoError(t, err)
	v.Payload.ClientScore = tampered

	d := judgeOne(t, v.pendingRun(t))
	require.Equal(t, StatusFlagged, d.Status)

	var doc validationDoc
	require.NoError(t, json.Unmarshal(d.Validation, &doc))
	assert.Equal(t, verdictValid, doc.Verdict, "the LOG is fine; only the reported number is not")
	assert.Equal(t, ReasonScoreMismatch, doc.Reason)
	require.NotNil(t, doc.Divergence)
	assert.Equal(t, "total", doc.Divergence.Field)
	require.NotNil(t, doc.Divergence.Client)
	assert.Equal(t, honest*10, *doc.Divergence.Client)
	assert.Equal(t, honest, doc.Divergence.Server)

	// The server's own numbers are still recorded: they are the correction.
	assert.Equal(t, honest, numbers(t, d.ServerScore)["total"])
}

// A one-part-in-a-million nudge to a metric is still a mismatch: the tolerance
// is 1e-9, not "about right".
func TestNudgedClientMetricIsFlagged(t *testing.T) {
	v := firstVector(t, "words-clean")

	var metrics map[string]any
	require.NoError(t, json.Unmarshal(v.Payload.ClientMetrics, &metrics))
	honest := metrics["wpm"].(float64)
	metrics["wpm"] = honest + 1e-6
	tampered, err := json.Marshal(metrics)
	require.NoError(t, err)
	v.Payload.ClientMetrics = tampered

	d := judgeOne(t, v.pendingRun(t))
	require.Equal(t, StatusFlagged, d.Status)

	var doc validationDoc
	require.NoError(t, json.Unmarshal(d.Validation, &doc))
	assert.Equal(t, ReasonMetricMismatch, doc.Reason)
	require.NotNil(t, doc.Divergence)
	assert.Equal(t, "wpm", doc.Divergence.Field)
	assert.Equal(t, honest, doc.Divergence.Server)
}

// Noise below the tolerance is NOT a mismatch — the two sides may take
// different-but-equivalent routes through JSON.
func TestMetricNoiseWithinToleranceIsAccepted(t *testing.T) {
	v := firstVector(t, "words-clean")

	var metrics map[string]any
	require.NoError(t, json.Unmarshal(v.Payload.ClientMetrics, &metrics))
	metrics["wpm"] = metrics["wpm"].(float64) + 1e-12
	tampered, err := json.Marshal(metrics)
	require.NoError(t, err)
	v.Payload.ClientMetrics = tampered

	d := judgeOne(t, v.pendingRun(t))
	assert.Equal(t, StatusAccepted, d.Status, "validation: %s", d.Validation)
}

// A dictionary the registry has never published cannot be replayed — but the
// run is NOT the player's fault: it may simply predate a rotation.
func TestUnknownDictIsFlaggedNeverRejected(t *testing.T) {
	v := firstVector(t, "words-clean")
	v.Payload.DictHash = "deadbeef"

	run := v.pendingRun(t)
	d := judgeOne(t, run)
	require.Equal(t, StatusFlagged, d.Status)
	assert.NotEqual(t, StatusRejected, d.Status)

	var doc validationDoc
	require.NoError(t, json.Unmarshal(d.Validation, &doc))
	assert.Equal(t, verdictError, doc.Verdict)
	assert.Equal(t, ReasonUnknownDict, doc.Reason)
	assert.Contains(t, d.LastError, "deadbeef")
	assert.Zero(t, d.Attempts, "an unpublished dictionary is not a retryable replay failure")
}

// A run whose score_version the server cannot route is a replay error, not a
// silent accept: no formula, no verdict.
func TestUnknownScoreVersionIsFlagged(t *testing.T) {
	v := firstVector(t, "words-clean")
	run := v.pendingRun(t)
	run.ScoreVersion = 99

	d := judgeOne(t, run)
	require.Equal(t, StatusFlagged, d.Status)

	var doc validationDoc
	require.NoError(t, json.Unmarshal(d.Validation, &doc))
	assert.Equal(t, ReasonReplayError, doc.Reason)
	assert.Contains(t, d.LastError, "score version 99")
	assert.Equal(t, int16(1), d.Attempts)
}

// Score version routing has to actually switch formulas, and the only way to
// see that is a run whose mod multiplier is not 1.
func TestScoreVersionRouting(t *testing.T) {
	// With mods active, v2 multiplies and v1 does not — so claiming the wrong
	// version changes the total, and the worker notices.
	mods := firstVector(t, "words-mods")
	require.Equal(t, int16(2), mods.Payload.ScoreVersion)

	v2 := judgeOne(t, mods.pendingRun(t))
	require.Equal(t, StatusAccepted, v2.Status, "validation: %s", v2.Validation)
	v2Score := numbers(t, v2.ServerScore)
	assert.Equal(t, float64(2), v2Score["version"])
	assert.Greater(t, v2Score["modMultiplier"], float64(1), "this vector exists to have mods")

	asV1 := mods.pendingRun(t)
	asV1.ScoreVersion = 1
	v1 := judgeOne(t, asV1)
	v1Score := numbers(t, v1.ServerScore)
	assert.Equal(t, float64(1), v1Score["version"])
	assert.NotContains(t, v1Score, "modMultiplier", "scoreV1 has no mod layer")
	assert.Less(t, v1Score["total"], v2Score["total"], "the mod multiplier must be gone")
	assert.Equal(t, StatusFlagged, v1.Status, "a v2 total claimed as v1 no longer matches")

	// Without mods the two formulas collapse onto the same total (the core's own
	// regression invariant), so the same run is accepted under either version.
	clean := firstVector(t, "words-clean")
	cleanV2 := judgeOne(t, clean.pendingRun(t))
	require.Equal(t, StatusAccepted, cleanV2.Status)

	cleanAsV1 := clean.pendingRun(t)
	cleanAsV1.ScoreVersion = 1
	cleanV1 := judgeOne(t, cleanAsV1)
	assert.Equal(t, StatusAccepted, cleanV1.Status, "validation: %s", cleanV1.Validation)
	assert.Equal(t,
		numbers(t, cleanV2.ServerScore)["total"],
		numbers(t, cleanV1.ServerScore)["total"])
}

// --- resilience -------------------------------------------------------------

// A pathological submission must cost one worker one timeout, then be flagged —
// never wedge the loop, never take the process down.
func TestPathologicalLogTimesOutAndTheLoopStaysHealthy(t *testing.T) {
	v := firstVector(t, "words-clean")

	// A words-mode run asking for a hundred million words: generation alone
	// cannot finish inside the budget.
	var setup map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(v.Payload.Setup, &setup))
	setup["generation"] = json.RawMessage(
		`{"mode":"words","length":100000000,"punctuation":true,"numbers":true,"randomCase":true,"reverse":true}`)
	bloated, err := json.Marshal(setup)
	require.NoError(t, err)

	poison := v.pendingRun(t)
	poison.Setup = bloated
	healthy := v.pendingRun(t)

	q := newFakeQueue(poison, healthy)
	w, _ := testWorker(t, q)
	// A short budget keeps the test quick; the mechanism is identical at 5s.
	core := mustCore(t, 300*time.Millisecond)

	n, err := w.RunBatch(context.Background(), core, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err, "one poisonous run must not fail the batch")
	require.Equal(t, 2, n)

	bad := q.decision(t, poison.ID)
	require.Equal(t, StatusFlagged, bad.Status)
	var doc validationDoc
	require.NoError(t, json.Unmarshal(bad.Validation, &doc))
	assert.Equal(t, ReasonReplayTimeout, doc.Reason)
	assert.Equal(t, int16(1), bad.Attempts, "a timeout is retryable, so it counts")

	// The very next run in the same batch, on the same runtime, is fine: the
	// interrupt flag was cleared, not left armed.
	good := q.decision(t, healthy.ID)
	assert.Equal(t, StatusAccepted, good.Status, "validation: %s", good.Validation)
}

// A log the core cannot even parse is a replay error, and the run comes back
// with an incremented attempt count rather than a verdict it did not earn.
func TestMalformedSetupIsFlaggedAsReplayError(t *testing.T) {
	v := firstVector(t, "words-clean")
	run := v.pendingRun(t)
	run.Setup = json.RawMessage(`{"config":null,"generation":null,"declaration":{}}`)

	d := judgeOne(t, run)
	require.Equal(t, StatusFlagged, d.Status)
	var doc validationDoc
	require.NoError(t, json.Unmarshal(d.Validation, &doc))
	assert.Equal(t, verdictError, doc.Verdict)
	assert.Equal(t, ReasonReplayError, doc.Reason)
	assert.NotEmpty(t, d.LastError)
	assert.Equal(t, int16(1), d.Attempts)
}

// Attempts accumulate across retries rather than resetting, so an operator can
// see a run that keeps failing.
func TestAttemptsAccumulate(t *testing.T) {
	v := firstVector(t, "words-clean")
	run := v.pendingRun(t)
	run.Setup = json.RawMessage(`{"config":null,"generation":null,"declaration":{}}`)
	run.Attempts = 3

	d := judgeOne(t, run)
	assert.Equal(t, int16(4), d.Attempts)
}

// --- goja host --------------------------------------------------------------

// The names bound in NewCore have to exist in the bundle. This is the check
// that turns "the bundle changed" into a startup failure instead of a
// mysterious nil call.
func TestBundleExportsAreBound(t *testing.T) {
	core := mustCore(t, DefaultReplayTimeout)
	for _, fn := range []struct {
		name string
		got  any
	}{
		{"dictVersion", core.dictVersion},
		{"generateWords", core.generateWords},
		{"validateLog", core.validateLog},
		{"scoreOfLog", core.scoreOfLog},
		{"scoreV2OfLog", core.scoreV2OfLog},
	} {
		assert.NotNil(t, fn.got, "core export %q is not bound", fn.name)
	}
}

// The interrupt must be per-call: a runtime that timed out once has to keep
// working, or one bad run would poison every run after it.
func TestInterruptDoesNotLeakToTheNextCall(t *testing.T) {
	core := mustCore(t, 150*time.Millisecond)
	v := firstVector(t, "words-clean")
	var setup setupParts
	require.NoError(t, json.Unmarshal(v.Payload.Setup, &setup))

	body, ok := registryForTest(t).Body(v.Payload.DictHash)
	require.True(t, ok)
	in := Input{
		Seed:         v.Payload.Seed,
		DictHash:     v.Payload.DictHash,
		DictBody:     body,
		Setup:        v.Payload.Setup,
		Log:          v.Payload.Log,
		ScoreVersion: v.Payload.ScoreVersion,
	}

	poisoned := in
	poisoned.Setup = json.RawMessage(fmt.Sprintf(
		`{"config":%s,"generation":{"mode":"words","length":100000000,"punctuation":true,"numbers":true,"randomCase":true,"reverse":true},"declaration":{}}`,
		setup.Config))
	_, err := core.Replay(context.Background(), poisoned)
	require.ErrorIs(t, err, ErrReplayTimeout)

	res, err := core.Replay(context.Background(), in)
	require.NoError(t, err, "the runtime must be usable again after an interrupt")
	assert.Equal(t, verdictValid, res.Verdict)
}

func registryForTest(t *testing.T) *Registry {
	t.Helper()
	reg, err := NewRegistry(mustCore(t, DefaultReplayTimeout))
	require.NoError(t, err)
	return reg
}

// The bundle digest is what ties a verdict to the code that produced it.
func TestBundleSHAIsStableAndRecorded(t *testing.T) {
	assert.Len(t, BundleSHA(), 64)
	assert.Equal(t, BundleSHA(), bundleSHA)

	v := firstVector(t, "words-clean")
	d := judgeOne(t, v.pendingRun(t))
	assert.Equal(t, BundleSHA(), d.BundleSHA)
}
