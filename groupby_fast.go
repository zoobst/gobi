package gobi

import (
	"math"
	"slices"
	"sort"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// numericView is a lightweight snapshot of a single-chunk numeric column,
// carrying the underlying slice + the array (for null checks). See
// series_ops.go for the equivalent single-column fast-path helpers.
type numericView struct {
	f64  []float64
	i64  []int64
	arr  arrow.Array
	kind uint8 // 0=empty, 1=f64, 2=i64
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
	// NUnique also fall through: First/Last need to preserve the source
	// column type (may be non-numeric), and NUnique's byte encoding
	// path is easier to keep in the generic path than to inline here.
	// Std / Var stay in the fast path — cheap Welford add.
	for _, a := range aggs {
		if a.Fn != nil {
			return nil, false, nil
		}
		switch a.Kind {
		case AggFirst, AggLast, AggNUnique:
			return nil, false, nil
		}
	}
	chunks := g.keys[0].col.Data().Chunks()
	if len(chunks) != 1 {
		return nil, false, nil
	}

	// Pre-extract numeric views for every aggregation column that isn't
	// count-star. Any column that doesn't have a fast view will trigger a
	// fallback to the general path.
	aggViews := make([]numericView, len(aggs))
	for i, a := range aggs {
		if a.Kind == AggCount && a.Column == "" {
			continue
		}
		colS, err := g.frame.Column(a.Column)
		if err != nil {
			return nil, false, err
		}
		v, ok := viewNumeric(colS)
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

// aggFastString handles the string-key fast path. This is the hot shape in
// the benchmark: 1M rows with 100 unique string keys.
func (g *GroupBy) aggFastString(keyArr *array.String, aggs []Aggregation, aggViews []numericView) (*Frame, bool, error) {
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

	aggBuilders, aggFields := makeAggBuilders(pool, aggs)
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
func (g *GroupBy) aggFastInt64(keyArr *array.Int64, aggs []Aggregation, aggViews []numericView) (*Frame, bool, error) {
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

	aggBuilders, aggFields := makeAggBuilders(pool, aggs)
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
func (g *GroupBy) aggFastFloat64(keyArr *array.Float64, aggs []Aggregation, aggViews []numericView) (*Frame, bool, error) {
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

	aggBuilders, aggFields := makeAggBuilders(pool, aggs)
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

// viewNumeric returns a numericView plus ok=true when s is a single-chunk
// numeric column (Float64 or Int64). Multi-chunk or non-numeric columns
// return ok=false and the caller should fall back to the general path.
func viewNumeric(s Series) (numericView, bool) {
	chunks := s.col.Data().Chunks()
	if len(chunks) != 1 {
		return numericView{}, false
	}
	switch a := chunks[0].(type) {
	case *array.Float64:
		return numericView{f64: a.Float64Values(), arr: a, kind: 1}, true
	case *array.Int64:
		return numericView{i64: a.Int64Values(), arr: a, kind: 2}, true
	}
	return numericView{}, false
}

// at returns (value, isValid) for row i.
func (v numericView) at(i int) (float64, bool) {
	if v.kind == 0 {
		return 0, false
	}
	if v.arr.IsNull(i) {
		return 0, false
	}
	if v.kind == 1 {
		return v.f64[i], true
	}
	return float64(v.i64[i]), true
}

// makeAggBuilders returns one output-array builder + field per aggregation.
// Count / NUnique produce Int64; every other Kind supported in this
// fast path produces Float64. First / Last / custom Fn kinds never
// reach here — the caller (aggFast) bails out for those.
func makeAggBuilders(pool memory.Allocator, aggs []Aggregation) ([]array.Builder, []arrow.Field) {
	bs := make([]array.Builder, len(aggs))
	fs := make([]arrow.Field, len(aggs))
	for i, a := range aggs {
		if a.Kind == AggCount || a.Kind == AggNUnique {
			bs[i] = array.NewInt64Builder(pool)
			fs[i] = arrow.Field{Name: aggName(a), Type: arrow.PrimitiveTypes.Int64, Nullable: false}
			continue
		}
		bs[i] = array.NewFloat64Builder(pool)
		fs[i] = arrow.Field{Name: aggName(a), Type: arrow.PrimitiveTypes.Float64, Nullable: true}
	}
	return bs, fs
}

// appendFastAgg computes one aggregation output over rows and appends the
// result to b. Uses the pre-extracted view v to avoid per-row chunk walks.
func appendFastAgg(b array.Builder, a Aggregation, v numericView, rows []int) {
	if a.Kind == AggCount {
		ib := b.(*array.Int64Builder)
		if a.Column == "" {
			ib.Append(int64(len(rows)))
			return
		}
		var n int64
		for _, row := range rows {
			if _, ok := v.at(row); ok {
				n++
			}
		}
		ib.Append(n)
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
		x, ok := v.at(row)
		if !ok {
			continue
		}
		if n == 0 {
			minV, maxV = x, x
		} else {
			if x < minV {
				minV = x
			}
			if x > maxV {
				maxV = x
			}
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
	for i, a := range aggArrs {
		c := arrow.NewChunked(a.DataType(), []arrow.Array{a})
		cols[i+1] = *arrow.NewColumn(fields[i+1], c)
	}
	f, err := NewFrame(schema, cols)
	if err != nil {
		return nil, false, err
	}
	return f, true, nil
}
