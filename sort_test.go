package gobi

import (
	"errors"
	"math"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// sortFrame builds a small frame with mixed key types for SortBy tests:
//
//	name   score (f64)  qty (i64)  active (bool)   region (str)
//	Alpha    3.5           5         true            "US"
//	Bravo    1.0           2         false           "EU"
//	Charlie  3.5           3         true            "US"
//	Delta    2.0           7         false           "EU"
func sortFrame(t *testing.T) *Frame {
	t.Helper()
	pool := memory.DefaultAllocator

	nameB := array.NewStringBuilder(pool)
	defer nameB.Release()
	nameB.AppendValues([]string{"Alpha", "Bravo", "Charlie", "Delta"}, nil)

	scoreB := array.NewFloat64Builder(pool)
	defer scoreB.Release()
	scoreB.AppendValues([]float64{3.5, 1.0, 3.5, 2.0}, nil)

	qtyB := array.NewInt64Builder(pool)
	defer qtyB.Release()
	qtyB.AppendValues([]int64{5, 2, 3, 7}, nil)

	activeB := array.NewBooleanBuilder(pool)
	defer activeB.Release()
	activeB.AppendValues([]bool{true, false, true, false}, nil)

	regionB := array.NewStringBuilder(pool)
	defer regionB.Release()
	regionB.AppendValues([]string{"US", "EU", "US", "EU"}, nil)

	fields := []arrow.Field{
		{Name: "name", Type: arrow.BinaryTypes.String, Nullable: false},
		{Name: "score", Type: arrow.PrimitiveTypes.Float64, Nullable: true},
		{Name: "qty", Type: arrow.PrimitiveTypes.Int64, Nullable: true},
		{Name: "active", Type: arrow.FixedWidthTypes.Boolean, Nullable: true},
		{Name: "region", Type: arrow.BinaryTypes.String, Nullable: true},
	}
	schema := arrow.NewSchema(fields, nil)
	arrs := []arrow.Array{
		nameB.NewArray(), scoreB.NewArray(), qtyB.NewArray(),
		activeB.NewArray(), regionB.NewArray(),
	}
	defer func() {
		for _, a := range arrs {
			a.Release()
		}
	}()
	cols := make([]arrow.Column, len(fields))
	for i, a := range arrs {
		chunked := arrow.NewChunked(a.DataType(), []arrow.Array{a})
		cols[i] = *arrow.NewColumn(fields[i], chunked)
	}
	f, err := NewFrame(schema, cols)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// sortedNames reads the "name" column of df and returns its values in
// row order. Used to assert sort orderings without repeating chunk-walk
// boilerplate in every test.
func sortedNames(t *testing.T, df *Frame) []string {
	t.Helper()
	col, err := df.Column("name")
	if err != nil {
		t.Fatal(err)
	}
	arr := col.Column().Data().Chunks()[0].(*array.String)
	out := make([]string, arr.Len())
	for i := range arr.Len() {
		out[i] = arr.Value(i)
	}
	return out
}

func TestSortBy_SingleKeyAsc(t *testing.T) {
	df := sortFrame(t)
	out, err := df.SortBy(SortKey{Column: "qty"})
	if err != nil {
		t.Fatal(err)
	}
	// qty ascending: 2 (Bravo), 3 (Charlie), 5 (Alpha), 7 (Delta)
	got := sortedNames(t, out)
	want := []string{"Bravo", "Charlie", "Alpha", "Delta"}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("row %d = %s, want %s", i, got[i], w)
		}
	}
}

func TestSortBy_SingleKeyDesc(t *testing.T) {
	df := sortFrame(t)
	out, err := df.SortBy(SortKey{Column: "qty", Descending: true})
	if err != nil {
		t.Fatal(err)
	}
	// qty descending: 7 (Delta), 5 (Alpha), 3 (Charlie), 2 (Bravo)
	got := sortedNames(t, out)
	want := []string{"Delta", "Alpha", "Charlie", "Bravo"}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("row %d = %s, want %s", i, got[i], w)
		}
	}
}

func TestSortBy_MultiKeyTiebreaker(t *testing.T) {
	df := sortFrame(t)
	// score asc, then name asc → 1.0 (Bravo), 2.0 (Delta),
	//                            then 3.5 tie broken by name: Alpha, Charlie
	out, err := df.SortBy(
		SortKey{Column: "score"},
		SortKey{Column: "name"},
	)
	if err != nil {
		t.Fatal(err)
	}
	got := sortedNames(t, out)
	want := []string{"Bravo", "Delta", "Alpha", "Charlie"}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("row %d = %s, want %s", i, got[i], w)
		}
	}
}

func TestSortBy_MultiKeyMixedDirection(t *testing.T) {
	df := sortFrame(t)
	// region asc (EU before US), then score desc within region.
	// EU: Bravo (1.0), Delta (2.0) → sorted desc: Delta, Bravo.
	// US: Alpha (3.5), Charlie (3.5) → tie on score desc → stable
	//     retains input order: Alpha, Charlie.
	out, err := df.SortBy(
		SortKey{Column: "region"},
		SortKey{Column: "score", Descending: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	got := sortedNames(t, out)
	want := []string{"Delta", "Bravo", "Alpha", "Charlie"}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("row %d = %s, want %s", i, got[i], w)
		}
	}
}

func TestSortBy_Stable(t *testing.T) {
	// A single key where two rows tie: score=3.5 for Alpha and Charlie.
	// Stable sort preserves their relative input order regardless of
	// direction (their tie doesn't swap).
	df := sortFrame(t)
	for _, desc := range []bool{false, true} {
		out, err := df.SortBy(SortKey{Column: "active", Descending: desc})
		if err != nil {
			t.Fatal(err)
		}
		got := sortedNames(t, out)
		// active=true rows: Alpha, Charlie (input order).
		// active=false rows: Bravo, Delta (input order).
		var trueFirst, falseFirst []string
		if desc {
			trueFirst = []string{"Alpha", "Charlie"}
			falseFirst = []string{"Bravo", "Delta"}
			if got[0] != trueFirst[0] || got[1] != trueFirst[1] ||
				got[2] != falseFirst[0] || got[3] != falseFirst[1] {
				t.Errorf("desc stable: got %v", got)
			}
		} else {
			trueFirst = []string{"Bravo", "Delta"}
			falseFirst = []string{"Alpha", "Charlie"}
			if got[0] != trueFirst[0] || got[1] != trueFirst[1] ||
				got[2] != falseFirst[0] || got[3] != falseFirst[1] {
				t.Errorf("asc stable: got %v", got)
			}
		}
	}
}

func TestSortBy_NullsSortLast(t *testing.T) {
	// Build a frame where qty has a null in the middle. On both
	// ascending and descending sorts, the null must sink to the end.
	pool := memory.DefaultAllocator
	nameB := array.NewStringBuilder(pool)
	defer nameB.Release()
	nameB.AppendValues([]string{"A", "B", "C", "D"}, nil)
	qtyB := array.NewInt64Builder(pool)
	defer qtyB.Release()
	qtyB.AppendValues([]int64{5, 0, 2, 8}, []bool{true, false, true, true}) // B null

	fields := []arrow.Field{
		{Name: "name", Type: arrow.BinaryTypes.String, Nullable: false},
		{Name: "qty", Type: arrow.PrimitiveTypes.Int64, Nullable: true},
	}
	schema := arrow.NewSchema(fields, nil)
	arrs := []arrow.Array{nameB.NewArray(), qtyB.NewArray()}
	defer func() {
		for _, a := range arrs {
			a.Release()
		}
	}()
	cols := make([]arrow.Column, 2)
	for i, a := range arrs {
		chunked := arrow.NewChunked(a.DataType(), []arrow.Array{a})
		cols[i] = *arrow.NewColumn(fields[i], chunked)
	}
	df, err := NewFrame(schema, cols)
	if err != nil {
		t.Fatal(err)
	}

	for _, desc := range []bool{false, true} {
		out, err := df.SortBy(SortKey{Column: "qty", Descending: desc})
		if err != nil {
			t.Fatal(err)
		}
		got := sortedNames(t, out)
		if got[len(got)-1] != "B" {
			t.Errorf("desc=%v: null row should be last, got %v", desc, got)
		}
	}
}

func TestSortBy_NaNSortsLikeNullLast(t *testing.T) {
	// Float64 with a NaN — NaN sorts to the end regardless of direction.
	pool := memory.DefaultAllocator
	nameB := array.NewStringBuilder(pool)
	defer nameB.Release()
	nameB.AppendValues([]string{"A", "B", "C"}, nil)
	scoreB := array.NewFloat64Builder(pool)
	defer scoreB.Release()
	scoreB.AppendValues([]float64{1.5, math.NaN(), 0.5}, nil)

	fields := []arrow.Field{
		{Name: "name", Type: arrow.BinaryTypes.String, Nullable: false},
		{Name: "score", Type: arrow.PrimitiveTypes.Float64, Nullable: true},
	}
	schema := arrow.NewSchema(fields, nil)
	arrs := []arrow.Array{nameB.NewArray(), scoreB.NewArray()}
	defer func() {
		for _, a := range arrs {
			a.Release()
		}
	}()
	cols := make([]arrow.Column, 2)
	for i, a := range arrs {
		chunked := arrow.NewChunked(a.DataType(), []arrow.Array{a})
		cols[i] = *arrow.NewColumn(fields[i], chunked)
	}
	df, _ := NewFrame(schema, cols)

	out, err := df.SortBy(SortKey{Column: "score"})
	if err != nil {
		t.Fatal(err)
	}
	got := sortedNames(t, out)
	// asc: 0.5 (C), 1.5 (A), NaN (B)
	if got[2] != "B" {
		t.Fatalf("NaN row should be last, got %v", got)
	}
}

func TestSortBy_TimestampKey(t *testing.T) {
	pool := memory.DefaultAllocator
	tsType := &arrow.TimestampType{Unit: arrow.Nanosecond}
	nameB := array.NewStringBuilder(pool)
	defer nameB.Release()
	nameB.AppendValues([]string{"A", "B", "C"}, nil)
	tsB := array.NewTimestampBuilder(pool, tsType)
	defer tsB.Release()
	tsB.Append(arrow.Timestamp(3_000_000))
	tsB.Append(arrow.Timestamp(1_000_000))
	tsB.Append(arrow.Timestamp(2_000_000))

	fields := []arrow.Field{
		{Name: "name", Type: arrow.BinaryTypes.String, Nullable: false},
		{Name: "when", Type: tsType, Nullable: true},
	}
	schema := arrow.NewSchema(fields, nil)
	arrs := []arrow.Array{nameB.NewArray(), tsB.NewArray()}
	defer func() {
		for _, a := range arrs {
			a.Release()
		}
	}()
	cols := make([]arrow.Column, 2)
	for i, a := range arrs {
		chunked := arrow.NewChunked(a.DataType(), []arrow.Array{a})
		cols[i] = *arrow.NewColumn(fields[i], chunked)
	}
	df, _ := NewFrame(schema, cols)

	out, err := df.SortBy(SortKey{Column: "when"})
	if err != nil {
		t.Fatal(err)
	}
	got := sortedNames(t, out)
	// ascending timestamps: 1M (B), 2M (C), 3M (A)
	want := []string{"B", "C", "A"}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("row %d = %s, want %s", i, got[i], w)
		}
	}
}

func TestSortBy_NoKeysErrors(t *testing.T) {
	df := sortFrame(t)
	if _, err := df.SortBy(); err == nil {
		t.Fatal("expected error for zero keys")
	}
}

func TestSortBy_MissingColumnErrors(t *testing.T) {
	df := sortFrame(t)
	_, err := df.SortBy(SortKey{Column: "nope"})
	if !errors.Is(err, ErrColumnNotFound) {
		t.Fatalf("want ErrColumnNotFound, got %v", err)
	}
}
