package shpio

import (
	"math/rand/v2"
	"testing"

	"github.com/zoobst/gobi/geometry"
)

// A shapefile tells shells from holes by ring winding, so ringIsCW
// has to be right for small rings far from the origin. Summed over
// raw coordinates it wasn't: at UTM-sized coordinates the winding of
// a sliver a few cm² across came out wrong about one time in three,
// and a shell read that way becomes a hole.
func TestRingIsCW_SmallRingFarFromOrigin(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 1))
	for range 5000 {
		x0, y0 := 5e5+r.Float64()*1e3, 4.5e6+r.Float64()*1e3
		w, h := 0.001+r.Float64()*0.05, 0.001+r.Float64()*0.01
		// Counter-clockwise: right along the base, then up to the apex.
		ccw := []geometry.Point{
			{X: x0, Y: y0}, {X: x0 + w, Y: y0 + r.Float64()*h/2}, {X: x0 + w/2, Y: y0 + h}, {X: x0, Y: y0},
		}
		cw := []geometry.Point{ccw[3], ccw[2], ccw[1], ccw[0]}
		if ringIsCW(ccw) {
			t.Fatalf("counter-clockwise sliver read as clockwise: %v", ccw)
		}
		if !ringIsCW(cw) {
			t.Fatalf("clockwise sliver read as counter-clockwise: %v", cw)
		}
	}
	// Degenerate rings aren't clockwise.
	if ringIsCW(nil) || ringIsCW([]geometry.Point{{X: 1, Y: 1}, {X: 2, Y: 2}}) {
		t.Error("a ring of fewer than three points read as clockwise")
	}
}
