package gobi

import (
	"fmt"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// Cast returns an expression that converts the inner column to the
// given arrow data type. Numeric-to-numeric conversions are supported
// today; string / boolean / date-time conversions are follow-ups.
//
// Supported source→target combinations (v1, numeric-to-numeric only):
//
//   - Float64 ← {Float32, Int64, Int32, Uint64, Uint32}
//   - Int64   ← {Float64, Float32, Int32, Uint32}
//   - Float32 ← {Float64, Int64, Int32}
//   - Int32   ← {Int64, Float64, Float32}
//   - Uint64  ← {Int64, Uint32}
//   - Uint32  ← {Uint64, Int64}
//
// Same-type is a no-op (returns the input). Unsupported combinations
// error at Eval with a clear message.
//
// Numeric widening carries values exactly. Narrowing (Int64→Int32,
// Float64→Int64, etc.) truncates without overflow-checking — mirrors
// Go's numeric conversion semantics. Nulls propagate: a null row in
// the source produces a null row in the output.
//
// Primary motivating use case is unlocking numeric widening in
// If / Coalesce, which require exact type match otherwise:
//
//	gobi.If(cond, gobi.Col("int_col").Cast(arrow.PrimitiveTypes.Float64),
//	              gobi.Lit(1.5))
func (e Expr) Cast(target arrow.DataType) Expr {
	return Expr{node: &castNode{inner: e.node, target: target}}
}

type castNode struct {
	inner  ExprNode
	target arrow.DataType
}

func (n *castNode) Eval(input *Frame) (Series, error) {
	if n.inner == nil {
		return Series{}, fmt.Errorf("gobi: Cast on nil inner expression")
	}
	if n.target == nil {
		return Series{}, fmt.Errorf("gobi: Cast target type is nil")
	}
	s, err := n.inner.Eval(input)
	if err != nil {
		return Series{}, err
	}
	return castSeries(s, n.target)
}

func (n *castNode) Type(schema *arrow.Schema) (arrow.DataType, error) {
	if n.target == nil {
		return nil, fmt.Errorf("gobi: Cast target type is nil")
	}
	if _, err := n.inner.Type(schema); err != nil {
		return nil, err
	}
	return n.target, nil
}

func (n *castNode) Children() []Expr { return []Expr{{node: n.inner}} }
func (n *castNode) String() string {
	return fmt.Sprintf("%s.cast(%s)", n.inner, n.target)
}

// castSeries converts s to target. No-op when source and target
// match. Numeric-to-numeric fast paths use a per-target dispatch;
// each path type-switches once on the source chunk and iterates
// with a direct typed accessor + Go's built-in numeric conversion.
func castSeries(s Series, target arrow.DataType) (Series, error) {
	if arrow.TypeEqual(s.DataType(), target) {
		return s, nil
	}
	switch target.ID() {
	case arrow.FLOAT64:
		return castToFloat64(s)
	case arrow.INT64:
		return castToInt64(s)
	case arrow.FLOAT32:
		return castToFloat32(s)
	case arrow.INT32:
		return castToInt32(s)
	case arrow.UINT64:
		return castToUint64(s)
	case arrow.UINT32:
		return castToUint32(s)
	}
	return Series{}, fmt.Errorf(
		"%w: Cast: unsupported target type %s (v1 covers Float32/64, Int32/64, Uint32/64)",
		ErrExprTypeMismatch, target)
}

func castToFloat64(s Series) (Series, error) {
	pool := memory.DefaultAllocator
	b := array.NewFloat64Builder(pool)
	defer b.Release()
	for _, chunk := range s.col.Data().Chunks() {
		switch arr := chunk.(type) {
		case *array.Float64:
			for i := range arr.Len() {
				if arr.IsNull(i) {
					b.AppendNull()
					continue
				}
				b.Append(arr.Value(i))
			}
		case *array.Float32:
			for i := range arr.Len() {
				if arr.IsNull(i) {
					b.AppendNull()
					continue
				}
				b.Append(float64(arr.Value(i)))
			}
		case *array.Int64:
			for i := range arr.Len() {
				if arr.IsNull(i) {
					b.AppendNull()
					continue
				}
				b.Append(float64(arr.Value(i)))
			}
		case *array.Int32:
			for i := range arr.Len() {
				if arr.IsNull(i) {
					b.AppendNull()
					continue
				}
				b.Append(float64(arr.Value(i)))
			}
		case *array.Uint64:
			for i := range arr.Len() {
				if arr.IsNull(i) {
					b.AppendNull()
					continue
				}
				b.Append(float64(arr.Value(i)))
			}
		case *array.Uint32:
			for i := range arr.Len() {
				if arr.IsNull(i) {
					b.AppendNull()
					continue
				}
				b.Append(float64(arr.Value(i)))
			}
		case *array.Timestamp:
			// Timestamp → Float64: emit the raw epoch value in the
			// timestamp's declared unit (nanoseconds / microseconds /
			// milliseconds / seconds). Callers that want a
			// unit-normalized value should route via
			// Expr.UnixNano().Cast(Float64) instead.
			for i := range arr.Len() {
				if arr.IsNull(i) {
					b.AppendNull()
					continue
				}
				b.Append(float64(arr.Value(i)))
			}
		default:
			return Series{}, castUnsupportedErr(chunk.DataType(), arrow.PrimitiveTypes.Float64)
		}
	}
	return arrayToSeries(pool, "cast", arrow.PrimitiveTypes.Float64, b.NewArray())
}

func castToInt64(s Series) (Series, error) {
	pool := memory.DefaultAllocator
	b := array.NewInt64Builder(pool)
	defer b.Release()
	for _, chunk := range s.col.Data().Chunks() {
		switch arr := chunk.(type) {
		case *array.Int64:
			for i := range arr.Len() {
				if arr.IsNull(i) {
					b.AppendNull()
					continue
				}
				b.Append(arr.Value(i))
			}
		case *array.Int32:
			for i := range arr.Len() {
				if arr.IsNull(i) {
					b.AppendNull()
					continue
				}
				b.Append(int64(arr.Value(i)))
			}
		case *array.Uint32:
			for i := range arr.Len() {
				if arr.IsNull(i) {
					b.AppendNull()
					continue
				}
				b.Append(int64(arr.Value(i)))
			}
		case *array.Float64:
			for i := range arr.Len() {
				if arr.IsNull(i) {
					b.AppendNull()
					continue
				}
				b.Append(int64(arr.Value(i)))
			}
		case *array.Float32:
			for i := range arr.Len() {
				if arr.IsNull(i) {
					b.AppendNull()
					continue
				}
				b.Append(int64(arr.Value(i)))
			}
		case *array.Timestamp:
			// Timestamp → Int64: emit the raw epoch value in the
			// timestamp's declared unit. See castToFloat64's Timestamp
			// arm for the unit-normalization note.
			for i := range arr.Len() {
				if arr.IsNull(i) {
					b.AppendNull()
					continue
				}
				b.Append(int64(arr.Value(i)))
			}
		default:
			return Series{}, castUnsupportedErr(chunk.DataType(), arrow.PrimitiveTypes.Int64)
		}
	}
	return arrayToSeries(pool, "cast", arrow.PrimitiveTypes.Int64, b.NewArray())
}

func castToFloat32(s Series) (Series, error) {
	pool := memory.DefaultAllocator
	b := array.NewFloat32Builder(pool)
	defer b.Release()
	for _, chunk := range s.col.Data().Chunks() {
		switch arr := chunk.(type) {
		case *array.Float32:
			for i := range arr.Len() {
				if arr.IsNull(i) {
					b.AppendNull()
					continue
				}
				b.Append(arr.Value(i))
			}
		case *array.Float64:
			for i := range arr.Len() {
				if arr.IsNull(i) {
					b.AppendNull()
					continue
				}
				b.Append(float32(arr.Value(i)))
			}
		case *array.Int64:
			for i := range arr.Len() {
				if arr.IsNull(i) {
					b.AppendNull()
					continue
				}
				b.Append(float32(arr.Value(i)))
			}
		case *array.Int32:
			for i := range arr.Len() {
				if arr.IsNull(i) {
					b.AppendNull()
					continue
				}
				b.Append(float32(arr.Value(i)))
			}
		default:
			return Series{}, castUnsupportedErr(chunk.DataType(), arrow.PrimitiveTypes.Float32)
		}
	}
	return arrayToSeries(pool, "cast", arrow.PrimitiveTypes.Float32, b.NewArray())
}

func castToInt32(s Series) (Series, error) {
	pool := memory.DefaultAllocator
	b := array.NewInt32Builder(pool)
	defer b.Release()
	for _, chunk := range s.col.Data().Chunks() {
		switch arr := chunk.(type) {
		case *array.Int32:
			for i := range arr.Len() {
				if arr.IsNull(i) {
					b.AppendNull()
					continue
				}
				b.Append(arr.Value(i))
			}
		case *array.Int64:
			for i := range arr.Len() {
				if arr.IsNull(i) {
					b.AppendNull()
					continue
				}
				b.Append(int32(arr.Value(i)))
			}
		case *array.Float64:
			for i := range arr.Len() {
				if arr.IsNull(i) {
					b.AppendNull()
					continue
				}
				b.Append(int32(arr.Value(i)))
			}
		case *array.Float32:
			for i := range arr.Len() {
				if arr.IsNull(i) {
					b.AppendNull()
					continue
				}
				b.Append(int32(arr.Value(i)))
			}
		default:
			return Series{}, castUnsupportedErr(chunk.DataType(), arrow.PrimitiveTypes.Int32)
		}
	}
	return arrayToSeries(pool, "cast", arrow.PrimitiveTypes.Int32, b.NewArray())
}

func castToUint64(s Series) (Series, error) {
	pool := memory.DefaultAllocator
	b := array.NewUint64Builder(pool)
	defer b.Release()
	for _, chunk := range s.col.Data().Chunks() {
		switch arr := chunk.(type) {
		case *array.Uint64:
			for i := range arr.Len() {
				if arr.IsNull(i) {
					b.AppendNull()
					continue
				}
				b.Append(arr.Value(i))
			}
		case *array.Uint32:
			for i := range arr.Len() {
				if arr.IsNull(i) {
					b.AppendNull()
					continue
				}
				b.Append(uint64(arr.Value(i)))
			}
		case *array.Int64:
			for i := range arr.Len() {
				if arr.IsNull(i) {
					b.AppendNull()
					continue
				}
				b.Append(uint64(arr.Value(i)))
			}
		default:
			return Series{}, castUnsupportedErr(chunk.DataType(), arrow.PrimitiveTypes.Uint64)
		}
	}
	return arrayToSeries(pool, "cast", arrow.PrimitiveTypes.Uint64, b.NewArray())
}

func castToUint32(s Series) (Series, error) {
	pool := memory.DefaultAllocator
	b := array.NewUint32Builder(pool)
	defer b.Release()
	for _, chunk := range s.col.Data().Chunks() {
		switch arr := chunk.(type) {
		case *array.Uint32:
			for i := range arr.Len() {
				if arr.IsNull(i) {
					b.AppendNull()
					continue
				}
				b.Append(arr.Value(i))
			}
		case *array.Uint64:
			for i := range arr.Len() {
				if arr.IsNull(i) {
					b.AppendNull()
					continue
				}
				b.Append(uint32(arr.Value(i)))
			}
		case *array.Int64:
			for i := range arr.Len() {
				if arr.IsNull(i) {
					b.AppendNull()
					continue
				}
				b.Append(uint32(arr.Value(i)))
			}
		default:
			return Series{}, castUnsupportedErr(chunk.DataType(), arrow.PrimitiveTypes.Uint32)
		}
	}
	return arrayToSeries(pool, "cast", arrow.PrimitiveTypes.Uint32, b.NewArray())
}

func castUnsupportedErr(src, target arrow.DataType) error {
	return fmt.Errorf("%w: Cast: unsupported source type %s → %s",
		ErrExprTypeMismatch, src, target)
}
