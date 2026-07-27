package gobi

import (
	"fmt"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// UnixNano returns an expression that converts a Timestamp column to
// Int64 nanoseconds since the Unix epoch, normalizing across the
// source column's TimeUnit (Second / Millisecond / Microsecond /
// Nanosecond). Distinct from `Cast(Int64)` on a Timestamp — that
// returns the raw underlying value in the source unit; UnixNano
// always scales to nanoseconds.
//
// Combines with arithmetic to derive time-delta expressions inline
// without leaving LazyFrame land:
//
//	// ts_hours = float64 hours-since-epoch
//	Col("ts").UnixNano().Cast(arrow.PrimitiveTypes.Float64).
//	    Div(Lit(float64(time.Hour)))
//
// Nulls propagate. Errors at Eval time if the source isn't a
// Timestamp column.
func (e Expr) UnixNano() Expr {
	return Expr{node: &unixNanoNode{inner: e.node}}
}

type unixNanoNode struct {
	inner ExprNode
}

func (n *unixNanoNode) Eval(input *Frame) (Series, error) {
	if n.inner == nil {
		return Series{}, fmt.Errorf("gobi: UnixNano on nil inner expression")
	}
	s, err := n.inner.Eval(input)
	if err != nil {
		return Series{}, err
	}
	dt, ok := s.DataType().(*arrow.TimestampType)
	if !ok {
		return Series{}, fmt.Errorf(
			"%w: UnixNano requires a Timestamp column, got %s",
			ErrExprTypeMismatch, s.DataType())
	}
	mult := unitToNanos(dt.Unit)

	pool := memory.DefaultAllocator
	b := array.NewInt64Builder(pool)
	defer b.Release()
	for _, chunk := range s.col.Data().Chunks() {
		arr, ok := chunk.(*array.Timestamp)
		if !ok {
			return Series{}, fmt.Errorf(
				"%w: UnixNano: chunk type %T isn't *array.Timestamp",
				ErrExprTypeMismatch, chunk)
		}
		for i := range arr.Len() {
			if arr.IsNull(i) {
				b.AppendNull()
				continue
			}
			b.Append(int64(arr.Value(i)) * mult)
		}
	}
	return arrayToSeries(pool, "unix_nano", arrow.PrimitiveTypes.Int64, b.NewArray())
}

func (n *unixNanoNode) Type(schema *arrow.Schema) (arrow.DataType, error) {
	innerType, err := n.inner.Type(schema)
	if err != nil {
		return nil, err
	}
	if _, ok := innerType.(*arrow.TimestampType); !ok {
		return nil, fmt.Errorf(
			"%w: UnixNano requires a Timestamp column, got %s",
			ErrExprTypeMismatch, innerType)
	}
	return arrow.PrimitiveTypes.Int64, nil
}

func (n *unixNanoNode) Children() []Expr { return []Expr{{node: n.inner}} }
func (n *unixNanoNode) String() string   { return fmt.Sprintf("%s.unix_nano()", n.inner) }

// unitToNanos returns the multiplier to convert a raw Timestamp
// value in the given TimeUnit into nanoseconds.
func unitToNanos(u arrow.TimeUnit) int64 {
	switch u {
	case arrow.Nanosecond:
		return 1
	case arrow.Microsecond:
		return 1_000
	case arrow.Millisecond:
		return 1_000_000
	case arrow.Second:
		return 1_000_000_000
	}
	return 1
}
