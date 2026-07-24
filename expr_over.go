package gobi

import (
	"fmt"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// -----------------------------------------------------------------------------
// Scalar aggregate ExprNodes — fluent Sum/Mean/Min/Max/Count on Expr.
//
// These reduce their inner column to a single value and broadcast it
// to every row of the input Frame. Standalone, they behave like a
// group-of-one aggregation (e.g. Col("v").Sum() gives a column of
// N rows all equal to sum(v)). Wrapped in .Over(partitionCols...),
// the aggregate is computed per partition and scattered back to the
// row's partition value.
// -----------------------------------------------------------------------------

// Sum returns an expression that evaluates to the sum of e's non-null
// values, broadcast to every input row. Combine with Over to get
// per-partition sums.
func (e Expr) Sum() Expr { return Expr{node: &scalarAggNode{inner: e.node, kind: AggSum}} }

// Mean returns an expression that evaluates to the arithmetic mean of
// e's non-null values, broadcast to every input row.
func (e Expr) Mean() Expr { return Expr{node: &scalarAggNode{inner: e.node, kind: AggMean}} }

// MinAgg returns an expression that evaluates to the minimum of e's
// non-null values, broadcast to every input row. Named MinAgg to
// avoid clashing with Series.Min-shaped methods that already exist.
func (e Expr) MinAgg() Expr { return Expr{node: &scalarAggNode{inner: e.node, kind: AggMin}} }

// MaxAgg returns an expression that evaluates to the maximum of e's
// non-null values, broadcast to every input row.
func (e Expr) MaxAgg() Expr { return Expr{node: &scalarAggNode{inner: e.node, kind: AggMax}} }

// Count returns an expression that evaluates to the count of e's
// non-null values, broadcast to every input row. Output is Int64.
func (e Expr) Count() Expr { return Expr{node: &scalarAggNode{inner: e.node, kind: AggCount}} }

// Over wraps a scalar aggregate expression with partition keys.
// e.Over("a", "b") computes the aggregate per unique (a, b) combination
// and broadcasts the group value back to every row in that group. The
// result Series has the same row order as the input Frame — this is
// the key contract that separates Over from GroupBy.
//
// Only the built-in Kinds (Sum, Mean, Min, Max, Count) chain cleanly
// with Over today. Custom Aggregator Fns are on the roadmap once the
// public Aggregator interface unifies with the internal aggAccumulator
// path (see CLAUDE.md's Track 2 deferred work).
func (e Expr) Over(partitionCols ...string) Expr {
	return Expr{node: &overNode{inner: e.node, partitionCols: partitionCols}}
}

// scalarAggNode reduces its inner column to a single value and
// broadcasts it to input length. Sum/Mean/Min/Max output types match
// the eager aggregate path (see accumulator OutputType methods).
type scalarAggNode struct {
	inner ExprNode
	kind  AggKind
}

func (n *scalarAggNode) Eval(input *Frame) (Series, error) {
	col, err := n.inner.Eval(input)
	if err != nil {
		return Series{}, err
	}
	// Compute the aggregate over every row.
	rows := make([]int, col.Len())
	for i := range rows {
		rows[i] = i
	}
	acc, err := newAccumulator(Aggregation{Kind: n.kind, Column: col.Name()})
	if err != nil {
		return Series{}, fmt.Errorf("%s: %w", n.kind, err)
	}
	if err := acc.Update(col, rows); err != nil {
		return Series{}, err
	}
	v := acc.Finalize()
	return broadcastScalar(v, acc.OutputType(), input.NumRows(), n.kind.String())
}

func (n *scalarAggNode) Type(schema *arrow.Schema) (arrow.DataType, error) {
	// Type inference: build a temporary accumulator to ask its
	// OutputType. Cheap — the accumulator has no state until Update.
	acc, err := newAccumulator(Aggregation{Kind: n.kind})
	if err != nil {
		return nil, err
	}
	// The accumulator's OutputType doesn't depend on input dtype for
	// most kinds; Sum/Min/Max ignore it, Mean is always Float64,
	// Count is always Int64.
	_, err = n.inner.Type(schema)
	if err != nil {
		return nil, err
	}
	return acc.OutputType(), nil
}

func (n *scalarAggNode) Children() []Expr { return []Expr{{node: n.inner}} }
func (n *scalarAggNode) String() string   { return fmt.Sprintf("%s(%s)", n.kind, n.inner) }

// overNode is a partition-aware wrapper around a scalar aggregate.
// Requires the inner ExprNode to be a *scalarAggNode — chains like
// col.Add(lit).Over(...) would need per-partition Frame views, which
// isn't scoped here.
type overNode struct {
	inner         ExprNode
	partitionCols []string
}

func (n *overNode) Eval(input *Frame) (Series, error) {
	agg, ok := n.inner.(*scalarAggNode)
	if !ok {
		return Series{}, fmt.Errorf("%w: Over requires a scalar aggregate (Sum/Mean/Min/Max/Count), got %T",
			ErrExprTypeMismatch, n.inner)
	}
	if len(n.partitionCols) == 0 {
		// Over() with no keys is just the un-partitioned aggregate.
		return agg.Eval(input)
	}
	// Evaluate the inner column expression once — same column is
	// sliced by row indices for each group.
	col, err := agg.inner.Eval(input)
	if err != nil {
		return Series{}, err
	}

	// Resolve partition columns.
	partCols := make([]Series, len(n.partitionCols))
	for i, name := range n.partitionCols {
		s, err := input.Column(name)
		if err != nil {
			return Series{}, fmt.Errorf("Over: %w", err)
		}
		partCols[i] = s
	}

	// Aligned + sorted fast path: if the input Frame carries a
	// PartitionMetadata claim proving rows are grouped by the
	// partition columns AND sorted by them (writer-enforced),
	// same-K rows are guaranteed contiguous. Skip the row →
	// group-id hash-map build; do a single-pass linear scan
	// detecting group boundaries and reduce per contiguous run.
	if overFastPathApplicable(input.PartitionMetadata(), n.partitionCols) {
		return n.evalContiguous(input, agg, col, partCols)
	}

	// Build row → group-id + group-id → rows[] in first-seen order.
	nRows := input.NumRows()
	rowToGroup := make([]int, nRows)
	keyToGID := map[string]int{}
	groupRows := [][]int{}
	var keyScratch []byte
	for row := range nRows {
		keyScratch = keyScratch[:0]
		keyScratch, err = composeCompositeKeyInto(keyScratch, partCols, row)
		if err != nil {
			return Series{}, fmt.Errorf("Over: partition key row %d: %w", row, err)
		}
		key := string(keyScratch)
		gid, ok := keyToGID[key]
		if !ok {
			gid = len(groupRows)
			keyToGID[key] = gid
			groupRows = append(groupRows, nil)
		}
		rowToGroup[row] = gid
		groupRows[gid] = append(groupRows[gid], row)
	}

	// Per-group aggregate.
	groupVals := make([]any, len(groupRows))
	var outType arrow.DataType
	for gid, rows := range groupRows {
		acc, err := newAccumulator(Aggregation{Kind: agg.kind, Column: col.Name()})
		if err != nil {
			return Series{}, fmt.Errorf("Over: %w", err)
		}
		if err := acc.Update(col, rows); err != nil {
			return Series{}, fmt.Errorf("Over: group %d: %w", gid, err)
		}
		groupVals[gid] = acc.Finalize()
		if outType == nil {
			outType = acc.OutputType()
		}
	}
	if outType == nil {
		// Zero-row input — fall back to the accumulator's declared type.
		acc, err := newAccumulator(Aggregation{Kind: agg.kind, Column: col.Name()})
		if err != nil {
			return Series{}, err
		}
		outType = acc.OutputType()
	}

	// Scatter per-row.
	pool := memory.DefaultAllocator
	b, err := builderForType(pool, outType)
	if err != nil {
		return Series{}, fmt.Errorf("Over: %w", err)
	}
	defer b.Release()
	for row := range nRows {
		v := groupVals[rowToGroup[row]]
		if v == nil {
			b.AppendNull()
			continue
		}
		if err := appendCustomValue(b, v); err != nil {
			return Series{}, fmt.Errorf("Over: emit row %d: %w", row, err)
		}
	}
	return buildSeries(agg.kind.String()+"_over", outType, b.NewArray()), nil
}

func (n *overNode) Type(schema *arrow.Schema) (arrow.DataType, error) {
	return n.inner.Type(schema)
}

func (n *overNode) Children() []Expr { return []Expr{{node: n.inner}} }
func (n *overNode) String() string {
	return fmt.Sprintf("%s.over(%v)", n.inner, n.partitionCols)
}

// overFastPathApplicable reports whether the aligned + sorted fast
// path is safe to take. Requires all three:
//
//  1. `Aligned(meta, partitionCols)` — meta must claim partitioning
//     on exactly partitionCols (ordered).
//  2. `meta.SortedBy` starts with partitionCols (a prefix suffices —
//     sorted by [K, ts] is fine for Over("K") because same-K rows
//     are still contiguous). Descending flags are ignored: same-K
//     rows are neighbors either way.
//  3. `meta.SortEnforced == true` — writer guaranteed the sort. A
//     hint-only sort would let Over produce silently-wrong results
//     if the actual data isn't ordered.
//
// Any failure falls through to the row→group-id hash-map path,
// which is correct for any input regardless of metadata claims.
func overFastPathApplicable(meta *PartitionMetadata, partitionCols []string) bool {
	if meta == nil || !meta.SortEnforced {
		return false
	}
	if !Aligned(meta, partitionCols) {
		return false
	}
	if len(meta.SortedBy) < len(partitionCols) {
		return false
	}
	for i, c := range partitionCols {
		if meta.SortedBy[i].Column != c {
			return false
		}
	}
	return true
}

// evalContiguous is the aligned + sorted fast path for overNode.
// Rows are guaranteed contiguous by partition key, so a linear scan
// detects group boundaries and reduces per contiguous run. Skips the
// `keyToGID` map, `rowToGroup` index, and `groupRows [][]int`
// allocations of the general path — the main saving. Correctness
// hinges on the caller having proven contiguity via
// overFastPathApplicable before dispatching here.
func (n *overNode) evalContiguous(input *Frame, agg *scalarAggNode, col Series, partCols []Series) (Series, error) {
	nRows := input.NumRows()

	// Discover the output type via a throwaway accumulator (cheap;
	// same call the general path makes per group).
	protoAcc, err := newAccumulator(Aggregation{Kind: agg.kind, Column: col.Name()})
	if err != nil {
		return Series{}, fmt.Errorf("Over: %w", err)
	}
	outType := protoAcc.OutputType()

	pool := memory.DefaultAllocator
	b, err := builderForType(pool, outType)
	if err != nil {
		return Series{}, fmt.Errorf("Over: %w", err)
	}
	defer b.Release()

	if nRows == 0 {
		return buildSeries(agg.kind.String()+"_over", outType, b.NewArray()), nil
	}

	// Reusable row-index buffer for feeding acc.Update. Grown to
	// max-group-size on the fly; per-group Update takes a slice
	// view of the first (end-start) entries.
	rowsBuf := make([]int, 0, nRows)

	// Group-boundary detection via composite-key comparison of
	// current vs. previous row. Two scratch buffers, swapped each
	// time we advance across a boundary.
	curKey := make([]byte, 0, 32)
	nextKey := make([]byte, 0, 32)
	curKey, err = composeCompositeKeyInto(curKey, partCols, 0)
	if err != nil {
		return Series{}, fmt.Errorf("Over: partition key row 0: %w", err)
	}

	groupStart := 0
	// emit reduces rows [groupStart, groupEnd) and appends the
	// aggregate value to `b` groupEnd-groupStart times.
	emit := func(groupStart, groupEnd int) error {
		rowsBuf = rowsBuf[:0]
		for k := groupStart; k < groupEnd; k++ {
			rowsBuf = append(rowsBuf, k)
		}
		acc, err := newAccumulator(Aggregation{Kind: agg.kind, Column: col.Name()})
		if err != nil {
			return fmt.Errorf("Over: %w", err)
		}
		if err := acc.Update(col, rowsBuf); err != nil {
			return fmt.Errorf("Over: group [%d,%d): %w", groupStart, groupEnd, err)
		}
		v := acc.Finalize()
		for k := groupStart; k < groupEnd; k++ {
			if v == nil {
				b.AppendNull()
				continue
			}
			if err := appendCustomValue(b, v); err != nil {
				return fmt.Errorf("Over: emit row %d: %w", k, err)
			}
		}
		return nil
	}

	for row := 1; row < nRows; row++ {
		nextKey, err = composeCompositeKeyInto(nextKey[:0], partCols, row)
		if err != nil {
			return Series{}, fmt.Errorf("Over: partition key row %d: %w", row, err)
		}
		if bytesEqual(curKey, nextKey) {
			continue
		}
		if err := emit(groupStart, row); err != nil {
			return Series{}, err
		}
		groupStart = row
		curKey, nextKey = nextKey, curKey // swap: cur is now this row's key
	}
	// Final group [groupStart, nRows).
	if err := emit(groupStart, nRows); err != nil {
		return Series{}, err
	}
	return buildSeries(agg.kind.String()+"_over", outType, b.NewArray()), nil
}

// bytesEqual is a small inline byte-slice equality check. Kept local
// so the Over fast path doesn't pull in the "bytes" package just for
// one call site — cheaper for compile time and inline-cost.
func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// broadcastScalar builds a length-n Series where every row holds v.
// nil v produces an all-null series.
func broadcastScalar(v any, dtype arrow.DataType, n int, name string) (Series, error) {
	pool := memory.DefaultAllocator
	b, err := builderForType(pool, dtype)
	if err != nil {
		return Series{}, fmt.Errorf("%s broadcast: %w", name, err)
	}
	defer b.Release()
	for i := range n {
		if v == nil {
			b.AppendNull()
			continue
		}
		if err := appendCustomValue(b, v); err != nil {
			return Series{}, fmt.Errorf("%s broadcast row %d: %w", name, i, err)
		}
	}
	return buildSeries(name, dtype, b.NewArray()), nil
}
