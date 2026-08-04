package runs_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/typemore/typemore-server/internal/runs"
	runspg "github.com/typemore/typemore-server/internal/runs/pgstore"
	"github.com/typemore/typemore-server/internal/runstatus"
)

// THE WAY BACK from a verdict, and the two properties that make it worth having.
//
// A run's status decides whether it holds a leaderboard slot
// (`leaderboard_eligible_runs` selects on `accepted`), and until this surface
// existed the replay worker was its only writer. That made every false positive
// permanent, which in turn made every threshold above it a hostage: a check
// whose mistakes cannot be undone has to be set where the blast radius allows
// rather than where the evidence says.
//
// Two things have to be true for the way back to be real, and neither is
// obvious from the endpoint existing:
//
//  1. the decision STICKS — the next revalidate pass must not quietly recompute
//     the worker's answer over the operator's;
//  2. the decision is RECORDED — who, when, from what, to what, and why.
func TestOperatorOverrideSticksAndIsRecorded(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	store := runspg.New(h.pool)

	admin := h.player(t, "override-admin")
	player := h.player(t, "override-player")

	// A judged run: flagged, exactly as an over-eager detector would leave a
	// fast honest player.
	runID := h.judgedRun(t, player, runstatus.Flagged)

	decision, err := store.OverrideRunStatus(ctx, runs.OverrideParams{
		RunID:     runID,
		ToStatus:  runstatus.Accepted,
		Reason:    "reviewed the log by hand: a real player, not a script",
		DecidedBy: admin,
	})
	require.NoError(t, err)
	assert.Equal(t, runstatus.Flagged, decision.FromStatus)
	assert.Equal(t, runstatus.Accepted, decision.ToStatus)
	assert.Equal(t, admin, decision.DecidedBy)
	assert.NotZero(t, decision.DecidedAt)

	assert.Equal(t, runstatus.Accepted, h.statusOf(t, runID),
		"the status the operator asked for is the status the run has")

	// (2) The trail. A decision that is not attributable is indistinguishable
	// from a mistake six months later.
	history, err := store.RunStatusOverrides(ctx, runID)
	require.NoError(t, err)
	require.Len(t, history, 1)
	assert.Equal(t, "reviewed the log by hand: a real player, not a script", history[0].Reason)
	assert.Equal(t, "override-admin", history[0].DecidedByName)

	// (1) The stickiness, which is the property the whole thing rests on. The
	// revalidation claim must step over this run — otherwise the override is a
	// suggestion, and the next core release writes the worker's verdict straight
	// back over the human's.
	claimed := h.claimable(t)
	assert.NotContains(t, claimed, runID,
		"a run a human decided about was offered to revalidate; the override would be overwritten")

	// A run nobody has decided about is still claimable — the filter must not
	// have turned revalidation off wholesale.
	untouched := h.judgedRun(t, player, runstatus.Flagged)
	assert.Contains(t, h.claimable(t), untouched,
		"revalidation stopped seeing ordinary runs")
}

// The refusals, which are about keeping the audit trail meaningful rather than
// about safety: a row that moves nothing, or that moves a run the worker has not
// judged yet, is a note that will later read as a decision.
func TestOperatorOverrideRefusesNonDecisions(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	store := runspg.New(h.pool)

	admin := h.player(t, "refusal-admin")
	player := h.player(t, "refusal-player")

	accepted := h.judgedRun(t, player, runstatus.Accepted)
	_, err := store.OverrideRunStatus(ctx, runs.OverrideParams{
		RunID: accepted, ToStatus: runstatus.Accepted,
		Reason: "no change", DecidedBy: admin,
	})
	assert.ErrorIs(t, err, runs.ErrStatusUnchanged)

	pending := h.judgedRun(t, player, runstatus.Pending)
	_, err = store.OverrideRunStatus(ctx, runs.OverrideParams{
		RunID: pending, ToStatus: runstatus.Accepted,
		Reason: "too early", DecidedBy: admin,
	})
	assert.ErrorIs(t, err, runs.ErrRunNotJudged,
		"a pending run has no verdict to disagree with")

	_, err = store.OverrideRunStatus(ctx, runs.OverrideParams{
		RunID: uuid.New(), ToStatus: runstatus.Accepted,
		Reason: "no such run", DecidedBy: admin,
	})
	assert.ErrorIs(t, err, runs.ErrNotFound)
}

// --- helpers -----------------------------------------------------------------

// judgedRun inserts one already-judged run directly: the submission plus the
// verdict row, which is what makes it visible to the revalidation claim (that
// claim joins run_verdicts — a verdict row IS the "judged" predicate).
func (h *harness) judgedRun(t *testing.T, userID uuid.UUID, status string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	var id uuid.UUID
	require.NoError(t, h.pool.QueryRow(ctx, `
		INSERT INTO runs (user_id, mode, duration_ms, lang, seed, dict_hash,
		                  score_version, setup, client_metrics, client_score, log,
		                  log_bytes, status)
		VALUES ($1, 'time', 15000, 'english', 42, 'deadbeef', 2,
		        '{"config":{},"generation":{}}'::jsonb,
		        '{"wpm":90,"raw":95,"acc":0.99}'::jsonb,
		        '{"version":2,"total":100}'::jsonb,
		        '\x1f8b'::bytea, 2, $2)
		RETURNING id`, userID, status).Scan(&id))
	if status != runstatus.Pending {
		_, err := h.pool.Exec(ctx, `
			INSERT INTO run_verdicts (run_id, user_id, server_metrics, server_score,
			                          validation, bundle_sha, policy_version, validated_at)
			VALUES ($1, $2,
			        '{"wpm":90,"raw":95,"accuracy":0.99}'::jsonb,
			        '{"version":2,"total":100}'::jsonb,
			        '{"verdict":"valid","flags":[],"policy":{"suspicion":0.6,"threshold":1}}'::jsonb,
			        'stale-on-purpose', 1, now())`, id, userID)
		require.NoError(t, err)
	}
	return id
}

func (h *harness) statusOf(t *testing.T, id uuid.UUID) string {
	t.Helper()
	var status string
	require.NoError(t, h.pool.QueryRow(context.Background(),
		`SELECT status FROM runs WHERE id = $1`, id).Scan(&status))
	return status
}

// claimable runs the revalidation claim's own predicate. Not the worker: the
// question is which rows the CLAIM offers, and running a whole worker to find
// out would drag a core and a registry into a test about one WHERE clause.
func (h *harness) claimable(t *testing.T) []uuid.UUID {
	t.Helper()
	rows, err := h.pool.Query(context.Background(), `
		SELECT r.id
		FROM runs r
		         JOIN run_verdicts v ON v.run_id = r.id
		WHERE (v.policy_version IS NULL
		    OR v.policy_version < 99
		    OR v.bundle_sha IS DISTINCT FROM 'current-bundle')
		  AND NOT EXISTS (SELECT 1 FROM run_status_overrides o WHERE o.run_id = r.id)`)
	require.NoError(t, err)
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		require.NoError(t, rows.Scan(&id))
		out = append(out, id)
	}
	require.NoError(t, rows.Err())
	return out
}

// player registers an account and returns its id.
func (h *harness) player(t *testing.T, name string) uuid.UUID {
	t.Helper()
	h.login(name+"@example.com", "sup3r-secret-pw", name)
	var id uuid.UUID
	require.NoError(t, h.pool.QueryRow(context.Background(),
		`SELECT id FROM users WHERE display_name = $1`, name).Scan(&id))
	return id
}
