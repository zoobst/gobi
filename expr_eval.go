package gobi

import (
	"fmt"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// -----------------------------------------------------------------------------
// colRefNode: `Col("x")`
// -----------------------------------------------------------------------------

type colRefNode struct {
	name string
}

func (n *colRefNode) Eval(input *Frame) (Series, error) {
	return input.Column(n.name)
}

func (n *colRefNode) Type(schema *arrow.Schema) (arrow.DataType, error) {
	fields, ok := schema.FieldsByName(n.name)
	if !ok || len(fields) == 0 {
		return nil, fmt.Errorf("%w: %q", ErrColumnNotFound, n.name)
	}
	return fields[0].Type, nil
}

func (n *colRefNode) Children() []Expr { return nil }
func (n *colRefNode) String() string   { return fmt.Sprintf("col(%q)", n.name) }

// -----------------------------------------------------------------------------
// literalNode: `Lit(v)`
// -----------------------------------------------------------------------------

type literalNode struct {
	value any
	dtype arrow.DataType
	err   error // set at construction time if the Go type is unsupported
}

func newLiteralNode(v any) *literalNode {
	switch v := v.(type) {
	case bool:
		return &literalNode{value: v, dtype: arrow.FixedWidthTypes.Boolean}
	case int:
		return &literalNode{value: int64(v), dtype: arrow.PrimitiveTypes.Int64}
	case int32:
		return &literalNode{value: v, dtype: arrow.PrimitiveTypes.Int32}
	case int64:
		return &literalNode{value: v, dtype: arrow.PrimitiveTypes.Int64}
	case float32:
		return &literalNode{value: v, dtype: arrow.PrimitiveTypes.Float32}
	case float64:
		return &literalNode{value: v, dtype: arrow.PrimitiveTypes.Float64}
	case string:
		return &literalNode{value: v, dtype: arrow.BinaryTypes.String}
	default:
		return &literalNode{err: fmt.Errorf("%w: literal of type %T",
			ErrUnsupportedLiteral, v)}
	}
}

func (n *literalNode) Eval(input *Frame) (Series, error) {
	if n.err != nil {
		return Series{}, n.err
	}
	return broadcastLiteral(n.value, n.dtype, input.NumRows())
}

func (n *literalNode) Type(*arrow.Schema) (arrow.DataType, error) {
	if n.err != nil {
		return nil, n.err
	}
	return n.dtype, nil
}

func (n *literalNode) Children() []Expr { return nil }
func (n *literalNode) String() string {
	if n.err != nil {
		return fmt.Sprintf("lit(<err %v>)", n.err)
	}
	if s, ok := n.value.(string); ok {
		return fmt.Sprintf("lit(%q)", s)
	}
	return fmt.Sprintf("lit(%v)", n.value)
}

// literalNullNode broadcasts a typed null to every input row. Kept
// separate from literalNode because null literals need explicit type
// info at construction time (a Go nil has no arrow-shaped type). Users
// build these via gobi.LitNull(dtype).
type literalNullNode struct {
	dtype arrow.DataType
}

func (n *literalNullNode) Eval(input *Frame) (Series, error) {
	if n.dtype == nil {
		return Series{}, fmt.Errorf("gobi: LitNull with nil dtype")
	}
	pool := memory.DefaultAllocator
	b, err := builderForType(pool, n.dtype)
	if err != nil {
		return Series{}, fmt.Errorf("LitNull: %w", err)
	}
	defer b.Release()
	for range input.NumRows() {
		b.AppendNull()
	}
	return arrayToSeries(pool, "lit_null", n.dtype, b.NewArray())
}

func (n *literalNullNode) Type(_ *arrow.Schema) (arrow.DataType, error) {
	if n.dtype == nil {
		return nil, fmt.Errorf("gobi: LitNull with nil dtype")
	}
	return n.dtype, nil
}

func (n *literalNullNode) Children() []Expr { return nil }
func (n *literalNullNode) String() string   { return fmt.Sprintf("lit_null(%s)", n.dtype) }

// literalEmptyListNode broadcasts a non-null empty list of the given
// element type to every input row. Distinct from LitNull(ListOf(t)),
// which produces null-list rows. Built via gobi.LitEmptyList(elemType).
type literalEmptyListNode struct {
	elemType arrow.DataType
}

func (n *literalEmptyListNode) Eval(input *Frame) (Series, error) {
	if n.elemType == nil {
		return Series{}, fmt.Errorf("gobi: LitEmptyList with nil element type")
	}
	pool := memory.DefaultAllocator
	lb := array.NewListBuilder(pool, n.elemType)
	defer lb.Release()
	for range input.NumRows() {
		lb.Append(true) // non-null, zero elements — the value builder gets nothing.
	}
	return arrayToSeries(pool, "lit_empty_list", arrow.ListOf(n.elemType), lb.NewArray())
}

func (n *literalEmptyListNode) Type(_ *arrow.Schema) (arrow.DataType, error) {
	if n.elemType == nil {
		return nil, fmt.Errorf("gobi: LitEmptyList with nil element type")
	}
	return arrow.ListOf(n.elemType), nil
}

func (n *literalEmptyListNode) Children() []Expr { return nil }
func (n *literalEmptyListNode) String() string {
	return fmt.Sprintf("lit_empty_list(%s)", n.elemType)
}

// asFloat64 extracts a float64 from a literal that could be int32/int64/
// float32/float64. Used to hit the *Scalar fast paths on Series, which
// speak float64.
func (n *literalNode) asFloat64() (float64, bool) {
	switch v := n.value.(type) {
	case int64:
		return float64(v), true
	case int32:
		return float64(v), true
	case float64:
		return v, true
	case float32:
		return float64(v), true
	}
	return 0, false
}

// -----------------------------------------------------------------------------
// binOpNode: `left OP right`
// -----------------------------------------------------------------------------

type binOpNode struct {
	op          binOpKind
	left, right ExprNode
}

func (n *binOpNode) Eval(input *Frame) (Series, error) {
	// Fast path: (col op literal). Applies to arithmetic and numeric
	// comparisons; left must be non-literal, right must be a numeric
	// literal that maps cleanly to Series' *Scalar methods.
	if rlit, ok := n.right.(*literalNode); ok && rlit.err == nil {
		if _, isLeftLit := n.left.(*literalNode); !isLeftLit {
			if v, ok := rlit.asFloat64(); ok {
				left, err := n.left.Eval(input)
				if err != nil {
					return Series{}, err
				}
				// Integer-preserving fast path: when the input column
				// is Int64 single-chunk, the literal is a
				// losslessly-representable int64, and the op isn't Div
				// (which per IEEE widens), emit Int64 so binOpNode.Type()
				// (Int64) matches the runtime output. Series-level
				// AddScalar/SubScalar/MulScalar keep their public
				// Float64-emitting contract; this routing lives in the
				// expression layer where we know both the column dtype
				// and the literal's original dtype.
				if (n.op != bopDiv && isIntegerType(rlit.dtype)) || n.op.isBitwise() {
					if s, ok, err := tryScalarIntFastPath(n.op, left, int64(v)); err != nil {
						return Series{}, err
					} else if ok {
						return s, nil
					}
				}
				// Bitwise ops don't have a float fallback — skip the
				// float64 fast path. Non-integer inputs surface at
				// Type() check time; here we just refuse to widen.
				if n.op.isBitwise() {
					// Fall through to general col-vs-col path with a
					// literal broadcast on the right.
				} else if s, ok, err := tryScalarFastPath(n.op, left, v); err != nil {
					return Series{}, err
				} else if ok {
					return s, nil
				}
			}
		}
	}

	left, err := n.left.Eval(input)
	if err != nil {
		return Series{}, err
	}
	right, err := n.right.Eval(input)
	if err != nil {
		return Series{}, err
	}
	return applyBinaryOp(n.op, left, right)
}

// tryScalarIntFastPath emits (col op int-literal) with Int64 output
// preserved — only when the input is Int64 single-chunk. Returns
// (_, false, nil) otherwise so the caller can fall back to the
// float64 scalar path (which widens) or the general col-vs-col path.
// Fires for non-Div arithmetic AND bitwise ops (which are defined
// only for integer operands).
func tryScalarIntFastPath(op binOpKind, left Series, v int64) (Series, bool, error) {
	vals, arr, ok := left.singleI64()
	if !ok {
		return Series{}, false, nil
	}
	switch op {
	case bopAdd:
		return scalarI64(vals, arr, v, opAdd, left.name), true, nil
	case bopSub:
		return scalarI64(vals, arr, v, opSub, left.name), true, nil
	case bopMul:
		return scalarI64(vals, arr, v, opMul, left.name), true, nil
	case bopBitAnd:
		return scalarI64(vals, arr, v, opBitAnd, left.name), true, nil
	case bopBitOr:
		return scalarI64(vals, arr, v, opBitOr, left.name), true, nil
	case bopBitXor:
		return scalarI64(vals, arr, v, opBitXor, left.name), true, nil
	}
	return Series{}, false, nil
}

// isIntegerType reports whether dt is one of the integer arrow types
// gobi's literals can carry. Used to decide whether an int-preserving
// scalar path applies.
func isIntegerType(dt arrow.DataType) bool {
	if dt == nil {
		return false
	}
	switch dt.ID() {
	case arrow.INT32, arrow.INT64, arrow.UINT32, arrow.UINT64:
		return true
	}
	return false
}

// bitwiseColCol dispatches (col & col), (col | col), (col ^ col) to
// the Int64 kernel. Requires both operands be Int64-typed at
// runtime — binOpNode.Type already gate-checked integer types at
// Compile, but a mismatched pair reaching here (e.g. Int32) returns
// a clear error rather than a panic.
func bitwiseColCol(left, right Series, op binOpKind) (Series, error) {
	if left.Len() != right.Len() {
		return Series{}, fmt.Errorf("%w: %d vs %d",
			ErrColumnLenMismatch, left.Len(), right.Len())
	}
	aVals, aArr, aOk := left.singleI64()
	bVals, bArr, bOk := right.singleI64()
	if !aOk || !bOk {
		return Series{}, fmt.Errorf(
			"%w: bitwise %s requires Int64 single-chunk operands (got %s and %s)",
			ErrExprTypeMismatch, op, left.DataType(), right.DataType())
	}
	var kernelOp arithOp
	switch op {
	case bopBitAnd:
		kernelOp = opBitAnd
	case bopBitOr:
		kernelOp = opBitOr
	case bopBitXor:
		kernelOp = opBitXor
	default:
		return Series{}, fmt.Errorf("%w: bitwiseColCol: op %s", ErrExprTypeMismatch, op)
	}
	return arithI64I64(aVals, bVals, aArr, bArr, kernelOp, left.Name()), nil
}

func (n *binOpNode) Type(schema *arrow.Schema) (arrow.DataType, error) {
	lt, err := n.left.Type(schema)
	if err != nil {
		return nil, err
	}
	rt, err := n.right.Type(schema)
	if err != nil {
		return nil, err
	}
	switch {
	case n.op.isBitwise():
		// Bitwise ops are integer-only. Require both operands be
		// Int32/Int64/Uint32/Uint64; error out on float/bool/string.
		if !isIntegerType(lt) || !isIntegerType(rt) {
			return nil, fmt.Errorf("%w: %s requires integer operands, got %s and %s",
				ErrExprTypeMismatch, n.op, lt, rt)
		}
		// Output type: Int64 in all cases (matches scalarI64/
		// arithI64I64 kernels).
		return arrow.PrimitiveTypes.Int64, nil
	case n.op.isArithmetic():
		// Div always widens to Float64 per IEEE-754 division semantics
		// (Series.Div passes wantFloat=true), even for Int64 / Int64.
		// Every other arithmetic op honors promoteNumeric.
		if n.op == bopDiv {
			if _, err := promoteNumeric(lt, rt); err != nil {
				return nil, err
			}
			return arrow.PrimitiveTypes.Float64, nil
		}
		return promoteNumeric(lt, rt)
	case n.op.isComparison():
		if _, err := promoteForComparison(lt, rt); err != nil {
			return nil, err
		}
		return arrow.FixedWidthTypes.Boolean, nil
	case n.op.isLogical():
		if lt.ID() != arrow.BOOL || rt.ID() != arrow.BOOL {
			return nil, fmt.Errorf("%w: %s requires Boolean operands, got %s and %s",
				ErrExprTypeMismatch, n.op, lt, rt)
		}
		return arrow.FixedWidthTypes.Boolean, nil
	}
	return nil, fmt.Errorf("%w: unknown op %s", ErrExprTypeMismatch, n.op)
}

func (n *binOpNode) Children() []Expr {
	return []Expr{{node: n.left}, {node: n.right}}
}

func (n *binOpNode) String() string {
	return fmt.Sprintf("(%s %s %s)", n.left, n.op, n.right)
}

// tryScalarFastPath attempts to dispatch (col op literal) to a Series
// *Scalar method. Returns (result, true, nil) on success, (_, false,
// nil) if the op has no fast path and the caller should fall back to
// the general column-vs-column path.
func tryScalarFastPath(op binOpKind, left Series, rhs float64) (Series, bool, error) {
	switch op {
	case bopAdd:
		s, err := left.AddScalar(rhs)
		return s, true, err
	case bopSub:
		s, err := left.SubScalar(rhs)
		return s, true, err
	case bopMul:
		s, err := left.MulScalar(rhs)
		return s, true, err
	case bopDiv:
		s, err := left.DivScalar(rhs)
		return s, true, err
	case bopEq:
		s, err := left.EqScalar(rhs)
		return s, true, err
	case bopLt:
		s, err := left.LtScalar(rhs)
		return s, true, err
	case bopGt:
		s, err := left.GtScalar(rhs)
		return s, true, err
	}
	// Ne/Le/Ge and the logical ops go through the general path.
	return Series{}, false, nil
}

// applyBinaryOp performs left OP right on full-length Series.
func applyBinaryOp(op binOpKind, left, right Series) (Series, error) {
	// String Eq/Ne: Series.Eq is numeric-only, so route string operand
	// pairs through a dedicated comparator here.
	if op == bopEq || op == bopNe {
		if left.DataType() != nil && left.DataType().ID() == arrow.STRING &&
			right.DataType() != nil && right.DataType().ID() == arrow.STRING {
			return stringCompare(left, right, op == bopNe)
		}
	}
	switch op {
	case bopAdd:
		return left.Add(right)
	case bopSub:
		return left.Sub(right)
	case bopMul:
		return left.Mul(right)
	case bopDiv:
		return left.Div(right)
	case bopBitAnd, bopBitOr, bopBitXor:
		return bitwiseColCol(left, right, op)
	case bopEq:
		return left.Eq(right)
	case bopNe:
		return left.Ne(right)
	case bopLt:
		return left.Lt(right)
	case bopLe:
		return left.Le(right)
	case bopGt:
		return left.Gt(right)
	case bopGe:
		return left.Ge(right)
	case bopAnd:
		return boolBinary(left, right, func(a, b bool) bool { return a && b })
	case bopOr:
		return boolBinary(left, right, func(a, b bool) bool { return a || b })
	}
	return Series{}, fmt.Errorf("%w: unhandled op %s", ErrExprTypeMismatch, op)
}

// stringCompare returns a Boolean Series of left ==/!= right, element-
// wise. Both inputs must be single-chunk String columns of the same
// length; the caller is expected to have already verified that.
func stringCompare(left, right Series, negate bool) (Series, error) {
	if left.Len() != right.Len() {
		return Series{}, fmt.Errorf("%w: %d vs %d",
			ErrColumnLenMismatch, left.Len(), right.Len())
	}
	la := left.col.Data().Chunks()[0].(*array.String)
	ra := right.col.Data().Chunks()[0].(*array.String)
	n := la.Len()

	pool := memory.DefaultAllocator
	out := array.NewBooleanBuilder(pool)
	defer out.Release()
	for i := range n {
		if la.IsNull(i) || ra.IsNull(i) {
			out.AppendNull()
			continue
		}
		eq := la.Value(i) == ra.Value(i)
		if negate {
			eq = !eq
		}
		out.Append(eq)
	}
	return arrayToSeries(pool, "", arrow.FixedWidthTypes.Boolean, out.NewArray())
}

// -----------------------------------------------------------------------------
// notNode: `Not(inner)`
// -----------------------------------------------------------------------------

type notNode struct {
	inner ExprNode
}

func (n *notNode) Eval(input *Frame) (Series, error) {
	s, err := n.inner.Eval(input)
	if err != nil {
		return Series{}, err
	}
	return boolUnary(s, func(b bool) bool { return !b })
}

func (n *notNode) Type(schema *arrow.Schema) (arrow.DataType, error) {
	t, err := n.inner.Type(schema)
	if err != nil {
		return nil, err
	}
	if t.ID() != arrow.BOOL {
		return nil, fmt.Errorf("%w: NOT requires Boolean, got %s",
			ErrExprTypeMismatch, t)
	}
	return arrow.FixedWidthTypes.Boolean, nil
}

func (n *notNode) Children() []Expr { return []Expr{{node: n.inner}} }
func (n *notNode) String() string   { return fmt.Sprintf("NOT %s", n.inner) }

// -----------------------------------------------------------------------------
// aliasNode: rename output
// -----------------------------------------------------------------------------

type aliasNode struct {
	inner ExprNode
	name  string
}

func (n *aliasNode) Eval(input *Frame) (Series, error) {
	s, err := n.inner.Eval(input)
	if err != nil {
		return Series{}, err
	}
	return renameSeries(s, n.name), nil
}

func (n *aliasNode) Type(schema *arrow.Schema) (arrow.DataType, error) {
	return n.inner.Type(schema)
}

func (n *aliasNode) Children() []Expr { return []Expr{{node: n.inner}} }
func (n *aliasNode) String() string   { return fmt.Sprintf("%s AS %q", n.inner, n.name) }

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

// broadcastLiteral produces a Series of n copies of value with the
// given arrow type. Used when a Literal appears outside a scalar fast
// path (e.g. as the left operand, or as the sole value of an entire
// expression).
func broadcastLiteral(value any, dtype arrow.DataType, n int) (Series, error) {
	pool := memory.DefaultAllocator
	switch dtype.ID() {
	case arrow.BOOL:
		v := value.(bool)
		b := array.NewBooleanBuilder(pool)
		defer b.Release()
		for range n {
			b.Append(v)
		}
		return arrayToSeries(pool, "lit", dtype, b.NewArray())
	case arrow.INT64:
		v := value.(int64)
		b := array.NewInt64Builder(pool)
		defer b.Release()
		for range n {
			b.Append(v)
		}
		return arrayToSeries(pool, "lit", dtype, b.NewArray())
	case arrow.INT32:
		v := value.(int32)
		b := array.NewInt32Builder(pool)
		defer b.Release()
		for range n {
			b.Append(v)
		}
		return arrayToSeries(pool, "lit", dtype, b.NewArray())
	case arrow.FLOAT64:
		v := value.(float64)
		b := array.NewFloat64Builder(pool)
		defer b.Release()
		for range n {
			b.Append(v)
		}
		return arrayToSeries(pool, "lit", dtype, b.NewArray())
	case arrow.FLOAT32:
		v := value.(float32)
		b := array.NewFloat32Builder(pool)
		defer b.Release()
		for range n {
			b.Append(v)
		}
		return arrayToSeries(pool, "lit", dtype, b.NewArray())
	case arrow.STRING:
		v := value.(string)
		b := array.NewStringBuilder(pool)
		defer b.Release()
		for range n {
			b.Append(v)
		}
		return arrayToSeries(pool, "lit", dtype, b.NewArray())
	}
	return Series{}, fmt.Errorf("%w: cannot broadcast literal of type %s",
		ErrUnsupportedLiteral, dtype)
}

// arrayToSeries wraps a fresh arrow.Array in a Series named name. Thin
// wrapper over SeriesFromArray that keeps the (allocator, error)
// signature for the expression-eval callers that expect it — the
// allocator argument is unused (kept for API stability across
// call sites that pass memory.DefaultAllocator).
func arrayToSeries(_ memory.Allocator, name string, dtype arrow.DataType, arr arrow.Array) (Series, error) {
	return SeriesFromArray(
		arrow.Field{Name: name, Type: dtype, Nullable: true}, arr), nil
}

// renameSeries returns a Series identical to s but under a new name.
// Shares buffers via arrow's ref-counted Chunked.
func renameSeries(s Series, name string) Series {
	field := s.field
	field.Name = name
	col := arrow.NewColumn(field, s.col.Data())
	return NewSeries(col)
}

// boolBinary combines two Boolean Series element-wise via op. Returns
// an error if either input isn't Boolean or lengths differ.
func boolBinary(a, b Series, op func(x, y bool) bool) (Series, error) {
	if err := requireBool(a, "left"); err != nil {
		return Series{}, err
	}
	if err := requireBool(b, "right"); err != nil {
		return Series{}, err
	}
	if a.Len() != b.Len() {
		return Series{}, fmt.Errorf("%w: %d vs %d",
			ErrColumnLenMismatch, a.Len(), b.Len())
	}
	aa := a.col.Data().Chunks()[0].(*array.Boolean)
	bb := b.col.Data().Chunks()[0].(*array.Boolean)
	n := aa.Len()

	pool := memory.DefaultAllocator
	out := array.NewBooleanBuilder(pool)
	defer out.Release()
	for i := range n {
		if aa.IsNull(i) || bb.IsNull(i) {
			out.AppendNull()
			continue
		}
		out.Append(op(aa.Value(i), bb.Value(i)))
	}
	return arrayToSeries(pool, "", arrow.FixedWidthTypes.Boolean, out.NewArray())
}

// boolUnary applies op element-wise to a Boolean Series.
func boolUnary(s Series, op func(bool) bool) (Series, error) {
	if err := requireBool(s, "operand"); err != nil {
		return Series{}, err
	}
	aa := s.col.Data().Chunks()[0].(*array.Boolean)
	n := aa.Len()

	pool := memory.DefaultAllocator
	out := array.NewBooleanBuilder(pool)
	defer out.Release()
	for i := range n {
		if aa.IsNull(i) {
			out.AppendNull()
			continue
		}
		out.Append(op(aa.Value(i)))
	}
	return arrayToSeries(pool, "", arrow.FixedWidthTypes.Boolean, out.NewArray())
}

func requireBool(s Series, label string) error {
	if s.DataType() == nil || s.DataType().ID() != arrow.BOOL {
		return fmt.Errorf("%w: %s must be Boolean, got %s",
			ErrExprTypeMismatch, label, s.DataType())
	}
	return nil
}

// promoteNumeric returns the arithmetic result type of (lt OP rt).
// Follows the same promotion rules as Series.Add etc.: if either side
// is Float64 the result is Float64; otherwise if both are Int64 the
// result is Int64. Other numeric combinations error out and the caller
// is expected to widen first via a future Cast node.
func promoteNumeric(lt, rt arrow.DataType) (arrow.DataType, error) {
	if !isNumericType(lt) || !isNumericType(rt) {
		return nil, fmt.Errorf("%w: arithmetic on %s and %s",
			ErrExprTypeMismatch, lt, rt)
	}
	if lt.ID() == arrow.FLOAT64 || rt.ID() == arrow.FLOAT64 {
		return arrow.PrimitiveTypes.Float64, nil
	}
	if lt.ID() == arrow.INT64 && rt.ID() == arrow.INT64 {
		return arrow.PrimitiveTypes.Int64, nil
	}
	return arrow.PrimitiveTypes.Float64, nil
}

// promoteForComparison verifies that lt and rt can be compared. Numeric
// types compare against any numeric type; String compares against
// String; Boolean against Boolean. Other combinations error.
func promoteForComparison(lt, rt arrow.DataType) (arrow.DataType, error) {
	if isNumericType(lt) && isNumericType(rt) {
		return promoteNumeric(lt, rt)
	}
	if lt.ID() == arrow.STRING && rt.ID() == arrow.STRING {
		return arrow.BinaryTypes.String, nil
	}
	if lt.ID() == arrow.BOOL && rt.ID() == arrow.BOOL {
		return arrow.FixedWidthTypes.Boolean, nil
	}
	return nil, fmt.Errorf("%w: comparison between %s and %s",
		ErrExprTypeMismatch, lt, rt)
}

func isNumericType(t arrow.DataType) bool {
	switch t.ID() {
	case arrow.INT32, arrow.INT64, arrow.FLOAT32, arrow.FLOAT64,
		arrow.UINT32, arrow.UINT64:
		return true
	}
	return false
}
