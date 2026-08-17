package gobi

import (
	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
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
		// Timestamp literals aren't handled here — the general
		// filter path can compare Timestamp cols against int64/f64
		// literals via numericAt widening, but that's a rare
		// filter shape. Reject to keep the fused semantics tight.
		return fusedFilterLeaf{}, false
	}
	return fusedFilterLeaf{}, false
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
