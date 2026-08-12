package gobi

import (
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/zoobst/gobi/geometry"
)

// TestSeries_GeomDWithin covers the Series layer's within-distance
// predicate. Fixed-CRS PseudoMercator so "distance = 3" means 3
// coord units (meters in that CRS).
func TestSeries_GeomDWithin(t *testing.T) {
	// AOI bbox = [0, 0, 10, 10] (right edge x=10).
	// Row 0: [10, 0, 15, 5]  → touches AOI right edge (dist = 0).
	// Row 1: [12, 0, 13, 1]  → 2 units past AOI right edge.
	// Row 2: [50, 0, 55, 5]  → 40 units past AOI right edge.
	// Row 3: null.
	s := geomSeries(t, "geom", int32(geometry.PseudoMercator.EPSG), []geometry.Geometry{
		projectedSquare(10, 0, 5),
		projectedSquare(12, 0, 1),
		projectedSquare(50, 0, 5),
		nil,
	})
	aoi := projectedSquare(0, 0, 10)

	got, err := s.GeomDWithin(aoi, 3)
	if err != nil {
		t.Fatalf("GeomDWithin: %v", err)
	}
	vals := boolSeriesValues(t, got)
	// row 0 touches → within 3
	// row 1 is 2 units from AOI's right edge → within 3
	// row 2 is 40 units from AOI → NOT within 3
	// row 3 null → null
	want := []any{true, true, false, nil}
	for i, w := range want {
		if vals[i] != w {
			t.Errorf("row %d = %v, want %v", i, vals[i], w)
		}
	}
}

// TestSeries_GeomDWithin_ZeroIsIntersects: distance = 0 must match
// the Intersects contract (touching = true, disjoint = false).
func TestSeries_GeomDWithin_ZeroIsIntersects(t *testing.T) {
	s := geomSeries(t, "geom", int32(geometry.PseudoMercator.EPSG), []geometry.Geometry{
		projectedSquare(10, 0, 5), // touches AOI (edge at x=10)
		projectedSquare(11, 0, 5), // 1 unit away → disjoint
	})
	aoi := projectedSquare(0, 0, 10)
	got, err := s.GeomDWithin(aoi, 0)
	if err != nil {
		t.Fatalf("GeomDWithin: %v", err)
	}
	vals := boolSeriesValues(t, got)
	want := []any{true, false}
	for i, w := range want {
		if vals[i] != w {
			t.Errorf("row %d = %v, want %v", i, vals[i], w)
		}
	}
}

// TestExpr_GeomDWithin_ConstantRight — Expr form composes with
// LazyFrame.Filter the same way as the binary predicates.
func TestExpr_GeomDWithin_ConstantRight(t *testing.T) {
	epsg := int32(geometry.PseudoMercator.EPSG)
	f := levelGeomFrame(t,
		[]any{1.0, 1.0, 1.0, nil},
		[]geometry.Geometry{
			projectedSquare(10, 0, 5), // touches AOI
			projectedSquare(12, 0, 1), // 2 units away
			projectedSquare(50, 0, 5), // 40 units away
			nil,
		},
		epsg,
	)
	aoi := projectedSquare(0, 0, 10)

	got := evalBool(t, f, Col("geometry").GeomDWithin(Lit(aoi), 3))
	want := []any{true, true, false, nil}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("row %d = %v, want %v", i, got[i], w)
		}
	}
}

// TestExpr_GeomDWithin_ComposesWithScalar: the motivation shape —
// "level == 1 AND geometry within 5km of AOI" as one Filter.
func TestExpr_GeomDWithin_ComposesWithScalar(t *testing.T) {
	epsg := int32(geometry.PseudoMercator.EPSG)
	f := levelGeomFrame(t,
		[]any{1.0, 2.0, 1.0},
		[]geometry.Geometry{
			projectedSquare(12, 0, 1), // level=1, 2 units from AOI → KEEP
			projectedSquare(12, 0, 1), // level=2, 2 units → drop (scalar fail)
			projectedSquare(50, 0, 1), // level=1, far → drop (dwithin fail)
		},
		epsg,
	)
	aoi := projectedSquare(0, 0, 10)

	out, err := f.Lazy().Filter(
		Col("level").Eq(Lit(1.0)).
			And(Col("geometry").GeomDWithin(Lit(aoi), 3)),
	).Collect()
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	defer out.Release()
	if got := out.NumRows(); got != 1 {
		t.Errorf("row count = %d, want 1", got)
	}
}

// TestExpr_GeomDWithin_NegativeDistance yields all-false (matches
// the geometry.WithinDistance contract).
func TestExpr_GeomDWithin_NegativeDistance(t *testing.T) {
	epsg := int32(geometry.PseudoMercator.EPSG)
	f := levelGeomFrame(t,
		[]any{1.0, 1.0},
		[]geometry.Geometry{
			projectedSquare(0, 0, 5),
			projectedSquare(5, 0, 5),
		},
		epsg,
	)
	got := evalBool(t, f, Col("geometry").GeomDWithin(Lit(projectedSquare(0, 0, 1)), -1))
	for i, v := range got {
		if v != false {
			t.Errorf("row %d = %v, want false (negative distance)", i, v)
		}
	}
}

// TestExpr_GeomDWithin_ColumnRight exercises the pair-wise per-row
// DWithin path — both operands are geometry columns, not a
// constant. Different code path from the constant-right fast path:
// walks two flatBinary buffers in lockstep, attaches CRS to both
// sides. Regression protection for the flatBinary + CRS-attachment
// plumbing.
func TestExpr_GeomDWithin_ColumnRight(t *testing.T) {
	epsg := int32(geometry.PseudoMercator.EPSG)
	pool := memory.DefaultAllocator

	// Left column: 3 polygons, one null.
	lb := array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary)
	defer lb.Release()
	lb.Append(geometry.WKB(projectedSquare(0, 0, 1))) // near right's row 0
	lb.Append(geometry.WKB(projectedSquare(0, 0, 1))) // far from right's row 1
	lb.AppendNull()                                   // null → null out

	// Right column: pair-wise partner geometries.
	rb := array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary)
	defer rb.Release()
	rb.Append(geometry.WKB(projectedSquare(3, 0, 1)))  // 2 units from left row 0
	rb.Append(geometry.WKB(projectedSquare(50, 0, 1))) // 49 units from left row 1
	rb.Append(geometry.WKB(projectedSquare(0, 0, 1)))  // right non-null but left null → null

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
	defer f.Release()

	// DWithin distance 5 — row 0 pair is 2 units apart (within),
	// row 1 is 49 units apart (not within), row 2 is null (null).
	got := evalBool(t, f, Col("left_geom").GeomDWithin(Col("right_geom"), 5))
	want := []any{true, false, nil}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %v, want %v", i, got[i], want[i])
		}
	}
}

// TestExpr_GeomDWithin_ReflectionSurface — ExprNode contract sanity.
func TestExpr_GeomDWithin_ReflectionSurface(t *testing.T) {
	e := Col("geometry").GeomDWithin(Lit(projectedSquare(0, 0, 1)), 5)
	if len(e.Node().Children()) != 2 {
		t.Errorf("children count = %d, want 2", len(e.Node().Children()))
	}
	dt, err := e.Node().Type(nil)
	if err != nil {
		t.Fatalf("Type: %v", err)
	}
	if dt.String() != "bool" {
		t.Errorf("Type = %s, want bool", dt)
	}
}
