package gobi

import (
	"bytes"
	"fmt"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/zoobst/gobi/compute"
)

// groupByFastPathApplicable reports whether GroupBy.Agg can take
// the aligned + sorted linear-scan fast path instead of the general
// hash-map path. Mirrors overFastPathApplicable's rule set: same-key
// rows must be contiguous in the input, which follows from three
// preconditions on the Frame's PartitionMetadata:
//
//  1. Aligned(meta, keys) — meta claims partitioning on exactly the
//     GroupBy key columns (ordered).
//  2. meta.SortedBy starts with the GroupBy key columns. Prefix
//     match suffices: sorted on [K, ts] is fine for GroupBy(K).
//     Direction (ascending vs descending) doesn't matter — same-K
//     rows are neighbors either way.
//  3. meta.SortEnforced == true. A hint-only sort could silently
//     produce wrong group boundaries if the actual data isn't
//     ordered.
//
// Any failure falls through to the general hash-map path, which is
// correct for any input regardless of metadata claims.
func groupByFastPathApplicable(meta *PartitionMetadata, keys []string) bool {
	if meta == nil || !meta.SortEnforced {
		return false
	}
	if !Aligned(meta, keys) {
		return false
	}
	if len(meta.SortedBy) < len(keys) {
		return false
	}
	for i, k := range keys {
		if meta.SortedBy[i].Column != k {
			return false
		}
	}
	return true
}

// aggAligned is the linear-scan fast path for GroupBy.Agg. Runs when
// groupByFastPathApplicable holds on the input Frame's PartitionMetadata.
// Detects group boundaries via composite-key comparison between
// consecutive rows, emits each group in stream order (which is
// already the sort order — output rows come out sorted by group
// keys without needing an explicit sort at the end).
//
// Skipped vs. the general path:
//   - No `rowKeys []string` array (one alloc per row on the slow path).
//   - No `groups map[string][]int` hash map (map ops per row + rehashing).
//   - No `sort.Strings(order)` at the end (unique-groups sort).
//
// Retained: the per-group `rows []int` slice + `appendAgg` call
// pattern, which is the same as the slow path. That means the fast
// path composes with every existing Aggregation.Kind + custom Fn
// without duplicating aggregation code.
func (g *GroupBy) aggAligned(aggs []Aggregation) (*Frame, error) {
	frame := g.frame
	nRows := frame.NumRows()

	pool := memory.DefaultAllocator
	keyBuilders, err := makeKeyBuilders(pool, g.keys)
	if err != nil {
		return nil, err
	}
	defer releaseBuilders(keyBuilders)

	aggBuilders, aggFields, err := g.buildAggBuilders(aggs, pool)
	if err != nil {
		return nil, err
	}
	defer releaseBuilders(aggBuilders)

	if nRows == 0 {
		return g.assembleOutput(keyBuilders, aggBuilders, aggFields)
	}

	// Precompute per-agg filter masks (nil entry = no filter).
	filterMasks, err := precomputeFilterMasks(frame, aggs)
	if err != nil {
		return nil, err
	}

	// Group-boundary detection: composite-key comparison of
	// consecutive rows via two scratch buffers, swapped whenever we
	// cross a boundary.
	curKey := make([]byte, 0, 32)
	nextKey := make([]byte, 0, 32)
	curKey, err = g.rowKeyInto(curKey, 0)
	if err != nil {
		return nil, err
	}

	// Reusable per-group row-index buffer. Slice-view semantics —
	// each emit call takes rowsBuf[:0..end-start].
	rowsBuf := make([]int, 0, 64)
	// Reusable scratch for per-agg filtered subset.
	filteredBuf := make([]int, 0, 64)

	// emit reduces rows [start, end) as a single group and appends
	// its key row + aggregation values to the output builders.
	//
	// Two dispatch paths per aggregation:
	//   - Contiguous SIMD: when the agg kind is Sum / Min / Max /
	//     Mean on a null-free single-chunk Float64 or Int64 column
	//     AND no per-agg filter is active, hand the group's
	//     contiguous slice directly to gobi/compute's reduction
	//     kernels. Skips the rowsBuf construction + per-row indexed
	//     access; SIMD wins ~4× on Float64 Min/Max under
	//     GOEXPERIMENT=simd.
	//   - General: the existing rowsBuf + appendAgg path. Handles
	//     everything the SIMD path doesn't cover (custom aggs,
	//     nulls, non-numeric types, filtered aggs, First/Last, etc.).
	//
	// Whether the SIMD attempt actually succeeded is tracked in
	// handledBySIMD (per-emit-call) — the general-path second loop
	// must NOT skip based on kind eligibility, because kind checks
	// can't tell whether tryEmitContiguousSIMD accepted or refused
	// (e.g., Sum on a null-carrying column is kind-eligible but
	// shape-ineligible, and skipping it would silently drop the
	// agg and misalign the output columns).
	handledBySIMD := make([]bool, len(aggs))
	emit := func(start, end int) error {
		if err := appendKeyRow(keyBuilders, g.keys, start); err != nil {
			return err
		}
		for i := range handledBySIMD {
			handledBySIMD[i] = false
		}
		needRowsBuf := false
		for i, a := range aggs {
			if filterMasks[i] == nil {
				if ok, err := g.tryEmitContiguousSIMD(aggBuilders[i], a, start, end); err != nil {
					return err
				} else if ok {
					handledBySIMD[i] = true
					continue
				}
			}
			needRowsBuf = true
		}
		if !needRowsBuf {
			return nil
		}
		// Some agg wants the row-index path — build rowsBuf once.
		rowsBuf = rowsBuf[:0]
		for k := start; k < end; k++ {
			rowsBuf = append(rowsBuf, k)
		}
		for i, a := range aggs {
			if handledBySIMD[i] {
				continue
			}
			toAgg := rowsBuf
			if filterMasks[i] != nil {
				toAgg = applyFilterMask(rowsBuf, filterMasks[i], filteredBuf[:0])
				filteredBuf = toAgg
			}
			if err := g.appendAgg(aggBuilders[i], a, toAgg); err != nil {
				return err
			}
		}
		return nil
	}

	groupStart := 0
	for row := 1; row < nRows; row++ {
		nextKey, err = g.rowKeyInto(nextKey[:0], row)
		if err != nil {
			return nil, err
		}
		if bytes.Equal(curKey, nextKey) {
			continue
		}
		if err := emit(groupStart, row); err != nil {
			return nil, err
		}
		groupStart = row
		curKey, nextKey = nextKey, curKey
	}
	// Final group [groupStart, nRows).
	if err := emit(groupStart, nRows); err != nil {
		return nil, err
	}

	return g.assembleOutput(keyBuilders, aggBuilders, aggFields)
}

// rowKeyInto appends the composite key bytes for row into dst,
// returning the resulting slice. Alloc-free counterpart to
// GroupBy.rowKey (which allocates + returns a string). Reused
// across the fast path's group-boundary scan.
func (g *GroupBy) rowKeyInto(dst []byte, row int) ([]byte, error) {
	for i, s := range g.keys {
		if i > 0 {
			dst = append(dst, 0x1F)
		}
		var err error
		dst, err = keyOfAppend(dst, s, row)
		if err != nil {
			return nil, err
		}
	}
	return dst, nil
}

// buildAggBuilders is the shared aggregation-builder setup extracted
// from GroupBy.Agg's slow path. Sits here so the fast path doesn't
// duplicate the type-selection logic (Count/NUnique → Int64;
// First/Last preserve source type; custom Fn uses declared Type;
// default Float64).
func (g *GroupBy) buildAggBuilders(aggs []Aggregation, pool memory.Allocator) ([]array.Builder, []arrow.Field, error) {
	aggBuilders := make([]array.Builder, len(aggs))
	aggFields := make([]arrow.Field, len(aggs))
	for i, a := range aggs {
		if a.Fn != nil {
			if _, err := g.frame.Column(a.Column); err != nil {
				return nil, nil, err
			}
			b, err := builderForType(pool, a.Fn.Type())
			if err != nil {
				return nil, nil, fmt.Errorf("gobi: aggregation %d (%s): %w",
					i, aggName(a), err)
			}
			aggBuilders[i] = b
			aggFields[i] = arrow.Field{
				Name: aggName(a), Type: a.Fn.Type(), Nullable: true,
			}
			continue
		}
		if a.Kind == AggCount || a.Kind == AggNUnique {
			aggBuilders[i] = array.NewInt64Builder(pool)
			aggFields[i] = arrow.Field{
				Name: aggName(a), Type: arrow.PrimitiveTypes.Int64,
				Nullable: a.Kind != AggCount && a.Kind != AggNUnique,
			}
			continue
		}
		if a.Kind == AggFirst || a.Kind == AggLast || a.Kind == AggMode {
			src, err := g.frame.Column(a.Column)
			if err != nil {
				return nil, nil, err
			}
			srcType := src.DataType()
			b, err := builderForType(pool, srcType)
			if err != nil {
				return nil, nil, fmt.Errorf("gobi: aggregation %d (%s): %w",
					i, aggName(a), err)
			}
			aggBuilders[i] = b
			aggFields[i] = arrow.Field{Name: aggName(a), Type: srcType, Nullable: true}
			continue
		}
		if a.Kind == AggMin || a.Kind == AggMax {
			src, err := g.frame.Column(a.Column)
			if err != nil {
				return nil, nil, err
			}
			if _, isTS := src.DataType().(*arrow.TimestampType); isTS {
				srcType := src.DataType()
				b, err := builderForType(pool, srcType)
				if err != nil {
					return nil, nil, fmt.Errorf("gobi: aggregation %d (%s): %w",
						i, aggName(a), err)
				}
				aggBuilders[i] = b
				aggFields[i] = arrow.Field{Name: aggName(a), Type: srcType, Nullable: true}
				continue
			}
		}
		if _, err := g.frame.Column(a.Column); err != nil {
			return nil, nil, err
		}
		aggBuilders[i] = array.NewFloat64Builder(pool)
		aggFields[i] = arrow.Field{
			Name: aggName(a), Type: arrow.PrimitiveTypes.Float64, Nullable: true,
		}
	}
	return aggBuilders, aggFields, nil
}

// assembleOutput builds the final Frame from populated key + agg
// builders. Extracted from Agg's tail so both paths share the same
// schema + Frame construction. Reads-out arrays and constructs a
// Frame with (keys..., aggs...) column order.
func (g *GroupBy) assembleOutput(keyBuilders, aggBuilders []array.Builder, aggFields []arrow.Field) (*Frame, error) {
	keyFields := make([]arrow.Field, len(g.keys))
	for i, k := range g.keys {
		keyFields[i] = arrow.Field{Name: k.name, Type: k.DataType(), Nullable: false}
	}
	fields := append(append([]arrow.Field{}, keyFields...), aggFields...)
	schema := arrow.NewSchema(fields, nil)

	arrays := make([]arrow.Array, 0, len(fields))
	defer func() {
		for _, a := range arrays {
			a.Release()
		}
	}()
	for _, b := range keyBuilders {
		arrays = append(arrays, b.NewArray())
	}
	for _, b := range aggBuilders {
		arrays = append(arrays, b.NewArray())
	}

	cols := make([]arrow.Column, len(fields))
	for i, a := range arrays {
		chunked := arrow.NewChunked(a.DataType(), []arrow.Array{a})
		cols[i] = *arrow.NewColumn(fields[i], chunked)
		chunked.Release()
	}
	return NewFrame(schema, cols)
}

// tryEmitContiguousSIMD emits one aggregation over rows [start, end)
// via the gobi/compute reduction kernels. The rows must be a
// contiguous range in the source frame — the aligned GroupBy path
// guarantees this. Returns (true, nil) on success; (false, nil)
// when the shape doesn't match (falls through to the general
// per-row-index path). Returns an error only for real problems
// (missing column, unexpected builder type).
//
// Preconditions checked here:
//   - Aggregation kind is one of Sum / Min / Max / Mean (numeric
//     reductions). Others fall through.
//   - Source column resolves cleanly.
//   - Source column is single-chunk (RecordBatch columns always
//     are, but the eager path may see multi-chunk).
//   - Source column is null-free (any nulls fall through — the
//     general path handles null propagation).
//   - Source column is Float64 or Int64 (both have SIMD Sum;
//     Float64 additionally has SIMD Min/Max).
//   - Output builder is *array.Float64Builder (buildAggBuilders
//     picks Float64 for all four reduction kinds on numeric input).
func (g *GroupBy) tryEmitContiguousSIMD(b array.Builder, a Aggregation, start, end int) (bool, error) {
	// Kind gate: only reducible numeric aggs are candidates.
	// Custom Fn aggs, First/Last/Median/Mode/Count/NUnique all
	// fall through to the general path.
	if a.Fn != nil {
		return false, nil
	}
	switch a.Kind {
	case AggSum, AggMin, AggMax, AggMean:
	default:
		return false, nil
	}
	col, err := g.frame.Column(a.Column)
	if err != nil {
		return false, err
	}
	chunks := col.col.Data().Chunks()
	if len(chunks) != 1 {
		return false, nil
	}
	fb, ok := b.(*array.Float64Builder)
	if !ok {
		return false, nil
	}
	switch arr := chunks[0].(type) {
	case *array.Float64:
		if arr.NullN() != 0 {
			return false, nil
		}
		vals := arr.Float64Values()[start:end]
		return emitReducedFloat64(fb, a.Kind, vals), nil
	case *array.Int64:
		if arr.NullN() != 0 {
			return false, nil
		}
		return emitReducedInt64(fb, a.Kind, arr.Int64Values()[start:end]), nil
	}
	return false, nil
}

// emitReducedFloat64 appends the reduction of vals to b. Returns
// true unconditionally (the caller already committed to this
// path); the bool is for symmetry with tryEmitContiguousSIMD.
// Empty ranges emit null for min/max/mean (no values seen) and 0
// for sum (identity).
func emitReducedFloat64(b *array.Float64Builder, kind AggKind, vals []float64) bool {
	if len(vals) == 0 {
		switch kind {
		case AggSum:
			b.Append(0)
		default:
			b.AppendNull()
		}
		return true
	}
	switch kind {
	case AggSum:
		b.Append(compute.SumF64(vals))
	case AggMean:
		b.Append(compute.SumF64(vals) / float64(len(vals)))
	case AggMin:
		m, _ := compute.MinF64(vals)
		b.Append(m)
	case AggMax:
		m, _ := compute.MaxF64(vals)
		b.Append(m)
	}
	return true
}

// emitReducedInt64 is the Int64-input counterpart. Aligned
// GroupBy's output builder for numeric reductions is Float64
// (matches the general path), so integer results widen on emit —
// same convention as gobi's other integer aggregations.
func emitReducedInt64(b *array.Float64Builder, kind AggKind, vals []int64) bool {
	if len(vals) == 0 {
		switch kind {
		case AggSum:
			b.Append(0)
		default:
			b.AppendNull()
		}
		return true
	}
	switch kind {
	case AggSum:
		b.Append(float64(compute.SumI64(vals)))
	case AggMean:
		b.Append(float64(compute.SumI64(vals)) / float64(len(vals)))
	case AggMin:
		m, _ := compute.MinI64(vals)
		b.Append(float64(m))
	case AggMax:
		m, _ := compute.MaxI64(vals)
		b.Append(float64(m))
	}
	return true
}
