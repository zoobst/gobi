package gobi

import (
	"fmt"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/zoobst/gobi/geometry"
)

// LitGeom returns an expression that broadcasts a constant geometry
// value to every input row. Companion to Lit for the spatial-predicate
// builders (Expr.GeomIntersects and friends): the executor detects a
// literalGeomNode on the right side of a predicate and takes the
// constant-right fast path, which is the same code path Series-level
// GeomIntersects(g) already uses.
//
// The plain `Lit(v)` constructor also accepts any value implementing
// geometry.Geometry and routes it here — so `Lit(aoi)` and
// `LitGeom(aoi)` are interchangeable in a Filter expression. Callers
// building predicates over a geometry column typically write the
// former for symmetry with `Lit(1.0)` etc.
//
// The CRS of the constant is inherited from the left-hand geometry
// column at evaluation time, matching Series.GeomIntersects's behavior
// — the constant is re-tagged with the column's CRS so predicates
// don't cross-reference CRSes silently.
func LitGeom(g geometry.Geometry) Expr {
	return Expr{node: &literalGeomNode{value: g}}
}

// literalGeomNode is the ExprNode form of LitGeom. Kept separate from
// literalNode (which handles bool / numeric / string scalars) so the
// spatial-predicate executor can pattern-match on it directly to reach
// the constant-right fast path without a runtime type assertion on
// literalNode.value.
type literalGeomNode struct {
	value geometry.Geometry
}

// Eval broadcasts the geometry as a WKB blob to every input row.
// Eval errors when called from a non-predicate position. LitGeom is
// a predicate-only marker — the spatial-predicate executor pattern-
// matches on literalGeomNode BEFORE calling Eval and takes the
// constant-right fast path from there.
//
// Reaching Eval directly means someone put LitGeom into a Select /
// WithColumn / other row-emitting position, which would require
// materializing N copies of the WKB blob as an Arrow Binary column.
// Arrow Binary's offsets-must-be-monotonic constraint means there's
// no cheap "point N rows at one buffer" form; the naive
// implementation allocates O(N × wkb-size) memory. On a 1M-row
// frame with a 10k-vertex AOI polygon that's ~200MB of duplicated
// bytes for a value the caller almost certainly meant as a constant.
//
// Callers who genuinely need a broadcast geometry column (rare —
// usually surfaces as debug output or a join-key shim) should build
// it explicitly outside the Expr layer: a single BinaryBuilder loop
// + NewFrame keeps the intent visible and the allocation choice in
// the caller's hands.
func (n *literalGeomNode) Eval(_ *Frame) (Series, error) {
	return Series{}, fmt.Errorf("gobi: LitGeom is a predicate-only marker; " +
		"use it as an operand of GeomIntersects / GeomContains / ... " +
		"or build a broadcast column explicitly outside the Expr layer")
}

func (n *literalGeomNode) Type(*arrow.Schema) (arrow.DataType, error) {
	return arrow.BinaryTypes.Binary, nil
}

func (n *literalGeomNode) Children() []Expr { return nil }
func (n *literalGeomNode) String() string {
	if n.value == nil {
		return "lit_geom(<nil>)"
	}
	return fmt.Sprintf("lit_geom(%s)", geometry.TypeString(n.value))
}

// -----------------------------------------------------------------------------
// Fluent builders — spatial predicates on Expr
// -----------------------------------------------------------------------------

// GeomIntersects returns e ∩ other ≠ ∅ as a Boolean expression.
// Compose it into LazyFrame.Filter alongside scalar predicates:
//
//	lf.Filter(
//	    gobi.Col("level").Eq(gobi.Lit(1.0)).
//	        And(gobi.Col("geometry").GeomIntersects(gobi.Lit(aoi))),
//	).Collect()
//
// e must reference a geometry column. `other` may be a LitGeom
// constant (fast path — bbox short-circuit per row, matches
// Series.GeomIntersects semantics) or another geometry-column
// expression (pair-wise per-row test — new capability not exposed on
// Series today).
//
// Null propagates from either side: if the left row is null, or the
// right row is null (column-right case), the output row is null.
// Matches Polars' null-propagating comparison contract.
func (e Expr) GeomIntersects(other Expr) Expr {
	return Expr{node: &geomPredicateNode{
		pred:  geometry.PredIntersects,
		left:  e.node,
		right: other.node,
	}}
}

// GeomContains returns e ⊇ other (every point of other lies in e) as
// a Boolean expression. Same composition rules as GeomIntersects.
// Matches GeoPandas' GeoSeries.contains and Series.GeomContains.
func (e Expr) GeomContains(other Expr) Expr {
	return Expr{node: &geomPredicateNode{
		pred:  geometry.PredContains,
		left:  e.node,
		right: other.node,
	}}
}

// GeomWithin returns e ⊆ other (every point of e lies in other) as a
// Boolean expression. Equivalent to `other.GeomContains(e)`; kept as
// its own method for symmetry with GeoPandas' .within(). Same
// composition rules as GeomIntersects.
func (e Expr) GeomWithin(other Expr) Expr {
	return Expr{node: &geomPredicateNode{
		pred:  geometry.PredWithin,
		left:  e.node,
		right: other.node,
	}}
}

// GeomDisjoint returns e ∩ other == ∅ as a Boolean expression —
// exactly the negation of GeomIntersects (nulls still propagate; a
// null row is null, NOT true). Same composition rules as
// GeomIntersects.
func (e Expr) GeomDisjoint(other Expr) Expr {
	return Expr{node: &geomPredicateNode{
		pred:  geometry.PredDisjoint,
		left:  e.node,
		right: other.node,
	}}
}

// GeomTouches returns "e and other share boundary points but no
// interior points" as a Boolean expression. Matches GeoPandas'
// GeoSeries.touches. Same composition rules as GeomIntersects.
func (e Expr) GeomTouches(other Expr) Expr {
	return Expr{node: &geomPredicateNode{
		pred:  geometry.PredTouches,
		left:  e.node,
		right: other.node,
	}}
}

// GeomCrosses returns "e crosses other" (share some but not all
// interior points, with the intersection lower-dimensional than the
// higher operand) as a Boolean expression. Typical shape:
// LineString × Polygon. Matches GeoPandas' GeoSeries.crosses. Same
// composition rules as GeomIntersects.
func (e Expr) GeomCrosses(other Expr) Expr {
	return Expr{node: &geomPredicateNode{
		pred:  geometry.PredCrosses,
		left:  e.node,
		right: other.node,
	}}
}

// GeomOverlaps returns "e and other share interior points, neither
// contains the other, and both are of the same dimension" as a
// Boolean expression. Matches GeoPandas' GeoSeries.overlaps. Same
// composition rules as GeomIntersects.
func (e Expr) GeomOverlaps(other Expr) Expr {
	return Expr{node: &geomPredicateNode{
		pred:  geometry.PredOverlaps,
		left:  e.node,
		right: other.node,
	}}
}

// -----------------------------------------------------------------------------
// Executor node
// -----------------------------------------------------------------------------

// geomPredicateNode evaluates a binary spatial predicate over two
// operands, one of which must be a geometry column. The constant-right
// fast path detects a literalGeomNode on the right and reuses the
// Series-level geomPredicateOp driver so the bbox short-circuit
// semantics carry over unchanged.
//
// pred is the single source of truth for which predicate applies —
// including PredDisjoint, which was previously represented as
// (PredIntersects, invert=true). No side-channel flags.
type geomPredicateNode struct {
	pred        geometry.Predicate
	left, right ExprNode
}

func (n *geomPredicateNode) Eval(input *Frame) (Series, error) {
	// Constant-right fast path. Reuses the Series driver, which
	// already carries bbox-reject short-circuit + null propagation.
	if rlit, ok := n.right.(*literalGeomNode); ok {
		if rlit.value == nil {
			return allNullBool(input.NumRows(), n.outputName(""))
		}
		left, err := n.left.Eval(input)
		if err != nil {
			return Series{}, err
		}
		return n.evalConstantRight(left, rlit.value)
	}
	// Column-right: pair-wise per-row test.
	left, err := n.left.Eval(input)
	if err != nil {
		return Series{}, err
	}
	right, err := n.right.Eval(input)
	if err != nil {
		return Series{}, err
	}
	return n.evalColumnRight(left, right)
}

func (n *geomPredicateNode) evalConstantRight(left Series, right geometry.Geometry) (Series, error) {
	// Route to the Series driver. It handles CRS attachment, null
	// propagation, and the per-row loop. PredDisjoint dispatches
	// through geometry.Test as !Intersects — no post-invert needed.
	out, err := geomPredicateOp(left, right, n.pred, "", false)
	if err != nil {
		return Series{}, err
	}
	return renameSeries(out, n.outputName(left.name)), nil
}

func (n *geomPredicateNode) evalColumnRight(left, right Series) (Series, error) {
	if !left.IsGeometry() {
		return Series{}, fmt.Errorf("%w: %s left operand", ErrNotGeometry, n.pred)
	}
	if !right.IsGeometry() {
		return Series{}, fmt.Errorf("%w: %s right operand", ErrNotGeometry, n.pred)
	}
	if left.Len() != right.Len() {
		return Series{}, fmt.Errorf("gobi: %s: length mismatch (%d vs %d)",
			n.pred, left.Len(), right.Len())
	}
	leftBin, err := flatBinary(left)
	if err != nil {
		return Series{}, err
	}
	defer leftBin.Release()
	rightBin, err := flatBinary(right)
	if err != nil {
		return Series{}, err
	}
	defer rightBin.Release()

	lcrs, _ := geometry.LookupCRS(geometryCRSFromField(left.field))
	rcrs, _ := geometry.LookupCRS(geometryCRSFromField(right.field))

	pool := memory.DefaultAllocator
	b := array.NewBooleanBuilder(pool)
	defer b.Release()
	for i := range leftBin.Len() {
		if leftBin.IsNull(i) || rightBin.IsNull(i) {
			b.AppendNull()
			continue
		}
		lg, err := geometry.ParseWKB(leftBin.Value(i))
		if err != nil {
			return Series{}, err
		}
		rg, err := geometry.ParseWKB(rightBin.Value(i))
		if err != nil {
			return Series{}, err
		}
		lg = attachCRS(lg, lcrs)
		rg = attachCRS(rg, rcrs)
		b.Append(geometry.Test(n.pred, lg, rg))
	}
	field := arrow.Field{Name: n.outputName(left.name), Type: arrow.FixedWidthTypes.Boolean, Nullable: true}
	return SeriesFromArray(field, b.NewArray()), nil
}

func (n *geomPredicateNode) Type(_ *arrow.Schema) (arrow.DataType, error) {
	return arrow.FixedWidthTypes.Boolean, nil
}

func (n *geomPredicateNode) Children() []Expr {
	return []Expr{{node: n.left}, {node: n.right}}
}

func (n *geomPredicateNode) String() string {
	return fmt.Sprintf("%s(%s, %s)", n.pred, n.left, n.right)
}

func (n *geomPredicateNode) outputName(leftName string) string {
	suffix := "_" + n.pred.String()
	if leftName == "" {
		return suffix[1:]
	}
	return leftName + suffix
}

// flatBinary returns s's data as a single *array.Binary. Single-chunk
// inputs are returned as-is (with a Retain so the caller's Release
// balances); multi-chunk inputs are concatenated. Callers must Release
// the returned array.
func flatBinary(s Series) (*array.Binary, error) {
	chunks := s.col.Data().Chunks()
	switch len(chunks) {
	case 0:
		return nil, fmt.Errorf("gobi: %s: empty geometry column", s.name)
	case 1:
		bin, ok := chunks[0].(*array.Binary)
		if !ok {
			return nil, fmt.Errorf("%w: geometry column not Binary (%T)",
				ErrColumnTypeMismatch, chunks[0])
		}
		bin.Retain()
		return bin, nil
	}
	pool := memory.DefaultAllocator
	arrs := make([]arrow.Array, len(chunks))
	for i, c := range chunks {
		if _, ok := c.(*array.Binary); !ok {
			return nil, fmt.Errorf("%w: geometry column not Binary (%T)",
				ErrColumnTypeMismatch, c)
		}
		arrs[i] = c
	}
	joined, err := array.Concatenate(arrs, pool)
	if err != nil {
		return nil, err
	}
	bin, ok := joined.(*array.Binary)
	if !ok {
		joined.Release()
		return nil, fmt.Errorf("gobi: Concatenate returned %T, not *array.Binary", joined)
	}
	return bin, nil
}

// allNullBool returns a Boolean Series of n rows, every value null.
// Used when a constant-right predicate operand is nil — matches
// Polars' "any null → null" semantic.
func allNullBool(n int, name string) (Series, error) {
	pool := memory.DefaultAllocator
	b := array.NewBooleanBuilder(pool)
	defer b.Release()
	for range n {
		b.AppendNull()
	}
	field := arrow.Field{Name: name, Type: arrow.FixedWidthTypes.Boolean, Nullable: true}
	return SeriesFromArray(field, b.NewArray()), nil
}
