package runs

import (
	"encoding/json"
	"math"

	"github.com/google/uuid"
)

// Structural limits. These are cheap, game-agnostic bounds — the point is to
// reject obviously-malformed or abusive payloads before they reach storage, NOT
// to judge whether a run is plausible (that is the replay worker's job).
const (
	// supportedLogVersion is the only EventLog wire version this server accepts
	// (frontend core: EVENT_LOG_VERSION). Bumped in lockstep with the client.
	supportedLogVersion = 1
	// supportedScoreVersion is the only ScoreResult version accepted for now.
	supportedScoreVersion = 1

	// Event-count bounds: a real run has at least one event, and 50k is far
	// above any legitimate session (10k words × a few keystrokes) while still
	// bounding the linear scan and the stored blob.
	minEvents = 1
	maxEvents = 50_000

	// seedMax is 2³²−1: mulberry32 is a 32-bit PRNG (PROTOCOL.md §4).
	seedMax = int64(math.MaxUint32)

	// Dimension caps. Structural only: durations up to an hour, word counts up
	// to the client's 10k ceiling. Which one applies is decided by presence,
	// not by the mode string (mode semantics are game knowledge).
	maxDurationMs = int32(3_600_000)
	maxWordCount  = int32(10_000)

	// Small opaque-string caps so a bucket/hash field cannot be abused as a
	// large blob smuggled outside the log.
	maxModeLen     = 32
	maxLangLen     = 32
	maxDictHashLen = 64
)

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
	codeSeedOutOfRange          = "seed_out_of_range"
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
}

// logEnvelope is the minimum of the EventLog we parse: the version and each
// event's sequence number. Everything else stays opaque — the log is stored and
// replayed verbatim later, so the server does not model its full shape.
type logEnvelope struct {
	Version *int `json:"version"`
	Events  []struct {
		Seq *int64 `json:"seq"`
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

	// scoreVersion gate: only v1 is understood; anything else is a client the
	// server cannot score yet.
	if req.ScoreVersion == nil || *req.ScoreVersion != supportedScoreVersion {
		return CreateRunParams{}, apiErrUnprocessable(codeUnsupportedScoreVersion,
			"unsupported score version; this server accepts scoreVersion 1")
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

	// Dimensions: exactly one of durationMs / wordCount, each in range.
	if verr := validateDimensions(req); verr != nil {
		return CreateRunParams{}, verr
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
		UserID:        userID,
		Mode:          req.Mode,
		DurationMs:    req.DurationMs,
		WordCount:     req.WordCount,
		Lang:          req.Lang,
		Seed:          *req.Seed,
		DictHash:      req.DictHash,
		Setup:         req.Setup,
		ClientMetrics: req.ClientMetrics,
		ClientScore:   req.ClientScore,
		ScoreVersion:  int16(supportedScoreVersion),
		Log:           gz,
		LogBytes:      int32(len(req.Log)),
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

// validateDimensions enforces the "exactly one of durationMs / wordCount, in
// range" rule (mirrors the runs_one_dimension schema CHECK).
func validateDimensions(req *ingestRequest) *apiError {
	switch {
	case req.DurationMs != nil && req.WordCount != nil:
		return apiErrUnprocessable(codeInvalidDimensions,
			"exactly one of durationMs or wordCount must be set, not both")
	case req.DurationMs != nil:
		if *req.DurationMs <= 0 || *req.DurationMs > maxDurationMs {
			return apiErrUnprocessable(codeInvalidDimensions,
				"durationMs is out of range")
		}
	case req.WordCount != nil:
		if *req.WordCount <= 0 || *req.WordCount > maxWordCount {
			return apiErrUnprocessable(codeInvalidDimensions,
				"wordCount is out of range")
		}
	default:
		return apiErrUnprocessable(codeInvalidDimensions,
			"exactly one of durationMs or wordCount must be set")
	}
	return nil
}

// validateLog checks the EventLog envelope structurally: version, event count,
// and strictly-increasing non-negative seq (a cheap linear scan). It does NOT
// enforce contiguity, monotonic time, or any reduce-level rule — those are the
// replay worker's deep validation (the frontend core's validate.ts).
func validateLog(raw json.RawMessage) *apiError {
	var env logEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return apiErrUnprocessable(codeMalformedLog,
			"log is not a valid EventLog envelope")
	}
	if env.Version == nil || *env.Version != supportedLogVersion {
		return apiErrUnprocessable(codeUnsupportedLogVersion,
			"unsupported log version; this server accepts version 1")
	}
	n := len(env.Events)
	if n < minEvents {
		return apiErrUnprocessable(codeEmptyLog, "log has no events")
	}
	if n > maxEvents {
		return apiErrUnprocessable(codeTooManyEvents,
			"log exceeds the maximum of 50000 events")
	}
	// Strictly increasing, non-negative seq. prev starts below zero so the first
	// event must be >= 0; each subsequent event must exceed its predecessor.
	prev := int64(-1)
	for i := range env.Events {
		seq := env.Events[i].Seq
		if seq == nil {
			return apiErrUnprocessable(codeNonMonotonicSeq,
				"an event is missing its seq")
		}
		if *seq <= prev {
			return apiErrUnprocessable(codeNonMonotonicSeq,
				"event seq is not strictly increasing from a non-negative start")
		}
		prev = *seq
	}
	return nil
}
