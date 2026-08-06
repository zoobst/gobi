package geometry

import (
	"errors"
	"math"
	"testing"
)

func TestNewEllipse_Fields(t *testing.T) {
	center := Point{X: 5, Y: -3, CRSValue: PseudoMercator}
	e := NewEllipse(center, 7, 2, math.Pi/6)
	if e.SemiA != 7 || e.SemiB != 2 {
		t.Errorf("semi-axes = %v/%v, want 7/2", e.SemiA, e.SemiB)
	}
	if e.Rotation != math.Pi/6 {
		t.Errorf("rotation = %v, want π/6", e.Rotation)
	}
	if e.Center != center {
		t.Errorf("center = %v, want %v", e.Center, center)
	}
}

// TestEllipse_Contains_AxisAligned covers the un-rotated ellipse
// with SemiA along +X and SemiB along +Y.
func TestEllipse_Contains_AxisAligned(t *testing.T) {
	e := NewEllipse(Point{X: 0, Y: 0}, 3, 2, 0)
	cases := []struct {
		name string
		p    Point
		want bool
	}{
		{"center", Point{X: 0, Y: 0}, true},
		{"on +X boundary", Point{X: 3, Y: 0}, true},
		{"on +Y boundary", Point{X: 0, Y: 2}, true},
		{"inside", Point{X: 1, Y: 1}, true},
		{"just outside +X", Point{X: 3.01, Y: 0}, false},
		{"just outside +Y", Point{X: 0, Y: 2.01}, false},
		{"outside diagonal", Point{X: 3, Y: 2}, false}, // (1)² + (1)² = 2 > 1
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := e.Contains(c.p); got != c.want {
				t.Errorf("Contains(%v) = %v, want %v", c.p, got, c.want)
			}
		})
	}
}

// TestEllipse_Contains_Rotated90 checks that a 90° rotation swaps
// which physical axis SemiA covers.
func TestEllipse_Contains_Rotated90(t *testing.T) {
	// SemiA=3 (was on +X), rotated by π/2 CCW — now along +Y.
	// SemiB=2 (was on +Y), now along -X → same as +X after |·|.
	e := NewEllipse(Point{X: 0, Y: 0}, 3, 2, math.Pi/2)
	// After the rotation, (0, 3) should be on the boundary.
	if !e.Contains(Point{X: 0, Y: 3}) {
		t.Errorf("rotated: (0,3) should be on boundary")
	}
	// (3, 0) should be OUTSIDE — SemiA has moved to +Y.
	if e.Contains(Point{X: 3, Y: 0}) {
		t.Errorf("rotated: (3,0) should be outside")
	}
	// (2, 0) should be on the boundary — SemiB has moved to -X (same magnitude).
	if !e.Contains(Point{X: 2, Y: 0}) {
		t.Errorf("rotated: (2,0) should be on boundary")
	}
}

func TestEllipse_Area(t *testing.T) {
	e := NewEllipse(Point{}, 3, 2, 0.5) // rotation doesn't affect area
	want := math.Pi * 6
	if math.Abs(e.Area()-want) > 1e-12 {
		t.Errorf("Area = %v, want %v", e.Area(), want)
	}
}

// TestEllipse_Circumference_CircleCase: when a == b the ellipse is
// a circle, Ramanujan's formula should reduce to 2πr exactly (h=0).
func TestEllipse_Circumference_CircleCase(t *testing.T) {
	e := NewEllipse(Point{}, 5, 5, 0)
	want := 2 * math.Pi * 5
	if math.Abs(e.Circumference()-want) > 1e-12 {
		t.Errorf("Circumference = %v, want %v (a==b → circle)", e.Circumference(), want)
	}
}

// TestEllipse_Circumference_ModerateEccentricity: for a 2:1 ellipse,
// the true perimeter (from a reference numerical elliptic integral)
// is ~9.6884482... Ramanujan II gets it to <1e-9 relative.
func TestEllipse_Circumference_ModerateEccentricity(t *testing.T) {
	e := NewEllipse(Point{}, 2, 1, 0)
	// Reference from Wolfram: 2·EllipticE(sqrt(1 - 1/4)) with a=2:
	// approximately 9.6884482205418...
	const reference = 9.6884482205418155
	rel := math.Abs(e.Circumference()-reference) / reference
	if rel > 1e-6 {
		t.Errorf("Circumference = %v, want %v (rel err %g)",
			e.Circumference(), reference, rel)
	}
}

func TestEllipse_Bounds_AxisAlignedThenRotated(t *testing.T) {
	// Axis-aligned 3×2 ellipse at (5, -3): bounds should be
	// [5-3, -3-2] to [5+3, -3+2].
	e := NewEllipse(Point{X: 5, Y: -3}, 3, 2, 0)
	b := e.Bounds()
	if b.MinX != 2 || b.MaxX != 8 || b.MinY != -5 || b.MaxY != -1 {
		t.Errorf("axis-aligned bounds = %+v; want [2,-5]-[8,-1]", b)
	}
	// Rotate by 90°: SemiA (3) is now along Y, SemiB (2) along X.
	// Bounds width = 4, height = 6.
	e = NewEllipse(Point{X: 0, Y: 0}, 3, 2, math.Pi/2)
	b = e.Bounds()
	if math.Abs(b.MinX+2) > 1e-9 || math.Abs(b.MaxX-2) > 1e-9 ||
		math.Abs(b.MinY+3) > 1e-9 || math.Abs(b.MaxY-3) > 1e-9 {
		t.Errorf("rotated 90° bounds = %+v; want [-2,-3]-[2,3]", b)
	}
}

// TestEllipse_Boundary_VerticesOnEllipse: every emitted vertex must
// satisfy the ellipse equation to within numerical noise.
func TestEllipse_Boundary_VerticesOnEllipse(t *testing.T) {
	e := NewEllipse(Point{X: 10, Y: -5}, 7, 3, math.Pi/7)
	poly := e.Boundary(64)
	if len(poly.Rings) != 1 || len(poly.Rings[0]) != 65 {
		t.Fatalf("boundary shape: rings=%d len=%d; want 1×65",
			len(poly.Rings), len(poly.Rings[0]))
	}
	// Closing vertex must match the first.
	first, last := poly.Rings[0][0], poly.Rings[0][64]
	if first != last {
		t.Errorf("ring not closed: first=%v last=%v", first, last)
	}
	// Every vertex, transformed back to ellipse-local, should satisfy
	// (x'/a)² + (y'/b)² == 1 within tolerance.
	cosR := math.Cos(-e.Rotation)
	sinR := math.Sin(-e.Rotation)
	for i, p := range poly.Rings[0][:64] {
		dx := p.X - e.Center.X
		dy := p.Y - e.Center.Y
		xL := dx*cosR - dy*sinR
		yL := dx*sinR + dy*cosR
		f := (xL*xL)/(e.SemiA*e.SemiA) + (yL*yL)/(e.SemiB*e.SemiB)
		if math.Abs(f-1) > 1e-9 {
			t.Errorf("vertex %d off ellipse: f=%v (want 1)", i, f)
		}
	}
}

// TestEllipse_BoundaryLine covers the open-ring variant.
func TestEllipse_BoundaryLine(t *testing.T) {
	e := NewEllipse(Point{X: 0, Y: 0}, 5, 3, 0)
	l := e.BoundaryLine(32)
	if len(l.Points) != 32 {
		t.Errorf("BoundaryLine len = %d, want 32 (open)", len(l.Points))
	}
	// First point at (5, 0) since Rotation = 0, t=0 gives (SemiA·1, SemiB·0).
	if math.Abs(l.Points[0].X-5) > 1e-9 || math.Abs(l.Points[0].Y) > 1e-9 {
		t.Errorf("first = %v, want (5, 0)", l.Points[0])
	}
}

func TestEllipseFromFoci_Basic(t *testing.T) {
	// Foci at (-4, 0) and (4, 0), majorAxis = 10.
	// → Center (0,0), SemiA = 5, c = 4, SemiB = sqrt(25-16) = 3.
	// Rotation = atan2(0, 8) = 0.
	e, err := EllipseFromFoci(
		Point{X: -4, Y: 0}, Point{X: 4, Y: 0}, 10)
	if err != nil {
		t.Fatalf("EllipseFromFoci: %v", err)
	}
	if e.Center.X != 0 || e.Center.Y != 0 {
		t.Errorf("center = %v, want (0,0)", e.Center)
	}
	if math.Abs(e.SemiA-5) > 1e-12 {
		t.Errorf("SemiA = %v, want 5", e.SemiA)
	}
	if math.Abs(e.SemiB-3) > 1e-12 {
		t.Errorf("SemiB = %v, want 3", e.SemiB)
	}
	if e.Rotation != 0 {
		t.Errorf("rotation = %v, want 0", e.Rotation)
	}
	// Sanity: both foci should be inside (they're always inside their
	// own ellipse).
	if !e.Contains(Point{X: -4, Y: 0}) || !e.Contains(Point{X: 4, Y: 0}) {
		t.Errorf("foci should be inside their ellipse")
	}
}

func TestEllipseFromFoci_Rotated(t *testing.T) {
	// Foci along the diagonal — rotation should be π/4.
	f1 := Point{X: 0, Y: 0}
	f2 := Point{X: 6, Y: 6}
	e, err := EllipseFromFoci(f1, f2, 10)
	if err != nil {
		t.Fatalf("EllipseFromFoci: %v", err)
	}
	if math.Abs(e.Rotation-math.Pi/4) > 1e-12 {
		t.Errorf("rotation = %v, want π/4", e.Rotation)
	}
	if math.Abs(e.Center.X-3) > 1e-12 || math.Abs(e.Center.Y-3) > 1e-12 {
		t.Errorf("center = %v, want (3, 3)", e.Center)
	}
}

// TestEllipseFromFoci_MajorTooSmall: an ellipse can't exist when
// majorAxis < |f1 - f2| (the foci lie outside the would-be ellipse).
func TestEllipseFromFoci_MajorTooSmall(t *testing.T) {
	_, err := EllipseFromFoci(
		Point{X: 0, Y: 0}, Point{X: 10, Y: 0}, 5)
	if !errors.Is(err, ErrEllipseFromFoci) {
		t.Errorf("err = %v, want ErrEllipseFromFoci", err)
	}
}

// TestEllipseFromFoci_DegenerateCircle: coincident foci → the
// definition reduces to a circle, so SemiA == SemiB == majorAxis/2.
func TestEllipseFromFoci_DegenerateCircle(t *testing.T) {
	center := Point{X: 5, Y: 5, CRSValue: PseudoMercator}
	e, err := EllipseFromFoci(center, center, 8)
	if err != nil {
		t.Fatalf("EllipseFromFoci: %v", err)
	}
	if math.Abs(e.SemiA-4) > 1e-12 || math.Abs(e.SemiB-4) > 1e-12 {
		t.Errorf("degenerate case: semi-axes = %v/%v, want 4/4", e.SemiA, e.SemiB)
	}
	if e.Rotation != 0 {
		t.Errorf("degenerate case: rotation = %v, want 0", e.Rotation)
	}
	if e.Center.CRSValue.EPSG != PseudoMercator.EPSG {
		t.Errorf("CRS not propagated: got %v", e.Center.CRSValue)
	}
}

func TestEllipseFromFoci_NonPositiveMajor(t *testing.T) {
	_, err := EllipseFromFoci(Point{}, Point{X: 1}, 0)
	if !errors.Is(err, ErrEllipseFromFoci) {
		t.Errorf("majorAxis=0: err = %v, want ErrEllipseFromFoci", err)
	}
	_, err = EllipseFromFoci(Point{}, Point{X: 1}, -3)
	if !errors.Is(err, ErrEllipseFromFoci) {
		t.Errorf("majorAxis<0: err = %v, want ErrEllipseFromFoci", err)
	}
}
