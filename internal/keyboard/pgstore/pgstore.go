// Package pgstore implements the keyboard projection against Postgres: the
// replay worker's KeyboardProjector seam, folding the core's per-character
// observations through the layouts asset into user_keyboard_profile rows —
// inside the verdict transaction, exactly-once per run via the
// keyboard_projected_runs stamp table (migrations 00016, 00020). The stamp is
// presence: a row means "this run's contribution is counted", stamping is an
// INSERT, unstamping is a DELETE — and both tables written here belong to this
// domain; runs is only ever read.
package pgstore

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/typemore/typemore-server/internal/keyboard"
	"github.com/typemore/typemore-server/internal/replay"
)

// Projector implements replay/pgstore.KeyboardProjector.
type Projector struct {
	layouts *keyboard.Layouts
}

// New builds the projector over the parsed layouts asset.
func New(layouts *keyboard.Layouts) *Projector {
	return &Projector{layouts: layouts}
}

// keyDelta is one physical key's contribution from a single run.
type keyDelta struct {
	presses, errors, intervals int64
	intervalSum                float64
}

// ProjectKeyboard applies or reverses one run's keyboard contribution.
//
// The accounting is exactly-once by the stamp, in both directions:
//
//	accepted  && !stamped → ADD the contribution, stamp
//	!accepted && stamped  → REVERSE it, unstamp   (a demotion)
//	anything else         → no-op                  (already counted, or nothing to count)
//
// The reversal recomputes from THIS replay's observations, which is
// bit-identical under the bundle that added them; across a bundle change a
// reversal can leave a small residue, which `make revalidate`'s full pass —
// re-judging every run the bundle change touched — walks back out. A demotion
// whose log no longer replays at all (observations nil) leaves the
// contribution in place and the stamp on: counted evidence from a run that WAS
// accepted beats silently unbalancing the aggregates.
func (p *Projector) ProjectKeyboard(ctx context.Context, tx pgx.Tx, runID uuid.UUID,
	accepted bool, observations []replay.CharObservation) error {
	// The stamp read is serialized by the verdict transaction's claim lock on
	// the runs row (FOR UPDATE SKIP LOCKED), not by this query — no two verdict
	// transactions ever hold the same run.
	var userID uuid.UUID
	var lang string
	var stamped bool
	if err := tx.QueryRow(ctx,
		`SELECT r.user_id, r.lang, (k.run_id IS NOT NULL)
		 FROM runs r
		          LEFT JOIN keyboard_projected_runs k ON k.run_id = r.id
		 WHERE r.id = $1`, runID).
		Scan(&userID, &lang, &stamped); err != nil {
		return fmt.Errorf("keyboard/pgstore: read run %s: %w", runID, err)
	}

	add := accepted && !stamped && len(observations) > 0
	reverse := !accepted && stamped && len(observations) > 0
	if !add && !reverse {
		return nil
	}

	// Fold chars onto physical keys. Distinct chars per run are bounded by the
	// alphabet (~dozens), so this map is tiny whatever the log's size.
	deltas := make(map[string]*keyDelta)
	for _, o := range observations {
		id := p.layouts.KeyOf(lang, o.Char)
		d := deltas[id]
		if d == nil {
			d = &keyDelta{}
			deltas[id] = d
		}
		d.presses += o.Presses
		d.errors += o.Errors
		d.intervalSum += o.IntervalSumMs
		d.intervals += o.IntervalCount
	}

	keys := make([]string, 0, len(deltas))
	presses := make([]int64, 0, len(deltas))
	errCounts := make([]int64, 0, len(deltas))
	sums := make([]float64, 0, len(deltas))
	counts := make([]int64, 0, len(deltas))
	for id, d := range deltas {
		keys = append(keys, id)
		presses = append(presses, d.presses)
		errCounts = append(errCounts, d.errors)
		sums = append(sums, d.intervalSum)
		counts = append(counts, d.intervals)
	}

	if add {
		// One statement for the whole run: unnest the parallel arrays, upsert.
		if _, err := tx.Exec(ctx, `
			INSERT INTO user_keyboard_profile AS p
			       (user_id, key_id, presses, errors, interval_sum_ms, interval_count)
			SELECT $1, k, pr, er, s, c
			FROM unnest($2::text[], $3::bigint[], $4::bigint[], $5::float8[], $6::bigint[])
			     AS t(k, pr, er, s, c)
			ON CONFLICT (user_id, key_id) DO UPDATE SET
			    presses         = p.presses + EXCLUDED.presses,
			    errors          = p.errors + EXCLUDED.errors,
			    interval_sum_ms = p.interval_sum_ms + EXCLUDED.interval_sum_ms,
			    interval_count  = p.interval_count + EXCLUDED.interval_count`,
			userID, keys, presses, errCounts, sums, counts); err != nil {
			return fmt.Errorf("keyboard/pgstore: add contribution for run %s: %w", runID, err)
		}
	} else {
		// Reversal: subtract, floored at zero — a cross-bundle residue must
		// never drive a counter negative. Keys with no row simply don't match:
		// there is nothing there to reverse.
		if _, err := tx.Exec(ctx, `
			UPDATE user_keyboard_profile AS p SET
			    presses         = GREATEST(0, p.presses - t.pr),
			    errors          = GREATEST(0, p.errors - t.er),
			    interval_sum_ms = GREATEST(0, p.interval_sum_ms - t.s),
			    interval_count  = GREATEST(0, p.interval_count - t.c)
			FROM unnest($2::text[], $3::bigint[], $4::bigint[], $5::float8[], $6::bigint[])
			     AS t(k, pr, er, s, c)
			WHERE p.user_id = $1 AND p.key_id = t.k`,
			userID, keys, presses, errCounts, sums, counts); err != nil {
			return fmt.Errorf("keyboard/pgstore: reverse contribution for run %s: %w", runID, err)
		}
	}

	if add {
		if _, err := tx.Exec(ctx,
			`INSERT INTO keyboard_projected_runs (run_id) VALUES ($1)
			 ON CONFLICT (run_id) DO NOTHING`, runID); err != nil {
			return fmt.Errorf("keyboard/pgstore: stamp run %s: %w", runID, err)
		}
	} else {
		if _, err := tx.Exec(ctx,
			`DELETE FROM keyboard_projected_runs WHERE run_id = $1`, runID); err != nil {
			return fmt.Errorf("keyboard/pgstore: unstamp run %s: %w", runID, err)
		}
	}
	return nil
}
