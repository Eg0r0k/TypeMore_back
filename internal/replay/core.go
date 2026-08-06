package replay

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/dop251/goja"

	"github.com/typemore/typemore-server/internal/replay/policy"
)

// coreBundle is the frontend's src/shared/core compiled to a single IIFE that
// assigns its exports to the global `TypeMoreCore`. It is a checked-in build
// artifact; see corejs/README.md for provenance and `make core-bundle` for the
// exact command that reproduces it.
//
//go:embed corejs/core.bundle.js
var coreBundle string

const (
	// bundleName is the script name goja reports in stack traces.
	bundleName = "core.bundle.js"
	// coreGlobal is the esbuild --global-name the bundle assigns its exports to.
	coreGlobal = "TypeMoreCore"
	// DefaultReplayTimeout bounds one core call when no explicit budget is
	// given. It is the FALLBACK, not the shipped value — production passes
	// cfg.ReplayTimeout — but it had silently diverged: it stayed at 5 s while
	// the config default went to 300 s and then to 45 s, and its comment still
	// claimed a 10k-word run "folds in milliseconds", which the measurements
	// contradicted by four orders of magnitude. A fallback that cannot judge a
	// legal run is a trap for every caller that constructs a Core directly, so
	// it now tracks the config default. See docs/PERFORMANCE.md, zone 2.
	DefaultReplayTimeout = 45 * time.Second
	// TextSourceQuote is the `setup.generation.textSource.kind` that marks a
	// run played on a fixed text from the quote registry. An ABSENT textSource
	// means seeded — which is what keeps EVENT_LOG_VERSION at 1, so nothing
	// here may require the key to exist.
	TextSourceQuote = "quote"
	// quoteDictName is the `name` of the stand-in dictionary object a quote run
	// is replayed with. The bundle reports it back as `dictName` and nothing
	// downstream reads it, so the only requirement is that it is honest.
	quoteDictName = "quote"
)

// bundleSHA is the SHA-256 of the vendored bundle, computed once. Every replay
// decision records it, so a later rebalance can tell exactly which code judged
// which run.
var bundleSHA = func() string {
	sum := sha256.Sum256([]byte(coreBundle))
	return hex.EncodeToString(sum[:])
}()

// BundleSHA returns the SHA-256 (hex) of the vendored core bundle.
func BundleSHA() string { return bundleSHA }

// BuildInfo is what the vendored bundle reports about itself: the @typemore/core
// package version and the wire-format constants as EXPORTED CONSTANTS of the
// bundle (read out of the goja runtime, so they cannot disagree with the code
// that judges runs), plus the provenance trailer the package build appends
// (frontend git sha + dirty flag — the facts `make core-bundle` refuses on).
//
// Zero values mean "the bundle predates the field", never an error: the server
// must keep loading older vendored bundles, and the startup log is where a
// missing identity gets noticed.
type BuildInfo struct {
	// PackageVersion is the bundle's CORE_PACKAGE_VERSION export (injected by
	// the package build; a source-consumption fallback reads "0.0.0-dev").
	PackageVersion string
	// EventLogVersion / TelemetryLogVersion are EVENT_LOG_VERSION and
	// EVENT_LOG_VERSION_TELEMETRY — the v1 base every stored run is pinned to,
	// and the v2 keystroke-telemetry format.
	EventLogVersion     int64
	TelemetryLogVersion int64
	// GitSHA / GitDirty come from the build-info trailer line, not from an
	// export: the sha moves with every frontend commit, and as an export it
	// would churn the export-parity guard for no informational gain.
	GitSHA   string
	GitDirty bool
}

// bundleTrailerPrefix marks the machine-readable last line of the bundle; the
// same prefix is parsed by `make core-bundle` and the freshness gate.
const bundleTrailerPrefix = "//# typemore-core-build "

// bundleBuildInfo parses the provenance trailer once. Absent or malformed
// trailers yield the zero value — see BuildInfo on why that is not an error.
var bundleBuildInfo = func() (info struct {
	GitSHA   string `json:"gitSha"`
	GitDirty bool   `json:"gitDirty"`
}) {
	trimmed := strings.TrimRight(coreBundle, "\n")
	if i := strings.LastIndexByte(trimmed, '\n'); i >= 0 && strings.HasPrefix(trimmed[i+1:], bundleTrailerPrefix) {
		// Best-effort by design; a bad trailer leaves the zero value.
		_ = json.Unmarshal([]byte(strings.TrimPrefix(trimmed[i+1:], bundleTrailerPrefix)), &info)
	}
	return info
}()

// readBuildInfo lifts the identity constants off the bundle's export object.
// Missing exports (an older vendored bundle) leave zero values.
func readBuildInfo(exports *goja.Object) BuildInfo {
	info := BuildInfo{GitSHA: bundleBuildInfo.GitSHA, GitDirty: bundleBuildInfo.GitDirty}
	if v := exports.Get("CORE_PACKAGE_VERSION"); v != nil {
		if s, ok := v.Export().(string); ok {
			info.PackageVersion = s
		}
	}
	if v := exports.Get("EVENT_LOG_VERSION"); v != nil {
		info.EventLogVersion = v.ToInteger()
	}
	if v := exports.Get("EVENT_LOG_VERSION_TELEMETRY"); v != nil {
		info.TelemetryLogVersion = v.ToInteger()
	}
	return info
}

// ErrReplayTimeout is returned when a core call exceeded the interrupt budget.
// It is a *decision input*, not a crash: the run is flagged and retried later.
var ErrReplayTimeout = errors.New("replay: core call timed out")

// CoreError is a structured failure the core itself reported (a neverthrow Err),
// as opposed to a JS exception. Kind is 'DictVersionMismatch' or
// 'GenerationFailed' (validate.ts ValidationErrorKind).
type CoreError struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

func (e *CoreError) Error() string { return "replay: core error " + e.Kind + ": " + e.Message }

// Core is one instance of the vendored game core running inside goja.
//
// A goja.Runtime is NOT safe for concurrent use. The worker gives every
// goroutine its own Core; mu is the belt-and-braces guarantee that an accidental
// share degrades to serialisation instead of memory corruption.
type Core struct {
	mu      sync.Mutex
	rt      *goja.Runtime
	timeout time.Duration

	// buildInfo is the bundle's self-reported identity, read once at NewCore.
	buildInfo BuildInfo

	// Bound core exports. Names verified against the bundle — see
	// TestBundleExportsAreBound.
	dictVersion   goja.Callable
	generateWords goja.Callable
	validateLog   goja.Callable
	scoreOfLog    goja.Callable
	scoreV2OfLog  goja.Callable
	scoreV3OfLog  goja.Callable
	// charObservationsOf feeds the user_keyboard_profile projection: per typed
	// character — presses, errors, inter-key intervals (docs/PROFILE.md).
	charObservationsOf goja.Callable

	// Host intrinsics used to move plain JSON across the boundary.
	jsonParse     goja.Callable
	jsonStringify goja.Callable

	// dicts caches parsed dictionary objects by dict_hash. Dictionaries are
	// immutable and shared by every run of a language, so parsing a 20 KB word
	// list once per runtime instead of once per run is free correctness-wise.
	dicts map[string]goja.Value

	// quoteDict is the stand-in `dictionary` argument a QUOTE run is replayed
	// with. A quote run has no dictionary at all — its words are the quote's
	// own bytes — but generateWords and validateLog still take one, and the
	// bundle's quote branch reads exactly one field off it: `dict.name`.
	//
	// Its word list is deliberately EMPTY rather than plausible. If the quote
	// branch ever stopped firing (a bundle regression, a textSource the core
	// no longer recognises), the seeded path would hit `EmptyDictionary` and
	// the run would be flagged — instead of quietly replaying a quote run
	// against ten random German nouns and rejecting an honest log.
	quoteDict goja.Value
}

// BuildInfo returns the loaded bundle's self-reported identity. The server
// logs it next to BundleSHA at startup, so "which core judged this run" is
// answerable from the boot log alone.
func (c *Core) BuildInfo() BuildInfo { return c.buildInfo }

// NewCore evaluates the vendored bundle and binds the exports the pipeline
// calls. A failure here means the vendored artifact is broken, was built with a
// syntax goja cannot parse, or no longer exports something the server needs —
// all startup failures, never first-request surprises.
func NewCore(timeout time.Duration) (*Core, error) {
	if timeout <= 0 {
		timeout = DefaultReplayTimeout
	}
	rt := goja.New()
	if _, err := rt.RunScript(bundleName, coreBundle); err != nil {
		return nil, fmt.Errorf("replay: evaluate core bundle: %w", err)
	}

	exports, ok := rt.Get(coreGlobal).(*goja.Object)
	if !ok || exports == nil {
		return nil, fmt.Errorf("replay: core bundle did not define global %q", coreGlobal)
	}

	c := &Core{rt: rt, timeout: timeout, dicts: make(map[string]goja.Value), buildInfo: readBuildInfo(exports)}
	bind := func(dst *goja.Callable, name string) error {
		fn, ok := goja.AssertFunction(exports.Get(name))
		if !ok {
			return fmt.Errorf("replay: core bundle exports no %s function", name)
		}
		*dst = fn
		return nil
	}
	for _, b := range []struct {
		dst  *goja.Callable
		name string
	}{
		{&c.dictVersion, "dictVersion"},
		{&c.generateWords, "generateWords"},
		{&c.validateLog, "validateLog"},
		{&c.scoreOfLog, "scoreOfLog"},
		{&c.scoreV2OfLog, "scoreV2OfLog"},
		{&c.scoreV3OfLog, "scoreV3OfLog"},
		{&c.charObservationsOf, "charObservationsOf"},
	} {
		if err := bind(b.dst, b.name); err != nil {
			return nil, err
		}
	}

	jsonObj, ok := rt.Get("JSON").(*goja.Object)
	if !ok {
		return nil, errors.New("replay: runtime has no JSON intrinsic")
	}
	if c.jsonParse, ok = goja.AssertFunction(jsonObj.Get("parse")); !ok {
		return nil, errors.New("replay: JSON.parse is not callable")
	}
	if c.jsonStringify, ok = goja.AssertFunction(jsonObj.Get("stringify")); !ok {
		return nil, errors.New("replay: JSON.stringify is not callable")
	}

	// Built once per runtime: it is two constant fields, and a quote run must
	// not pay a JSON.parse for them.
	quoteDict := rt.NewObject()
	if err := errors.Join(
		quoteDict.Set("name", quoteDictName),
		quoteDict.Set("words", rt.NewArray()),
	); err != nil {
		return nil, fmt.Errorf("replay: build the quote stand-in dictionary: %w", err)
	}
	c.quoteDict = quoteDict
	return c, nil
}

// callOn runs one core function under a context-driven interrupt, with an
// explicit `this`. goja executes synchronously on this goroutine, so the only
// way to stop a runaway script is a watchdog that flips the runtime's interrupt
// flag.
//
// Three failure modes collapse into Go errors here and none of them panics:
// a JS `throw` (goja returns *goja.Exception), an interrupt (the deadline), and
// an interpreter-level panic (stack overflow).
func (c *Core) callOn(ctx context.Context, this goja.Value, fn goja.Callable, args ...goja.Value) (v goja.Value, err error) {
	callCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	// The watchdog must be fully retired before ClearInterrupt, otherwise a
	// deadline that fires as the call returns would leave the flag set and kill
	// the NEXT call instead.
	returned := make(chan struct{})
	watchdogDone := make(chan struct{})
	go func() {
		defer close(watchdogDone)
		select {
		case <-callCtx.Done():
			c.rt.Interrupt(callCtx.Err())
		case <-returned:
		}
	}()
	defer func() {
		close(returned)
		<-watchdogDone
		c.rt.ClearInterrupt()
		if r := recover(); r != nil {
			v, err = nil, fmt.Errorf("replay: core panicked: %v", r)
		}
	}()

	res, callErr := fn(this, args...)
	if callErr != nil {
		var interrupted *goja.InterruptedError
		if errors.As(callErr, &interrupted) || callCtx.Err() != nil {
			return nil, ErrReplayTimeout
		}
		return nil, fmt.Errorf("replay: core threw: %w", callErr)
	}
	return res, nil
}

// call invokes a plain (unbound) core export.
func (c *Core) call(ctx context.Context, fn goja.Callable, args ...goja.Value) (goja.Value, error) {
	return c.callOn(ctx, goja.Undefined(), fn, args...)
}

// parseJSON turns a JSON document into a native JS value inside the runtime.
// Building the input as JSON in Go and letting the engine parse it is both
// faster and less surprising than reflecting Go maps across the boundary.
func (c *Core) parseJSON(ctx context.Context, doc string) (goja.Value, error) {
	return c.call(ctx, c.jsonParse, c.rt.ToValue(doc))
}

// stringifyJSON serialises a JS value back to JSON. Numbers survive exactly:
// JS emits the shortest decimal that round-trips to the same IEEE double, and
// Go parses it back to that same double — which is what makes the server's
// score comparable to the client's *exactly*, with no epsilon.
func (c *Core) stringifyJSON(ctx context.Context, v goja.Value) ([]byte, error) {
	out, err := c.call(ctx, c.jsonStringify, v)
	if err != nil {
		return nil, err
	}
	if goja.IsUndefined(out) || goja.IsNull(out) {
		return nil, errors.New("replay: core returned a non-serialisable value")
	}
	return []byte(out.String()), nil
}

// DictVersion returns the core's FNV-1a fingerprint of a word list — the same
// `dict_hash` the client computes, byte-for-byte, because it is literally the
// same code. Never reimplement this in Go: a drifting hash silently invalidates
// every stored run.
func (c *Core) DictVersion(words []string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// A native JS array, not a wrapped Go slice: the bundle is free to use any
	// Array method it likes, now or after the next core change.
	items := make([]any, len(words))
	for i, w := range words {
		items[i] = w
	}
	res, err := c.call(context.Background(), c.dictVersion, c.rt.NewArray(items...))
	if err != nil {
		return "", err
	}
	s, ok := res.Export().(string)
	if !ok {
		return "", fmt.Errorf("replay: dictVersion returned %T, want string", res.Export())
	}
	return s, nil
}

// QuoteTargets returns the TARGET WORD LIST the core derives from a quote's
// bytes — the same tokens the browser typed against, produced by the same
// vendored bundle, for the text exactly as the registry holds it.
//
// It exists so the server can be checked on the thing that breaks FIRST. A
// quote's text is turned into targets by one rule and one only (the core's
// `generateWords`: every line break is rewritten to "\n ", then the text is
// split on U+0020 and empty tokens are dropped). If that rule were ever
// reimplemented in Go — or if anything on the way in "tidied" a typographic
// quote, a no-break space or an em dash — the server would disagree with the
// client on SOME texts and on no others, and every run on those quotes would
// fail its fold with a score mismatch. Comparing targets is what makes that
// failure say "the text disagrees" instead of "the score disagrees".
//
// Note what it does NOT do: it never normalises. `normalize.ts` is an INPUT
// adapter — it decides which grapheme a keystroke STORES — and by design it
// never transforms the expected text (see its header: "changes what is WRITTEN,
// never how anything is judged"). The bytes below are the registry's, verbatim.
func (c *Core) QuoteTargets(ctx context.Context, text string) ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// The hash the quote branch checks the text against. Computed by the bundle,
	// like every other digest in this system; c.dictVersion is called directly
	// rather than through DictVersion because the mutex is already held.
	hashValue, err := c.call(ctx, c.dictVersion, c.rt.NewArray(text))
	if err != nil {
		return nil, err
	}
	hash, ok := hashValue.Export().(string)
	if !ok {
		return nil, fmt.Errorf("replay: dictVersion returned %T, want string", hashValue.Export())
	}

	// A quote run's generation config, spelled the way a stored setup spells it.
	// `quoteId` is a placeholder: the core only ever puts it in an error message,
	// and this call resolves nothing.
	var doc strings.Builder
	doc.WriteString(`{"seed":0,"dictVersion":`)
	writeJSONString(&doc, hash)
	doc.WriteString(`,"generation":{"mode":"quote","length":0,"punctuation":false,` +
		`"numbers":false,"randomCase":false,"reverse":false,"textSource":` +
		`{"kind":"quote","quoteId":"00000000-0000-0000-0000-000000000000","quoteHash":`)
	writeJSONString(&doc, hash)
	doc.WriteString(`,"text":`)
	writeJSONString(&doc, text)
	doc.WriteString(`}}}`)

	seedContext, err := c.parseJSON(ctx, doc.String())
	if err != nil {
		return nil, err
	}
	generated, err := c.call(ctx, c.generateWords, c.quoteDict, seedContext)
	if err != nil {
		return nil, err
	}
	value, err := c.unwrapResult(ctx, generated)
	if err != nil {
		return nil, err
	}
	genObj, ok := value.(*goja.Object)
	if !ok {
		return nil, errors.New("replay: generateWords returned a non-object")
	}
	raw, err := c.stringifyJSON(ctx, genObj.Get("words"))
	if err != nil {
		return nil, err
	}
	var words []string
	if err := json.Unmarshal(raw, &words); err != nil {
		return nil, fmt.Errorf("replay: decode quote targets: %w", err)
	}
	return words, nil
}

// unwrapResult splits a neverthrow Result into its Ok value or its Err payload.
// The core returns Results from generateWords and validateLog; nothing else in
// this package knows what a Result looks like.
func (c *Core) unwrapResult(ctx context.Context, v goja.Value) (goja.Value, error) {
	obj, ok := v.(*goja.Object)
	if !ok {
		return nil, fmt.Errorf("replay: expected a Result object, got %s", v.ExportType())
	}
	isErr, ok := goja.AssertFunction(obj.Get("isErr"))
	if !ok {
		return nil, errors.New("replay: value is not a neverthrow Result (no isErr)")
	}
	// isErr() reads `this`: neverthrow's Err delegates to `!this.isOk()`.
	flag, err := c.callOn(ctx, obj, isErr)
	if err != nil {
		return nil, err
	}
	if !flag.ToBoolean() {
		return obj.Get("value"), nil
	}
	raw, err := c.stringifyJSON(ctx, obj.Get("error"))
	if err != nil {
		return nil, err
	}
	var ce CoreError
	if err := json.Unmarshal(raw, &ce); err != nil {
		return nil, fmt.Errorf("replay: decode core error: %w", err)
	}
	return nil, &ce
}

// dictionaryValue returns the parsed dictionary object for a hash, caching it
// for the lifetime of this runtime. The core never mutates a dictionary, so one
// parsed copy is safely shared across every run of that language.
func (c *Core) dictionaryValue(ctx context.Context, dictHash string, body []byte) (goja.Value, error) {
	if v, ok := c.dicts[dictHash]; ok {
		return v, nil
	}
	v, err := c.parseJSON(ctx, string(body))
	if err != nil {
		return nil, err
	}
	c.dicts[dictHash] = v
	return v, nil
}

// Input is one run as the pipeline needs it. Every jsonb field arrives
// exactly as the client submitted it and is handed to the core untouched — the
// server models none of these shapes.
//
// The one exception is Quote, and it is an exception on purpose: the bytes a
// quote run was typed against are read back from the registry by id, never
// taken from the submission.
type Input struct {
	Seed int64
	// DictHash is the run's claimed dictionary fingerprint, and doubles as the
	// `dictVersion` the core checks the regenerated words against. IGNORED for
	// a quote run: there is no dictionary, and the digest that matters is the
	// resolved text's (see Quote).
	DictHash string
	// DictBody is the dictionary file as the registry stores it. Nil for a
	// quote run.
	DictBody []byte
	// Quote is the SERVER-RESOLVED fixed text for a quote run, or nil for a
	// seeded one. Resolved exactly once per run, by Judge, before the core is
	// entered — note that Replay drives generateWords TWICE per judgement
	// (validateLog does its own pass, then score does another), so anything
	// resolved per generateWords call would be resolved twice and could resolve
	// to two different answers under a concurrent re-import.
	Quote *QuoteText
	// Setup is the submitted {config, generation, declaration} snapshot.
	Setup json.RawMessage
	// Log is the submitted {version, events} EventLog.
	Log json.RawMessage
	// ScoreVersion routes the score recomputation
	// (1 → scoreOfLog, 2 → scoreV2OfLog, 3 → scoreV3OfLog).
	ScoreVersion int16
	// CanariesArmed asks validateLog to run its canary detectors for this run.
	//
	// FALSE (the zero value) is the pre-canary contract, byte for byte: the
	// core's disarmed report is bit-identical to the report it produced before
	// the detectors existed, which is what keeps every stored run, every golden
	// vector and every re-judgement of history reproducible. Set only from
	// CanariesArmedAt — the epoch gate — and never from a per-call default.
	CanariesArmed bool
}

// QuoteText is one quote as the registry holds it: the bytes the run was
// actually typed against, and the digest the registry stores for them.
//
// Hash is the registry's own `text_hash`, NOT the client's `quoteHash` — Judge
// has already refused the run if the two disagree, so by the time a QuoteText
// exists there is exactly one hash left in play.
type QuoteText struct {
	Text string
	Hash string
}

// Flag is one scored plausibility flag from validateLog.
//
// An ALIAS, not a definition: the type belongs to internal/replay/policy,
// because a Judge must be writable against that package alone and this package
// imports it. The alias keeps every existing caller — and the JSON that lands in
// validation.flags — exactly as it was.
type Flag = policy.Flag

// Result is what the core says about a run. Metrics and Score are the raw
// JSON the core produced (never re-encoded by Go), so what lands in the database
// is bit-for-bit what the same bundle produces in the browser.
type Result struct {
	Verdict string `json:"verdict"`
	Reason  string `json:"reason,omitempty"`
	Flags   []Flag `json:"flags"`
	// Metrics is the server-recomputed Metrics object; nil when the verdict is
	// invalid (the core reports zeroes and the numbers are meaningless).
	Metrics json.RawMessage `json:"-"`
	// Score is the server-recomputed ScoreResult; nil when the verdict is invalid.
	Score json.RawMessage `json:"-"`
	// CharObservations is the keyboard projection's input, extracted by the
	// core from the same replay (nil when the verdict is invalid — a refused
	// log's keystrokes are not evidence of anything).
	CharObservations []CharObservation `json:"-"`
}

// CharObservation mirrors the core's CharObservation (shared/core/keyboard.ts):
// one typed character's aggregates over a single run.
type CharObservation struct {
	Char          string  `json:"char"`
	Presses       int64   `json:"presses"`
	Errors        int64   `json:"errors"`
	IntervalSumMs float64 `json:"intervalSumMs"`
	IntervalCount int64   `json:"intervalCount"`
}

// setupParts is the only piece of the submitted setup the pipeline has to look
// at, and it looks at it structurally only: the sub-objects are forwarded to
// the core verbatim.
type setupParts struct {
	Config      json.RawMessage `json:"config"`
	Generation  json.RawMessage `json:"generation"`
	Declaration json.RawMessage `json:"declaration"`
}

// Replay recomputes one run through the core: validate the log against words
// regenerated from its own seed, then recompute the score with the formula
// version the run was submitted under.
//
// The composition lives in Go, but every computation is the bundle's:
// generateWords, validateLog and the scoreOfLog/scoreV2OfLog/scoreV3OfLog folds
// are the same functions the browser calls. Nothing here re-derives a metric, a
// multiplier, or a hash.
func (c *Core) Replay(ctx context.Context, in Input) (Result, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	var setup setupParts
	if err := json.Unmarshal(in.Setup, &setup); err != nil {
		return Result{}, fmt.Errorf("replay: setup is not an object: %w", err)
	}
	if len(setup.Config) == 0 || len(setup.Generation) == 0 {
		return Result{}, errors.New("replay: setup is missing config or generation")
	}

	// A quote run has no dictionary and no seeded regeneration: its words ARE
	// the registry's bytes, and the digest the core checks them against is
	// dictVersion([text]) — the registry's own text_hash. The submitted
	// dict_hash is deliberately not consulted, so a client that got it wrong
	// cannot break a run whose text the server already resolved by id.
	dictionary, dictVersion := c.quoteDict, ""
	if in.Quote != nil {
		dictVersion = in.Quote.Hash
	} else {
		parsedDict, err := c.dictionaryValue(ctx, in.DictHash, in.DictBody)
		if err != nil {
			return Result{}, err
		}
		dictionary, dictVersion = parsedDict, in.DictHash
	}

	// One JSON.parse for everything that came from the client. The dictionary is
	// attached afterwards from the (cached) parsed copy.
	var doc strings.Builder
	doc.WriteString(`{"seed":`)
	fmt.Fprintf(&doc, "%d", in.Seed)
	doc.WriteString(`,"dictVersion":`)
	writeJSONString(&doc, dictVersion)
	doc.WriteString(`,"configSnapshot":{"config":`)
	doc.Write(setup.Config)
	doc.WriteString(`,"generation":`)
	doc.Write(setup.Generation)
	doc.WriteString(`},"declaration":`)
	if len(setup.Declaration) == 0 {
		doc.WriteString(`{}`)
	} else {
		doc.Write(setup.Declaration)
	}
	doc.WriteString(`,"log":`)
	doc.Write(in.Log)
	// Written unconditionally, including the `false` a pre-epoch run carries:
	// the core's own default IS false, so the two agree, and a key that is
	// always present is one fewer shape for the bundle to branch on.
	doc.WriteString(`,"canariesArmed":`)
	if in.CanariesArmed {
		doc.WriteString(`true`)
	} else {
		doc.WriteString(`false`)
	}
	doc.WriteString(`}`)

	parsed, err := c.parseJSON(ctx, doc.String())
	if err != nil {
		return Result{}, err
	}
	input, ok := parsed.(*goja.Object)
	if !ok {
		return Result{}, errors.New("replay: assembled input did not parse to an object")
	}
	if err := input.Set("dictionary", dictionary); err != nil {
		return Result{}, fmt.Errorf("replay: attach dictionary: %w", err)
	}
	if in.Quote != nil {
		// The resolved text is injected into the PARSED input, not spliced into
		// the JSON above, for the same reason the dictionary is: it lands once,
		// on the single object both generateWords calls read their generation
		// config out of. There is no second copy to keep in step, and the
		// client's own `text` — refused at ingestion — could not reach here
		// even if a stored row carried one, because this overwrites it.
		if err := setQuoteText(input, in.Quote.Text); err != nil {
			return Result{}, err
		}
	}

	report, err := c.validate(ctx, input)
	if err != nil {
		return Result{}, err
	}
	if report.Verdict != verdictValid {
		// An invalid log has no meaningful numbers: the core returns zeroed
		// metrics and scoring a log the reducer rejected is nonsense.
		return report, nil
	}

	score, err := c.score(ctx, input, in.ScoreVersion, dictionary)
	if err != nil {
		return Result{}, err
	}
	report.Score = score

	// The keyboard observations for the user_keyboard_profile projection —
	// extracted HERE, while the log is already parsed in this runtime, so the
	// aggregates never require replaying a log anywhere else. Valid logs only:
	// an invalid log returned above, and its keystrokes are not evidence of
	// anything.
	observations, err := c.charObservations(ctx, input, dictionary)
	if err != nil {
		return Result{}, err
	}
	report.CharObservations = observations
	return report, nil
}

// charObservations calls the core's charObservationsOf over the same words and
// events the verdict was computed from, and decodes the per-character rows.
func (c *Core) charObservations(ctx context.Context, input *goja.Object, dictionary goja.Value) ([]CharObservation, error) {
	snapshot, ok := input.Get("configSnapshot").(*goja.Object)
	if !ok {
		return nil, errors.New("replay: configSnapshot is not an object")
	}
	seedContext := c.rt.NewObject()
	if err := errors.Join(
		seedContext.Set("seed", input.Get("seed")),
		seedContext.Set("dictVersion", input.Get("dictVersion")),
		seedContext.Set("generation", snapshot.Get("generation")),
	); err != nil {
		return nil, fmt.Errorf("replay: build seed context: %w", err)
	}
	generated, err := c.call(ctx, c.generateWords, dictionary, seedContext)
	if err != nil {
		return nil, err
	}
	value, err := c.unwrapResult(ctx, generated)
	if err != nil {
		return nil, err
	}
	genObj, ok := value.(*goja.Object)
	if !ok {
		return nil, errors.New("replay: generateWords returned a non-object")
	}
	logObj, ok := input.Get("log").(*goja.Object)
	if !ok {
		return nil, errors.New("replay: log is not an object")
	}
	coreCtx := c.rt.NewObject()
	if err := errors.Join(
		coreCtx.Set("config", snapshot.Get("config")),
		coreCtx.Set("words", genObj.Get("words")),
	); err != nil {
		return nil, fmt.Errorf("replay: build observation context: %w", err)
	}
	rows, err := c.call(ctx, c.charObservationsOf, coreCtx, logObj.Get("events"))
	if err != nil {
		return nil, err
	}
	raw, err := c.stringifyJSON(ctx, rows)
	if err != nil {
		return nil, err
	}
	var out []CharObservation
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("replay: decode char observations: %w", err)
	}
	return out, nil
}

// validate runs validateLog and lifts its report into Go, keeping the metrics
// as the core's own JSON.
func (c *Core) validate(ctx context.Context, input *goja.Object) (Result, error) {
	res, err := c.call(ctx, c.validateLog, input)
	if err != nil {
		return Result{}, err
	}
	value, err := c.unwrapResult(ctx, res)
	if err != nil {
		return Result{}, err
	}
	report, ok := value.(*goja.Object)
	if !ok {
		return Result{}, errors.New("replay: validateLog returned a non-object report")
	}

	raw, err := c.stringifyJSON(ctx, report)
	if err != nil {
		return Result{}, err
	}
	var out Result
	if err := json.Unmarshal(raw, &out); err != nil {
		return Result{}, fmt.Errorf("replay: decode validation report: %w", err)
	}
	if out.Flags == nil {
		out.Flags = []Flag{}
	}
	if out.Verdict == verdictValid {
		metrics, err := c.stringifyJSON(ctx, report.Get("metrics"))
		if err != nil {
			return Result{}, err
		}
		out.Metrics = metrics
	}
	return out, nil
}

// score regenerates the run's words and recomputes the score with the formula
// the run was submitted under. The words never cross into Go: generateWords
// hands back a JS array that is passed straight into the scorer.
func (c *Core) score(ctx context.Context, input *goja.Object, scoreVersion int16, dictionary goja.Value) (json.RawMessage, error) {
	snapshot, ok := input.Get("configSnapshot").(*goja.Object)
	if !ok {
		return nil, errors.New("replay: configSnapshot is not an object")
	}
	config := snapshot.Get("config")
	generation := snapshot.Get("generation")

	// makeSeedContext is trivial data assembly ({seed, dictVersion, generation})
	// and the hash is already known — building it here avoids a sixth binding
	// whose only job would be to allocate the same three fields.
	seedContext := c.rt.NewObject()
	if err := errors.Join(
		seedContext.Set("seed", input.Get("seed")),
		seedContext.Set("dictVersion", input.Get("dictVersion")),
		seedContext.Set("generation", generation),
	); err != nil {
		return nil, fmt.Errorf("replay: build seed context: %w", err)
	}

	generated, err := c.call(ctx, c.generateWords, dictionary, seedContext)
	if err != nil {
		return nil, err
	}
	value, err := c.unwrapResult(ctx, generated)
	if err != nil {
		return nil, err
	}
	genObj, ok := value.(*goja.Object)
	if !ok {
		return nil, errors.New("replay: generateWords returned a non-object")
	}
	words := genObj.Get("words")

	logObj, ok := input.Get("log").(*goja.Object)
	if !ok {
		return nil, errors.New("replay: log is not an object")
	}
	events := logObj.Get("events")

	setup := c.rt.NewObject()
	if err := errors.Join(setup.Set("config", config), setup.Set("words", words)); err != nil {
		return nil, fmt.Errorf("replay: build score setup: %w", err)
	}

	var result goja.Value
	switch scoreVersion {
	case scoreVersionV1:
		result, err = c.call(ctx, c.scoreOfLog, events, setup)
	case scoreVersionV2:
		// v2 adds the mod layer: the multiplier is derived from the generation
		// config (verifiable mods) and the declaration (view-only, on trust).
		if err := setup.Set("generation", generation); err != nil {
			return nil, fmt.Errorf("replay: build score setup: %w", err)
		}
		result, err = c.call(ctx, c.scoreV2OfLog, events, setup, input.Get("declaration"))
	case scoreVersionV3:
		// v3 keeps v2's setup shape whole; the formula difference (ime replaces
		// score) lives entirely inside the bundle's fold.
		if err := setup.Set("generation", generation); err != nil {
			return nil, fmt.Errorf("replay: build score setup: %w", err)
		}
		result, err = c.call(ctx, c.scoreV3OfLog, events, setup, input.Get("declaration"))
	default:
		return nil, fmt.Errorf("replay: unsupported score version %d", scoreVersion)
	}
	if err != nil {
		return nil, err
	}
	return c.stringifyJSON(ctx, result)
}

// setQuoteText plants the registry's bytes on the parsed input's
// generation.textSource, which is where the bundle's quoteOf() looks for them.
//
// The path is ASSERTED, never created. A run only reaches here because Judge
// found a quote textSource in the same setup bytes; a node missing now means
// the two readings disagreed, and a run must not be judged on a shape the
// server had to invent.
func setQuoteText(input *goja.Object, text string) error {
	snapshot, ok := input.Get("configSnapshot").(*goja.Object)
	if !ok {
		return errors.New("replay: configSnapshot is not an object")
	}
	generation, ok := snapshot.Get("generation").(*goja.Object)
	if !ok {
		return errors.New("replay: setup.generation is not an object")
	}
	source, ok := generation.Get("textSource").(*goja.Object)
	if !ok {
		return errors.New("replay: setup.generation.textSource is not an object")
	}
	if err := source.Set("text", text); err != nil {
		return fmt.Errorf("replay: inject the resolved quote text: %w", err)
	}
	return nil
}

// writeJSONString appends a JSON string literal. json.Marshal of a string never
// fails, so the error is structurally impossible.
func writeJSONString(dst *strings.Builder, s string) {
	b, _ := json.Marshal(s)
	dst.Write(b)
}
