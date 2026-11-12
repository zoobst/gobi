// Copyright 2010 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE-go file.
//
// Copied from the Go standard library's math package and modified for
// gobi so that no multiply is fused into an add; see doc.go.

package detmath

import "math"

// Coefficients _sin[] and _cos[] are found in pkg/math/sin.go.

// Sincos returns Sin(x), Cos(x).
//
// Special cases are:
//
//	Sincos(±0) = ±0, 1
//	Sincos(±Inf) = NaN, NaN
//	Sincos(NaN) = NaN, NaN
func Sincos(x float64) (sin, cos float64) {
	const (
		PI4A = 7.85398125648498535156e-1  // 0x3fe921fb40000000, Pi/4 split into three parts
		PI4B = 3.77489470793079817668e-8  // 0x3e64442d00000000,
		PI4C = 2.69515142907905952645e-15 // 0x3ce8469898cc5170,
	)
	// special cases
	switch {
	case x == 0:
		return x, 1 // return ±0.0, 1.0
	case math.IsNaN(x) || math.IsInf(x, 0):
		return math.NaN(), math.NaN()
	}

	// make argument positive
	sinSign, cosSign := false, false
	if x < 0 {
		x = -x
		sinSign = true
	}

	var j uint64
	var y, z float64
	if x >= reduceThreshold {
		j, z = trigReduce(x)
	} else {
		j = uint64(x * (4 / math.Pi)) // integer part of x/(Pi/4), as integer for tests on the phase angle
		y = float64(j)                // integer part of x/(Pi/4), as float

		if j&1 == 1 { // map zeros to origin
			j++
			y++
		}
		j &= 7                                                          // octant modulo 2Pi radians (360 degrees)
		z = ((x - float64(y*PI4A)) - float64(y*PI4B)) - float64(y*PI4C) // Extended precision modular arithmetic
	}
	if j > 3 { // reflect in x axis
		j -= 4
		sinSign, cosSign = !sinSign, !cosSign
	}
	if j > 1 {
		cosSign = !cosSign
	}

	zz := z * z
	cos = 1.0 - float64(0.5*zz) + float64(zz*zz*(float64((float64((float64((float64((float64(_cos[0]*zz)+_cos[1])*zz)+_cos[2])*zz)+_cos[3])*zz)+_cos[4])*zz)+_cos[5]))
	sin = z + float64(z*zz*(float64((float64((float64((float64((float64(_sin[0]*zz)+_sin[1])*zz)+_sin[2])*zz)+_sin[3])*zz)+_sin[4])*zz)+_sin[5]))
	if j == 1 || j == 2 {
		sin, cos = cos, sin
	}
	if cosSign {
		cos = -cos
	}
	if sinSign {
		sin = -sin
	}
	return
}
