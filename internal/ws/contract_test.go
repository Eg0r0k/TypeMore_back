package ws

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/typemore/typemore-server/internal/protocol"
)

// The match timings are a CROSS-REPO contract and were not treated as one. The
// server's AfkTrailingMs = 15 000 was asserted by no Go test at all — every test
// injects its own value through WithAfkKick — while the frontend carried its own
// 12 000 as a literal in a store and a second copy in a test. Two numbers that
// have to stay in a fixed relationship, neither pinned, both green whatever the
// other did.
//
// So the server's side of it becomes an artifact: contract/match-timings.json,
// generated from the constants and checked in. The Go tests below assert the
// VALUES (a constant cannot move without a failure here) and then assert the
// file still matches them (the artifact cannot go stale). The frontend reads the
// file and asserts its own thresholds sit strictly inside the server's — a
// client that kicks later than the server would let a player believe they are
// still racing after the server has dnf'd them.

// contractPath is the artifact's path relative to the repo root.
const contractPath = "contract/match-timings.json"

// updateContractEnv regenerates the artifact instead of asserting it. Set by
// `make contract`; there is no flag, because a test that rewrites the thing it
// is checking on an ordinary run is not a check.
const updateContractEnv = "TYPEMORE_UPDATE_CONTRACT"

// matchTimingContract is the artifact's shape. Every field is a threshold the
// client must know to stay inside the server's behaviour.
type matchTimingContract struct {
	Comment string `json:"$comment"`
	Source  string `json:"source"`
	Match   struct {
		AfkTrailingMs   int64   `json:"afkTrailingMs"`
		AfkKickShare    float64 `json:"afkKickShare"`
		AfkWarmupMs     int64   `json:"afkWarmupMs"`
		AfkBucketMs     int64   `json:"afkBucketMs"`
		FinishWindowMs  int64   `json:"finishWindowMs"`
		GraceMs         int64   `json:"graceMs"`
		CountdownLeadMs int64   `json:"countdownLeadMs"`
	} `json:"match"`
}

// TestMatchTimingsAreTheDocumentedValues asserts the numbers themselves. This is
// the test that was missing: the production constants were only ever exercised
// through overrides, so any of them could have been retyped without a single
// failure. Changing one here is meant to be work — the value is in a client, in
// docs/MATCH.md and docs/PROTOCOL.md, and in the frontend's own thresholds.
func TestMatchTimingsAreTheDocumentedValues(t *testing.T) {
	assert.Equal(t, 15_000, protocol.AfkTrailingMs,
		"the trailing-AFK rule: 15 s of silence dnf's a racing seat, in every mode")
	assert.InDelta(t, 0.6, protocol.AfkKickShare, 1e-12,
		"the share rule: 60 % idle over the elapsed window, words mode only")
	assert.Equal(t, 10_000, protocol.AfkWarmupMs,
		"the share rule is not judged before the window is 10 s old")
	assert.Equal(t, 1_000, protocol.AfkBucketMs,
		"the AFK accounting bucket, and the sweep's tick")
	assert.Equal(t, 120_000, protocol.FinishWindowMs,
		"the first finish opens a 120 s window on a words match")

	assert.Equal(t, 15*time.Second, graceDurationDefault,
		"the seat-reconnect grace window on any connection drop")
	assert.Equal(t, int64(5000), int64(countdownLeadMs),
		"how far ahead of now a countdown schedules t=0")

	// The relationships that make the numbers a system rather than a list.
	assert.Less(t, int64(protocol.AfkWarmupMs), int64(protocol.AfkTrailingMs),
		"a window too young for the share rule must still be old enough to have "+
			"survived the trailing rule, or the trailing rule judges an unjudgeable window")
	assert.Positive(t, protocol.AfkBucketMs)
	assert.Zero(t, protocol.AfkTrailingMs%protocol.AfkBucketMs,
		"the sweep only looks once per bucket, so a trailing rule that is not a "+
			"whole number of buckets does not mean what it says")
	assert.LessOrEqual(t, int64(protocol.AfkTrailingMs), graceDurationDefault.Milliseconds(),
		"the grace window must not be shorter than the silence a live seat is allowed")
}

// TestMatchTimingContractSnapshotIsCurrent asserts the checked-in artifact still
// says what the constants say. It never rewrites the file on an ordinary run.
func TestMatchTimingContractSnapshotIsCurrent(t *testing.T) {
	want := currentMatchTimingContract()
	encoded, err := json.MarshalIndent(want, "", "  ")
	require.NoError(t, err)
	encoded = append(encoded, '\n')

	path := repoPath(t, contractPath)
	if os.Getenv(updateContractEnv) != "" {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, encoded, 0o644))
		t.Logf("wrote %s", contractPath)
		return
	}

	raw, err := os.ReadFile(path)
	require.NoError(t, err, "%s is missing — run `make contract`", contractPath)
	assert.Equal(t, string(encoded), string(raw),
		"%s is stale: the constants moved and the artifact did not. Run `make contract`, "+
			"and remember the frontend reads this file — a threshold change is a change "+
			"on both sides.", contractPath)
}

// currentMatchTimingContract builds the artifact from the live constants.
func currentMatchTimingContract() matchTimingContract {
	var c matchTimingContract
	c.Comment = "Generated from the Go constants — do not edit by hand; run `make contract`. " +
		"Consumers must assert their own thresholds sit strictly INSIDE these: a client " +
		"that gives up later than the server shows a player still racing after the server dnf'd them."
	c.Source = "internal/protocol/protocol.go, internal/ws/room_match.go"
	c.Match.AfkTrailingMs = protocol.AfkTrailingMs
	c.Match.AfkKickShare = protocol.AfkKickShare
	c.Match.AfkWarmupMs = protocol.AfkWarmupMs
	c.Match.AfkBucketMs = protocol.AfkBucketMs
	c.Match.FinishWindowMs = protocol.FinishWindowMs
	c.Match.GraceMs = graceDurationDefault.Milliseconds()
	c.Match.CountdownLeadMs = countdownLeadMs
	return c
}

// repoPath resolves a repo-root-relative path from this package's directory.
func repoPath(t *testing.T, rel string) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)
	return filepath.Join(root, filepath.FromSlash(rel))
}
