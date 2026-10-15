package gobi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/decimal128"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// TestPickKeyMode_Int641Detection asserts that pickKeyMode returns
// keyModeInt641 for every arrow int-ish type that fits losslessly in
// int64. If a new type is added to the fast path, extend this table.
func TestPickKeyMode_Int641Detection(t *testing.T) {
	cases := []struct {
		name string
		dt   arrow.DataType
		want keyMode
	}{
		{"int64", arrow.PrimitiveTypes.Int64, keyModeInt641},
		{"int32", arrow.PrimitiveTypes.Int32, keyModeInt641},
		{"uint64", arrow.PrimitiveTypes.Uint64, keyModeInt641},
		{"uint32", arrow.PrimitiveTypes.Uint32, keyModeInt641},
		{"timestamp_ns", arrow.FixedWidthTypes.Timestamp_ns, keyModeInt641},
		{"bool", arrow.FixedWidthTypes.Boolean, keyModeInt641},
		{"float64", arrow.PrimitiveTypes.Float64, keyModeComposite},
		{"string", arrow.BinaryTypes.String, keyModeString1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			schema := arrow.NewSchema([]arrow.Field{
				{Name: "k", Type: tc.dt},
				{Name: "v", Type: arrow.PrimitiveTypes.Float64},
			}, nil)
			frame := emptyFrameFromSchema(t, schema)
			node := newAggregateNode(&scanFrameNode{frame: frame},
				[]string{"k"},
				[]Aggregation{{Column: "v", Kind: AggSum}})
			got := pickKeyMode(node)
			if got != tc.want {
				t.Fatalf("pickKeyMode(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// TestAggInt641_ParityAcrossWorkers verifies the int64 fast path
// produces identical results across worker counts and against the
// eager (CollectRaw) baseline. Uses the shared lazyFrame — group
// key is `id` (Int64), so this test alone exercises the new path.
func TestAggInt641_ParityAcrossWorkers(t *testing.T) {
	df := lazyFrame(t)
	build := func() *aggregateNode {
		lf := df.Lazy().GroupBy("id").Agg(
			Aggregation{Column: "price", Kind: AggSum, Alias: "sum"},
			Aggregation{Column: "price", Kind: AggMean, Alias: "mean"},
			Aggregation{Column: "price", Kind: AggCount, Alias: "n"},
		)
		return lf.Plan().(*aggregateNode)
	}
	baseline := runAggWithWorkers(t, build(), 1)

	for _, w := range []int{2, 4, 8} {
		w := w
		t.Run("workers="+itoa(w), func(t *testing.T) {
			got := runAggWithWorkers(t, build(), w)
			if got.NumRows() != baseline.NumRows() {
				t.Fatalf("workers=%d rows=%d, baseline=%d",
					w, got.NumRows(), baseline.NumRows())
			}
			for _, col := range []string{"id", "sum", "mean", "n"} {
				a, err := got.Column(col)
				if err != nil {
					t.Fatalf("workers=%d missing %q", w, col)
				}
				b, err := baseline.Column(col)
				if err != nil {
					t.Fatalf("baseline missing %q", col)
				}
				compareSeriesValues(t, col, a, b)
			}
		})
	}
}

// TestAggInt641_EachIntType — build a small frame per int-type and
// confirm the aggregate result comes back correctly typed. Catches
// regressions where the output type doesn't follow the input.
func TestAggInt641_EachIntType(t *testing.T) {
	// Local helper: two-column frame with a typed key + a Float64
	// value column, five rows, two distinct keys.
	buildTyped := func(t *testing.T, keyType arrow.DataType, keyBuilder func() arrow.Array) *Frame {
		pool := memory.DefaultAllocator
		vb := array.NewFloat64Builder(pool)
		defer vb.Release()
		vb.AppendValues([]float64{1, 2, 3, 4, 5}, nil)
		schema := arrow.NewSchema([]arrow.Field{
			{Name: "k", Type: keyType},
			{Name: "v", Type: arrow.PrimitiveTypes.Float64},
		}, nil)
		keyArr := keyBuilder()
		defer keyArr.Release()
		valArr := vb.NewArray()
		defer valArr.Release()
		cols := []arrow.Column{
			*arrow.NewColumn(schema.Field(0), arrow.NewChunked(keyType, []arrow.Array{keyArr})),
			*arrow.NewColumn(schema.Field(1), arrow.NewChunked(arrow.PrimitiveTypes.Float64, []arrow.Array{valArr})),
		}
		f, err := NewFrame(schema, cols)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}

	cases := []struct {
		name    string
		keyType arrow.DataType
		build   func() arrow.Array
	}{
		{"int64", arrow.PrimitiveTypes.Int64, func() arrow.Array {
			b := array.NewInt64Builder(memory.DefaultAllocator)
			defer b.Release()
			b.AppendValues([]int64{10, 20, 10, 20, 10}, nil)
			return b.NewArray()
		}},
		{"int32", arrow.PrimitiveTypes.Int32, func() arrow.Array {
			b := array.NewInt32Builder(memory.DefaultAllocator)
			defer b.Release()
			b.AppendValues([]int32{10, 20, 10, 20, 10}, nil)
			return b.NewArray()
		}},
		{"uint64", arrow.PrimitiveTypes.Uint64, func() arrow.Array {
			b := array.NewUint64Builder(memory.DefaultAllocator)
			defer b.Release()
			b.AppendValues([]uint64{10, 20, 10, 20, 10}, nil)
			return b.NewArray()
		}},
		{"bool", arrow.FixedWidthTypes.Boolean, func() arrow.Array {
			b := array.NewBooleanBuilder(memory.DefaultAllocator)
			defer b.Release()
			b.AppendValues([]bool{true, false, true, false, true}, nil)
			return b.NewArray()
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			df := buildTyped(t, tc.keyType, tc.build)
			out, err := df.Lazy().
				GroupBy("k").
				Agg(Aggregation{Column: "v", Kind: AggSum, Alias: "sum"}).
				Collect()
			if err != nil {
				t.Fatal(err)
			}
			if out.NumRows() != 2 {
				t.Fatalf("rows = %d, want 2", out.NumRows())
			}
			// Output key column arrow type must match the input.
			kCol, _ := out.Column("k")
			if kCol.DataType().ID() != tc.keyType.ID() {
				t.Fatalf("output key type = %s, want %s",
					kCol.DataType(), tc.keyType)
			}
		})
	}
}

// TestAggInt641_NullKeyRowsAreSkipped documents that null-keyed
// rows drop out of the int64 fast path. If we later add null-group
// support to the fast path, this test flips to check the null bucket
// is present.
func TestAggInt641_NullKeyRowsAreSkipped(t *testing.T) {
	pool := memory.DefaultAllocator
	kb := array.NewInt64Builder(pool)
	defer kb.Release()
	kb.AppendValues([]int64{1, 2, 3, 0}, []bool{true, true, true, false}) // last row null
	vb := array.NewFloat64Builder(pool)
	defer vb.Release()
	vb.AppendValues([]float64{10, 20, 30, 40}, nil)

	schema := arrow.NewSchema([]arrow.Field{
		{Name: "k", Type: arrow.PrimitiveTypes.Int64, Nullable: true},
		{Name: "v", Type: arrow.PrimitiveTypes.Float64, Nullable: false},
	}, nil)
	kArr, vArr := kb.NewArray(), vb.NewArray()
	defer kArr.Release()
	defer vArr.Release()
	df, err := NewFrame(schema, []arrow.Column{
		*arrow.NewColumn(schema.Field(0), arrow.NewChunked(kArr.DataType(), []arrow.Array{kArr})),
		*arrow.NewColumn(schema.Field(1), arrow.NewChunked(vArr.DataType(), []arrow.Array{vArr})),
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := df.Lazy().
		GroupBy("k").
		Agg(Aggregation{Column: "v", Kind: AggSum, Alias: "s"}).
		Collect()
	if err != nil {
		t.Fatal(err)
	}
	// Nulls skipped → 3 groups for keys {1,2,3}. The null-keyed row
	// (v=40) is dropped entirely.
	if out.NumRows() != 3 {
		t.Fatalf("rows = %d, want 3 (null-keyed row must be skipped)", out.NumRows())
	}
}

// emptyFrameFromSchema builds a zero-row Frame matching schema —
// used by pickKeyMode tests that only need a Schema, not data.
func emptyFrameFromSchema(t *testing.T, schema *arrow.Schema) *Frame {
	t.Helper()
	pool := memory.DefaultAllocator
	cols := make([]arrow.Column, schema.NumFields())
	for i, f := range schema.Fields() {
		b, err := builderForType(pool, f.Type)
		if err != nil {
			t.Fatal(err)
		}
		arr := b.NewArray()
		defer arr.Release()
		b.Release()
		cols[i] = *arrow.NewColumn(f, arrow.NewChunked(arr.DataType(), []arrow.Array{arr}))
	}
	f, err := NewFrame(schema, cols)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// TestAggKinds_ParityEagerVsStreaming verifies the new Aggregation
// Kinds (First / Last / Std / Var / NUnique) return identical
// results whether executed via the eager `GroupBy.Agg` path
// (`CollectRaw`) or the streaming aggregate (`Collect`). Uses the
// shared lazyFrame() fixture — 5 rows, 2 regions.
func TestAggKinds_ParityEagerVsStreaming(t *testing.T) {
	df := lazyFrame(t)

	aggs := []Aggregation{
		{Column: "id", Kind: AggFirst, Alias: "id_first"},
		{Column: "id", Kind: AggLast, Alias: "id_last"},
		{Column: "price", Kind: AggStd, Alias: "price_std"},
		{Column: "price", Kind: AggVar, Alias: "price_var"},
		{Column: "region", Kind: AggNUnique, Alias: "region_nunique"},
	}

	lf := df.Lazy().GroupBy("region").Agg(aggs...)
	streamed, err := lf.Collect()
	if err != nil {
		t.Fatalf("streaming Collect: %v", err)
	}
	eager, err := lf.CollectRaw()
	if err != nil {
		t.Fatalf("eager CollectRaw: %v", err)
	}
	if streamed.NumRows() != eager.NumRows() {
		t.Fatalf("row count: streaming=%d eager=%d",
			streamed.NumRows(), eager.NumRows())
	}
	for _, col := range []string{"region", "id_first", "id_last", "price_std", "price_var", "region_nunique"} {
		s1, err := streamed.Column(col)
		if err != nil {
			t.Fatalf("streamed missing %q", col)
		}
		s2, err := eager.Column(col)
		if err != nil {
			t.Fatalf("eager missing %q", col)
		}
		compareSeriesValues(t, col, s1, s2)
	}
}

// TestAggKinds_FirstLastValues locks in exact expected values so a
// refactor of the row-order logic won't silently swap first/last.
// EU rows are ids [2, 4]; US rows are ids [1, 3, 5]. Streaming +
// eager paths must both agree.
func TestAggKinds_FirstLastValues(t *testing.T) {
	df := lazyFrame(t)
	lf := df.Lazy().GroupBy("region").Agg(
		Aggregation{Column: "id", Kind: AggFirst, Alias: "first"},
		Aggregation{Column: "id", Kind: AggLast, Alias: "last"},
	)
	out, err := lf.Collect()
	if err != nil {
		t.Fatal(err)
	}
	// Streaming aggregate emits groups in sorted key order — EU, US.
	regionCol, _ := out.Column("region")
	firstCol, _ := out.Column("first")
	lastCol, _ := out.Column("last")
	regions := regionCol.Column().Data().Chunks()[0].(*array.String)
	firsts := firstCol.Column().Data().Chunks()[0].(*array.Int64)
	lasts := lastCol.Column().Data().Chunks()[0].(*array.Int64)

	type want struct {
		region      string
		first, last int64
	}
	wanted := []want{{"EU", 2, 4}, {"US", 1, 5}}
	for i, w := range wanted {
		if regions.Value(i) != w.region {
			t.Fatalf("row %d region = %q, want %q", i, regions.Value(i), w.region)
		}
		if firsts.Value(i) != w.first || lasts.Value(i) != w.last {
			t.Errorf("row %d [%s]: first=%d last=%d, want first=%d last=%d",
				i, w.region, firsts.Value(i), lasts.Value(i), w.first, w.last)
		}
	}
}

// TestAggKinds_StdVarValues checks Welford's produces the correct
// sample std/var (Bessel-corrected, n-1 denominator). EU prices
// [20, 40] → variance = 200, std = ~14.142. US prices [10, 30, 50]
// → variance = 400, std = 20.
func TestAggKinds_StdVarValues(t *testing.T) {
	df := lazyFrame(t)
	lf := df.Lazy().GroupBy("region").Agg(
		Aggregation{Column: "price", Kind: AggStd, Alias: "std"},
		Aggregation{Column: "price", Kind: AggVar, Alias: "var"},
	)
	out, err := lf.Collect()
	if err != nil {
		t.Fatal(err)
	}
	stdCol, _ := out.Column("std")
	varCol, _ := out.Column("var")
	stds := stdCol.Column().Data().Chunks()[0].(*array.Float64)
	vars := varCol.Column().Data().Chunks()[0].(*array.Float64)

	// Order: EU first (n=2 → std=√200), US (n=3 → std=20).
	wantStd := []float64{math.Sqrt(200), 20}
	wantVar := []float64{200, 400}
	for i := 0; i < 2; i++ {
		if math.Abs(stds.Value(i)-wantStd[i]) > 1e-9 {
			t.Errorf("row %d std=%v, want %v", i, stds.Value(i), wantStd[i])
		}
		if math.Abs(vars.Value(i)-wantVar[i]) > 1e-9 {
			t.Errorf("row %d var=%v, want %v", i, vars.Value(i), wantVar[i])
		}
	}
}

// TestAggKinds_NUniqueDistinctValues confirms the distinct-value
// counter collapses bit-equal values and skips nulls. EU rows
// have distinct id set {2, 4} → 2; US has {1, 3, 5} → 3.
func TestAggKinds_NUniqueDistinctValues(t *testing.T) {
	df := lazyFrame(t)
	lf := df.Lazy().GroupBy("region").Agg(
		Aggregation{Column: "id", Kind: AggNUnique, Alias: "n"},
	)
	out, err := lf.Collect()
	if err != nil {
		t.Fatal(err)
	}
	nCol, _ := out.Column("n")
	ns := nCol.Column().Data().Chunks()[0].(*array.Int64)
	// Sorted region order: EU (2 distinct), US (3 distinct).
	if ns.Value(0) != 2 || ns.Value(1) != 3 {
		t.Fatalf("n = [%d, %d], want [2, 3]", ns.Value(0), ns.Value(1))
	}
}

// TestAggKinds_StdVarSingleRowGroupsEmitNull mirrors polars / pandas
// semantics: sample std/var over a single row is undefined; the
// aggregate must emit null rather than 0 or NaN.
func TestAggKinds_StdVarSingleRowGroupsEmitNull(t *testing.T) {
	// GroupBy id — every group is size 1. std/var must all be null.
	df := lazyFrame(t)
	lf := df.Lazy().GroupBy("id").Agg(
		Aggregation{Column: "price", Kind: AggStd, Alias: "std"},
		Aggregation{Column: "price", Kind: AggVar, Alias: "var"},
	)
	out, err := lf.Collect()
	if err != nil {
		t.Fatal(err)
	}
	stdCol, _ := out.Column("std")
	varCol, _ := out.Column("var")
	stdArr := stdCol.Column().Data().Chunks()[0].(*array.Float64)
	varArr := varCol.Column().Data().Chunks()[0].(*array.Float64)
	for i := 0; i < int(out.NumRows()); i++ {
		if !stdArr.IsNull(i) {
			t.Errorf("row %d: std should be null (single-row group), got %v", i, stdArr.Value(i))
		}
		if !varArr.IsNull(i) {
			t.Errorf("row %d: var should be null (single-row group), got %v", i, varArr.Value(i))
		}
	}
}

// chunkedFrame builds a Frame whose columns are split into chunks at
// the given row boundaries (every column the same), so a lazy scan
// emits one batch per chunk. build appends rows [lo, hi) of column c.
func chunkedFrame(t *testing.T, fields []arrow.Field, nRows, chunkRows int, build func(c int, b array.Builder, lo, hi int)) *Frame {
	t.Helper()
	pool := memory.DefaultAllocator
	cols := make([]arrow.Column, len(fields))
	for c, f := range fields {
		var chunks []arrow.Array
		for lo := 0; lo < nRows; lo += chunkRows {
			b := array.NewBuilder(pool, f.Type)
			build(c, b, lo, min(lo+chunkRows, nRows))
			chunks = append(chunks, b.NewArray())
			b.Release()
		}
		ch := arrow.NewChunked(f.Type, chunks)
		cols[c] = *arrow.NewColumn(f, ch)
		ch.Release()
		for _, a := range chunks {
			a.Release()
		}
	}
	df, err := NewFrame(arrow.NewSchema(fields, nil), cols)
	if err != nil {
		t.Fatal(err)
	}
	return df
}

// parityFrame: 3,000 rows in 7 chunks — string key ks (37 values), int
// key ki, composite part kc (Int32 with nulls) — and value columns of
// several types, each with nulls.
func parityFrame(t *testing.T) *Frame {
	tsType := &arrow.TimestampType{Unit: arrow.Microsecond, TimeZone: "UTC"}
	fields := []arrow.Field{
		{Name: "ks", Type: arrow.BinaryTypes.String},
		{Name: "ki", Type: arrow.PrimitiveTypes.Int64},
		{Name: "kc", Type: arrow.PrimitiveTypes.Int32, Nullable: true},
		{Name: "f", Type: arrow.PrimitiveTypes.Float64, Nullable: true},
		{Name: "i", Type: arrow.PrimitiveTypes.Int64, Nullable: true},
		{Name: "u", Type: arrow.PrimitiveTypes.Uint32, Nullable: true},
		{Name: "i8", Type: arrow.PrimitiveTypes.Int8, Nullable: true},
		{Name: "b", Type: arrow.FixedWidthTypes.Boolean, Nullable: true},
		{Name: "s", Type: arrow.BinaryTypes.String, Nullable: true},
		{Name: "ts", Type: tsType, Nullable: true},
	}
	return chunkedFrame(t, fields, 3000, 431, func(c int, b array.Builder, lo, hi int) {
		for r := lo; r < hi; r++ {
			null := (r*7+c)%11 == 0
			switch c {
			case 0:
				b.(*array.StringBuilder).Append(fmt.Sprintf("k%02d", r*13%37))
			case 1:
				b.(*array.Int64Builder).Append(int64(r*13%37) - 18)
			case 2:
				if r%9 == 0 {
					b.AppendNull()
				} else {
					b.(*array.Int32Builder).Append(int32(r % 3))
				}
			case 3:
				if null {
					b.AppendNull()
				} else {
					b.(*array.Float64Builder).Append(float64(r%101) / 7)
				}
			case 4:
				if null {
					b.AppendNull()
				} else {
					b.(*array.Int64Builder).Append(int64(r%997) - 300)
				}
			case 5:
				if null {
					b.AppendNull()
				} else {
					b.(*array.Uint32Builder).Append(uint32(r % 4001))
				}
			case 6:
				if null {
					b.AppendNull()
				} else {
					b.(*array.Int8Builder).Append(int8(r%251 - 125))
				}
			case 7:
				if null {
					b.AppendNull()
				} else {
					b.(*array.BooleanBuilder).Append(r%3 == 0)
				}
			case 8:
				if null {
					b.AppendNull()
				} else {
					b.(*array.StringBuilder).Append(fmt.Sprintf("v%d", r))
				}
			case 9:
				if null {
					b.AppendNull()
				} else {
					b.(*array.TimestampBuilder).Append(arrow.Timestamp(int64(r) * 1_000_003))
				}
			}
		}
	})
}

var parityAggs = []Aggregation{
	{Kind: AggCount, Alias: "n"},
	{Column: "f", Kind: AggCount, Alias: "f_n"},
	{Column: "f", Kind: AggSum, Alias: "f_sum"},
	{Column: "i", Kind: AggSum, Alias: "i_sum"},
	{Column: "u", Kind: AggSum, Alias: "u_sum"},
	{Column: "i8", Kind: AggSum, Alias: "i8_sum"},
	{Column: "i", Kind: AggMean, Alias: "i_mean"},
	{Column: "f", Kind: AggMin, Alias: "f_min"},
	{Column: "i", Kind: AggMax, Alias: "i_max"},
	{Column: "ts", Kind: AggMin, Alias: "ts_min"},
	{Column: "ts", Kind: AggMax, Alias: "ts_max"},
	{Column: "s", Kind: AggFirst, Alias: "s_first"},
	{Column: "s", Kind: AggLast, Alias: "s_last"},
	{Column: "i", Kind: AggFirst, Alias: "i_first"},
	{Column: "f", Kind: AggLast, Alias: "f_last"},
	{Column: "b", Kind: AggFirst, Alias: "b_first"},
	{Column: "ts", Kind: AggLast, Alias: "ts_last"},
	{Column: "s", Kind: AggNUnique, Alias: "s_nunique"},
	{Column: "f", Kind: AggStd, Alias: "f_std"},
	{Column: "f", Kind: AggMedian, Alias: "f_median"},
	{Column: "i", Kind: AggMode, Alias: "i_mode"},
	{Column: "i", Kind: AggBitOr, Alias: "i_bitor"},
}

// assertSameFrame compares two Frames column by column: names, types,
// nulls and values (floats to 1e-9, NaN equal to NaN).
func assertSameFrame(t *testing.T, label string, got, want *Frame) {
	t.Helper()
	if !got.Schema().Equal(want.Schema()) {
		t.Fatalf("%s: schema\n got: %s\nwant: %s", label, got.Schema(), want.Schema())
	}
	if got.NumRows() != want.NumRows() {
		t.Fatalf("%s: %d rows, want %d", label, got.NumRows(), want.NumRows())
	}
	g, err := got.CompactChunks()
	if err != nil {
		t.Fatal(err)
	}
	defer g.Release()
	w, err := want.CompactChunks()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Release()
	for i := range g.NumCols() {
		a := g.series[i].col.Data().Chunks()[0]
		b := w.series[i].col.Data().Chunks()[0]
		if !array.ApproxEqual(a, b, array.WithNaNsEqual(true), array.WithAbsTolerance(1e-9)) {
			t.Errorf("%s: column %q differs\n got: %v\nwant: %v", label, g.series[i].name, a, b)
		}
	}
}

// TestStreamingAggregate_ParityWithEager — the columnar streaming
// aggregate matches the eager GroupBy on every key mode (single
// string, single int, composite with null keys) and aggregation kind,
// over multi-batch input with nulls, serial and parallel. Unordered
// output holds the same rows.
func TestStreamingAggregate_ParityWithEager(t *testing.T) {
	df := parityFrame(t)
	defer df.Release()
	prev := MaxParallelism()
	defer SetMaxParallelism(prev)
	for _, keys := range [][]string{{"ks"}, {"ki"}, {"ks", "kc"}} {
		eager, err := df.Lazy().GroupBy(keys...).Agg(parityAggs...).CollectRaw()
		if err != nil {
			t.Fatalf("keys %v eager: %v", keys, err)
		}
		sortKeys := make([]SortKey, len(keys))
		for i, k := range keys {
			sortKeys[i] = SortKey{Column: k}
		}
		eagerSorted, err := eager.SortBy(sortKeys...)
		if err != nil {
			t.Fatal(err)
		}
		for _, workers := range []int{1, 4} {
			SetMaxParallelism(workers)
			label := fmt.Sprintf("keys=%v workers=%d", keys, workers)
			got, err := df.Lazy().GroupBy(keys...).Agg(parityAggs...).Collect()
			if err != nil {
				t.Fatalf("%s: %v", label, err)
			}
			want := eager
			if keys[0] == "ki" {
				// Eager orders integer keys by their byte encoding
				// (negatives after positives); the streaming aggregate
				// orders them numerically.
				want = eagerSorted
			}
			assertSameFrame(t, label, got, want)
			got.Release()

			un, err := df.Lazy().GroupBy(keys...).Unordered().Agg(parityAggs...).Collect()
			if err != nil {
				t.Fatalf("%s unordered: %v", label, err)
			}
			unSorted, err := un.SortBy(sortKeys...)
			if err != nil {
				t.Fatal(err)
			}
			assertSameFrame(t, label+" unordered", unSorted, eagerSorted)
			un.Release()
			unSorted.Release()
		}
		eager.Release()
		eagerSorted.Release()
	}
}

// TestGroupBy_IntegerSumTypes — grouped sums of integer columns are
// Int64 (signed) or Uint64 (unsigned), widened from narrow types, in
// the eager and streaming engines; float sums stay Float64.
func TestGroupBy_IntegerSumTypes(t *testing.T) {
	fields := []arrow.Field{
		{Name: "k", Type: arrow.BinaryTypes.String},
		{Name: "i8", Type: arrow.PrimitiveTypes.Int8},
		{Name: "u16", Type: arrow.PrimitiveTypes.Uint16},
		{Name: "f", Type: arrow.PrimitiveTypes.Float32},
	}
	df := chunkedFrame(t, fields, 4, 4, func(c int, b array.Builder, lo, hi int) {
		for r := lo; r < hi; r++ {
			switch c {
			case 0:
				b.(*array.StringBuilder).Append("a")
			case 1:
				b.(*array.Int8Builder).Append(100) // 4 × 100 overflows int8
			case 2:
				b.(*array.Uint16Builder).Append(60000)
			case 3:
				b.(*array.Float32Builder).Append(0.5)
			}
		}
	})
	defer df.Release()
	aggs := []Aggregation{
		{Column: "i8", Kind: AggSum, Alias: "i8"},
		{Column: "u16", Kind: AggSum, Alias: "u16"},
		{Column: "f", Kind: AggSum, Alias: "f"},
	}
	gb, err := df.GroupBy("k")
	if err != nil {
		t.Fatal(err)
	}
	eager, err := gb.Agg(aggs...)
	if err != nil {
		t.Fatal(err)
	}
	defer eager.Release()
	streamed, err := df.Lazy().GroupBy("k").Agg(aggs...).Collect()
	if err != nil {
		t.Fatal(err)
	}
	defer streamed.Release()
	for _, f := range []*Frame{eager, streamed} {
		i8, _ := f.Column("i8")
		u16, _ := f.Column("u16")
		fl, _ := f.Column("f")
		if v := i8.col.Data().Chunks()[0].(*array.Int64).Value(0); v != 400 {
			t.Errorf("i8 sum = %d, want 400", v)
		}
		if v := u16.col.Data().Chunks()[0].(*array.Uint64).Value(0); v != 240000 {
			t.Errorf("u16 sum = %d, want 240000", v)
		}
		if v := fl.col.Data().Chunks()[0].(*array.Float64).Value(0); v != 2 {
			t.Errorf("float sum = %v, want 2", v)
		}
	}
	if !df.Lazy().GroupBy("k").Agg(aggs...).Schema().Equal(eager.Schema()) {
		t.Errorf("plan schema %s != result schema %s", df.Lazy().GroupBy("k").Agg(aggs...).Schema(), eager.Schema())
	}
}

// TestGroupBy_IntegerSumOverflow — an integer sum past the output
// type's range fails with ErrSumOverflow on every path, instead of
// wrapping.
func TestGroupBy_IntegerSumOverflow(t *testing.T) {
	for _, tc := range []struct {
		name string
		dt   arrow.DataType
		put  func(array.Builder)
	}{
		{"int64", arrow.PrimitiveTypes.Int64, func(b array.Builder) { b.(*array.Int64Builder).Append(math.MaxInt64/2 + 1) }},
		{"negative", arrow.PrimitiveTypes.Int64, func(b array.Builder) { b.(*array.Int64Builder).Append(math.MinInt64/2 - 1) }},
		{"uint64", arrow.PrimitiveTypes.Uint64, func(b array.Builder) { b.(*array.Uint64Builder).Append(math.MaxUint64/2 + 1) }},
	} {
		fields := []arrow.Field{{Name: "k", Type: arrow.PrimitiveTypes.Int64}, {Name: "v", Type: tc.dt}}
		df := chunkedFrame(t, fields, 2, 1, func(c int, b array.Builder, lo, hi int) {
			if c == 0 {
				b.(*array.Int64Builder).Append(1)
				return
			}
			tc.put(b)
		})
		agg := Aggregation{Column: "v", Kind: AggSum}
		gb, err := df.GroupBy("k")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := gb.Agg(agg); !errors.Is(err, ErrSumOverflow) {
			t.Errorf("%s eager: err = %v, want ErrSumOverflow", tc.name, err)
		}
		for _, w := range []int{1, 2} {
			prev := MaxParallelism()
			SetMaxParallelism(w)
			_, err := df.Lazy().GroupBy("k").Agg(agg).Collect()
			SetMaxParallelism(prev)
			if !errors.Is(err, ErrSumOverflow) {
				t.Errorf("%s streaming workers=%d: err = %v, want ErrSumOverflow", tc.name, w, err)
			}
		}
		df.Release()
	}
}

// TestStreamingAggregate_LastAcrossManyBatches — Last over hundreds of
// batches keeps the last non-null value per group, through the string
// arena's and the take-based capture's compaction.
func TestStreamingAggregate_LastAcrossManyBatches(t *testing.T) {
	const batches = 1500
	decType := &arrow.Decimal128Type{Precision: 10, Scale: 0}
	fields := []arrow.Field{
		{Name: "k", Type: arrow.BinaryTypes.String},
		{Name: "s", Type: arrow.BinaryTypes.String, Nullable: true},
		{Name: "d", Type: decType, Nullable: true},
	}
	long := strings.Repeat("x", 200)
	df := chunkedFrame(t, fields, 2*batches, 2, func(c int, b array.Builder, lo, hi int) {
		for r := lo; r < hi; r++ {
			batch := r / 2
			switch c {
			case 0:
				b.(*array.StringBuilder).Append([]string{"a", "b"}[r%2])
			case 1:
				// Group b's last batch is null: its Last is the batch before.
				if r%2 == 1 && batch == batches-1 {
					b.AppendNull()
				} else {
					b.(*array.StringBuilder).Append(fmt.Sprintf("%d-%s", batch, long))
				}
			case 2:
				if r%2 == 1 && batch == batches-1 {
					b.AppendNull()
				} else {
					b.(*array.Decimal128Builder).Append(decimal128.FromI64(int64(batch)))
				}
			}
		}
	})
	defer df.Release()
	for _, w := range []int{1, 2} {
		prev := MaxParallelism()
		SetMaxParallelism(w)
		out, err := df.Lazy().GroupBy("k").Agg(
			Aggregation{Column: "s", Kind: AggLast, Alias: "s"},
			Aggregation{Column: "d", Kind: AggLast, Alias: "d"},
			Aggregation{Column: "s", Kind: AggFirst, Alias: "s_first"},
		).Collect()
		SetMaxParallelism(prev)
		if err != nil {
			t.Fatalf("workers=%d: %v", w, err)
		}
		s, _ := out.Column("s")
		d, _ := out.Column("d")
		first, _ := out.Column("s_first")
		sArr := s.col.Data().Chunks()[0].(*array.String)
		dArr := d.col.Data().Chunks()[0].(*array.Decimal128)
		fArr := first.col.Data().Chunks()[0].(*array.String)
		for i, wantBatch := range []int{batches - 1, batches - 2} {
			if want := fmt.Sprintf("%d-%s", wantBatch, long); sArr.Value(i) != want {
				t.Errorf("workers=%d group %d: Last = %.12q…, want %.12q…", w, i, sArr.Value(i), want)
			}
			if got := dArr.Value(i).LowBits(); got != uint64(wantBatch) {
				t.Errorf("workers=%d group %d: decimal Last = %d, want %d", w, i, got, wantBatch)
			}
			if want := "0-" + long; fArr.Value(i) != want {
				t.Errorf("workers=%d group %d: First = %.12q…", w, i, fArr.Value(i))
			}
		}
		out.Release()
	}
}

// TestStreamingAggregate_FirstLastDates — First / Last on Date32 and
// Date64 columns stream through the typed storage and keep the type.
func TestStreamingAggregate_FirstLastDates(t *testing.T) {
	fields := []arrow.Field{
		{Name: "k", Type: arrow.BinaryTypes.String},
		{Name: "d32", Type: arrow.FixedWidthTypes.Date32, Nullable: true},
		{Name: "d64", Type: arrow.FixedWidthTypes.Date64, Nullable: true},
	}
	df := chunkedFrame(t, fields, 6, 2, func(c int, b array.Builder, lo, hi int) {
		for r := lo; r < hi; r++ {
			switch c {
			case 0:
				b.(*array.StringBuilder).Append([]string{"a", "b"}[r%2])
			case 1:
				if r == 0 {
					b.AppendNull()
				} else {
					b.(*array.Date32Builder).Append(arrow.Date32(100 + r))
				}
			case 2:
				b.(*array.Date64Builder).Append(arrow.Date64(int64(r) * 86_400_000))
			}
		}
	})
	defer df.Release()
	out, err := df.Lazy().GroupBy("k").Agg(
		Aggregation{Column: "d32", Kind: AggFirst, Alias: "d32_first"},
		Aggregation{Column: "d64", Kind: AggLast, Alias: "d64_last"},
	).Collect()
	if err != nil {
		t.Fatal(err)
	}
	defer out.Release()
	first, _ := out.Column("d32_first")
	last, _ := out.Column("d64_last")
	fa := first.col.Data().Chunks()[0].(*array.Date32)
	la := last.col.Data().Chunks()[0].(*array.Date64)
	// a: rows 0, 2, 4 (row 0's d32 is null); b: rows 1, 3, 5.
	if fa.Value(0) != 102 || fa.Value(1) != 101 {
		t.Errorf("d32 first = [%d %d], want [102 101]", fa.Value(0), fa.Value(1))
	}
	if la.Value(0) != 4*86_400_000 || la.Value(1) != 5*86_400_000 {
		t.Errorf("d64 last = [%d %d]", la.Value(0), la.Value(1))
	}
}

// TestGroupTable_PlannedTypeMismatch — a batch whose column doesn't
// fit the planned output type is an error, not a column of nulls.
func TestGroupTable_PlannedTypeMismatch(t *testing.T) {
	fields := []arrow.Field{{Name: "k", Type: arrow.BinaryTypes.String}, {Name: "v", Type: arrow.PrimitiveTypes.Float64}}
	df := chunkedFrame(t, fields, 2, 2, func(c int, b array.Builder, lo, hi int) {
		for range hi - lo {
			if c == 0 {
				b.(*array.StringBuilder).Append("a")
			} else {
				b.(*array.Float64Builder).Append(1.5)
			}
		}
	})
	defer df.Release()
	batch := frameToBatch(df)
	defer batch.Release()
	for _, tc := range []struct {
		agg Aggregation
		dt  arrow.DataType
	}{
		{Aggregation{Column: "v", Kind: AggSum}, arrow.PrimitiveTypes.Int64},
		{Aggregation{Column: "v", Kind: AggMax}, &arrow.TimestampType{Unit: arrow.Second}},
		{Aggregation{Column: "v", Kind: AggFirst}, arrow.BinaryTypes.String},
	} {
		out := arrow.NewSchema([]arrow.Field{{Name: "k", Type: arrow.BinaryTypes.String}, {Name: "x", Type: tc.dt}}, nil)
		tbl, err := newGroupTable(keyModeString1, []string{"k"}, []Aggregation{tc.agg}, out)
		if err != nil {
			t.Fatal(err)
		}
		if err := tbl.consume(batch, nil); !errors.Is(err, ErrColumnTypeMismatch) {
			t.Errorf("%v planned %s: err = %v, want ErrColumnTypeMismatch", tc.agg.Kind, tc.dt, err)
		}
		tbl.release()
	}
}

// cancelAtEOF yields its batches, then cancels the context as it
// reports io.EOF.
type cancelAtEOF struct {
	batches []arrow.RecordBatch
	schema  *arrow.Schema
	cancel  context.CancelFunc
}

func (c *cancelAtEOF) Next(context.Context) (arrow.RecordBatch, error) {
	if len(c.batches) == 0 {
		c.cancel()
		return nil, io.EOF
	}
	b := c.batches[0]
	c.batches = c.batches[1:]
	return b, nil
}
func (c *cancelAtEOF) Schema() *arrow.Schema { return c.schema }
func (c *cancelAtEOF) Close() error          { return nil }

// TestStreamingAggregate_CancelAfterEOF — a context cancelled just as
// the input ends is reported, not turned into a partial result.
func TestStreamingAggregate_CancelAfterEOF(t *testing.T) {
	df := parityFrame(t)
	defer df.Release()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	one, err := df.CompactChunks()
	if err != nil {
		t.Fatal(err)
	}
	defer one.Release()
	batch := frameToBatch(one)
	n := newAggregateNode(&scanFrameNode{frame: df}, []string{"ks"}, []Aggregation{{Kind: AggCount, Alias: "n"}})
	exec := &streamingAggregateExec{
		input:     &cancelAtEOF{batches: []arrow.RecordBatch{batch}, schema: df.Schema(), cancel: cancel},
		keys:      n.keys,
		aggs:      n.aggs,
		outSchema: n.outSchema,
		workers:   4,
		keyMode:   keyModeString1,
	}
	exec.rowsPool.New = func() any { s := make([]int, 0, 128); return &s }
	if _, err := exec.Next(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// TestValueCapture_PiecesStayFew — capturing a value per batch over
// many batches keeps a logarithmic number of pieces, in order.
func TestValueCapture_PiecesStayFew(t *testing.T) {
	const batches = 1000
	fields := []arrow.Field{{Name: "k", Type: arrow.PrimitiveTypes.Int64}}
	df := chunkedFrame(t, fields, batches, 1, func(c int, b array.Builder, lo, hi int) {
		b.(*array.Int64Builder).Append(int64(lo))
	})
	defer df.Release()
	out := arrow.NewSchema([]arrow.Field{{Name: "k", Type: arrow.PrimitiveTypes.Int64}, {Name: "n", Type: arrow.PrimitiveTypes.Int64}}, nil)
	tbl, err := newGroupTable(keyModeInt641, []string{"k"}, []Aggregation{{Kind: AggCount, Alias: "n"}}, out)
	if err != nil {
		t.Fatal(err)
	}
	defer tbl.release()
	col, _ := df.Column("k")
	for _, chunk := range col.col.Data().Chunks() {
		fld := arrow.Field{Name: "k", Type: chunk.DataType()}
		rb := array.NewRecordBatch(arrow.NewSchema([]arrow.Field{fld}, nil), []arrow.Array{chunk}, 1)
		if err := tbl.consume(rb, nil); err != nil {
			t.Fatal(err)
		}
		rb.Release()
	}
	if got := len(tbl.keyCaps[0].pieces); got > 12 {
		t.Errorf("%d pieces after %d batches, want O(log n)", got, batches)
	}
	tbl.finish(false, false)
	keys, err := takeCaptures(memory.DefaultAllocator, arrow.PrimitiveTypes.Int64,
		[]*valueCapture{&tbl.keyCaps[0]}, outputOrder([]*groupTable{tbl}, keyModeInt641, false),
		func(r rowRef) int32 { return r.g })
	if err != nil {
		t.Fatal(err)
	}
	defer keys.Release()
	for i, v := range keys.(*array.Int64).Int64Values() {
		if v != int64(i) {
			t.Fatalf("key %d = %d", i, v)
		}
	}
}
