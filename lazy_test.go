package gobi

import (
	"errors"
	"strings"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// lazyFrame builds a small dataset the plan tests use:
//
//	id  price  region  active
//	1   10.0   "US"    true
//	2   20.0   "EU"    false
//	3   30.0   "US"    true
//	4   40.0   "EU"    false
//	5   50.0   "US"    true
func lazyFrame(t *testing.T) *Frame {
	t.Helper()
	pool := memory.DefaultAllocator

	idB := array.NewInt64Builder(pool)
	defer idB.Release()
	idB.AppendValues([]int64{1, 2, 3, 4, 5}, nil)

	priceB := array.NewFloat64Builder(pool)
	defer priceB.Release()
	priceB.AppendValues([]float64{10, 20, 30, 40, 50}, nil)

	regionB := array.NewStringBuilder(pool)
	defer regionB.Release()
	regionB.AppendValues([]string{"US", "EU", "US", "EU", "US"}, nil)

	activeB := array.NewBooleanBuilder(pool)
	defer activeB.Release()
	activeB.AppendValues([]bool{true, false, true, false, true}, nil)

	fields := []arrow.Field{
		{Name: "id", Type: arrow.PrimitiveTypes.Int64, Nullable: false},
		{Name: "price", Type: arrow.PrimitiveTypes.Float64, Nullable: false},
		{Name: "region", Type: arrow.BinaryTypes.String, Nullable: false},
		{Name: "active", Type: arrow.FixedWidthTypes.Boolean, Nullable: false},
	}
	schema := arrow.NewSchema(fields, nil)
	arrs := []arrow.Array{idB.NewArray(), priceB.NewArray(), regionB.NewArray(), activeB.NewArray()}
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

// -- Basic pipeline ---------------------------------------------------------

func TestLazy_FilterSelectCollect(t *testing.T) {
	df := lazyFrame(t)
	out, err := df.Lazy().
		Filter(Col("price").Gt(Lit(20.0))).
		Select(Col("id"), Col("price").Mul(Lit(1.08)).Alias("usd")).
		Collect()
	if err != nil {
		t.Fatal(err)
	}
	rows, cols := out.Shape()
	if rows != 3 || cols != 2 {
		t.Fatalf("shape = (%d, %d), want (3, 2)", rows, cols)
	}
	names := out.ColumnNames()
	if names[0] != "id" || names[1] != "usd" {
		t.Fatalf("cols = %v, want [id usd]", names)
	}
	// usd column: 30*1.08, 40*1.08, 50*1.08.
	usd, _ := out.Column("usd")
	arr := usd.Column().Data().Chunks()[0].(*array.Float64)
	want := []float64{32.4, 43.2, 54}
	for i, w := range want {
		if v := arr.Value(i); v < w-0.0001 || v > w+0.0001 {
			t.Errorf("row %d usd = %v, want %v", i, v, w)
		}
	}
}

func TestLazy_ChainImmutable(t *testing.T) {
	// Extending a LazyFrame must not mutate the parent — chains
	// starting from the same base must be independent.
	df := lazyFrame(t)
	base := df.Lazy()

	branchA, _ := base.Filter(Col("active")).Collect()
	branchB, _ := base.Filter(Col("active").Not()).Collect()

	if branchA.NumRows() != 3 {
		t.Fatalf("active rows = %d, want 3", branchA.NumRows())
	}
	if branchB.NumRows() != 2 {
		t.Fatalf("inactive rows = %d, want 2", branchB.NumRows())
	}
	// The base plan must still be a plain Scan[frame] — no filter
	// added.
	if _, ok := base.Plan().(*scanFrameNode); !ok {
		t.Fatalf("base plan mutated: %T", base.Plan())
	}
}

// -- WithColumn -------------------------------------------------------------

func TestLazy_WithColumn_AppendsColumn(t *testing.T) {
	df := lazyFrame(t)
	out, err := df.Lazy().
		WithColumn("doubled", Col("price").Mul(Lit(2.0))).
		Collect()
	if err != nil {
		t.Fatal(err)
	}
	if got := out.NumCols(); got != 5 {
		t.Fatalf("cols = %d, want 5 (4 + 1 derived)", got)
	}
	names := out.ColumnNames()
	if names[len(names)-1] != "doubled" {
		t.Fatalf("last col = %q, want doubled", names[len(names)-1])
	}
}

func TestLazy_WithColumn_ReplacesInPlace(t *testing.T) {
	df := lazyFrame(t)
	out, err := df.Lazy().
		WithColumn("price", Col("price").Mul(Lit(1.10))).
		Collect()
	if err != nil {
		t.Fatal(err)
	}
	if got := out.NumCols(); got != 4 {
		t.Fatalf("cols = %d, want 4 (replaced in place)", got)
	}
	// Column order preserved — "price" stays at position 1.
	if out.ColumnNames()[1] != "price" {
		t.Fatalf("cols = %v, want price at index 1", out.ColumnNames())
	}
	price, _ := out.Column("price")
	arr := price.Column().Data().Chunks()[0].(*array.Float64)
	if v := arr.Value(0); v < 10.9999 || v > 11.0001 {
		t.Fatalf("row 0 price = %v, want 11", v)
	}
}

// -- Limit ------------------------------------------------------------------

func TestLazy_Limit(t *testing.T) {
	df := lazyFrame(t)
	out, err := df.Lazy().Limit(2).Collect()
	if err != nil {
		t.Fatal(err)
	}
	if out.NumRows() != 2 {
		t.Fatalf("limited rows = %d, want 2", out.NumRows())
	}
}

func TestLazy_FilterThenLimit(t *testing.T) {
	// Filter reduces to 3 rows; Limit(2) trims to the first 2 of those.
	df := lazyFrame(t)
	out, err := df.Lazy().
		Filter(Col("active")).
		Limit(2).
		Collect()
	if err != nil {
		t.Fatal(err)
	}
	if out.NumRows() != 2 {
		t.Fatalf("rows = %d, want 2", out.NumRows())
	}
	// First two active rows are id=1 and id=3.
	ids, _ := out.Column("id")
	arr := ids.Column().Data().Chunks()[0].(*array.Int64)
	if arr.Value(0) != 1 || arr.Value(1) != 3 {
		t.Fatalf("ids = %v, %v; want 1, 3", arr.Value(0), arr.Value(1))
	}
}

// -- Schema propagation without execution ---------------------------------

func TestLazy_Schema_FilterUnchanged(t *testing.T) {
	df := lazyFrame(t)
	lf := df.Lazy().Filter(Col("price").Gt(Lit(20.0)))
	sch := lf.Schema()
	if got := len(sch.Fields()); got != 4 {
		t.Fatalf("filter should preserve schema; got %d fields", got)
	}
	if sch.Field(1).Name != "price" || sch.Field(1).Type.ID() != arrow.FLOAT64 {
		t.Fatalf("price field not preserved: %+v", sch.Field(1))
	}
}

func TestLazy_Schema_SelectReshapes(t *testing.T) {
	df := lazyFrame(t)
	lf := df.Lazy().Select(
		Col("id"),
		Col("price").Mul(Lit(1.08)).Alias("usd"),
	)
	sch := lf.Schema()
	if len(sch.Fields()) != 2 {
		t.Fatalf("select produces %d fields, want 2", len(sch.Fields()))
	}
	if sch.Field(0).Name != "id" || sch.Field(0).Type.ID() != arrow.INT64 {
		t.Errorf("field 0: %+v", sch.Field(0))
	}
	if sch.Field(1).Name != "usd" || sch.Field(1).Type.ID() != arrow.FLOAT64 {
		t.Errorf("field 1: %+v", sch.Field(1))
	}
}

func TestLazy_Schema_WithColumnPreservesOthers(t *testing.T) {
	df := lazyFrame(t)
	lf := df.Lazy().WithColumn("usd", Col("price").Mul(Lit(1.08)))
	sch := lf.Schema()
	if len(sch.Fields()) != 5 {
		t.Fatalf("with_column produces %d fields, want 5", len(sch.Fields()))
	}
	// The 4 original fields are unchanged.
	origNames := []string{"id", "price", "region", "active"}
	for i, n := range origNames {
		if sch.Field(i).Name != n {
			t.Errorf("field %d = %q, want %q", i, sch.Field(i).Name, n)
		}
	}
	if sch.Field(4).Name != "usd" {
		t.Errorf("appended field = %q, want usd", sch.Field(4).Name)
	}
}

// -- Explain ----------------------------------------------------------------

func TestLazy_Explain(t *testing.T) {
	df := lazyFrame(t)
	got := df.Lazy().
		Filter(Col("price").Gt(Lit(20.0))).
		Select(Col("id"), Col("price").Mul(Lit(1.08)).Alias("usd")).
		Limit(100).
		Explain()

	// Order: outermost first (Limit), Scan last. Indentation grows.
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("explain lines = %d, want 4:\n%s", len(lines), got)
	}
	if !strings.HasPrefix(lines[0], "Limit(") {
		t.Errorf("line 0 = %q, want Limit(", lines[0])
	}
	if !strings.HasPrefix(lines[1], "  Project(") {
		t.Errorf("line 1 = %q, want indented Project(", lines[1])
	}
	if !strings.HasPrefix(lines[2], "    Filter(") {
		t.Errorf("line 2 = %q, want doubly-indented Filter(", lines[2])
	}
	if !strings.HasPrefix(lines[3], "      Scan[frame]") {
		t.Errorf("line 3 = %q, want triply-indented Scan", lines[3])
	}
}

// -- Column naming rules ---------------------------------------------------

func TestLazy_Select_ColRefKeepsName(t *testing.T) {
	df := lazyFrame(t)
	out, err := df.Lazy().Select(Col("id"), Col("region")).Collect()
	if err != nil {
		t.Fatal(err)
	}
	if got := out.ColumnNames(); got[0] != "id" || got[1] != "region" {
		t.Fatalf("cols = %v, want [id region]", got)
	}
}

func TestLazy_Select_AnonymousExprGetsPositional(t *testing.T) {
	// A non-Col, non-Alias expression falls back to expr_N naming.
	df := lazyFrame(t)
	out, err := df.Lazy().Select(Col("price").Mul(Lit(2.0))).Collect()
	if err != nil {
		t.Fatal(err)
	}
	if got := out.ColumnNames(); got[0] != "expr_0" {
		t.Fatalf("cols = %v, want [expr_0]", got)
	}
}

func TestLazy_Select_AliasWins(t *testing.T) {
	df := lazyFrame(t)
	out, err := df.Lazy().Select(
		Col("price").Mul(Lit(2.0)).Alias("doubled"),
	).Collect()
	if err != nil {
		t.Fatal(err)
	}
	if got := out.ColumnNames(); got[0] != "doubled" {
		t.Fatalf("cols = %v, want [doubled]", got)
	}
}

// -- Empty select edge case ------------------------------------------------

func TestLazy_Select_Empty(t *testing.T) {
	df := lazyFrame(t)
	out, err := df.Lazy().Select().Collect()
	if err != nil {
		t.Fatal(err)
	}
	if out.NumCols() != 0 {
		t.Fatalf("empty select cols = %d, want 0", out.NumCols())
	}
}

// -- Error propagation -----------------------------------------------------

func TestLazy_MissingColumn_ErrorsAtCollect(t *testing.T) {
	// Building the plan should succeed — the error only surfaces
	// when Collect() actually runs the expression.
	df := lazyFrame(t)
	lf := df.Lazy().Filter(Col("nope").Gt(Lit(0.0)))
	if lf.Plan() == nil {
		t.Fatal("plan should exist despite missing column")
	}
	_, err := lf.Collect()
	if !errors.Is(err, ErrColumnNotFound) {
		t.Fatalf("want ErrColumnNotFound at collect, got %v", err)
	}
}

func TestLazy_FilterNonBool_ErrorsAtCollect(t *testing.T) {
	// price is Float64, not Bool. Filter should reject at Collect().
	df := lazyFrame(t)
	_, err := df.Lazy().Filter(Col("price").Mul(Lit(2.0))).Collect()
	if !errors.Is(err, ErrExprTypeMismatch) {
		t.Fatalf("want ErrExprTypeMismatch, got %v", err)
	}
}

// -- Custom Namer via user extension --------------------------------------

// namedCustomNode is a user-defined ExprNode that carries its own
// output name via the Namer interface — verifies external packages
// can plug into the Select naming rules.
type namedCustomNode struct {
	name string
}

func (n *namedCustomNode) Eval(input *Frame) (Series, error) {
	// Return a Float64 series of all ones, matching input length.
	return broadcastLiteral(1.0, arrow.PrimitiveTypes.Float64, input.NumRows())
}
func (n *namedCustomNode) Type(*arrow.Schema) (arrow.DataType, error) {
	return arrow.PrimitiveTypes.Float64, nil
}
func (n *namedCustomNode) Children() []Expr    { return nil }
func (n *namedCustomNode) String() string      { return "custom(" + n.name + ")" }
func (n *namedCustomNode) OutputName() string  { return n.name }

func TestLazy_Select_CustomNamerHonored(t *testing.T) {
	df := lazyFrame(t)
	e := Custom(&namedCustomNode{name: "flag"})
	out, err := df.Lazy().Select(e).Collect()
	if err != nil {
		t.Fatal(err)
	}
	if got := out.ColumnNames(); got[0] != "flag" {
		t.Fatalf("cols = %v, want [flag]", got)
	}
}

// -- Sort ------------------------------------------------------------------

func TestLazy_SortBy(t *testing.T) {
	df := lazyFrame(t)
	out, err := df.Lazy().
		SortBy(SortKey{Column: "price", Descending: true}).
		Collect()
	if err != nil {
		t.Fatal(err)
	}
	// Descending price: 50, 40, 30, 20, 10 → ids 5, 4, 3, 2, 1.
	ids, _ := out.Column("id")
	arr := ids.Column().Data().Chunks()[0].(*array.Int64)
	want := []int64{5, 4, 3, 2, 1}
	for i, w := range want {
		if arr.Value(i) != w {
			t.Errorf("row %d id = %d, want %d", i, arr.Value(i), w)
		}
	}
}

func TestLazy_SortBy_SchemaUnchanged(t *testing.T) {
	df := lazyFrame(t)
	sch := df.Lazy().SortBy(SortKey{Column: "id"}).Schema()
	if len(sch.Fields()) != 4 {
		t.Fatalf("sort should preserve schema; got %d fields", len(sch.Fields()))
	}
}

func TestLazy_SortBy_Explain(t *testing.T) {
	df := lazyFrame(t)
	got := df.Lazy().
		SortBy(SortKey{Column: "price"}, SortKey{Column: "id", Descending: true}).
		Explain()
	if !strings.Contains(got, "Sort(price, id DESC)") {
		t.Fatalf("explain missing Sort line:\n%s", got)
	}
}

// -- Aggregate -------------------------------------------------------------

func TestLazy_GroupByAgg(t *testing.T) {
	df := lazyFrame(t)
	out, err := df.Lazy().
		GroupBy("region").
		Agg(
			Aggregation{Column: "price", Kind: AggSum},
			Aggregation{Column: "price", Kind: AggCount, Alias: "n"},
		).
		Collect()
	if err != nil {
		t.Fatal(err)
	}
	// Groups: EU (price 20+40=60, n=2), US (10+30+50=90, n=3).
	if got := out.NumRows(); got != 2 {
		t.Fatalf("groups = %d, want 2", got)
	}
	sums, _ := out.Column("price_sum")
	sumsArr := sums.Column().Data().Chunks()[0].(*array.Float64)
	counts, _ := out.Column("n")
	countsArr := counts.Column().Data().Chunks()[0].(*array.Int64)
	// Groups arrive in sorted key order: EU first, then US.
	if sumsArr.Value(0) != 60 || countsArr.Value(0) != 2 {
		t.Errorf("EU: got sum=%v n=%d, want 60/2", sumsArr.Value(0), countsArr.Value(0))
	}
	if sumsArr.Value(1) != 90 || countsArr.Value(1) != 3 {
		t.Errorf("US: got sum=%v n=%d, want 90/3", sumsArr.Value(1), countsArr.Value(1))
	}
}

func TestLazy_GroupByAgg_Schema(t *testing.T) {
	df := lazyFrame(t)
	sch := df.Lazy().
		GroupBy("region").
		Agg(
			Aggregation{Column: "price", Kind: AggSum},
			Aggregation{Column: "price", Kind: AggCount, Alias: "n"},
		).
		Schema()
	if len(sch.Fields()) != 3 {
		t.Fatalf("agg schema fields = %d, want 3 (key + 2 aggs)", len(sch.Fields()))
	}
	if sch.Field(0).Name != "region" || sch.Field(0).Type.ID() != arrow.STRING {
		t.Errorf("key field = %+v, want region:String", sch.Field(0))
	}
	if sch.Field(1).Name != "price_sum" || sch.Field(1).Type.ID() != arrow.FLOAT64 {
		t.Errorf("sum field = %+v, want price_sum:Float64", sch.Field(1))
	}
	if sch.Field(2).Name != "n" || sch.Field(2).Type.ID() != arrow.INT64 {
		t.Errorf("count field = %+v, want n:Int64", sch.Field(2))
	}
	if sch.Field(2).Nullable {
		t.Errorf("count should be non-null")
	}
}

// -- Join ------------------------------------------------------------------

func TestLazy_Join_Inner(t *testing.T) {
	out, err := lazyFrame(t).Lazy().
		Join(lazyRegions(t).Lazy(), "region", "region", JoinInner).
		Collect()
	if err != nil {
		t.Fatal(err)
	}
	if got := out.NumRows(); got != 5 {
		t.Fatalf("inner join rows = %d, want 5", got)
	}
	if got := out.NumCols(); got != 5 {
		t.Fatalf("cols = %d, want 5 (4 left + 1 right)", got)
	}
	if _, err := out.Column("region_name"); err != nil {
		t.Fatalf("region_name missing: %v", err)
	}
}

func TestLazy_Join_Left(t *testing.T) {
	// Right frame lacks a "US" row; US left rows survive with a null
	// region_name.
	out, err := lazyFrame(t).Lazy().
		Join(lazyRegionsMissingUS(t).Lazy(), "region", "region", JoinLeft).
		Collect()
	if err != nil {
		t.Fatal(err)
	}
	if got := out.NumRows(); got != 5 {
		t.Fatalf("left join rows = %d, want 5", got)
	}
	col, _ := out.Column("region_name")
	arr := col.Column().Data().Chunks()[0].(*array.String)
	nulls := 0
	for i := range arr.Len() {
		if arr.IsNull(i) {
			nulls++
		}
	}
	if nulls != 3 {
		t.Fatalf("null region_names = %d, want 3", nulls)
	}
}

func TestLazy_Join_Semi(t *testing.T) {
	out, err := lazyFrame(t).Lazy().
		Join(lazyRegions(t).Lazy(), "region", "region", JoinSemi).
		Collect()
	if err != nil {
		t.Fatal(err)
	}
	if got := out.NumCols(); got != 4 {
		t.Fatalf("semi join cols = %d, want 4 (left only)", got)
	}
	if _, err := out.Column("region_name"); err == nil {
		t.Fatal("semi join should not carry right-side columns")
	}
}

func TestLazy_Join_Schema(t *testing.T) {
	sch := lazyFrame(t).Lazy().
		Join(lazyRegions(t).Lazy(), "region", "region", JoinInner).
		Schema()
	if len(sch.Fields()) != 5 {
		t.Fatalf("join schema fields = %d, want 5", len(sch.Fields()))
	}
	if sch.Field(4).Name != "region_name" {
		t.Errorf("last field = %q, want region_name", sch.Field(4).Name)
	}
}

func TestLazy_Join_Explain_TwoSubtrees(t *testing.T) {
	got := lazyFrame(t).Lazy().
		Join(lazyRegions(t).Lazy(), "region", "region", JoinInner).
		Explain()
	if !strings.Contains(got, "Join(inner, left.region = right.region)") {
		t.Fatalf("explain missing Join line:\n%s", got)
	}
	// Two Scan leaves — one per input plan.
	if scans := strings.Count(got, "Scan[frame]"); scans != 2 {
		t.Fatalf("scan lines = %d, want 2:\n%s", scans, got)
	}
}

// -- Mixed pipeline: everything together ----------------------------------

func TestLazy_MixedPipeline(t *testing.T) {
	// filter → join → group-by → sort → limit end-to-end.
	out, err := lazyFrame(t).Lazy().
		Filter(Col("active")).
		Join(lazyRegions(t).Lazy(), "region", "region", JoinInner).
		GroupBy("region_name").
		Agg(Aggregation{Column: "price", Kind: AggSum}).
		SortBy(SortKey{Column: "price_sum", Descending: true}).
		Limit(1).
		Collect()
	if err != nil {
		t.Fatal(err)
	}
	if got := out.NumRows(); got != 1 {
		t.Fatalf("rows = %d, want 1", got)
	}
	// Active US rows: prices 10, 30, 50 = 90. Active EU rows: none.
	// So the top-1 row is "United States", 90.
	names, _ := out.Column("region_name")
	arr := names.Column().Data().Chunks()[0].(*array.String)
	if arr.Value(0) != "United States" {
		t.Fatalf("top region = %q, want United States", arr.Value(0))
	}
}

// -- Test helpers ---------------------------------------------------------

// lazyRegions builds a small right-side lookup for join tests:
//
//	region  region_name
//	EU      "European Union"
//	US      "United States"
func lazyRegions(t *testing.T) *Frame {
	t.Helper()
	return buildRegionFrame(t, []string{"EU", "US"}, []string{"European Union", "United States"})
}

// lazyRegionsMissingUS returns a right-side frame that lacks a "US"
// row — used by the left-join test.
func lazyRegionsMissingUS(t *testing.T) *Frame {
	t.Helper()
	return buildRegionFrame(t, []string{"EU"}, []string{"European Union"})
}

// -- DropColumn / Head / Tail --------------------------------------------

func TestLazy_DropColumn(t *testing.T) {
	df := lazyFrame(t)
	out, err := df.Lazy().DropColumn("region").Collect()
	if err != nil {
		t.Fatal(err)
	}
	if got := out.NumCols(); got != 3 {
		t.Fatalf("cols = %d, want 3 (4 - 1)", got)
	}
	if _, err := out.Column("region"); err == nil {
		t.Fatal("region should be gone")
	}
}

func TestLazy_DropColumn_Schema(t *testing.T) {
	df := lazyFrame(t)
	sch := df.Lazy().DropColumn("region").Schema()
	if len(sch.Fields()) != 3 {
		t.Fatalf("schema fields = %d, want 3", len(sch.Fields()))
	}
	for _, f := range sch.Fields() {
		if f.Name == "region" {
			t.Fatalf("region still in schema: %+v", sch.Fields())
		}
	}
}

func TestLazy_DropColumn_MissingErrorsAtCollect(t *testing.T) {
	df := lazyFrame(t)
	lf := df.Lazy().DropColumn("nope")
	if lf.Plan() == nil {
		t.Fatal("plan should exist despite missing column")
	}
	_, err := lf.Collect()
	if !errors.Is(err, ErrColumnNotFound) {
		t.Fatalf("want ErrColumnNotFound, got %v", err)
	}
}

func TestLazy_Head_IsLimit(t *testing.T) {
	// Head(n) should build the same plan as Limit(n) — just a
	// pandas-shaped alias.
	df := lazyFrame(t)
	headPlan := df.Lazy().Head(2).Plan()
	limitPlan := df.Lazy().Limit(2).Plan()
	if _, ok := headPlan.(*limitNode); !ok {
		t.Fatalf("Head node = %T, want *limitNode", headPlan)
	}
	if headPlan.String() != limitPlan.String() {
		t.Fatalf("Head plan = %s, want same as Limit: %s",
			headPlan.String(), limitPlan.String())
	}
}

func TestLazy_Tail(t *testing.T) {
	df := lazyFrame(t)
	out, err := df.Lazy().Tail(2).Collect()
	if err != nil {
		t.Fatal(err)
	}
	if out.NumRows() != 2 {
		t.Fatalf("tail rows = %d, want 2", out.NumRows())
	}
	// Last two rows are ids 4 and 5.
	ids, _ := out.Column("id")
	arr := ids.Column().Data().Chunks()[0].(*array.Int64)
	if arr.Value(0) != 4 || arr.Value(1) != 5 {
		t.Fatalf("tail ids = %d, %d; want 4, 5", arr.Value(0), arr.Value(1))
	}
}

func TestLazy_Tail_AfterFilter(t *testing.T) {
	// Tail should apply after Filter has narrowed the frame — the
	// "last 2 rows" are relative to the filtered result, not the
	// source.
	df := lazyFrame(t)
	out, err := df.Lazy().
		Filter(Col("active")).
		Tail(2).
		Collect()
	if err != nil {
		t.Fatal(err)
	}
	if out.NumRows() != 2 {
		t.Fatalf("rows = %d, want 2", out.NumRows())
	}
	// Active rows are ids 1, 3, 5; tail 2 → ids 3, 5.
	ids, _ := out.Column("id")
	arr := ids.Column().Data().Chunks()[0].(*array.Int64)
	if arr.Value(0) != 3 || arr.Value(1) != 5 {
		t.Fatalf("filtered tail ids = %d, %d; want 3, 5", arr.Value(0), arr.Value(1))
	}
}

func TestLazy_Tail_Explain(t *testing.T) {
	df := lazyFrame(t)
	got := df.Lazy().Tail(10).Explain()
	if !strings.Contains(got, "Tail(10)") {
		t.Fatalf("explain missing Tail:\n%s", got)
	}
}

// -- NewScanNode extension point -----------------------------------------

// TestLazy_NewScanNode_DeferredExecution verifies that constructing a
// LazyFrame from a scan node does not invoke the read closure — only
// Collect does.
func TestLazy_NewScanNode_DeferredExecution(t *testing.T) {
	invoked := 0
	frame := lazyFrame(t)
	node := NewScanNode(
		"Scan[test](fake)",
		frame.Schema(),
		func() (*Frame, error) {
			invoked++
			return frame, nil
		},
	)
	lf := NewLazyFrame(node)

	// Neither Schema() nor Explain() should trigger the read.
	_ = lf.Schema()
	_ = lf.Explain()
	if invoked != 0 {
		t.Fatalf("read invoked %d times before Collect; want 0", invoked)
	}
	if _, err := lf.Collect(); err != nil {
		t.Fatal(err)
	}
	if invoked != 1 {
		t.Fatalf("read invoked %d times after Collect; want 1", invoked)
	}
}

func TestLazy_NewScanNode_DeferredError(t *testing.T) {
	// Constructor errors that a source package caches into the closure
	// must surface at Collect, not at build time.
	node := NewScanNode(
		"Scan[test](bad)",
		nil,
		func() (*Frame, error) {
			return nil, errors.New("boom")
		},
	)
	lf := NewLazyFrame(node)
	_, err := lf.Collect()
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("want boom, got %v", err)
	}
}

// -- Test helpers ---------------------------------------------------------

func buildRegionFrame(t *testing.T, codes, names []string) *Frame {
	t.Helper()
	pool := memory.DefaultAllocator

	codeB := array.NewStringBuilder(pool)
	defer codeB.Release()
	codeB.AppendValues(codes, nil)

	nameB := array.NewStringBuilder(pool)
	defer nameB.Release()
	nameB.AppendValues(names, nil)

	fields := []arrow.Field{
		{Name: "region", Type: arrow.BinaryTypes.String, Nullable: false},
		{Name: "region_name", Type: arrow.BinaryTypes.String, Nullable: false},
	}
	schema := arrow.NewSchema(fields, nil)
	arrs := []arrow.Array{codeB.NewArray(), nameB.NewArray()}
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
