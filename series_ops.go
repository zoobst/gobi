package gobi

import (
	"fmt"
	"math"

	"github.com/apache/arrow/go/v18/arrow"
	"github.com/apache/arrow/go/v18/arrow/array"
	"github.com/apache/arrow/go/v18/arrow/memory"
)

// ErrNotNumeric is returned when arithmetic is attempted on a non-numeric
// Series. Only int64/int32/float64/float32 are currently supported.
var ErrNotNumeric = fmt.Errorf("gobi: series is not numeric")

// numericAt returns the value at row i as float64 plus a validity flag.
// It walks chunks and does a type-switched value lookup — significantly
// slower than the batched kernels below and used only as a fallback (or by
// callers that genuinely need per-row semantics, e.g. GroupBy).
func (s Series) numericAt(i int) (float64, bool, error) {
	if i < 0 || i >= s.Len() {
		return 0, false, fmt.Errorf("%w: %d not in [0,%d)", ErrRowOutOfRange, i, s.Len())
	}
	offset := 0
	for _, chunk := range s.col.Data().Chunks() {
		if i < offset+chunk.Len() {
			local := i - offset
			switch a := chunk.(type) {
			case *array.Float64:
				if a.IsNull(local) {
					return 0, false, nil
				}
				return a.Value(local), true, nil
			case *array.Int64:
				if a.IsNull(local) {
					return 0, false, nil
				}
				return float64(a.Value(local)), true, nil
			case *array.Float32:
				if a.IsNull(local) {
					return 0, false, nil
				}
				return float64(a.Value(local)), true, nil
			case *array.Int32:
				if a.IsNull(local) {
					return 0, false, nil
				}
				return float64(a.Value(local)), true, nil
			default:
				return 0, false, fmt.Errorf("%w: %s", ErrNotNumeric, s.DataType())
			}
		}
		offset += chunk.Len()
	}
	return 0, false, fmt.Errorf("%w: index %d unreachable", ErrRowOutOfRange, i)
}

// isNumeric reports whether s carries a supported numeric Arrow type.
func (s Series) isNumeric() bool {
	if s.col == nil {
		return false
	}
	switch s.DataType().ID() {
	case arrow.FLOAT64, arrow.FLOAT32, arrow.INT64, arrow.INT32:
		return true
	default:
		return false
	}
}

// singleF64 returns the underlying float64 slice, the array (for null
// bitmap access), and ok=true when s is a single-chunk Float64 Series.
func (s Series) singleF64() (vals []float64, arr *array.Float64, ok bool) {
	chunks := s.col.Data().Chunks()
	if len(chunks) != 1 {
		return nil, nil, false
	}
	a, isF64 := chunks[0].(*array.Float64)
	if !isF64 {
		return nil, nil, false
	}
	return a.Float64Values(), a, true
}

// singleI64 mirrors singleF64 for Int64.
func (s Series) singleI64() (vals []int64, arr *array.Int64, ok bool) {
	chunks := s.col.Data().Chunks()
	if len(chunks) != 1 {
		return nil, nil, false
	}
	a, isI64 := chunks[0].(*array.Int64)
	if !isI64 {
		return nil, nil, false
	}
	return a.Int64Values(), a, true
}

// bothInt reports whether both operands are integer-typed.
func bothInt(a, b Series) bool {
	aInt := a.DataType().ID() == arrow.INT64 || a.DataType().ID() == arrow.INT32
	bInt := b.DataType().ID() == arrow.INT64 || b.DataType().ID() == arrow.INT32
	return aInt && bInt
}

// ---------- binary arithmetic ----------

// Add returns s + o element-wise.
func (s Series) Add(o Series) (Series, error) {
	return s.arith(o, opAdd, false)
}

// Sub returns s - o element-wise.
func (s Series) Sub(o Series) (Series, error) {
	return s.arith(o, opSub, false)
}

// Mul returns s * o element-wise.
func (s Series) Mul(o Series) (Series, error) {
	return s.arith(o, opMul, false)
}

// Div returns s / o element-wise, promoting to float64. Division by zero
// yields ±Inf or NaN per IEEE 754.
func (s Series) Div(o Series) (Series, error) {
	return s.arith(o, opDiv, true)
}

// arithOp identifies the elementwise binary operation. Kept as a small enum
// so kernels can dispatch to native operators (which the compiler inlines)
// rather than a function pointer per element.
type arithOp uint8

const (
	opAdd arithOp = iota
	opSub
	opMul
	opDiv
)

func (s Series) arith(o Series, op arithOp, wantFloat bool) (Series, error) {
	if !s.isNumeric() || !o.isNumeric() {
		return Series{}, fmt.Errorf("%w", ErrNotNumeric)
	}
	if s.Len() != o.Len() {
		return Series{}, fmt.Errorf("%w: %d vs %d", ErrColumnLenMismatch, s.Len(), o.Len())
	}
	// Fast paths (single-chunk, matching primitive type).
	if !wantFloat && bothInt(s, o) {
		if aVals, aArr, ok := s.singleI64(); ok {
			if bVals, bArr, ok := o.singleI64(); ok {
				return arithI64I64(aVals, bVals, aArr, bArr, op, s.name), nil
			}
		}
	}
	if aVals, aArr, ok := s.singleF64(); ok {
		if bVals, bArr, ok := o.singleF64(); ok {
			return arithF64F64(aVals, bVals, aArr, bArr, op, s.name), nil
		}
	}
	return s.arithSlow(o, op, wantFloat)
}

// arithF64F64 is the batched float64-only kernel. Reads both operand
// slices, writes a preallocated result slice, and constructs an Arrow
// Float64 array from it directly. Skips per-element null checks entirely
// when both operands have zero nulls.
func arithF64F64(a, b []float64, aArr, bArr *array.Float64, op arithOp, name string) Series {
	n := len(a)
	out := make([]float64, n)
	aNulls := aArr.NullN() > 0
	bNulls := bArr.NullN() > 0
	if !aNulls && !bNulls {
		switch op {
		case opAdd:
			addF64Kernel(out, a, b)
		case opSub:
			subF64Kernel(out, a, b)
		case opMul:
			mulF64Kernel(out, a, b)
		case opDiv:
			divF64Kernel(out, a, b)
		}
		return buildFloat64Series(name, out, nil)
	}
	validity := make([]bool, n)
	for i := 0; i < n; i++ {
		if aArr.IsNull(i) || bArr.IsNull(i) {
			continue
		}
		switch op {
		case opAdd:
			out[i] = a[i] + b[i]
		case opSub:
			out[i] = a[i] - b[i]
		case opMul:
			out[i] = a[i] * b[i]
		case opDiv:
			out[i] = a[i] / b[i]
		}
		validity[i] = true
	}
	return buildFloat64Series(name, out, validity)
}

// arithI64I64 is the integer-integer batched kernel. Add/Sub/Mul produce
// Int64; Div is never routed here (Div goes through the float64 kernel).
func arithI64I64(a, b []int64, aArr, bArr *array.Int64, op arithOp, name string) Series {
	n := len(a)
	out := make([]int64, n)
	aNulls := aArr.NullN() > 0
	bNulls := bArr.NullN() > 0
	if !aNulls && !bNulls {
		switch op {
		case opAdd:
			for i := 0; i < n; i++ {
				out[i] = a[i] + b[i]
			}
		case opSub:
			for i := 0; i < n; i++ {
				out[i] = a[i] - b[i]
			}
		case opMul:
			for i := 0; i < n; i++ {
				out[i] = a[i] * b[i]
			}
		}
		return buildInt64Series(name, out, nil)
	}
	validity := make([]bool, n)
	for i := 0; i < n; i++ {
		if aArr.IsNull(i) || bArr.IsNull(i) {
			continue
		}
		switch op {
		case opAdd:
			out[i] = a[i] + b[i]
		case opSub:
			out[i] = a[i] - b[i]
		case opMul:
			out[i] = a[i] * b[i]
		}
		validity[i] = true
	}
	return buildInt64Series(name, out, validity)
}

// arithSlow is the general per-row fallback for mixed-type or multi-chunk
// operands.
func (s Series) arithSlow(o Series, op arithOp, wantFloat bool) (Series, error) {
	pool := memory.DefaultAllocator
	n := s.Len()
	if wantFloat || !bothInt(s, o) {
		b := array.NewFloat64Builder(pool)
		defer b.Release()
		for i := 0; i < n; i++ {
			av, aok, err := s.numericAt(i)
			if err != nil {
				return Series{}, err
			}
			bv, bok, err := o.numericAt(i)
			if err != nil {
				return Series{}, err
			}
			if !aok || !bok {
				b.AppendNull()
				continue
			}
			b.Append(applyF64(op, av, bv))
		}
		return newSeriesFromArray(s.name, b.NewArray()), nil
	}
	b := array.NewInt64Builder(pool)
	defer b.Release()
	for i := 0; i < n; i++ {
		av, aok, err := s.numericAt(i)
		if err != nil {
			return Series{}, err
		}
		bv, bok, err := o.numericAt(i)
		if err != nil {
			return Series{}, err
		}
		if !aok || !bok {
			b.AppendNull()
			continue
		}
		b.Append(int64(applyF64(op, av, bv)))
	}
	return newSeriesFromArray(s.name, b.NewArray()), nil
}

func applyF64(op arithOp, a, b float64) float64 {
	switch op {
	case opAdd:
		return a + b
	case opSub:
		return a - b
	case opMul:
		return a * b
	case opDiv:
		return a / b
	}
	return 0
}

// ---------- scalar arithmetic ----------

// AddScalar returns s + v element-wise (result is float64).
func (s Series) AddScalar(v float64) (Series, error) { return s.scalar(v, opAdd) }

// SubScalar returns s - v element-wise.
func (s Series) SubScalar(v float64) (Series, error) { return s.scalar(v, opSub) }

// MulScalar returns s * v element-wise.
func (s Series) MulScalar(v float64) (Series, error) { return s.scalar(v, opMul) }

// DivScalar returns s / v element-wise.
func (s Series) DivScalar(v float64) (Series, error) { return s.scalar(v, opDiv) }

func (s Series) scalar(v float64, op arithOp) (Series, error) {
	if !s.isNumeric() {
		return Series{}, ErrNotNumeric
	}
	// Fast path: float64 single chunk.
	if vals, arr, ok := s.singleF64(); ok {
		return scalarF64(vals, arr, v, op, s.name), nil
	}
	// Slow path.
	pool := memory.DefaultAllocator
	b := array.NewFloat64Builder(pool)
	defer b.Release()
	n := s.Len()
	for i := 0; i < n; i++ {
		av, aok, err := s.numericAt(i)
		if err != nil {
			return Series{}, err
		}
		if !aok {
			b.AppendNull()
			continue
		}
		b.Append(applyF64(op, av, v))
	}
	return newSeriesFromArray(s.name, b.NewArray()), nil
}

func scalarF64(a []float64, arr *array.Float64, v float64, op arithOp, name string) Series {
	n := len(a)
	out := make([]float64, n)
	if arr.NullN() == 0 {
		switch op {
		case opAdd:
			addScalarF64Kernel(out, a, v)
		case opSub:
			// Reuse addScalar with negated v when SIMD is available;
			// the scalar fallback still gets a tight loop.
			addScalarF64Kernel(out, a, -v)
		case opMul:
			mulScalarF64Kernel(out, a, v)
		case opDiv:
			mulScalarF64Kernel(out, a, 1/v)
		}
		return buildFloat64Series(name, out, nil)
	}
	validity := make([]bool, n)
	for i := 0; i < n; i++ {
		if arr.IsNull(i) {
			continue
		}
		switch op {
		case opAdd:
			out[i] = a[i] + v
		case opSub:
			out[i] = a[i] - v
		case opMul:
			out[i] = a[i] * v
		case opDiv:
			out[i] = a[i] / v
		}
		validity[i] = true
	}
	return buildFloat64Series(name, out, validity)
}

// ---------- aggregations ----------

// Sum returns the sum of non-null numeric values.
func (s Series) Sum() (float64, error) {
	if !s.isNumeric() {
		return 0, ErrNotNumeric
	}
	if vals, arr, ok := s.singleF64(); ok {
		return sumF64(vals, arr), nil
	}
	if vals, arr, ok := s.singleI64(); ok {
		return sumI64(vals, arr), nil
	}
	// Slow path.
	var total float64
	for i := 0; i < s.Len(); i++ {
		v, ok, err := s.numericAt(i)
		if err != nil {
			return 0, err
		}
		if ok {
			total += v
		}
	}
	return total, nil
}

func sumF64(a []float64, arr *array.Float64) float64 {
	if arr.NullN() == 0 {
		return sumF64Kernel(a)
	}
	var total float64
	for i, v := range a {
		if !arr.IsNull(i) {
			total += v
		}
	}
	return total
}

func sumI64(a []int64, arr *array.Int64) float64 {
	var total int64
	if arr.NullN() == 0 {
		for _, v := range a {
			total += v
		}
		return float64(total)
	}
	for i, v := range a {
		if !arr.IsNull(i) {
			total += v
		}
	}
	return float64(total)
}

// Mean returns the arithmetic mean of non-null numeric values, or NaN when
// the series has no non-null rows.
func (s Series) Mean() (float64, error) {
	if !s.isNumeric() {
		return 0, ErrNotNumeric
	}
	if vals, arr, ok := s.singleF64(); ok {
		return meanF64(vals, arr), nil
	}
	if vals, arr, ok := s.singleI64(); ok {
		return meanI64(vals, arr), nil
	}
	var total float64
	var n int
	for i := 0; i < s.Len(); i++ {
		v, ok, err := s.numericAt(i)
		if err != nil {
			return 0, err
		}
		if ok {
			total += v
			n++
		}
	}
	if n == 0 {
		return math.NaN(), nil
	}
	return total / float64(n), nil
}

func meanF64(a []float64, arr *array.Float64) float64 {
	var total float64
	if arr.NullN() == 0 {
		if len(a) == 0 {
			return math.NaN()
		}
		for _, v := range a {
			total += v
		}
		return total / float64(len(a))
	}
	var n int
	for i, v := range a {
		if !arr.IsNull(i) {
			total += v
			n++
		}
	}
	if n == 0 {
		return math.NaN()
	}
	return total / float64(n)
}

func meanI64(a []int64, arr *array.Int64) float64 {
	var total int64
	if arr.NullN() == 0 {
		if len(a) == 0 {
			return math.NaN()
		}
		for _, v := range a {
			total += v
		}
		return float64(total) / float64(len(a))
	}
	var n int
	for i, v := range a {
		if !arr.IsNull(i) {
			total += v
			n++
		}
	}
	if n == 0 {
		return math.NaN()
	}
	return float64(total) / float64(n)
}

// Min returns the minimum non-null numeric value, or NaN when the series
// has no non-null rows.
func (s Series) Min() (float64, error) {
	if !s.isNumeric() {
		return 0, ErrNotNumeric
	}
	if vals, arr, ok := s.singleF64(); ok {
		return minMaxF64(vals, arr, true), nil
	}
	if vals, arr, ok := s.singleI64(); ok {
		return minMaxI64(vals, arr, true), nil
	}
	m := math.Inf(1)
	any := false
	for i := 0; i < s.Len(); i++ {
		v, ok, err := s.numericAt(i)
		if err != nil {
			return 0, err
		}
		if !ok {
			continue
		}
		any = true
		if v < m {
			m = v
		}
	}
	if !any {
		return math.NaN(), nil
	}
	return m, nil
}

// Max returns the maximum non-null numeric value, or NaN when the series
// has no non-null rows.
func (s Series) Max() (float64, error) {
	if !s.isNumeric() {
		return 0, ErrNotNumeric
	}
	if vals, arr, ok := s.singleF64(); ok {
		return minMaxF64(vals, arr, false), nil
	}
	if vals, arr, ok := s.singleI64(); ok {
		return minMaxI64(vals, arr, false), nil
	}
	m := math.Inf(-1)
	any := false
	for i := 0; i < s.Len(); i++ {
		v, ok, err := s.numericAt(i)
		if err != nil {
			return 0, err
		}
		if !ok {
			continue
		}
		any = true
		if v > m {
			m = v
		}
	}
	if !any {
		return math.NaN(), nil
	}
	return m, nil
}

func minMaxF64(a []float64, arr *array.Float64, wantMin bool) float64 {
	if len(a) == 0 {
		return math.NaN()
	}
	if arr.NullN() == 0 {
		m := a[0]
		if wantMin {
			for _, v := range a[1:] {
				if v < m {
					m = v
				}
			}
		} else {
			for _, v := range a[1:] {
				if v > m {
					m = v
				}
			}
		}
		return m
	}
	m := math.Inf(1)
	if !wantMin {
		m = math.Inf(-1)
	}
	any := false
	for i, v := range a {
		if arr.IsNull(i) {
			continue
		}
		any = true
		if wantMin && v < m {
			m = v
		}
		if !wantMin && v > m {
			m = v
		}
	}
	if !any {
		return math.NaN()
	}
	return m
}

func minMaxI64(a []int64, arr *array.Int64, wantMin bool) float64 {
	if len(a) == 0 {
		return math.NaN()
	}
	if arr.NullN() == 0 {
		m := a[0]
		if wantMin {
			for _, v := range a[1:] {
				if v < m {
					m = v
				}
			}
		} else {
			for _, v := range a[1:] {
				if v > m {
					m = v
				}
			}
		}
		return float64(m)
	}
	var m int64
	initialized := false
	any := false
	for i, v := range a {
		if arr.IsNull(i) {
			continue
		}
		any = true
		if !initialized {
			m = v
			initialized = true
			continue
		}
		if wantMin && v < m {
			m = v
		}
		if !wantMin && v > m {
			m = v
		}
	}
	if !any {
		return math.NaN()
	}
	return float64(m)
}

// Count returns the number of non-null rows.
func (s Series) Count() int {
	if s.col == nil {
		return 0
	}
	var n int
	for _, chunk := range s.col.Data().Chunks() {
		n += chunk.Len() - chunk.NullN()
	}
	return n
}

// ---------- comparisons ----------

// cmpOp identifies an elementwise comparison operator.
type cmpOp uint8

const (
	cmpEq cmpOp = iota
	cmpNe
	cmpLt
	cmpLe
	cmpGt
	cmpGe
)

// Eq returns a boolean Series that is true where s[i] == o[i].
func (s Series) Eq(o Series) (Series, error) { return s.cmp(o, cmpEq) }

// Ne returns a boolean Series that is true where s[i] != o[i].
func (s Series) Ne(o Series) (Series, error) { return s.cmp(o, cmpNe) }

// Lt returns a boolean Series that is true where s[i] < o[i].
func (s Series) Lt(o Series) (Series, error) { return s.cmp(o, cmpLt) }

// Le returns a boolean Series that is true where s[i] <= o[i].
func (s Series) Le(o Series) (Series, error) { return s.cmp(o, cmpLe) }

// Gt returns a boolean Series that is true where s[i] > o[i].
func (s Series) Gt(o Series) (Series, error) { return s.cmp(o, cmpGt) }

// Ge returns a boolean Series that is true where s[i] >= o[i].
func (s Series) Ge(o Series) (Series, error) { return s.cmp(o, cmpGe) }

// EqScalar returns a boolean Series true where s[i] == v.
func (s Series) EqScalar(v float64) (Series, error) { return s.cmpScalar(v, cmpEq) }

// LtScalar returns a boolean Series true where s[i] < v.
func (s Series) LtScalar(v float64) (Series, error) { return s.cmpScalar(v, cmpLt) }

// GtScalar returns a boolean Series true where s[i] > v.
func (s Series) GtScalar(v float64) (Series, error) { return s.cmpScalar(v, cmpGt) }

func (s Series) cmp(o Series, op cmpOp) (Series, error) {
	if !s.isNumeric() || !o.isNumeric() {
		return Series{}, ErrNotNumeric
	}
	if s.Len() != o.Len() {
		return Series{}, fmt.Errorf("%w: %d vs %d", ErrColumnLenMismatch, s.Len(), o.Len())
	}
	if aVals, aArr, ok := s.singleF64(); ok {
		if bVals, bArr, ok := o.singleF64(); ok {
			return cmpF64F64(aVals, bVals, aArr, bArr, op, s.name), nil
		}
	}
	return s.cmpSlow(o, op)
}

func cmpF64F64(a, b []float64, aArr, bArr *array.Float64, op cmpOp, name string) Series {
	n := len(a)
	out := make([]bool, n)
	aNulls := aArr.NullN() > 0
	bNulls := bArr.NullN() > 0
	if !aNulls && !bNulls {
		switch op {
		case cmpEq:
			for i := 0; i < n; i++ {
				out[i] = a[i] == b[i]
			}
		case cmpNe:
			for i := 0; i < n; i++ {
				out[i] = a[i] != b[i]
			}
		case cmpLt:
			for i := 0; i < n; i++ {
				out[i] = a[i] < b[i]
			}
		case cmpLe:
			for i := 0; i < n; i++ {
				out[i] = a[i] <= b[i]
			}
		case cmpGt:
			for i := 0; i < n; i++ {
				out[i] = a[i] > b[i]
			}
		case cmpGe:
			for i := 0; i < n; i++ {
				out[i] = a[i] >= b[i]
			}
		}
		return buildBoolSeries(name, out, nil)
	}
	validity := make([]bool, n)
	for i := 0; i < n; i++ {
		if aArr.IsNull(i) || bArr.IsNull(i) {
			continue
		}
		switch op {
		case cmpEq:
			out[i] = a[i] == b[i]
		case cmpNe:
			out[i] = a[i] != b[i]
		case cmpLt:
			out[i] = a[i] < b[i]
		case cmpLe:
			out[i] = a[i] <= b[i]
		case cmpGt:
			out[i] = a[i] > b[i]
		case cmpGe:
			out[i] = a[i] >= b[i]
		}
		validity[i] = true
	}
	return buildBoolSeries(name, out, validity)
}

func (s Series) cmpSlow(o Series, op cmpOp) (Series, error) {
	b := array.NewBooleanBuilder(memory.DefaultAllocator)
	defer b.Release()
	for i := 0; i < s.Len(); i++ {
		av, aok, err := s.numericAt(i)
		if err != nil {
			return Series{}, err
		}
		bv, bok, err := o.numericAt(i)
		if err != nil {
			return Series{}, err
		}
		if !aok || !bok {
			b.AppendNull()
			continue
		}
		b.Append(applyCmp(op, av, bv))
	}
	return newSeriesFromArray(s.name, b.NewArray()), nil
}

func (s Series) cmpScalar(v float64, op cmpOp) (Series, error) {
	if !s.isNumeric() {
		return Series{}, ErrNotNumeric
	}
	if vals, arr, ok := s.singleF64(); ok {
		return cmpScalarF64(vals, arr, v, op, s.name), nil
	}
	b := array.NewBooleanBuilder(memory.DefaultAllocator)
	defer b.Release()
	for i := 0; i < s.Len(); i++ {
		av, aok, err := s.numericAt(i)
		if err != nil {
			return Series{}, err
		}
		if !aok {
			b.AppendNull()
			continue
		}
		b.Append(applyCmp(op, av, v))
	}
	return newSeriesFromArray(s.name, b.NewArray()), nil
}

func cmpScalarF64(a []float64, arr *array.Float64, v float64, op cmpOp, name string) Series {
	n := len(a)
	out := make([]bool, n)
	if arr.NullN() == 0 {
		switch op {
		case cmpEq:
			for i := 0; i < n; i++ {
				out[i] = a[i] == v
			}
		case cmpNe:
			for i := 0; i < n; i++ {
				out[i] = a[i] != v
			}
		case cmpLt:
			for i := 0; i < n; i++ {
				out[i] = a[i] < v
			}
		case cmpLe:
			for i := 0; i < n; i++ {
				out[i] = a[i] <= v
			}
		case cmpGt:
			for i := 0; i < n; i++ {
				out[i] = a[i] > v
			}
		case cmpGe:
			for i := 0; i < n; i++ {
				out[i] = a[i] >= v
			}
		}
		return buildBoolSeries(name, out, nil)
	}
	validity := make([]bool, n)
	for i := 0; i < n; i++ {
		if arr.IsNull(i) {
			continue
		}
		switch op {
		case cmpEq:
			out[i] = a[i] == v
		case cmpNe:
			out[i] = a[i] != v
		case cmpLt:
			out[i] = a[i] < v
		case cmpLe:
			out[i] = a[i] <= v
		case cmpGt:
			out[i] = a[i] > v
		case cmpGe:
			out[i] = a[i] >= v
		}
		validity[i] = true
	}
	return buildBoolSeries(name, out, validity)
}

func applyCmp(op cmpOp, a, b float64) bool {
	switch op {
	case cmpEq:
		return a == b
	case cmpNe:
		return a != b
	case cmpLt:
		return a < b
	case cmpLe:
		return a <= b
	case cmpGt:
		return a > b
	case cmpGe:
		return a >= b
	}
	return false
}

// ---------- Series builders ----------

// buildFloat64Series returns a Series wrapping vals with the given validity
// (nil validity means all valid).
func buildFloat64Series(name string, vals []float64, validity []bool) Series {
	b := array.NewFloat64Builder(memory.DefaultAllocator)
	defer b.Release()
	b.AppendValues(vals, validity)
	return newSeriesFromArray(name, b.NewArray())
}

func buildInt64Series(name string, vals []int64, validity []bool) Series {
	b := array.NewInt64Builder(memory.DefaultAllocator)
	defer b.Release()
	b.AppendValues(vals, validity)
	return newSeriesFromArray(name, b.NewArray())
}

func buildBoolSeries(name string, vals []bool, validity []bool) Series {
	b := array.NewBooleanBuilder(memory.DefaultAllocator)
	defer b.Release()
	b.AppendValues(vals, validity)
	return newSeriesFromArray(name, b.NewArray())
}

// newSeriesFromArray wraps an arrow.Array in a Series with the given name.
// The returned Series takes ownership of arr — callers should not release
// it themselves.
func newSeriesFromArray(name string, arr arrow.Array) Series {
	field := arrow.Field{Name: name, Type: arr.DataType(), Nullable: true}
	chunked := arrow.NewChunked(arr.DataType(), []arrow.Array{arr})
	col := arrow.NewColumn(field, chunked)
	return Series{name: name, field: field, col: col}
}
