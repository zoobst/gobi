package gobi

import (
	"testing"

	"github.com/apache/arrow-go/v18/arrow/array"

	"github.com/zoobst/gobi/geometry"
)

func boolSeriesValues(t *testing.T, s Series) []any {
	t.Helper()
	out := make([]any, 0, s.Len())
	for _, chunk := range s.Column().Data().Chunks() {
		b := chunk.(*array.Boolean)
		for i := range b.Len() {
			if b.IsNull(i) {
				out = append(out, nil)
			} else {
				out = append(out, b.Value(i))
			}
		}
	}
	return out
}

func TestSeries_GeomIntersects(t *testing.T) {
	// Three subject polygons: one overlapping the mask, one disjoint,
	// one null. Mask is the [5,15]×[0,10] rectangle.
	s := geomSeries(t, "geom", int32(geometry.PseudoMercator.EPSG), []geometry.Geometry{
		projectedSquare(0, 0, 10),  // overlaps mask on X=[5,10]
		projectedSquare(50, 50, 5), // disjoint
		nil,
	})
	mask := projectedSquare(5, 0, 10)
	got, err := s.GeomIntersects(mask)
	if err != nil {
		t.Fatalf("GeomIntersects: %v", err)
	}
	if got.Len() != 3 {
		t.Fatalf("Len = %d, want 3", got.Len())
	}
	vals := boolSeriesValues(t, got)
	want := []any{true, false, nil}
	for i := range want {
		if vals[i] != want[i] {
			t.Errorf("row %d = %v, want %v", i, vals[i], want[i])
		}
	}
}

func TestSeries_GeomContains(t *testing.T) {
	// Row 0: big square contains a small mask polygon.
	// Row 1: small square does NOT contain the big mask.
	s := geomSeries(t, "geom", int32(geometry.PseudoMercator.EPSG), []geometry.Geometry{
		projectedSquare(0, 0, 100), // 100×100, easily contains a 5×5
		projectedSquare(0, 0, 5),   // 5×5, smaller than the mask
	})
	mask := projectedSquare(10, 10, 5) // 5×5 square at (10,10)-(15,15)
	got, err := s.GeomContains(mask)
	if err != nil {
		t.Fatalf("GeomContains: %v", err)
	}
	vals := boolSeriesValues(t, got)
	want := []any{true, false}
	for i := range want {
		if vals[i] != want[i] {
			t.Errorf("row %d = %v, want %v", i, vals[i], want[i])
		}
	}
}

func TestSeries_GeomWithin(t *testing.T) {
	// Symmetric of GeomContains: subject small, other large.
	s := geomSeries(t, "geom", int32(geometry.PseudoMercator.EPSG), []geometry.Geometry{
		projectedSquare(10, 10, 5),   // 5×5 inside the mask
		projectedSquare(200, 200, 5), // 5×5 outside the mask
	})
	mask := projectedSquare(0, 0, 100)
	got, err := s.GeomWithin(mask)
	if err != nil {
		t.Fatalf("GeomWithin: %v", err)
	}
	vals := boolSeriesValues(t, got)
	want := []any{true, false}
	for i := range want {
		if vals[i] != want[i] {
			t.Errorf("row %d = %v, want %v", i, vals[i], want[i])
		}
	}
}

func TestSeries_GeomDisjoint(t *testing.T) {
	s := geomSeries(t, "geom", int32(geometry.PseudoMercator.EPSG), []geometry.Geometry{
		projectedSquare(0, 0, 10),     // overlaps mask
		projectedSquare(50, 50, 5),    // disjoint
		projectedSquare(100, 100, 20), // disjoint
	})
	mask := projectedSquare(5, 0, 10)
	got, err := s.GeomDisjoint(mask)
	if err != nil {
		t.Fatalf("GeomDisjoint: %v", err)
	}
	vals := boolSeriesValues(t, got)
	want := []any{false, true, true}
	for i := range want {
		if vals[i] != want[i] {
			t.Errorf("row %d = %v, want %v", i, vals[i], want[i])
		}
	}
}

func TestSeries_GeomIntersects_NotGeometry(t *testing.T) {
	// Feed a plain int64 series through the predicate op — it should
	// error with ErrNotGeometry rather than mis-parse WKB.
	name := "not_geometry"
	s := newSeriesFromArray(name, array.NewInt64Builder(nil).NewArray())
	if _, err := s.GeomIntersects(projectedSquare(0, 0, 1)); err == nil {
		t.Errorf("GeomIntersects on non-geom Series should error")
	}
}
