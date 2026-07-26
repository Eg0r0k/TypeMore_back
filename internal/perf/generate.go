package perf

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"strings"
)

// Ingestion ceilings, mirrored from internal/runs (docs/RUNS.md). The zone-2
// and zone-5 generators produce payloads AT these limits, because "the largest
// thing a client may legally send" is the only interesting size: anything the
// server accepts, it must be able to process.
const (
	MaxBodyBytes  = 13 << 19 // 6.5 MiB
	MaxEvents     = 120_000
	MaxWordCount  = 10_000
	MaxDurationMs = 3_600_000
)

// Realistic-run parameters. A human at ~100 wpm types ~8.3 characters a second,
// so a 15-second run is ~125 keystrokes and a 60-second run ~500. Intervals are
// jittered around the mean: a fixed interval would raise the core's
// uniform-intervals and zero-variance flags and produce a fixture that says more
// about the plausibility checks than about throughput (docs/REPLAY.md).
const (
	humanWPM          = 100
	charsPerWord      = 5
	jitterMinFraction = 0.55
	jitterMaxFraction = 1.85
)

// Event is one entry of an EventLog as the wire format defines it
// (docs/RUNS.md). Kept as a plain struct here so generators can build logs
// without importing the runs domain (which would make the perf package a
// dependency of the thing it measures).
type Event struct {
	Kind string `json:"kind"`
	Seq  int    `json:"seq"`
	T    int64  `json:"t"`
	Text string `json:"text,omitempty"`
}

// EventLog is the submitted log envelope.
type EventLog struct {
	Version int     `json:"version"`
	Events  []Event `json:"events"`
}

// LogSpec describes a log to generate.
type LogSpec struct {
	// Events is how many events to emit.
	Events int
	// Seed makes a generated log reproducible; two calls with the same spec
	// produce byte-identical output, which is what lets a benchmark compare
	// runs instead of comparing dice.
	Seed uint64
	// MeanIntervalMs overrides the human default (0 = derive from humanWPM).
	MeanIntervalMs float64
	// TextLen is the character length of each insert's text. 1 is a keystroke;
	// larger values model a paste (and trip multi-grapheme-insert).
	TextLen int
}

// GenerateLog builds a plausible event log of the requested size: strictly
// increasing seq from 1, monotonic jittered timestamps, single-character
// inserts. It satisfies every structural rule in docs/RUNS.md, so it is a log
// the server will actually accept rather than reject in validation and never
// measure.
func GenerateLog(spec LogSpec) EventLog {
	if spec.Events <= 0 {
		spec.Events = 1
	}
	if spec.TextLen <= 0 {
		spec.TextLen = 1
	}
	mean := spec.MeanIntervalMs
	if mean <= 0 {
		mean = 1000.0 / (humanWPM * charsPerWord / 60.0)
	}

	rng := rand.New(rand.NewPCG(spec.Seed, 0x5eed))
	letters := []rune("abcdefghijklmnopqrstuvwxyz")
	text := strings.Repeat("a", spec.TextLen)

	events := make([]Event, 0, spec.Events)
	var t float64
	for i := range spec.Events {
		t += mean * (jitterMinFraction + rng.Float64()*(jitterMaxFraction-jitterMinFraction))
		e := Event{Kind: "insert", Seq: i + 1, T: int64(t)}
		if spec.TextLen == 1 {
			e.Text = string(letters[rng.IntN(len(letters))])
		} else {
			e.Text = text
		}
		events = append(events, e)
	}
	return EventLog{Version: 1, Events: events}
}

// MaxLegalLog is the largest log the EVENT cap allows: exactly MaxEvents events.
//
// Note that this is NOT a submittable payload — see MaxLegalPayload. It is the
// worst case the *validator* declares, and the replay worker's cost is a
// function of event count, so zone 2 measures against it deliberately.
func MaxLegalLog(seed uint64) EventLog {
	return GenerateLog(LogSpec{Events: MaxEvents, Seed: seed})
}

// SubmittableEvents is the largest event count whose full POST body fits under
// MaxBodyBytes.
//
// The two caps do not MEET, and are not meant to. The EVENT cap is the
// operative one: it is sized above the largest run the game permits on ANY
// published dictionary — a full MaxWordCount css_code run with punctuation
// measures 108 274 events — and the body cap sits above what that many
// single-character events encode to, so it only ever catches a payload that is
// fat for some other reason (a paste: one insert carrying many graphemes).
//
// Both are now reachable by real play, which is the property the old pair
// lacked: 50 000 events could never be submitted at all, because 2 MiB ran out
// first at 39 915.
//
// Computed rather than hardcoded: the number moves if the event encoding does,
// and a stale constant would silently stop measuring the boundary.
func SubmittableEvents(seed uint64) int {
	lo, hi := 1, MaxEvents
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if len(MustJSON(payloadWithEvents(mid, seed))) <= MaxBodyBytes {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return lo
}

func payloadWithEvents(events int, seed uint64) IngestPayload {
	return BuildPayload(PayloadSpec{
		Setup: SetupSpec{Mode: "words", WordCount: MaxWordCount, DurationMs: MaxDurationMs},
		Log:   LogSpec{Events: events, Seed: seed},
	})
}

// Setup is the CoreConfig+GenerationConfig snapshot a run carries.
type Setup struct {
	Config      map[string]any `json:"config"`
	Generation  map[string]any `json:"generation"`
	Declaration map[string]any `json:"declaration"`
}

// SetupSpec describes the run configuration to snapshot.
type SetupSpec struct {
	Mode        string // "time" | "words"
	DurationMs  int
	WordCount   int
	Difficulty  string
	Nospace     bool
	MinWPM      int
	Punctuation bool
	Numbers     bool
	RandomCase  bool
	Reverse     bool
	Blind       bool
	Fading      bool
	Flashlight  bool
}

// BuildSetup renders a SetupSpec into the snapshot shape the core replays
// against.
func BuildSetup(s SetupSpec) Setup {
	if s.Mode == "" {
		s.Mode = "time"
	}
	if s.Difficulty == "" {
		s.Difficulty = "normal"
	}
	length := s.WordCount
	if s.Mode == "time" {
		length = 0
	}
	return Setup{
		Config: map[string]any{
			"mode": s.Mode, "durationMs": s.DurationMs, "maxExtraChars": 20,
			"difficulty": s.Difficulty, "nospace": s.Nospace, "minWpm": s.MinWPM,
		},
		Generation: map[string]any{
			"mode": s.Mode, "length": length, "punctuation": s.Punctuation,
			"numbers": s.Numbers, "randomCase": s.RandomCase, "reverse": s.Reverse,
		},
		Declaration: map[string]any{
			"blind": s.Blind, "fading": s.Fading, "flashlight": s.Flashlight,
		},
	}
}

// IngestPayload is the POST /runs body.
type IngestPayload struct {
	Mode          string          `json:"mode"`
	DurationMs    *int            `json:"durationMs,omitempty"`
	WordCount     *int            `json:"wordCount,omitempty"`
	Lang          string          `json:"lang"`
	Seed          int64           `json:"seed"`
	DictHash      string          `json:"dictHash"`
	ScoreVersion  int             `json:"scoreVersion"`
	Setup         Setup           `json:"setup"`
	ClientMetrics map[string]any  `json:"clientMetrics"`
	ClientScore   map[string]any  `json:"clientScore"`
	Log           json.RawMessage `json:"log"`
}

// PayloadSpec describes an ingestion body to generate.
type PayloadSpec struct {
	Setup    SetupSpec
	Log      LogSpec
	Lang     string
	DictHash string
	Seed     int64
}

// BuildPayload renders a full POST /runs body. The client-reported numbers are
// plausible but arbitrary: ingestion is structural and never checks them, and
// zone 5 measures the ingestion path, not the verdict.
func BuildPayload(p PayloadSpec) IngestPayload {
	if p.Lang == "" {
		p.Lang = "german"
	}
	if p.DictHash == "" {
		p.DictHash = "804728e8"
	}
	log := GenerateLog(p.Log)
	raw, err := json.Marshal(log)
	if err != nil {
		panic(fmt.Sprintf("perf: marshal generated log: %v", err)) // structurally impossible
	}

	out := IngestPayload{
		Mode: p.Setup.Mode, Lang: p.Lang, Seed: p.Seed, DictHash: p.DictHash,
		ScoreVersion: 2, Setup: BuildSetup(p.Setup),
		ClientMetrics: map[string]any{"wpm": 100.0, "raw": 100.0, "acc": 1.0},
		ClientScore:   map[string]any{"version": 2, "total": 1234},
		Log:           raw,
	}
	if out.Mode == "" {
		out.Mode = "time"
	}
	if out.Mode == "time" {
		d := p.Setup.DurationMs
		if d == 0 {
			d = 15000
		}
		out.DurationMs = &d
	} else {
		w := p.Setup.WordCount
		if w == 0 {
			w = 25
		}
		out.WordCount = &w
	}
	return out
}

// MaxLegalPayload is the largest body ingestion actually ACCEPTS: a
// MaxWordCount word run over the longest legal duration, carrying as many
// events as fit under the body cap (SubmittableEvents — still fewer than
// MaxEvents, because the body cap is deliberately the binding one).
//
// "Largest accepted" rather than "largest documented" is the right fixture: a
// payload the server rejects measures the rejection path, and zone 5 needs the
// accept path's worst case.
func MaxLegalPayload(seed uint64) IngestPayload {
	return payloadWithEvents(SubmittableEvents(seed), seed)
}

// MaxEventsPayload is the payload the DOCUMENTED event cap allows: over the
// body cap and therefore un-submittable as one request. Zone 2 uses it because
// the replay worker's cost scales with event count, and MaxEvents is the number
// the validator promises to accept — the worker must survive a log that large
// however it arrived (a revalidation of an older row, a raised body cap).
func MaxEventsPayload(seed uint64) IngestPayload {
	return payloadWithEvents(MaxEvents, seed)
}

// Gzip compresses raw the way ingestion stores a log (stdlib, best speed).
func Gzip(raw []byte) []byte {
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
	if err != nil {
		panic(fmt.Sprintf("perf: gzip writer: %v", err))
	}
	if _, err := zw.Write(raw); err != nil {
		panic(fmt.Sprintf("perf: gzip write: %v", err))
	}
	if err := zw.Close(); err != nil {
		panic(fmt.Sprintf("perf: gzip close: %v", err))
	}
	return buf.Bytes()
}

// MustJSON marshals or panics. Generators build only shapes they define, so a
// failure here is a programming error, not an input error.
func MustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("perf: marshal: %v", err))
	}
	return b
}
