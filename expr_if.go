package gobi

import (
	"fmt"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// If returns an expression that evaluates to a where cond is true, to
// b where cond is false, and to null where cond is null (SQL CASE-WHEN
// semantics). All three arguments are evaluated in full — this is not
// short-circuit. If short-circuit evaluation matters (e.g. one branch
// would error on rows the other branch handles), split into two
// filtered pipelines.
//
// cond must produce a Boolean column. a and b must have the same
// arrow output type — no automatic numeric widening in v1. To combine
// mixed numeric types write `gobi.If(cond, gobi.Lit(1.0), gobi.Col("x"))`
// where "x" is Float64, or add a Cast on one side.
//
// Example — mean-fill a nullable column:
//
//	gobi.If(gobi.Col("x").IsNull(), gobi.Col("x_mean"), gobi.Col("x"))
//
// Nested else-if via chained If is supported:
//
//	gobi.If(
//	    gobi.Col("region").Eq(gobi.Lit("EU")), gobi.Lit(1.08),
//	    gobi.If(
//	        gobi.Col("region").Eq(gobi.Lit("NA")), gobi.Lit(1.10),
//	        gobi.Lit(1.0), // default
//	    ),
//	)
func If(cond, a, b Expr) Expr {
	return Expr{node: &ifNode{cond: cond.node, then: a.node, otherwise: b.node}}
}

// ifNode implements SQL-style CASE WHEN. Three sub-nodes evaluated
// against the same input Frame; per-row selection into a builder of
// the common output type.
type ifNode struct {
	cond, then, otherwise ExprNode
}

func (n *ifNode) Eval(input *Frame) (Series, error) {
	if n.cond == nil || n.then == nil || n.otherwise == nil {
		return Series{}, fmt.Errorf("gobi: If with nil sub-expression")
	}
	nRows := input.NumRows()

	cs, err := n.cond.Eval(input)
	if err != nil {
		return Series{}, fmt.Errorf("gobi: If.cond: %w", err)
	}
	if cs.DataType().ID() != arrow.BOOL {
		return Series{}, fmt.Errorf("%w: If.cond must be Boolean, got %s",
			ErrExprTypeMismatch, cs.DataType())
	}
	if cs.Len() != nRows {
		return Series{}, fmt.Errorf("gobi: If.cond length %d != input rows %d", cs.Len(), nRows)
	}

	ts, err := n.then.Eval(input)
	if err != nil {
		return Series{}, fmt.Errorf("gobi: If.then: %w", err)
	}
	if ts.Len() != nRows {
		return Series{}, fmt.Errorf("gobi: If.then length %d != input rows %d", ts.Len(), nRows)
	}

	os, err := n.otherwise.Eval(input)
	if err != nil {
		return Series{}, fmt.Errorf("gobi: If.otherwise: %w", err)
	}
	if os.Len() != nRows {
		return Series{}, fmt.Errorf("gobi: If.otherwise length %d != input rows %d", os.Len(), nRows)
	}

	if !arrow.TypeEqual(ts.DataType(), os.DataType()) {
		return Series{}, fmt.Errorf(
			"%w: If.then / If.otherwise type mismatch (%s vs %s) — cast one side or match Lit types",
			ErrExprTypeMismatch, ts.DataType(), os.DataType())
	}

	outType := ts.DataType()
	pool := memory.DefaultAllocator
	b, err := builderForType(pool, outType)
	if err != nil {
		return Series{}, fmt.Errorf("If: %w", err)
	}
	defer b.Release()

	for row := range nRows {
		condNull, err := isNullAtSeries(cs, row)
		if err != nil {
			return Series{}, fmt.Errorf("gobi: If.cond row %d: %w", row, err)
		}
		if condNull {
			// SQL CASE semantics: null cond → null output.
			b.AppendNull()
			continue
		}
		condVal, err := boolAtSeries(cs, row)
		if err != nil {
			return Series{}, fmt.Errorf("gobi: If.cond row %d: %w", row, err)
		}
		src := os
		if condVal {
			src = ts
		}
		null, err := isNullAtSeries(src, row)
		if err != nil {
			return Series{}, err
		}
		if null {
			b.AppendNull()
			continue
		}
		v, err := readScalarAt(src, row)
		if err != nil {
			return Series{}, fmt.Errorf("If row %d: %w", row, err)
		}
		if err := appendCustomValue(b, v); err != nil {
			return Series{}, fmt.Errorf("If row %d emit: %w", row, err)
		}
	}
	return buildSeries("if", outType, b.NewArray()), nil
}

func (n *ifNode) Type(schema *arrow.Schema) (arrow.DataType, error) {
	if n.then == nil {
		return nil, fmt.Errorf("gobi: If with nil then branch")
	}
	// Type inference validates all three subtrees; if either side is
	// mistyped (unknown column, bad literal), surface early.
	condType, err := n.cond.Type(schema)
	if err != nil {
		return nil, err
	}
	if condType != nil && condType.ID() != arrow.BOOL {
		return nil, fmt.Errorf("%w: If.cond must be Boolean, got %s",
			ErrExprTypeMismatch, condType)
	}
	tt, err := n.then.Type(schema)
	if err != nil {
		return nil, err
	}
	ot, err := n.otherwise.Type(schema)
	if err != nil {
		return nil, err
	}
	if tt != nil && ot != nil && !arrow.TypeEqual(tt, ot) {
		return nil, fmt.Errorf(
			"%w: If.then / If.otherwise type mismatch (%s vs %s)",
			ErrExprTypeMismatch, tt, ot)
	}
	return tt, nil
}

func (n *ifNode) Children() []Expr {
	return []Expr{
		{node: n.cond}, {node: n.then}, {node: n.otherwise},
	}
}

func (n *ifNode) String() string {
	return fmt.Sprintf("if(%s, %s, %s)", n.cond, n.then, n.otherwise)
}

// boolAtSeries reads the Boolean value at row from a single-chunk or
// multi-chunk Boolean Series. Callers should isNullAtSeries first.
func boolAtSeries(s Series, row int) (bool, error) {
	offset := 0
	for _, chunk := range s.col.Data().Chunks() {
		if row < offset+chunk.Len() {
			ba, ok := chunk.(*array.Boolean)
			if !ok {
				return false, fmt.Errorf("gobi: boolAtSeries: chunk not Boolean (%T)", chunk)
			}
			return ba.Value(row - offset), nil
		}
		offset += chunk.Len()
	}
	return false, fmt.Errorf("gobi: boolAtSeries: row %d out of range", row)
}
