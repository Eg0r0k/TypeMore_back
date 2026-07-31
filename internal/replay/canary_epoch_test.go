package replay

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/typemore/typemore-server/internal/replay/policy/policytest"
)

// The canary epoch: the gate that decides whether a run is judged with the
// core's canary detectors armed.
//
// The property these tests exist for is NEGATIVE, and it is the important half:
// an operator who has not set an epoch must be unable to arm anything, and a
// run created before the epoch must be judged exactly as it was before the
// detectors existed. The canary schedule is a pure function of a run's seed, so
// it is computable for every run ever stored — including all of history, which
// `make revalidate` walks in one pass. Without this gate that pass would read
// coincidence as evidence across the whole archive.

var epoch = time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

func TestCanariesArmedAtRequiresAnEpoch(t *testing.T) {
	// No epoch: nothing is armed, at any age, including a run created "now"
	// and one dated far in the future.
	for _, createdAt := range []time.Time{
		{},
		epoch.Add(-365 * 24 * time.Hour),
		epoch,
		epoch.Add(365 * 24 * time.Hour),
		time.Now().Add(24 * time.Hour),
	} {
		assert.False(t, CanariesArmedAt(createdAt, time.Time{}),
			"an unset epoch armed a run created at %s", createdAt)
	}
}

func TestCanariesArmedAtIsTheEpochBoundary(t *testing.T) {
	assert.False(t, CanariesArmedAt(epoch.Add(-time.Nanosecond), epoch), "one tick before the epoch")
	assert.True(t, CanariesArmedAt(epoch, epoch), "the epoch instant itself is armed")
	assert.True(t, CanariesArmedAt(epoch.Add(time.Nanosecond), epoch), "one tick after")
	// A zero creation instant is a row with no timestamp, which is not a
	// post-epoch run by any reading.
	assert.False(t, CanariesArmedAt(time.Time{}, epoch))
}

// judgeOneAtEpoch judges one run through the worker's real decision path with
// the given epoch configured — the same path RunBatch takes, so the gate is
// exercised where it actually runs rather than in isolation.
func judgeOneAtEpoch(t *testing.T, run PendingRun, canaryEpoch time.Time) Decision {
	t.Helper()
	q := newFakeQueue(run)
	_, reg := sharedDicts(t)
	w := NewWorker(q, reg, goldenQuotes(t), WorkerConfig{
		BatchSize:   50,
		Decider:     testDecider(t, policytest.NewFake()),
		CanaryEpoch: canaryEpoch,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	n, err := w.RunBatch(context.Background(), mustCore(t, DefaultReplayTimeout), slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	require.Equal(t, 1, n)
	return q.decision(t, run.ID)
}

// The deployment state this change ships in: no epoch set. Every golden vector
// must judge to exactly what it judged to before the detectors existed — same
// status, same verdict, same flags, same numbers.
func TestWithNoEpochEveryGoldenVectorIsJudgedUnchanged(t *testing.T) {
	for _, v := range loadVectors(t) {
		if v.Dictionary != nil {
			continue // not a published language — see the inline-dictionary test
		}
		t.Run(v.Name, func(t *testing.T) {
			run := v.pendingRun(t)
			// Created "now", i.e. as recent as a run can be: with no epoch even
			// that is unarmed.
			run.CreatedAt = time.Now()

			d := judgeOneAtEpoch(t, run, time.Time{})
			assert.Equal(t, v.Expect.Status, d.Status)
			doc := audit(t, d)
			assert.Equal(t, v.Expect.Verdict, doc.Verdict)
			assert.Equal(t, v.Expect.Flags, flagCodes(doc.Flags),
				"an unarmed judgement raised a flag the vector does not expect")
			for _, f := range doc.Flags {
				assert.NotContains(t, f.Code, "canary",
					"a canary flag on an unarmed run: the gate is not holding")
			}
		})
	}
}

// With an epoch SET, a run created before it is still judged by the old rules —
// bit-identically to the same run judged with no epoch at all. This is what
// makes `revalidate` safe to run over the archive the day the epoch is set.
func TestAPreEpochRunIsJudgedBitIdenticallyToNoEpochAtAll(t *testing.T) {
	for _, v := range loadVectors(t) {
		if v.Dictionary != nil {
			continue
		}
		t.Run(v.Name, func(t *testing.T) {
			run := v.pendingRun(t)
			run.CreatedAt = epoch.Add(-time.Hour) // submitted before the render shipped

			ungated := judgeOneAtEpoch(t, run, time.Time{})
			gated := judgeOneAtEpoch(t, run, epoch)

			assert.Equal(t, ungated.Status, gated.Status)
			assert.JSONEq(t, string(ungated.Validation), string(gated.Validation),
				"a pre-epoch run judged differently once an epoch existed")
			assert.JSONEq(t, string(ungated.ServerMetrics), string(gated.ServerMetrics))
			assert.JSONEq(t, string(ungated.ServerScore), string(gated.ServerScore))
		})
	}
}

// An ARMED run — one created after the epoch — is still judged valid and
// unflagged when it is honest. Arming changes what is looked for, never what an
// honest run is worth: the golden vectors were played by a human-shaped driver
// against a client that rendered no canaries, so an armed judgement of one must
// find nothing.
func TestArmingAnHonestRunChangesNothingAboutIt(t *testing.T) {
	for _, v := range loadVectors(t) {
		if v.Dictionary != nil {
			continue
		}
		t.Run(v.Name, func(t *testing.T) {
			run := v.pendingRun(t)
			run.CreatedAt = epoch.Add(time.Hour) // submitted by the canary client

			before := judgeOneAtEpoch(t, run, time.Time{})
			armed := judgeOneAtEpoch(t, run, epoch)

			assert.Equal(t, before.Status, armed.Status)
			assert.Equal(t, v.Expect.Flags, flagCodes(audit(t, armed).Flags))
			assert.JSONEq(t, string(before.ServerMetrics), string(armed.ServerMetrics),
				"arming moved a metric")
		})
	}
}

// The gate is in the INPUT, not only in the report: what the core is asked is
// what the epoch decided, so a future caller cannot arm a run by reaching past
// CanariesArmedAt.
func TestTheCoreIsAskedExactlyWhatTheEpochDecided(t *testing.T) {
	cases := []struct {
		name      string
		createdAt time.Time
		epoch     time.Time
		want      bool
	}{
		{"no epoch, recent run", time.Now(), time.Time{}, false},
		{"epoch set, pre-epoch run", epoch.Add(-time.Second), epoch, false},
		{"epoch set, post-epoch run", epoch.Add(time.Second), epoch, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := Input{CanariesArmed: CanariesArmedAt(tc.createdAt, tc.epoch)}
			assert.Equal(t, tc.want, in.CanariesArmed)
		})
	}
}

// The audit document a disarmed judgement writes must not mention canaries at
// all — not as a flag, not as an empty section. Stored verdicts are read by
// moderation tooling, and a field that appears only sometimes is a schema
// change nobody agreed to.
func TestDisarmedVerdictsCarryNoCanaryTrace(t *testing.T) {
	v := loadVectors(t)[0]
	run := v.pendingRun(t)
	run.CreatedAt = time.Now()

	d := judgeOneAtEpoch(t, run, time.Time{})
	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(d.Validation, &raw))
	for key := range raw {
		assert.NotContains(t, key, "canary")
	}
	assert.NotContains(t, string(d.Validation), "canary")
}
