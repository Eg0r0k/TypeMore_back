package perf

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// Budget is one asserted performance ceiling.
//
// The point of naming a budget rather than eyeballing a number is that CI can
// hold it. A regression that doubles a query's cost is a failing test, not a
// line in a report nobody reads — and a budget that is MISSED is reported with
// its measurement, never quietly widened.
type Budget struct {
	// Zone and Workload identify the row this lands on in docs/PERFORMANCE.md.
	Zone     string
	Workload string
	// Limit is the ceiling. Rationale says why THAT number: a budget without a
	// reason is a number someone will raise the first time it fails.
	Limit     time.Duration
	Rationale string
}

// Assert reports the measurement and fails if it exceeds the budget. The
// measurement is always logged, passing or not: the report table is built from
// these lines, so a green run still has to say what it saw.
func (b Budget) Assert(t *testing.T, measured time.Duration) {
	t.Helper()
	pct := float64(measured) / float64(b.Limit) * 100
	t.Logf("BUDGET %s | %s | measured %s | limit %s (%.0f%%) | %s",
		b.Zone, b.Workload, round(measured), round(b.Limit), pct, b.Rationale)
	if measured > b.Limit {
		t.Errorf("BUDGET MISSED %s | %s | measured %s exceeds %s by %.1f×",
			b.Zone, b.Workload, round(measured), round(b.Limit),
			float64(measured)/float64(b.Limit))
	}
}

// AssertBytes is the same contract for a memory ceiling.
func AssertBytes(t *testing.T, zone, workload string, measured, limit uint64, rationale string) {
	t.Helper()
	pct := float64(measured) / float64(limit) * 100
	t.Logf("BUDGET %s | %s | measured %s | limit %s (%.0f%%) | %s",
		zone, workload, MiB(measured), MiB(limit), pct, rationale)
	if measured > limit {
		t.Errorf("BUDGET MISSED %s | %s | measured %s exceeds %s",
			zone, workload, MiB(measured), MiB(limit))
	}
}

// Report logs a measurement that has no budget yet — a curve point, a
// throughput number, a ratio. It exists so the report table can be assembled
// from `go test -v` output instead of from memory.
func Report(t *testing.T, zone, what string, value any) {
	t.Helper()
	t.Logf("MEASURE %s | %s | %v", zone, what, value)
}

// MiB renders a byte count for humans.
func MiB(b uint64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.2f GiB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(b)/(1<<10))
	default:
		return fmt.Sprintf("%d B", b)
	}
}

// round trims a duration to something readable without losing the magnitude.
func round(d time.Duration) time.Duration {
	switch {
	case d >= time.Second:
		return d.Round(time.Millisecond)
	case d >= time.Millisecond:
		return d.Round(10 * time.Microsecond)
	default:
		return d.Round(time.Nanosecond)
	}
}

// Percentile returns the p-th percentile (0..100) of samples, which it sorts in
// place. Nearest-rank, because interpolating between two samples invents a
// latency that never happened.
func Percentile(samples []time.Duration, p float64) time.Duration {
	if len(samples) == 0 {
		return 0
	}
	sortDurations(samples)
	idx := int(float64(len(samples)-1) * p / 100)
	return samples[idx]
}

// sortDurations sorts ascending. Sample sets here reach the hundreds of
// thousands (zone 7 records one latency per relayed batch), so this is
// slices.Sort, not a hand-rolled loop.
func sortDurations(s []time.Duration) {
	slices.Sort(s)
}

// Summary renders a latency distribution as one line.
func Summary(samples []time.Duration) string {
	if len(samples) == 0 {
		return "no samples"
	}
	var total time.Duration
	for _, s := range samples {
		total += s
	}
	var b strings.Builder
	fmt.Fprintf(&b, "n=%d mean=%s p50=%s p95=%s p99=%s max=%s",
		len(samples), round(total/time.Duration(len(samples))),
		round(Percentile(samples, 50)), round(Percentile(samples, 95)),
		round(Percentile(samples, 99)), round(Percentile(samples, 100)))
	return b.String()
}
