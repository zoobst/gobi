package gobi

import (
	"fmt"
	"strings"
)

// ExplainPhysical returns a description of how the LazyFrame will be
// executed — which operators stream, which materialize, which scan
// strategy each source uses. Predicts the compilation choices
// Compile would make without actually building the executor tree
// (no goroutines started, no right-side materialization for joins,
// no file I/O). Cheap enough to call in tests and inspection loops.
//
// Format: outer-most node first, one node per line, two-space
// indent per depth. Each label is prefixed with the strategy
// ("Streaming*" or "Materialize*") so the choice is visible at a
// glance.
//
// Example (analytical pipeline over parquet):
//
//	StreamingAggregate(keys=[region], aggs=[value_sum])
//	  StreamingJoin(inner, left.user_id = right.user_id)
//	    Filter((col("value") > lit(100)))
//	      ScanFile[stream, parquet]("events.parquet", cols=[user_id value region])
//	    ScanFrame(100 rows × 2 cols)
func (lf *LazyFrame) ExplainPhysical() string {
	var sb strings.Builder
	explainPhysical(Optimize(lf.plan), &sb, 0)
	return sb.String()
}

func explainPhysical(p LogicalPlan, sb *strings.Builder, depth int) {
	for range depth {
		sb.WriteString("  ")
	}
	sb.WriteString(physicalLabel(p))
	sb.WriteByte('\n')
	for _, c := range p.Children() {
		explainPhysical(c, sb, depth+1)
	}
}

// physicalLabel returns the one-line label ExplainPhysical uses for
// a plan node. The label reflects the strategy Compile would pick
// for that node — matches the type-switch in Compile so the two
// stay in sync. If the compile logic changes, update this alongside.
func physicalLabel(p LogicalPlan) string {
	switch n := p.(type) {

	case *scanFrameNode:
		rows, cols := n.frame.Shape()
		return fmt.Sprintf("ScanFrame(%d rows × %d cols)", rows, cols)

	case *scanFileNode:
		// Predict the strategy Compile will pick, matching the
		// preference order in compileScanFile.
		strategy := "materialize"
		if n.streamRead != nil {
			strategy = "stream"
		}
		if n.parallelStream != nil {
			// Cost: calling parallelStream opens the parquet
			// footer to compute NumRowGroups. Cheap (few KB) but
			// non-zero — worth noting if this shows up in a hot
			// path.
			subs := n.parallelStream()
			if len(subs) > 1 {
				strategy = fmt.Sprintf("stream, workers=%d", len(subs))
			}
		}
		return fmt.Sprintf("ScanFile[%s] %s", strategy, n.String())

	case *emptyNode:
		return n.String()

	case *filterNode:
		return "StreamingFilter(" + n.cond.String() + ")"

	case *projectNode:
		return "Streaming" + n.String()

	case *withColumnNode:
		return "Streaming" + n.String()

	case *dropNode:
		return "Streaming" + n.String()

	case *limitNode:
		return "Streaming" + n.String()

	case *tailNode:
		return "Materialize" + n.String()

	case *sortNode:
		// Sort must see all rows — always materializes.
		return "Materialize" + n.String()

	case *aggregateNode:
		// Compile picks streaming when every Aggregation uses a
		// built-in Kind (no custom Fn). Match that here, and echo
		// the resolved worker count so users can see when Slice D's
		// partitioned build kicks in (workers>1) versus the serial
		// fast path (workers==1). Uses resolveWorkers() with no
		// per-op overrides, matching Compile.
		prefix := "Materialize"
		if allBuiltInAggs(n.aggs) {
			prefix = "Streaming"
			if w := resolveWorkers(); w > 1 {
				return fmt.Sprintf("%s%s [workers=%d]", prefix, n.String(), w)
			}
		}
		return prefix + n.String()

	case *joinNode:
		// Compile picks streaming for left-driven kinds (Inner,
		// Left, Semi, Anti). Right/Full route through the
		// materializing fallback until we do second-phase state.
		prefix := "Materialize"
		if canStreamJoin(n.kind) {
			prefix = "Streaming"
		}
		return prefix + n.String()
	}

	return fmt.Sprintf("Unknown(%T)", p)
}
