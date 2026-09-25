package gobi

import (
	"fmt"
	"math"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// Round rounds e's values to the nearest integer, halves away from
// zero (math.Round: 2.5 → 3, -2.5 → -3). Float32 / Float64 in, same
// type out; integer columns pass through unchanged. Nulls stay null;
// NaN and ±Inf are preserved.
//
// The result is still floating point. To land it in an integer field,
// follow with ToStructs(…, StructCoerceNumbers()) — the rounded values
// are whole, so the conversion is exact, and values outside the
// field's range still fail with ErrStructFieldOverflow. (Cast to an
// integer type truncates instead and doesn't range-check.)
func (e Expr) Round() Expr { return Expr{node: &roundNode{inner: e.node, op: roundNearest}} }

// Floor rounds e's values down (toward −Inf). Same typing as Round.
func (e Expr) Floor() Expr { return Expr{node: &roundNode{inner: e.node, op: roundFloor}} }

// Ceil rounds e's values up (toward +Inf). Same typing as Round.
func (e Expr) Ceil() Expr { return Expr{node: &roundNode{inner: e.node, op: roundCeil}} }

// Trunc rounds e's values toward zero, dropping the fraction. Same
// typing as Round.
func (e Expr) Trunc() Expr { return Expr{node: &roundNode{inner: e.node, op: roundTrunc}} }

type roundOp uint8

const (
	roundNearest roundOp = iota
	roundFloor
	roundCeil
	roundTrunc
)

func (op roundOp) String() string {
	switch op {
	case roundFloor:
		return "floor"
	case roundCeil:
		return "ceil"
	case roundTrunc:
		return "trunc"
	}
	return "round"
}

func (op roundOp) fn() func(float64) float64 {
	switch op {
	case roundFloor:
		return math.Floor
	case roundCeil:
		return math.Ceil
	case roundTrunc:
		return math.Trunc
	}
	return math.Round
}

type roundNode struct {
	inner ExprNode
	op    roundOp
}

func (n *roundNode) Eval(input *Frame) (Series, error) {
	if n.inner == nil {
		return Series{}, fmt.Errorf("gobi: %s on nil inner expression", n.op)
	}
	s, err := n.inner.Eval(input)
	if err != nil {
		return Series{}, err
	}
	dt := s.DataType()
	switch dt.ID() {
	case arrow.INT8, arrow.INT16, arrow.INT32, arrow.INT64,
		arrow.UINT8, arrow.UINT16, arrow.UINT32, arrow.UINT64:
		return s, nil // already whole
	case arrow.FLOAT64, arrow.FLOAT32:
	default:
		return Series{}, fmt.Errorf("%w: %s requires a numeric column, got %s", ErrExprTypeMismatch, n.op, dt)
	}

	f := n.op.fn()
	pool := memory.DefaultAllocator
	if dt.ID() == arrow.FLOAT64 {
		b := array.NewFloat64Builder(pool)
		defer b.Release()
		b.Reserve(s.Len())
		for _, chunk := range s.col.Data().Chunks() {
			a := chunk.(*array.Float64)
			for i := range a.Len() {
				if a.IsNull(i) {
					b.AppendNull()
					continue
				}
				b.Append(f(a.Value(i)))
			}
		}
		return arrayToSeries(pool, s.name, dt, b.NewArray())
	}
	b := array.NewFloat32Builder(pool)
	defer b.Release()
	b.Reserve(s.Len())
	for _, chunk := range s.col.Data().Chunks() {
		a := chunk.(*array.Float32)
		for i := range a.Len() {
			if a.IsNull(i) {
				b.AppendNull()
				continue
			}
			// Exact: every float32 is a float64, and rounding a float32
			// value yields an integer-valued float32.
			b.Append(float32(f(float64(a.Value(i)))))
		}
	}
	return arrayToSeries(pool, s.name, dt, b.NewArray())
}

func (n *roundNode) Type(schema *arrow.Schema) (arrow.DataType, error) {
	t, err := n.inner.Type(schema)
	if err != nil {
		return nil, err
	}
	switch t.ID() {
	case arrow.INT8, arrow.INT16, arrow.INT32, arrow.INT64,
		arrow.UINT8, arrow.UINT16, arrow.UINT32, arrow.UINT64,
		arrow.FLOAT64, arrow.FLOAT32:
		return t, nil
	}
	return nil, fmt.Errorf("%w: %s requires a numeric column, got %s", ErrExprTypeMismatch, n.op, t)
}

func (n *roundNode) Children() []Expr { return []Expr{{node: n.inner}} }
func (n *roundNode) String() string   { return fmt.Sprintf("%s.%s()", n.inner, n.op) }
