//go:build load

package replay

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/dop251/goja"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/typemore/typemore-server/internal/perf"
)

// Zone 2 measures ONE thing: can the worker replay the largest run ingestion
// will ever hand it inside the interrupt budget that bounds a core call
// (TYPEMORE_REPLAY_TIMEOUT, DefaultReplayTimeout = 5 s)? If it cannot, an honest
// player who typed a legal maximum run gets `replay_timeout` — the server
// punishing someone for its own limits, which is the worst failure this
// pipeline has.
const replayZone = "replay-worker"

// measureTimeout is the interrupt budget of the runtime used for MEASURING.
//
// It has to be far above the production budget: a core built at 5 s cannot tell
// you how long a 10 s run takes, only that it was cut off at 5 s, and "5 s" is
// exactly the answer that would hide the finding. The production budget is
// exercised separately, on its own runtime, so the consequence of a miss is
// demonstrated rather than asserted from arithmetic.
const measureTimeout = 10 * time.Minute

// Fixture seeds. Fixed, because a budget that moved because the dice moved
// would be unreadable across runs.
const (
	fixtureSeed   = int64(20260725)
	fixtureLogRNG = uint64(0x7ea51e)
)

// keystrokeMeanMs is the base gap between synthesised keystrokes; the jitter
// below multiplies it by 0.55..1.85. At 55 ms no interval lands under the core's
// 15 ms rollover threshold and even the max-event fixture spans a plausible
// stretch of game time — a plausible log rather than merely a parseable one, so
// the flags being measured are the run's and not the generator's.
const keystrokeMeanMs = 55.0

// loadQuotes is the quote registry these fixtures run against: empty, because
// every fixture here is a SEEDED run. Judge needs a resolver; it never reaches
// this one.
type loadQuotes struct{}

func (loadQuotes) ResolveQuote(context.Context, uuid.UUID) (string, string, bool, error) {
	return "", "", false, nil
}

// realisticMeanMs paces the 60 s baseline so its ~500 keystrokes fill the run
// instead of finishing in half of it and tripping trailing-afk.
const realisticMeanMs = 96.0

const (
	jitterLo   = 0.55
	jitterSpan = 1.30
)

// --- fixtures ----------------------------------------------------------------

// logBuilder turns generated words into an event log. Pluggable so two fixtures
// can hold the event count fixed and vary only how far through the word list the
// log gets — which is how the shape of the cost curve is pinned down.
type logBuilder func(words []string, maxEvents int, meanMs float64, seed uint64) perf.EventLog

// runSpec describes a run to synthesise.
type runSpec struct {
	name       string
	mode       string
	wordCount  int
	durationMs int
	maxEvents  int
	meanMs     float64
	// log defaults to typedLog.
	log logBuilder
}

// fixture is one synthesised run plus the sizes that make its cost readable.
type fixture struct {
	runSpec
	run       PendingRun
	words     int
	events    int
	rawLog    int
	gzipLog   int
	bodyBytes int
	spanMs    int64
}

// coreGeneratedWords drives the vendored generateWords on the worker's own
// runtime.
//
// The words a run is judged against are the CORE's, never Go's. A synthesised
// log has to type the text the core will regenerate from the seed, or every
// keystroke would be an error, the fold would take the mistyped path, and the
// measurement would describe a run nobody can submit.
func coreGeneratedWords(t *testing.T, core *Core, dictHash string, body []byte, seed int64, generation json.RawMessage) []string {
	t.Helper()
	ctx := context.Background()

	dict, err := core.dictionaryValue(ctx, dictHash, body)
	require.NoError(t, err)
	gen, err := core.parseJSON(ctx, string(generation))
	require.NoError(t, err)

	seedCtx := core.rt.NewObject()
	require.NoError(t, seedCtx.Set("seed", seed))
	require.NoError(t, seedCtx.Set("dictVersion", dictHash))
	require.NoError(t, seedCtx.Set("generation", gen))

	res, err := core.call(ctx, core.generateWords, dict, seedCtx)
	require.NoError(t, err)
	value, err := core.unwrapResult(ctx, res)
	require.NoError(t, err)
	obj, ok := value.(*goja.Object)
	require.True(t, ok, "generateWords returned a non-object")

	var words []string
	require.NoError(t, core.rt.ExportTo(obj.Get("words"), &words))
	require.NotEmpty(t, words)
	return words
}

// typedLog synthesises the events a player produces typing `words` correctly:
// one insert per grapheme, one commit per word, stopping at maxEvents.
//
// It stops mid-run on purpose for the large fixtures. The two ingestion caps
// cannot both be saturated by a FINISHED run — 10 000 German words are ~79 000
// keystrokes, short of the 120 000-event cap but well past what a finished run
// needs — so the run that maximises both is a 10 000-word generation abandoned
// partway through. That is a legal run (nothing requires a player to finish),
// and it is the expensive one: the fold cost is a function of event count and
// the generation cost of word count, regardless of whether the last word was
// committed.
func typedLog(words []string, maxEvents int, meanMs float64, seed uint64) perf.EventLog {
	rng := rand.New(rand.NewPCG(seed, 0x5eed))
	events := make([]perf.Event, 0, maxEvents)

	var at float64
	next := func() int64 {
		at += meanMs * (jitterLo + rng.Float64()*jitterSpan)
		return int64(at)
	}

	for _, word := range words {
		for _, ch := range word {
			if len(events) >= maxEvents {
				return perf.EventLog{Version: 1, Events: events}
			}
			events = append(events, perf.Event{Kind: "insert", Seq: len(events) + 1, T: next(), Text: string(ch)})
		}
		if len(events) >= maxEvents {
			break
		}
		events = append(events, perf.Event{Kind: "commit", Seq: len(events) + 1, T: next()})
	}
	return perf.EventLog{Version: 1, Events: events}
}

// churnLog types and immediately deletes a single character, forever. It never
// commits, so the reducer's per-word input array stays one entry long however
// long the log gets.
//
// Paired with typedLog at the same event count it isolates the two halves of the
// per-event cost: the constant work every event pays (state clone, settle,
// reduce, the stats pass) from the work that grows with the number of words
// committed so far. Without that pair, "the curve bends" is a guess about which
// term is responsible.
func churnLog(words []string, maxEvents int, meanMs float64, seed uint64) perf.EventLog {
	rng := rand.New(rand.NewPCG(seed, 0x5eed))
	events := make([]perf.Event, 0, maxEvents)

	first := "a"
	if len(words) > 0 && words[0] != "" {
		r, _ := utf8.DecodeRuneInString(words[0])
		first = string(r)
	}

	var at float64
	for i := range maxEvents {
		at += meanMs * (jitterLo + rng.Float64()*jitterSpan)
		e := perf.Event{Seq: i + 1, T: int64(at)}
		if i%2 == 0 {
			e.Kind, e.Text = "insert", first
		} else {
			e.Kind = "delete"
		}
		events = append(events, e)
	}
	return perf.EventLog{Version: 1, Events: events}
}

// buildFixture generates the words, types them, and packages the result exactly
// as the queue would hand it to the worker.
func buildFixture(t *testing.T, core *Core, reg *Registry, dictHash, lang string, spec runSpec) fixture {
	t.Helper()

	body, ok := reg.Body(dictHash)
	require.True(t, ok, "dictionary %s is not in the registry", dictHash)

	setup := perf.BuildSetup(perf.SetupSpec{
		Mode:       spec.mode,
		WordCount:  spec.wordCount,
		DurationMs: spec.durationMs,
	})
	words := coreGeneratedWords(t, core, dictHash, body, fixtureSeed, perf.MustJSON(setup.Generation))

	build := spec.log
	if build == nil {
		build = typedLog
	}
	log := build(words, spec.maxEvents, spec.meanMs, fixtureLogRNG)
	raw := perf.MustJSON(log)
	gz := perf.Gzip(raw)

	f := fixture{
		runSpec: spec,
		words:   len(words),
		events:  len(log.Events),
		rawLog:  len(raw),
		gzipLog: len(gz),
		run: PendingRun{
			ID:           uuid.New(),
			Seed:         fixtureSeed,
			DictHash:     dictHash,
			ScoreVersion: scoreVersionV2,
			Setup:        perf.MustJSON(setup),
			// The client snapshots are empty objects: this fixture measures the
			// replay path, and a fabricated client total would only decide
			// whether the run comes out accepted or score_mismatch. Both are
			// reached AFTER validateLog and the score recomputation, so the cost
			// measured here is the full cost either way.
			ClientMetrics: json.RawMessage(`{}`),
			ClientScore:   json.RawMessage(`{}`),
			Log:           gz,
		},
	}
	if n := len(log.Events); n > 0 {
		f.spanMs = log.Events[n-1].T - log.Events[0].T
	}

	// The POST body this run would have arrived in, so the measurement can be
	// read against the ingestion body cap.
	payload := map[string]any{
		"mode": spec.mode, "lang": lang, "seed": fixtureSeed, "dictHash": dictHash,
		"scoreVersion": 2, "setup": setup,
		"clientMetrics": map[string]any{"wpm": 100.0, "raw": 100.0, "acc": 1.0},
		"clientScore":   map[string]any{"version": 2, "total": 1234},
		"log":           json.RawMessage(raw),
	}
	if spec.mode == "time" {
		payload["durationMs"] = spec.durationMs
	} else {
		payload["wordCount"] = spec.wordCount
	}
	f.bodyBytes = len(perf.MustJSON(payload))
	return f
}

// describe logs everything about a fixture that a reader of the report needs to
// judge whether the number below it is the number they think it is.
func (f fixture) describe(t *testing.T) {
	t.Helper()
	perf.Report(t, replayZone, f.name+" fixture", fmt.Sprintf(
		"%d words generated, %d events, log %s raw / %s gzip, POST body %s, %.0fs of game time",
		f.words, f.events, perf.MiB(uint64(f.rawLog)), perf.MiB(uint64(f.gzipLog)),
		perf.MiB(uint64(f.bodyBytes)), float64(f.spanMs)/1000))
}

// --- measurement -------------------------------------------------------------

// judgeSamples replays one run n times on the SAME runtime and returns the
// samples plus the heap churn of one replay.
//
// The runtime is warmed first: a worker goroutine lives for the process and
// parses each dictionary once, so the steady state — not the first call — is
// what the timeout has to hold. The allocation figure is taken from that warm-up
// call rather than from an extra one, because at the top of the range a single
// replay costs two minutes and paying for one more would double the suite.
func judgeSamples(t *testing.T, core *Core, reg *Registry, f fixture, n int) (samples []time.Duration, allocBytes, allocs uint64) {
	t.Helper()

	var warm Decision
	allocBytes, allocs = perf.Delta(func() {
		warm = Judge(context.Background(), core, reg, loadQuotes{}, DefaultPolicy(), f.run)
	})
	requireReplayed(t, f, warm)

	samples = make([]time.Duration, 0, n)
	for range n {
		started := time.Now()
		d := Judge(context.Background(), core, reg, loadQuotes{}, DefaultPolicy(), f.run)
		samples = append(samples, time.Since(started))
		requireReplayed(t, f, d)
	}
	return samples, allocBytes, allocs
}

// requireReplayed fails loudly when a fixture did not actually go through the
// whole pipeline. A log the core rejects at event three is cheap, and reporting
// its cost as "the worst case" would be a lie.
func requireReplayed(t *testing.T, f fixture, d Decision) {
	t.Helper()
	var doc validationDoc
	require.NoError(t, json.Unmarshal(d.Validation, &doc))
	require.Equalf(t, verdictValid, doc.Verdict,
		"%s: fixture is not a valid run (%s), so its cost is not the worst case", f.name, doc.Reason)
	require.NotEmpty(t, d.ServerScore, "%s: the score phase did not run", f.name)
}

// phases is the attribution of one replay to the work it is made of.
type phases struct {
	gunzip   time.Duration
	parse    time.Duration
	generate time.Duration
	validate time.Duration
	score    time.Duration
}

// assembleInput mirrors Core.Replay's input assembly.
//
// It is a deliberate copy: measuring through Replay can only ever produce a
// total, and the question this zone has to answer — raise the timeout or lower
// the caps — depends entirely on WHICH phase the seconds are in.
func assembleInput(t *testing.T, core *Core, in Input) (*goja.Object, goja.Value) {
	t.Helper()
	ctx := context.Background()

	var setup setupParts
	require.NoError(t, json.Unmarshal(in.Setup, &setup))
	dict, err := core.dictionaryValue(ctx, in.DictHash, in.DictBody)
	require.NoError(t, err)

	var doc strings.Builder
	fmt.Fprintf(&doc, `{"seed":%d,"dictVersion":%q,"configSnapshot":{"config":`, in.Seed, in.DictHash)
	doc.Write(setup.Config)
	doc.WriteString(`,"generation":`)
	doc.Write(setup.Generation)
	doc.WriteString(`},"declaration":`)
	doc.Write(setup.Declaration)
	doc.WriteString(`,"log":`)
	doc.Write(in.Log)
	doc.WriteString(`}`)

	parsed, err := core.parseJSON(ctx, doc.String())
	require.NoError(t, err)
	obj, ok := parsed.(*goja.Object)
	require.True(t, ok)
	require.NoError(t, obj.Set("dictionary", dict))
	return obj, dict
}

// breakdown times each phase of one replay separately. `generate` is ONE
// generateWords call; Core.Replay pays it twice, once inside validateLog and
// once in the score phase, so `validate` and `score` each already contain a copy
// of it.
func breakdown(t *testing.T, core *Core, reg *Registry, dictHash string, f fixture) phases {
	t.Helper()
	ctx := context.Background()

	body, ok := reg.Body(dictHash)
	require.True(t, ok)

	var p phases

	started := time.Now()
	logJSON, err := gunzip(f.run.Log)
	p.gunzip = time.Since(started)
	require.NoError(t, err)

	in := Input{
		Seed: f.run.Seed, DictHash: dictHash, DictBody: body,
		Setup: f.run.Setup, Log: logJSON, ScoreVersion: f.run.ScoreVersion,
	}

	started = time.Now()
	input, dict := assembleInput(t, core, in)
	p.parse = time.Since(started)

	var setup setupParts
	require.NoError(t, json.Unmarshal(f.run.Setup, &setup))
	started = time.Now()
	coreGeneratedWords(t, core, dictHash, body, f.run.Seed, setup.Generation)
	p.generate = time.Since(started)

	started = time.Now()
	report, err := core.validate(ctx, input)
	p.validate = time.Since(started)
	require.NoError(t, err)
	require.Equal(t, verdictValid, report.Verdict)

	started = time.Now()
	_, err = core.score(ctx, input, f.run.ScoreVersion, dict)
	p.score = time.Since(started)
	require.NoError(t, err)

	perf.Report(t, replayZone, f.name+" phase breakdown", fmt.Sprintf(
		"gunzip %v | JSON.parse %v | generateWords(x1) %v | validateLog %v | scoreV2OfLog %v",
		p.gunzip.Round(time.Millisecond), p.parse.Round(time.Millisecond),
		p.generate.Round(time.Millisecond), p.validate.Round(time.Millisecond),
		p.score.Round(time.Millisecond)))
	return p
}

// testCore builds a worker-equivalent runtime and its registry at the given
// interrupt budget, plus the dictionary every fixture plays against. The golden
// vectors already pin a real published dictionary and its hash; borrowing them
// keeps this file from hard-coding a fingerprint a rotation would invalidate.
func testCore(t *testing.T, timeout time.Duration) (*Core, *Registry, string, string) {
	t.Helper()
	core, err := NewCore(timeout)
	require.NoError(t, err)
	reg, err := NewRegistry(core)
	require.NoError(t, err)
	v := firstVector(t, "words-clean")
	return core, reg, v.Payload.DictHash, v.Payload.Lang
}

// --- the correctness budget --------------------------------------------------

// TestLoadReplayMaxRun is the zone's headline: the largest run the validator can
// be asked to replay, against the 5 s interrupt budget that bounds one core
// call.
//
// Two fixtures, because the two documented ceilings still do not meet — but the
// ordering has flipped, deliberately (docs/RUNS.md, "Caps"). The EVENT cap is
// now the operative one: 120 000 events marshal to ~6.1 MiB, inside the 6.5 MiB
// body cap, so the worker can be handed the full documented log. The two specs
// therefore converge, and both are kept: if a future encoding change makes the
// body cap bind again, the first fixture shrinks below the second and the gap
// is visible in the report instead of silent.
func TestLoadReplayMaxRun(t *testing.T) {
	core, reg, dictHash, lang := testCore(t, measureTimeout)

	submittable := perf.SubmittableEvents(uint64(fixtureSeed))
	specs := []runSpec{{
		name:       fmt.Sprintf("max-submittable (%d events, body ceiling)", submittable),
		mode:       "words",
		wordCount:  perf.MaxWordCount,
		durationMs: perf.MaxDurationMs,
		maxEvents:  submittable,
		meanMs:     keystrokeMeanMs,
	}, {
		name:       fmt.Sprintf("max-events (%d, validator ceiling)", perf.MaxEvents),
		mode:       "words",
		wordCount:  perf.MaxWordCount,
		durationMs: perf.MaxDurationMs,
		maxEvents:  perf.MaxEvents,
		meanMs:     keystrokeMeanMs,
	}}

	for _, spec := range specs {
		f := buildFixture(t, core, reg, dictHash, lang, spec)
		f.describe(t)
		breakdown(t, core, reg, dictHash, f)

		samples, bytes, allocs := judgeSamples(t, core, reg, f, 3)
		perf.Report(t, replayZone, spec.name+" distribution", perf.Summary(samples))
		median := perf.Percentile(samples, 50)
		worst := perf.Percentile(samples, 100)
		perf.Report(t, replayZone, spec.name+" allocations", fmt.Sprintf("%s over %d allocs", perf.MiB(bytes), allocs))

		// The correctness budget. Exceeding it means an honest maximum run is
		// flagged replay_timeout, which blames the player for the server's
		// limits — the one failure mode this pipeline must not have.
		perf.Budget{
			Zone:      replayZone,
			Workload:  spec.name + " — worst sample vs the interrupt budget",
			Limit:     DefaultReplayTimeout,
			Rationale: "over DefaultReplayTimeout the core call is interrupted and an honest run is flagged replay_timeout",
		}.Assert(t, worst)

		// The engineering budget: 2x margin on the median, so a slower CI box,
		// a colder cache or a marginally heavier bundle cannot walk a real run
		// into the timeout.
		perf.Budget{
			Zone:      replayZone,
			Workload:  spec.name + " — median at 2x margin",
			Limit:     DefaultReplayTimeout / 2,
			Rationale: "a 2x margin below the interrupt budget absorbs a slower box without re-tuning the timeout",
		}.Assert(t, median)
	}
}

// TestLoadReplayMaxRunV2Telemetry re-checks the replay-timeout margin for the
// telemetry grammar (log v2): the max-submittable REAL run — typed against the
// core-generated words — with every keystroke bracketed by its down/up pair,
// which is the exact wire shape a v2 client submits. Telemetry folds as
// no-ops, but the goja PARSE and the structural walk scale with TOTAL events
// (~3× the v1 run), which is precisely what "measure, don't assume" means for
// the caps re-derivation (docs/RUNS.md).
func TestLoadReplayMaxRunV2Telemetry(t *testing.T) {
	core, reg, dictHash, lang := testCore(t, measureTimeout)
	f := buildFixture(t, core, reg, dictHash, lang, runSpec{
		name:       "max-submittable v2 (telemetry interleaved)",
		mode:       "words",
		wordCount:  perf.MaxWordCount,
		durationMs: perf.MaxDurationMs,
		maxEvents:  perf.SubmittableEvents(uint64(fixtureSeed)),
		meanMs:     keystrokeMeanMs,
	})
	total := interleaveTelemetry(t, &f)
	require.LessOrEqual(t, total, perf.MaxEventsV2,
		"the interleaved max run exceeds the v2 event cap: the cap model has drifted from the capture shape")
	perf.Report(t, replayZone, "v2 telemetry fixture",
		fmt.Sprintf("%d total events (%d state), gzip %s", total, f.events, perf.MiB(uint64(f.gzipLog))))

	samples, bytes, allocs := judgeSamples(t, core, reg, f, 3)
	perf.Report(t, replayZone, "max v2 telemetry distribution", perf.Summary(samples))
	perf.Report(t, replayZone, "max v2 telemetry allocations", fmt.Sprintf("%s over %d allocs", perf.MiB(bytes), allocs))

	perf.Budget{
		Zone:      replayZone,
		Workload:  "max v2 telemetry — worst sample vs the interrupt budget",
		Limit:     DefaultReplayTimeout,
		Rationale: "over DefaultReplayTimeout the core call is interrupted and an honest v2 run is flagged replay_timeout",
	}.Assert(t, perf.Percentile(samples, 100))
	perf.Budget{
		Zone:      replayZone,
		Workload:  "max v2 telemetry — median at 2x margin",
		Limit:     DefaultReplayTimeout / 2,
		Rationale: "a 2x margin below the interrupt budget absorbs a slower box without re-tuning the timeout",
	}.Assert(t, perf.Percentile(samples, 50))
}

// interleaveTelemetry rewrites a fixture's v1 log as the v2 capture of the
// same keystrokes: down 8 ms before every event, up 25 ms after, seq
// renumbered contiguously — the input adapter's exact shape. Returns the new
// total event count and updates the fixture's log/size fields in place.
func interleaveTelemetry(t *testing.T, f *fixture) int {
	t.Helper()

	zr, err := gzip.NewReader(bytes.NewReader(f.run.Log))
	require.NoError(t, err)
	rawLog, err := io.ReadAll(zr)
	require.NoError(t, err)
	require.NoError(t, zr.Close())

	var log struct {
		Version int               `json:"version"`
		Events  []json.RawMessage `json:"events"`
	}
	require.NoError(t, json.Unmarshal(rawLog, &log))

	out := make([]json.RawMessage, 0, 3*len(log.Events))
	seq := 0
	lastT := int64(0)
	// Clamped stamping keeps `t` monotonic even where the fixture put two
	// state events on the same instant (a commit sharing its keystroke's t).
	stamp := func(kind string, at int64, code string) {
		if at < lastT {
			at = lastT
		}
		lastT = at
		seq++
		out = append(out, json.RawMessage(fmt.Sprintf(
			`{"kind":%q,"seq":%d,"t":%d,"code":%q}`, kind, seq, at, code)))
	}
	for _, raw := range log.Events {
		var e map[string]any
		require.NoError(t, json.Unmarshal(raw, &e))
		at := int64(e["t"].(float64))
		code := "Space"
		if text, ok := e["text"].(string); ok && text != "" {
			if r := []rune(text)[0]; (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				code = "Key" + strings.ToUpper(string(r))
			} else {
				code = "IntlBackslash" // non-ASCII graphemes: a stable stand-in code
			}
		}
		stamp("down", at-8, code)
		if at < lastT {
			at = lastT
		}
		lastT = at
		seq++
		e["seq"] = seq
		e["t"] = at
		reseq, err := json.Marshal(e)
		require.NoError(t, err)
		out = append(out, reseq)
		stamp("up", at+25, code)
	}

	v2, err := json.Marshal(map[string]any{"version": 2, "events": out})
	require.NoError(t, err)
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, err = zw.Write(v2)
	require.NoError(t, err)
	require.NoError(t, zw.Close())

	f.run.Log = buf.Bytes()
	f.rawLog = len(v2)
	f.gzipLog = buf.Len()
	return len(out)
}

// TestLoadReplayMaxRunUnderProductionTimeout demonstrates the consequence of the
// budget above rather than deducing it: the same fixture, on a runtime
// configured exactly as production is, produces whatever it produces and this
// test reports it.
//
// It asserts only the honest-player invariant — a legal run must not come back
// `replay_timeout` — because that is the actual contract. If the run replays in
// time this passes silently; if it does not, the failure names the run the
// server would have punished.
func TestLoadReplayMaxRunUnderProductionTimeout(t *testing.T) {
	build, reg, dictHash, lang := testCore(t, measureTimeout)
	f := buildFixture(t, build, reg, dictHash, lang, runSpec{
		name:       "max-submittable under the shipped 5s timeout",
		mode:       "words",
		wordCount:  perf.MaxWordCount,
		durationMs: perf.MaxDurationMs,
		maxEvents:  perf.SubmittableEvents(uint64(fixtureSeed)),
		meanMs:     keystrokeMeanMs,
	})

	prod, prodReg, _, _ := testCore(t, DefaultReplayTimeout)

	started := time.Now()
	d := Judge(context.Background(), prod, prodReg, loadQuotes{}, DefaultPolicy(), f.run)
	elapsed := time.Since(started)

	var doc validationDoc
	require.NoError(t, json.Unmarshal(d.Validation, &doc))
	perf.Report(t, replayZone, "max-submittable judged at TYPEMORE_REPLAY_TIMEOUT=5s", fmt.Sprintf(
		"%v, status %s, verdict %s, reason %q", elapsed.Round(time.Millisecond), d.Status, doc.Verdict, doc.Reason))

	require.NotEqualf(t, ReasonReplayTimeout, doc.Reason,
		"a legal %d-event run was flagged replay_timeout: the server punished a player for its own caps", f.events)
}

// TestLoadReplayEventScaling is the curve the recommendation is built on: the
// same 10 000-word generation replayed against logs of growing length, so the
// per-event cost can be read off directly.
//
// It matters whether the cost is linear or worse. The reducer rebuilds its
// per-word input array on every keystroke (`setInput` slices the whole array),
// so a log that commits N words pays O(N) per insert — and the fold runs several
// times per replay. A super-linear curve here means capping events is worth far
// more than raising the timeout, and a linear one means the opposite.
func TestLoadReplayEventScaling(t *testing.T) {
	core, reg, dictHash, lang := testCore(t, measureTimeout)

	for _, events := range []int{500, 2_000, 5_000, 10_000, 20_000, 35_000, perf.MaxEvents} {
		f := buildFixture(t, core, reg, dictHash, lang, runSpec{
			name:       fmt.Sprintf("%d events", events),
			mode:       "words",
			wordCount:  perf.MaxWordCount,
			durationMs: perf.MaxDurationMs,
			maxEvents:  events,
			meanMs:     keystrokeMeanMs,
		})

		started := time.Now()
		d := Judge(context.Background(), core, reg, loadQuotes{}, DefaultPolicy(), f.run)
		elapsed := time.Since(started)
		requireReplayed(t, f, d)

		perf.Report(t, replayZone, fmt.Sprintf("scaling: %d events", events), fmt.Sprintf(
			"%v total, %.3fms/event", elapsed.Round(time.Millisecond),
			float64(elapsed.Microseconds())/float64(events)/1000))
	}
}

// TestLoadReplayCostShape attributes the bend in the curve above.
//
// Two logs, the same event count, the same 10 000-word generation, differing in
// one thing: how many words the log commits. `typedLog` walks forward through
// the word list; `churnLog` types and deletes a single character and never
// leaves word zero. Everything that is per-EVENT — the state clone, settle,
// reduce, the stats pass, the interpreter itself — is identical between them.
// The difference is the work that grows with committed words, which is the term
// that decides whether the fix is a lower event cap or a higher timeout.
func TestLoadReplayCostShape(t *testing.T) {
	core, reg, dictHash, lang := testCore(t, measureTimeout)

	const events = 20_000
	shapes := []struct {
		label string
		build logBuilder
	}{
		{"forward through the word list", typedLog},
		{"pinned to word zero (insert/delete churn)", churnLog},
	}

	for _, shape := range shapes {
		f := buildFixture(t, core, reg, dictHash, lang, runSpec{
			name:       shape.label,
			mode:       "words",
			wordCount:  perf.MaxWordCount,
			durationMs: perf.MaxDurationMs,
			maxEvents:  events,
			meanMs:     keystrokeMeanMs,
			log:        shape.build,
		})

		started := time.Now()
		d := Judge(context.Background(), core, reg, loadQuotes{}, DefaultPolicy(), f.run)
		elapsed := time.Since(started)
		requireReplayed(t, f, d)

		perf.Report(t, replayZone, fmt.Sprintf("cost shape at %d events: %s", events, shape.label),
			fmt.Sprintf("%v total, %.3fms/event", elapsed.Round(time.Millisecond),
				float64(elapsed.Microseconds())/float64(events)/1000))
	}
}

// TestLoadReplayGenerationCost isolates the word generation, which Core.Replay
// pays TWICE per run: once inside validateLog (which regenerates the words to
// judge the log against) and once in the score phase. A 10 000-word generation
// that costs X therefore costs 2X per replay before a single event is folded.
func TestLoadReplayGenerationCost(t *testing.T) {
	core, reg, dictHash, lang := testCore(t, measureTimeout)
	body, ok := reg.Body(dictHash)
	require.True(t, ok)

	for _, count := range []int{60, 1_000, perf.MaxWordCount} {
		gen := perf.MustJSON(perf.BuildSetup(perf.SetupSpec{Mode: "words", WordCount: count}).Generation)
		coreGeneratedWords(t, core, dictHash, body, fixtureSeed, gen) // warm

		const reps = 5
		started := time.Now()
		for range reps {
			coreGeneratedWords(t, core, dictHash, body, fixtureSeed, gen)
		}
		each := time.Since(started) / reps
		perf.Report(t, replayZone, fmt.Sprintf("generateWords(%d words)", count),
			fmt.Sprintf("%v per call, %v per replay (paid twice)",
				each.Round(time.Microsecond), (2*each).Round(time.Microsecond)))
	}

	// A 10 000-word generation with a log too short to matter: whatever this
	// costs is the floor under every max-word run, independent of event count.
	f := buildFixture(t, core, reg, dictHash, lang, runSpec{
		name:       "max-words with a 200-event log",
		mode:       "words",
		wordCount:  perf.MaxWordCount,
		durationMs: perf.MaxDurationMs,
		maxEvents:  200,
		meanMs:     keystrokeMeanMs,
	})
	f.describe(t)
	breakdown(t, core, reg, dictHash, f)

	samples, bytes, allocs := judgeSamples(t, core, reg, f, 5)
	perf.Report(t, replayZone, f.name+" distribution", perf.Summary(samples))
	perf.Report(t, replayZone, f.name+" allocations", fmt.Sprintf("%s over %d allocs", perf.MiB(bytes), allocs))
	perf.Budget{
		Zone:      replayZone,
		Workload:  f.name,
		Limit:     DefaultReplayTimeout / 2,
		Rationale: "the generation floor alone must stay inside the same 2x margin as a full run",
	}.Assert(t, perf.Percentile(samples, 50))
}

// TestLoadReplayRealisticRun measures what the worker actually processes all day:
// a 60-second run at a human cadence.
func TestLoadReplayRealisticRun(t *testing.T) {
	core, reg, dictHash, lang := testCore(t, measureTimeout)

	f := buildFixture(t, core, reg, dictHash, lang, runSpec{
		name:       "realistic 60s run",
		mode:       "time",
		durationMs: 60_000,
		maxEvents:  500,
		meanMs:     realisticMeanMs,
	})
	f.describe(t)
	breakdown(t, core, reg, dictHash, f)

	samples, bytes, allocs := judgeSamples(t, core, reg, f, 50)
	perf.Report(t, replayZone, f.name+" distribution", perf.Summary(samples))
	perf.Report(t, replayZone, f.name+" allocations", fmt.Sprintf("%s over %d allocs", perf.MiB(bytes), allocs))

	// The synthesised fixture is cross-checked against a golden vector — a real
	// payload produced by driving the same bundle in Node — so a surprising
	// per-event cost cannot be blamed on the generator.
	v := firstVector(t, "words-clean")
	golden := v.pendingRun(t)
	Judge(context.Background(), core, reg, loadQuotes{}, DefaultPolicy(), golden)
	var log perf.EventLog
	require.NoError(t, json.Unmarshal(v.Payload.Log, &log))
	started := time.Now()
	Judge(context.Background(), core, reg, loadQuotes{}, DefaultPolicy(), golden)
	goldenElapsed := time.Since(started)
	perf.Report(t, replayZone, "golden vector words-clean (cross-check)", fmt.Sprintf(
		"%v for %d events, %.3fms/event", goldenElapsed.Round(time.Microsecond), len(log.Events),
		float64(goldenElapsed.Microseconds())/float64(len(log.Events))/1000))

	// 50 ms at p99 is the budget. It is not derived from the interrupt budget —
	// a typical run has orders of magnitude of headroom there — but from
	// throughput: the worker is single-goroutine by default
	// (TYPEMORE_REPLAY_CONCURRENCY=1), so a per-run cost of C seconds caps the
	// whole server at 1/C runs per second. 50 ms holds the floor at 20 runs/s,
	// which is ~1.7 million runs a day on one worker goroutine, comfortably
	// above any plausible peak. A regression past that is a capacity change and
	// deserves to fail a test rather than be discovered under load.
	perf.Budget{
		Zone:      replayZone,
		Workload:  "realistic 60s run (p99)",
		Limit:     50 * time.Millisecond,
		Rationale: "one worker goroutine at 50ms/run sustains 20 runs/s; slower is a capacity regression, not a timeout risk",
	}.Assert(t, perf.Percentile(samples, 99))
}

// TestLoadReplayThroughput measures the sustained rate of one worker goroutine,
// which is what TYPEMORE_REPLAY_CONCURRENCY=1 actually buys.
func TestLoadReplayThroughput(t *testing.T) {
	core, reg, dictHash, lang := testCore(t, measureTimeout)

	f := buildFixture(t, core, reg, dictHash, lang, runSpec{
		name:       "realistic 60s run",
		mode:       "time",
		durationMs: 60_000,
		maxEvents:  500,
		meanMs:     realisticMeanMs,
	})

	// Distinct ids, because the queue never hands the same run twice and a
	// shared id is the one thing a real batch cannot do.
	const runs = 200
	batch := make([]PendingRun, runs)
	for i := range batch {
		batch[i] = f.run
		batch[i].ID = uuid.New()
	}

	Judge(context.Background(), core, reg, loadQuotes{}, DefaultPolicy(), f.run) // warm

	started := time.Now()
	for i := range batch {
		requireReplayed(t, f, Judge(context.Background(), core, reg, loadQuotes{}, DefaultPolicy(), batch[i]))
	}
	elapsed := time.Since(started)
	rate := float64(runs) / elapsed.Seconds()

	perf.Report(t, replayZone, "single-goroutine throughput (realistic runs)",
		fmt.Sprintf("%.1f runs/s over %d runs in %v", rate, runs, elapsed.Round(time.Millisecond)))

	// The rates the server must keep up with. The assumption is stated so it can
	// be argued with: 10 000 daily active players finishing 20 runs each is
	// 200 000 runs/day. Spread evenly that is 2.3 runs/s; concentrated in a
	// four-hour peak it is 14 runs/s. The queue absorbs bursts, so the average is
	// the survival number and the peak is the latency-of-verdict number.
	const (
		averageRunsPerSecond = 200_000.0 / 86_400.0
		peakRunsPerSecond    = 200_000.0 / (4 * 3600.0)
	)
	perf.Report(t, replayZone, "throughput headroom (10k DAU x 20 runs/day)", fmt.Sprintf(
		"%.1fx the %.1f runs/s daily average; %.2fx the %.1f runs/s 4h peak (%.1f goroutines needed to hold the peak)",
		rate/averageRunsPerSecond, averageRunsPerSecond,
		rate/peakRunsPerSecond, peakRunsPerSecond, peakRunsPerSecond/rate))
}

// TestLoadReplayStructuralWorstCase measures the perf toolkit's synthetic
// max-event payload — 50 000 random keystrokes that do NOT type the generated
// words.
//
// It is here to be compared against the fixtures above, not to stand in for
// them: the reducer refuses the first word once the buffer passes
// target+maxExtraChars, so the fold aborts within tens of events and the run is
// rejected. The cost is one generation plus a very short fold, which makes the
// obvious "50 000 garbage events" fixture the CHEAP case, not the worst one —
// and is exactly why this zone synthesises a log that types the real words.
func TestLoadReplayStructuralWorstCase(t *testing.T) {
	core, reg, dictHash, _ := testCore(t, measureTimeout)

	payload := perf.MaxEventsPayload(uint64(fixtureSeed))
	raw := perf.MustJSON(payload.Log)
	run := PendingRun{
		ID:            uuid.New(),
		Seed:          payload.Seed,
		DictHash:      dictHash,
		ScoreVersion:  scoreVersionV2,
		Setup:         perf.MustJSON(payload.Setup),
		ClientMetrics: json.RawMessage(`{}`),
		ClientScore:   json.RawMessage(`{}`),
		Log:           perf.Gzip(raw),
	}

	d := Judge(context.Background(), core, reg, loadQuotes{}, DefaultPolicy(), run) // warm
	var doc validationDoc
	require.NoError(t, json.Unmarshal(d.Validation, &doc))
	require.Empty(t, d.LastError, "the structural fixture must be judged, not fail to replay")

	const reps = 5
	started := time.Now()
	for range reps {
		Judge(context.Background(), core, reg, loadQuotes{}, DefaultPolicy(), run)
	}
	each := time.Since(started) / reps

	perf.Report(t, replayZone, "structural 50k-event log (perf.MaxEventsPayload)", fmt.Sprintf(
		"%v per replay, log %s raw / %s gzip, status %s, verdict %s (%s)",
		each.Round(time.Millisecond), perf.MiB(uint64(len(raw))), perf.MiB(uint64(len(run.Log))),
		d.Status, doc.Verdict, doc.Reason))

	perf.Budget{
		Zone:      replayZone,
		Workload:  "structural 50k-event log (rejected early by the reducer)",
		Limit:     DefaultReplayTimeout,
		Rationale: "even a log the reducer refuses must not occupy a worker past the interrupt budget",
	}.Assert(t, each)
}
