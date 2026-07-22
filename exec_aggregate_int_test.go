package gobi

import (
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
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
