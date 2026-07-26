package gobi

import (
	"bytes"
	"fmt"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
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
	emit := func(start, end int) error {
		rowsBuf = rowsBuf[:0]
		for k := start; k < end; k++ {
			rowsBuf = append(rowsBuf, k)
		}
		if err := appendKeyRow(keyBuilders, g.keys, start); err != nil {
			return err
		}
		for i, a := range aggs {
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
	}
	return NewFrame(schema, cols)
}
