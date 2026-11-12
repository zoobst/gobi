package detmath

import (
	"hash/fnv"
	"math"
	"math/rand/v2"
	"runtime"
	"testing"
)

// fn is one ported function beside the standard one. Functions of
// one argument ignore y.
type fn struct {
	name string
	det  func(x, y float64) float64
	std  func(x, y float64) float64
	// domain maps an arbitrary input into the function's domain.
	domain func(x float64) float64
}

func one(f func(float64) float64) func(x, y float64) float64 {
	return func(x, _ float64) float64 { return f(x) }
}

func unit(x float64) float64 { return math.Mod(x, 1) }

var fns = []fn{
	{"Sin", one(Sin), one(math.Sin), nil},
	{"Cos", one(Cos), one(math.Cos), nil},
	{"Sincos.sin", func(x, _ float64) float64 { s, _ := Sincos(x); return s }, func(x, _ float64) float64 { s, _ := math.Sincos(x); return s }, nil},
	{"Sincos.cos", func(x, _ float64) float64 { _, c := Sincos(x); return c }, func(x, _ float64) float64 { _, c := math.Sincos(x); return c }, nil},
	{"Tan", one(Tan), one(math.Tan), nil},
	{"Atan", one(Atan), one(math.Atan), nil},
	{"Atan2", Atan2, math.Atan2, nil},
	{"Asin", one(Asin), one(math.Asin), unit},
	{"Acos", one(Acos), one(math.Acos), unit},
	{"Hypot", Hypot, math.Hypot, nil},
	{"Exp", one(Exp), one(math.Exp), func(x float64) float64 { return math.Mod(x, 700) }},
	{"Log", one(Log), one(math.Log), math.Abs},
	{"Pow", Pow, math.Pow, math.Abs},
}

// inputs calls visit with a fixed sequence of argument pairs: angles,
// degrees, projected-coordinate magnitudes, values across the whole
// exponent range, and the special values. They are built with no
// multiply-add the compiler could fuse, so every platform feeds the
// functions the same bits.
func inputs(visit func(x, y float64)) {
	r := rand.New(rand.NewPCG(1, 2))
	u := func() float64 { return float64(r.Uint64()>>11) / (1 << 53) }
	span := func(lo, hi float64) float64 { return float64(u()*(hi-lo)) + lo }
	for i := range 400_000 {
		switch i % 5 {
		case 0:
			visit(span(-math.Pi, math.Pi), span(-1, 1))
		case 1:
			visit(span(-180, 180), span(-90, 90))
		case 2:
			visit(span(0, 1e6), span(0, 1e7))
		case 3:
			visit(math.Ldexp(span(-1, 1), r.IntN(120)-60), math.Ldexp(span(-1, 1), r.IntN(120)-60))
		default:
			// Past 2**29, where the trigonometric functions switch
			// to their big-argument reduction.
			visit(math.Ldexp(span(-1, 1), 29+r.IntN(40)), span(-3, 3))
		}
	}
	special := []float64{
		0, math.Copysign(0, -1), 1, -1, 0.5, -0.5, 2, 1e-300, 5e-324, math.MaxFloat64, -math.MaxFloat64,
		math.Pi, math.Pi / 2, math.Pi / 4, 1 << 29, 1<<29 + 1, 709.78, -745.13,
		math.Inf(1), math.Inf(-1), math.NaN(),
	}
	for _, x := range special {
		for _, y := range special {
			visit(x, y)
		}
	}
}

// same reports whether a and b are the same value bit for bit, with
// every NaN counted as the same.
func same(a, b float64) bool {
	return math.Float64bits(a) == math.Float64bits(b) || (math.IsNaN(a) && math.IsNaN(b))
}

// On wasm the compiler fuses nothing and math has no assembly, so the
// standard functions are exactly the algorithms this package copied:
// each function here must return the standard one's bits for every
// input. That is the check that the port changed nothing but the
// rounding.
func TestMatchesStandardLibraryOnWasm(t *testing.T) {
	if runtime.GOARCH != "wasm" {
		t.Skip("exact comparison only where the standard library is unfused pure Go (GOARCH=wasm)")
	}
	for _, f := range fns {
		bad := 0
		inputs(func(x, y float64) {
			if f.domain != nil {
				x = f.domain(x)
			}
			if got, want := f.det(x, y), f.std(x, y); !same(got, want) && bad < 3 {
				bad++
				t.Errorf("%s(%v, %v) = %v, math gives %v", f.name, x, y, got, want)
			}
		})
	}
}

// On every platform the functions stay close to the standard
// library's and agree with it exactly on the special values. Close,
// not equal: where the standard library is fused it can be several
// units in the last place away, and in the right direction (Asin
// near 1 computes 1 - x*x, which a fused multiply gets exactly). The
// bound is a relative 1e-13, plus an absolute 1e-14 for Acos, whose
// algorithm (π/2 - Asin) loses that much near 1 on any platform.
//
// Subnormal arguments are left out: amd64's assembly Log returns
// about -709 for 5e-324, whose logarithm is -744.44, which is what
// this package and the pure-Go standard version return.
func TestCloseToStandardLibrary(t *testing.T) {
	subnormal := func(v float64) bool { return v != 0 && math.Abs(v) < 0x1p-1022 }
	for _, f := range fns {
		bad := 0
		abs := 4 * math.SmallestNonzeroFloat64
		if f.name == "Acos" {
			abs = 1e-14
		}
		inputs(func(x, y float64) {
			if f.domain != nil {
				x = f.domain(x)
			}
			if subnormal(x) || subnormal(y) {
				return
			}
			got, want := f.det(x, y), f.std(x, y)
			if same(got, want) {
				return
			}
			ok := !math.IsNaN(got) && !math.IsNaN(want) && !math.IsInf(got, 0) && !math.IsInf(want, 0) &&
				math.Abs(got-want) <= math.Max(1e-13*math.Abs(want), abs)
			if !ok && bad < 3 {
				bad++
				t.Errorf("%s(%v, %v) = %v, math gives %v", f.name, x, y, got, want)
			}
		})
	}
}

// digests are the FNV-1a hashes of each function's results over
// inputs, computed once (on wasm, where they are also the standard
// library's). Every platform must reproduce them: this is the test
// that the package gives the same bits everywhere.
var digests = map[string]uint64{
	"Sin":        0xf55e036f7472c880,
	"Cos":        0x4fbd70edf4c7ab6b,
	"Sincos.sin": 0xf55e036f7472c880,
	"Sincos.cos": 0x4fbd70edf4c7ab6b,
	"Tan":        0x30adb8a3e4f89b83,
	"Atan":       0x47f871a0461b4e28,
	"Atan2":      0x370ee6500c39b6a3,
	"Asin":       0x2f09c00ac51b3810,
	"Acos":       0x03235fd170f86254,
	"Hypot":      0x1bbd3e484629191b,
	"Exp":        0xde22bc7a941cb97f,
	"Log":        0x3eae12d3c1434bab,
	"Pow":        0xb66b5715246351e1,
}

func TestSameBitsOnEveryPlatform(t *testing.T) {
	for _, f := range fns {
		h := fnv.New64a()
		var b [8]byte
		inputs(func(x, y float64) {
			if f.domain != nil {
				x = f.domain(x)
			}
			v := f.det(x, y)
			bits := math.Float64bits(v)
			if math.IsNaN(v) {
				bits = 0x7FF8000000000001 // one NaN
			}
			for k := range 8 {
				b[k] = byte(bits >> (8 * k))
			}
			h.Write(b[:])
		})
		got := h.Sum64()
		want, ok := digests[f.name]
		if !ok {
			t.Errorf("%s: no recorded digest; this platform (%s/%s) gives %#016x", f.name, runtime.GOOS, runtime.GOARCH, got)
			continue
		}
		if got != want {
			t.Errorf("%s: digest %#016x on %s/%s, want %#016x: results differ from other platforms", f.name, got, runtime.GOOS, runtime.GOARCH, want)
		}
	}
}
