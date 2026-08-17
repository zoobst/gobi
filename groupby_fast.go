package gobi

import (
	"math"
	"slices"
	"sort"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// aggViewKind discriminates what shape of column view a fast-path
// aggregation needs.
type aggViewKind uint8

const (
	// aggViewCountStar is the count-star agg (no source column) — no view
	// is needed, per-row extraction is skipped, output is len(rows).
	aggViewCountStar aggViewKind = iota
	// aggViewNumeric backs the numeric arithmetic aggs (Sum, Mean, Min,
	// Max, Std, Var, and Count on a numeric column). f64 or i64 backing
	// slice populated based on source type.
	aggViewNumeric
	// aggViewTimestamp backs Min / Max on a Timestamp column. Backing
	// storage is int64 (arrow.Timestamp is int64), but the aggregation
	// emits back as Timestamp preserving the source's TimeUnit + TimeZone.
	aggViewTimestamp
	// aggViewHashable backs NUnique. String / Int64 / Float64 columns
	// each get a direct-hash path; other types fall back to the general
	// slow path via keyOfAppend.
	aggViewHashable
)

// aggView is a per-aggregation column snapshot used by the fast path.
// One aggView per aggregation in the call — each carries the specific
// column shape its aggregation needs (numeric arithmetic, timestamp
// compare, or hashable set-insert). Kind discriminates which fields
// are populated.
type aggView struct {
	kind aggViewKind

	// Numeric / Timestamp backing. numericKind tracks whether f64 or
	// i64 is populated:
	//   0 = neither (count-star, or hashable+string)
	//   1 = f64
	//   2 = i64
	numericKind uint8
	f64         []float64
	i64         []int64

	// Underlying arrow array — used for null lookup regardless of
	// backing slice choice. nil for count-star.
	arr arrow.Array

	// Set for hashable+string aggs. The String array is the direct
	// nunique source; the value at row i is used as a map[string] key.
	strs *array.String

	// Timestamp arrow type (unit + timezone). Populated when
	// kind == aggViewTimestamp — used by makeAggBuilders / builder
	// selection so the output column preserves the source type.
	tsType arrow.DataType
}

// aggFast is a specialization of Agg for the common shape: exactly one key
// column, single-chunk, of a directly-hashable primitive type. It avoids
// the per-row byte-slice concatenation and chunk-walk that the general
// path spends most of its time on.
//
// Returns (nil, false, nil) when the fast path doesn't apply — callers
// should fall back to the slow path in that case.
func (g *GroupBy) aggFast(aggs []Aggregation) (*Frame, bool, error) {
	if len(g.keys) != 1 {
		return nil, false, nil
	}
	// Custom aggregators are user-defined and produce arbitrary output
	// types — they can't share the numeric fast path. First / Last /
	// Median / Mode still fall through: First/Last need to preserve the
	// source column type (may be non-numeric), Median needs a sort
	// buffer, Mode needs the same keyOfAppend byte-encoding the slow
	// path uses. Count / Sum / Mean / Min / Max / Std / Var / NUnique
	// all live here.
	for _, a := range aggs {
		if a.Fn != nil {
			return nil, false, nil
		}
		if a.Filter.node != nil {
			// Filtered aggs need per-row mask lookup; the fast path
			// doesn't carry that plumbing. Fall back to the general
			// appendAgg path.
			return nil, false, nil
		}
		switch a.Kind {
		case AggFirst, AggLast, AggMedian, AggMode:
			return nil, false, nil
		}
	}
	chunks := g.keys[0].col.Data().Chunks()
	if len(chunks) != 1 {
		return nil, false, nil
	}

	// Pre-build one view per aggregation. Each view carries the specific
	// column snapshot its aggregation kind requires. If any agg's view
	// can't be built (multi-chunk source, or Timestamp on a non-Min/Max
	// agg, or NUnique on an unsupported type), the whole fast path bails
	// and the slow path takes over.
	aggViews := make([]aggView, len(aggs))
	for i, a := range aggs {
		v, ok, err := buildAggView(g.frame, a)
		if err != nil {
			return nil, false, err
		}
		if !ok {
			return nil, false, nil
		}
		aggViews[i] = v
	}

	switch keyArr := chunks[0].(type) {
	case *array.String:
		return g.aggFastString(keyArr, aggs, aggViews)
	case *array.Int64:
		return g.aggFastInt64(keyArr, aggs, aggViews)
	case *array.Float64:
		return g.aggFastFloat64(keyArr, aggs, aggViews)
	}
	return nil, false, nil
}

// buildAggView picks the right view shape for one aggregation. Returns
// (view, false, nil) when the fast path can't handle this agg's
// column shape (e.g. Timestamp source for Sum, or multi-chunk source,
// or NUnique on List<T>) — caller bails to the slow path.
func buildAggView(frame *Frame, a Aggregation) (aggView, bool, error) {
	if a.Kind == AggCount && a.Column == "" {
		return aggView{kind: aggViewCountStar}, true, nil
	}
	colS, err := frame.Column(a.Column)
	if err != nil {
		return aggView{}, false, err
	}
	chunks := colS.col.Data().Chunks()
	if len(chunks) != 1 {
		return aggView{}, false, nil
	}

	// NUnique: hashable view. String / Int64 / Float64 all supported via
	// per-cell direct-hash maps in appendFastNUnique.
	if a.Kind == AggNUnique {
		switch arr := chunks[0].(type) {
		case *array.String:
			return aggView{kind: aggViewHashable, arr: arr, strs: arr}, true, nil
		case *array.Int64:
			return aggView{
				kind: aggViewHashable, arr: arr,
				numericKind: 2, i64: arr.Int64Values(),
			}, true, nil
		case *array.Float64:
			return aggView{
				kind: aggViewHashable, arr: arr,
				numericKind: 1, f64: arr.Float64Values(),
			}, true, nil
		}
		// List, Struct, Binary, etc. → slow path handles them via
		// keyOfAppend.
		return aggView{}, false, nil
	}

	// Timestamp Min / Max: preserve source arrow.TimestampType through
	// the aggregation. int64 backing under the hood (arrow.Timestamp is
	// int64), same compare semantics as int64. Only Min / Max are
	// meaningful — Sum / Mean / Std / Var on Timestamp would need a
	// Duration return type, which pandas / polars disagree on; we
	// route those to the numeric branch which rejects Timestamp.
	if a.Kind == AggMin || a.Kind == AggMax {
		if tsArr, ok := chunks[0].(*array.Timestamp); ok {
			values := tsArr.TimestampValues()
			i64 := make([]int64, len(values))
			for i, v := range values {
				i64[i] = int64(v)
			}
			return aggView{
				kind: aggViewTimestamp, arr: tsArr,
				numericKind: 2, i64: i64,
				tsType: colS.DataType(),
			}, true, nil
		}
	}

	// Numeric arithmetic: Sum / Mean / Min / Max / Std / Var / Count(col).
	switch arr := chunks[0].(type) {
	case *array.Float64:
		return aggView{kind: aggViewNumeric, arr: arr, numericKind: 1, f64: arr.Float64Values()}, true, nil
	case *array.Int64:
		return aggView{kind: aggViewNumeric, arr: arr, numericKind: 2, i64: arr.Int64Values()}, true, nil
	}
	return aggView{}, false, nil
}

// aggFastString handles the string-key fast path. This is the hot shape in
// the benchmark: 1M rows with 100 unique string keys.
func (g *GroupBy) aggFastString(keyArr *array.String, aggs []Aggregation, aggViews []aggView) (*Frame, bool, error) {
	n := keyArr.Len()
	// Insertion-order tracked keys; sort just before emit.
	groups := make(map[string][]int, 64)
	var order []string
	// Nulls collapse into a sentinel key. Using "\x00" is safe because
	// Arrow strings can't contain a raw NUL — but even if they can, we
	// disambiguate below by seeing null=true separately if we cared. Here
	// we just treat null-keyed rows as their own group.
	const nullKey = "\x00__gobi_null__"
	for i := range n {
		var k string
		if keyArr.IsNull(i) {
			k = nullKey
		} else {
			k = keyArr.Value(i)
		}
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		groups[k] = append(groups[k], i)
	}
	sort.Strings(order)

	pool := memory.DefaultAllocator
	keyB := array.NewStringBuilder(pool)
	defer keyB.Release()

	aggBuilders, aggFields := makeAggBuilders(pool, aggs, aggViews)
	defer releaseBuilders(aggBuilders)

	for _, k := range order {
		if k == nullKey {
			keyB.AppendNull()
		} else {
			keyB.Append(k)
		}
		rows := groups[k]
		for i, a := range aggs {
			appendFastAgg(aggBuilders[i], a, aggViews[i], rows)
		}
	}

	return finishAggFrame(g.keys[0].field, keyB.NewArray(), aggFields, aggBuilders)
}

// aggFastInt64 handles the int64-key fast path.
func (g *GroupBy) aggFastInt64(keyArr *array.Int64, aggs []Aggregation, aggViews []aggView) (*Frame, bool, error) {
	n := keyArr.Len()
	vals := keyArr.Int64Values()
	groups := make(map[int64][]int, 64)
	nullRows := []int(nil)
	hasNull := false
	for i := range n {
		if keyArr.IsNull(i) {
			hasNull = true
			nullRows = append(nullRows, i)
			continue
		}
		groups[vals[i]] = append(groups[vals[i]], i)
	}
	keysSorted := make([]int64, 0, len(groups))
	for k := range groups {
		keysSorted = append(keysSorted, k)
	}
	slices.Sort(keysSorted)

	pool := memory.DefaultAllocator
	keyB := array.NewInt64Builder(pool)
	defer keyB.Release()

	aggBuilders, aggFields := makeAggBuilders(pool, aggs, aggViews)
	defer releaseBuilders(aggBuilders)

	for _, k := range keysSorted {
		keyB.Append(k)
		rows := groups[k]
		for i, a := range aggs {
			appendFastAgg(aggBuilders[i], a, aggViews[i], rows)
		}
	}
	if hasNull {
		keyB.AppendNull()
		for i, a := range aggs {
			appendFastAgg(aggBuilders[i], a, aggViews[i], nullRows)
		}
	}

	return finishAggFrame(g.keys[0].field, keyB.NewArray(), aggFields, aggBuilders)
}

// aggFastFloat64 handles the float64-key fast path. Float64 keys with NaNs
// or floating-point noise are inherently a footgun; we group by exact bit
// equality (which matches map[float64] semantics).
func (g *GroupBy) aggFastFloat64(keyArr *array.Float64, aggs []Aggregation, aggViews []aggView) (*Frame, bool, error) {
	n := keyArr.Len()
	vals := keyArr.Float64Values()
	groups := make(map[float64][]int, 64)
	nullRows := []int(nil)
	hasNull := false
	for i := range n {
		if keyArr.IsNull(i) {
			hasNull = true
			nullRows = append(nullRows, i)
			continue
		}
		groups[vals[i]] = append(groups[vals[i]], i)
	}
	keysSorted := make([]float64, 0, len(groups))
	for k := range groups {
		keysSorted = append(keysSorted, k)
	}
	sort.Float64s(keysSorted)

	pool := memory.DefaultAllocator
	keyB := array.NewFloat64Builder(pool)
	defer keyB.Release()

	aggBuilders, aggFields := makeAggBuilders(pool, aggs, aggViews)
	defer releaseBuilders(aggBuilders)

	for _, k := range keysSorted {
		keyB.Append(k)
		rows := groups[k]
		for i, a := range aggs {
			appendFastAgg(aggBuilders[i], a, aggViews[i], rows)
		}
	}
	if hasNull {
		keyB.AppendNull()
		for i, a := range aggs {
			appendFastAgg(aggBuilders[i], a, aggViews[i], nullRows)
		}
	}

	return finishAggFrame(g.keys[0].field, keyB.NewArray(), aggFields, aggBuilders)
}

// numAt returns (value, isValid) for row i, working across the numeric
// backing slices. Callers that already know the numericKind should
// inline the slice access; this is the general-case shim.
func (v aggView) numAt(i int) (float64, bool) {
	if v.arr == nil || v.arr.IsNull(i) {
		return 0, false
	}
	switch v.numericKind {
	case 1:
		return v.f64[i], true
	case 2:
		return float64(v.i64[i]), true
	}
	return 0, false
}

// makeAggBuilders returns one output-array builder + field per aggregation.
// Count / NUnique → Int64 non-null. Min / Max on Timestamp preserve the
// source's TimestampType via tsType captured in the aggView. Everything
// else → Float64 nullable.
func makeAggBuilders(pool memory.Allocator, aggs []Aggregation, views []aggView) ([]array.Builder, []arrow.Field) {
	bs := make([]array.Builder, len(aggs))
	fs := make([]arrow.Field, len(aggs))
	for i, a := range aggs {
		if a.Kind == AggCount || a.Kind == AggNUnique {
			bs[i] = array.NewInt64Builder(pool)
			fs[i] = arrow.Field{Name: aggName(a), Type: arrow.PrimitiveTypes.Int64, Nullable: false}
			continue
		}
		if views[i].kind == aggViewTimestamp {
			tsType := views[i].tsType.(*arrow.TimestampType)
			bs[i] = array.NewTimestampBuilder(pool, tsType)
			fs[i] = arrow.Field{Name: aggName(a), Type: views[i].tsType, Nullable: true}
			continue
		}
		bs[i] = array.NewFloat64Builder(pool)
		fs[i] = arrow.Field{Name: aggName(a), Type: arrow.PrimitiveTypes.Float64, Nullable: true}
	}
	return bs, fs
}

// appendFastAgg computes one aggregation output over rows and appends the
// result to b. Uses the pre-extracted view v to avoid per-row chunk walks.
func appendFastAgg(b array.Builder, a Aggregation, v aggView, rows []int) {
	if a.Kind == AggCount {
		ib := b.(*array.Int64Builder)
		if a.Column == "" {
			ib.Append(int64(len(rows)))
			return
		}
		var n int64
		for _, row := range rows {
			if _, ok := v.numAt(row); ok {
				n++
			}
		}
		ib.Append(n)
		return
	}
	if a.Kind == AggNUnique {
		appendFastNUnique(b.(*array.Int64Builder), v, rows)
		return
	}
	if v.kind == aggViewTimestamp {
		appendFastTimestampMinMax(b.(*array.TimestampBuilder), a.Kind, v, rows)
		return
	}
	fb := b.(*array.Float64Builder)
	// Welford's running mean + M2 alongside the existing sum/min/max
	// tracking. Cheap enough to do unconditionally — the Std/Var
	// branches read m2/mean, others ignore.
	var (
		sum, minV, maxV float64
		mean, m2        float64
		n               int
	)
	for _, row := range rows {
		x, ok := v.numAt(row)
		if !ok {
			continue
		}
		if n == 0 {
			minV, maxV = x, x
		} else {
			minV = min(minV, x)
			maxV = max(maxV, x)
		}
		sum += x
		n++
		delta := x - mean
		mean += delta / float64(n)
		m2 += delta * (x - mean)
	}
	if n == 0 {
		fb.AppendNull()
		return
	}
	switch a.Kind {
	case AggSum:
		fb.Append(sum)
	case AggMean:
		fb.Append(sum / float64(n))
	case AggMin:
		fb.Append(minV)
	case AggMax:
		fb.Append(maxV)
	case AggVar:
		if n < 2 {
			fb.AppendNull()
			return
		}
		fb.Append(m2 / float64(n-1))
	case AggStd:
		if n < 2 {
			fb.AppendNull()
			return
		}
		fb.Append(math.Sqrt(m2 / float64(n-1)))
	default:
		fb.AppendNull()
	}
}

// appendFastNUnique writes the count of distinct non-null values in rows
// to b. Type-specialized on the source column: string → map[string],
// int64 → map[int64], float64 → map[float64]. Each dispatch avoids the
// keyOfAppend byte-encoding the slow path uses.
func appendFastNUnique(b *array.Int64Builder, v aggView, rows []int) {
	switch {
	case v.strs != nil:
		// Small starting capacity — most groups have a modest number of
		// distinct values, and Go maps grow well enough.
		seen := make(map[string]struct{}, 8)
		for _, row := range rows {
			if v.strs.IsNull(row) {
				continue
			}
			seen[v.strs.Value(row)] = struct{}{}
		}
		b.Append(int64(len(seen)))
	case v.numericKind == 2:
		seen := make(map[int64]struct{}, 8)
		for _, row := range rows {
			if v.arr.IsNull(row) {
				continue
			}
			seen[v.i64[row]] = struct{}{}
		}
		b.Append(int64(len(seen)))
	case v.numericKind == 1:
		seen := make(map[float64]struct{}, 8)
		for _, row := range rows {
			if v.arr.IsNull(row) {
				continue
			}
			seen[v.f64[row]] = struct{}{}
		}
		b.Append(int64(len(seen)))
	default:
		// buildAggView guarantees one of the above shapes; belt-and-
		// suspenders — emit 0 to match the "all rows null / no rows"
		// contract.
		b.Append(0)
	}
}

// appendFastTimestampMinMax reduces a Timestamp column's rows to a single
// min or max value and appends it as arrow.Timestamp. Comparisons run on
// the raw int64 backing — no float64 conversion, so nanosecond precision
// is preserved throughout the reduction.
func appendFastTimestampMinMax(b *array.TimestampBuilder, kind AggKind, v aggView, rows []int) {
	isMin := kind == AggMin
	var extreme int64
	seen := false
	for _, row := range rows {
		if v.arr.IsNull(row) {
			continue
		}
		x := v.i64[row]
		if !seen {
			extreme = x
			seen = true
			continue
		}
		if isMin && x < extreme {
			extreme = x
		} else if !isMin && x > extreme {
			extreme = x
		}
	}
	if !seen {
		b.AppendNull()
		return
	}
	b.Append(arrow.Timestamp(extreme))
}

// finishAggFrame stitches the key column + agg columns into a Frame with
// the requested schema. keyField provides the name/type for the key column.
func finishAggFrame(keyField arrow.Field, keyArr arrow.Array, aggFields []arrow.Field, aggBs []array.Builder) (*Frame, bool, error) {
	fields := make([]arrow.Field, 0, 1+len(aggFields))
	fields = append(fields, arrow.Field{Name: keyField.Name, Type: keyField.Type, Nullable: false})
	fields = append(fields, aggFields...)

	aggArrs := make([]arrow.Array, len(aggBs))
	for i, b := range aggBs {
		aggArrs[i] = b.NewArray()
	}
	defer func() {
		keyArr.Release()
		for _, a := range aggArrs {
			a.Release()
		}
	}()

	schema := arrow.NewSchema(fields, nil)
	cols := make([]arrow.Column, len(fields))
	chunked := arrow.NewChunked(keyArr.DataType(), []arrow.Array{keyArr})
	cols[0] = *arrow.NewColumn(fields[0], chunked)
	chunked.Release()
	for i, a := range aggArrs {
		c := arrow.NewChunked(a.DataType(), []arrow.Array{a})
		cols[i+1] = *arrow.NewColumn(fields[i+1], c)
		c.Release()
	}
	f, err := NewFrame(schema, cols)
	if err != nil {
		return nil, false, err
	}
	return f, true, nil
}
