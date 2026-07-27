package gobi

import (
	"fmt"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/zoobst/gobi/geometry"
)

// HaversineExpr returns an expression that computes the great-circle
// distance between two lon/lat point columns, in the requested unit.
// Composes with Shift(1).Over(K) for prev-row coordinate lookups, so
// per-segment ground-track distance is expressible entirely in
// LazyFrame land:
//
//	speedKMH := HaversineExpr(
//	    Col("lat"), Col("lon"),
//	    Col("lat").Shift(1).Over("eid"),
//	    Col("lon").Shift(1).Over("eid"),
//	    geometry.UnitKilometers,
//	).Div(Col("delta_hours"))
//
// Argument order is (lat1, lon1, lat2, lon2) — the pair-per-side
// grouping matches how coordinates are usually named in a source
// dataset. Internally the call routes to geometry.Haversine (which
// takes lon first), so a mistake in ordering here surfaces as
// silently-wrong distances, not an error. Types are checked at
// Compile time (all four operands must be Float64).
//
// Nulls in any of the four inputs propagate to a null output. Errors
// at Eval time if a column isn't Float64.
func HaversineExpr(lat1, lon1, lat2, lon2 Expr, u geometry.Unit) Expr {
	return Expr{node: &haversineNode{
		lat1: lat1.node, lon1: lon1.node,
		lat2: lat2.node, lon2: lon2.node,
		unit: u,
	}}
}

type haversineNode struct {
	lat1, lon1, lat2, lon2 ExprNode
	unit                   geometry.Unit
}

func (n *haversineNode) Eval(input *Frame) (Series, error) {
	lat1, err := n.evalFloat64(input, n.lat1, "lat1")
	if err != nil {
		return Series{}, err
	}
	lon1, err := n.evalFloat64(input, n.lon1, "lon1")
	if err != nil {
		return Series{}, err
	}
	lat2, err := n.evalFloat64(input, n.lat2, "lat2")
	if err != nil {
		return Series{}, err
	}
	lon2, err := n.evalFloat64(input, n.lon2, "lon2")
	if err != nil {
		return Series{}, err
	}
	nRows := input.NumRows()
	if lat1.Len() != nRows || lon1.Len() != nRows ||
		lat2.Len() != nRows || lon2.Len() != nRows {
		return Series{}, fmt.Errorf(
			"%w: HaversineExpr operand length mismatch (lat1=%d lon1=%d lat2=%d lon2=%d, expected %d)",
			ErrColumnLenMismatch, lat1.Len(), lon1.Len(), lat2.Len(), lon2.Len(), nRows)
	}
	// Prefer zero-copy views; fall back to Nulls masks for null
	// gating. All-valid inputs skip null checks entirely.
	lat1Vals, lat1Ok := lat1.Float64Values()
	lon1Vals, lon1Ok := lon1.Float64Values()
	lat2Vals, lat2Ok := lat2.Float64Values()
	lon2Vals, lon2Ok := lon2.Float64Values()
	if !(lat1Ok && lon1Ok && lat2Ok && lon2Ok) {
		return n.evalMultiChunk(lat1, lon1, lat2, lon2)
	}

	pool := memory.DefaultAllocator
	b := array.NewFloat64Builder(pool)
	defer b.Release()

	anyNull := lat1.HasNulls() || lon1.HasNulls() || lat2.HasNulls() || lon2.HasNulls()
	if !anyNull {
		for i := range nRows {
			d, err := geometry.Haversine(lon1Vals[i], lat1Vals[i], lon2Vals[i], lat2Vals[i], n.unit)
			if err != nil {
				return Series{}, fmt.Errorf("HaversineExpr row %d: %w", i, err)
			}
			b.Append(d)
		}
		return arrayToSeries(pool, "haversine", arrow.PrimitiveTypes.Float64, b.NewArray())
	}
	lat1N := lat1.Nulls()
	lon1N := lon1.Nulls()
	lat2N := lat2.Nulls()
	lon2N := lon2.Nulls()
	for i := range nRows {
		if isNullAtMask(lat1N, i) || isNullAtMask(lon1N, i) ||
			isNullAtMask(lat2N, i) || isNullAtMask(lon2N, i) {
			b.AppendNull()
			continue
		}
		d, err := geometry.Haversine(lon1Vals[i], lat1Vals[i], lon2Vals[i], lat2Vals[i], n.unit)
		if err != nil {
			return Series{}, fmt.Errorf("HaversineExpr row %d: %w", i, err)
		}
		b.Append(d)
	}
	return arrayToSeries(pool, "haversine", arrow.PrimitiveTypes.Float64, b.NewArray())
}

// evalMultiChunk is the fallback for the rare case where an operand
// Series isn't Float64 single-chunk (e.g., a downstream op left the
// column with multiple chunks). Uses the copy extractor and the
// vectorized Nulls mask — same output as the fast path, one extra
// allocation per operand.
func (n *haversineNode) evalMultiChunk(lat1, lon1, lat2, lon2 Series) (Series, error) {
	lat1V, err := lat1.Float64s()
	if err != nil {
		return Series{}, fmt.Errorf("HaversineExpr lat1: %w", err)
	}
	lon1V, err := lon1.Float64s()
	if err != nil {
		return Series{}, fmt.Errorf("HaversineExpr lon1: %w", err)
	}
	lat2V, err := lat2.Float64s()
	if err != nil {
		return Series{}, fmt.Errorf("HaversineExpr lat2: %w", err)
	}
	lon2V, err := lon2.Float64s()
	if err != nil {
		return Series{}, fmt.Errorf("HaversineExpr lon2: %w", err)
	}
	lat1N := lat1.Nulls()
	lon1N := lon1.Nulls()
	lat2N := lat2.Nulls()
	lon2N := lon2.Nulls()
	pool := memory.DefaultAllocator
	b := array.NewFloat64Builder(pool)
	defer b.Release()
	for i := range lat1V {
		if isNullAtMask(lat1N, i) || isNullAtMask(lon1N, i) ||
			isNullAtMask(lat2N, i) || isNullAtMask(lon2N, i) {
			b.AppendNull()
			continue
		}
		d, err := geometry.Haversine(lon1V[i], lat1V[i], lon2V[i], lat2V[i], n.unit)
		if err != nil {
			return Series{}, fmt.Errorf("HaversineExpr row %d: %w", i, err)
		}
		b.Append(d)
	}
	return arrayToSeries(pool, "haversine", arrow.PrimitiveTypes.Float64, b.NewArray())
}

func (n *haversineNode) evalFloat64(input *Frame, node ExprNode, label string) (Series, error) {
	if node == nil {
		return Series{}, fmt.Errorf("gobi: HaversineExpr %s is nil", label)
	}
	s, err := node.Eval(input)
	if err != nil {
		return Series{}, fmt.Errorf("HaversineExpr %s: %w", label, err)
	}
	if s.DataType().ID() != arrow.FLOAT64 {
		return Series{}, fmt.Errorf(
			"%w: HaversineExpr %s must be Float64, got %s",
			ErrExprTypeMismatch, label, s.DataType())
	}
	return s, nil
}

func (n *haversineNode) Type(schema *arrow.Schema) (arrow.DataType, error) {
	for _, sub := range []struct {
		node  ExprNode
		label string
	}{
		{n.lat1, "lat1"}, {n.lon1, "lon1"},
		{n.lat2, "lat2"}, {n.lon2, "lon2"},
	} {
		if sub.node == nil {
			return nil, fmt.Errorf("gobi: HaversineExpr %s is nil", sub.label)
		}
		dt, err := sub.node.Type(schema)
		if err != nil {
			return nil, err
		}
		if dt.ID() != arrow.FLOAT64 {
			return nil, fmt.Errorf(
				"%w: HaversineExpr %s must be Float64, got %s",
				ErrExprTypeMismatch, sub.label, dt)
		}
	}
	return arrow.PrimitiveTypes.Float64, nil
}

func (n *haversineNode) Children() []Expr {
	return []Expr{
		{node: n.lat1}, {node: n.lon1},
		{node: n.lat2}, {node: n.lon2},
	}
}

func (n *haversineNode) String() string {
	return fmt.Sprintf("haversine(%s, %s, %s, %s, %s)",
		n.lat1, n.lon1, n.lat2, n.lon2, n.unit)
}

// isNullAtMask is the local counterpart to gobi's Nulls-mask
// gate. Mirrors series-consumer code shape: nil mask (no nulls in
// series) treats every index as valid.
func isNullAtMask(mask []bool, i int) bool {
	return i < len(mask) && mask[i]
}
