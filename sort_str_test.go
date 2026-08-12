package gobi

import (
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/zoobst/gobi/geometry"
)

// TestFrame_SortBySTR_TightRowGroupBboxes: same invariant as the
// Hilbert-sort tightness test — STR should also collapse row-group
// bboxes on a shuffled grid.
func TestFrame_SortBySTR_TightRowGroupBboxes(t *testing.T) {
	// Grid over [0..2500] × [0..2500]. Insertion order is
	// column-major (spatially incoherent for row-major row-groups).
	f := gridFrame(t, 0, 2500, 0, 2500, 50) // 50*50 = 2500 polygons
	defer f.Release()

	sorted, err := f.SortBySTR("geometry", 50)
	if err != nil {
		t.Fatalf("SortBySTR: %v", err)
	}
	defer sorted.Release()

	unsorted := chunkedDiagonal(t, f, 50)
	sortedD := chunkedDiagonal(t, sorted, 50)
	if sortedD*2 > unsorted {
		t.Errorf("STR sort didn't tighten row-group bboxes enough: sorted=%.2f unsorted=%.2f",
			sortedD, unsorted)
	}
}

// TestFrame_SortBySTR_NullsLast — same contract as Hilbert.
func TestFrame_SortBySTR_NullsLast(t *testing.T) {
	pool := memory.DefaultAllocator
	geomB := array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary)
	defer geomB.Release()
	geomB.Append(geometry.WKB(geometry.SimplePolygon([]geometry.Point{
		{X: 5, Y: 5}, {X: 6, Y: 5}, {X: 6, Y: 6}, {X: 5, Y: 6}, {X: 5, Y: 5},
	}, geometry.PseudoMercator)))
	geomB.AppendNull()
	geomB.Append(geometry.WKB(geometry.SimplePolygon([]geometry.Point{
		{X: 0, Y: 0}, {X: 1, Y: 0}, {X: 1, Y: 1}, {X: 0, Y: 1}, {X: 0, Y: 0},
	}, geometry.PseudoMercator)))

	field := GeometryField("geometry", int32(geometry.PseudoMercator.EPSG))
	arr := geomB.NewArray()
	defer arr.Release()
	chunked := arrow.NewChunked(field.Type, []arrow.Array{arr})
	col := arrow.NewColumn(field, chunked)
	chunked.Release()
	schema := arrow.NewSchema([]arrow.Field{field}, nil)
	f, err := NewFrame(schema, []arrow.Column{*col})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Release()

	sorted, err := f.SortBySTR("geometry", 10)
	if err != nil {
		t.Fatalf("SortBySTR: %v", err)
	}
	defer sorted.Release()

	geomOut, _ := sorted.Column("geometry")
	bin := geomOut.Column().Data().Chunks()[0].(*array.Binary)
	if !bin.IsNull(bin.Len() - 1) {
		t.Errorf("expected null row at last position, got non-null")
	}
}

// TestFrame_SortBySTR_DefaultLeafSize: leafSize <= 0 falls back
// to STRDefaultLeafSize. With N << default (25 << 5000), the STR
// algorithm degenerates to a single strip → sorted purely by X.
// Assert that ordering to prove the leafSize path did the right
// thing rather than just checking row count.
func TestFrame_SortBySTR_DefaultLeafSize(t *testing.T) {
	f := gridFrame(t, 0, 100, 0, 100, 5) // 5×5 = 25 polygons
	defer f.Release()
	// leafSize=0 → default (5000, bigger than N → single strip).
	sorted, err := f.SortBySTR("geometry", 0)
	if err != nil {
		t.Fatalf("SortBySTR: %v", err)
	}
	defer sorted.Release()
	if sorted.NumRows() != f.NumRows() {
		t.Fatalf("row count changed: sorted=%d original=%d",
			sorted.NumRows(), f.NumRows())
	}
	// Single-strip STR: pass 1 sorts the whole set by X into one
	// strip, then pass 2 sorts within that strip by Y. Net effect:
	// the output is Y-monotone (not X-monotone — the within-strip
	// Y-sort clobbers pass 1's X order). Asserting Y-monotonicity
	// proves pass 2 ran on the whole set as a single strip, which
	// is the leafSize > N branch's whole contract.
	ys := centroidYSequence(t, sorted)
	for i := 1; i < len(ys); i++ {
		if ys[i] < ys[i-1] {
			t.Errorf("single-strip STR not Y-monotone at row %d: %v -> %v",
				i, ys[i-1], ys[i])
			break
		}
	}
}

// centroidYSequence returns the Y-coordinate of each row's centroid,
// in row order. Companion to centroidXSequence in sort_hilbert_test.
func centroidYSequence(t *testing.T, f *Frame) []float64 {
	t.Helper()
	col, err := f.Column("geometry")
	if err != nil {
		t.Fatal(err)
	}
	ys := make([]float64, 0, f.NumRows())
	for _, chunk := range col.Column().Data().Chunks() {
		bin := chunk.(*array.Binary)
		for i := range bin.Len() {
			if bin.IsNull(i) {
				ys = append(ys, 0)
				continue
			}
			g, err := geometry.ParseWKB(bin.Value(i))
			if err != nil {
				t.Fatal(err)
			}
			ys = append(ys, g.Centroid().Y)
		}
	}
	return ys
}

// TestFrame_SortBySTR_MissingColumnErrors: a bogus column name
// surfaces an error rather than panicking. The gridFrame fixture
// carries only a geometry column, so a "not a geometry column"
// case would need a different fixture — covered separately if we
// ever need it.
func TestFrame_SortBySTR_MissingColumnErrors(t *testing.T) {
	f := gridFrame(t, 0, 10, 0, 10, 3)
	defer f.Release()
	if _, err := f.SortBySTR("nonexistent", 10); err == nil {
		t.Errorf("expected error for missing column")
	}
}
