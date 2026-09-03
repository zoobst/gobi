package gobi

import (
	"math"

	"github.com/zoobst/gobi/compute"
)

// tryAndFusionFastPath detects AND-chained scalar comparisons on
// Float64 columns and dispatches to a fused compute kernel in a
// single pass — no intermediate boolean-column materialization.
//
// # Recognized shapes
//
//   - `Col(x) >=/> lo AND Col(x) <=/< hi` (same column, two-sided
//     range) → `compute.AndChainF64Range`.
//   - `Col(a) BETWEEN aLo AND aHi AND Col(b) BETWEEN bLo AND bHi`
//     (two-column bbox filter, four scalar comparisons ANDed
//     together in the standard bbox shape) → `compute.AndChainF64BBox`.
//
// The kernels use inclusive bounds; strict-inequality inputs
// (`>` / `<`) are converted via `math.Nextafter` so the fused
// output matches the two-step composition bit-for-bit on typical
// float64 data. Comparison ops other than the four order ops
// don't match; the caller falls through to the general AND path.
//
// Returns (result, matched, err). matched=false means no
// recognized pattern — the caller should take the general path.
func tryAndFusionFastPath(n *binOpNode, input *Frame) (Series, bool, error) {
	// Shape 1: `binOpNode(AND, binOpNode(AND, cmp, cmp), binOpNode(AND, cmp, cmp))`
	// — four-cmp bbox filter. Try before the two-cmp range case so
	// nested AND chains dispatch to the widest kernel.
	if s, ok, err := tryBBoxFusion(n, input); err != nil || ok {
		return s, ok, err
	}
	// Shape 2: two-sided range on a single column.
	if s, ok, err := tryRangeFusion(n, input); err != nil || ok {
		return s, ok, err
	}
	return Series{}, false, nil
}

// tryRangeFusion detects `Col(x) op1 lit1 AND Col(x) op2 lit2` on
// the same column with orderable ops, dispatching to
// compute.AndChainF64Range.
func tryRangeFusion(n *binOpNode, input *Frame) (Series, bool, error) {
	leftCol, leftOp, leftLit, ok := destructureScalarCmp(n.left)
	if !ok {
		return Series{}, false, nil
	}
	rightCol, rightOp, rightLit, ok := destructureScalarCmp(n.right)
	if !ok {
		return Series{}, false, nil
	}
	if leftCol != rightCol {
		return Series{}, false, nil
	}
	lo, hi, ok := normalizeRange(leftOp, leftLit, rightOp, rightLit)
	if !ok {
		return Series{}, false, nil
	}
	// Column must be single-chunk non-null Float64 for the kernel;
	// otherwise fall through to the general two-step AND path (which
	// handles nulls correctly).
	series, err := input.Column(leftCol)
	if err != nil {
		return Series{}, false, err
	}
	vals, arr, ok := series.singleF64()
	if !ok {
		return Series{}, false, nil
	}
	if arr.NullN() != 0 {
		return Series{}, false, nil
	}
	out := make([]bool, len(vals))
	compute.AndChainF64Range(vals, lo, hi, out)
	return buildBoolSeries(series.name, out, nil), true, nil
}

// tryBBoxFusion detects the four-cmp bbox pattern:
// `(colA op aLoLit AND colA op aHiLit) AND (colB op bLoLit AND colB op bHiLit)`.
// Dispatches to compute.AndChainF64BBox on match.
func tryBBoxFusion(n *binOpNode, input *Frame) (Series, bool, error) {
	// Both children must themselves be AND of two scalar comparisons.
	leftAND, ok := n.left.(*binOpNode)
	if !ok || leftAND.op != bopAnd {
		return Series{}, false, nil
	}
	rightAND, ok := n.right.(*binOpNode)
	if !ok || rightAND.op != bopAnd {
		return Series{}, false, nil
	}
	colA, opAL, litAL, ok := destructureScalarCmp(leftAND.left)
	if !ok {
		return Series{}, false, nil
	}
	colA2, opAH, litAH, ok := destructureScalarCmp(leftAND.right)
	if !ok || colA != colA2 {
		return Series{}, false, nil
	}
	colB, opBL, litBL, ok := destructureScalarCmp(rightAND.left)
	if !ok {
		return Series{}, false, nil
	}
	colB2, opBH, litBH, ok := destructureScalarCmp(rightAND.right)
	if !ok || colB != colB2 {
		return Series{}, false, nil
	}
	aLo, aHi, ok := normalizeRange(opAL, litAL, opAH, litAH)
	if !ok {
		return Series{}, false, nil
	}
	bLo, bHi, ok := normalizeRange(opBL, litBL, opBH, litBH)
	if !ok {
		return Series{}, false, nil
	}
	seriesA, err := input.Column(colA)
	if err != nil {
		return Series{}, false, err
	}
	seriesB, err := input.Column(colB)
	if err != nil {
		return Series{}, false, err
	}
	aVals, aArr, ok := seriesA.singleF64()
	if !ok || aArr.NullN() != 0 {
		return Series{}, false, nil
	}
	bVals, bArr, ok := seriesB.singleF64()
	if !ok || bArr.NullN() != 0 || len(bVals) != len(aVals) {
		return Series{}, false, nil
	}
	out := make([]bool, len(aVals))
	compute.AndChainF64BBox(aVals, aLo, aHi, bVals, bLo, bHi, out)
	return buildBoolSeries(seriesA.name, out, nil), true, nil
}

// destructureScalarCmp returns (colName, op, literalValue, ok) if
// the node is a `Col(name) OP literal` shape with an order-comparison
// op and a numeric literal.
func destructureScalarCmp(node ExprNode) (string, binOpKind, float64, bool) {
	bin, ok := node.(*binOpNode)
	if !ok {
		return "", 0, 0, false
	}
	if !isOrderCmp(bin.op) {
		return "", 0, 0, false
	}
	col, ok := bin.left.(*colRefNode)
	if !ok {
		return "", 0, 0, false
	}
	lit, ok := bin.right.(*literalNode)
	if !ok || lit.err != nil {
		return "", 0, 0, false
	}
	v, ok := lit.asFloat64()
	if !ok {
		return "", 0, 0, false
	}
	return col.name, bin.op, v, true
}

// normalizeRange converts two scalar comparisons on the same column
// into an inclusive (lo, hi) pair. Strict inequalities are bumped
// via math.Nextafter so `x > lo` becomes `x >= nextafter(lo, +∞)`
// — bit-exact match with the two-step composition on any float64
// input.
//
// Returns ok=false when the two ops don't form a valid two-sided
// range (both upper, both lower, or unsupported ops).
func normalizeRange(op1 binOpKind, v1 float64, op2 binOpKind, v2 float64) (lo, hi float64, ok bool) {
	lo1, hi1, ok1 := bounds(op1, v1)
	lo2, hi2, ok2 := bounds(op2, v2)
	if !ok1 || !ok2 {
		return 0, 0, false
	}
	// One side must supply lo, the other hi. tightest lower + tightest upper.
	loVal := math.Inf(-1)
	hiVal := math.Inf(1)
	if !math.IsInf(lo1, -1) {
		loVal = lo1
	}
	if !math.IsInf(lo2, -1) {
		if lo2 > loVal {
			loVal = lo2
		}
	}
	if !math.IsInf(hi1, 1) {
		hiVal = hi1
	}
	if !math.IsInf(hi2, 1) {
		if hi2 < hiVal {
			hiVal = hi2
		}
	}
	if math.IsInf(loVal, -1) || math.IsInf(hiVal, 1) {
		// Not a proper two-sided range (both ops were on the same
		// side, e.g. `x > 0 AND x > 5`).
		return 0, 0, false
	}
	return loVal, hiVal, true
}

// bounds returns (lo, hi) for a scalar comparison, using +/-Inf on
// the unbounded side. Strict inequalities are bumped via
// math.Nextafter so callers get an inclusive range that matches
// bit-for-bit.
func bounds(op binOpKind, v float64) (lo, hi float64, ok bool) {
	switch op {
	case bopGe:
		return v, math.Inf(1), true
	case bopGt:
		return math.Nextafter(v, math.Inf(1)), math.Inf(1), true
	case bopLe:
		return math.Inf(-1), v, true
	case bopLt:
		return math.Inf(-1), math.Nextafter(v, math.Inf(-1)), true
	}
	return 0, 0, false
}

// isOrderCmp reports whether op is one of the four order-comparison
// operators (>=, >, <=, <). Eq/Ne are excluded — they don't form
// a range check.
func isOrderCmp(op binOpKind) bool {
	return op == bopGe || op == bopGt || op == bopLe || op == bopLt
}
