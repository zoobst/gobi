package geometry

import (
	"testing"
)

// TestHilbertIndex_CornersAndCenter: the four corners of a unit
// square should map to distinct indices, and (0, 0) always maps to
// 0 by the Hilbert-curve definition.
func TestHilbertIndex_CornersAndCenter(t *testing.T) {
	b := Bounds{MinX: 0, MinY: 0, MaxX: 1, MaxY: 1}
	idx := func(x, y float64) uint64 { return HilbertIndex(x, y, b, 4) }

	// (0, 0) always at position 0.
	if got := idx(0, 0); got != 0 {
		t.Errorf("origin index = %d, want 0", got)
	}
	// Corners must all be distinct.
	seen := map[uint64]string{}
	corners := []struct {
		name string
		x, y float64
	}{
		{"bl", 0.0, 0.0},
		{"br", 0.999, 0.0},
		{"tr", 0.999, 0.999},
		{"tl", 0.0, 0.999},
	}
	for _, c := range corners {
		v := idx(c.x, c.y)
		if prev, ok := seen[v]; ok {
			t.Errorf("corners %s and %s both map to %d", prev, c.name, v)
		}
		seen[v] = c.name
	}
}

// TestHilbertIndex_LocalityPreserved: two points near each other in
// (x, y) should be near each other in the 1D index — the whole
// reason we use a Hilbert curve. Compared against a random baseline
// (two far-apart points), the nearby pair's index-distance should be
// substantially smaller on average.
func TestHilbertIndex_LocalityPreserved(t *testing.T) {
	b := Bounds{MinX: 0, MinY: 0, MaxX: 1000, MaxY: 1000}
	order := DefaultHilbertOrder

	// Nearby: within a 10-unit patch.
	center := HilbertIndex(500, 500, b, order)
	nearby := HilbertIndex(505, 502, b, order)
	// Far: opposite corner.
	far := HilbertIndex(50, 50, b, order)

	near := absDiff(center, nearby)
	distant := absDiff(center, far)
	// Not a tight bound — Hilbert order and coordinate values matter —
	// but nearby should be orders of magnitude closer than distant.
	if near*100 >= distant {
		t.Errorf("locality violated: near-diff=%d far-diff=%d (near*100 should be < far)", near, distant)
	}
}

// TestHilbertIndex_OrderDefaultsWhenInvalid: order 0, -1, and 40
// should silently fall back to DefaultHilbertOrder rather than
// producing nonsense.
func TestHilbertIndex_OrderDefaultsWhenInvalid(t *testing.T) {
	b := Bounds{MinX: 0, MinY: 0, MaxX: 1, MaxY: 1}
	want := HilbertIndex(0.5, 0.5, b, DefaultHilbertOrder)
	for _, o := range []int{0, -1, 40, 999} {
		if got := HilbertIndex(0.5, 0.5, b, o); got != want {
			t.Errorf("order=%d: got %d, want %d (default fallback)", o, got, want)
		}
	}
}

// TestHilbertIndex_EmptyBounds: no crash, deterministic 0 for the
// null-bbox case.
func TestHilbertIndex_EmptyBounds(t *testing.T) {
	if got := HilbertIndex(1, 2, EmptyBounds(), 8); got != 0 {
		t.Errorf("empty bounds: got %d, want 0", got)
	}
	if got := HilbertIndex(1, 2, Bounds{MinX: 5, MinY: 5, MaxX: 5, MaxY: 5}, 8); got != 0 {
		t.Errorf("zero-extent bounds: got %d, want 0", got)
	}
}

// TestHilbertIndex_OutOfBoundsClamps: a point outside bounds
// shouldn't panic or wrap around — it should clamp to the grid edge
// and produce a stable index.
func TestHilbertIndex_OutOfBoundsClamps(t *testing.T) {
	b := Bounds{MinX: 0, MinY: 0, MaxX: 10, MaxY: 10}
	// Above-right corner should equal (near-max, near-max) inside bounds.
	inside := HilbertIndex(9.99, 9.99, b, 8)
	outside := HilbertIndex(100, 100, b, 8)
	if outside != inside {
		t.Errorf("out-of-bounds should clamp: outside=%d inside=%d", outside, inside)
	}
	// Below-left similarly clamps to (0, 0).
	if got := HilbertIndex(-100, -100, b, 8); got != 0 {
		t.Errorf("below-left clamp: got %d, want 0", got)
	}
}

// TestHilbertIndex_MonotoneAlongAxis: at fixed y, the Hilbert curve
// visits x=0 before x=n-1 (bl → br path exists at every order).
// We check the coarser property: an ordered sample of points along
// y=0 doesn't collapse into a single index — every x maps to some
// value, and adjacent xs stay reasonably close.
func TestHilbertIndex_MonotoneAlongAxis(t *testing.T) {
	b := Bounds{MinX: 0, MinY: 0, MaxX: 1024, MaxY: 1024}
	order := 10 // 1024×1024 grid → each unit is one cell
	distinct := map[uint64]struct{}{}
	for x := 0; x < 1024; x += 32 {
		d := HilbertIndex(float64(x), 0, b, order)
		distinct[d] = struct{}{}
	}
	if len(distinct) < 30 {
		t.Errorf("expected ~32 distinct indices along y=0, got %d", len(distinct))
	}
}

// TestHilbertIndex_Deterministic: pure function — same inputs, same
// outputs across calls.
func TestHilbertIndex_Deterministic(t *testing.T) {
	b := Bounds{MinX: -180, MinY: -90, MaxX: 180, MaxY: 90}
	x, y := -122.42, 37.77 // San Francisco
	first := HilbertIndex(x, y, b, 20)
	for range 100 {
		if HilbertIndex(x, y, b, 20) != first {
			t.Fatalf("non-deterministic Hilbert index")
		}
	}
}

func absDiff(a, b uint64) uint64 {
	if a > b {
		return a - b
	}
	return b - a
}

// TestHilbertIndex_FloatPrecisionAtScale: WGS84 world-extent bbox
// with meter-scale coordinate differences should still produce
// distinct indices at order 24+. Regression check for float
// precision loss during normalization.
func TestHilbertIndex_FloatPrecisionAtScale(t *testing.T) {
	world := Bounds{MinX: -180, MinY: -90, MaxX: 180, MaxY: 90}
	// Two points ~1km apart at San Francisco lat (~0.009° longitude).
	a := HilbertIndex(-122.4194, 37.7749, world, 24)
	b := HilbertIndex(-122.4104, 37.7749, world, 24)
	if a == b {
		t.Errorf("points ~1km apart collapsed to same index at order 24: %d", a)
	}
}
