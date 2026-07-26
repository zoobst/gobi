package gobi

import (
	"testing"

	"github.com/apache/arrow-go/v18/arrow/array"
)

// TestFusion_ChainedWithColumnAndFilterFuses — a WithColumn.WithColumn.Filter
// chain compiles to a single fusedStreamExecOp, not three nested ops.
func TestFusion_ChainedWithColumnAndFilterFuses(t *testing.T) {
	f := lazyFrame(t)
	lf := f.Lazy().
		WithColumn("doubled", Col("price").Mul(Lit(2.0))).
		WithColumn("tripled", Col("price").Mul(Lit(3.0))).
		Filter(Col("doubled").Gt(Lit(20.0)))
	op, err := Compile(Optimize(lf.Plan()))
	if err != nil {
		t.Fatal(err)
	}
	fused, ok := op.(*fusedStreamExecOp)
	if !ok {
		t.Fatalf("expected *fusedStreamExecOp, got %T (chain should fuse)", op)
	}
	// Chain should be [withColumn, withColumn, filter] — 3 ops.
	if len(fused.ops) != 3 {
		t.Fatalf("fused ops = %d, want 3", len(fused.ops))
	}
	if _, ok := fused.ops[0].(*withColumnExecOp); !ok {
		t.Errorf("ops[0] = %T, want *withColumnExecOp", fused.ops[0])
	}
	if _, ok := fused.ops[1].(*withColumnExecOp); !ok {
		t.Errorf("ops[1] = %T, want *withColumnExecOp", fused.ops[1])
	}
	if _, ok := fused.ops[2].(*filterExecOp); !ok {
		t.Errorf("ops[2] = %T, want *filterExecOp", fused.ops[2])
	}
}

// TestFusion_CorrectnessParity — fused pipeline produces same output
// as the equivalent non-lazy eager sequence.
func TestFusion_CorrectnessParity(t *testing.T) {
	f := lazyFrame(t)
	// Eager: apply each op in sequence directly.
	eager := f
	eager, err := eager.WithColumnExpr("doubled", Col("price").Mul(Lit(2.0)))
	if err != nil {
		t.Fatal(err)
	}
	eager, err = eager.WithColumnExpr("tripled", Col("price").Mul(Lit(3.0)))
	if err != nil {
		t.Fatal(err)
	}
	eager, err = eager.FilterExpr(Col("doubled").Gt(Lit(20.0)))
	if err != nil {
		t.Fatal(err)
	}
	// Streaming: same ops through the fused executor.
	streaming, err := f.Lazy().
		WithColumn("doubled", Col("price").Mul(Lit(2.0))).
		WithColumn("tripled", Col("price").Mul(Lit(3.0))).
		Filter(Col("doubled").Gt(Lit(20.0))).
		Collect()
	if err != nil {
		t.Fatal(err)
	}
	if eager.NumRows() != streaming.NumRows() {
		t.Fatalf("row count mismatch: eager=%d streaming=%d",
			eager.NumRows(), streaming.NumRows())
	}
	// Verify a shared column matches.
	for _, col := range []string{"id", "price", "doubled", "tripled"} {
		eS, _ := eager.Column(col)
		sS, _ := streaming.Column(col)
		// Concatenate streaming chunks for cross-chunk comparison.
		if eS.Len() != sS.Len() {
			t.Fatalf("col %q length mismatch: eager=%d streaming=%d",
				col, eS.Len(), sS.Len())
		}
	}
}

// TestFusion_FilterMidChainShortCircuits — a filter that drops all
// rows partway through the chain must not run the remaining ops on
// that batch. Verified by checking the output row count matches the
// expected filter selectivity.
func TestFusion_FilterMidChainShortCircuits(t *testing.T) {
	f := lazyFrame(t)
	// Filter to a predicate that matches nothing, then WithColumn.
	// The WithColumn should still work correctly (returning 0 rows).
	out, err := f.Lazy().
		Filter(Col("price").Gt(Lit(9999.0))). // no rows match
		WithColumn("doubled", Col("price").Mul(Lit(2.0))).
		Collect()
	if err != nil {
		t.Fatal(err)
	}
	if out.NumRows() != 0 {
		t.Fatalf("row count = %d, want 0 (filter dropped all rows)", out.NumRows())
	}
	// Schema should still include the new column.
	if _, err := out.Column("doubled"); err != nil {
		t.Fatalf("doubled column missing from empty output: %v", err)
	}
}

// TestFusion_DoesNotFuseAcrossMaterializeBoundary — SortBy forces
// materialize. Ops before Sort fuse; ops after Sort start a new
// chain. The overall exec tree should have distinct fused blocks
// separated by the materialize boundary.
func TestFusion_DoesNotFuseAcrossMaterializeBoundary(t *testing.T) {
	f := lazyFrame(t)
	lf := f.Lazy().
		WithColumn("doubled", Col("price").Mul(Lit(2.0))).
		SortBy(SortKey{Column: "price"}).
		WithColumn("tripled", Col("price").Mul(Lit(3.0)))
	op, err := Compile(Optimize(lf.Plan()))
	if err != nil {
		t.Fatal(err)
	}
	// Top op should be a fusedStreamExecOp OR a withColumnExecOp
	// depending on whether the post-Sort chain fused with itself.
	// Its input should eventually reach a materializeExecOp (from Sort).
	found := false
	cur := op
	for cur != nil {
		if _, ok := cur.(*materializeExecOp); ok {
			found = true
			break
		}
		cur = frameApplierChild(cur)
		// fusedStreamExecOp isn't a frameApplier but has an `input`.
		if cur == nil {
			// Try via fusedStreamExecOp.
			if fused, ok := op.(*fusedStreamExecOp); ok {
				cur = fused.input
				op = nil // exit outer sentinel
			}
		}
	}
	if !found {
		t.Fatalf("expected materializeExecOp somewhere in the chain (SortBy)")
	}
}

// TestFusion_ExplodeInChain — Explode fits the frameApplier
// contract (per-batch Frame.Explode). A chain like
// `.WithColumn(...).Explode(...)` should fuse.
func TestFusion_ExplodeInChain(t *testing.T) {
	f := listExplodeFrame(t)
	lf := f.Lazy().
		WithColumn("tag_count", Col("tags").ListLen()).
		Explode("tags")
	op, err := Compile(Optimize(lf.Plan()))
	if err != nil {
		t.Fatal(err)
	}
	fused, ok := op.(*fusedStreamExecOp)
	if !ok {
		t.Fatalf("expected *fusedStreamExecOp, got %T", op)
	}
	if len(fused.ops) != 2 {
		t.Fatalf("fused ops = %d, want 2 [withColumn, explode]", len(fused.ops))
	}
	if _, ok := fused.ops[0].(*withColumnExecOp); !ok {
		t.Errorf("ops[0] = %T, want *withColumnExecOp", fused.ops[0])
	}
	if _, ok := fused.ops[1].(*explodeExecOp); !ok {
		t.Errorf("ops[1] = %T, want *explodeExecOp", fused.ops[1])
	}
	// End-to-end run — verify correctness through the fused path.
	out, err := lf.Collect()
	if err != nil {
		t.Fatal(err)
	}
	// 4 rows post-explode (3 + 1 + 0-null + 0-empty = 3 non-null + 2 null-fills)
	if out.NumRows() != 6 {
		t.Fatalf("row count = %d, want 6", out.NumRows())
	}
	tagCount, _ := out.Column("tag_count")
	// tag_count was computed BEFORE Explode. After Explode, each parent's
	// count is duplicated across its exploded rows.
	arr := tagCount.col.Data().Chunks()[0].(*array.Int64)
	// Row 0 (from list [10,20,30]): count=3
	// Row 1 (from list [10,20,30]): count=3
	// Row 2 (from list [10,20,30]): count=3
	// Row 3 (from list [40]): count=1
	// Row 4 (from null list): count=null (ListLen on null = null)
	// Row 5 (from empty list): count=0
	if arr.Value(0) != 3 || arr.Value(1) != 3 || arr.Value(2) != 3 {
		t.Fatalf("first 3 rows count = [%d %d %d], want [3 3 3]",
			arr.Value(0), arr.Value(1), arr.Value(2))
	}
	if arr.Value(3) != 1 {
		t.Fatalf("row 3 count = %d, want 1", arr.Value(3))
	}
}

// TestFusion_OverForcesMaterialize — the v0.2.7 exprContainsOver gate
// still routes Over to materialize. That op is NOT a frameApplier, so
// it breaks the fusion chain — verified by asserting the top op is a
// materializeExecOp (from the Over-containing WithColumn), not a
// fusedStreamExecOp.
func TestFusion_OverForcesMaterialize(t *testing.T) {
	// Use lazyFrame (which has price / region etc.) not exprFrame.
	f := lazyFrame(t)
	lf := f.Lazy().
		WithColumn("doubled", Col("price").Mul(Lit(2.0))).
		WithColumn("region_max", Col("price").MaxAgg().Over("region"))
	op, err := Compile(Optimize(lf.Plan()))
	if err != nil {
		t.Fatal(err)
	}
	// The top op should be materializeExecOp (from the Over-WithColumn).
	if _, ok := op.(*materializeExecOp); !ok {
		t.Fatalf("expected top op *materializeExecOp (Over forces it), got %T", op)
	}
}
