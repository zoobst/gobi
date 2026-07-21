package gobi

import (
	"fmt"

	"github.com/apache/arrow-go/v18/arrow"
)

// Expr is an expression tree — a value that describes a computation
// without performing it. Build expressions with the package-level
// constructors (Col, Lit, Custom) and combine them with the fluent
// methods on Expr (Add, Mul, Gt, And, etc.).
//
// Expressions evaluate lazily: nothing runs until Frame.FilterExpr or
// Frame.WithColumnExpr consumes them. Because the tree is data, it can
// be inspected, printed, and (in a future release) rewritten by an
// optimizer before evaluation.
//
// Example:
//
//	// (price * 1.08) > 100
//	e := gobi.Col("price").Mul(gobi.Lit(1.08)).Gt(gobi.Lit(100.0))
//	filtered, err := df.FilterExpr(e)
type Expr struct {
	node ExprNode
}

// Node returns the underlying ExprNode. Rarely needed unless you're
// implementing your own tree walker or optimizer.
func (e Expr) Node() ExprNode { return e.node }

// String returns a human-readable representation of the expression.
func (e Expr) String() string {
	if e.node == nil {
		return "<nil-expr>"
	}
	return e.node.String()
}

// ExprNode is the interface every expression node implements. Users
// extending gobi with custom expression types (H3 encoding, hashes,
// ML inference, etc.) implement this and wrap the result with Custom.
type ExprNode interface {
	// Eval evaluates the expression against input, returning a Series
	// whose values are the result. The returned Series should have
	// length equal to input.NumRows(). Scalar broadcasts are handled
	// by the caller for built-in nodes; custom nodes may either
	// broadcast internally or emit a length-1 Series.
	Eval(input *Frame) (Series, error)

	// Type returns the arrow data type of the result given the input
	// schema. Called by type-inference passes before evaluation.
	Type(schema *arrow.Schema) (arrow.DataType, error)

	// Children returns the sub-expressions of this node in
	// deterministic order (empty for leaves). Used by tree walkers.
	Children() []Expr

	// String returns a human-readable representation of the node.
	String() string
}

// Custom wraps a user-defined ExprNode into an Expr. This is the entry
// point for extending gobi with your own expression types.
func Custom(node ExprNode) Expr { return Expr{node: node} }

// -----------------------------------------------------------------------------
// Constructors
// -----------------------------------------------------------------------------

// Col returns an expression that references the column named name in
// the input Frame.
func Col(name string) Expr {
	return Expr{node: &colRefNode{name: name}}
}

// Lit returns an expression that evaluates to a constant value. The
// value's arrow type is inferred from its Go type; the following are
// supported:
//
//	bool                → Boolean
//	int, int32, int64   → Int64
//	float32, float64    → Float64
//	string              → String
//
// Other Go types return an Expr whose Eval reports a type-inference
// error.
func Lit(v any) Expr {
	return Expr{node: newLiteralNode(v)}
}

// -----------------------------------------------------------------------------
// Fluent combinators
// -----------------------------------------------------------------------------

// Add returns e + o.
func (e Expr) Add(o Expr) Expr { return binExpr(bopAdd, e, o) }

// Sub returns e - o.
func (e Expr) Sub(o Expr) Expr { return binExpr(bopSub, e, o) }

// Mul returns e * o.
func (e Expr) Mul(o Expr) Expr { return binExpr(bopMul, e, o) }

// Div returns e / o.
func (e Expr) Div(o Expr) Expr { return binExpr(bopDiv, e, o) }

// Eq returns e == o (Boolean result).
func (e Expr) Eq(o Expr) Expr { return binExpr(bopEq, e, o) }

// Ne returns e != o (Boolean result).
func (e Expr) Ne(o Expr) Expr { return binExpr(bopNe, e, o) }

// Lt returns e < o (Boolean result).
func (e Expr) Lt(o Expr) Expr { return binExpr(bopLt, e, o) }

// Le returns e <= o (Boolean result).
func (e Expr) Le(o Expr) Expr { return binExpr(bopLe, e, o) }

// Gt returns e > o (Boolean result).
func (e Expr) Gt(o Expr) Expr { return binExpr(bopGt, e, o) }

// Ge returns e >= o (Boolean result).
func (e Expr) Ge(o Expr) Expr { return binExpr(bopGe, e, o) }

// And returns e AND o. Both e and o must produce Boolean values.
func (e Expr) And(o Expr) Expr { return binExpr(bopAnd, e, o) }

// Or returns e OR o. Both e and o must produce Boolean values.
func (e Expr) Or(o Expr) Expr { return binExpr(bopOr, e, o) }

// Not returns the logical negation of e. e must produce Boolean values.
func (e Expr) Not() Expr {
	return Expr{node: &notNode{inner: e.node}}
}

// Alias renames the output column produced by e. Only used by
// Frame.WithColumnExpr and future SelectExpr — has no effect inside
// FilterExpr.
func (e Expr) Alias(name string) Expr {
	return Expr{node: &aliasNode{inner: e.node, name: name}}
}

// -----------------------------------------------------------------------------
// binOp kind (unexported)
// -----------------------------------------------------------------------------

type binOpKind uint8

const (
	bopAdd binOpKind = iota
	bopSub
	bopMul
	bopDiv
	bopEq
	bopNe
	bopLt
	bopLe
	bopGt
	bopGe
	bopAnd
	bopOr
)

func (k binOpKind) String() string {
	switch k {
	case bopAdd:
		return "+"
	case bopSub:
		return "-"
	case bopMul:
		return "*"
	case bopDiv:
		return "/"
	case bopEq:
		return "=="
	case bopNe:
		return "!="
	case bopLt:
		return "<"
	case bopLe:
		return "<="
	case bopGt:
		return ">"
	case bopGe:
		return ">="
	case bopAnd:
		return "AND"
	case bopOr:
		return "OR"
	}
	return fmt.Sprintf("op(%d)", k)
}

func (k binOpKind) isArithmetic() bool { return k <= bopDiv }
func (k binOpKind) isComparison() bool { return k >= bopEq && k <= bopGe }
func (k binOpKind) isLogical() bool    { return k == bopAnd || k == bopOr }

// binExpr is the shared constructor for BinaryOp-shaped Exprs.
func binExpr(op binOpKind, l, r Expr) Expr {
	return Expr{node: &binOpNode{op: op, left: l.node, right: r.node}}
}
