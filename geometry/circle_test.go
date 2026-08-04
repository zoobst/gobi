package geometry

import (
	"errors"
	"math"
	"math/rand/v2"
	"testing"
)

func TestCircle_ContainsAndDistance(t *testing.T) {
	c := Circle{Center: Point{X: 5, Y: 5}, Radius: 3}

	cases := []struct {
		name         string
		p            Point
		wantContains bool
		wantSigned   float64 // signed distance to boundary
	}{
		{"center", Point{X: 5, Y: 5}, true, -3},
		{"on boundary", Point{X: 8, Y: 5}, true, 0},
		{"strictly inside", Point{X: 6, Y: 5}, true, -2},
		{"just outside", Point{X: 8.5, Y: 5}, false, 0.5},
		{"far away", Point{X: 100, Y: 100}, false, math.Sqrt(2)*95 - 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := c.Contains(tc.p); got != tc.wantContains {
				t.Errorf("Contains(%v) = %v, want %v", tc.p, got, tc.wantContains)
			}
			if got := c.Distance(tc.p); math.Abs(got-tc.wantSigned) > 1e-9 {
				t.Errorf("Distance(%v) = %v, want %v", tc.p, got, tc.wantSigned)
			}
		})
	}
}

func TestCircle_AreaAndCircumference(t *testing.T) {
	c := Circle{Radius: 3}
	if a := c.Area(); math.Abs(a-9*math.Pi) > 1e-9 {
		t.Errorf("Area = %v, want 9π", a)
	}
	if p := c.Circumference(); math.Abs(p-6*math.Pi) > 1e-9 {
		t.Errorf("Circumference = %v, want 6π", p)
	}
}

func TestCircle_BoundaryAndBoundaryLine(t *testing.T) {
	c := Circle{Center: Point{X: 0, Y: 0}, Radius: 10}
	poly := c.Boundary(64)
	if len(poly.Rings) != 1 || len(poly.Rings[0]) != 65 {
		t.Errorf("Boundary(64): rings=%d, len=%d; want 1 ring, 65 points",
			len(poly.Rings), len(poly.Rings[0]))
	}
	// First and last must be equal (closed ring).
	first, last := poly.Rings[0][0], poly.Rings[0][64]
	if first.X != last.X || first.Y != last.Y {
		t.Errorf("closing vertex mismatch: %v vs %v", first, last)
	}
	// Every vertex should sit on the circle to within numerical noise.
	for i, p := range poly.Rings[0] {
		r := math.Hypot(p.X, p.Y)
		if math.Abs(r-10) > 1e-9 {
			t.Errorf("Boundary vertex %d radius %v, want 10", i, r)
		}
	}

	line := c.BoundaryLine(64)
	if len(line.Points) != 64 {
		t.Errorf("BoundaryLine(64) len = %d, want 64 (open)", len(line.Points))
	}
}

func TestFitCircle_PerfectCircle_BothMethods(t *testing.T) {
	// 20 points on a circle centered at (100, 50) with radius 7.
	const cx, cy, r = 100.0, 50.0, 7.0
	n := 20
	pts := make([]Point, n)
	for i := range n {
		theta := 2 * math.Pi * float64(i) / float64(n)
		pts[i] = Point{X: cx + r*math.Cos(theta), Y: cy + r*math.Sin(theta)}
	}
	for _, method := range []CircleFitMethod{FitTaubin, FitKasa} {
		t.Run(circleMethodName(method), func(t *testing.T) {
			got, resid, err := FitCircle(pts, CircleFitOptions{Method: method})
			if err != nil {
				t.Fatalf("FitCircle: %v", err)
			}
			if math.Abs(got.Center.X-cx) > 1e-6 || math.Abs(got.Center.Y-cy) > 1e-6 {
				t.Errorf("center = %v, want (%v,%v)", got.Center, cx, cy)
			}
			if math.Abs(got.Radius-r) > 1e-6 {
				t.Errorf("radius = %v, want %v", got.Radius, r)
			}
			// Residuals should be ~0 for every point on a perfect circle.
			for i, r := range resid {
				if math.Abs(r) > 1e-6 {
					t.Errorf("residual[%d] = %v, want ~0", i, r)
				}
			}
		})
	}
}

func TestFitCircle_NoisyCircle_TaubinBeatsKasaOnArcs(t *testing.T) {
	// Points on a partial arc — the pathological case for Kasa. 12
	// points on 90° of a circle centered at (0, 0) radius 100, with
	// small deterministic noise.
	rng := rand.New(rand.NewPCG(0xDEAD, 0xBEEF))
	const cx, cy, r = 0.0, 0.0, 100.0
	n := 12
	pts := make([]Point, n)
	for i := range n {
		// 90° arc from 0 to π/2.
		theta := math.Pi / 2 * float64(i) / float64(n-1)
		noise := (rng.Float64() - 0.5) * 0.5 // ±0.25
		pts[i] = Point{
			X: cx + (r+noise)*math.Cos(theta),
			Y: cy + (r+noise)*math.Sin(theta),
		}
	}
	kasa, _, err := FitCircle(pts, CircleFitOptions{Method: FitKasa})
	if err != nil {
		t.Fatalf("FitKasa: %v", err)
	}
	taubin, _, err := FitCircle(pts, CircleFitOptions{Method: FitTaubin})
	if err != nil {
		t.Fatalf("FitTaubin: %v", err)
	}
	// Taubin should be closer to the truth radius than Kasa. This is
	// the whole reason Taubin is default.
	kasaErr := math.Abs(kasa.Radius - r)
	taubinErr := math.Abs(taubin.Radius - r)
	if taubinErr >= kasaErr {
		t.Errorf("expected Taubin (err=%v) to beat Kasa (err=%v) on a partial arc",
			taubinErr, kasaErr)
	}
	// Both should at least be in the right ballpark.
	if taubinErr > 5 {
		t.Errorf("Taubin radius error %v > 5 units (expected <1 for noise 0.25)", taubinErr)
	}
}

func TestFitCircle_CollinearPoints_ReturnsError(t *testing.T) {
	pts := []Point{
		{X: 0, Y: 0}, {X: 1, Y: 1}, {X: 2, Y: 2}, {X: 3, Y: 3}, {X: 4, Y: 4},
	}
	for _, method := range []CircleFitMethod{FitTaubin, FitKasa} {
		t.Run(circleMethodName(method), func(t *testing.T) {
			_, _, err := FitCircle(pts, CircleFitOptions{Method: method})
			if !errors.Is(err, ErrCircleFit) {
				t.Errorf("collinear input: err = %v, want ErrCircleFit", err)
			}
		})
	}
}

func TestFitCircle_TooFewPoints_Errors(t *testing.T) {
	for _, n := range []int{0, 1, 2} {
		pts := make([]Point, n)
		_, _, err := FitCircle(pts, CircleFitOptions{})
		if !errors.Is(err, ErrCircleFit) {
			t.Errorf("n=%d: err = %v, want ErrCircleFit", n, err)
		}
	}
}

func TestFitCircle_CRSPropagates(t *testing.T) {
	pts := []Point{
		{X: 0, Y: 1, CRSValue: PseudoMercator},
		{X: 1, Y: 0, CRSValue: PseudoMercator},
		{X: 0, Y: -1, CRSValue: PseudoMercator},
		{X: -1, Y: 0, CRSValue: PseudoMercator},
	}
	c, _, err := FitCircle(pts, CircleFitOptions{})
	if err != nil {
		t.Fatalf("FitCircle: %v", err)
	}
	if c.Center.CRSValue.EPSG != PseudoMercator.EPSG {
		t.Errorf("center CRS = %v, want %v", c.Center.CRSValue, PseudoMercator)
	}
}

func circleMethodName(m CircleFitMethod) string {
	switch m {
	case FitTaubin:
		return "Taubin"
	case FitKasa:
		return "Kasa"
	}
	return "unknown"
}
