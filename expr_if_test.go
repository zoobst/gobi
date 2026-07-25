package gobi

import (
	"strings"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
)

// Reuses nullyFrame from expr_null_test.go:
//   id  price       tag
//   1   10.0        "a"
//   2   null        null
//   3   30.0        null
//   4   null        "d"
//   5   50.0        "e"

func TestIf_BasicNumericSelect(t *testing.T) {
	f := nullyFrame(t)
	// Mean-fill: If price is null, use 100.0, else keep price.
	out, err := f.WithColumnExpr("filled_price",
		If(Col("price").IsNull(), Lit(100.0), Col("price")))
	if err != nil {
		t.Fatal(err)
	}
	filled, _ := out.Column("filled_price")
	if filled.DataType().ID() != arrow.FLOAT64 {
		t.Fatalf("filled_price type = %s, want FLOAT64", filled.DataType())
	}
	arr := filled.col.Data().Chunks()[0].(*array.Float64)
	want := []float64{10, 100, 30, 100, 50}
	for i, w := range want {
		if arr.IsNull(i) {
			t.Fatalf("row %d unexpectedly null", i)
		}
		if arr.Value(i) != w {
			t.Fatalf("row %d = %v, want %v", i, arr.Value(i), w)
		}
	}
}

func TestIf_StringBranches(t *testing.T) {
	f := nullyFrame(t)
	// Categorize: "known" if tag set, "unknown" otherwise.
	out, err := f.WithColumnExpr("category",
		If(Col("tag").IsNotNull(), Lit("known"), Lit("unknown")))
	if err != nil {
		t.Fatal(err)
	}
	cat, _ := out.Column("category")
	arr := cat.col.Data().Chunks()[0].(*array.String)
	want := []string{"known", "unknown", "unknown", "known", "known"}
	for i, w := range want {
		if arr.Value(i) != w {
			t.Fatalf("row %d = %q, want %q", i, arr.Value(i), w)
		}
	}
}

// SQL semantics: null cond → null output, even when both branches are
// non-null. Tests that we don't accidentally coerce null to false.
func TestIf_NullCondProducesNullOutput(t *testing.T) {
	f := nullyFrame(t)
	// Cond: price > 20. price is null in rows 1 and 3 → cond null → output null.
	out, err := f.WithColumnExpr("gt20",
		If(Col("price").Gt(Lit(20.0)), Lit("yes"), Lit("no")))
	if err != nil {
		t.Fatal(err)
	}
	arr := out.series[3].col.Data().Chunks()[0].(*array.String)
	// price = [10, null, 30, null, 50]
	// cond  = [false, null, true, null, true]
	// out   = ["no", null, "yes", null, "yes"]
	wantNull := map[int]bool{0: false, 1: true, 2: false, 3: true, 4: false}
	wantVal := map[int]string{0: "no", 2: "yes", 4: "yes"}
	for i := 0; i < out.NumRows(); i++ {
		if wantNull[i] {
			if !arr.IsNull(i) {
				t.Fatalf("row %d expected null, got %q", i, arr.Value(i))
			}
			continue
		}
		if arr.IsNull(i) {
			t.Fatalf("row %d expected %q, got null", i, wantVal[i])
		}
		if arr.Value(i) != wantVal[i] {
			t.Fatalf("row %d = %q, want %q", i, arr.Value(i), wantVal[i])
		}
	}
}

// Null in the selected branch propagates. Cond=false where otherwise
// is null should emit null.
func TestIf_NullInSelectedBranchPropagates(t *testing.T) {
	f := nullyFrame(t)
	// If tag IsNotNull -> tag, else -> null (via Col("tag") itself in the else too).
	// Actually a cleaner example: If price > 0 use tag (which has nulls),
	// else use "fallback". Rows with tag=null AND price > 0 emit null.
	out, err := f.WithColumnExpr("out",
		If(Col("price").Gt(Lit(0.0)), Col("tag"), Lit("fallback")))
	if err != nil {
		t.Fatal(err)
	}
	arr := out.series[3].col.Data().Chunks()[0].(*array.String)
	// price > 0 → cond = [true, null, true, null, true]
	// then=Col(tag)          = ["a", null, null, "d", "e"]
	// otherwise=Lit("fallback")
	// row 0: cond true → tag[0]="a"
	// row 1: cond null → null (SQL rule)
	// row 2: cond true → tag[2]=null → null
	// row 3: cond null → null (SQL rule)
	// row 4: cond true → tag[4]="e"
	if arr.Value(0) != "a" {
		t.Fatalf("row 0 = %q, want a", arr.Value(0))
	}
	if !arr.IsNull(1) || !arr.IsNull(2) || !arr.IsNull(3) {
		t.Fatalf("rows 1,2,3 should be null; got %q %q %q", arr.Value(1), arr.Value(2), arr.Value(3))
	}
	if arr.Value(4) != "e" {
		t.Fatalf("row 4 = %q, want e", arr.Value(4))
	}
}

// Nested else-if — chained If for a three-way branch.
func TestIf_NestedElseIf(t *testing.T) {
	f := nullyFrame(t)
	// Bucketize by price: <20 → "low", <40 → "mid", else "high".
	// Skip null-price rows via IsNotNull().And(...).
	out, err := f.WithColumnExpr("bucket",
		If(Col("price").IsNull(), Lit("null"),
			If(Col("price").Lt(Lit(20.0)), Lit("low"),
				If(Col("price").Lt(Lit(40.0)), Lit("mid"), Lit("high")))))
	if err != nil {
		t.Fatal(err)
	}
	arr := out.series[3].col.Data().Chunks()[0].(*array.String)
	want := []string{"low", "null", "mid", "null", "high"}
	for i, w := range want {
		if arr.Value(i) != w {
			t.Fatalf("row %d = %q, want %q", i, arr.Value(i), w)
		}
	}
}

func TestIf_CondMustBeBoolean(t *testing.T) {
	f := nullyFrame(t)
	// Non-boolean cond: pass Col("price") directly (Float64).
	_, err := f.WithColumnExpr("bad", If(Col("price"), Lit(1.0), Lit(0.0)))
	if err == nil {
		t.Fatal("expected type error when cond is non-Boolean")
	}
	if !strings.Contains(err.Error(), "Boolean") {
		t.Fatalf("error should mention Boolean requirement; got %v", err)
	}
}

func TestIf_BranchTypeMismatch(t *testing.T) {
	f := nullyFrame(t)
	// Lit(1) is Int64, Lit(1.0) is Float64 — mismatch, must error.
	_, err := f.WithColumnExpr("mixed",
		If(Col("price").Gt(Lit(0.0)), Lit(int64(1)), Lit(1.0)))
	if err == nil {
		t.Fatal("expected type-mismatch error between Int64 and Float64 branches")
	}
	if !strings.Contains(err.Error(), "type mismatch") {
		t.Fatalf("error should mention type mismatch; got %v", err)
	}
}

// End-to-end via Lazy Collect — verifies the ExprNode round-trips
// through Compile + Execute unchanged.
func TestIf_LazyPipeline(t *testing.T) {
	f := nullyFrame(t)
	out, err := f.Lazy().
		WithColumn("filled_price",
			If(Col("price").IsNull(), Lit(0.0), Col("price"))).
		Filter(Col("filled_price").Gt(Lit(0.0))).
		Collect()
	if err != nil {
		t.Fatal(err)
	}
	// price=[10,null,30,null,50] → filled=[10,0,30,0,50] → filter >0 → 3 rows.
	if out.NumRows() != 3 {
		t.Fatalf("row count = %d, want 3", out.NumRows())
	}
}
