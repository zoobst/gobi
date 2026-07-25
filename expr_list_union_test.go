package gobi

import (
	"strings"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// twoListFrame builds a small frame with two List<Int64> columns for
// per-row union testing.
//
//	row  a              b
//	0    [1, 2, 3]      [3, 4, 5]
//	1    [10, 20]       [20, 30]
//	2    []             [100]
//	3    [7]            []
//	4    [1, 1, 2]      [2, 2, 3]   // duplicates within a single list
//	5    null           [999]       // null propagates
//	6    [42]           null        // null propagates
//	7    []             []          // both empty → empty (non-null)
func twoListFrame(t *testing.T, elemType arrow.DataType) *Frame {
	t.Helper()
	pool := memory.DefaultAllocator

	buildList := func(rows [][]int64, nullRows map[int]bool) arrow.Array {
		lb := array.NewListBuilder(pool, elemType)
		defer lb.Release()
		vb := lb.ValueBuilder()
		switch b := vb.(type) {
		case *array.Int64Builder:
			for i, xs := range rows {
				if nullRows[i] {
					lb.AppendNull()
					continue
				}
				lb.Append(true)
				for _, x := range xs {
					b.Append(x)
				}
			}
		case *array.StringBuilder:
			for i, xs := range rows {
				if nullRows[i] {
					lb.AppendNull()
					continue
				}
				lb.Append(true)
				for _, x := range xs {
					// convert numeric fixture to string
					b.Append(itoaHelper(x))
				}
			}
		}
		return lb.NewArray()
	}

	rowsA := [][]int64{
		{1, 2, 3},
		{10, 20},
		{},
		{7},
		{1, 1, 2},
		{}, // null via nullRows
		{42},
		{},
	}
	rowsB := [][]int64{
		{3, 4, 5},
		{20, 30},
		{100},
		{},
		{2, 2, 3},
		{999},
		{}, // null via nullRows
		{},
	}
	aArr := buildList(rowsA, map[int]bool{5: true})
	bArr := buildList(rowsB, map[int]bool{6: true})
	defer aArr.Release()
	defer bArr.Release()

	fields := []arrow.Field{
		{Name: "a", Type: arrow.ListOf(elemType), Nullable: true},
		{Name: "b", Type: arrow.ListOf(elemType), Nullable: true},
	}
	schema := arrow.NewSchema(fields, nil)
	cols := []arrow.Column{
		*arrow.NewColumn(fields[0], arrow.NewChunked(aArr.DataType(), []arrow.Array{aArr})),
		*arrow.NewColumn(fields[1], arrow.NewChunked(bArr.DataType(), []arrow.Array{bArr})),
	}
	f, err := NewFrame(schema, cols)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// itoaHelper is a tiny local int64→string converter to avoid importing
// strconv for a fixture-only need.
func itoaHelper(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// listRow extracts row i from a List column as []int64.
func listRowInt64(t *testing.T, s Series, i int) (vals []int64, isNull bool) {
	la := s.col.Data().Chunks()[0].(*array.List)
	if la.IsNull(i) {
		return nil, true
	}
	values := la.ListValues().(*array.Int64)
	start, end := la.ValueOffsets(i)
	out := make([]int64, end-start)
	for j := start; j < end; j++ {
		out[j-start] = values.Value(int(j))
	}
	return out, false
}

func TestListUnion_Int64Basic(t *testing.T) {
	f := twoListFrame(t, arrow.PrimitiveTypes.Int64)
	out, err := f.WithColumnExpr("ab", Col("a").ListUnion(Col("b")))
	if err != nil {
		t.Fatal(err)
	}
	ab, _ := out.Column("ab")
	if ab.DataType().ID() != arrow.LIST {
		t.Fatalf("ab type = %s, want LIST", ab.DataType())
	}
	// Row 0: [1,2,3] ∪ [3,4,5] preserving first-seen order = [1,2,3,4,5].
	if got, isNull := listRowInt64(t, ab, 0); isNull || !int64Equal(got, []int64{1, 2, 3, 4, 5}) {
		t.Fatalf("row 0 = %v (null=%v), want [1 2 3 4 5]", got, isNull)
	}
	// Row 1: [10,20] ∪ [20,30] = [10,20,30]
	if got, _ := listRowInt64(t, ab, 1); !int64Equal(got, []int64{10, 20, 30}) {
		t.Fatalf("row 1 = %v, want [10 20 30]", got)
	}
	// Row 2: [] ∪ [100] = [100]
	if got, _ := listRowInt64(t, ab, 2); !int64Equal(got, []int64{100}) {
		t.Fatalf("row 2 = %v, want [100]", got)
	}
	// Row 3: [7] ∪ [] = [7]
	if got, _ := listRowInt64(t, ab, 3); !int64Equal(got, []int64{7}) {
		t.Fatalf("row 3 = %v, want [7]", got)
	}
	// Row 4: [1,1,2] ∪ [2,2,3] with dedup = [1,2,3]
	if got, _ := listRowInt64(t, ab, 4); !int64Equal(got, []int64{1, 2, 3}) {
		t.Fatalf("row 4 = %v, want [1 2 3]", got)
	}
	// Row 5: null ∪ anything = null
	if _, isNull := listRowInt64(t, ab, 5); !isNull {
		t.Fatalf("row 5 should be null (left side null)")
	}
	// Row 6: anything ∪ null = null
	if _, isNull := listRowInt64(t, ab, 6); !isNull {
		t.Fatalf("row 6 should be null (right side null)")
	}
	// Row 7: [] ∪ [] = [] (non-null)
	got, isNull := listRowInt64(t, ab, 7)
	if isNull {
		t.Fatalf("row 7 both-empty union should be non-null empty list")
	}
	if len(got) != 0 {
		t.Fatalf("row 7 = %v, want []", got)
	}
}

func TestListUnion_StringDedupPreservesOrder(t *testing.T) {
	f := twoListFrame(t, arrow.BinaryTypes.String)
	out, err := f.WithColumnExpr("ab", Col("a").ListUnion(Col("b")))
	if err != nil {
		t.Fatal(err)
	}
	ab, _ := out.Column("ab")
	la := ab.col.Data().Chunks()[0].(*array.List)
	values := la.ListValues().(*array.String)
	get := func(row int) []string {
		start, end := la.ValueOffsets(row)
		out := make([]string, end-start)
		for j := start; j < end; j++ {
			out[j-start] = values.Value(int(j))
		}
		return out
	}
	// Row 4: ["1","1","2"] ∪ ["2","2","3"] = ["1","2","3"]
	if got := get(4); len(got) != 3 || got[0] != "1" || got[1] != "2" || got[2] != "3" {
		t.Fatalf("row 4 = %v, want [1 2 3]", got)
	}
}

// Element-type mismatch surfaces at eval time with a clear error.
func TestListUnion_ElementTypeMismatch(t *testing.T) {
	pool := memory.DefaultAllocator
	i64B := array.NewListBuilder(pool, arrow.PrimitiveTypes.Int64)
	defer i64B.Release()
	i64B.Append(true)
	i64B.ValueBuilder().(*array.Int64Builder).Append(1)
	sB := array.NewListBuilder(pool, arrow.BinaryTypes.String)
	defer sB.Release()
	sB.Append(true)
	sB.ValueBuilder().(*array.StringBuilder).Append("x")

	iArr := i64B.NewArray()
	defer iArr.Release()
	sArr := sB.NewArray()
	defer sArr.Release()

	fields := []arrow.Field{
		{Name: "a", Type: arrow.ListOf(arrow.PrimitiveTypes.Int64), Nullable: true},
		{Name: "b", Type: arrow.ListOf(arrow.BinaryTypes.String), Nullable: true},
	}
	schema := arrow.NewSchema(fields, nil)
	cols := []arrow.Column{
		*arrow.NewColumn(fields[0], arrow.NewChunked(iArr.DataType(), []arrow.Array{iArr})),
		*arrow.NewColumn(fields[1], arrow.NewChunked(sArr.DataType(), []arrow.Array{sArr})),
	}
	f, err := NewFrame(schema, cols)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WithColumnExpr("ab", Col("a").ListUnion(Col("b")))
	if err == nil {
		t.Fatal("expected element-type-mismatch error")
	}
	if !strings.Contains(err.Error(), "element-type mismatch") {
		t.Fatalf("error should mention element-type mismatch; got %v", err)
	}
}

// Non-list inputs are rejected.
func TestListUnion_NonListInputs(t *testing.T) {
	f := nullyFrame(t)
	_, err := f.WithColumnExpr("ab", Col("price").ListUnion(Col("tag")))
	if err == nil {
		t.Fatal("expected type error for non-List inputs")
	}
}

// End-to-end via LazyFrame.Collect — the "aligned pipeline" pattern
// the user described: two CollectSet-style aggregations joined on cell,
// then ListUnion them.
func TestListUnion_LazyChainWithCollectSet(t *testing.T) {
	f := setAggFrame(t)
	// Two independent aggregations of provider distinct-sets per region.
	// Then chain a ListUnion on itself just to verify it works in-plan.
	out, err := f.Lazy().
		GroupBy("region").
		Agg(Aggregation{Column: "provider", Fn: NewStringSetAggregator(), Alias: "s"}).
		WithColumn("s2", Col("s").ListUnion(Col("s"))).
		Collect()
	if err != nil {
		t.Fatal(err)
	}
	// s ∪ s = s (idempotent). Row count should still be 2 (NA, EU).
	if r, _ := out.Shape(); r != 2 {
		t.Fatalf("row count = %d, want 2", r)
	}
	s, _ := out.Column("s")
	s2, _ := out.Column("s2")
	// Both columns should hold the same lists after idempotent union.
	sLA := s.col.Data().Chunks()[0].(*array.List)
	s2LA := s2.col.Data().Chunks()[0].(*array.List)
	for i := 0; i < 2; i++ {
		s1Start, s1End := sLA.ValueOffsets(i)
		s2Start, s2End := s2LA.ValueOffsets(i)
		if (s1End - s1Start) != (s2End - s2Start) {
			t.Fatalf("row %d list length differs after idempotent union: %d vs %d",
				i, s1End-s1Start, s2End-s2Start)
		}
	}
}

func int64Equal(a, b []int64) bool {
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
