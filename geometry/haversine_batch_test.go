package geometry

import (
	"errors"
	"math"
	"testing"
)

// TestHaversineBatch_MatchesScalar — the batch call and the scalar
// Haversine agree pair-for-pair. Uses a mix of short (~100km),
// medium (~4000km), and long (~5500km) legs to catch any regime-
// specific numerical drift.
func TestHaversineBatch_MatchesScalar(t *testing.T) {
	from := []Point{
		{X: -73.9857, Y: 40.7484},  // NYC
		{X: -118.2437, Y: 34.0522}, // LA
		{X: 0, Y: 0},               // null island
		{X: 2.3522, Y: 48.8566},    // Paris
	}
	to := []Point{
		{X: -0.1276, Y: 51.5074},  // London
		{X: -73.9857, Y: 40.7484}, // NYC
		{X: 1, Y: 0},              // one deg east
		{X: -0.1276, Y: 51.5074},  // London
	}
	got, err := HaversineBatch(from, to, UnitKilometers)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(from) {
		t.Fatalf("len(got) = %d, want %d", len(got), len(from))
	}
	for i := range from {
		want, err := Haversine(from[i], to[i], UnitKilometers)
		if err != nil {
			t.Fatal(err)
		}
		if math.Abs(got[i]-want) > 1e-9 {
			t.Errorf("row %d: got %v, want %v (scalar Haversine)", i, got[i], want)
		}
	}
}

// TestHaversineBatch_KnownDistances — spot-check the well-known
// distances to catch any wholesale unit / scale mistake.
func TestHaversineBatch_KnownDistances(t *testing.T) {
	got, err := HaversineBatch(
		[]Point{
			{X: -73.9857, Y: 40.7484}, // NYC
			{X: 0, Y: 90},             // North pole
			{X: 0, Y: 0},              // Equator
		},
		[]Point{
			{X: -0.1276, Y: 51.5074}, // London
			{X: 0, Y: 0},             // Equator
			{X: 1, Y: 0},             // One deg east
		},
		UnitKilometers,
	)
	if err != nil {
		t.Fatal(err)
	}
	// NYC → London ~5570 km
	if got[0] < 5500 || got[0] > 5600 {
		t.Errorf("NYC→London = %v km, want ~5570", got[0])
	}
	// Pole → Equator = quarter Earth circumference ~10007 km
	if got[1] < 9990 || got[1] > 10020 {
		t.Errorf("pole→equator = %v km, want ~10007", got[1])
	}
	// One degree at equator ~111 km
	if got[2] < 110 || got[2] > 112 {
		t.Errorf("1-deg-east = %v km, want ~111", got[2])
	}
}

// TestHaversineBatch_UnitScaling — same input, different units,
// ratios match. Sanity check that metersPerUnit is hoisted correctly.
func TestHaversineBatch_UnitScaling(t *testing.T) {
	from := []Point{{X: 0, Y: 0}}
	to := []Point{{X: 1, Y: 0}}

	km, _ := HaversineBatch(from, to, UnitKilometers)
	m, _ := HaversineBatch(from, to, UnitMeters)
	mi, _ := HaversineBatch(from, to, UnitMiles)

	if math.Abs(km[0]*1000-m[0]) > 1e-6 {
		t.Errorf("km→m mismatch: %v vs %v", km[0]*1000, m[0])
	}
	if math.Abs(km[0]/mi[0]-1.609344) > 1e-6 {
		t.Errorf("km/mi ratio = %v, want 1.609344", km[0]/mi[0])
	}
}

// TestHaversineBatch_LengthMismatch — mismatched slice lengths
// error, don't panic.
func TestHaversineBatch_LengthMismatch(t *testing.T) {
	from := []Point{{X: 0, Y: 0}, {X: 1, Y: 1}}
	to := []Point{{X: 0, Y: 0}}
	_, err := HaversineBatch(from, to, UnitKilometers)
	if err == nil {
		t.Fatal("expected length mismatch error")
	}
}

// TestHaversineBatch_EmptyInputs — zero-length slices return a
// non-nil empty result, not an error.
func TestHaversineBatch_EmptyInputs(t *testing.T) {
	got, err := HaversineBatch(nil, nil, UnitKilometers)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Errorf("expected non-nil empty slice, got nil")
	}
	if len(got) != 0 {
		t.Errorf("len = %d, want 0", len(got))
	}
}

// TestHaversineBatch_InvalidUnit — bad Unit propagates the same
// error the scalar Haversine returns.
func TestHaversineBatch_InvalidUnit(t *testing.T) {
	from := []Point{{X: 0, Y: 0}}
	to := []Point{{X: 1, Y: 0}}
	_, err := HaversineBatch(from, to, Unit("furlongs"))
	if err == nil {
		t.Fatal("expected error on invalid unit")
	}
	if !errors.Is(err, ErrInvalidUnit) {
		t.Errorf("error should wrap ErrInvalidUnit, got %v", err)
	}
}

// BenchmarkHaversineBatch_vs_ScalarLoop compares the bulk-loop
// path against per-row scalar Haversine calls on the same N=10k
// fixture. Loop-scaffolding + constant hoisting are the whole win —
// no SIMD, no polynomial approximations, just plain scalar math
// with fewer per-call boundaries.
func BenchmarkHaversineBatch_vs_ScalarLoop(b *testing.B) {
	const N = 10_000
	from := make([]Point, N)
	to := make([]Point, N)
	for i := range from {
		// Some geographic spread — random-ish but deterministic.
		from[i] = Point{X: float64(i%180 - 90), Y: float64(i%90 - 45)}
		to[i] = Point{X: float64((i+7)%180 - 90), Y: float64((i+3)%90 - 45)}
	}

	b.Run("Batch", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			out, err := HaversineBatch(from, to, UnitKilometers)
			if err != nil {
				b.Fatal(err)
			}
			_ = out
		}
	})

	b.Run("ScalarLoop", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			out := make([]float64, N)
			for j := range from {
				d, err := Haversine(from[j], to[j], UnitKilometers)
				if err != nil {
					b.Fatal(err)
				}
				out[j] = d
			}
			_ = out
		}
	})
}
