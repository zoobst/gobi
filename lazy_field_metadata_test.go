package gobi

import (
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/zoobst/gobi/geometry"
)

// TestCollect_KeepsComputedFieldMetadata — a geometry column computed
// in a lazy Select / WithColumn keeps its geometry tag (and CRS) in
// Collect's output, although the plan's declared schema, built from
// ExprNode.Type, carries only the data type.
func TestCollect_KeepsComputedFieldMetadata(t *testing.T) {
	b := array.NewBinaryBuilder(memory.DefaultAllocator, arrow.BinaryTypes.Binary)
	b.Append(geometry.WKB(geometry.NewPoint(1, 2, geometry.WGS84)))
	g := SeriesFromArray(GeometryField("g", 4326), b.NewArray())
	b.Release()
	f, err := NewFrameFromSeries(g)
	g.Column().Release()
	if err != nil {
		t.Fatal(err)
	}
	defer f.Release()

	for name, lf := range map[string]*LazyFrame{
		"Select":     f.Lazy().Select(Col("g").GeomForceZ(1)),
		"WithColumn": f.Lazy().WithColumn("z", Col("g").GeomForceZ(1)),
	} {
		out, err := lf.Collect()
		if err != nil {
			t.Fatal(err)
		}
		s, _ := out.Column(out.ColumnNames()[out.NumCols()-1])
		if !s.IsGeometry() || geometryCRSFromField(out.Schema().Field(out.NumCols()-1)) != 4326 {
			t.Errorf("%s: computed geometry column lost its tag: %v", name, out.Schema())
		}
		out.Release()
	}
}

// TestPlanSchema_KeepsGeometryTag — the plan's declared schema
// (LazyFrame.Schema) and a zero-row Collect show computed and passed-
// through geometry columns as geometry, with their CRS.
func TestPlanSchema_KeepsGeometryTag(t *testing.T) {
	b := array.NewBinaryBuilder(memory.DefaultAllocator, arrow.BinaryTypes.Binary)
	b.Append(geometry.WKB(geometry.NewPoint(1, 2, geometry.WGS84)))
	g := SeriesFromArray(GeometryField("g", 3857), b.NewArray())
	b.Release()
	f, err := NewFrameFromSeries(g, NewInt64Series("n", []int64{1}, nil))
	g.Column().Release()
	if err != nil {
		t.Fatal(err)
	}
	defer f.Release()

	lf := f.Lazy().
		WithColumn("z", Col("g").GeomForceZ(1)).
		Select(Col("g"), Col("g").Alias("g2"), Col("z"), Col("n"))
	none := lf.Filter(Col("n").Gt(Lit(int64(5))))
	out, err := none.Collect()
	if err != nil {
		t.Fatal(err)
	}
	defer out.Release()
	if out.NumRows() != 0 {
		t.Fatalf("want zero rows, got %d", out.NumRows())
	}
	for name, sch := range map[string]*arrow.Schema{"Schema()": lf.Schema(), "zero-row Collect": out.Schema()} {
		for i, col := range []string{"g", "g2", "z"} {
			fld := sch.Field(i)
			if fld.Name != col || !isGeometryField(fld) || geometryCRSFromField(fld) != 3857 {
				t.Errorf("%s: field %d = %v, want geometry %q in EPSG:3857", name, i, fld, col)
			}
		}
		if isGeometryField(sch.Field(3)) {
			t.Errorf("%s: n tagged as geometry", name)
		}
	}
}

// TestPassThroughExprs_KeepGeometryTag — Shift, Over, If and Coalesce
// over geometry columns keep the tag at runtime (so a later geometry
// op in the same plan works), in LazyFrame.Schema, and in a zero-row
// result. If / Coalesce skip literal operands; operands in different
// CRSs give an untagged result.
func TestPassThroughExprs_KeepGeometryTag(t *testing.T) {
	pt := func(name string, epsg int32) Series {
		b := array.NewBinaryBuilder(memory.DefaultAllocator, arrow.BinaryTypes.Binary)
		defer b.Release()
		for i := range 4 {
			b.Append(geometry.WKB(geometry.NewPoint(float64(i), 0, geometry.CRS{})))
		}
		return SeriesFromArray(GeometryField(name, epsg), b.NewArray())
	}
	series := []Series{pt("g", 3857), pt("h", 4326),
		NewInt64Series("k", []int64{1, 1, 2, 2}, nil), NewBoolSeries("c", []bool{true, false, true, false}, nil)}
	f, err := NewFrameFromSeries(series...)
	for _, s := range series {
		s.Column().Release()
	}
	if err != nil {
		t.Fatal(err)
	}
	defer f.Release()

	tagged := map[string]Expr{
		"Shift":            Col("g").Shift(1),
		"Over":             Col("g").Shift(1).Over("k"),
		"If":               If(Col("c"), Col("g"), Col("g").Shift(1)),
		"If + LitNull":     If(Col("c"), Col("g"), LitNull(arrow.BinaryTypes.Binary)),
		"Coalesce":         Coalesce(Col("g").Shift(1), Col("g")),
		"Coalesce(ForceZ)": Coalesce(Col("g").GeomForceZ(1), Col("g").GeomForceZ(2)),
	}
	for name, e := range tagged {
		lf := f.Lazy().WithColumn("x", e)
		out, err := lf.WithColumn("y", Col("x").GeomForceZ(1)).Collect()
		if err != nil {
			t.Errorf("%s: downstream geometry op: %v", name, err)
			continue
		}
		x, _ := out.Column("x")
		if !x.IsGeometry() || geometryCRSFromField(out.Schema().Field(4)) != 3857 {
			t.Errorf("%s: runtime column untagged: %v", name, out.Schema().Field(4))
		}
		out.Release()
		empty, err := lf.Filter(Col("k").Gt(Lit(int64(9)))).Collect()
		if err != nil {
			t.Fatal(err)
		}
		for where, sch := range map[string]*arrow.Schema{"Schema()": lf.Schema(), "zero-row": empty.Schema()} {
			if fld := sch.Field(4); !isGeometryField(fld) || geometryCRSFromField(fld) != 3857 {
				t.Errorf("%s: %s field %v untagged", name, where, fld)
			}
		}
		empty.Release()
	}

	// Different CRSs: no shared metadata, so untagged (as before).
	for name, e := range map[string]Expr{
		"If mixed CRS":       If(Col("c"), Col("g"), Col("h")),
		"Coalesce mixed CRS": Coalesce(Col("g"), Col("h")),
	} {
		lf := f.Lazy().WithColumn("x", e)
		out, err := lf.Collect()
		if err != nil {
			t.Fatal(err)
		}
		if x, _ := out.Column("x"); x.IsGeometry() || isGeometryField(lf.Schema().Field(4)) {
			t.Errorf("%s: tagged despite differing CRSs", name)
		}
		out.Release()
	}
}
