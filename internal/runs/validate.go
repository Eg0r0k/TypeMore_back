package runs

import (
	"encoding/json"
	"fmt"
	"math"

	"github.com/google/uuid"

	"github.com/typemore/typemore-server/internal/runlimits"
)

// Structural limits. These are cheap, game-agnostic bounds — the point is to
// reject obviously-malformed or abusive payloads before they reach storage, NOT
// to judge whether a run is plausible (that is the replay worker's job).
const (
	// Event-count bounds: a real run has at least one event, and 120k is above
	// the largest v1 run the documented game permits on ANY published
	// dictionary. The old 50 000 was sized around an interpreter cost that no
	// longer exists and refused a mode the docs promise. See the caps table in
	// docs/RUNS.md for the measurements and the tradeoff.
	//
	// THE CAP ITSELF NEVER MOVES, and this number was derived when MaxWordCount
	// was 10 000 — where the worst published dictionary (league_of_legends)
	// measured 118 943 events, 99% of it. Lowering MaxWordCount to 3 000 lowered
	// the MEASURING POINT, not this bound, so every one of those measurements is
	// still an upper bound and the headroom only grew. Raising this number is
	// what docs/DICTIONARIES.md forbids; lowering the measuring point is the
	// other legal answer, and it is the one that was taken.
	minEvents = 1
	maxEvents = 120_000
	// maxEventsV2 bounds a telemetry (log v2) submission: a v2 capture is 3×
	// its state events (one down/up pair per physical keystroke) plus Shift
	// pairs. The worst published dictionary measures 417 710 total events
	// (code_abap_1k, re-measured by the same generator-driven guard test that
	// sized maxEvents); this cap gives it ~15% headroom. Mirrored as
	// perf.MaxEventsV2.
	maxEventsV2 = 480_000
	// maxLogBytesV1 preserves the pre-telemetry envelope for v1 logs: the old
	// 6.5 MiB BODY cap bounded log+snapshots together, so bounding a v1 LOG
	// alone at the same number admits every previously-legal payload while the
	// transport cap above it is sized for v2.
	maxLogBytesV1 = 13 << 19

	// seedMax is 2³²−1: mulberry32 is a 32-bit PRNG (PROTOCOL.md §4).
	seedMax = int64(math.MaxUint32)

	// Dimension caps, read from internal/runlimits rather than restated — that
	// package carries the derivation of each number and is the one place all
	// three consumers (this validator, the bucket bound, the load fixtures) now
	// agree through. Which cap applies is decided by PRESENCE, not by the mode
	// string: mode semantics are game knowledge and this layer has none.
	//
	// Both are HARD, UNCONDITIONAL refusals — 422 before the run is stored and
	// long before the worker would replay it. Not judgements: nothing here is
	// behind policy.Judge or the anticheat tag, because the open build has to
	// refuse exactly what the closed one refuses (docs/SELF_HOST.md).
	maxDurationMs = int32(runlimits.MaxDurationMs)
	maxWordCount  = int32(runlimits.MaxWordCount)

	// Small opaque-string caps so a bucket/hash field cannot be abused as a
	// large blob smuggled outside the log.
	maxModeLen     = 32
	maxLangLen     = 32
	maxDictHashLen = 64

	// maxRestarts caps restartsSinceLastSubmit. The count is client-reported
	// and unverifiable (a restarted run leaves no log to check), so like the
	// declared view-only mods it is accepted on trust — the cap bounds abuse of
	// the profile's tests-started arithmetic, it does not judge plausibility.
	maxRestarts = int32(10_000)
)

// KnownScoreVersions enumerates the ScoreResult formula versions this server
// accepts. It is the single source of truth for the scoreVersion structural
// check (below) and docs/RUNS.md: v1 is the original formula, v2 is the current
// client's scoreV2. Anything outside this set is rejected as unsupported.
var KnownScoreVersions = []int16{1, 2}

// KnownLogVersions enumerates the EventLog wire versions this server accepts
// (frontend core: EVENT_LOG_VERSION / EVENT_LOG_VERSION_TELEMETRY) — the same
// single-source pattern as KnownScoreVersions, mirrored in docs/RUNS.md. v1 is
// the state-event grammar and is accepted forever; v2 additionally carries the
// optional down/up keystroke telemetry. The version is decided per run by the
// client's capability detection, so both stay live indefinitely.
var KnownLogVersions = []int16{1, 2}

// logVersionTelemetry is the version whose grammar admits down/up events.
const logVersionTelemetry = 2

// isKnownLogVersion reports whether v is an accepted EventLog wire version.
func isKnownLogVersion(v int) bool {
	for _, known := range KnownLogVersions {
		if int(known) == v {
			return true
		}
	}
	return false
}

// isKnownScoreVersion reports whether v is an accepted score-formula version.
func isKnownScoreVersion(v int) bool {
	for _, known := range KnownScoreVersions {
		if int(known) == v {
			return true
		}
	}
	return false
}

// Structural-validation error codes (HTTP 422). Distinct per rule so the client
// can react precisely; documented in docs/RUNS.md.
const (
	codeUnsupportedScoreVersion = "unsupported_score_version"
	codeUnsupportedLogVersion   = "unsupported_log_version"
	codeEmptyLog                = "empty_log"
	codeTooManyEvents           = "too_many_events"
	codeNonMonotonicSeq         = "non_monotonic_seq"
	codeMalformedLog            = "malformed_log"
	codeInvalidDimensions       = "invalid_dimensions"
	// Distinct from invalid_dimensions on purpose. "You sent both durationMs and
	// wordCount" is a client that does not know what it played; "you asked for
	// 5 000 words" is a client asking for a run this product does not offer.
	// A UI can act on the second (it names the limit that was crossed) and can
	// only apologise for the first.
	codeWordCountTooLarge  = "word_count_too_large"
	codeDurationTooLong    = "duration_too_long"
	codeSeedOutOfRange     = "seed_out_of_range"
	codeInvalidTextSource  = "invalid_text_source"
	codeQuoteTextSubmitted = "quote_text_submitted"
	codeLogTooLarge        = "log_too_large"
	codeInvalidRestarts    = "invalid_restarts"
	codeInvalidAdoptedFrom = "invalid_adopted_from"
)

// ingestRequest is the POST /runs body. The opaque snapshots and the log are
// captured as json.RawMessage so the log's exact bytes survive to gzip
// byte-for-byte; nothing re-encodes them. The bucket fields are lifted top-level
// to populate the indexed columns.
type ingestRequest struct {
	Mode          string          `json:"mode"`
	DurationMs    *int32          `json:"durationMs"`
	WordCount     *int32          `json:"wordCount"`
	Lang          string          `json:"lang"`
	Seed          *int64          `json:"seed"`
	DictHash      string          `json:"dictHash"`
	Setup         json.RawMessage `json:"setup"`
	Log           json.RawMessage `json:"log"`
	ClientMetrics json.RawMessage `json:"clientMetrics"`
	ClientScore   json.RawMessage `json:"clientScore"`
	ScoreVersion  *int            `json:"scoreVersion"`
	// RestartsSinceLastSubmit is optional: a client that predates the field
	// simply never restarted as far as the profile can tell, and 0 is the only
	// honest default. When present it must sit in [0, maxRestarts].
	RestartsSinceLastSubmit *int32 `json:"restartsSinceLastSubmit"`
}

// Text-source kinds. A run's text either comes from its seed (the seeded,
// effectively-infinite case) or from a published quote (a fixed, shared text).
//
// An ABSENT textSource means seeded. That is load-bearing: it is what let
// quotes land without bumping EVENT_LOG_VERSION, and every row written before
// quotes existed has no such key. `{"kind":"seeded"}` and no textSource at all
// must therefore be treated identically.
const (
	textSourceSeeded = "seeded"
	textSourceQuote  = "quote"
)

// setupEnvelope is the ONLY part of the opaque setup snapshot ingestion reads.
//
// Parsing any of the setup at all is a change of posture, so it is worth being
// explicit about why: the text source decides which DIMENSION RULE applies, and
// the dimension columns are indexed columns this layer owns. Everything else in
// the snapshot stays opaque and is still stored verbatim.
type setupEnvelope struct {
	// AdoptedFromRunId names the run this run's TEXT was taken from — the
	// "race this run" flow, which applies the target run's seed and word list
	// wholesale. Absent (the overwhelming majority, and every row written before
	// this field existed) means the text was generated fresh.
	//
	// It is read here for one reason only: shape. Whether the run it names
	// exists, belongs to anybody, or is even the player's own is deliberately
	// NOT asked — the field can only ever cost its own submitter a board slot,
	// so there is nothing to gain by forging it and nothing to protect. What a
	// malformed value WOULD cost is a run that silently counts because SQL read
	// nonsense as NULL, and that is what the shape check prevents.
	// Raw, so a value of the wrong JSON TYPE (a number, an object) is reported
	// as this field's problem rather than collapsing the whole envelope decode
	// into "setup.generation is not an object".
	AdoptedFromRunID json.RawMessage `json:"adoptedFromRunId"`
	Generation       *struct {
		TextSource *struct {
			Kind      string `json:"kind"`
			QuoteID   string `json:"quoteId"`
			QuoteHash string `json:"quoteHash"`
			// Text is here ONLY to be refused. The client never sends it — the
			// server re-reads the bytes from the registry by id — so a payload
			// that carries one is hand-rolled or tampered with, and accepting
			// it while ignoring it would leave the contract ambiguous about
			// whose copy of the text counts.
			Text json.RawMessage `json:"text"`
		} `json:"textSource"`
	} `json:"generation"`
}

// logEnvelope is the minimum of the EventLog we parse: the version, each
// event's sequence number, and — because log v2's telemetry has a version-gated
// grammar — the kind and the telemetry key code. Everything else stays opaque:
// the log is stored and replayed verbatim later, so the server does not model
// its full shape, and an unknown kind stays the worker's business.
type logEnvelope struct {
	Version *int `json:"version"`
	Events  []struct {
		Seq  *int64 `json:"seq"`
		Kind string `json:"kind"`
		Code string `json:"code"`
	} `json:"events"`
}

// validateIngest runs every structural check in a fixed order and, on success,
// produces the storage params (gzip-compressing the raw log). It never inspects
// game semantics. The returned *apiError is nil on success.
func validateIngest(userID uuid.UUID, req *ingestRequest) (CreateRunParams, *apiError) {
	// Required opaque payloads must be present (the decoder already proved they
	// are well-formed JSON when non-nil).
	if len(req.Setup) == 0 {
		return CreateRunParams{}, apiErrBadRequest("setup is required")
	}
	if len(req.ClientMetrics) == 0 {
		return CreateRunParams{}, apiErrBadRequest("clientMetrics is required")
	}
	if len(req.ClientScore) == 0 {
		return CreateRunParams{}, apiErrBadRequest("clientScore is required")
	}
	if len(req.Log) == 0 {
		return CreateRunParams{}, apiErrBadRequest("log is required")
	}

	// scoreVersion gate: only versions the server can eventually score are
	// accepted (KnownScoreVersions); anything else is a client too new/old.
	if req.ScoreVersion == nil || !isKnownScoreVersion(*req.ScoreVersion) {
		return CreateRunParams{}, apiErrUnprocessable(codeUnsupportedScoreVersion,
			fmt.Sprintf("unsupported score version; accepted versions are %v", KnownScoreVersions))
	}

	// Bucket string fields: present and small.
	if err := validateOpaqueField("mode", req.Mode, maxModeLen); err != nil {
		return CreateRunParams{}, err
	}
	if err := validateOpaqueField("lang", req.Lang, maxLangLen); err != nil {
		return CreateRunParams{}, err
	}
	if err := validateOpaqueField("dictHash", req.DictHash, maxDictHashLen); err != nil {
		return CreateRunParams{}, err
	}

	// Seed: present and within the 32-bit PRNG range.
	if req.Seed == nil {
		return CreateRunParams{}, apiErrBadRequest("seed is required")
	}
	if *req.Seed < 0 || *req.Seed > seedMax {
		return CreateRunParams{}, apiErrUnprocessable(codeSeedOutOfRange,
			"seed must be an integer in [0, 2^32-1]")
	}

	// The one part of the opaque setup snapshot this layer parses, read once.
	env, verr := parseSetupEnvelope(req.Setup)
	if verr != nil {
		return CreateRunParams{}, verr
	}

	// Text source: which of the two dimension rules applies, and the quote
	// reference's own shape. Must run BEFORE the dimension check, which reads
	// its answer.
	kind, verr := validateTextSource(env)
	if verr != nil {
		return CreateRunParams{}, verr
	}

	// Text provenance. Shape only — see setupEnvelope.AdoptedFromRunID.
	if verr := validateAdoptedFrom(env); verr != nil {
		return CreateRunParams{}, verr
	}

	// Dimensions: conditional on the text source (see validateDimensions).
	if verr := validateDimensions(req, kind); verr != nil {
		return CreateRunParams{}, verr
	}

	// Restart counter: optional, and bounded when present. Negative counts and
	// absurd ones are a client that is broken or lying — both get a distinct 422
	// rather than a silent clamp, for the same reason a submitted quote text is
	// refused rather than dropped.
	restarts := int32(0)
	if req.RestartsSinceLastSubmit != nil {
		restarts = *req.RestartsSinceLastSubmit
		if restarts < 0 || restarts > maxRestarts {
			return CreateRunParams{}, apiErrUnprocessable(codeInvalidRestarts,
				fmt.Sprintf("restartsSinceLastSubmit must be an integer in [0, %d]", maxRestarts))
		}
	}

	// Log envelope: version, event count, strictly increasing non-negative seq.
	if verr := validateLog(req.Log); verr != nil {
		return CreateRunParams{}, verr
	}

	gz, err := gzipLog(req.Log)
	if err != nil {
		return CreateRunParams{}, apiErrInternal
	}

	return CreateRunParams{
		UserID:                  userID,
		Mode:                    req.Mode,
		DurationMs:              req.DurationMs,
		WordCount:               req.WordCount,
		Lang:                    req.Lang,
		Seed:                    *req.Seed,
		DictHash:                req.DictHash,
		Setup:                   req.Setup,
		ClientMetrics:           req.ClientMetrics,
		ClientScore:             req.ClientScore,
		ScoreVersion:            int16(*req.ScoreVersion),
		Log:                     gz,
		LogBytes:                int32(len(req.Log)),
		RestartsSinceLastSubmit: restarts,
	}, nil
}

// validateOpaqueField enforces non-empty and a length cap on a bucket string.
func validateOpaqueField(name, value string, maxLen int) *apiError {
	if value == "" {
		return apiErrBadRequest(name + " is required")
	}
	if len(value) > maxLen {
		return apiErrBadRequest(name + " is too long")
	}
	return nil
}

// parseSetupEnvelope lifts the sliver of the setup snapshot ingestion reads.
// Everything else stays opaque and is stored verbatim.
func parseSetupEnvelope(setup json.RawMessage) (setupEnvelope, *apiError) {
	var env setupEnvelope
	if err := json.Unmarshal(setup, &env); err != nil {
		return setupEnvelope{}, apiErrUnprocessable(codeInvalidTextSource,
			"setup.generation is not an object")
	}
	return env, nil
}

// validateAdoptedFrom checks `setup.adoptedFromRunId`: absent, or a UUID.
//
// A run that names one is a SEEDED REPEAT — its text was taken from the run it
// names rather than generated — and it is ranked nowhere (migration 00017). It
// is still accepted, judged, stored and listed: "saved" and "counted" have been
// two different things on this surface since the first pending run, and this is
// one more way to be the first without being the second.
func validateAdoptedFrom(env setupEnvelope) *apiError {
	if len(env.AdoptedFromRunID) == 0 || string(env.AdoptedFromRunID) == "null" {
		return nil
	}
	var value string
	if err := json.Unmarshal(env.AdoptedFromRunID, &value); err != nil || !isCanonicalUUID(value) {
		return apiErrUnprocessable(codeInvalidAdoptedFrom,
			"setup.adoptedFromRunId must be a canonical lowercase dashed uuid when present")
	}
	return nil
}

// isCanonicalUUID reports whether s is a uuid in the ONE spelling the rest of
// the system agrees on: 36 characters, lowercase, dashed.
//
// uuid.Parse alone is not enough, and the gap is not cosmetic. It also accepts
// `{...}`, `urn:uuid:...` and the undashed 32-character form — but the SQL that
// reads these ids back out of the setup document (run_quote_id and
// run_adopted_from, migrations 00009 and 00017) matches a DASHED pattern and
// answers NULL to anything else. So a non-canonical spelling passes ingestion
// here and then means something different downstream:
//
//   - an undashed adoptedFromRunId would be no marker at all in SQL, and the
//     seeded repeat it marks would quietly count;
//   - an undashed quoteId resolves in the worker (which uses uuid.Parse) but not
//     in the view, so the run would be judged against its quote and ranked on no
//     board, for no reason a reader could see.
//
// Refusing the spelling at the door is what keeps "the id" one thing. It is the
// same rule leaderboard.ParseBucketKey applies to a bucket key, for the same
// reason: these ids are also what people link to, and two spellings of one id
// are two of everything that is keyed by it.
func isCanonicalUUID(s string) bool {
	parsed, err := uuid.Parse(s)
	return err == nil && parsed.String() == s
}

// validateTextSource checks `setup.generation.textSource` and reports the run's
// text-source kind. An absent textSource is `seeded` — the legacy shape, and
// what every row written before quotes existed carries.
//
// This looks no further than it must: the kind, and — for a quote — that the
// reference is usable as a reference. Whether the quote EXISTS is not asked
// here. Ingestion is game-agnostic and never consults a registry; the worker
// resolves the id and flags `unknown_quote` if it cannot, exactly as it already
// does for an unknown dictionary.
func validateTextSource(env setupEnvelope) (string, *apiError) {
	if env.Generation == nil || env.Generation.TextSource == nil {
		return textSourceSeeded, nil
	}

	src := env.Generation.TextSource
	switch src.Kind {
	case textSourceSeeded:
		return textSourceSeeded, nil
	case textSourceQuote:
	default:
		return "", apiErrUnprocessable(codeInvalidTextSource,
			`setup.generation.textSource.kind must be "seeded" or "quote"`)
	}

	// A submitted text is refused outright rather than dropped. The server
	// re-reads the bytes from the registry by id and would ignore this field
	// anyway — but ignoring it silently would leave two plausible readings of
	// the contract, and the one where the client's copy matters is the one that
	// makes a quote score meaningless.
	if len(src.Text) > 0 {
		return "", apiErrUnprocessable(codeQuoteTextSubmitted,
			"setup.generation.textSource must not carry text; the server resolves it from quoteId")
	}
	// Canonical spelling, not merely parseable — see isCanonicalUUID. An
	// undashed id resolves in the worker and NOT in leaderboard_eligible_runs,
	// which would judge a run against its quote and then rank it nowhere.
	if !isCanonicalUUID(src.QuoteID) {
		return "", apiErrUnprocessable(codeInvalidTextSource,
			"setup.generation.textSource.quoteId must be a canonical lowercase dashed uuid")
	}
	if src.QuoteHash == "" || len(src.QuoteHash) > maxDictHashLen {
		return "", apiErrUnprocessable(codeInvalidTextSource,
			"setup.generation.textSource.quoteHash is required and must be short")
	}
	return textSourceQuote, nil
}

// validateDimensions enforces the dimension rule, which is CONDITIONAL on the
// text source (and mirrors the runs_one_dimension schema CHECK, migration
// 00008):
//
//	seeded → exactly one of durationMs / wordCount, in range
//	quote  → neither
//
// A quote run has no dimension because neither number would mean anything. Its
// length is a property of the text, not of the session: the player did not
// choose 25 words or 30 seconds, they chose a quote, and the same quote is the
// same length for everyone. A wordCount here would be a second, unverified copy
// of something the registry already knows, and a durationMs would describe a
// deadline the run does not have.
//
// The seeded rule stays a strict XOR. Relaxing it for quotes is not a reason to
// weaken it everywhere: a seeded run with both, or with neither, is still a
// client that does not know what it played.
func validateDimensions(req *ingestRequest, textSource string) *apiError {
	if textSource == textSourceQuote {
		if req.DurationMs != nil || req.WordCount != nil {
			return apiErrUnprocessable(codeInvalidDimensions,
				"a quote run carries neither durationMs nor wordCount")
		}
		return nil
	}
	switch {
	case req.DurationMs != nil && req.WordCount != nil:
		return apiErrUnprocessable(codeInvalidDimensions,
			"exactly one of durationMs or wordCount must be set, not both")
	case req.DurationMs != nil:
		if *req.DurationMs <= 0 {
			return apiErrUnprocessable(codeInvalidDimensions,
				"durationMs must be positive")
		}
		if *req.DurationMs > maxDurationMs {
			return apiErrUnprocessable(codeDurationTooLong,
				fmt.Sprintf("a run may last at most %d ms", maxDurationMs))
		}
	case req.WordCount != nil:
		if *req.WordCount <= 0 {
			return apiErrUnprocessable(codeInvalidDimensions,
				"wordCount must be positive")
		}
		if *req.WordCount > maxWordCount {
			return apiErrUnprocessable(codeWordCountTooLarge,
				fmt.Sprintf("a run may ask for at most %d words", maxWordCount))
		}
	default:
		return apiErrUnprocessable(codeInvalidDimensions,
			"exactly one of durationMs or wordCount must be set")
	}
	return nil
}

// validateLog checks the EventLog envelope structurally: version, event count,
// strictly-increasing non-negative seq (a cheap linear scan), and the
// version-gated telemetry grammar — a v1 log must carry no down/up events, and
// a v2 telemetry event's code must look like a KeyboardEvent.code. It does NOT
// enforce contiguity, monotonic time, pairing, or any reduce-level rule — those
// are the replay worker's deep validation (the frontend core's validate.ts;
// pairing in particular is a scored FLAG there, never a rejection).
func validateLog(raw json.RawMessage) *apiError {
	var env logEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return apiErrUnprocessable(codeMalformedLog,
			"log is not a valid EventLog envelope")
	}
	if env.Version == nil || !isKnownLogVersion(*env.Version) {
		return apiErrUnprocessable(codeUnsupportedLogVersion,
			fmt.Sprintf("unsupported log version; accepted versions are %v", KnownLogVersions))
	}
	telemetryAllowed := *env.Version == logVersionTelemetry
	// The transport cap admits the largest grammar (v2); a v1 log is bounded
	// here at the pre-telemetry envelope so that raising the transport cap did
	// not quietly widen what a v1 client may send.
	if !telemetryAllowed && len(raw) > maxLogBytesV1 {
		return apiErrUnprocessable(codeLogTooLarge,
			fmt.Sprintf("a version-1 log may be at most %d bytes", maxLogBytesV1))
	}
	n := len(env.Events)
	if n < minEvents {
		return apiErrUnprocessable(codeEmptyLog, "log has no events")
	}
	eventCap := maxEvents
	if telemetryAllowed {
		eventCap = maxEventsV2
	}
	if n > eventCap {
		return apiErrUnprocessable(codeTooManyEvents,
			fmt.Sprintf("log exceeds the maximum of %d events", eventCap))
	}
	// Strictly increasing, non-negative seq. prev starts below zero so the first
	// event must be >= 0; each subsequent event must exceed its predecessor.
	prev := int64(-1)
	for i := range env.Events {
		event := &env.Events[i]
		if event.Seq == nil {
			return apiErrUnprocessable(codeNonMonotonicSeq,
				"an event is missing its seq")
		}
		if *event.Seq <= prev {
			return apiErrUnprocessable(codeNonMonotonicSeq,
				"event seq is not strictly increasing from a non-negative start")
		}
		prev = *event.Seq
		if event.Kind == "down" || event.Kind == "up" {
			if !telemetryAllowed {
				return apiErrUnprocessable(codeMalformedLog,
					"telemetry events (down/up) require log version 2")
			}
			if !isKeyCode(event.Code) {
				return apiErrUnprocessable(codeMalformedLog,
					"a telemetry event's code must be 1-32 characters of the KeyboardEvent.code charset")
			}
		}
	}
	return nil
}

// isKeyCode reports whether s is shaped like a KeyboardEvent.code: 1-32 ASCII
// alphanumerics ("KeyF", "ShiftLeft", "Numpad0"). Mirrors the core's parse.ts.
func isKeyCode(s string) bool {
	if len(s) < 1 || len(s) > 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
			continue
		}
		return false
	}
	return true
}
