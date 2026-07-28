package runs_test

// Profile end-to-end (docs/PROFILE.md): real HTTP against the same wiring
// cmd/server runs — auth session, runs ingested through POST /runs, judged by
// the real replay worker in goja, aggregated by the profile SQL, decorated by
// the leaderboard bucket parser. Nothing is seeded behind the API's back
// except where a test says so explicitly.

import (
	"encoding/json"
	"math"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type profileSummaryResp struct {
	DisplayName          string  `json:"displayName"`
	Joined               string  `json:"joined"`
	TestsStarted         int64   `json:"testsStarted"`
	TestsCompleted       int64   `json:"testsCompleted"`
	RestartsPerCompleted float64 `json:"restartsPerCompleted"`
	TimeTypingMs         int64   `json:"timeTypingMs"`
	EstimatedWordsTyped  int64   `json:"estimatedWordsTyped"`
	Wpm                  struct {
		Highest       float64 `json:"highest"`
		Average       float64 `json:"average"`
		AverageLast10 float64 `json:"averageLast10"`
	} `json:"wpm"`
	Raw struct {
		Highest       float64 `json:"highest"`
		Average       float64 `json:"average"`
		AverageLast10 float64 `json:"averageLast10"`
	} `json:"raw"`
	Acc struct {
		Highest       float64 `json:"highest"`
		Average       float64 `json:"average"`
		AverageLast10 float64 `json:"averageLast10"`
	} `json:"acc"`
	Consistency struct {
		Highest       float64 `json:"highest"`
		Average       float64 `json:"average"`
		AverageLast10 float64 `json:"averageLast10"`
	} `json:"consistency"`
	Streak struct {
		Current int32 `json:"current"`
		Best    int32 `json:"best"`
	} `json:"streak"`
	Languages []struct {
		Lang  string `json:"lang"`
		Tests int64  `json:"tests"`
	} `json:"languages"`
}

// Every profile route is session-scoped: the caller's own data or a 401 —
// there is no handle for anyone else's profile at all (privacy by absent code
// path, docs/PROFILE.md).
func TestProfileRequiresSession(t *testing.T) {
	h := newHarness(t)
	for _, path := range []string{
		"/api/v1/profile/summary",
		"/api/v1/profile/activity",
		"/api/v1/profile/histogram",
		"/api/v1/profile/timeseries",
		"/api/v1/profile/pbs",
	} {
		requireStatus(t, h.get(path), http.StatusUnauthorized)
	}
}

// A fresh account reads zeroes everywhere — honest empty states, no NaN, no
// division errors, and empty (not absent) collections.
func TestProfileFreshAccountReadsZeroes(t *testing.T) {
	h := newHarness(t)
	h.login("fresh@example.com", "correct horse battery", "freshest")

	summary := decodeInto[profileSummaryResp](t, h.get("/api/v1/profile/summary"))
	assert.Equal(t, "freshest", summary.DisplayName)
	assert.NotEmpty(t, summary.Joined)
	assert.Zero(t, summary.TestsStarted)
	assert.Zero(t, summary.TestsCompleted)
	assert.Zero(t, summary.RestartsPerCompleted)
	assert.Zero(t, summary.TimeTypingMs)
	assert.Zero(t, summary.EstimatedWordsTyped)
	assert.Zero(t, summary.Wpm.Highest)
	assert.Zero(t, summary.Consistency.Average)
	assert.Zero(t, summary.Streak.Current)
	assert.Zero(t, summary.Streak.Best)
	assert.Empty(t, summary.Languages)

	activity := decodeInto[struct {
		Days []any `json:"days"`
	}](t, h.get("/api/v1/profile/activity"))
	assert.Empty(t, activity.Days)

	histogram := decodeInto[struct {
		Buckets []any `json:"buckets"`
	}](t, h.get("/api/v1/profile/histogram"))
	assert.Empty(t, histogram.Buckets)

	series := decodeInto[struct {
		Days       []any   `json:"days"`
		WpmPerHour float64 `json:"wpmPerHour"`
	}](t, h.get("/api/v1/profile/timeseries"))
	assert.Empty(t, series.Days)
	assert.Zero(t, series.WpmPerHour)

	pbs := decodeInto[struct {
		PBs []any `json:"pbs"`
	}](t, h.get("/api/v1/profile/pbs"))
	assert.Empty(t, pbs.PBs)
}

// The summary aggregates real judged runs: counters fold restarts in, metric
// groups agree with the server's own per-run numbers, the streak sees today,
// and the language counts see every submission.
func TestProfileSummaryAggregatesJudgedRuns(t *testing.T) {
	h := newHarness(t)
	h.login("aggregates@example.com", "correct horse battery", "aggregator")

	// Two distinct runs (different vectors ⇒ different metrics), with
	// client-reported restarts on each: 2 + 3 restarts, 2 completions.
	first := goldenPayload(t, "time-clean")
	first["restartsSinceLastSubmit"] = 2
	requireStatus(t, h.post("/api/v1/runs", first), http.StatusAccepted)
	second := goldenPayload(t, "words-consistency-chars")
	second["restartsSinceLastSubmit"] = 3
	requireStatus(t, h.post("/api/v1/runs", second), http.StatusAccepted)

	h.replayOnce(t)

	// Collect the server's own numbers from the run summaries — the profile
	// must agree with THESE, not with the client's report.
	list := decodeInto[struct {
		Runs []struct {
			Status        string `json:"status"`
			ServerMetrics struct {
				Wpm         float64 `json:"wpm"`
				Raw         float64 `json:"raw"`
				Accuracy    float64 `json:"accuracy"`
				Consistency float64 `json:"consistency"`
				DurationSec float64 `json:"durationSec"`
				Spaces      float64 `json:"spaces"`
				Chars       struct {
					Correct   float64 `json:"correct"`
					Incorrect float64 `json:"incorrect"`
					Extra     float64 `json:"extra"`
					Missed    float64 `json:"missed"`
				} `json:"chars"`
			} `json:"serverMetrics"`
		} `json:"runs"`
	}](t, h.get("/api/v1/runs"))
	require.Len(t, list.Runs, 2)
	var wpandSum, rawSum, accSum, consSum, timeMs, words float64
	wpmHigh, consHigh := 0.0, 0.0
	for _, run := range list.Runs {
		require.Equal(t, "accepted", run.Status)
		m := run.ServerMetrics
		wpandSum += m.Wpm
		rawSum += m.Raw
		accSum += m.Accuracy
		consSum += m.Consistency
		wpmHigh = math.Max(wpmHigh, m.Wpm)
		consHigh = math.Max(consHigh, m.Consistency)
		timeMs += m.DurationSec * 1000
		words += (m.Chars.Correct + m.Chars.Incorrect + m.Chars.Extra + m.Spaces) / 5
	}

	summary := decodeInto[profileSummaryResp](t, h.get("/api/v1/profile/summary"))
	assert.EqualValues(t, 2, summary.TestsCompleted)
	assert.EqualValues(t, 7, summary.TestsStarted, "started = completed + Σ restarts")
	assert.InDelta(t, 2.5, summary.RestartsPerCompleted, 1e-9)
	assert.InDelta(t, timeMs, float64(summary.TimeTypingMs), 1.0)
	assert.InDelta(t, words, float64(summary.EstimatedWordsTyped), 1.0)

	assert.InDelta(t, wpmHigh, summary.Wpm.Highest, 1e-9)
	assert.InDelta(t, wpandSum/2, summary.Wpm.Average, 1e-9)
	assert.InDelta(t, wpandSum/2, summary.Wpm.AverageLast10, 1e-9)
	assert.InDelta(t, rawSum/2, summary.Raw.Average, 1e-9)
	assert.InDelta(t, accSum/2, summary.Acc.Average, 1e-9)
	assert.InDelta(t, consHigh, summary.Consistency.Highest, 1e-9)
	assert.InDelta(t, consSum/2, summary.Consistency.Average, 1e-9)
	assert.True(t, summary.Consistency.Highest > 0 && summary.Consistency.Highest <= 1,
		"consistency travels as a [0, 1] fraction")

	// Played today ⇒ a live one-day streak, and german twice in the counts.
	assert.EqualValues(t, 1, summary.Streak.Current)
	assert.EqualValues(t, 1, summary.Streak.Best)
	require.Len(t, summary.Languages, 1)
	assert.Equal(t, "german", summary.Languages[0].Lang)
	assert.EqualValues(t, 2, summary.Languages[0].Tests)
}

// Activity, histogram and timeseries all read the same judged runs, bucketed
// three different ways; the timeseries additionally carries the regression
// header stat.
func TestProfileActivityHistogramTimeseries(t *testing.T) {
	h := newHarness(t)
	h.login("series@example.com", "correct horse battery", "seriesguy")

	requireStatus(t, h.post("/api/v1/runs", goldenPayload(t, "time-clean")), http.StatusAccepted)
	requireStatus(t, h.post("/api/v1/runs", goldenPayload(t, "words-consistency-chars")), http.StatusAccepted)
	h.replayOnce(t)

	today := time.Now().UTC().Format("2006-01-02")

	activity := decodeInto[struct {
		Days []struct {
			Date   string `json:"date"`
			Tests  int32  `json:"tests"`
			TimeMs int64  `json:"timeMs"`
		} `json:"days"`
	}](t, h.get("/api/v1/profile/activity?days=30"))
	require.Len(t, activity.Days, 1)
	assert.Equal(t, today, activity.Days[0].Date)
	assert.EqualValues(t, 2, activity.Days[0].Tests)
	assert.Positive(t, activity.Days[0].TimeMs)

	histogram := decodeInto[struct {
		Buckets []struct {
			Wpm   int32 `json:"wpm"`
			Tests int32 `json:"tests"`
		} `json:"buckets"`
	}](t, h.get("/api/v1/profile/histogram"))
	require.NotEmpty(t, histogram.Buckets)
	total := int32(0)
	for _, b := range histogram.Buckets {
		assert.Zero(t, b.Wpm%10, "buckets are 10-wpm lower bounds")
		assert.Positive(t, b.Tests)
		total += b.Tests
	}
	assert.EqualValues(t, 2, total, "every accepted run lands in exactly one bucket")

	series := decodeInto[struct {
		Days []struct {
			Date         string  `json:"date"`
			TimeTypingMs int64   `json:"timeTypingMs"`
			AvgWpm       float64 `json:"avgWpm"`
			AvgAcc       float64 `json:"avgAcc"`
		} `json:"days"`
		WpmPerHour float64 `json:"wpmPerHour"`
	}](t, h.get("/api/v1/profile/timeseries"))
	require.Len(t, series.Days, 1)
	assert.Equal(t, today, series.Days[0].Date)
	assert.Positive(t, series.Days[0].TimeTypingMs)
	assert.Positive(t, series.Days[0].AvgWpm)
	assert.True(t, series.Days[0].AvgAcc > 0 && series.Days[0].AvgAcc <= 1)
	assert.False(t, math.IsNaN(series.WpmPerHour))

	// A range that excludes today is empty with a zero slope — the range
	// filter's "no data" answer.
	past := decodeInto[struct {
		Days       []any   `json:"days"`
		WpmPerHour float64 `json:"wpmPerHour"`
	}](t, h.get("/api/v1/profile/timeseries?from=2020-01-01&to=2020-12-31"))
	assert.Empty(t, past.Days)
	assert.Zero(t, past.WpmPerHour)

	// Malformed bounds are refused, not guessed at.
	requireStatus(t, h.get("/api/v1/profile/timeseries?from=notadate"), http.StatusBadRequest)
	requireStatus(t, h.get("/api/v1/profile/activity?days=x"), http.StatusBadRequest)
}

// The PB cards are the leaderboard entries, verbatim, decorated with the
// parsed bucket — and they cost zero new computation because the projection
// already keeps one best run per (player, bucket).
func TestProfilePBsMirrorTheLeaderboardEntries(t *testing.T) {
	h := newHarness(t)
	h.login("pbs@example.com", "correct horse battery", "pbholder")

	// time-clean sits in a ranked bucket (15 s), so judging it projects a PB.
	requireStatus(t, h.post("/api/v1/runs", goldenPayload(t, "time-clean")), http.StatusAccepted)
	h.replayOnce(t)

	pbs := decodeInto[struct {
		PBs []struct {
			Bucket     string          `json:"bucket"`
			Mode       string          `json:"mode"`
			DurationMs *int32          `json:"durationMs"`
			WordCount  *int32          `json:"wordCount"`
			Lang       string          `json:"lang"`
			TextSource string          `json:"textSource"`
			RunID      string          `json:"runId"`
			Score      int64           `json:"score"`
			Wpm        float64         `json:"wpm"`
			Acc        float64         `json:"acc"`
			Grade      string          `json:"grade"`
			Mods       json.RawMessage `json:"mods"`
			AchievedAt time.Time       `json:"achievedAt"`
		} `json:"pbs"`
	}](t, h.get("/api/v1/profile/pbs"))
	require.Len(t, pbs.PBs, 1)
	pb := pbs.PBs[0]
	assert.Equal(t, "time:15000:german:seeded", pb.Bucket)
	assert.Equal(t, "time", pb.Mode)
	require.NotNil(t, pb.DurationMs)
	assert.EqualValues(t, 15000, *pb.DurationMs)
	assert.Nil(t, pb.WordCount)
	assert.Equal(t, "german", pb.Lang)
	assert.Equal(t, "seeded", pb.TextSource)
	assert.Equal(t, "SS", pb.Grade)
	assert.NotEmpty(t, pb.Mods)

	// Byte-for-byte the board's own row: same run, same score.
	entries := h.boardEntries(pb.Bucket)
	require.Len(t, entries, 1)
	assert.Equal(t, entries[0]["runId"], pb.RunID)
	assert.EqualValues(t, entries[0]["score"].(float64), float64(pb.Score))
}

// GET /runs summaries carry the profile table's derived cells — additive, and
// exactly the values the SQL derivations produce (B4 of the profile arc).
func TestRunsListCarriesDerivedProfileCells(t *testing.T) {
	h := newHarness(t)
	h.login("derived@example.com", "correct horse battery", "derived")

	payload := goldenPayload(t, "words-consistency-chars")
	requireStatus(t, h.post("/api/v1/runs", payload), http.StatusAccepted)

	// Pending: no verdict, so no grade/consistency/chars — but mods are a
	// property of the setup and are already there.
	pending := decodeInto[map[string]any](t, h.get("/api/v1/runs"))
	row := pending["runs"].([]any)[0].(map[string]any)
	assert.NotContains(t, row, "grade")
	assert.NotContains(t, row, "consistency")
	assert.NotContains(t, row, "chars")
	assert.NotContains(t, row, "quoteId")
	assert.Contains(t, row, "mods")

	h.replayOnce(t)

	judged := decodeInto[map[string]any](t, h.get("/api/v1/runs"))
	row = judged["runs"].([]any)[0].(map[string]any)
	serverMetrics := row["serverMetrics"].(map[string]any)

	// The lifted cells agree with the documents they were lifted from.
	assert.Equal(t, serverMetrics["consistency"], row["consistency"])
	assert.Equal(t, serverMetrics["chars"], row["chars"])
	grade, ok := row["grade"].(string)
	require.True(t, ok, "an accepted run carries its grade")
	acc := serverMetrics["accuracy"].(float64)
	assert.Equal(t, gradeOf(acc), grade, "run_grade mirrors the core's thresholds")
	assert.NotContains(t, row, "quoteId", "a seeded run names no quote")

	mods := row["mods"].(map[string]any)
	setup := payload["setup"].(map[string]any)
	generation := setup["generation"].(map[string]any)
	assert.Equal(t, generation["punctuation"], mods["punctuation"])
	assert.Equal(t, setup["config"].(map[string]any)["difficulty"], mods["difficulty"])
}

// gradeOf restates the SS/S/A/B/C thresholds (SCORING_CONCEPT §4) for the
// derived-cells assertion. The authoritative fence between SQL and the core is
// internal/leaderboard's TestGradeMatchesTheCore; this is just the test's own
// arithmetic.
func gradeOf(acc float64) string {
	switch {
	case acc >= 1:
		return "SS"
	case acc >= 0.98:
		return "S"
	case acc >= 0.95:
		return "A"
	case acc >= 0.9:
		return "B"
	default:
		return "C"
	}
}

// A quote run's summary lifts the quote id into the derived cells — the handle
// the profile table's quote link needs. It is there from ingestion (the id is
// a property of the setup, not of the verdict).
func TestRunsListLiftsTheQuoteId(t *testing.T) {
	h := newHarness(t)
	h.login("quotecell@example.com", "correct horse battery", "quotecell")

	vector := goldenVector(t, "quote-fixed-text")
	require.NotNil(t, vector.Quote)
	h.publishQuote(t, vector.Quote.ID, vector.Quote.Text, vector.Quote.Hash, false)
	requireStatus(t, h.post("/api/v1/runs", vector.Payload), http.StatusAccepted)

	list := decodeInto[map[string]any](t, h.get("/api/v1/runs"))
	row := list["runs"].([]any)[0].(map[string]any)
	assert.Equal(t, vector.Quote.ID.String(), row["quoteId"],
		"run_quote_id lifts the setup's quote reference")
}
