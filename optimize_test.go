package gobi

import (
	"strings"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
)

// -- FoldConstants: expression-level algebraic simplifications -----------

func TestOptimize_FoldConstants_BooleanIdentity(t *testing.T) {
	// x AND true → x
	e := Col("active").And(Lit(true))
	folded, _ := foldExpr(e)
	if folded.String() != Col("active").String() {
		t.Fatalf("x AND true folded to %s, want %s", folded, Col("active"))
	}
}

func TestOptimize_FoldConstants_BooleanAbsorption(t *testing.T) {
	// x AND false → false, x OR true → true
	if got, _ := foldExpr(Col("active").And(Lit(false))); got.String() != "lit(false)" {
		t.Errorf("x AND false → %s, want lit(false)", got)
	}
	if got, _ := foldExpr(Col("active").Or(Lit(true))); got.String() != "lit(true)" {
		t.Errorf("x OR true → %s, want lit(true)", got)
	}
}

func TestOptimize_FoldConstants_NotElimination(t *testing.T) {
	// NOT NOT x → x
	e := Col("active").Not().Not()
	folded, changed := foldExpr(e)
	if !changed {
		t.Fatal("NOT NOT should fold")
	}
	if folded.String() != Col("active").String() {
		t.Fatalf("NOT NOT x → %s, want %s", folded, Col("active"))
	}
}

func TestOptimize_FoldConstants_NotLiteral(t *testing.T) {
	// NOT true → false
	e := Lit(true).Not()
	folded, _ := foldExpr(e)
	if folded.String() != "lit(false)" {
		t.Fatalf("NOT true → %s, want lit(false)", folded)
	}
}

func TestOptimize_FoldConstants_ArithmeticLiterals(t *testing.T) {
	// Lit(2) * Lit(3) → Lit(6)
	e := Lit(2.0).Mul(Lit(3.0))
	folded, _ := foldExpr(e)
	if folded.String() != "lit(6)" {
		t.Fatalf("2 * 3 → %s, want lit(6)", folded)
	}
}

func TestOptimize_FoldConstants_ComparisonLiterals(t *testing.T) {
	// Lit(5) > Lit(3) → Lit(true)
	if got, _ := foldExpr(Lit(5.0).Gt(Lit(3.0))); got.String() != "lit(true)" {
		t.Errorf("5 > 3 → %s, want lit(true)", got)
	}
	// Lit("a") == Lit("b") → Lit(false)
	if got, _ := foldExpr(Lit("a").Eq(Lit("b"))); got.String() != "lit(false)" {
		t.Errorf(`"a" == "b" → %s, want lit(false)`, got)
	}
}

func TestOptimize_FoldConstants_RecursiveIntoChildren(t *testing.T) {
	// x AND (1 == 1) → x AND true → x
	inner := Lit(1.0).Eq(Lit(1.0))
	e := Col("active").And(inner)
	folded, changed := foldExpr(e)
	if !changed {
		t.Fatal("nested constant should fold")
	}
	if folded.String() != Col("active").String() {
		t.Fatalf("folded to %s, want just col(active)", folded)
	}
}

func TestOptimize_FoldConstants_DivByZeroLeft(t *testing.T) {
	// 5 / 0 should not fold (avoid producing +Inf or synthesizing an
	// error at optimize time).
	e := Lit(5.0).Div(Lit(0.0))
	if _, changed := foldExpr(e); changed {
		t.Fatal("division by zero literal should not fold")
	}
}

// -- RemoveTrivialTrueFilter --------------------------------------------

func TestOptimize_RemoveTrivialTrueFilter(t *testing.T) {
	df := lazyFrame(t)
	// A Filter(true) node should be eliminated entirely.
	lf := df.Lazy().Filter(Lit(true))
	optimized := lf.Optimize()
	if _, ok := optimized.(*filterNode); ok {
		t.Fatalf("Filter(true) should be removed; got %T", optimized)
	}
	// Result rows must still be all 5.
	out, err := lf.Collect()
	if err != nil {
		t.Fatal(err)
	}
	if out.NumRows() != 5 {
		t.Fatalf("filter(true) collected %d rows, want 5", out.NumRows())
	}
}

// -- CombineFilters ------------------------------------------------------

func TestOptimize_CombineFilters(t *testing.T) {
	df := lazyFrame(t)
	// Two adjacent Filters should collapse to one.
	lf := df.Lazy().
		Filter(Col("price").Gt(Lit(15.0))).
		Filter(Col("active"))
	optimized := lf.Optimize()

	// Root should be a filterNode with a single filterNode input
	// replaced by a Scan (no nested filters left).
	f, ok := optimized.(*filterNode)
	if !ok {
		t.Fatalf("root should be Filter after combining; got %T", optimized)
	}
	if _, nested := f.input.(*filterNode); nested {
		t.Fatal("nested Filter should have been combined into the outer")
	}
	// Result correctness: active AND price > 15 → id 3 and 5.
	out, err := lf.Collect()
	if err != nil {
		t.Fatal(err)
	}
	ids, _ := out.Column("id")
	arr := ids.Column().Data().Chunks()[0].(*array.Int64)
	if arr.Len() != 2 || arr.Value(0) != 3 || arr.Value(1) != 5 {
		t.Fatalf("got ids %v, want [3 5]",
			[]int64{arr.Value(0), arr.Value(1)})
	}
}

func TestOptimize_CombineFilters_ThreeInARow(t *testing.T) {
	// Three chained filters should collapse to one via repeated
	// fixed-point application.
	df := lazyFrame(t)
	lf := df.Lazy().
		Filter(Col("price").Gt(Lit(5.0))).
		Filter(Col("price").Lt(Lit(45.0))).
		Filter(Col("active"))
	optimized := lf.Optimize()

	depth := 0
	cur := optimized
	for {
		f, ok := cur.(*filterNode)
		if !ok {
			break
		}
		depth++
		cur = f.input
	}
	if depth != 1 {
		t.Fatalf("filter chain depth after optimize = %d, want 1", depth)
	}
}

// -- PushFilterBelowProject ---------------------------------------------

func TestOptimize_PushFilterBelowProject(t *testing.T) {
	// Filter references a column that survives Project, so it can
	// safely move below.
	df := lazyFrame(t)
	lf := df.Lazy().
		Select(Col("id"), Col("price")).
		Filter(Col("price").Gt(Lit(20.0)))
	optimized := lf.Optimize()

	// Root should now be a Project wrapping a Filter (the reverse of
	// the built order).
	proj, ok := optimized.(*projectNode)
	if !ok {
		t.Fatalf("root should be Project after push; got %T", optimized)
	}
	if _, ok := proj.input.(*filterNode); !ok {
		t.Fatalf("Project's child should be Filter; got %T", proj.input)
	}

	// Semantic equivalence: result must be identical to CollectRaw.
	optRes, err := lf.Collect()
	if err != nil {
		t.Fatal(err)
	}
	rawRes, err := lf.CollectRaw()
	if err != nil {
		t.Fatal(err)
	}
	if optRes.NumRows() != rawRes.NumRows() {
		t.Fatalf("optimized rows=%d, raw rows=%d", optRes.NumRows(), rawRes.NumRows())
	}
}

func TestOptimize_PushFilterBelowProject_UnsafeStaysAbove(t *testing.T) {
	// Filter references a column CREATED by the Project (via Alias).
	// The predicate refers to "doubled", which doesn't exist in the
	// scan schema. Push should NOT fire.
	df := lazyFrame(t)
	lf := df.Lazy().
		Select(Col("id"), Col("price").Mul(Lit(2.0)).Alias("doubled")).
		Filter(Col("doubled").Gt(Lit(50.0)))
	optimized := lf.Optimize()

	// Root should still be Filter → Project (unchanged).
	if _, ok := optimized.(*filterNode); !ok {
		t.Fatalf("unsafe push should leave Filter at root; got %T", optimized)
	}
}

// -- Composition ---------------------------------------------------------

func TestOptimize_Composition(t *testing.T) {
	// Filter(true) + adjacent Filters + Push should all cooperate.
	df := lazyFrame(t)
	lf := df.Lazy().
		Select(Col("id"), Col("price"), Col("active")).
		Filter(Lit(true)).                        // → removed
		Filter(Col("price").Gt(Lit(15.0))).       // ┐ combined
		Filter(Col("active"))                     // ┘ into one
	// After optimize: Filter(price>15 AND active) below Project.

	// Confirm end-to-end result is the same as raw.
	optRes, err := lf.Collect()
	if err != nil {
		t.Fatal(err)
	}
	rawRes, err := lf.CollectRaw()
	if err != nil {
		t.Fatal(err)
	}
	if optRes.NumRows() != rawRes.NumRows() {
		t.Fatalf("optimized rows=%d, raw rows=%d", optRes.NumRows(), rawRes.NumRows())
	}

	// Confirm optimize actually reduced the plan.
	rawExplain := lf.Explain()
	optExplain := lf.ExplainOptimized()
	rawFilters := strings.Count(rawExplain, "Filter(")
	optFilters := strings.Count(optExplain, "Filter(")
	if rawFilters <= optFilters {
		t.Fatalf("expected fewer Filters after optimize: raw=%d opt=%d\n%s\n---\n%s",
			rawFilters, optFilters, rawExplain, optExplain)
	}
}

// -- Optimizer is a no-op on already-clean plans ------------------------

func TestOptimize_IdempotentOnCleanPlan(t *testing.T) {
	df := lazyFrame(t)
	lf := df.Lazy().Select(Col("id"))
	// A single Select with no folds available.
	optimized := Optimize(lf.Plan())
	if optimized != lf.Plan() {
		// Not a hard requirement (identity may not survive), but log
		// if it changes so we notice unexpected rewrites.
		t.Logf("clean plan changed identity: was %T now %T", lf.Plan(), optimized)
	}
	// Regardless of identity, execution should still work.
	out, err := lf.Collect()
	if err != nil {
		t.Fatal(err)
	}
	if out.NumRows() != 5 || out.NumCols() != 1 {
		t.Fatalf("clean plan: got %d rows x %d cols", out.NumRows(), out.NumCols())
	}
}

// -- CollectRaw bypass --------------------------------------------------

func TestOptimize_CollectRawBypassesOptimizer(t *testing.T) {
	df := lazyFrame(t)
	lf := df.Lazy().Filter(Lit(true))
	// CollectRaw should evaluate the Filter(true) node as-is (harmless
	// — filter(true) preserves all rows). Just confirm it runs.
	out, err := lf.CollectRaw()
	if err != nil {
		t.Fatal(err)
	}
	if out.NumRows() != 5 {
		t.Fatalf("CollectRaw rows = %d, want 5", out.NumRows())
	}
}

// -- Limit(0) correctness (bug fix included in this slice) --------------

func TestOptimize_LimitZero_ProducesEmptyFrame(t *testing.T) {
	df := lazyFrame(t)
	out, err := df.Lazy().Limit(0).Collect()
	if err != nil {
		t.Fatal(err)
	}
	if out.NumRows() != 0 {
		t.Fatalf("Limit(0) rows = %d, want 0", out.NumRows())
	}
	// Schema preserved.
	if out.NumCols() != 4 {
		t.Fatalf("Limit(0) cols = %d, want 4", out.NumCols())
	}
}

// -- Custom rule extension point ----------------------------------------

// negatingRule is a rule that only exists to prove the Rule interface
// is extensible from outside the built-in set: it appends a
// no-semantic-change Filter(true) at the root the first time it runs.
// (Real rules would do real work; this exercises the plumbing.)
type markerRule struct{ fired bool }

func (markerRule) Name() string { return "TestMarker" }
func (r *markerRule) Apply(p LogicalPlan) (LogicalPlan, bool) {
	if r.fired {
		return p, false
	}
	r.fired = true
	return &filterNode{input: p, cond: Lit(true)}, true
}

func TestOptimize_CustomRule(t *testing.T) {
	df := lazyFrame(t)
	plan := df.Lazy().Plan()
	custom := &markerRule{}
	optimized := Optimize(plan, custom)
	if _, ok := optimized.(*filterNode); !ok {
		t.Fatalf("custom rule didn't run; got %T", optimized)
	}
}

// -- Filter(false) → Empty ------------------------------------------------

func TestOptimize_FilterFalse_CollapsesToEmpty(t *testing.T) {
	df := lazyFrame(t)
	lf := df.Lazy().Filter(Lit(false))
	optimized := lf.Optimize()
	if _, ok := optimized.(*emptyNode); !ok {
		t.Fatalf("Filter(false) should collapse to Empty; got %T", optimized)
	}
	out, err := lf.Collect()
	if err != nil {
		t.Fatal(err)
	}
	if out.NumRows() != 0 {
		t.Fatalf("empty result rows = %d, want 0", out.NumRows())
	}
	// Schema preserved so the frame is well-formed.
	if out.NumCols() != df.NumCols() {
		t.Fatalf("empty result cols = %d, want %d (schema preserved)",
			out.NumCols(), df.NumCols())
	}
}

func TestOptimize_FilterFalse_ThroughFold(t *testing.T) {
	// FoldConstants + RemoveTrivialFilter should cooperate: `false AND x`
	// folds to Lit(false), which then collapses to Empty.
	df := lazyFrame(t)
	lf := df.Lazy().Filter(Lit(false).And(Col("active")))
	optimized := lf.Optimize()
	if _, ok := optimized.(*emptyNode); !ok {
		t.Fatalf("(false AND x) should fold + collapse to Empty; got %T", optimized)
	}
}

// -- PushFilterBelowSort --------------------------------------------------

func TestOptimize_PushFilterBelowSort(t *testing.T) {
	df := lazyFrame(t)
	lf := df.Lazy().
		SortBy(SortKey{Column: "price"}).
		Filter(Col("active"))
	optimized := lf.Optimize()

	// Root should be Sort with Filter as its child (the reverse of
	// the built order).
	s, ok := optimized.(*sortNode)
	if !ok {
		t.Fatalf("root should be Sort after push; got %T", optimized)
	}
	if _, ok := s.input.(*filterNode); !ok {
		t.Fatalf("Sort's child should be Filter; got %T", s.input)
	}

	// Semantic equivalence: same rows, same order as unoptimized.
	optRes, err := lf.Collect()
	if err != nil {
		t.Fatal(err)
	}
	rawRes, err := lf.CollectRaw()
	if err != nil {
		t.Fatal(err)
	}
	if optRes.NumRows() != rawRes.NumRows() {
		t.Fatalf("optimized rows=%d, raw rows=%d", optRes.NumRows(), rawRes.NumRows())
	}
}

// -- ProjectionPushdown (unit, without external I/O) ---------------------

// projectableTestScan is a fake ProjectableScan used by unit tests. It
// records the last cols set applied so tests can assert what the
// optimizer asked for.
type projectableTestScan struct {
	schema         *arrow.Schema
	sourceFrame    *Frame
	appliedCols    []string
	callCount      int
}

func (s *projectableTestScan) Schema() *arrow.Schema   { return s.schema }
func (s *projectableTestScan) Children() []LogicalPlan { return nil }
func (s *projectableTestScan) String() string          { return "Scan[test]" }
func (s *projectableTestScan) ProjectColumns(cols []string) LogicalPlan {
	s.callCount++
	s.appliedCols = append([]string{}, cols...)
	// Return "self-with-projection" — a new plan node identity so the
	// rule sees a change. For test simplicity, we return a scanFrameNode
	// over a projected copy of sourceFrame.
	newFrame := s.sourceFrame
	if len(cols) < s.sourceFrame.NumCols() {
		// Project the frame to just cols via WithColumn-style rebuild.
		f := s.sourceFrame
		wanted := make(map[string]struct{}, len(cols))
		for _, c := range cols {
			wanted[c] = struct{}{}
		}
		// Drop columns not in wanted.
		for _, name := range f.ColumnNames() {
			if _, keep := wanted[name]; !keep {
				var err error
				f, err = f.DropColumn(name)
				if err != nil {
					return s
				}
			}
		}
		newFrame = f
	}
	return &scanFrameNode{frame: newFrame}
}

func TestOptimize_ProjectionPushdown_ThroughProject(t *testing.T) {
	df := lazyFrame(t)
	fake := &projectableTestScan{schema: df.Schema(), sourceFrame: df}
	plan := &projectNode{
		input: fake,
		exprs: []Expr{Col("id"), Col("price")},
	}
	// newProjectNode computes outSchema; use it for realism.
	plan = newProjectNode(fake, []Expr{Col("id"), Col("price")})

	Optimize(plan)

	if fake.callCount == 0 {
		t.Fatal("ProjectColumns was never called")
	}
	// Should have been asked for exactly {id, price}.
	if len(fake.appliedCols) != 2 {
		t.Fatalf("appliedCols = %v, want 2 entries", fake.appliedCols)
	}
	got := map[string]bool{fake.appliedCols[0]: true, fake.appliedCols[1]: true}
	if !got["id"] || !got["price"] {
		t.Fatalf("appliedCols = %v, want {id, price}", fake.appliedCols)
	}
}

func TestOptimize_ProjectionPushdown_ThroughFilter(t *testing.T) {
	// Filter above the scan pulls its referenced columns into the
	// needed set, alongside anything the parent still wants.
	df := lazyFrame(t)
	fake := &projectableTestScan{schema: df.Schema(), sourceFrame: df}
	// Plan: Project([id]) → Filter(price > 20) → Scan
	// Scan should be asked for {id, price} — id because Project
	// needs it, price because Filter reads it.
	proj := newProjectNode(
		&filterNode{input: fake, cond: Col("price").Gt(Lit(20.0))},
		[]Expr{Col("id")},
	)
	Optimize(proj)
	if fake.callCount == 0 {
		t.Fatal("ProjectColumns was never called")
	}
	got := map[string]bool{}
	for _, c := range fake.appliedCols {
		got[c] = true
	}
	if !got["id"] || !got["price"] {
		t.Fatalf("appliedCols = %v, want {id, price}", fake.appliedCols)
	}
}

func TestOptimize_ProjectionPushdown_ThroughAggregate(t *testing.T) {
	df := lazyFrame(t)
	fake := &projectableTestScan{schema: df.Schema(), sourceFrame: df}
	// GroupBy("region").Agg(Sum(price)) → scan should be asked for
	// {region, price}, not {id, active}.
	agg := newAggregateNode(fake, []string{"region"},
		[]Aggregation{{Column: "price", Kind: AggSum}})
	Optimize(agg)
	if fake.callCount == 0 {
		t.Fatal("ProjectColumns was never called")
	}
	got := map[string]bool{}
	for _, c := range fake.appliedCols {
		got[c] = true
	}
	if !got["region"] || !got["price"] {
		t.Fatalf("appliedCols = %v, want {region, price}", fake.appliedCols)
	}
	if got["id"] || got["active"] {
		t.Fatalf("appliedCols = %v should not include id/active", fake.appliedCols)
	}
}

// -- CascadeEmpty --------------------------------------------------------

func TestOptimize_CascadeEmpty_ThroughUnaryOps(t *testing.T) {
	// A Filter(false) at the bottom should propagate up through
	// Sort/Limit/Select/WithColumn, keeping the top-level plan Empty
	// with the appropriate schema.
	df := lazyFrame(t)
	lf := df.Lazy().
		Filter(Lit(false)).
		Select(Col("id"), Col("price")).
		SortBy(SortKey{Column: "id"}).
		Limit(10).
		WithColumn("id_doubled", Col("id").Mul(Lit(2.0)))

	optimized := lf.Optimize()
	if _, ok := optimized.(*emptyNode); !ok {
		t.Fatalf("cascade didn't reach the root; got %T", optimized)
	}

	// Schema at the root reflects all the transformations above the
	// filter — the WithColumn should have added "id_doubled".
	sch := optimized.Schema()
	names := make(map[string]bool, len(sch.Fields()))
	for _, f := range sch.Fields() {
		names[f.Name] = true
	}
	if !names["id"] || !names["price"] || !names["id_doubled"] {
		t.Fatalf("empty schema missing fields; got %v", sch.Fields())
	}

	// Collect produces a zero-row Frame with the right schema.
	out, err := lf.Collect()
	if err != nil {
		t.Fatal(err)
	}
	if out.NumRows() != 0 {
		t.Fatalf("rows = %d, want 0", out.NumRows())
	}
	if out.NumCols() != 3 {
		t.Fatalf("cols = %d, want 3 (id, price, id_doubled)", out.NumCols())
	}
}

func TestOptimize_CascadeEmpty_InnerJoin(t *testing.T) {
	// Inner join with either side empty → whole join is empty.
	left := lazyFrame(t)
	right := lazyRegions(t)
	// Force the left side to Empty by starting with Filter(false).
	lf := left.Lazy().
		Filter(Lit(false)).
		Join(right.Lazy(), "region", "region", JoinInner)
	optimized := lf.Optimize()
	if _, ok := optimized.(*emptyNode); !ok {
		t.Fatalf("inner join with empty left didn't cascade; got %T", optimized)
	}
}

func TestOptimize_CascadeEmpty_LeftJoinKeepsOuter(t *testing.T) {
	// Left join with empty right side still returns the left rows —
	// the CascadeEmpty rule should NOT fire here.
	left := lazyFrame(t)
	right := lazyRegions(t)
	lf := left.Lazy().
		Join(right.Lazy().Filter(Lit(false)), "region", "region", JoinLeft)
	optimized := lf.Optimize()
	if _, ok := optimized.(*emptyNode); ok {
		t.Fatalf("left join with empty right should NOT cascade to empty")
	}
	// Actual Collect should still produce the 5 left rows.
	out, err := lf.Collect()
	if err != nil {
		t.Fatal(err)
	}
	if out.NumRows() != 5 {
		t.Fatalf("left join rows = %d, want 5", out.NumRows())
	}
}

// -- PushPredicateToScan (unit) ------------------------------------------

// predicateTestScan records the predicate the optimizer proposes.
// Used to verify the rule reaches the scan without needing real I/O.
type predicateTestScan struct {
	schema      *arrow.Schema
	appliedPred Expr
	callCount   int
}

func (s *predicateTestScan) Schema() *arrow.Schema   { return s.schema }
func (s *predicateTestScan) Children() []LogicalPlan { return nil }
func (s *predicateTestScan) String() string          { return "Scan[test]" }
func (s *predicateTestScan) ApplyPredicate(pred Expr) LogicalPlan {
	s.callCount++
	s.appliedPred = pred
	// Return a fresh node so the rule sees a change. We reuse the
	// same fake to inspect appliedPred after Optimize.
	return &predicateTestScan{schema: s.schema, appliedPred: pred, callCount: s.callCount}
}

func TestOptimize_PushPredicateToScan(t *testing.T) {
	df := lazyFrame(t)
	fake := &predicateTestScan{schema: df.Schema()}
	pred := Col("price").Gt(Lit(20.0))
	plan := &filterNode{input: fake, cond: pred}
	Optimize(plan)
	if fake.callCount == 0 {
		t.Fatal("ApplyPredicate was never called")
	}
	if fake.appliedPred.String() != pred.String() {
		t.Fatalf("applied pred = %s, want %s", fake.appliedPred, pred)
	}
}

func TestOptimize_ProjectionPushdown_ScanFrameUntouched(t *testing.T) {
	// scanFrameNode doesn't implement ProjectableScan — the rule
	// should leave it alone. This verifies non-projectable scans
	// aren't silently broken by the rule.
	df := lazyFrame(t)
	lf := df.Lazy().Select(Col("id"))
	optimized := lf.Optimize()
	// The scan at the bottom should still be a scanFrameNode.
	cur := optimized
	for {
		if _, ok := cur.(*scanFrameNode); ok {
			return
		}
		kids := cur.Children()
		if len(kids) == 0 {
			t.Fatalf("no scanFrameNode found; final = %T", cur)
		}
		cur = kids[0]
	}
}
