package gobi

import (
	"fmt"
	"sort"

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

// Over wraps an expression with partition keys.
//
// Two shapes are supported depending on the inner:
//
//  1. **Scalar aggregate inner** (Sum/Mean/Min/Max/Count on a column):
//     the aggregate is computed per unique partition-key combination
//     and broadcast to every row in that group. This is the reduce-
//     and-scatter shape gobi has had since v0.2.0.
//
//  2. **Shape-preserving inner** (Shift, arithmetic chains like
//     `Col("v").Add(Lit(1.0))`, and other row-order-preserving
//     ExprNodes): the inner is evaluated separately on each
//     partition's rows and the per-row output is scattered back to
//     the original row positions. Input row order is preserved
//     within each partition (polars-parity default).
//
// If the shape-preserving transform is row-order sensitive (e.g.
// Shift) and you need a specific within-partition order, use
// OverOrdered — this variant does not sort within partitions.
func (e Expr) Over(partitionCols ...string) Expr {
	return Expr{node: &overNode{inner: e.node, partitionCols: partitionCols}}
}

// OverOrdered is Over with an explicit within-partition sort order.
// Rows in each partition are sorted by orderBy (multi-key,
// stable, nulls-last, per-key Descending) before the shape-preserving
// inner runs on them. Output row order matches input row order —
// the sort only affects what "previous row" / "next row" mean inside
// each partition.
//
// For scalar aggregate inners the orderBy is ignored (Sum, Mean,
// Min, Max, Count are order-invariant).
//
// Example: previous v within each K, ordered by t.
//
//	Col("v").Shift(1).OverOrdered([]string{"K"}, gobi.SortKey{Column: "t"})
func (e Expr) OverOrdered(partitionCols []string, orderBy ...SortKey) Expr {
	return Expr{node: &overNode{
		inner:         e.node,
		partitionCols: partitionCols,
		orderBy:       orderBy,
	}}
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

// overNode is a partition-aware wrapper. Two eval modes selected
// automatically by inner type:
//
//   - inner *scalarAggNode → reduce-and-scatter (existing v0.2 shape).
//   - anything else        → shape-preserving per-partition eval.
//
// orderBy is set only via OverOrdered; plain .Over(...) leaves it nil.
type overNode struct {
	inner         ExprNode
	partitionCols []string
	orderBy       []SortKey
}

func (n *overNode) Eval(input *Frame) (Series, error) {
	if agg, ok := n.inner.(*scalarAggNode); ok {
		return n.evalScalarAgg(input, agg)
	}
	return n.evalShapePreserving(input)
}

func (n *overNode) Type(schema *arrow.Schema) (arrow.DataType, error) {
	return n.inner.Type(schema)
}

func (n *overNode) Children() []Expr { return []Expr{{node: n.inner}} }
func (n *overNode) String() string {
	if len(n.orderBy) == 0 {
		return fmt.Sprintf("%s.over(%v)", n.inner, n.partitionCols)
	}
	orderParts := make([]string, len(n.orderBy))
	for i, k := range n.orderBy {
		if k.Descending {
			orderParts[i] = k.Column + " DESC"
		} else {
			orderParts[i] = k.Column
		}
	}
	return fmt.Sprintf("%s.over(%v).orderBy(%v)", n.inner, n.partitionCols, orderParts)
}

// -----------------------------------------------------------------------------
// evalScalarAgg: reduce-and-scatter path (unchanged from v0.2).
// -----------------------------------------------------------------------------

func (n *overNode) evalScalarAgg(input *Frame, agg *scalarAggNode) (Series, error) {
	if len(n.partitionCols) == 0 {
		// Over() with no keys is just the un-partitioned aggregate.
		// orderBy is meaningless for scalar aggregates; ignored.
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

// -----------------------------------------------------------------------------
// evalShapePreserving: per-partition eval + scatter-back path.
//
// Splits input into partitions by partition-key, optionally sorts each
// partition's rows by orderBy, evaluates the inner ExprNode on each
// partition's mini-Frame, and scatters values back into the output at
// the original row positions.
//
// Contract: the inner must be shape-preserving — its output length
// must equal the mini-Frame's length. Enforced at runtime; a length
// mismatch surfaces a helpful error rather than a silent misalignment.
// -----------------------------------------------------------------------------

func (n *overNode) evalShapePreserving(input *Frame) (Series, error) {
	nRows := input.NumRows()
	if len(n.partitionCols) == 0 {
		// No partitioning — evaluate inner directly. orderBy has no
		// meaning without a partition (the whole Frame is one group
		// and would need a top-level SortBy instead).
		return n.inner.Eval(input)
	}

	// Resolve partition + order columns.
	partCols := make([]Series, len(n.partitionCols))
	for i, name := range n.partitionCols {
		s, err := input.Column(name)
		if err != nil {
			return Series{}, fmt.Errorf("Over: %w", err)
		}
		partCols[i] = s
	}

	// Bucket rows into partitions. Aligned-input fast path walks
	// linear runs of same-key rows (no hash map). Otherwise hash-
	// bucket into partition slices in first-seen order.
	aligned := overShapeFastPathApplicable(input.PartitionMetadata(), n.partitionCols, n.orderBy)
	var partitions [][]int
	if aligned {
		partitions = collectContiguousPartitions(nRows, partCols)
	} else {
		partitions = collectHashedPartitions(nRows, partCols)
		// Only the general path needs a per-partition sort — the
		// aligned path is already in-order by orderBy by construction.
		if len(n.orderBy) > 0 {
			cmps, err := buildOrderComparators(input, n.orderBy)
			if err != nil {
				return Series{}, fmt.Errorf("Over.OrderBy: %w", err)
			}
			for _, rows := range partitions {
				sortRowIndicesBy(rows, cmps)
			}
		}
	}

	// Type inference on the inner expression against the input
	// schema. Shape-preserving inners must return the same arrow
	// type per partition, matching this.
	outType, err := n.inner.Type(input.Schema())
	if err != nil {
		return Series{}, fmt.Errorf("Over: type inference on inner: %w", err)
	}

	// Per-row output values. Arrow builders don't support random-
	// position writes, so we buffer values in a Go slice and emit
	// them in row order at the end. Nil = null; anything else is
	// consumed by appendCustomValue.
	outValues := make([]any, nRows)

	for _, rows := range partitions {
		if len(rows) == 0 {
			continue
		}
		miniFrame, err := input.take(rows)
		if err != nil {
			return Series{}, fmt.Errorf("Over: take partition: %w", err)
		}
		miniResult, err := n.inner.Eval(miniFrame)
		if err != nil {
			return Series{}, fmt.Errorf("Over: eval partition: %w", err)
		}
		if miniResult.Len() != len(rows) {
			return Series{}, fmt.Errorf(
				"Over: inner returned %d rows for a partition of %d — Over requires a shape-preserving inner",
				miniResult.Len(), len(rows))
		}
		for i, rowIdx := range rows {
			null, err := isNullAtSeries(miniResult, i)
			if err != nil {
				return Series{}, fmt.Errorf("Over: read partition row: %w", err)
			}
			if null {
				outValues[rowIdx] = nil
				continue
			}
			v, err := readScalarAt(miniResult, i)
			if err != nil {
				return Series{}, fmt.Errorf("Over: read partition row: %w", err)
			}
			outValues[rowIdx] = v
		}
	}

	pool := memory.DefaultAllocator
	b, err := builderForType(pool, outType)
	if err != nil {
		return Series{}, fmt.Errorf("Over: %w", err)
	}
	defer b.Release()
	for _, v := range outValues {
		if err := appendCustomValue(b, v); err != nil {
			return Series{}, fmt.Errorf("Over: emit: %w", err)
		}
	}
	return buildSeries("over", outType, b.NewArray()), nil
}

// -----------------------------------------------------------------------------
// Partition-collection helpers.
// -----------------------------------------------------------------------------

// collectHashedPartitions buckets rows into partitions by composite
// partition-key. Each returned []int is a partition's row indices in
// input row order. First-seen partition-key order determines the
// outer slice order.
func collectHashedPartitions(nRows int, partCols []Series) [][]int {
	keyToGID := map[string]int{}
	partitions := [][]int{}
	var keyScratch []byte
	for row := range nRows {
		keyScratch = keyScratch[:0]
		keyScratch, _ = composeCompositeKeyInto(keyScratch, partCols, row)
		key := string(keyScratch)
		gid, ok := keyToGID[key]
		if !ok {
			gid = len(partitions)
			keyToGID[key] = gid
			partitions = append(partitions, nil)
		}
		partitions[gid] = append(partitions[gid], row)
	}
	return partitions
}

// collectContiguousPartitions walks the input assuming rows are
// contiguous by partition key (guaranteed by the aligned fast-path
// gate). Each contiguous run becomes one entry in the returned slice
// of row-index slices.
func collectContiguousPartitions(nRows int, partCols []Series) [][]int {
	if nRows == 0 {
		return nil
	}
	partitions := [][]int{}
	curKey := make([]byte, 0, 32)
	nextKey := make([]byte, 0, 32)
	curKey, _ = composeCompositeKeyInto(curKey, partCols, 0)
	start := 0
	for row := 1; row < nRows; row++ {
		nextKey, _ = composeCompositeKeyInto(nextKey[:0], partCols, row)
		if bytesEqual(curKey, nextKey) {
			continue
		}
		partitions = append(partitions, rangeSlice(start, row))
		start = row
		curKey, nextKey = nextKey, curKey
	}
	partitions = append(partitions, rangeSlice(start, nRows))
	return partitions
}

// rangeSlice returns []int{start, start+1, ..., end-1}.
func rangeSlice(start, end int) []int {
	out := make([]int, 0, end-start)
	for i := start; i < end; i++ {
		out = append(out, i)
	}
	return out
}

// buildOrderComparators returns a rowComparator per orderBy key,
// suitable for stable multi-key sort of row indices into input.
func buildOrderComparators(input *Frame, orderBy []SortKey) ([]rowComparator, error) {
	cmps := make([]rowComparator, len(orderBy))
	for i, k := range orderBy {
		s, err := input.Column(k.Column)
		if err != nil {
			return nil, err
		}
		cmp, err := newRowComparator(s, k.Descending)
		if err != nil {
			return nil, fmt.Errorf("order key %q: %w", k.Column, err)
		}
		cmps[i] = cmp
	}
	return cmps, nil
}

// sortRowIndicesBy sorts rows in place by cmps (lex, stable,
// null-last per rowComparator semantics).
func sortRowIndicesBy(rows []int, cmps []rowComparator) {
	sort.SliceStable(rows, func(a, b int) bool {
		ra, rb := rows[a], rows[b]
		for _, cmp := range cmps {
			c := cmp(ra, rb)
			if c != 0 {
				return c < 0
			}
		}
		return false
	})
}

// -----------------------------------------------------------------------------
// Fast-path applicability
// -----------------------------------------------------------------------------

// overFastPathApplicable reports whether the aligned + sorted fast
// path is safe to take for the scalar-agg Over path. Requires all
// three:
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

// overShapeFastPathApplicable is the shape-preserving analogue. Same
// contiguity requirement as the scalar-agg path, plus a stricter
// within-partition-sort check: meta.SortedBy after the partition-key
// prefix must match orderBy exactly (column names AND Descending
// flags). If orderBy is nil, only the contiguity check applies —
// input row order within each partition is used as-is.
func overShapeFastPathApplicable(meta *PartitionMetadata, partitionCols []string, orderBy []SortKey) bool {
	if !overFastPathApplicable(meta, partitionCols) {
		return false
	}
	if len(orderBy) == 0 {
		return true
	}
	// SortedBy must extend beyond partitionCols to cover every
	// orderBy key, with matching Column + Descending.
	if len(meta.SortedBy) < len(partitionCols)+len(orderBy) {
		return false
	}
	for i, k := range orderBy {
		got := meta.SortedBy[len(partitionCols)+i]
		if got.Column != k.Column || got.Descending != k.Descending {
			return false
		}
	}
	return true
}

// evalContiguous is the aligned + sorted fast path for the scalar-
// agg Over. Rows are guaranteed contiguous by partition key, so a
// linear scan detects group boundaries and reduces per contiguous
// run. Skips the `keyToGID` map, `rowToGroup` index, and
// `groupRows [][]int` allocations of the general path — the main
// saving. Correctness hinges on the caller having proven contiguity
// via overFastPathApplicable before dispatching here.
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
