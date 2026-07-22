package gobi

import (
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// buildSetOpFixture returns two small frames with the same schema
// so set-op semantics can be checked without wrestling with the
// lazyFrame helper. Layout matches the shape callers usually pass
// to Union/Intersect/Difference: a two-column table with an int
// key + string label.
func buildSetOpFixture(t *testing.T) (*Frame, *Frame) {
	t.Helper()
	build := func(ids []int64, labels []string) *Frame {
		pool := memory.DefaultAllocator
		ib := array.NewInt64Builder(pool)
		defer ib.Release()
		ib.AppendValues(ids, nil)
		lb := array.NewStringBuilder(pool)
		defer lb.Release()
		lb.AppendValues(labels, nil)
		fields := []arrow.Field{
			{Name: "id", Type: arrow.PrimitiveTypes.Int64, Nullable: false},
			{Name: "label", Type: arrow.BinaryTypes.String, Nullable: false},
		}
		schema := arrow.NewSchema(fields, nil)
		idArr, labelArr := ib.NewArray(), lb.NewArray()
		defer idArr.Release()
		defer labelArr.Release()
		cols := []arrow.Column{
			*arrow.NewColumn(fields[0], arrow.NewChunked(idArr.DataType(), []arrow.Array{idArr})),
			*arrow.NewColumn(fields[1], arrow.NewChunked(labelArr.DataType(), []arrow.Array{labelArr})),
		}
		f, err := NewFrame(schema, cols)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	left := build([]int64{1, 2, 3, 2}, []string{"a", "b", "c", "b"})    // 4 rows, (2,"b") is dup
	right := build([]int64{2, 3, 4}, []string{"b", "c", "d"})           // 3 rows, disjoint from row 0
	return left, right
}

// TestFrame_Concat_RowStack — 4 + 3 = 7 rows, dupes preserved.
func TestFrame_Concat_RowStack(t *testing.T) {
	l, r := buildSetOpFixture(t)
	out, err := l.Concat(r)
	if err != nil {
		t.Fatal(err)
	}
	if out.NumRows() != 7 {
		t.Fatalf("rows = %d, want 7", out.NumRows())
	}
	// (2,"b") appears twice on the left, once on the right — total 3.
	ids, _ := out.Column("id")
	labels, _ := out.Column("label")
	count := 0
	for i := 0; i < int(out.NumRows()); i++ {
		v, _, _ := ids.numericAt(i)
		lab, err := readScalarAt(labels, i)
		if err != nil {
			t.Fatal(err)
		}
		if int64(v) == 2 && lab == "b" {
			count++
		}
	}
	if count != 3 {
		t.Fatalf("(2,b) appears %d times in Concat output, want 3", count)
	}
}

// TestFrame_Concat_ManyFrames covers variadic Concat with N>1.
func TestFrame_Concat_ManyFrames(t *testing.T) {
	l, r := buildSetOpFixture(t)
	out, err := l.Concat(r, r)
	if err != nil {
		t.Fatal(err)
	}
	if out.NumRows() != 4+3+3 {
		t.Fatalf("rows = %d, want 10", out.NumRows())
	}
}

// TestConcat_PackageLevel exercises the free-function form. Given a
// []*Frame slice, callers should be able to `gobi.Concat(slice...)`
// without threading through slice[0].Concat(slice[1:]...).
func TestConcat_PackageLevel(t *testing.T) {
	l, r := buildSetOpFixture(t)
	frames := []*Frame{l, r, r}
	out, err := Concat(frames...)
	if err != nil {
		t.Fatal(err)
	}
	if out.NumRows() != 4+3+3 {
		t.Fatalf("rows = %d, want 10", out.NumRows())
	}
}

// TestConcat_PackageLevel_Empty — no frames must be an error, not a
// panic (there's no schema to synthesize an empty frame from).
func TestConcat_PackageLevel_Empty(t *testing.T) {
	if _, err := Concat(); err == nil {
		t.Fatal("Concat() with no frames should error")
	}
}

// TestFrame_Concat_SchemaMismatch — error must name the offending
// column and both types so the caller knows what to cast.
func TestFrame_Concat_SchemaMismatch(t *testing.T) {
	l, _ := buildSetOpFixture(t)
	// Build a frame with the same names but different id type.
	pool := memory.DefaultAllocator
	ib := array.NewInt32Builder(pool) // Int32 vs left's Int64
	defer ib.Release()
	ib.AppendValues([]int32{10, 20}, nil)
	lb := array.NewStringBuilder(pool)
	defer lb.Release()
	lb.AppendValues([]string{"x", "y"}, nil)
	fields := []arrow.Field{
		{Name: "id", Type: arrow.PrimitiveTypes.Int32, Nullable: false},
		{Name: "label", Type: arrow.BinaryTypes.String, Nullable: false},
	}
	schema := arrow.NewSchema(fields, nil)
	idArr, labelArr := ib.NewArray(), lb.NewArray()
	defer idArr.Release()
	defer labelArr.Release()
	badR, err := NewFrame(schema, []arrow.Column{
		*arrow.NewColumn(fields[0], arrow.NewChunked(idArr.DataType(), []arrow.Array{idArr})),
		*arrow.NewColumn(fields[1], arrow.NewChunked(labelArr.DataType(), []arrow.Array{labelArr})),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = l.Concat(badR)
	if err == nil {
		t.Fatal("expected error for int32 vs int64 mismatch")
	}
	if !containsAll(err.Error(), []string{"id", "int32", "int64", "cast"}) {
		t.Fatalf("error message must name column + both types + a cast suggestion; got: %v", err)
	}
}

// TestFrame_Union_DedupeAllCols — (1,a),(2,b),(3,c),(4,d).
// Left has (2,b) twice, right has (2,b),(3,c). Union deduped over
// all cols should be 4 distinct rows.
func TestFrame_Union_DedupeAllCols(t *testing.T) {
	l, r := buildSetOpFixture(t)
	out, err := l.Union(r)
	if err != nil {
		t.Fatal(err)
	}
	if out.NumRows() != 4 {
		t.Fatalf("rows = %d, want 4", out.NumRows())
	}
}

// TestFrame_Union_DedupeSubsetCol — dedupe just by id. Left ids are
// {1,2,3,2} → distinct {1,2,3}; right adds id=4 → total 4 distinct.
func TestFrame_Union_DedupeSubsetCol(t *testing.T) {
	l, r := buildSetOpFixture(t)
	out, err := l.Union(r, "id")
	if err != nil {
		t.Fatal(err)
	}
	if out.NumRows() != 4 {
		t.Fatalf("rows = %d, want 4 distinct ids", out.NumRows())
	}
}

// TestFrame_Intersect — rows in both. Left has {(1,a),(2,b),(3,c),(2,b)};
// right has {(2,b),(3,c),(4,d)}. Intersection deduped over all cols
// = {(2,b),(3,c)}.
func TestFrame_Intersect(t *testing.T) {
	l, r := buildSetOpFixture(t)
	out, err := l.Intersect(r)
	if err != nil {
		t.Fatal(err)
	}
	if out.NumRows() != 2 {
		t.Fatalf("rows = %d, want 2", out.NumRows())
	}
}

// TestFrame_Difference — rows in left but not right. Left distinct
// = {(1,a),(2,b),(3,c)}. Right key set covers (2,b) and (3,c). So
// diff = {(1,a)}.
func TestFrame_Difference(t *testing.T) {
	l, r := buildSetOpFixture(t)
	out, err := l.Difference(r)
	if err != nil {
		t.Fatal(err)
	}
	if out.NumRows() != 1 {
		t.Fatalf("rows = %d, want 1", out.NumRows())
	}
	ids, _ := out.Column("id")
	v, _, _ := ids.numericAt(0)
	if int64(v) != 1 {
		t.Fatalf("id = %v, want 1", v)
	}
}

// TestSeries_Concat_UnionEtc smoke-checks the Series counterparts.
func TestSeries_Concat_UnionEtc(t *testing.T) {
	l, r := buildSetOpFixture(t)
	a, _ := l.Column("id")
	b, _ := r.Column("id")

	// Concat: 4 + 3 rows.
	c, err := a.Concat(b)
	if err != nil {
		t.Fatal(err)
	}
	if c.Len() != 7 {
		t.Fatalf("Concat len = %d, want 7", c.Len())
	}

	// Union distinct = {1,2,3,4} = 4 values.
	u, err := a.Union(b)
	if err != nil {
		t.Fatal(err)
	}
	if u.Len() != 4 {
		t.Fatalf("Union len = %d, want 4", u.Len())
	}

	// Intersect distinct = {2,3} = 2 values.
	i, err := a.Intersect(b)
	if err != nil {
		t.Fatal(err)
	}
	if i.Len() != 2 {
		t.Fatalf("Intersect len = %d, want 2", i.Len())
	}

	// Difference distinct = {1} = 1 value.
	d, err := a.Difference(b)
	if err != nil {
		t.Fatal(err)
	}
	if d.Len() != 1 {
		t.Fatalf("Difference len = %d, want 1", d.Len())
	}
}

// TestSeries_SetOp_TypeMismatch — an Int64 series vs a String
// series should error at Concat / Intersect / etc.
func TestSeries_SetOp_TypeMismatch(t *testing.T) {
	l, _ := buildSetOpFixture(t)
	ids, _ := l.Column("id")
	labels, _ := l.Column("label")
	if _, err := ids.Concat(labels); err == nil {
		t.Fatal("Concat across mismatched types should error")
	}
	if _, err := ids.Intersect(labels); err == nil {
		t.Fatal("Intersect across mismatched types should error")
	}
}

// containsAll returns true when s contains every substr. Cheaper
// than pulling in strings.Contains chains inline.
func containsAll(s string, subs []string) bool {
	for _, sub := range subs {
		if !stringContains(s, sub) {
			return false
		}
	}
	return true
}

func stringContains(s, sub string) bool {
	// Naive substring check; test-only, no need for strings.Contains
	// import here since one call site is fine.
	if len(sub) == 0 {
		return true
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
