package geometry

import (
	"errors"
	"math/rand"
	"testing"
)

// TestLineStringViewFromWKB_MatchesAoS — direct-parse view must
// match ParseWKB + .View() exactly (both XY and XYZ).
func TestLineStringViewFromWKB_MatchesAoS(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	cases := []struct {
		name string
		hasZ bool
		n    int
	}{
		{"xy-empty", false, 0},
		{"xy-2pts", false, 2},
		{"xy-10pts", false, 10},
		{"xy-1000pts", false, 1000},
		{"xyz-2pts", true, 2},
		{"xyz-10pts", true, 10},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pts := make([]Point, c.n)
			for i := range pts {
				pts[i] = Point{X: rng.Float64(), Y: rng.Float64()}
				if c.hasZ {
					pts[i].Z = rng.Float64()
					pts[i].HasZ = true
				}
			}
			ls := LineString{Points: pts, HasZ: c.hasZ}
			data := WKB(ls)

			gotView, err := LineStringViewFromWKB(data)
			if err != nil {
				t.Fatalf("LineStringViewFromWKB: %v", err)
			}
			wantView := ls.View()
			if !equalPointsView(gotView, wantView) {
				t.Errorf("view mismatch\n got: %+v\nwant: %+v", gotView, wantView)
			}
		})
	}
}

// TestPolygonRingViewsFromWKB_MatchesAoS — direct-parse must match
// ParseWKB(data).RingViews() exactly.
func TestPolygonRingViewsFromWKB_MatchesAoS(t *testing.T) {
	poly := Polygon{Rings: [][]Point{
		// Exterior L-shape.
		{{X: 0, Y: 0}, {X: 10, Y: 0}, {X: 10, Y: 3}, {X: 5, Y: 3},
			{X: 5, Y: 7}, {X: 10, Y: 7}, {X: 10, Y: 10}, {X: 0, Y: 10}, {X: 0, Y: 0}},
		// Hole.
		{{X: 1, Y: 1}, {X: 2, Y: 1}, {X: 2, Y: 2}, {X: 1, Y: 2}, {X: 1, Y: 1}},
	}}
	data := WKB(poly)

	got, err := PolygonRingViewsFromWKB(data)
	if err != nil {
		t.Fatalf("PolygonRingViewsFromWKB: %v", err)
	}
	want := poly.RingViews()
	if len(got) != len(want) {
		t.Fatalf("ring count got=%d want=%d", len(got), len(want))
	}
	for i := range got {
		if !equalPointsView(got[i], want[i]) {
			t.Errorf("ring %d mismatch\n got: %+v\nwant: %+v", i, got[i], want[i])
		}
	}
}

// TestMultiPolygonRingViewsFromWKB_MatchesAoS — direct-parse must
// match ParseWKB(data).PolygonRingViews() exactly.
func TestMultiPolygonRingViewsFromWKB_MatchesAoS(t *testing.T) {
	m := MultiPolygon{Polygons: []Polygon{
		{Rings: [][]Point{{
			{X: 0, Y: 0}, {X: 5, Y: 0}, {X: 5, Y: 5}, {X: 0, Y: 5}, {X: 0, Y: 0},
		}}},
		{Rings: [][]Point{
			{{X: 10, Y: 10}, {X: 15, Y: 10}, {X: 15, Y: 15}, {X: 10, Y: 15}, {X: 10, Y: 10}},
			{{X: 11, Y: 11}, {X: 12, Y: 11}, {X: 12, Y: 12}, {X: 11, Y: 12}, {X: 11, Y: 11}},
		}},
	}}
	data := WKB(m)

	got, err := MultiPolygonRingViewsFromWKB(data)
	if err != nil {
		t.Fatalf("MultiPolygonRingViewsFromWKB: %v", err)
	}
	want := m.PolygonRingViews()
	if len(got) != len(want) {
		t.Fatalf("poly count got=%d want=%d", len(got), len(want))
	}
	for i := range got {
		if len(got[i]) != len(want[i]) {
			t.Fatalf("poly %d ring count got=%d want=%d", i, len(got[i]), len(want[i]))
		}
		for j := range got[i] {
			if !equalPointsView(got[i][j], want[i][j]) {
				t.Errorf("poly %d ring %d mismatch\n got: %+v\nwant: %+v",
					i, j, got[i][j], want[i][j])
			}
		}
	}
}

// TestPrepareFromWKB_MatchesPrepare — for Polygon / MultiPolygon
// inputs the WKB-direct prepared geometry must produce the same
// predicate answers as Prepare(ParseWKB(data)). Fuzz across many
// query points to catch any divergence.
func TestPrepareFromWKB_MatchesPrepare(t *testing.T) {
	poly := Polygon{Rings: [][]Point{
		{{X: 0, Y: 0}, {X: 10, Y: 0}, {X: 10, Y: 3}, {X: 5, Y: 3},
			{X: 5, Y: 7}, {X: 10, Y: 7}, {X: 10, Y: 10}, {X: 0, Y: 10}, {X: 0, Y: 0}},
		{{X: 1, Y: 1}, {X: 2, Y: 1}, {X: 2, Y: 2}, {X: 1, Y: 2}, {X: 1, Y: 1}},
	}}
	data := WKB(poly)

	pFromWKB, err := PrepareFromWKB(data)
	if err != nil {
		t.Fatalf("PrepareFromWKB: %v", err)
	}
	pAoS := Prepare(poly)

	rng := rand.New(rand.NewSource(2))
	for range 200 {
		pt := Point{X: rng.Float64()*14 - 2, Y: rng.Float64()*14 - 2}
		pPt := Prepare(pt)
		for _, pred := range []Predicate{PredIntersects, PredContains, PredWithin, PredDisjoint} {
			got := TestPrepared(pred, pPt, pFromWKB)
			want := TestPrepared(pred, pPt, pAoS)
			if got != want {
				t.Errorf("%s @ (%v, %v): from-WKB=%v vs from-Prepare=%v",
					pred, pt.X, pt.Y, got, want)
			}
		}
	}
}

// TestPrepareFromWKB_NonPolygonFallsThrough — a Point / LineString
// WKB blob decodes via ParseWKB and returns a PreparedGeometry
// with no cached slabs (matches Prepare's contract for those
// types).
func TestPrepareFromWKB_NonPolygonFallsThrough(t *testing.T) {
	ls := LineString{Points: []Point{{X: 0, Y: 0}, {X: 1, Y: 1}}}
	data := WKB(ls)
	p, err := PrepareFromWKB(data)
	if err != nil {
		t.Fatalf("PrepareFromWKB: %v", err)
	}
	if p.polyRings != nil {
		t.Errorf("polyRings should be nil for LineString, got %+v", p.polyRings)
	}
	if p.multiPolyRings != nil {
		t.Errorf("multiPolyRings should be nil for LineString")
	}
	if _, ok := p.G.(LineString); !ok {
		t.Errorf("G should be LineString, got %T", p.G)
	}
}

// TestLineStringViewFromWKB_WrongType — type-code mismatch returns
// ErrTypeMismatch rather than misparsing.
func TestLineStringViewFromWKB_WrongType(t *testing.T) {
	data := WKB(Point{X: 5, Y: 5})
	_, err := LineStringViewFromWKB(data)
	if !errors.Is(err, ErrTypeMismatch) {
		t.Errorf("got %v, want ErrTypeMismatch", err)
	}
}

// equalPointsView compares two views structurally — Xs, Ys, Zs,
// HasZ. CRS isn't checked because the WKB scanners don't attach it.
func equalPointsView(a, b PointsView) bool {
	if a.HasZ != b.HasZ || len(a.Xs) != len(b.Xs) || len(a.Ys) != len(b.Ys) {
		return false
	}
	for i := range a.Xs {
		if a.Xs[i] != b.Xs[i] || a.Ys[i] != b.Ys[i] {
			return false
		}
	}
	if a.HasZ {
		if len(a.Zs) != len(b.Zs) {
			return false
		}
		for i := range a.Zs {
			if a.Zs[i] != b.Zs[i] {
				return false
			}
		}
	}
	return true
}
