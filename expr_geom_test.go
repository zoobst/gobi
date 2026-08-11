package gobi

import (
	"strings"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/zoobst/gobi/geometry"
)

// levelGeomFrame builds a two-column Frame — a "level" Float64 column
// and a "geometry" Binary/WKB column — that mirrors the shape of the
// GSHHS parquet output the motivation calls out. Nulls in either
// column are supported to exercise null propagation.
func levelGeomFrame(t *testing.T, levels []any, geoms []geometry.Geometry, epsg int32) *Frame {
	t.Helper()
	if len(levels) != len(geoms) {
		t.Fatalf("levelGeomFrame: mismatched lengths (%d vs %d)", len(levels), len(geoms))
	}
	pool := memory.DefaultAllocator

	lb := array.NewFloat64Builder(pool)
	defer lb.Release()
	for _, v := range levels {
		if v == nil {
			lb.AppendNull()
			continue
		}
		lb.Append(v.(float64))
	}

	gb := array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary)
	defer gb.Release()
	for _, g := range geoms {
		if g == nil {
			gb.AppendNull()
			continue
		}
		gb.Append(geometry.WKB(g))
	}

	fields := []arrow.Field{
		{Name: "level", Type: arrow.PrimitiveTypes.Float64, Nullable: true},
		GeometryField("geometry", epsg),
	}
	schema := arrow.NewSchema(fields, nil)
	arrs := []arrow.Array{lb.NewArray(), gb.NewArray()}
	defer func() {
		for _, a := range arrs {
			a.Release()
		}
	}()
	cols := make([]arrow.Column, 2)
	for i, a := range arrs {
		chunked := arrow.NewChunked(a.DataType(), []arrow.Array{a})
		cols[i] = *arrow.NewColumn(fields[i], chunked)
		chunked.Release()
	}
	f, err := NewFrame(schema, cols)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// evalBool evaluates e against f and returns the resulting []any so
// tests can compare null / true / false uniformly.
func evalBool(t *testing.T, f *Frame, e Expr) []any {
	t.Helper()
	s, err := e.Node().Eval(f)
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if s.DataType().ID() != arrow.BOOL {
		t.Fatalf("expected Boolean output, got %s", s.DataType())
	}
	return boolSeriesValues(t, s)
}

// TestExpr_GeomIntersects_ConstantRight covers the fast path — a
// LitGeom on the right side. This is the primary shape the motivation
// calls out.
func TestExpr_GeomIntersects_ConstantRight(t *testing.T) {
	epsg := int32(geometry.PseudoMercator.EPSG)
	f := levelGeomFrame(t,
		[]any{1.0, 1.0, 2.0, nil},
		[]geometry.Geometry{
			projectedSquare(0, 0, 10),  // overlaps mask
			projectedSquare(50, 50, 5), // disjoint
			projectedSquare(6, 0, 4),   // overlaps mask
			nil,                        // null row
		},
		epsg,
	)
	aoi := projectedSquare(5, 0, 10) // mask spans [5,15] × [0,10]

	got := evalBool(t, f, Col("geometry").GeomIntersects(Lit(aoi)))
	want := []any{true, false, true, nil}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %v, want %v", i, got[i], want[i])
		}
	}
}

// TestExpr_GeomIntersects_ComposesWithScalar is the motivation's
// exact use case: `level == 1 AND geometry intersects AOI`, expressed
// as a single Filter expression.
func TestExpr_GeomIntersects_ComposesWithScalar(t *testing.T) {
	epsg := int32(geometry.PseudoMercator.EPSG)
	f := levelGeomFrame(t,
		[]any{1.0, 1.0, 2.0, nil},
		[]geometry.Geometry{
			projectedSquare(0, 0, 10),  // level=1, overlaps → KEEP
			projectedSquare(50, 50, 5), // level=1, disjoint → drop
			projectedSquare(6, 0, 4),   // level=2, overlaps → drop
			nil,                        // level null → drop
		},
		epsg,
	)
	aoi := projectedSquare(5, 0, 10)

	out, err := f.Lazy().Filter(
		Col("level").Eq(Lit(1.0)).
			And(Col("geometry").GeomIntersects(Lit(aoi))),
	).Collect()
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if got := out.NumRows(); got != 1 {
		t.Fatalf("row count = %d, want 1", got)
	}
	levelCol, _ := out.Column("level")
	if v, _, ok := levelCol.singleF64(); !ok || v[0] != 1.0 {
		t.Errorf("kept row's level = %v, want 1.0", v)
	}
}

// TestExpr_GeomIntersects_ColumnRight is the new capability: right
// side is another geometry column, not a constant.
func TestExpr_GeomIntersects_ColumnRight(t *testing.T) {
	epsg := int32(geometry.PseudoMercator.EPSG)
	pool := memory.DefaultAllocator

	// Left column: 3 polygons, one null.
	lb := array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary)
	defer lb.Release()
	lb.Append(geometry.WKB(projectedSquare(0, 0, 10)))
	lb.Append(geometry.WKB(projectedSquare(50, 50, 5)))
	lb.AppendNull()

	// Right column: pair-wise partner geometries.
	rb := array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary)
	defer rb.Release()
	rb.Append(geometry.WKB(projectedSquare(5, 5, 5))) // overlaps row 0
	rb.Append(geometry.WKB(projectedSquare(0, 0, 5))) // disjoint from row 1
	rb.Append(geometry.WKB(projectedSquare(0, 0, 5))) // right non-null but left null → null out

	fields := []arrow.Field{
		GeometryField("left_geom", epsg),
		GeometryField("right_geom", epsg),
	}
	schema := arrow.NewSchema(fields, nil)
	arrs := []arrow.Array{lb.NewArray(), rb.NewArray()}
	defer func() {
		for _, a := range arrs {
			a.Release()
		}
	}()
	cols := make([]arrow.Column, 2)
	for i, a := range arrs {
		chunked := arrow.NewChunked(a.DataType(), []arrow.Array{a})
		cols[i] = *arrow.NewColumn(fields[i], chunked)
		chunked.Release()
	}
	f, err := NewFrame(schema, cols)
	if err != nil {
		t.Fatal(err)
	}

	got := evalBool(t, f, Col("left_geom").GeomIntersects(Col("right_geom")))
	want := []any{true, false, nil}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %v, want %v", i, got[i], want[i])
		}
	}
}

// TestExpr_GeomIntersects_ColumnRight_NullRightPropagates: even when
// the LEFT is non-null, a null on the right side should produce null.
func TestExpr_GeomIntersects_ColumnRight_NullRightPropagates(t *testing.T) {
	epsg := int32(geometry.PseudoMercator.EPSG)
	pool := memory.DefaultAllocator

	lb := array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary)
	defer lb.Release()
	lb.Append(geometry.WKB(projectedSquare(0, 0, 10)))
	lb.Append(geometry.WKB(projectedSquare(0, 0, 10)))

	rb := array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary)
	defer rb.Release()
	rb.Append(geometry.WKB(projectedSquare(5, 5, 2)))
	rb.AppendNull() // both left and right present → but right null → null out

	fields := []arrow.Field{
		GeometryField("left_geom", epsg),
		GeometryField("right_geom", epsg),
	}
	schema := arrow.NewSchema(fields, nil)
	arrs := []arrow.Array{lb.NewArray(), rb.NewArray()}
	defer func() {
		for _, a := range arrs {
			a.Release()
		}
	}()
	cols := make([]arrow.Column, 2)
	for i, a := range arrs {
		chunked := arrow.NewChunked(a.DataType(), []arrow.Array{a})
		cols[i] = *arrow.NewColumn(fields[i], chunked)
		chunked.Release()
	}
	f, err := NewFrame(schema, cols)
	if err != nil {
		t.Fatal(err)
	}

	got := evalBool(t, f, Col("left_geom").GeomIntersects(Col("right_geom")))
	want := []any{true, nil}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %v, want %v", i, got[i], want[i])
		}
	}
}

// TestExpr_GeomContains_ConstantRight: Contains-shaped predicate,
// where the mask must lie inside the row's polygon.
func TestExpr_GeomContains_ConstantRight(t *testing.T) {
	epsg := int32(geometry.PseudoMercator.EPSG)
	f := levelGeomFrame(t,
		[]any{1.0, 1.0},
		[]geometry.Geometry{
			projectedSquare(0, 0, 100), // 100×100, easily contains a 5×5
			projectedSquare(0, 0, 5),   // 5×5, smaller than the mask
		},
		epsg,
	)
	mask := projectedSquare(10, 10, 5) // 5×5 square at (10,10)-(15,15)

	got := evalBool(t, f, Col("geometry").GeomContains(Lit(mask)))
	want := []any{true, false}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %v, want %v", i, got[i], want[i])
		}
	}
}

// TestExpr_GeomWithin_ConstantRight: mirror of Contains — the row's
// polygon must lie inside the constant mask.
func TestExpr_GeomWithin_ConstantRight(t *testing.T) {
	epsg := int32(geometry.PseudoMercator.EPSG)
	f := levelGeomFrame(t,
		[]any{1.0, 1.0},
		[]geometry.Geometry{
			projectedSquare(10, 10, 5),   // fully inside the big mask
			projectedSquare(200, 200, 5), // disjoint from mask
		},
		epsg,
	)
	mask := projectedSquare(0, 0, 100)

	got := evalBool(t, f, Col("geometry").GeomWithin(Lit(mask)))
	want := []any{true, false}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %v, want %v", i, got[i], want[i])
		}
	}
}

// TestExpr_GeomDisjoint_ConstantRight is the exact negation of
// GeomIntersects — matching test data.
func TestExpr_GeomDisjoint_ConstantRight(t *testing.T) {
	epsg := int32(geometry.PseudoMercator.EPSG)
	f := levelGeomFrame(t,
		[]any{1.0, 1.0, 1.0, nil},
		[]geometry.Geometry{
			projectedSquare(0, 0, 10),
			projectedSquare(50, 50, 5),
			projectedSquare(6, 0, 4),
			nil,
		},
		epsg,
	)
	aoi := projectedSquare(5, 0, 10)

	got := evalBool(t, f, Col("geometry").GeomDisjoint(Lit(aoi)))
	// Complement of the Intersects test (with null still → null).
	want := []any{false, true, false, nil}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %v, want %v", i, got[i], want[i])
		}
	}
}

// TestExpr_LitGeom_NilRightAllNull: Lit(nil-geom) is degenerate; the
// executor should return an all-null column rather than error.
func TestExpr_LitGeom_NilRightAllNull(t *testing.T) {
	epsg := int32(geometry.PseudoMercator.EPSG)
	f := levelGeomFrame(t,
		[]any{1.0, 1.0},
		[]geometry.Geometry{
			projectedSquare(0, 0, 10),
			projectedSquare(50, 50, 5),
		},
		epsg,
	)
	got := evalBool(t, f, Col("geometry").GeomIntersects(LitGeom(nil)))
	want := []any{nil, nil}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %v, want %v", i, got[i], want[i])
		}
	}
}

// TestExpr_LitGeom_NonPredicateEvalErrors: LitGeom is a
// predicate-only marker; putting it into a non-predicate position
// (WithColumn / Select) must error at Eval rather than silently
// materialize N copies of the WKB blob. See the docstring on
// literalGeomNode.Eval for the rationale.
func TestExpr_LitGeom_NonPredicateEvalErrors(t *testing.T) {
	epsg := int32(geometry.PseudoMercator.EPSG)
	f := levelGeomFrame(t,
		[]any{1.0, 1.0},
		[]geometry.Geometry{
			projectedSquare(0, 0, 10),
			projectedSquare(50, 50, 5),
		},
		epsg,
	)
	// A non-predicate position: WithColumn broadcasting a LitGeom.
	// Should error rather than allocate a Binary column of duplicates.
	_, err := f.WithColumnExpr("aoi_broadcast", LitGeom(projectedSquare(0, 0, 1)))
	if err == nil {
		t.Fatalf("WithColumnExpr(LitGeom) should error; got success")
	}
	// Message should mention LitGeom being predicate-only so users
	// can find the right escape hatch.
	if !strings.Contains(err.Error(), "LitGeom") || !strings.Contains(err.Error(), "predicate") {
		t.Errorf("error message = %q; expected mention of LitGeom / predicate", err.Error())
	}
}

// TestExpr_GeomIntersects_LitAcceptsGeometry: the plain Lit(v) shortcut
// must route geometry.Geometry values through LitGeom so callers don't
// have to think about which constructor to use.
func TestExpr_GeomIntersects_LitAcceptsGeometry(t *testing.T) {
	aoi := projectedSquare(5, 0, 10)
	e := Lit(aoi)
	if _, ok := e.Node().(*literalGeomNode); !ok {
		t.Fatalf("Lit(polygon) produced %T, want *literalGeomNode", e.Node())
	}
}

// TestExpr_GeomIntersects_Reflection_ChildrenAndType: sanity-check the
// ExprNode interface implementations that tree walkers depend on.
func TestExpr_GeomIntersects_Reflection_ChildrenAndType(t *testing.T) {
	e := Col("geometry").GeomIntersects(Lit(projectedSquare(0, 0, 1)))
	children := e.Node().Children()
	if len(children) != 2 {
		t.Fatalf("children count = %d, want 2", len(children))
	}
	dt, err := e.Node().Type(nil)
	if err != nil {
		t.Fatalf("Type: %v", err)
	}
	if dt.ID() != arrow.BOOL {
		t.Errorf("Type = %s, want Boolean", dt)
	}
}

// TestExpr_GeomTouches_ConstantRight: touching-but-not-overlapping is
// the classic Touches shape. Mirrors TestSeries_GeomTouches.
func TestExpr_GeomTouches_ConstantRight(t *testing.T) {
	epsg := int32(geometry.PseudoMercator.EPSG)
	f := levelGeomFrame(t,
		[]any{1.0, 1.0, 1.0},
		[]geometry.Geometry{
			projectedSquare(10, 0, 5),  // touches mask at X=10
			projectedSquare(0, 0, 10),  // overlaps → not a touch
			projectedSquare(50, 50, 5), // disjoint → not a touch
		},
		epsg,
	)
	mask := projectedSquare(5, 0, 5)
	got := evalBool(t, f, Col("geometry").GeomTouches(Lit(mask)))
	want := []any{true, false, false}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %v, want %v", i, got[i], want[i])
		}
	}
}

// TestExpr_GeomOverlaps_ConstantRight: partial-overlap yes, fully
// contained no, disjoint no. Mirrors TestSeries_GeomOverlaps.
func TestExpr_GeomOverlaps_ConstantRight(t *testing.T) {
	epsg := int32(geometry.PseudoMercator.EPSG)
	f := levelGeomFrame(t,
		[]any{1.0, 1.0, 1.0},
		[]geometry.Geometry{
			projectedSquare(0, 0, 10),  // partial overlap → true
			projectedSquare(6, 1, 2),   // fully contained by mask → false
			projectedSquare(50, 50, 5), // disjoint → false
		},
		epsg,
	)
	mask := projectedSquare(5, 0, 10)
	got := evalBool(t, f, Col("geometry").GeomOverlaps(Lit(mask)))
	want := []any{true, false, false}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %v, want %v", i, got[i], want[i])
		}
	}
}

// TestExpr_GeomCrosses_ConstantRight: a LineString cutting through a
// Polygon (mixed dimension → the shape Crosses is designed for).
func TestExpr_GeomCrosses_ConstantRight(t *testing.T) {
	epsg := int32(geometry.PseudoMercator.EPSG)
	// Row 0: a line entering and exiting the mask square → crosses.
	// Row 1: a line entirely outside the mask → no crosses.
	// Row 2: a line entirely inside the mask (endpoints inside) →
	//        Contains, so Crosses is false by definition.
	crossing := geometry.LineString{Points: []geometry.Point{
		{X: -1, Y: 5, CRSValue: geometry.PseudoMercator},
		{X: 20, Y: 5, CRSValue: geometry.PseudoMercator},
	}, CRSValue: geometry.PseudoMercator}
	outside := geometry.LineString{Points: []geometry.Point{
		{X: 100, Y: 100, CRSValue: geometry.PseudoMercator},
		{X: 120, Y: 100, CRSValue: geometry.PseudoMercator},
	}, CRSValue: geometry.PseudoMercator}
	inside := geometry.LineString{Points: []geometry.Point{
		{X: 6, Y: 3, CRSValue: geometry.PseudoMercator},
		{X: 8, Y: 3, CRSValue: geometry.PseudoMercator},
	}, CRSValue: geometry.PseudoMercator}
	f := levelGeomFrame(t,
		[]any{1.0, 1.0, 1.0},
		[]geometry.Geometry{crossing, outside, inside},
		epsg,
	)
	mask := projectedSquare(5, 0, 10) // [5,15] × [0,10]
	got := evalBool(t, f, Col("geometry").GeomCrosses(Lit(mask)))
	want := []any{true, false, false}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %v, want %v", i, got[i], want[i])
		}
	}
}
