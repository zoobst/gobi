package gobi

import (
	"math"

	"github.com/zoobst/gobi/geometry"
)

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
	case *geomPredicateNode:
		return canMatchGeomPredicate(n, s)
	case *geomDWithinNode:
		return canMatchGeomDWithin(n, s)
	}
	// notNode, custom nodes, arithmetic — bail conservatively.
	return true
}

// canMatchGeomPredicate handles a spatial predicate against a row
// group's covering-column stats. Only the constant-right case
// (literalGeomNode on right) is prunable; column-right expressions
// don't have a static bbox to compare against and pass through as
// "possibly matches."
//
// Supported predicates:
//
//   - Intersects / Contains / Overlaps / Touches / Crosses: skip
//     when the constant's bbox is fully disjoint from the row
//     group's covering bbox range.
//   - Within: same bbox-overlap necessary condition (a row's shape
//     must at least reach the constant's bbox to be inside it).
//   - Disjoint: same "row group inside constant's bbox" containment
//     rule as the naive prune — BUT only when the constant is a
//     proven axis-aligned rectangle (bbox area == shape area). For
//     concave / holed literals a row sitting in the bbox-hole is
//     genuinely disjoint from the shape, and pruning would silently
//     drop it. See litIsBboxRectangle.
//
// Any statistic missing → conservative "possibly matches."
func canMatchGeomPredicate(n *geomPredicateNode, s Stats) bool {
	lg, litGeom := extractGeomLit(n)
	if !litGeom {
		return true // column-right or nested — can't prune
	}
	geomCol, ok := extractGeomColRef(n)
	if !ok {
		return true // both sides literals, or nested structure
	}
	rgBounds, ok := coveringBounds(s, geomCol.name)
	if !ok {
		return true // no covering columns → file didn't opt into pushdown
	}
	litBounds := lg.value.Bounds()
	if litBounds.Empty() {
		// Fires for two shapes: (a) the nil-literal case, and (b) a
		// non-nil literal whose geometry has empty bounds (an empty
		// Polygon, an empty GeometryCollection). Both are degenerate.
		// Keep the row group either way — case (a) hits the
		// executor's constant-null short-circuit (all-null output),
		// case (b) reaches the per-row loop which will report the
		// predicate as false for every row. Silent-pruning would be
		// wrong for (b): it'd claim "no row possibly matches" when
		// the executor has a well-defined false answer to give.
		return true
	}

	switch n.pred {
	case geometry.PredDisjoint:
		// Disjoint: pruning here is unsound in general. The tempting
		// rule "row group is fully inside lit's bbox → no row is
		// disjoint from lit" only holds when lit's bbox equals lit's
		// actual shape (a filled rectangle). For a concave polygon
		// (an L-shape, an annulus, a country boundary), a row whose
		// geometry sits inside lit's bbox but outside lit's polygon
		// IS disjoint — and pruning it would silently drop a real
		// match. Silent-wrong is the failure mode gobi trades OOM
		// for; we don't cross that line for a niche predicate.
		//
		// Only prune when lit is a proven rectangle (its polygon area
		// equals its bbox area within a small tolerance). This still
		// catches the common AOI-rectangle case that users of
		// GeomDisjoint most often build.
		if !litIsBboxRectangle(lg.value) {
			return true
		}
		if litBounds.MinX <= rgBounds.MinX && litBounds.MinY <= rgBounds.MinY &&
			litBounds.MaxX >= rgBounds.MaxX && litBounds.MaxY >= rgBounds.MaxY {
			return false
		}
		return true
	case geometry.PredWithin:
		// Within: row's bbox must be inside the constant's bbox.
		// Prune only when the row group's minimum extent is already
		// bigger than the constant — i.e. the smallest x-span in the
		// group exceeds the constant's, which we can't tell from
		// min/max stats alone. Conservative: allow bbox-overlap
		// filter (necessary but not sufficient).
		return bboxesOverlap(litBounds, rgBounds)
	default:
		// Intersects / Contains / Overlaps / Touches / Crosses all
		// require bbox intersection as a necessary condition.
		return bboxesOverlap(litBounds, rgBounds)
	}
}

// canMatchGeomDWithin handles a DWithin predicate against a row
// group's covering-column stats. Same "necessary condition"
// framing as canMatchGeomPredicate: prune only when the row group's
// bbox is provably too far from the constant's bbox to have any
// pair of points within `distance`.
//
// Only the constant-right case prunes (column-right has no static
// distance geometry to compare against). Negative / NaN distance
// yields all-false at runtime — safe to keep the row group and let
// the executor produce the false answer.
func canMatchGeomDWithin(n *geomDWithinNode, s Stats) bool {
	lg, ok := unwrapAlias(n.right).(*literalGeomNode)
	if !ok {
		// Try the other side (unusual but symmetric).
		lg, ok = unwrapAlias(n.left).(*literalGeomNode)
		if !ok {
			return true
		}
	}
	geomCol, ok := extractDWithinColRef(n)
	if !ok {
		return true
	}
	rgBounds, ok := coveringBounds(s, geomCol.name)
	if !ok {
		return true
	}
	if lg.value == nil {
		return true
	}
	litBounds := lg.value.Bounds()
	if litBounds.Empty() {
		return true
	}
	if n.distance < 0 || math.IsNaN(n.distance) {
		return true // executor produces all-false; don't prune
	}
	// Expand the constant's bbox by `distance` in all directions.
	// Any row group whose bbox doesn't overlap the expanded region
	// is provably farther than `distance` from lit — safe to prune.
	expanded := geometry.Bounds{
		MinX: litBounds.MinX - n.distance,
		MinY: litBounds.MinY - n.distance,
		MaxX: litBounds.MaxX + n.distance,
		MaxY: litBounds.MaxY + n.distance,
	}
	return bboxesOverlap(expanded, rgBounds)
}

// extractDWithinColRef mirrors extractGeomColRef for the DWithin
// node — either operand may be the column reference, though
// left-column-right-literal is the common shape.
func extractDWithinColRef(n *geomDWithinNode) (*colRefNode, bool) {
	if c, ok := unwrapAlias(n.left).(*colRefNode); ok {
		return c, true
	}
	if c, ok := unwrapAlias(n.right).(*colRefNode); ok {
		return c, true
	}
	return nil, false
}

// litIsBboxRectangle reports whether g's planar polygon area is
// indistinguishable from its bbox area — i.e. g is a filled
// axis-aligned rectangle with no holes. Used to gate Disjoint
// pruning to the shapes where litBounds ⊇ rgBounds is actually
// sufficient to prove "no row is disjoint."
//
// Uses a planar shoelace (not Polygon.Area, which special-cases
// geographic CRS as a spherical integral — that would mismatch the
// bbox area's planar-degrees² scale and produce false negatives
// on WGS84 AOI rectangles). Relative tolerance of 1e-9 against
// the bbox area so float64 vertex rounding doesn't downgrade a
// legitimate rectangle to non-rectangular. Non-polygon geometries
// return false (nothing to prune conservatively).
func litIsBboxRectangle(g geometry.Geometry) bool {
	poly, ok := g.(geometry.Polygon)
	if !ok {
		return false
	}
	// Representation-strict: a Polygon whose exterior happens to be a
	// rectangle but that carries any holes fails this check even if
	// the holes are trivially empty. Same for a polygon that stores
	// its exterior in Rings[1] with an empty Rings[0]. That's fine —
	// the rectangle test is a fast-path guard for the AOI-rectangle
	// case; non-canonical shapes fall through to the conservative
	// "keep the row group" branch.
	if len(poly.Rings) != 1 {
		return false
	}
	b := poly.Bounds()
	if b.Empty() {
		return false
	}
	bboxArea := (b.MaxX - b.MinX) * (b.MaxY - b.MinY)
	if bboxArea <= 0 {
		return false
	}
	polyArea := geometry.PlanarRingArea(poly.Rings[0])
	return math.Abs(polyArea-bboxArea)/bboxArea < 1e-9
}

// extractGeomLit returns the literalGeomNode operand and true if
// exactly one of n's operands is a geometry literal. Also handles
// aliased forms.
func extractGeomLit(n *geomPredicateNode) (*literalGeomNode, bool) {
	if lg, ok := unwrapAlias(n.right).(*literalGeomNode); ok {
		return lg, true
	}
	if lg, ok := unwrapAlias(n.left).(*literalGeomNode); ok {
		return lg, true
	}
	return nil, false
}

// extractGeomColRef returns the column-ref operand and true if
// exactly one operand is a bare column reference. The other operand
// must be the literal (checked separately).
func extractGeomColRef(n *geomPredicateNode) (*colRefNode, bool) {
	if c, ok := unwrapAlias(n.left).(*colRefNode); ok {
		return c, true
	}
	if c, ok := unwrapAlias(n.right).(*colRefNode); ok {
		return c, true
	}
	return nil, false
}

func unwrapAlias(n ExprNode) ExprNode {
	for {
		if a, ok := n.(*aliasNode); ok {
			n = a.inner
			continue
		}
		return n
	}
}

// coveringBounds reads the covering-column min/max stats for a
// geometry column named geomName. Returns ok=false if any of the
// four covering columns lacks stats — the caller falls back to
// "possibly matches."
func coveringBounds(s Stats, geomName string) (geometry.Bounds, bool) {
	xminCol, yminCol, xmaxCol, ymaxCol := BboxColumnNames(geomName)
	// Row-group xmin/ymin comes from the covering column's MIN;
	// xmax/ymax from the covering column's MAX. The other stat
	// (min of xmax, max of xmin) is irrelevant for bbox overlap.
	xminV, _, ok1 := s.MinMax(xminCol)
	yminV, _, ok2 := s.MinMax(yminCol)
	_, xmaxV, ok3 := s.MinMax(xmaxCol)
	_, ymaxV, ok4 := s.MinMax(ymaxCol)
	if !ok1 || !ok2 || !ok3 || !ok4 {
		return geometry.Bounds{}, false
	}
	xmin, ok1 := toFloat64(xminV)
	ymin, ok2 := toFloat64(yminV)
	xmax, ok3 := toFloat64(xmaxV)
	ymax, ok4 := toFloat64(ymaxV)
	if !ok1 || !ok2 || !ok3 || !ok4 {
		return geometry.Bounds{}, false
	}
	return geometry.Bounds{MinX: xmin, MinY: ymin, MaxX: xmax, MaxY: ymax}, true
}

// bboxesOverlap reports whether two axis-aligned bboxes share any
// point. Closed intervals — touching edges count.
func bboxesOverlap(a, b geometry.Bounds) bool {
	if a.MinX > b.MaxX || b.MinX > a.MaxX {
		return false
	}
	if a.MinY > b.MaxY || b.MinY > a.MaxY {
		return false
	}
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
