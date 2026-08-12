package gobi

import (
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// listJoinFrame builds a small frame keyed by "cell" with a
// List<String> "providers" column — the exact shape Frame.Join must
// carry across for the unified-pipeline use case.
func listJoinFrame(t *testing.T, cells []int64, lists [][]string, nullListRows map[int]bool) *Frame {
	t.Helper()
	pool := memory.DefaultAllocator

	cellB := array.NewInt64Builder(pool)
	defer cellB.Release()
	cellB.AppendValues(cells, nil)

	lb := array.NewListBuilder(pool, arrow.BinaryTypes.String)
	defer lb.Release()
	vb := lb.ValueBuilder().(*array.StringBuilder)
	for i, xs := range lists {
		if nullListRows[i] {
			lb.AppendNull()
			continue
		}
		lb.Append(true)
		for _, x := range xs {
			vb.Append(x)
		}
	}

	fields := []arrow.Field{
		{Name: "cell", Type: arrow.PrimitiveTypes.Int64, Nullable: false},
		{Name: "providers", Type: arrow.ListOf(arrow.BinaryTypes.String), Nullable: true},
	}
	schema := arrow.NewSchema(fields, nil)
	cellArr := cellB.NewArray()
	defer cellArr.Release()
	listArr := lb.NewArray()
	defer listArr.Release()
	cols := []arrow.Column{
		*arrow.NewColumn(fields[0], arrow.NewChunked(cellArr.DataType(), []arrow.Array{cellArr})),
		*arrow.NewColumn(fields[1], arrow.NewChunked(listArr.DataType(), []arrow.Array{listArr})),
	}
	f, err := NewFrame(schema, cols)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// getListRow extracts row i from a List<String> column as []string
// (nil for null lists).
func getListRow(t *testing.T, s Series, i int) (vals []string, isNull bool) {
	t.Helper()
	la := s.col.Data().Chunks()[0].(*array.List)
	if la.IsNull(i) {
		return nil, true
	}
	values := la.ListValues().(*array.String)
	start, end := la.ValueOffsets(i)
	out := make([]string, end-start)
	for j := start; j < end; j++ {
		out[j-start] = values.Value(int(j))
	}
	return out, false
}

func TestJoin_InnerCarriesListStringColumn(t *testing.T) {
	// left:  cell=[1,2,3] providers=[[a],[b,c],[d]]
	// right: cell=[2,3,4] providers=[[X],[Y,Z],[W]]
	// Inner-join on cell: rows for 2 and 3 survive.
	left := listJoinFrame(t,
		[]int64{1, 2, 3},
		[][]string{{"a"}, {"b", "c"}, {"d"}},
		nil,
	)
	right := listJoinFrame(t,
		[]int64{2, 3, 4},
		[][]string{{"X"}, {"Y", "Z"}, {"W"}},
		nil,
	)
	// Rename right's "providers" so Join doesn't collide with the auto
	// _right suffix (easier to assert on).
	right, err := right.Rename("providers", "providers_r")
	if err != nil {
		t.Fatal(err)
	}
	out, err := left.Join(right, "cell", "cell", JoinInner)
	if err != nil {
		t.Fatalf("Inner join with List<String>: %v", err)
	}
	if out.NumRows() != 2 {
		t.Fatalf("row count = %d, want 2", out.NumRows())
	}
	// Row 0 = cell=2, left providers=[b,c], right providers=[X].
	// Row 1 = cell=3, left providers=[d], right providers=[Y,Z].
	lps, _ := out.Column("providers")
	rps, _ := out.Column("providers_r")
	if got, isNull := getListRow(t, lps, 0); isNull || !stringSliceEqual(got, []string{"b", "c"}) {
		t.Fatalf("row 0 providers = %v (null=%v), want [b c]", got, isNull)
	}
	if got, isNull := getListRow(t, rps, 0); isNull || !stringSliceEqual(got, []string{"X"}) {
		t.Fatalf("row 0 providers_r = %v (null=%v), want [X]", got, isNull)
	}
	if got, _ := getListRow(t, lps, 1); !stringSliceEqual(got, []string{"d"}) {
		t.Fatalf("row 1 providers = %v, want [d]", got)
	}
	if got, _ := getListRow(t, rps, 1); !stringSliceEqual(got, []string{"Y", "Z"}) {
		t.Fatalf("row 1 providers_r = %v, want [Y Z]", got)
	}
}

func TestJoin_FullCarriesListStringColumnWithNulls(t *testing.T) {
	// left:  cell=[1,2,3] providers=[[a],[b,c],[d]]
	// right: cell=[2,3,4] providers=[[X],[Y,Z],[W]]
	// Full outer: rows for cells {1,2,3,4}; unmatched sides emit null lists.
	left := listJoinFrame(t,
		[]int64{1, 2, 3},
		[][]string{{"a"}, {"b", "c"}, {"d"}},
		nil,
	)
	right := listJoinFrame(t,
		[]int64{2, 3, 4},
		[][]string{{"X"}, {"Y", "Z"}, {"W"}},
		nil,
	)
	right, err := right.Rename("providers", "providers_r")
	if err != nil {
		t.Fatal(err)
	}
	out, err := left.Join(right, "cell", "cell", JoinFull)
	if err != nil {
		t.Fatalf("Full join with List<String>: %v", err)
	}
	if out.NumRows() != 4 {
		t.Fatalf("row count = %d, want 4", out.NumRows())
	}
	// Sorted by cell after Full join emits matched then unmatched — but
	// gobi's Join doesn't specifically sort. Locate each cell.
	cellCol, _ := out.Column("cell")
	cellArr := cellCol.col.Data().Chunks()[0].(*array.Int64)
	rowByCell := map[int64]int{}
	for i := 0; i < out.NumRows(); i++ {
		rowByCell[cellArr.Value(i)] = i
	}
	for _, want := range []int64{1, 2, 3, 4} {
		if _, ok := rowByCell[want]; !ok {
			t.Fatalf("cell %d missing from full-join output", want)
		}
	}

	lps, _ := out.Column("providers")
	rps, _ := out.Column("providers_r")

	// cell=1 only on left → right list should be null.
	i1 := rowByCell[1]
	if got, isNull := getListRow(t, lps, i1); isNull || !stringSliceEqual(got, []string{"a"}) {
		t.Fatalf("cell=1 left = %v (null=%v), want [a]", got, isNull)
	}
	if _, isNull := getListRow(t, rps, i1); !isNull {
		t.Fatalf("cell=1 right should be null (no right match)")
	}

	// cell=4 only on right → left list should be null.
	i4 := rowByCell[4]
	if _, isNull := getListRow(t, lps, i4); !isNull {
		t.Fatalf("cell=4 left should be null (no left match)")
	}
	if got, isNull := getListRow(t, rps, i4); isNull || !stringSliceEqual(got, []string{"W"}) {
		t.Fatalf("cell=4 right = %v (null=%v), want [W]", got, isNull)
	}

	// cell=2 matched: left=[b,c], right=[X]
	i2 := rowByCell[2]
	if got, _ := getListRow(t, lps, i2); !stringSliceEqual(got, []string{"b", "c"}) {
		t.Fatalf("cell=2 left = %v, want [b c]", got)
	}
	if got, _ := getListRow(t, rps, i2); !stringSliceEqual(got, []string{"X"}) {
		t.Fatalf("cell=2 right = %v, want [X]", got)
	}
}

// The end-to-end use case that motivated this fix:
//
//	Full-outer-join two branches, coalesce nulls to empty lists, union.
func TestJoin_FullThenCoalesceThenListUnion(t *testing.T) {
	left := listJoinFrame(t,
		[]int64{1, 2, 3},
		[][]string{{"a"}, {"b", "c"}, {"d"}},
		nil,
	)
	right := listJoinFrame(t,
		[]int64{2, 3, 4},
		[][]string{{"X"}, {"Y", "Z"}, {"W"}},
		nil,
	)
	right, err := right.Rename("providers", "providers_r")
	if err != nil {
		t.Fatal(err)
	}
	joined, err := left.Join(right, "cell", "cell", JoinFull)
	if err != nil {
		t.Fatal(err)
	}

	// Coalesce both sides to empty list, then ListUnion.
	empty := LitEmptyList(arrow.BinaryTypes.String)
	withSeg, err := joined.WithColumnExpr("l_safe", Coalesce(Col("providers"), empty))
	if err != nil {
		t.Fatal(err)
	}
	withBoth, err := withSeg.WithColumnExpr("r_safe", Coalesce(Col("providers_r"), empty))
	if err != nil {
		t.Fatal(err)
	}
	final, err := withBoth.WithColumnExpr("merged",
		Col("l_safe").ListUnion(Col("r_safe")))
	if err != nil {
		t.Fatalf("ListUnion after Coalesce on Full join: %v", err)
	}
	merged, _ := final.Column("merged")
	// Locate each cell.
	cellArr := final.series[0].col.Data().Chunks()[0].(*array.Int64)
	rowByCell := map[int64]int{}
	for i := 0; i < final.NumRows(); i++ {
		rowByCell[cellArr.Value(i)] = i
	}
	// Expected merged sets:
	//   cell=1: [a] ∪ [] = [a]
	//   cell=2: [b,c] ∪ [X] = [b, c, X]
	//   cell=3: [d] ∪ [Y,Z] = [d, Y, Z]
	//   cell=4: [] ∪ [W] = [W]
	want := map[int64][]string{
		1: {"a"},
		2: {"b", "c", "X"},
		3: {"d", "Y", "Z"},
		4: {"W"},
	}
	for cell, exp := range want {
		row := rowByCell[cell]
		got, isNull := getListRow(t, merged, row)
		if isNull {
			t.Fatalf("cell %d merged is null; want %v", cell, exp)
		}
		if !stringSliceEqual(got, exp) {
			t.Fatalf("cell %d merged = %v, want %v", cell, got, exp)
		}
	}
}

// Regression: pre-fix null-list rows on the source were also carried
// through correctly (not just null on the "no match" side).
func TestJoin_InnerCarriesNullListRow(t *testing.T) {
	// left row 1 has an explicit null list.
	left := listJoinFrame(t,
		[]int64{1, 2, 3},
		[][]string{{"a"}, {}, {"d"}}, // row 1's slice is ignored (nullListRows says null)
		map[int]bool{1: true},
	)
	right := listJoinFrame(t,
		[]int64{2, 3},
		[][]string{{"X"}, {"Y"}},
		nil,
	)
	right, err := right.Rename("providers", "providers_r")
	if err != nil {
		t.Fatal(err)
	}
	out, err := left.Join(right, "cell", "cell", JoinInner)
	if err != nil {
		t.Fatal(err)
	}
	if out.NumRows() != 2 {
		t.Fatalf("row count = %d, want 2", out.NumRows())
	}
	cellCol, _ := out.Column("cell")
	cellArr := cellCol.col.Data().Chunks()[0].(*array.Int64)
	rowByCell := map[int64]int{}
	for i := 0; i < out.NumRows(); i++ {
		rowByCell[cellArr.Value(i)] = i
	}
	lps, _ := out.Column("providers")
	// cell=2 was the null-list row on the left.
	if _, isNull := getListRow(t, lps, rowByCell[2]); !isNull {
		t.Fatalf("cell=2 left providers should be null (was null pre-join)")
	}
}

func stringSliceEqual(a, b []string) bool {
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
