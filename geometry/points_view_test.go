package geometry

import (
	"math"
	"testing"
)

func TestPointsView_LineString_Basic(t *testing.T) {
	l := LineString{
		Points: []Point{
			{X: 1, Y: 2},
			{X: 3, Y: 4},
			{X: -1, Y: 5},
		},
		CRSValue: WGS84,
	}
	v := l.View()
	if v.Len() != 3 {
		t.Fatalf("Len = %d, want 3", v.Len())
	}
	if v.HasZ {
		t.Errorf("HasZ = true, want false")
	}
	if v.Zs != nil {
		t.Errorf("Zs = %v, want nil for 2D view", v.Zs)
	}
	if !v.CRS.Equal(WGS84) {
		t.Errorf("CRS = %+v, want WGS84", v.CRS)
	}
	if v.Xs[0] != 1 || v.Xs[1] != 3 || v.Xs[2] != -1 {
		t.Errorf("Xs = %v, want [1, 3, -1]", v.Xs)
	}
	if v.Ys[0] != 2 || v.Ys[1] != 4 || v.Ys[2] != 5 {
		t.Errorf("Ys = %v, want [2, 4, 5]", v.Ys)
	}
}

func TestPointsView_LineString_XYZ(t *testing.T) {
	l := LineString{
		Points: []Point{
			{X: 1, Y: 2, Z: 10, HasZ: true},
			{X: 3, Y: 4, Z: 20, HasZ: true},
		},
		CRSValue: WGS84,
		HasZ:     true,
	}
	v := l.View()
	if !v.HasZ {
		t.Fatal("HasZ should be true")
	}
	if len(v.Zs) != 2 {
		t.Fatalf("len(Zs) = %d, want 2", len(v.Zs))
	}
	if v.Zs[0] != 10 || v.Zs[1] != 20 {
		t.Errorf("Zs = %v, want [10, 20]", v.Zs)
	}
}

func TestPointsView_Empty(t *testing.T) {
	l := LineString{CRSValue: WGS84}
	v := l.View()
	if v.Len() != 0 {
		t.Errorf("Len = %d, want 0", v.Len())
	}
	if v.Xs == nil || v.Ys == nil {
		t.Errorf("Xs/Ys should be non-nil (empty view uses zero-length slices, not nil)")
	}
	if b := v.Bounds(); !b.Empty() {
		t.Errorf("Bounds on empty view = %+v, want empty", b)
	}
}

// TestPointsView_MatchesLineStringBounds — the SoA and AoS bounds
// paths must produce identical Bounds for the same input. This is
// the correctness guarantee that lets downstream slices swap between
// them freely.
func TestPointsView_MatchesLineStringBounds(t *testing.T) {
	l := LineString{
		Points: []Point{
			{X: 5, Y: -2},
			{X: -3, Y: 7},
			{X: 12, Y: 4},
			{X: 0, Y: -10},
		},
		CRSValue: WGS84,
	}
	want := l.Bounds()
	got := l.View().Bounds()
	if got != want {
		t.Errorf("SoA bounds %+v != AoS bounds %+v", got, want)
	}
}

func TestPointsView_Polygon_RingViews(t *testing.T) {
	// 5-vertex square exterior + 5-vertex hole.
	p := Polygon{
		Rings: [][]Point{
			{{X: 0, Y: 0}, {X: 10, Y: 0}, {X: 10, Y: 10}, {X: 0, Y: 10}, {X: 0, Y: 0}},
			{{X: 2, Y: 2}, {X: 4, Y: 2}, {X: 4, Y: 4}, {X: 2, Y: 4}, {X: 2, Y: 2}},
		},
		CRSValue: WGS84,
	}
	views := p.RingViews()
	if len(views) != 2 {
		t.Fatalf("got %d ring views, want 2", len(views))
	}
	if views[0].Bounds() != (Bounds{MinX: 0, MinY: 0, MaxX: 10, MaxY: 10}) {
		t.Errorf("exterior bounds = %+v", views[0].Bounds())
	}
	if views[1].Bounds() != (Bounds{MinX: 2, MinY: 2, MaxX: 4, MaxY: 4}) {
		t.Errorf("hole bounds = %+v", views[1].Bounds())
	}
}

func TestPointsView_MultiPoint(t *testing.T) {
	m := MultiPoint{
		Points:   []Point{{X: 1, Y: 1}, {X: 2, Y: 2}, {X: 3, Y: 3}},
		CRSValue: WGS84,
	}
	v := m.View()
	if v.Len() != 3 {
		t.Fatalf("Len = %d, want 3", v.Len())
	}
	if b := v.Bounds(); b != (Bounds{MinX: 1, MinY: 1, MaxX: 3, MaxY: 3}) {
		t.Errorf("Bounds = %+v", b)
	}
}

func TestPointsView_MultiLineString_LineViews(t *testing.T) {
	m := MultiLineString{
		Lines: []LineString{
			{Points: []Point{{X: 0, Y: 0}, {X: 1, Y: 1}}},
			{Points: []Point{{X: 5, Y: 5}, {X: 6, Y: 6}, {X: 7, Y: 5}}},
		},
		CRSValue: WGS84,
	}
	views := m.LineViews()
	if len(views) != 2 {
		t.Fatalf("got %d line views, want 2", len(views))
	}
	if views[0].Len() != 2 || views[1].Len() != 3 {
		t.Errorf("lens = [%d, %d], want [2, 3]", views[0].Len(), views[1].Len())
	}
}

func TestPointsView_MultiPolygon_PolygonRingViews(t *testing.T) {
	square := func(x, y float64) []Point {
		return []Point{{X: x, Y: y}, {X: x + 1, Y: y}, {X: x + 1, Y: y + 1}, {X: x, Y: y + 1}, {X: x, Y: y}}
	}
	m := MultiPolygon{
		Polygons: []Polygon{
			{Rings: [][]Point{square(0, 0)}},
			{Rings: [][]Point{square(10, 10), square(10.2, 10.2)}}, // hole
		},
		CRSValue: WGS84,
	}
	rings := m.PolygonRingViews()
	if len(rings) != 2 {
		t.Fatalf("got %d polygon views, want 2", len(rings))
	}
	if len(rings[0]) != 1 {
		t.Errorf("polygon 0 has %d rings, want 1", len(rings[0]))
	}
	if len(rings[1]) != 2 {
		t.Errorf("polygon 1 has %d rings, want 2", len(rings[1]))
	}
}

// TestBoundsFromXY_MatchesExtendLoop — the SoA kernel produces the
// same bounds as the equivalent AoS Extend-in-loop code path.
func TestBoundsFromXY_MatchesExtendLoop(t *testing.T) {
	xs := []float64{5, -3, 12, 0, 7.5, -8, math.Pi}
	ys := []float64{-2, 7, 4, -10, 3, 6, math.E}

	// Reference: build the bounds via the current AoS Extend loop.
	want := EmptyBounds()
	for i := range xs {
		want = want.Extend(xs[i], ys[i])
	}
	got := BoundsFromXY(xs, ys)
	if got != want {
		t.Errorf("BoundsFromXY = %+v, Extend-loop = %+v", got, want)
	}
}

func TestBoundsFromXY_Empty(t *testing.T) {
	if b := BoundsFromXY(nil, nil); !b.Empty() {
		t.Errorf("nil input: got %+v, want empty", b)
	}
	if b := BoundsFromXY([]float64{}, []float64{}); !b.Empty() {
		t.Errorf("empty input: got %+v, want empty", b)
	}
}

// TestBoundsFromXY_LengthMismatch — mismatched-length inputs are
// caller error, but the kernel must not panic. Documented behavior:
// derive from the shorter slice.
func TestBoundsFromXY_LengthMismatch(t *testing.T) {
	xs := []float64{1, 2, 3, 4}
	ys := []float64{1, 2}
	got := BoundsFromXY(xs, ys)
	want := Bounds{MinX: 1, MinY: 1, MaxX: 2, MaxY: 2}
	if got != want {
		t.Errorf("got %+v, want %+v (derived from shorter slice)", got, want)
	}
}

// TestBoundsFromXY_NaN — locks in the NaN semantics of the SoA
// scalar reduce: every comparison against NaN is false, so
// isolated NaN coords are silently ignored (they neither narrow
// nor poison the bounds). If a future refactor swaps in
// compute.BoundsF64 with the SIMD Min/Max reduce, NaN in the
// first-lane slot would propagate — tests in compute/geom_test.go
// lock that separate semantic in. This test covers the current
// entry point that geometry callers see.
func TestBoundsFromXY_NaN(t *testing.T) {
	// Mixed real + NaN with the FIRST coord finite (so the
	// running min/max is initialized from a real number).
	xs := []float64{0, math.NaN(), 10, math.NaN(), 2}
	ys := []float64{0, math.NaN(), 5, 3, math.NaN()}
	b := BoundsFromXY(xs, ys)
	if b.MinX != 0 || b.MaxX != 10 || b.MinY != 0 || b.MaxY != 5 {
		t.Errorf("mixed-NaN bounds = %+v, want {0,0,10,5}", b)
	}
	if math.IsNaN(b.MinX) || math.IsNaN(b.MinY) ||
		math.IsNaN(b.MaxX) || math.IsNaN(b.MaxY) {
		t.Errorf("bounds contain NaN: %+v", b)
	}

	// All-NaN input keeps the bounds anchored on xs[0]/ys[0] =
	// NaN; the return-value equality contract is: MinX=NaN,
	// MaxX=NaN. NaN != NaN, so Empty() is *not* well-defined
	// here — callers dealing with poisoned inputs must guard
	// upstream. Test only that we didn't crash and that the
	// result is not silently a valid bbox.
	all := []float64{math.NaN(), math.NaN(), math.NaN()}
	nan := BoundsFromXY(all, all)
	if !math.IsNaN(nan.MinX) || !math.IsNaN(nan.MaxX) {
		t.Errorf("all-NaN produced finite MinX/MaxX: %+v", nan)
	}
}
