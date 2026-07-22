package gobi

// Stats reports column-level bounds used by CanPossiblyMatch to
// prove predicates unsatisfiable over a data range (typically a
// parquet row-group). Implementations are supplied by source
// packages: parquetio, csvio, etc.
//
// The values returned by MinMax must be Go-typed scalars matching
// the column's arrow type: int64 for INT64, float64 for FLOAT64,
// string for STRING, bool for BOOL, and so on. Mixed types or
// unknown columns should signal ok=false so the caller falls back
// to the conservative "possibly matches" answer.
type Stats interface {
	// MinMax returns the inclusive bounds for col over the range
	// this Stats describes. ok=false when statistics are missing.
	MinMax(col string) (minV, maxV any, ok bool)
	// NullCount returns the number of null values in col over the
	// range. ok=false when null counts weren't recorded.
	NullCount(col string) (n int64, ok bool)
	// TotalRows is the total row count of the range.
	TotalRows() int64
}

// CanPossiblyMatch reports whether pred could be satisfied by any
// row in the range described by stats. Returns true when uncertain
// — false positives are safe (over-read); false negatives break
// correctness.
//
// Supported predicate shapes:
//
//   - AND / OR of shapes below
//   - col == literal, col != literal
//   - col <, <=, >, >= literal
//   - literal on either side (auto-normalized)
//
// Anything else (NOT, arithmetic, custom nodes) is treated as
// "possibly matches." Used by parquetio for row-group skipping;
// callable by any source package that can produce column stats.
func CanPossiblyMatch(pred Expr, stats Stats) bool {
	if pred.node == nil || stats == nil {
		return true
	}
	return canMatchNode(pred.node, stats)
}

func canMatchNode(n ExprNode, s Stats) bool {
	switch n := n.(type) {
	case *binOpNode:
		switch n.op {
		case bopAnd:
			// Skip only if both sides survive as "could match."
			return canMatchNode(n.left, s) && canMatchNode(n.right, s)
		case bopOr:
			// Skip only if BOTH sides can't match.
			return canMatchNode(n.left, s) || canMatchNode(n.right, s)
		default:
			return canMatchComparison(n.op, n.left, n.right, s)
		}
	case *literalNode:
		// A bare literal predicate: Filter(Lit(false)) can't match.
		if b, ok := n.value.(bool); ok {
			return b
		}
		return true
	case *aliasNode:
		return canMatchNode(n.inner, s)
	}
	// notNode, custom nodes, arithmetic — bail conservatively.
	return true
}

// canMatchComparison handles `col OP lit` (and `lit OP col`, by
// normalization). Returns true if the range described by stats
// possibly contains a row satisfying the comparison.
func canMatchComparison(op binOpKind, left, right ExprNode, s Stats) bool {
	col, lit, opNorm, ok := normalizeCmp(left, right, op)
	if !ok {
		return true
	}
	minV, maxV, ok := s.MinMax(col.name)
	if !ok || minV == nil || maxV == nil {
		return true
	}
	// If the entire column is null in this range, no non-null
	// comparison can match. But callers can express `col IS NULL`
	// as a separate check later; for now, treat this as a maybe.
	if nc, ok := s.NullCount(col.name); ok && nc >= s.TotalRows() {
		return true
	}

	litV := lit.value
	switch opNorm {
	case bopEq:
		// col == lit: possible iff min <= lit <= max.
		loCmp, ok1 := cmpVal(litV, minV)
		hiCmp, ok2 := cmpVal(litV, maxV)
		if !ok1 || !ok2 {
			return true
		}
		return loCmp >= 0 && hiCmp <= 0
	case bopNe:
		// col != lit: possible unless the range is a single value
		// equal to lit.
		spread, ok1 := cmpVal(minV, maxV)
		eq, ok2 := cmpVal(minV, litV)
		if !ok1 || !ok2 {
			return true
		}
		if spread == 0 && eq == 0 {
			return false
		}
		return true
	case bopLt:
		// col < lit: possible iff min < lit.
		c, ok := cmpVal(minV, litV)
		if !ok {
			return true
		}
		return c < 0
	case bopLe:
		// col <= lit: possible iff min <= lit.
		c, ok := cmpVal(minV, litV)
		if !ok {
			return true
		}
		return c <= 0
	case bopGt:
		// col > lit: possible iff max > lit.
		c, ok := cmpVal(maxV, litV)
		if !ok {
			return true
		}
		return c > 0
	case bopGe:
		// col >= lit: possible iff max >= lit.
		c, ok := cmpVal(maxV, litV)
		if !ok {
			return true
		}
		return c >= 0
	}
	return true
}

// normalizeCmp attempts to interpret a binary op as `col OP lit`.
// If the literal is on the left it flips the operator (`lit > col`
// → `col < lit`). Returns ok=false when both sides are non-literals
// or both are literals — those cases don't map to a col-vs-scalar
// range check.
func normalizeCmp(left, right ExprNode, op binOpKind) (*colRefNode, *literalNode, binOpKind, bool) {
	lCol, lIsCol := left.(*colRefNode)
	rCol, rIsCol := right.(*colRefNode)
	lLit, lIsLit := left.(*literalNode)
	rLit, rIsLit := right.(*literalNode)

	if lIsCol && rIsLit {
		return lCol, rLit, op, true
	}
	if lIsLit && rIsCol {
		// Flip literal to the right and reverse the op sense.
		return rCol, lLit, flipCmp(op), true
	}
	return nil, nil, 0, false
}

// flipCmp swaps operand order in a comparison — `x OP y` becomes
// `y flip(OP) x`. Only comparison ops are flipped meaningfully;
// arithmetic/logical fall through unchanged.
func flipCmp(op binOpKind) binOpKind {
	switch op {
	case bopLt:
		return bopGt
	case bopLe:
		return bopGe
	case bopGt:
		return bopLt
	case bopGe:
		return bopLe
	}
	return op // Eq, Ne, arithmetic, logical: unchanged
}

// cmpVal returns (-1/0/+1, true) for a<b, a==b, a>b when a and b are
// the same supported scalar type (numeric, string, or bool). Cross-
// type comparisons return (0, false) so the caller can fall back to
// the conservative "possibly matches" answer rather than mistakenly
// pruning.
func cmpVal(a, b any) (int, bool) {
	// Numeric fast path via float64 coercion.
	if af, ok := toFloat64(a); ok {
		if bf, ok := toFloat64(b); ok {
			switch {
			case af < bf:
				return -1, true
			case af > bf:
				return +1, true
			}
			return 0, true
		}
	}
	// Strings.
	if as, ok := a.(string); ok {
		if bs, ok := b.(string); ok {
			switch {
			case as < bs:
				return -1, true
			case as > bs:
				return +1, true
			}
			return 0, true
		}
	}
	// Booleans: false < true.
	if ab, ok := a.(bool); ok {
		if bb, ok := b.(bool); ok {
			switch {
			case !ab && bb:
				return -1, true
			case ab && !bb:
				return +1, true
			}
			return 0, true
		}
	}
	return 0, false
}

// toFloat64 attempts to widen a Go numeric type to float64. Used by
// cmpVal for cross-integer / float comparisons.
func toFloat64(v any) (float64, bool) {
	switch x := v.(type) {
	case int64:
		return float64(x), true
	case int32:
		return float64(x), true
	case int:
		return float64(x), true
	case uint64:
		return float64(x), true
	case uint32:
		return float64(x), true
	case float64:
		return x, true
	case float32:
		return float64(x), true
	}
	return 0, false
}
