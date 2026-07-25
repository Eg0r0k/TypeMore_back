package perf

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// Plan is a parsed EXPLAIN (FORMAT JSON) tree, flattened for assertion.
//
// The reason plans are asserted at all: a latency budget catches a regression
// only on the machine and the data volume the budget was written for. A dropped
// index makes CI slow before it makes CI red, and "slow" is exactly what a busy
// reviewer waves through. "This query must not sequentially scan `runs`" is a
// statement about the SHAPE of the execution, true on every machine and at every
// volume — and a migration that drops the index turns it red immediately.
type Plan struct {
	// Raw is the EXPLAIN output as text, for the failure message.
	Raw string
	// Nodes is every node type in the tree, in traversal order
	// ("Index Scan", "Seq Scan", "Sort", …).
	Nodes []string
	// Relations maps a node type to the relations it was applied to, so
	// "no Seq Scan on runs" can be distinguished from "no Seq Scan anywhere"
	// (a seq scan of a six-row lookup table is fine and forever).
	Relations map[string][]string
	// SortMethods collects every "Sort Method" reported, which is how an
	// external merge (spilling to disk) is detected.
	SortMethods []string
	// TotalMs is the actual execution time when the plan was ANALYZEd.
	TotalMs float64
}

// Explain runs EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) and parses the result.
//
// ANALYZE really executes the statement. Callers pass read-only queries, or
// wrap the call in a transaction they roll back.
func Explain(ctx context.Context, db Querier, sql string, args ...any) (Plan, error) {
	return explain(ctx, db, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) ", sql, args...)
}

// ExplainOnly plans without executing — for statements that write.
func ExplainOnly(ctx context.Context, db Querier, sql string, args ...any) (Plan, error) {
	return explain(ctx, db, "EXPLAIN (FORMAT JSON) ", sql, args...)
}

// Querier is the pgx surface these helpers need: a pool, a conn, or a tx.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

func explain(ctx context.Context, db Querier, prefix, sql string, args ...any) (Plan, error) {
	rows, err := db.Query(ctx, prefix+sql, args...)
	if err != nil {
		return Plan{}, fmt.Errorf("perf: explain: %w", err)
	}
	defer rows.Close()

	var raw []byte
	for rows.Next() {
		if err := rows.Scan(&raw); err != nil {
			return Plan{}, fmt.Errorf("perf: explain scan: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		return Plan{}, fmt.Errorf("perf: explain rows: %w", err)
	}

	var doc []struct {
		Plan          map[string]any `json:"Plan"`
		ExecutionTime float64        `json:"Execution Time"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return Plan{}, fmt.Errorf("perf: explain parse: %w", err)
	}
	if len(doc) == 0 {
		return Plan{}, fmt.Errorf("perf: explain returned no plan")
	}

	p := Plan{Raw: string(raw), Relations: map[string][]string{}, TotalMs: doc[0].ExecutionTime}
	p.walk(doc[0].Plan)
	return p, nil
}

func (p *Plan) walk(node map[string]any) {
	nodeType, _ := node["Node Type"].(string)
	if nodeType != "" {
		p.Nodes = append(p.Nodes, nodeType)
		if rel, ok := node["Relation Name"].(string); ok {
			p.Relations[nodeType] = append(p.Relations[nodeType], rel)
		}
	}
	if method, ok := node["Sort Method"].(string); ok {
		p.SortMethods = append(p.SortMethods, method)
	}
	if kids, ok := node["Plans"].([]any); ok {
		for _, k := range kids {
			if m, ok := k.(map[string]any); ok {
				p.walk(m)
			}
		}
	}
	// A CTE / subplan hangs off the same key in modern Postgres, but InitPlan
	// nodes can also appear under "Plans" only — nothing else to traverse.
}

// Has reports whether any node of that type is in the plan.
func (p Plan) Has(nodeType string) bool {
	for _, n := range p.Nodes {
		if n == nodeType {
			return true
		}
	}
	return false
}

// HasAny reports whether any of the node types is in the plan — for the several
// spellings of the same idea ("Index Scan" / "Index Only Scan" / "Bitmap Index
// Scan").
func (p Plan) HasAny(nodeTypes ...string) bool {
	for _, t := range nodeTypes {
		if p.Has(t) {
			return true
		}
	}
	return false
}

// ScansSequentially reports whether the plan sequentially scans the named
// relation. Scoped to one relation on purpose: a Seq Scan of `bans` (a handful
// of rows, forever) is correct, and a blanket ban on the node type would make
// this assertion something people disable.
func (p Plan) ScansSequentially(relation string) bool {
	for _, rel := range p.Relations["Seq Scan"] {
		if rel == relation {
			return true
		}
	}
	return false
}

// SpillsToDisk reports whether any sort used an external merge — the signal that
// the working set outgrew work_mem, which is a cliff rather than a slope.
func (p Plan) SpillsToDisk() bool {
	for _, m := range p.SortMethods {
		if strings.Contains(m, "external") {
			return true
		}
	}
	return false
}

// PlanAssertion is the per-zone plan contract, asserted by AssertPlan.
type PlanAssertion struct {
	Zone  string
	Query string
	// WantAny: the plan must contain at least one of these node types.
	WantAny []string
	// NoSeqScanOn: none of these relations may be scanned sequentially.
	NoSeqScanOn []string
	// NoSort forbids an explicit Sort node — the index must already deliver the
	// order. (An "Incremental Sort" is still a sort and still fails.)
	NoSort bool
	// AllowExternalSort permits a sort that spills; false is the default and
	// the right answer almost everywhere.
	AllowExternalSort bool
}

// AssertPlan holds a query to its plan contract, printing the plan on failure —
// a failure message that says "expected Index Scan" without showing what the
// planner actually chose costs the next person a debugging session.
func AssertPlan(t *testing.T, p Plan, a PlanAssertion) {
	t.Helper()

	fail := func(format string, args ...any) {
		t.Helper()
		t.Errorf("PLAN %s | %s | "+format+"\nnodes: %v\nplan:\n%s",
			append([]any{a.Zone, a.Query}, append(args, p.Nodes, indent(p.Raw))...)...)
	}

	if len(a.WantAny) > 0 && !p.HasAny(a.WantAny...) {
		fail("expected one of %v", a.WantAny)
	}
	for _, rel := range a.NoSeqScanOn {
		if p.ScansSequentially(rel) {
			fail("sequential scan of %q — an index this query depends on is missing", rel)
		}
	}
	if a.NoSort && (p.Has("Sort") || p.Has("Incremental Sort")) {
		fail("plan sorts; the index must supply the ordering")
	}
	if !a.AllowExternalSort && p.SpillsToDisk() {
		fail("sort spilled to disk (%v)", p.SortMethods)
	}
	t.Logf("PLAN OK %s | %s | %v | %.2f ms", a.Zone, a.Query, p.Nodes, p.TotalMs)
}

func indent(s string) string {
	return "  " + strings.ReplaceAll(s, "\n", "\n  ")
}
