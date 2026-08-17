// Package compute provides vectorized numeric kernels used by
// gobi's compute layer. Kernels dispatch to a SIMD implementation
// when built with `GOEXPERIMENT=simd` on a supported architecture
// (arm64 NEON, amd64 AVX2/AVX-512), and to a portable scalar
// implementation everywhere else.
//
// # Positioning vs arrow-go/arrow/compute
//
// arrow-go ships its own compute package (a "native-go Acero-like
// engine") at `arrow.compute`. Its Datum-based function-registry
// API is much more general than gobi's typed-slice kernels — it
// handles chunked arrays, arbitrary arrow types, promotion rules,
// null propagation, and overflow modes uniformly. That generality
// costs per-row overhead.
//
// Measured on arm64 (Apple M3 Pro, arrow-go v18.7.0, Go 1.27rc1)
// via `benchmarks/arrow_compute/main.go`:
//
//	Float64 Add, n=10M rows
//	  gobi   Series.Add:      755 ps/row  (1324 Mrows/s)
//	  arrow  compute.Add:    1570 ps/row  ( 637 Mrows/s)   2.1× slower
//
//	Float64 → Int64 Cast, n=10M rows
//	  gobi   Cast:            3838 ps/row  ( 261 Mrows/s)
//	  arrow  CastArray:        275 ps/row  (3629 Mrows/s)  13.9× faster
//
// Two entirely different stories:
//
//   - For Add (and any arithmetic), the 2× gap holds at 100K, 1M,
//     and 10M rows. That's per-row dispatch cost inside arrow-go's
//     `exec.ArrayKernelExec` + `ExecSpan` traversal, not a flat
//     setup tax that amortizes at large N. On amd64, arrow-go's
//     hand-written AVX2 SIMD kernels close some of that gap on
//     arithmetic, but a 4-lane float64 SIMD win (theoretical 4×)
//     vs a 2× per-row dispatch penalty nets out to a wash — not
//     a transformative win.
//
//   - For Cast, arrow-go's hand-written NEON SIMD kernel
//     (`internal/kernels/cast_numeric_neon_arm64.s`) crushes
//     gobi's scalar builder loop 14× on arm64. The batch nature
//     of casts amortizes the dispatch overhead; the SIMD kernel
//     dominates the runtime.
//
// Overlap surface (measured, not aspirational):
//
//	category                  arrow-go/compute       gobi
//	------------------------  ---------------------  --------------------
//	Add / Sub / Mul / Div     amd64 SIMD (.s files); keep gobi's — arrow-go's
//	                          arm64 scalar           per-row dispatch is 2× overhead
//	Type casts                amd64 + arm64 SIMD     ✅ use arrow-go's — 14× faster
//	                                                 on arm64 (measured)
//	Constant-factor scalar    amd64 SIMD             use arrow-go's on amd64
//	Compare (Eq/Ne/Lt/…)      amd64 SIMD (.s files); keep gobi's — measured 1.4×
//	                          arm64 scalar           slower on arm64 via arrow-go
//	Fused compare chains      absent                 unique (BBox, Range,
//	                                                 WithinSqDist)
//	Reductions (Sum/Min/Max)  absent                 unique when landed
//	arm64 SIMD arithmetic     absent                 landing via Go 1.27
//
// # Takeaway
//
// gobi/compute isn't a replacement for arrow-go/compute — different
// design points. arrow-go/compute is the right choice for one-shot
// batch operations where the Datum overhead is amortized across
// the batch AND where the general-arrow-type dispatch actually
// matters. gobi/compute is the right choice for the tight-inner-
// loop shapes on the LazyFrame hot path: comparisons, fused compare
// chains, and (eventually) reductions — the operations that recur
// per-row in filter/groupby/aggregate pipelines where 2× per-row
// dispatch overhead is prohibitive.
//
// Survey done in v0.3.9: measured Add / Ge / Cast against
// arrow-go/compute at n = 100K, 1M, 10M. Cast was the sole clear
// win (13.9× on arm64 via `cast_numeric_neon_arm64.s`) and got
// wired into `Expr.Cast`. Everything else in arrow-go's kernel
// surface either has SIMD only on amd64 (arithmetic, compare,
// constant-factor mul — Datum dispatch narrows the SIMD win to
// a wash) or has no SIMD at all (Boolean ops, string compare,
// rounding, sort, filter, hash, set lookup, temporal casts —
// scalar Go behind a 2× dispatch tax). arrow-go has NO
// aggregate/reduction kernels at all — Sum/Min/Max/Mean/etc.
// remain gobi's territory.
//
// # Surface stability
//
// The compute package is INTERNAL to gobi. Its API is not covered
// by the top-level gobi module's SemVer guarantees — kernels are
// added, renamed, or specialized without notice. Callers outside
// gobi should use the higher-level Series / Frame / Expr surface
// instead.
//
// # Build tags
//
// The SIMD path lives behind `//go:build goexperiment.simd && (arm64
// || amd64)`. When Go 1.27 stabilizes and the simd experiment
// graduates, that tag simplifies to just the arch check. Until
// then, code linked without `GOEXPERIMENT=simd` gets the scalar
// path — this file provides the entry points; the specific
// implementations live in *_simd.go / *_scalar.go pairs alongside.
//
// # Adding a kernel
//
// Every kernel is a plain function with typed slice arguments and
// a `[]bool` or scalar output. No arrow types, no gobi types — that
// keeps the SIMD-friendly inner loops free of any indirection the
// compiler can't lower. Callers convert to/from arrow at the
// package boundary.
//
// Kernel signature convention:
//
//	Cmp<Type><Op>(a []<Type>, b <Type>, out []bool)
//	    element-wise compare against a scalar; writes into out.
//	Cmp<Type><Op>Vec(a, b []<Type>, out []bool)
//	    element-wise compare against a same-length column.
//	Sum<Type>(a []<Type>) <Type>
//	Min<Type>(a []<Type>) (<Type>, bool)   — bool = "saw a value"
//	Max<Type>(a []<Type>) (<Type>, bool)
//
// Before adding an arithmetic-shaped kernel here, check whether
// arrow-go/compute already covers it — most likely it does with
// better amd64 SIMD than we could match. Fused shapes and
// comparisons are our unique territory.
//
// Every kernel is safe to call with zero-length inputs and produces
// the identity value (0 / (0,false) / no-op writes).
package compute

// Enabled reports whether this build has SIMD kernels compiled in.
// True when built with `GOEXPERIMENT=simd` on arm64 or amd64; false
// otherwise (portable scalar fallback active). Callers don't need
// to branch on this — kernel dispatch happens at build time — but
// tests and benchmarks may want to log which path is running.
func Enabled() bool { return simdEnabled }
