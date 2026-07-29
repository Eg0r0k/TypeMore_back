package policy

import (
	"fmt"
	"math"
	"strconv"
)

// ColumnNone is what VersionNone stores in runs.policy_version. The column is a
// smallint and NULL already means "no rule set judged this run" — which is
// exactly what a Noop verdict is — so the two agree rather than needing a new
// value, and the verdict format does not move to accommodate this change.
//
// The consequence is the one that matters: `revalidate` claims runs whose
// policy_version IS NULL, so every run judged without a policy is re-judged
// automatically the day a policy is switched on. Nothing has to be remembered.
const ColumnNone int16 = 0

// ParseVersion maps a judge's version onto the smallint column, or reports why
// it cannot. It is called ONCE, when the worker is built, so an implementation
// with an unusable version stops the process at startup instead of writing
// something unreadable onto a run at three in the morning.
func ParseVersion(v string) (int16, error) {
	if v == VersionNone {
		return ColumnNone, nil
	}
	if v == "" {
		return 0, fmt.Errorf("policy: a judge's Version() is empty; use %q for a judge that does not judge", VersionNone)
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("policy: version %q is neither %q nor a decimal integer: %w", v, VersionNone, err)
	}
	if n <= 0 || n > math.MaxInt16 {
		return 0, fmt.Errorf("policy: version %q is out of range for the policy_version column (1..%d)", v, math.MaxInt16)
	}
	return int16(n), nil
}
