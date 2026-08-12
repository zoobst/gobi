package gobi

import (
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/zoobst/gobi/geometry"
)

// TestFrame_SortByHilbert_TightRowGroupBboxes: an unsorted grid of
// polygons ends up with a wide row-group bbox if you naively chunk
// it in insertion order; sorting by Hilbert first collapses each
// row-group bbox to a spatially-tight cluster. This is the whole
// point of spatial sorting, so we verify it directly.
//
// Corpus: 100 polygons on a 10×10 grid at (i, j). Row-group size
// 10. Without sorting, each row-group has bbox spanning many cells
// (because insertion order zig-zags). With Hilbert sort, each
// row-group's bbox stays local.
func TestFrame_SortByHilbert_TightRowGroupBboxes(t *testing.T) {
	pool := memory.DefaultAllocator
	const gridSize = 10
	n := gridSize * gridSize

	geomB := array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary)
	defer geomB.Release()
	// Insertion order: bottom-to-top by column, then across columns.
	// Deliberately non-spatial so a naive chunking of 10 rows gives
	// wide bboxes.
	for i := range n {
		col := i / gridSize
		row := i % gridSize
		// Offset a bit so no two polygons share coords (breaks ties
		// deterministically for the stable sort).
		x := float64(col*10) + 0.1
		y := float64(row*10) + 0.1
		poly := geometry.SimplePolygon([]geometry.Point{
			{X: x, Y: y},
			{X: x + 1, Y: y},
			{X: x + 1, Y: y + 1},
			{X: x, Y: y + 1},
			{X: x, Y: y},
		}, geometry.PseudoMercator)
		geomB.Append(geometry.WKB(poly))
	}

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

	sorted, err := f.SortByHilbert("geometry")
	if err != nil {
		t.Fatalf("SortByHilbert: %v", err)
	}
	defer sorted.Release()

	// For each chunk of 10 rows in the sorted output, compute the
	// bbox diagonal. Compare to the unsorted diagonal — the sorted
	// version should be substantially smaller.
	unsortedDiag := chunkedDiagonal(t, f, 10)
	sortedDiag := chunkedDiagonal(t, sorted, 10)

	// Not a tight bound — depends on grid layout — but sorted
	// should be at least half the unsorted diagonal.
	if sortedDiag*2 > unsortedDiag {
		t.Errorf("Hilbert sort didn't tighten row-group bboxes enough: sorted=%.2f unsorted=%.2f (expected sorted*2 < unsorted)",
			sortedDiag, unsortedDiag)
	}
}

// chunkedDiagonal returns the average bbox-diagonal length over
// fixed-size row-group chunks. Larger → looser spatial locality.
func chunkedDiagonal(t *testing.T, f *Frame, groupSize int) float64 {
	t.Helper()
	col, err := f.Column("geometry")
	if err != nil {
		t.Fatal(err)
	}
	n := f.NumRows()
	var total float64
	var groups int
	for start := 0; start < n; start += groupSize {
		end := min(start+groupSize, n)
		b := geometry.EmptyBounds()
		idx := 0
		for _, chunk := range col.Column().Data().Chunks() {
			bin := chunk.(*array.Binary)
			for i := range bin.Len() {
				if idx >= end {
					break
				}
				if idx >= start && !bin.IsNull(i) {
					g, err := geometry.ParseWKB(bin.Value(i))
					if err != nil {
						t.Fatal(err)
					}
					gb := g.Bounds()
					b = b.Union(gb)
				}
				idx++
			}
		}
		if !b.Empty() {
			dx := b.MaxX - b.MinX
			dy := b.MaxY - b.MinY
			total += dx*dx + dy*dy // squared diagonal
			groups++
		}
	}
	if groups == 0 {
		return 0
	}
	return total / float64(groups)
}

// TestFrame_SortByHilbertWith_SharedReferenceFrame: two partitions
// of the same overall dataset, sorted independently with the SAME
// caller-supplied bounds, should produce Hilbert indices that live
// on the same 1D curve. Concretely, if we split a corpus into
// "left half" and "right half" and sort each with the FULL corpus's
// bbox, the two outputs concatenated form a spatially-coherent
// sequence — this is the primitive that multi-file / multi-partition
// pipelines need for cross-file locality.
func TestFrame_SortByHilbertWith_SharedReferenceFrame(t *testing.T) {
	// Build two frames: leftHalf covers x in [0, 500], rightHalf
	// covers x in [500, 1000]. Both share y in [0, 1000].
	sharedBounds := geometry.Bounds{MinX: 0, MinY: 0, MaxX: 1000, MaxY: 1000}
	left := gridFrame(t, 0, 500, 0, 500, 25)
	defer left.Release()
	right := gridFrame(t, 500, 1000, 0, 500, 25)
	defer right.Release()

	// Sort each with the SHARED reference frame.
	leftSorted, err := left.SortByHilbertWith("geometry",
		HilbertSortOptions{Bounds: sharedBounds})
	if err != nil {
		t.Fatal(err)
	}
	defer leftSorted.Release()
	rightSorted, err := right.SortByHilbertWith("geometry",
		HilbertSortOptions{Bounds: sharedBounds})
	if err != nil {
		t.Fatal(err)
	}
	defer rightSorted.Release()

	// Each partition's first row's centroid should sit at a smaller
	// Hilbert index (in the shared frame) than its last row's — the
	// sort worked. We check by re-computing HilbertIndex on the
	// centroids of the first and last row of each partition and
	// verifying the ordering holds.
	firstIdx := hilbertOfRow(t, leftSorted, 0, sharedBounds)
	lastIdx := hilbertOfRow(t, leftSorted, leftSorted.NumRows()-1, sharedBounds)
	if firstIdx > lastIdx {
		t.Errorf("left partition not sorted ascending: first=%d last=%d", firstIdx, lastIdx)
	}
	firstIdx = hilbertOfRow(t, rightSorted, 0, sharedBounds)
	lastIdx = hilbertOfRow(t, rightSorted, rightSorted.NumRows()-1, sharedBounds)
	if firstIdx > lastIdx {
		t.Errorf("right partition not sorted ascending: first=%d last=%d", firstIdx, lastIdx)
	}
}

// gridFrame builds a Frame of size×size polygons on an axis-aligned
// grid over [xMin..xMax] × [yMin..yMax]. Rows in insertion order
// (deliberately non-spatial to exercise the sort).
func gridFrame(t *testing.T, xMin, xMax, yMin, yMax float64, size int) *Frame {
	t.Helper()
	pool := memory.DefaultAllocator
	geomB := array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary)
	defer geomB.Release()
	dx := (xMax - xMin) / float64(size)
	dy := (yMax - yMin) / float64(size)
	for i := range size {
		for j := range size {
			x := xMin + float64(i)*dx
			y := yMin + float64(j)*dy
			poly := geometry.SimplePolygon([]geometry.Point{
				{X: x, Y: y}, {X: x + 1, Y: y}, {X: x + 1, Y: y + 1},
				{X: x, Y: y + 1}, {X: x, Y: y},
			}, geometry.PseudoMercator)
			geomB.Append(geometry.WKB(poly))
		}
	}
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
	return f
}

// hilbertOfRow returns the Hilbert index of the row-i centroid,
// computed in the given reference bounds. Used by the shared-frame
// test to check that a partition sort produces monotonically
// increasing indices when measured against the same frame every
// caller uses.
func hilbertOfRow(t *testing.T, f *Frame, rowIdx int, bounds geometry.Bounds) uint64 {
	t.Helper()
	col, err := f.Column("geometry")
	if err != nil {
		t.Fatal(err)
	}
	idx := 0
	for _, chunk := range col.Column().Data().Chunks() {
		bin := chunk.(*array.Binary)
		for i := range bin.Len() {
			if idx == rowIdx {
				g, err := geometry.ParseWKB(bin.Value(i))
				if err != nil {
					t.Fatal(err)
				}
				c := g.Centroid()
				return geometry.HilbertIndex(c.X, c.Y, bounds, geometry.DefaultHilbertOrder)
			}
			idx++
		}
	}
	t.Fatalf("row %d out of range", rowIdx)
	return 0
}

// TestFrame_SortByHilbertWith_OrderTakesEffect: a caller-supplied
// non-default Order should produce a sort permutation distinct from
// the default. Doesn't assert WHICH permutation is right (Hilbert
// is a deterministic function of order+bounds), only that changing
// the order changes the output — regression protection for a bug
// where Order silently gets ignored.
func TestFrame_SortByHilbertWith_OrderTakesEffect(t *testing.T) {
	sharedBounds := geometry.Bounds{MinX: 0, MinY: 0, MaxX: 1000, MaxY: 1000}
	f := gridFrame(t, 0, 1000, 0, 1000, 20)
	defer f.Release()

	// Sort with default (order = 16) and with a very coarse order (2:
	// only 4 cells per axis, so lots of ties broken by the stable sort).
	defaultSort, err := f.SortByHilbertWith("geometry",
		HilbertSortOptions{Bounds: sharedBounds})
	if err != nil {
		t.Fatal(err)
	}
	defer defaultSort.Release()
	coarseSort, err := f.SortByHilbertWith("geometry",
		HilbertSortOptions{Bounds: sharedBounds, Order: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer coarseSort.Release()

	// Extract per-row centroid X for both — different orderings
	// should produce different X sequences (with a coarse-enough
	// order, ties dominate and produce a visibly different pattern).
	defaultXs := centroidXSequence(t, defaultSort)
	coarseXs := centroidXSequence(t, coarseSort)
	same := true
	for i := range defaultXs {
		if defaultXs[i] != coarseXs[i] {
			same = false
			break
		}
	}
	if same {
		t.Errorf("SortByHilbertWith Order=2 produced identical ordering to default (order=16) — Order parameter is being ignored")
	}
}

// centroidXSequence returns the X-coordinate of each row's centroid,
// in row order. Used by the Order-parameter test to compare two
// permutations without deep-equal on the full row payload.
func centroidXSequence(t *testing.T, f *Frame) []float64 {
	t.Helper()
	col, err := f.Column("geometry")
	if err != nil {
		t.Fatal(err)
	}
	xs := make([]float64, 0, f.NumRows())
	for _, chunk := range col.Column().Data().Chunks() {
		bin := chunk.(*array.Binary)
		for i := range bin.Len() {
			if bin.IsNull(i) {
				xs = append(xs, 0)
				continue
			}
			g, err := geometry.ParseWKB(bin.Value(i))
			if err != nil {
				t.Fatal(err)
			}
			xs = append(xs, g.Centroid().X)
		}
	}
	return xs
}

// TestFrame_SortByHilbert_NullsLast: null-geometry rows should sort
// to the end so downstream chunking can put them in their own
// row-group (or drop them via Head).
func TestFrame_SortByHilbert_NullsLast(t *testing.T) {
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

	sorted, err := f.SortByHilbert("geometry")
	if err != nil {
		t.Fatalf("SortByHilbert: %v", err)
	}
	defer sorted.Release()

	geomOut, _ := sorted.Column("geometry")
	bin := geomOut.Column().Data().Chunks()[0].(*array.Binary)
	// Last row must be the null.
	if !bin.IsNull(bin.Len() - 1) {
		t.Errorf("expected null row at last position, got non-null")
	}
	// First two rows must be non-null.
	if bin.IsNull(0) || bin.IsNull(1) {
		t.Errorf("non-null rows not at front of sorted output")
	}
}

// TestFrame_SortByHilbert_NonGeometryColumnErrors: sanity check.
func TestFrame_SortByHilbert_NonGeometryColumnErrors(t *testing.T) {
	pool := memory.DefaultAllocator
	strB := array.NewStringBuilder(pool)
	defer strB.Release()
	strB.Append("a")
	arr := strB.NewArray()
	defer arr.Release()
	field := arrow.Field{Name: "name", Type: arrow.BinaryTypes.String, Nullable: false}
	chunked := arrow.NewChunked(field.Type, []arrow.Array{arr})
	col := arrow.NewColumn(field, chunked)
	chunked.Release()
	schema := arrow.NewSchema([]arrow.Field{field}, nil)
	f, err := NewFrame(schema, []arrow.Column{*col})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Release()
	if _, err := f.SortByHilbert("name"); err == nil {
		t.Errorf("SortByHilbert on non-geometry column should error")
	}
}

// TestFrame_SortByHilbert_EmptyFrame: no rows → returns a Frame
// with the same schema and zero rows, no error.
func TestFrame_SortByHilbert_EmptyFrame(t *testing.T) {
	pool := memory.DefaultAllocator
	geomB := array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary)
	defer geomB.Release()
	arr := geomB.NewArray()
	defer arr.Release()
	field := GeometryField("geometry", 3857)
	chunked := arrow.NewChunked(field.Type, []arrow.Array{arr})
	col := arrow.NewColumn(field, chunked)
	chunked.Release()
	schema := arrow.NewSchema([]arrow.Field{field}, nil)
	f, err := NewFrame(schema, []arrow.Column{*col})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Release()
	sorted, err := f.SortByHilbert("geometry")
	if err != nil {
		t.Fatalf("SortByHilbert on empty: %v", err)
	}
	defer sorted.Release()
	if sorted.NumRows() != 0 {
		t.Errorf("expected 0 rows, got %d", sorted.NumRows())
	}
}
