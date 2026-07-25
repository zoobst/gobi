package gobi

import (
	"fmt"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// IsNull returns a Boolean expression that is true where e evaluates
// to null, false everywhere else. The returned column itself is never
// null.
//
// Composes with Filter / WithColumn / Select and with the boolean
// combinators (And, Or, Not):
//
//	// Rows where price is missing.
//	f.FilterExpr(gobi.Col("price").IsNull())
//
//	// Rows where price is set AND qty is missing.
//	f.FilterExpr(gobi.Col("price").IsNotNull().And(gobi.Col("qty").IsNull()))
func (e Expr) IsNull() Expr {
	return Expr{node: &nullCheckNode{inner: e.node, wantNull: true}}
}

// IsNotNull returns a Boolean expression that is true where e
// evaluates to a non-null value. Semantic complement of IsNull.
func (e Expr) IsNotNull() Expr {
	return Expr{node: &nullCheckNode{inner: e.node, wantNull: false}}
}

// nullCheckNode emits a Bool column derived from the inner Series'
// validity bitmap. wantNull inverts the polarity: true → IsNull,
// false → IsNotNull.
type nullCheckNode struct {
	inner    ExprNode
	wantNull bool
}

func (n *nullCheckNode) Eval(input *Frame) (Series, error) {
	if n.inner == nil {
		return Series{}, fmt.Errorf("gobi: IsNull/IsNotNull on nil inner expression")
	}
	s, err := n.inner.Eval(input)
	if err != nil {
		return Series{}, err
	}
	pool := memory.DefaultAllocator
	b := array.NewBooleanBuilder(pool)
	defer b.Release()
	for _, chunk := range s.col.Data().Chunks() {
		for i := range chunk.Len() {
			b.Append(chunk.IsNull(i) == n.wantNull)
		}
	}
	// arrayToSeries takes ownership of the array and Releases it after
	// wrapping — no defer here or we'd double-release.
	return arrayToSeries(pool, n.name(), arrow.FixedWidthTypes.Boolean, b.NewArray())
}

func (n *nullCheckNode) Type(schema *arrow.Schema) (arrow.DataType, error) {
	if n.inner == nil {
		return nil, fmt.Errorf("gobi: IsNull/IsNotNull on nil inner expression")
	}
	// Force inner type inference so mistyped inner surfaces early
	// (unknown column, bad literal, etc.).
	if _, err := n.inner.Type(schema); err != nil {
		return nil, err
	}
	return arrow.FixedWidthTypes.Boolean, nil
}

func (n *nullCheckNode) Children() []Expr { return []Expr{{node: n.inner}} }

func (n *nullCheckNode) String() string {
	if n.wantNull {
		return fmt.Sprintf("%s.is_null()", n.inner)
	}
	return fmt.Sprintf("%s.is_not_null()", n.inner)
}

// name picks the output Series name for this node. For nullCheckNode
// wrapping a Namer inner we suffix with "_is_null" / "_is_not_null" so
// the caller sees a stable, self-descriptive column when using Select.
// Callers that want a specific name should Alias.
func (n *nullCheckNode) name() string {
	suffix := "_is_not_null"
	if n.wantNull {
		suffix = "_is_null"
	}
	if inner, ok := n.inner.(Namer); ok {
		if s := inner.OutputName(); s != "" {
			return s + suffix
		}
	}
	return "expr" + suffix
}

// OutputName so Select picks the derived name automatically.
func (n *nullCheckNode) OutputName() string { return n.name() }

// -----------------------------------------------------------------------------
// Coalesce — SQL-style first-non-null across N operands.
// -----------------------------------------------------------------------------

// Coalesce returns an expression that evaluates to the first non-null
// value among exprs, per row. Matches SQL COALESCE / polars
// `coalesce()`. All operands must produce the same arrow type — no
// automatic widening; cast explicitly or match Lit types.
//
// If every operand is null at a given row, the output at that row is
// null too. Zero operands or a single operand each error at
// construction time (zero) or degenerate to a passthrough (single).
//
// Common use — fill nulls on either side of a Full outer join before
// running ListUnion:
//
//	segSafe  := gobi.Coalesce(gobi.Col("seg"),  gobi.LitEmptyList(arrow.BinaryTypes.String))
//	pingSafe := gobi.Coalesce(gobi.Col("ping"), gobi.LitEmptyList(arrow.BinaryTypes.String))
//	merged   := segSafe.ListUnion(pingSafe)
//
// Not short-circuit: every operand is evaluated in full. If short-
// circuit matters (e.g. an expensive right-hand operand you only want
// on rows where the left is null), split into filtered pipelines.
func Coalesce(exprs ...Expr) Expr {
	nodes := make([]ExprNode, len(exprs))
	for i, e := range exprs {
		nodes[i] = e.node
	}
	return Expr{node: &coalesceNode{operands: nodes}}
}

type coalesceNode struct {
	operands []ExprNode
}

func (n *coalesceNode) Eval(input *Frame) (Series, error) {
	if len(n.operands) == 0 {
		return Series{}, fmt.Errorf("gobi: Coalesce requires at least one operand")
	}
	if len(n.operands) == 1 {
		return n.operands[0].Eval(input)
	}
	nRows := input.NumRows()
	series := make([]Series, len(n.operands))
	for i, op := range n.operands {
		s, err := op.Eval(input)
		if err != nil {
			return Series{}, fmt.Errorf("Coalesce operand %d: %w", i, err)
		}
		if s.Len() != nRows {
			return Series{}, fmt.Errorf("Coalesce operand %d length %d != input rows %d",
				i, s.Len(), nRows)
		}
		series[i] = s
	}
	// Type check — all operands must share type.
	outType := series[0].DataType()
	for i := 1; i < len(series); i++ {
		if !arrow.TypeEqual(series[i].DataType(), outType) {
			return Series{}, fmt.Errorf(
				"%w: Coalesce operand 0/%d type mismatch (%s vs %s)",
				ErrExprTypeMismatch, i, outType, series[i].DataType())
		}
	}

	pool := memory.DefaultAllocator
	b, err := builderForType(pool, outType)
	if err != nil {
		return Series{}, fmt.Errorf("Coalesce: %w", err)
	}
	defer b.Release()

	// Row-by-row: pick the first non-null operand, copy its value.
	// All-null rows emit null.
	for row := range nRows {
		picked := -1
		for i, s := range series {
			null, err := isNullAtSeries(s, row)
			if err != nil {
				return Series{}, fmt.Errorf("Coalesce row %d op %d: %w", row, i, err)
			}
			if !null {
				picked = i
				break
			}
		}
		if picked < 0 {
			b.AppendNull()
			continue
		}
		if err := copyRowValue(b, series[picked], row); err != nil {
			return Series{}, fmt.Errorf("Coalesce row %d op %d: %w", row, picked, err)
		}
	}
	return buildSeries("coalesce", outType, b.NewArray()), nil
}

func (n *coalesceNode) Type(schema *arrow.Schema) (arrow.DataType, error) {
	if len(n.operands) == 0 {
		return nil, fmt.Errorf("gobi: Coalesce requires at least one operand")
	}
	first, err := n.operands[0].Type(schema)
	if err != nil {
		return nil, err
	}
	for i := 1; i < len(n.operands); i++ {
		t, err := n.operands[i].Type(schema)
		if err != nil {
			return nil, err
		}
		if first != nil && t != nil && !arrow.TypeEqual(first, t) {
			return nil, fmt.Errorf(
				"%w: Coalesce operand 0/%d type mismatch (%s vs %s)",
				ErrExprTypeMismatch, i, first, t)
		}
	}
	return first, nil
}

func (n *coalesceNode) Children() []Expr {
	out := make([]Expr, len(n.operands))
	for i, op := range n.operands {
		out[i] = Expr{node: op}
	}
	return out
}

func (n *coalesceNode) String() string {
	parts := make([]string, len(n.operands))
	for i, op := range n.operands {
		if op == nil {
			parts[i] = "<nil>"
			continue
		}
		parts[i] = op.String()
	}
	// Slim formatter that avoids strings.Join to stay inside expr_null.go's
	// import set (fmt only).
	out := "coalesce("
	for i, p := range parts {
		if i > 0 {
			out += ", "
		}
		out += p
	}
	return out + ")"
}

// copyRowValue copies src[row] to b. Handles primitive types via
// readScalarAt + appendCustomValue, and List<T> via a per-element copy
// that walks the offsets and uses appendArrayValueAt for primitive
// elements. Returns an error for unsupported types (Struct nesting).
func copyRowValue(b array.Builder, src Series, row int) error {
	dt := src.DataType()
	if dt.ID() != arrow.LIST {
		v, err := readScalarAt(src, row)
		if err != nil {
			return err
		}
		return appendCustomValue(b, v)
	}
	// List<T> path — walk to the chunk holding row, copy elements.
	lb, ok := b.(*array.ListBuilder)
	if !ok {
		return fmt.Errorf("copyRowValue: dest builder is %T, want *array.ListBuilder for List type", b)
	}
	offset := 0
	for _, chunk := range src.col.Data().Chunks() {
		if row < offset+chunk.Len() {
			local := row - offset
			la, ok := chunk.(*array.List)
			if !ok {
				return fmt.Errorf("copyRowValue: list chunk not *array.List (%T)", chunk)
			}
			if la.IsNull(local) {
				lb.AppendNull()
				return nil
			}
			lb.Append(true)
			start, end := la.ValueOffsets(local)
			values := la.ListValues()
			inner := lb.ValueBuilder()
			for j := int(start); j < int(end); j++ {
				if values.IsNull(j) {
					inner.AppendNull()
					continue
				}
				if err := appendArrayValueAt(inner, values, j); err != nil {
					return fmt.Errorf("copyRowValue elem %d: %w", j, err)
				}
			}
			return nil
		}
		offset += chunk.Len()
	}
	return fmt.Errorf("copyRowValue: row %d out of range", row)
}
