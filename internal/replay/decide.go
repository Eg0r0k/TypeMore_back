package replay

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
)

// Verdicts. 'valid' and 'invalid' come from the core's validateLog; 'error' is
// the server's own: the run could not be replayed at all, so the core never had
// an opinion about it.
const (
	verdictValid   = "valid"
	verdictInvalid = "invalid"
	verdictError   = "error"
)

// Score formula versions the worker can recompute. Mirrors the core's
// SCORE_VERSION / SCORE_VERSION_2 and runs.KnownScoreVersions.
const (
	scoreVersionV1 int16 = 1
	scoreVersionV2 int16 = 2
)

// Decision reasons. These are stable machine codes: they land in
// validation.reason and are what an admin queue filters on.
const (
	// ReasonUnknownDict — the run's dict_hash is not in the registry, so its
	// text cannot be regenerated. Flagged, never rejected: the run may simply
	// predate a dictionary rotation, which is the server's fault, not the
	// player's.
	ReasonUnknownDict = "unknown_dict"
	// ReasonReplayTimeout — the core exceeded its interrupt budget.
	ReasonReplayTimeout = "replay_timeout"
	// ReasonReplayError — the core threw, or returned something undecodable.
	ReasonReplayError = "replay_error"
	// ReasonScoreMismatch — the server's score total differs from the client's.
	ReasonScoreMismatch = "score_mismatch"
	// ReasonMetricMismatch — a client-reported metric differs from the server's.
	ReasonMetricMismatch = "metric_mismatch"
	// ReasonPlausibility — the log is valid and the numbers agree, but the core
	// raised anti-cheat flags.
	ReasonPlausibility = "plausibility_flags"
)

// metricTolerance bounds the metric comparison. The two sides run the same code
// over the same log, so the only legitimate difference is the last bits of a
// double that took a different-but-equivalent route through JSON.
//
// Note what does NOT get a tolerance: the score total. It is an integer out of a
// single Math.round, so any difference at all means two different codes ran —
// an epsilon there would hide exactly the drift this worker exists to detect.
const metricTolerance = 1e-9

// ErrUnknownDict marks a run whose dictionary the registry does not know.
var ErrUnknownDict = errors.New("replay: unknown dict_hash")

// validationDoc is the `validation` jsonb column: the core's report plus the
// decision reason and, when numbers disagreed, which one and by how much.
type validationDoc struct {
	Verdict string `json:"verdict"`
	Reason  string `json:"reason,omitempty"`
	Flags   []Flag `json:"flags"`
	// Divergence names the first field whose value did not match the client's.
	// Both numbers are stored so a reviewer never has to re-run anything; the
	// full objects live in client_metrics/client_score and
	// server_metrics/server_score beside it.
	Divergence *divergence `json:"divergence,omitempty"`
}

// divergence is one client/server disagreement. Client is a pointer so a field
// the client never sent is recorded as null rather than a misleading zero.
type divergence struct {
	Field  string   `json:"field"`
	Client *float64 `json:"client"`
	Server float64  `json:"server"`
}

// clientScoreDoc / clientMetricsDoc are the only parts of the client's opaque
// snapshots the worker reads — the numbers it must agree with.
type clientScoreDoc struct {
	Total *float64 `json:"total"`
}

type clientMetricsDoc struct {
	WPM *float64 `json:"wpm"`
	Raw *float64 `json:"raw"`
	Acc *float64 `json:"acc"`
}

// serverScoreDoc / serverMetricsDoc mirror the core's ScoreResult / Metrics for
// comparison only. The stored columns keep the core's full JSON.
type serverScoreDoc struct {
	Total float64 `json:"total"`
}

type serverMetricsDoc struct {
	WPM      float64 `json:"wpm"`
	Raw      float64 `json:"raw"`
	Accuracy float64 `json:"accuracy"`
}

// decide maps a replay outcome onto the run's new state.
//
// It is pure and total: every input produces a decision, which is what keeps a
// poisonous run from wedging the loop. The table, in precedence order:
//
//	unknown dictionary        → flagged  unknown_dict        (attempts unchanged)
//	replay timed out          → flagged  replay_timeout      (attempts + 1)
//	core threw / undecodable  → flagged  replay_error        (attempts + 1)
//	verdict invalid           → rejected reason from the core
//	score total differs       → flagged  score_mismatch
//	a metric differs > 1e-9   → flagged  metric_mismatch
//	plausibility flags raised → flagged  plausibility_flags
//	otherwise                 → accepted
//
// An invalid log outranks a mismatch: numbers recomputed from a log the reducer
// refused are meaningless, so they are not stored at all.
func decide(run PendingRun, res Result, replayErr error) Decision {
	base := Decision{BundleSHA: bundleSHA, Attempts: run.Attempts}

	switch {
	case errors.Is(replayErr, ErrUnknownDict):
		// Not the player's fault and not retryable by waiting: an operator has
		// to republish the dictionary. Attempts stay put.
		return withValidation(base, StatusFlagged, validationDoc{
			Verdict: verdictError,
			Reason:  ReasonUnknownDict,
			Flags:   []Flag{},
		}, "dict_hash "+run.DictHash+" is not in the registry")

	case errors.Is(replayErr, ErrReplayTimeout):
		base.Attempts = run.Attempts + 1
		return withValidation(base, StatusFlagged, validationDoc{
			Verdict: verdictError,
			Reason:  ReasonReplayTimeout,
			Flags:   []Flag{},
		}, replayErr.Error())

	case replayErr != nil:
		base.Attempts = run.Attempts + 1
		return withValidation(base, StatusFlagged, validationDoc{
			Verdict: verdictError,
			Reason:  ReasonReplayError,
			Flags:   []Flag{},
		}, replayErr.Error())

	case res.Verdict == verdictInvalid:
		return withValidation(base, StatusRejected, validationDoc{
			Verdict: verdictInvalid,
			Reason:  res.Reason,
			Flags:   res.Flags,
		}, "")
	}

	// From here the log replayed cleanly, so the server's numbers are the
	// authoritative ones and are stored whatever the decision turns out to be.
	base.ServerMetrics = res.Metrics
	base.ServerScore = res.Score

	doc := validationDoc{Verdict: verdictValid, Flags: res.Flags}

	if d, err := compareScore(run.ClientScore, res.Score); err != nil {
		base.Attempts = run.Attempts + 1
		return withValidation(base, StatusFlagged, validationDoc{
			Verdict: verdictError,
			Reason:  ReasonReplayError,
			Flags:   res.Flags,
		}, err.Error())
	} else if d != nil {
		doc.Reason, doc.Divergence = ReasonScoreMismatch, d
		return withValidation(base, StatusFlagged, doc, "")
	}

	if d, err := compareMetrics(run.ClientMetrics, res.Metrics); err != nil {
		base.Attempts = run.Attempts + 1
		return withValidation(base, StatusFlagged, validationDoc{
			Verdict: verdictError,
			Reason:  ReasonReplayError,
			Flags:   res.Flags,
		}, err.Error())
	} else if d != nil {
		doc.Reason, doc.Divergence = ReasonMetricMismatch, d
		return withValidation(base, StatusFlagged, doc, "")
	}

	if len(res.Flags) > 0 {
		doc.Reason = ReasonPlausibility
		return withValidation(base, StatusFlagged, doc, "")
	}
	return withValidation(base, StatusAccepted, doc, "")
}

// withValidation finalises a decision by encoding its validation document. The
// document is built by this package from fixed shapes, so marshalling cannot
// fail; a broken encoder would still leave a decodable decision behind.
func withValidation(d Decision, status string, doc validationDoc, lastError string) Decision {
	if doc.Flags == nil {
		doc.Flags = []Flag{}
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		raw = []byte(`{"verdict":"error","reason":"replay_error","flags":[]}`)
	}
	d.Status = status
	d.Validation = raw
	d.LastError = lastError
	return d
}

// compareScore returns the divergence between the client's reported total and
// the server's, or nil when they are identical.
//
// Identical means ==, not "close". Both totals come out of the same
// Math.round in the same bundle; a difference is not a rounding artefact, it is
// evidence that the client ran different code (or edited the number).
func compareScore(client, server json.RawMessage) (*divergence, error) {
	var s serverScoreDoc
	if err := json.Unmarshal(server, &s); err != nil {
		return nil, fmt.Errorf("replay: decode server score: %w", err)
	}
	var c clientScoreDoc
	if err := json.Unmarshal(client, &c); err != nil {
		// A client_score that is not an object is a structural problem the
		// ingest layer let through; treat it as undecidable, not as a match.
		return nil, fmt.Errorf("replay: decode client score: %w", err)
	}
	if c.Total != nil && *c.Total == s.Total {
		return nil, nil
	}
	return &divergence{Field: "total", Client: c.Total, Server: s.Total}, nil
}

// compareMetrics returns the first metric whose client value differs from the
// server's by more than metricTolerance, or nil when all three agree. The client
// submits wpm/raw/acc; the core calls the third one `accuracy`.
func compareMetrics(client, server json.RawMessage) (*divergence, error) {
	var s serverMetricsDoc
	if err := json.Unmarshal(server, &s); err != nil {
		return nil, fmt.Errorf("replay: decode server metrics: %w", err)
	}
	var c clientMetricsDoc
	if err := json.Unmarshal(client, &c); err != nil {
		return nil, fmt.Errorf("replay: decode client metrics: %w", err)
	}
	for _, cmp := range []struct {
		field  string
		client *float64
		server float64
	}{
		{"wpm", c.WPM, s.WPM},
		{"raw", c.Raw, s.Raw},
		{"acc", c.Acc, s.Accuracy},
	} {
		if cmp.client == nil || math.Abs(*cmp.client-cmp.server) > metricTolerance {
			return &divergence{Field: cmp.field, Client: cmp.client, Server: cmp.server}, nil
		}
	}
	return nil, nil
}
