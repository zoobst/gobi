// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE-go file.

// Package detmath is the part of the standard math package that gobi's
// geometry needs, giving the same bits on every platform.
//
// The standard functions don't. Go lets the compiler fuse x*y + z into
// one FMA instruction that rounds once instead of twice, and it does
// so on arm64, ppc64, s390x, riscv64 and amd64 at GOAMD64=v3, but not
// on wasm or on amd64 by default. The pure-Go math functions are full
// of such expressions, so math.Sin, Cos, Atan2, Hypot, Log and most
// others differ in their last bit between an Apple Silicon machine and
// a browser. A few also have assembly versions on some architectures
// (Exp on amd64 and arm64), which differ again. For a GIS that runs
// the same operation in the browser and on a server, that is a buffer
// or a reprojection whose coordinates don't quite match.
//
// The functions here are the standard library's own algorithms,
// copied from math (Go 1.27) with three changes:
//
//   - every product that feeds an addition or subtraction is rounded
//     first, with an explicit float64 conversion, which the language
//     guarantees prevents fusing;
//   - the per-architecture assembly dispatch is removed;
//   - helpers that are exact everywhere (Abs, Sqrt, Frexp, Ldexp,
//     IsNaN and the like) are called from math.
//
// So on every platform each function returns what the standard one
// returns where nothing is fused or replaced by assembly: on wasm,
// detmath.F(x) == math.F(x) for every x, which the tests check.
// Accuracy is that of the standard library.
package detmath

// The IEEE 754 double layout constants math uses internally.
const (
	mask  = 0x7FF
	shift = 64 - 11 - 1
	bias  = 1023
)
