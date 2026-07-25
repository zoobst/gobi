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
