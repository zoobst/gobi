package gobi

import (
	"math"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/zoobst/gobi/geometry"
)

// TestSeriesGeomUnion_BboxDisjointFastPath — Slice 21a union
// bbox-disjoint fast path. Rows whose bboxes don't overlap the
// mask should return a MultiPolygon combining both. Rows that
// overlap the mask fall through to the AoS sweep. Both paths
// must produce the same area as the AoS-only reference.
func TestSeriesGeomUnion_BboxDisjointFastPath(t *testing.T) {
	// 3 rows: one disjoint, one overlapping, one fully contained.
	disjoint := projectedSquare(0, 0, 5)
	overlap := projectedSquare(20, 20, 15)
	contained := projectedSquare(25, 25, 5)
	other := projectedSquare(20, 20, 10)

	f := makeGeomFrame(t, disjoint, overlap, contained)
	geomCol, _ := f.Column("geometry")

	got, err := geomCol.GeomUnion(other)
	if err != nil {
		t.Fatal(err)
	}
	// AoS-only oracle: force sweep by clearing the fast path via a
	// helper that skips the disjoint check.
	want, err := legacyGeomBinaryOp(geomCol, other, geometry.OpUnion, "_union")
	if err != nil {
		t.Fatal(err)
	}
	compareGeomSeriesAreas(t, got, want, "GeomUnion")
}

// TestSeriesGeomSymDifference_BboxDisjointFastPath — Slice 21b.
// Same shape as Union — a △ b when disjoint = a ∪ b.
func TestSeriesGeomSymDifference_BboxDisjointFastPath(t *testing.T) {
	disjoint := projectedSquare(0, 0, 5)
	overlap := projectedSquare(20, 20, 15)
	other := projectedSquare(20, 20, 10)

	f := makeGeomFrame(t, disjoint, overlap)
	geomCol, _ := f.Column("geometry")

	got, err := geomCol.GeomSymDifference(other)
	if err != nil {
		t.Fatal(err)
	}
	want, err := legacyGeomBinaryOp(geomCol, other, geometry.OpSymDifference, "_symdiff")
	if err != nil {
		t.Fatal(err)
	}
	compareGeomSeriesAreas(t, got, want, "GeomSymDifference")
}

func makeGeomFrame(t testing.TB, polys ...geometry.Polygon) *Frame {
	t.Helper()
	pool := memory.DefaultAllocator
	gb := array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary)
	defer gb.Release()
	for _, p := range polys {
		gb.Append(geometry.WKB(p))
	}
	fields := []arrow.Field{
		GeometryField("geometry", int32(geometry.PseudoMercator.EPSG)),
	}
	schema := arrow.NewSchema(fields, nil)
	arr := gb.NewArray()
	defer arr.Release()
	chunked := arrow.NewChunked(arr.DataType(), []arrow.Array{arr})
	col := *arrow.NewColumn(fields[0], chunked)
	chunked.Release()
	f, err := NewFrame(schema, []arrow.Column{col})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func compareGeomSeriesAreas(t *testing.T, got, want Series, label string) {
	t.Helper()
	// Each row is a WKB geometry — decode both and compare areas.
	gotAreas := seriesRowAreas(t, got)
	wantAreas := seriesRowAreas(t, want)
	if len(gotAreas) != len(wantAreas) {
		t.Fatalf("%s: row count mismatch: got=%d want=%d", label, len(gotAreas), len(wantAreas))
	}
	for i := range gotAreas {
		if math.Abs(gotAreas[i]-wantAreas[i]) > 1e-6*math.Max(1, math.Abs(wantAreas[i])) {
			t.Errorf("%s row %d: got area=%v want area=%v", label, i, gotAreas[i], wantAreas[i])
		}
	}
}

func seriesRowAreas(t *testing.T, s Series) []float64 {
	t.Helper()
	var out []float64
	for _, chunk := range s.col.Data().Chunks() {
		bin := chunk.(*array.Binary)
		for i := range bin.Len() {
			if bin.IsNull(i) {
				out = append(out, 0)
				continue
			}
			g, err := geometry.ParseWKB(bin.Value(i))
			if err != nil {
				t.Fatal(err)
			}
			switch v := g.(type) {
			case geometry.Polygon:
				out = append(out, planarPolygonArea(v))
			case geometry.MultiPolygon:
				var a float64
				for _, p := range v.Polygons {
					a += planarPolygonArea(p)
				}
				out = append(out, a)
			default:
				out = append(out, 0)
			}
		}
	}
	return out
}

func planarPolygonArea(p geometry.Polygon) float64 {
	if len(p.Rings) == 0 {
		return 0
	}
	a := geometry.PlanarRingArea(p.Rings[0])
	for _, h := range p.Rings[1:] {
		a -= geometry.PlanarRingArea(h)
	}
	if a < 0 {
		a = -a
	}
	return a
}
