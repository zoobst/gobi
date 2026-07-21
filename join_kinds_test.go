package gobi

import (
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

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
