package gobi

import (
	"errors"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

func stringFrame(t *testing.T, colName, valuesName string, vals []string, extraName string, extra []int64) *Frame {
	t.Helper()
	pool := memory.DefaultAllocator
	kb := array.NewStringBuilder(pool)
	defer kb.Release()
	kb.AppendValues(vals, nil)
	xb := array.NewInt64Builder(pool)
	defer xb.Release()
	xb.AppendValues(extra, nil)

	fields := []arrow.Field{
		{Name: colName, Type: arrow.BinaryTypes.String, Nullable: false},
		{Name: extraName, Type: arrow.PrimitiveTypes.Int64, Nullable: false},
	}
	schema := arrow.NewSchema(fields, nil)
	arrays := []arrow.Array{kb.NewArray(), xb.NewArray()}
	defer func() {
		for _, a := range arrays {
			a.Release()
		}
	}()
	cols := make([]arrow.Column, 2)
	for i, a := range arrays {
		chunked := arrow.NewChunked(a.DataType(), []arrow.Array{a})
		cols[i] = *arrow.NewColumn(fields[i], chunked)
	}
	f, err := NewFrame(schema, cols)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestJoin_Inner(t *testing.T) {
	left := stringFrame(t, "id", "id",
		[]string{"a", "b", "c", "d"}, "leftv", []int64{1, 2, 3, 4})
	right := stringFrame(t, "id", "id",
		[]string{"b", "c", "e"}, "rightv", []int64{20, 30, 50})

	out, err := left.Join(right, "id", "id", JoinInner)
	if err != nil {
		t.Fatal(err)
	}
	r, c := out.Shape()
	if r != 2 {
		t.Fatalf("inner join rows = %d, want 2", r)
	}
	// left: (id, leftv), right (minus id): (rightv). Total = 3 cols.
	if c != 3 {
		t.Fatalf("cols = %d, want 3", c)
	}

	ids, _ := out.Column("id")
	idsArr := ids.col.Data().Chunks()[0].(*array.String)
	got := []string{idsArr.Value(0), idsArr.Value(1)}
	if got[0] != "b" || got[1] != "c" {
		t.Fatalf("inner-join ids = %v, want [b c]", got)
	}
	rv, _ := out.Column("rightv")
	rvArr := rv.col.Data().Chunks()[0].(*array.Int64)
	if rvArr.Value(0) != 20 || rvArr.Value(1) != 30 {
		t.Fatalf("rightv = %v, %v", rvArr.Value(0), rvArr.Value(1))
	}
}

func TestJoin_Left(t *testing.T) {
	left := stringFrame(t, "id", "id",
		[]string{"a", "b", "c", "d"}, "leftv", []int64{1, 2, 3, 4})
	right := stringFrame(t, "id", "id",
		[]string{"b", "c", "e"}, "rightv", []int64{20, 30, 50})

	out, err := left.Join(right, "id", "id", JoinLeft)
	if err != nil {
		t.Fatal(err)
	}
	r, _ := out.Shape()
	if r != 4 {
		t.Fatalf("left join rows = %d, want 4", r)
	}
	rv, _ := out.Column("rightv")
	rvArr := rv.col.Data().Chunks()[0].(*array.Int64)
	// Order: a (null), b (20), c (30), d (null)
	if !rvArr.IsNull(0) || rvArr.Value(1) != 20 || rvArr.Value(2) != 30 || !rvArr.IsNull(3) {
		t.Fatalf("left-join rightv incorrect")
	}
}

func TestJoin_ColumnNameCollisionRenames(t *testing.T) {
	// Both frames have a column called "value" — collision should rename right side.
	left := stringFrame(t, "id", "id", []string{"a"}, "value", []int64{1})
	right := stringFrame(t, "id", "id", []string{"a"}, "value", []int64{10})
	out, err := left.Join(right, "id", "id", JoinInner)
	if err != nil {
		t.Fatal(err)
	}
	names := out.ColumnNames()
	found := false
	for _, n := range names {
		if n == "value_right" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected value_right column, got %v", names)
	}
}

func TestJoin_KeyTypeMismatch(t *testing.T) {
	left := stringFrame(t, "id", "id", []string{"a"}, "v", []int64{1})
	// Right frame with an int64 "id" key
	pool := memory.DefaultAllocator
	kb := array.NewInt64Builder(pool)
	defer kb.Release()
	kb.AppendValues([]int64{1}, nil)
	vb := array.NewInt64Builder(pool)
	defer vb.Release()
	vb.AppendValues([]int64{2}, nil)
	fields := []arrow.Field{
		{Name: "id", Type: arrow.PrimitiveTypes.Int64, Nullable: false},
		{Name: "rv", Type: arrow.PrimitiveTypes.Int64, Nullable: false},
	}
	schema := arrow.NewSchema(fields, nil)
	arrays := []arrow.Array{kb.NewArray(), vb.NewArray()}
	defer func() {
		for _, a := range arrays {
			a.Release()
		}
	}()
	cols := make([]arrow.Column, 2)
	for i, a := range arrays {
		chunked := arrow.NewChunked(a.DataType(), []arrow.Array{a})
		cols[i] = *arrow.NewColumn(fields[i], chunked)
	}
	right, _ := NewFrame(schema, cols)

	_, err := left.Join(right, "id", "id", JoinInner)
	if !errors.Is(err, ErrColumnTypeMismatch) {
		t.Fatalf("want ErrColumnTypeMismatch, got %v", err)
	}
}

func TestJoin_Right(t *testing.T) {
	left := stringFrame(t, "id", "id",
		[]string{"a", "b", "c"}, "leftv", []int64{1, 2, 3})
	right := stringFrame(t, "id", "id",
		[]string{"b", "c", "e"}, "rightv", []int64{20, 30, 50})

	// Right join: every row from right, with nulls on left where no match.
	// b, c match; e has no left match.
	out, err := left.Join(right, "id", "id", JoinRight)
	if err != nil {
		t.Fatal(err)
	}
	r, c := out.Shape()
	if r != 3 {
		t.Fatalf("right join rows = %d, want 3", r)
	}
	if c != 3 {
		t.Fatalf("cols = %d, want 3 (id, leftv, rightv)", c)
	}

	// Rows arrive in right-frame order: b, c, e.
	ids := mustCol(t, out, "id").col.Data().Chunks()[0].(*array.String)
	if ids.Value(0) != "b" || ids.Value(1) != "c" || ids.Value(2) != "e" {
		t.Fatalf("id order = %v/%v/%v, want b/c/e",
			ids.Value(0), ids.Value(1), ids.Value(2))
	}
	// leftv should be null on the "e" row.
	lv := mustCol(t, out, "leftv").col.Data().Chunks()[0].(*array.Int64)
	if lv.IsNull(0) || lv.IsNull(1) {
		t.Fatalf("leftv null on matched rows")
	}
	if !lv.IsNull(2) {
		t.Fatalf("leftv should be null on unmatched right row")
	}
	rv := mustCol(t, out, "rightv").col.Data().Chunks()[0].(*array.Int64)
	if rv.Value(0) != 20 || rv.Value(1) != 30 || rv.Value(2) != 50 {
		t.Fatalf("rightv values: %v %v %v", rv.Value(0), rv.Value(1), rv.Value(2))
	}
}

func TestJoin_Full(t *testing.T) {
	left := stringFrame(t, "id", "id",
		[]string{"a", "b", "c"}, "leftv", []int64{1, 2, 3})
	right := stringFrame(t, "id", "id",
		[]string{"b", "c", "e"}, "rightv", []int64{20, 30, 50})

	out, err := left.Join(right, "id", "id", JoinFull)
	if err != nil {
		t.Fatal(err)
	}
	// Expected: a (left-only), b (matched), c (matched), e (right-only).
	// 4 rows total.
	r, _ := out.Shape()
	if r != 4 {
		t.Fatalf("full join rows = %d, want 4", r)
	}

	// Left rows first, then unmatched right. The id column is coalesced
	// across the two sides (pandas / SQL COALESCE semantics), so
	// unmatched-right rows still show the right key value rather than
	// a null.
	ids := mustCol(t, out, "id").col.Data().Chunks()[0].(*array.String)
	if ids.Value(0) != "a" {
		t.Fatalf("first id = %q, want a", ids.Value(0))
	}
	if ids.Value(3) != "e" {
		t.Fatalf("row 3 id = %q, want e (coalesced from right)", ids.Value(3))
	}

	lv := mustCol(t, out, "leftv").col.Data().Chunks()[0].(*array.Int64)
	rv := mustCol(t, out, "rightv").col.Data().Chunks()[0].(*array.Int64)

	// Row 0: a → leftv=1, rightv=null
	if lv.Value(0) != 1 || !rv.IsNull(0) {
		t.Fatalf("row 0 leftv/rightv = %v/%v, want 1/null", lv.Value(0), rv.IsNull(0))
	}
	// Row 3: unmatched right e → leftv=null, rightv=50
	if !lv.IsNull(3) || rv.Value(3) != 50 {
		t.Fatalf("row 3 leftv/rightv = %v/%v, want null/50", lv.IsNull(3), rv.Value(3))
	}
}

func TestJoin_Semi(t *testing.T) {
	// Semi: left rows that have at least one match on the right, no
	// duplication on multi-match, no right-side columns.
	left := stringFrame(t, "id", "id",
		[]string{"a", "b", "c", "d"}, "leftv", []int64{1, 2, 3, 4})
	// Right has "b" twice — Semi must not duplicate the left "b".
	right := stringFrame(t, "id", "id",
		[]string{"b", "b", "c", "e"}, "rightv", []int64{20, 21, 30, 50})

	out, err := left.Join(right, "id", "id", JoinSemi)
	if err != nil {
		t.Fatal(err)
	}
	r, c := out.Shape()
	if r != 2 {
		t.Fatalf("semi join rows = %d, want 2 (b, c)", r)
	}
	if c != 2 {
		t.Fatalf("cols = %d, want 2 (only left cols: id, leftv)", c)
	}
	// Confirm no rightv slipped through.
	if _, err := out.Column("rightv"); err == nil {
		t.Fatalf("semi join leaked right-side rightv column")
	}
	ids := mustCol(t, out, "id").col.Data().Chunks()[0].(*array.String)
	if ids.Value(0) != "b" || ids.Value(1) != "c" {
		t.Fatalf("semi ids = %v/%v, want b/c", ids.Value(0), ids.Value(1))
	}
}

func TestJoin_Anti(t *testing.T) {
	// Anti: left rows with no match on right.
	left := stringFrame(t, "id", "id",
		[]string{"a", "b", "c", "d"}, "leftv", []int64{1, 2, 3, 4})
	right := stringFrame(t, "id", "id",
		[]string{"b", "c"}, "rightv", []int64{20, 30})

	out, err := left.Join(right, "id", "id", JoinAnti)
	if err != nil {
		t.Fatal(err)
	}
	r, c := out.Shape()
	if r != 2 {
		t.Fatalf("anti join rows = %d, want 2 (a, d)", r)
	}
	if c != 2 {
		t.Fatalf("cols = %d, want 2 (only left cols)", c)
	}
	ids := mustCol(t, out, "id").col.Data().Chunks()[0].(*array.String)
	if ids.Value(0) != "a" || ids.Value(1) != "d" {
		t.Fatalf("anti ids = %v/%v, want a/d", ids.Value(0), ids.Value(1))
	}
}

// int64Frame builds a two-column Int64 frame (id, val). A `keyValid`
// slice marks which id entries are non-null; nil means all valid.
func int64Frame(t *testing.T, ids []int64, keyValid []bool, vals []int64, idName, valName string) *Frame {
	t.Helper()
	pool := memory.DefaultAllocator
	kb := array.NewInt64Builder(pool)
	defer kb.Release()
	kb.AppendValues(ids, keyValid)
	vb := array.NewInt64Builder(pool)
	defer vb.Release()
	vb.AppendValues(vals, nil)

	fields := []arrow.Field{
		{Name: idName, Type: arrow.PrimitiveTypes.Int64, Nullable: true},
		{Name: valName, Type: arrow.PrimitiveTypes.Int64, Nullable: false},
	}
	schema := arrow.NewSchema(fields, nil)
	arrs := []arrow.Array{kb.NewArray(), vb.NewArray()}
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
	f, err := NewFrame(schema, cols)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestJoin_AntiWithNullLeftKey(t *testing.T) {
	// A left row can lack a match either because its key is null (which
	// never matches anything) or because no right row has the same
	// non-null key. Both should appear in JoinAnti output.
	left := int64Frame(t,
		[]int64{1, 2, 3},
		[]bool{true, false, true}, // row 1 is null
		[]int64{10, 20, 30},
		"id", "leftv")
	right := int64Frame(t,
		[]int64{1}, nil,
		[]int64{100},
		"id", "rightv")

	out, err := left.Join(right, "id", "id", JoinAnti)
	if err != nil {
		t.Fatal(err)
	}
	// Left row 0 matches (id=1). Rows 1 (null key) and 2 (id=3, no match)
	// survive.
	r, _ := out.Shape()
	if r != 2 {
		t.Fatalf("anti rows = %d, want 2", r)
	}
	ids := mustCol(t, out, "id").col.Data().Chunks()[0].(*array.Int64)
	// One of the two output rows should be null (the null-keyed one).
	if !ids.IsNull(0) && !ids.IsNull(1) {
		t.Fatalf("expected one null-keyed row in anti output")
	}
}

func TestJoin_FullWithNullRightKey(t *testing.T) {
	// A null-keyed right row can't match anything, but JoinFull should
	// still emit it (with left-side nulls).
	left := int64Frame(t,
		[]int64{1, 2}, nil,
		[]int64{10, 20},
		"id", "leftv")
	right := int64Frame(t,
		[]int64{1, 99},
		[]bool{true, false}, // second right row is null-keyed
		[]int64{100, 999},
		"id", "rightv")

	out, err := left.Join(right, "id", "id", JoinFull)
	if err != nil {
		t.Fatal(err)
	}
	// Expected: id=1 matches; left id=2 has no match; right null-key
	// row appears as unmatched-right. Total 3 rows.
	r, _ := out.Shape()
	if r != 3 {
		t.Fatalf("full-outer rows = %d, want 3", r)
	}
}

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
