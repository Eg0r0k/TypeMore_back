package replay

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"

	"github.com/typemore/typemore-server/internal/replay/policy"
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
	// player's. `make revalidate` re-evaluates it if the dictionary returns.
	ReasonUnknownDict = "unknown_dict"
	// ReasonUnknownQuote — the run was played on a fixed text the quote
	// registry cannot currently produce: no quote with that id, or a quote
	// whose stored text_hash is not the quoteHash the run claims.
	//
	// Flagged, never rejected, and the distinction is the point. Rejection is
	// for a run we can PROVE is bad. This is a run we cannot judge at all: the
	// thing that moved may well be the corpus, not the player — a re-import
	// that superseded a revision, a restore from a stale dump, a quote id that
	// has not landed on this node yet. `make revalidate` re-judges it once the
	// registry can answer, and a wrong rejection is not recoverable that way.
	ReasonUnknownQuote = "unknown_quote"
	// ReasonReplayTimeout — the core exceeded its interrupt budget.
	ReasonReplayTimeout = "replay_timeout"
	// ReasonReplayError — the core threw, or returned something undecodable.
	ReasonReplayError = "replay_error"
	// ReasonScoreMismatch — the server's score total differs from the client's.
	ReasonScoreMismatch = "score_mismatch"
	// ReasonMetricMismatch — a client-reported metric differs from the server's.
	ReasonMetricMismatch = "metric_mismatch"
	// ReasonSuspicionThreshold — the log is valid and the numbers agree, but the
	// weighted flag severities reached the review threshold.
	ReasonSuspicionThreshold = "suspicion_threshold"
	// ReasonBotPattern — a bot-shaped flag combination fired. Independent of the
	// threshold: a shape, not a magnitude.
	ReasonBotPattern = "bot_pattern"
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

// ErrUnknownQuote marks a run whose fixed text the quote registry cannot
// produce — unknown id, or a text_hash that disagrees with the run's claim.
// Wrapped by Judge with which of the two it was.
var ErrUnknownQuote = errors.New("replay: unresolvable quote")

// validationDoc is the `validation` jsonb column: the core's report, the policy
// arithmetic that judged it, and — when numbers disagreed — which one and by
// how much.
//
// Flags and the policy block are written on EVERY valid decision, accepted ones
// included: moderation needs to see that a run raised min-interval and scored
// 0.009 suspicion, and it needs that visible without re-running anything. None
// of it is exposed to the player (docs/RUNS.md lists the summary fields).
type validationDoc struct {
	Verdict string `json:"verdict"`
	Reason  string `json:"reason,omitempty"`
	Flags   []Flag `json:"flags"`
	// Policy is the arithmetic behind the status: which rule set ran, what the
	// flags summed to, what it was compared against, and which combination
	// rules fired.
	Policy *policyDoc `json:"policy,omitempty"`
	// Divergence names the first field whose value did not match the client's.
	// Both numbers are stored so a reviewer never has to re-run anything; the
	// full objects live in client_metrics/client_score and
	// server_metrics/server_score beside it.
	Divergence *divergence `json:"divergence,omitempty"`
}

// policyDoc is the audit trail for one policy evaluation. The effective
// threshold is recorded alongside the suspicion because the weights are
// env-tunable: policy_version alone would not explain a verdict on a server
// running an override.
type policyDoc struct {
	Version   int16    `json:"version"`
	Suspicion float64  `json:"suspicion"`
	Threshold float64  `json:"threshold"`
	Rules     []string `json:"rules,omitempty"`
	// UnknownFlags are codes the bundle emitted that the weights table has no
	// entry for. Always empty in a healthy deployment; non-empty means the
	// bundle moved ahead of the policy.
	UnknownFlags []string `json:"unknownFlags,omitempty"`
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

// Decide maps a replay outcome onto the run's new state.
//
// It is pure and total: every input produces a decision, which is what keeps a
// poisonous run from wedging the loop. The table, in precedence order:
//
//	unknown dictionary          → flagged  unknown_dict         (attempts unchanged)
//	unresolvable quote          → flagged  unknown_quote        (attempts unchanged)
//	replay timed out            → flagged  replay_timeout       (attempts + 1)
//	core threw / undecodable    → flagged  replay_error         (attempts + 1)
//	verdict invalid             → rejected reason from the core
//	score total differs         → flagged  score_mismatch
//	a metric differs > 1e-9     → flagged  metric_mismatch
//	bot-shaped combination      → flagged  bot_pattern
//	suspicion ≥ threshold       → flagged  suspicion_threshold
//	otherwise                   → accepted (flags and suspicion still recorded)
//
// An invalid log outranks a mismatch: numbers recomputed from a log the reducer
// refused are meaningless, so they are not stored at all.
//
// Note what is NOT in the table: "a plausibility flag was raised". A single weak
// flag never sends a run to review — see internal/replay/policy for why.
//
// # Everything above the last two rows is independent of the judge
//
// Read the table again from the top. Every refusal down to metric_mismatch is
// decided before a judge is consulted and cannot be changed by one: they are
// structural facts about the log and arithmetic disagreements with the client.
// Only the last two rows are policy, and an instance running policy.Noop simply
// never reaches them.
//
// That is what makes the anti-cheat policy removable without making the product
// wrong, and it is asserted rather than asserted-in-a-comment:
// TestHardVerdictsDoNotDependOnTheJudge runs the tamper matrix against judges
// from "review nothing, ever" to "review everything" and gets the same verdicts.
type Decider struct {
	judge policy.Judge
	// version is judge.Version() mapped onto the smallint column, resolved ONCE
	// in NewDecider. A judge whose version cannot be stored is a startup
	// failure, not a surprise on some run at three in the morning.
	version int16
}

// NewDecider binds a judge to the decision path, rejecting one whose version
// cannot be recorded on a run.
func NewDecider(j policy.Judge) (Decider, error) {
	if j == nil {
		j = policy.Noop{}
	}
	version, err := policy.ParseVersion(j.Version())
	if err != nil {
		return Decider{}, err
	}
	return Decider{judge: j, version: version}, nil
}

// Judge returns the judge this decider consults. Used by the composition root
// to report what is running; the decision path is the only thing that calls it.
func (p Decider) Judge() policy.Judge { return p.judge }

// isZero reports whether this decider was never built. The worker replaces one
// with the open default rather than dereferencing a nil judge.
func (p Decider) isZero() bool { return p.judge == nil }

// Decide maps a replay outcome onto the run's new state.
func (p Decider) Decide(run PendingRun, res Result, replayErr error) Decision {
	base := Decision{BundleSHA: bundleSHA, PolicyVersion: p.version, Attempts: run.Attempts}

	switch {
	case errors.Is(replayErr, ErrUnknownDict):
		// Not the player's fault and not retryable by waiting: an operator has
		// to republish the dictionary. Attempts stay put, and `make revalidate`
		// re-evaluates the run if the dictionary comes back.
		return withValidation(base, StatusFlagged, validationDoc{
			Verdict: verdictError,
			Reason:  ReasonUnknownDict,
			Flags:   []Flag{},
		}, "dict_hash "+run.DictHash+" is not in the registry")

	case errors.Is(replayErr, ErrUnknownQuote):
		// Same shape as an unknown dictionary, same reasoning: the corpus is a
		// server-side artefact, so a text we cannot reproduce right now is our
		// gap, not the player's cheating. Attempts stay put so `make revalidate`
		// picks the run up again once the registry can answer.
		return withValidation(base, StatusFlagged, validationDoc{
			Verdict: verdictError,
			Reason:  ReasonUnknownQuote,
			Flags:   []Flag{},
		}, replayErr.Error())

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
	// The keyboard observations travel the same way: whatever the status, the
	// projector needs them — to add an accepted contribution, or to reverse a
	// stamped one this decision just demoted.
	base.ServerMetrics = res.Metrics
	base.ServerScore = res.Score
	base.CharObservations = res.CharObservations

	// The only place the judge is consulted. Everything above this line was
	// decided without it and stays decided without it.
	verdict := p.judge.Judge(res.Flags, policy.RunMeta{
		DurationSec:  runSeconds(res.Metrics),
		ScoreVersion: run.ScoreVersion,
	})

	// The policy block is attached to every JUDGED decision — accepted included.
	// An accepted run KEEPS its flags and its suspicion so moderation can audit
	// the boundary without re-running anything.
	//
	// A judge that does not judge writes no block at all. That is not the same
	// as a block full of zeroes: zeroes would read as "a policy looked and found
	// nothing", and the truth is that nothing looked. The absent block and the
	// NULL policy_version beside it are what keep such a run re-judgeable
	// forever (docs/SELF_HOST.md).
	//
	// Keyed on the resolved column value rather than on asking the judge again,
	// so "no policy block" and "NULL policy_version" are the same decision and
	// cannot drift into a row that has one but not the other.
	doc := validationDoc{Verdict: verdictValid, Flags: res.Flags}
	if p.version != policy.ColumnNone {
		doc.Policy = &policyDoc{
			Version:      p.version,
			Suspicion:    roundSuspicion(verdict.Suspicion),
			Threshold:    verdict.Threshold,
			Rules:        verdict.Reasons,
			UnknownFlags: verdict.UnknownFlags,
		}
	}

	if d, err := compareScore(run.ClientScore, res.Score); err != nil {
		base.Attempts = run.Attempts + 1
		return withValidation(base, StatusFlagged, validationDoc{
			Verdict: verdictError,
			Reason:  ReasonReplayError,
			Flags:   res.Flags,
			Policy:  doc.Policy,
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
			Policy:  doc.Policy,
		}, err.Error())
	} else if d != nil {
		doc.Reason, doc.Divergence = ReasonMetricMismatch, d
		return withValidation(base, StatusFlagged, doc, "")
	}

	// The judge's routing, recorded with the distinction the reason codes have
	// always drawn: a run routed because a SHAPE fired reads differently from
	// one routed because a magnitude crossed a line. Which shapes exist, and
	// where the line is, is the judge's business and not recorded here.
	if verdict.NeedsReview {
		doc.Reason = ReasonSuspicionThreshold
		if len(verdict.Reasons) > 0 {
			doc.Reason = ReasonBotPattern
		}
		return withValidation(base, StatusFlagged, doc, "")
	}
	return withValidation(base, StatusAccepted, doc, "")
}

// roundSuspicion trims the stored suspicion to six decimals. The comparison
// above uses the full double; this only keeps the audit JSON readable.
func roundSuspicion(v float64) float64 {
	return math.Round(v*1e6) / 1e6
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

// runSeconds reads durationSec out of the server's own metrics — the one piece
// of a run, besides its flags, that a judge is given. A missing or unreadable
// value reads as 0, which makes a duration-floor rule abstain rather than fire
// on a guess.
func runSeconds(metrics json.RawMessage) float64 {
	if len(metrics) == 0 {
		return 0
	}
	var m struct {
		DurationSec float64 `json:"durationSec"`
	}
	if err := json.Unmarshal(metrics, &m); err != nil {
		return 0
	}
	return m.DurationSec
}
