package gobi

import (
	"math"
	"unsafe"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/zoobst/gobi/compute"
)

// tryFusedFilterMask recognizes filter predicates of the shape
//
//	Col(a) OP lit AND Col(b) OP lit AND ...
//
// and evaluates them in a single row-loop, ANDing per-row rather than
// materializing one Boolean Series per comparison + one more per AND.
// Returns (mask, true, nil) on success, (_, false, nil) when the
// predicate doesn't match the fused shape — caller falls back to the
// normal Expr.Eval path unchanged.
//
// The recognized shape covers the common case of scalar-bbox +
// range filters: comparisons on primitive numeric or timestamp
// columns against numeric or timestamp literals. Predicates with
// column-vs-column comparisons, string equality, OR branches, or
// non-cmp inner nodes fall through to the general path.
//
// Short-circuits per row on the first false — high-selectivity
// filters (most rows fail early) get the biggest win.
func tryFusedFilterMask(f *Frame, e Expr) (Series, bool, error) {
	if e.node == nil {
		return Series{}, false, nil
	}
	var flat []Expr
	walkAnd(e.node, &flat)
	if len(flat) < 2 {
		// Single-leaf predicate: no fusion win. The scalar fast path
		// in binOpNode.Eval already handles (col OP lit) without
		// broadcasting; skip the pattern-match overhead.
		return Series{}, false, nil
	}

	leaves := make([]fusedFilterLeaf, 0, len(flat))
	allNullFree := true
	for _, leaf := range flat {
		l, ok := parseFusedLeaf(f, leaf.node)
		if !ok {
			return Series{}, false, nil
		}
		if l.arr.NullN() != 0 {
			allNullFree = false
		}
		leaves = append(leaves, l)
	}

	// All leaves refer to the same frame → same row count. Use the
	// first leaf's Series length as the master n (checked via the
	// column resolution in parseFusedLeaf).
	n := leaves[0].n
	for _, l := range leaves[1:] {
		if l.n != n {
			return Series{}, false, nil
		}
	}

	// Null-free fast path: every leaf's column has zero nulls, so the
	// per-row IsNull check across leaves × rows is pure overhead
	// (numeric columns from typical parquet reads are null-free —
	// this is the common shape). Skip both the IsNull calls and the
	// validity-bitmap bookkeeping.
	if allNullFree {
		out := make([]bool, n)
		for i := range n {
			keep := true
			for _, l := range leaves {
				if !l.evalRowNoNull(i) {
					keep = false
					break
				}
			}
			out[i] = keep
		}
		return buildBoolSeries("", out, nil), true, nil
	}

	// General path: any leaf may have nulls, so we propagate them
	// into the mask's validity bitmap. Frame.Filter reads null
	// entries as false, matching the general Expr.Eval semantics.
	out := make([]bool, n)
	validity := make([]bool, n)
	anyNull := false
	for i := range n {
		keep := true
		valid := true
		for _, l := range leaves {
			ok := l.evalRow(i)
			if !ok.valid {
				valid = false
				keep = false
				break
			}
			if !ok.keep {
				keep = false
				break
			}
		}
		out[i] = keep
		validity[i] = valid
		if !valid {
			anyNull = true
		}
	}

	var mask Series
	if !anyNull {
		mask = buildBoolSeries("", out, nil)
	} else {
		mask = buildBoolSeries("", out, validity)
	}
	return mask, true, nil
}

// fusedFilterEval bundles per-row output for a single leaf: valid
// tracks null propagation (any null operand ⇒ the row's overall mask
// is null); keep is the boolean result when valid=true.
type fusedFilterEval struct {
	valid bool
	keep  bool
}

// fusedFilterLeaf is a per-leaf snapshot of a scalar comparison
// against a single-chunk primitive column. Captures the column
// arrow view + null bitmap + the op + the literal scalar, so the
// per-row inner loop stays branch-light.
type fusedFilterLeaf struct {
	// kind: 1=float64, 2=int64, 3=timestamp(int64-backed)
	kind uint8
	f64  []float64
	i64  []int64
	arr  arrow.Array
	op   cmpOp
	// scalarF / scalarI hold the RHS scalar in the leaf's numeric
	// type. Only one is populated based on kind.
	scalarF float64
	scalarI int64
	// n is the source column's row count — used by the caller to
	// verify uniform length across leaves.
	n int
}

// evalRow reduces one leaf against row i.
func (l fusedFilterLeaf) evalRow(i int) fusedFilterEval {
	if l.arr.IsNull(i) {
		return fusedFilterEval{valid: false}
	}
	switch l.kind {
	case 1:
		return fusedFilterEval{valid: true, keep: cmpF64(l.op, l.f64[i], l.scalarF)}
	case 2, 3:
		return fusedFilterEval{valid: true, keep: cmpI64(l.op, l.i64[i], l.scalarI)}
	}
	return fusedFilterEval{valid: false}
}

// evalRowNoNull is the null-check-elided variant: callers verified
// at setup that l.arr.NullN() == 0, so the per-row IsNull call is
// skipped. Returns just the keep bit; validity is always true.
func (l fusedFilterLeaf) evalRowNoNull(i int) bool {
	switch l.kind {
	case 1:
		return cmpF64(l.op, l.f64[i], l.scalarF)
	case 2, 3:
		return cmpI64(l.op, l.i64[i], l.scalarI)
	}
	return false
}

// cmpF64 applies op to two float64 values. Inlined by the compiler
// once the caller's cmpOp is a constant-in-context switch — same
// pattern the per-op cmp loops in series_ops.go use.
func cmpF64(op cmpOp, a, b float64) bool {
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

// cmpI64 is the int64 counterpart to cmpF64. Timestamp comparisons
// route here (Timestamp storage is int64).
func cmpI64(op cmpOp, a, b int64) bool {
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

// parseFusedLeaf extracts a scalar-cmp leaf. Rejects and returns
// ok=false for any shape the fused evaluator doesn't handle —
// column-vs-column comparisons, string equality, unary ops,
// non-primitive columns, multi-chunk columns.
func parseFusedLeaf(f *Frame, n ExprNode) (fusedFilterLeaf, bool) {
	b, ok := n.(*binOpNode)
	if !ok || !b.op.isComparison() {
		return fusedFilterLeaf{}, false
	}
	// Two orientations: (col OP lit) and (lit OP col). Extract
	// which side is which; if the literal is on the left, flip
	// the operator so the leaf semantics stay "col OP scalar."
	var colNode *colRefNode
	var litNode *literalNode
	swap := false
	if c, isCol := b.left.(*colRefNode); isCol {
		if l, isLit := b.right.(*literalNode); isLit {
			colNode, litNode = c, l
		}
	}
	if colNode == nil {
		if c, isCol := b.right.(*colRefNode); isCol {
			if l, isLit := b.left.(*literalNode); isLit {
				colNode, litNode = c, l
				swap = true
			}
		}
	}
	if colNode == nil || litNode == nil || litNode.err != nil {
		return fusedFilterLeaf{}, false
	}

	col, err := f.Column(colNode.name)
	if err != nil {
		return fusedFilterLeaf{}, false
	}
	chunks := col.col.Data().Chunks()
	if len(chunks) != 1 {
		return fusedFilterLeaf{}, false
	}

	op := binOpToCmpOp(b.op)
	if swap {
		op = swapCmpOp(op)
	}

	switch arr := chunks[0].(type) {
	case *array.Float64:
		v, ok := litNode.asFloat64()
		if !ok {
			return fusedFilterLeaf{}, false
		}
		return fusedFilterLeaf{
			kind: 1, f64: arr.Float64Values(), arr: arr,
			op: op, scalarF: v, n: arr.Len(),
		}, true
	case *array.Int64:
		v, ok := litNode.asFloat64()
		if !ok {
			return fusedFilterLeaf{}, false
		}
		// Only accept if the literal is a losslessly-representable
		// int64. Otherwise semantics diverge from the general path
		// (which promotes to float64).
		iv := int64(v)
		if float64(iv) != v {
			return fusedFilterLeaf{}, false
		}
		return fusedFilterLeaf{
			kind: 2, i64: arr.Int64Values(), arr: arr,
			op: op, scalarI: iv, n: arr.Len(),
		}, true
	case *array.Timestamp:
		// Timestamp cmp Timestamp-lit — unit match required
		// (nanosecond-vs-microsecond would silently misorder).
		// Reject non-Timestamp literals; the general path errors
		// on those at Type() anyway, so no capability lost.
		tsLit, ok := litNode.value.(arrow.Timestamp)
		if !ok {
			return fusedFilterLeaf{}, false
		}
		colTS, okCol := colTSType(colNode, f)
		litTS, _ := litNode.dtype.(*arrow.TimestampType)
		if !okCol || litTS == nil || colTS.Unit != litTS.Unit {
			return fusedFilterLeaf{}, false
		}
		// arrow.Timestamp is int64 under the hood; unsafe.Slice-free
		// conversion via TimestampValues + reslice into []int64
		// isn't safe without unsafe, so copy through the arrow view.
		// The leaf's i64 slice aliases arrow's backing storage via
		// the arrow.Timestamp → int64 identity (both are 8-byte,
		// same layout) using a lane-count-checked reinterpret in
		// tsValuesAsInt64.
		return fusedFilterLeaf{
			kind: 3, i64: tsValuesAsInt64(arr), arr: arr,
			op: op, scalarI: int64(tsLit), n: arr.Len(),
		}, true
	}
	return fusedFilterLeaf{}, false
}

// colTsType looks up colNode's arrow type and returns the resolved
// TimestampType if the column is one. Second return is false when
// the column isn't Timestamp or the lookup failed — both fall through
// to the caller's reject path.
func colTSType(colNode *colRefNode, f *Frame) (*arrow.TimestampType, bool) {
	s, err := f.Column(colNode.name)
	if err != nil {
		return nil, false
	}
	ts, ok := s.DataType().(*arrow.TimestampType)
	return ts, ok
}

// tsValuesAsInt64 reinterprets an *array.Timestamp's backing values
// as []int64 without copying. Sound because arrow.Timestamp is
// declared `type Timestamp int64` at the arrow-go layer — identical
// size + alignment + layout. Skipping the copy matters for the
// fused-filter workload: the F64 and I64 paths get zero-copy views
// via Float64Values / Int64Values; the Timestamp path needs the same
// treatment to keep the "fused = faster" contract on 100M-row
// filters where an O(n) leaf-setup allocation would dominate.
func tsValuesAsInt64(a *array.Timestamp) []int64 {
	src := a.TimestampValues()
	if len(src) == 0 {
		return nil
	}
	return unsafe.Slice((*int64)(unsafe.Pointer(&src[0])), len(src))
}

// binOpToCmpOp translates the Expr layer's comparison operator into
// the Series layer's cmpOp enum. Comparison ops are guaranteed by
// the caller (parseFusedLeaf checked isComparison first).
func binOpToCmpOp(k binOpKind) cmpOp {
	switch k {
	case bopEq:
		return cmpEq
	case bopNe:
		return cmpNe
	case bopLt:
		return cmpLt
	case bopLe:
		return cmpLe
	case bopGt:
		return cmpGt
	case bopGe:
		return cmpGe
	}
	return cmpEq // unreachable — caller guards with isComparison
}

// swapCmpOp reverses the direction of a comparison so that
// `lit OP col` transposes to `col swap(OP) lit`. Eq/Ne are
// self-inverse; Lt↔Gt and Le↔Ge swap.
func swapCmpOp(op cmpOp) cmpOp {
	switch op {
	case cmpLt:
		return cmpGt
	case cmpLe:
		return cmpGe
	case cmpGt:
		return cmpLt
	case cmpGe:
		return cmpLe
	}
	return op
}

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
