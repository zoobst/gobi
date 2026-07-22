package gobi

import (
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
)

// TestExprToSQL_Basic exercises the simple shapes: a column
// reference, a literal, and a couple of binary ops. The emitted SQL
// is deliberately fully-parenthesized so operator precedence never
// depends on the target driver's rules.
func TestExprToSQL_Basic(t *testing.T) {
	cases := []struct {
		name     string
		expr     Expr
		wantSQL  string
		wantArgs []any
	}{
		{
			name:     "col > int",
			expr:     Col("price").Gt(Lit(int64(100))),
			wantSQL:  `("price" > ?)`,
			wantArgs: []any{int64(100)},
		},
		{
			name:     "col = string",
			expr:     Col("region").Eq(Lit("US")),
			wantSQL:  `("region" = ?)`,
			wantArgs: []any{"US"},
		},
		{
			name:     "col * literal > threshold",
			expr:     Col("price").Mul(Lit(1.08)).Gt(Lit(100.0)),
			wantSQL:  `(("price" * ?) > ?)`,
			wantArgs: []any{1.08, 100.0},
		},
		{
			name:     "AND of two comparisons",
			expr:     Col("price").Gt(Lit(int64(10))).And(Col("region").Eq(Lit("US"))),
			wantSQL:  `(("price" > ?) AND ("region" = ?))`,
			wantArgs: []any{int64(10), "US"},
		},
		{
			name:     "NOT wrapper",
			expr:     Col("active").Eq(Lit(true)).Not(),
			wantSQL:  `NOT (("active" = ?))`,
			wantArgs: []any{true},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sql, args, ok := ExprToSQL(tc.expr)
			if !ok {
				t.Fatal("ExprToSQL returned ok=false")
			}
			if sql != tc.wantSQL {
				t.Errorf("sql = %q, want %q", sql, tc.wantSQL)
			}
			if len(args) != len(tc.wantArgs) {
				t.Fatalf("args len = %d, want %d (%v)", len(args), len(tc.wantArgs), args)
			}
			for i, v := range args {
				if v != tc.wantArgs[i] {
					t.Errorf("args[%d] = %v (%T), want %v (%T)",
						i, v, v, tc.wantArgs[i], tc.wantArgs[i])
				}
			}
		})
	}
}

// TestExprToSQL_NullRewrite verifies the IS NULL / IS NOT NULL
// rewrite. A naive `x = NULL` translation would silently drop rows
// whose x IS actually NULL — SQL's null semantics say `x = NULL` is
// itself NULL, not TRUE.
//
// `Lit(nil)` isn't currently a valid gobi construction (newLiteralNode
// errors on nil), so this test builds nil-valued literalNodes
// directly via package-internal access. The rewrite path is still
// worth having because a future Lit-nil enhancement, or any custom
// ExprNode that produces a null literal, will route through it.
func TestExprToSQL_NullRewrite(t *testing.T) {
	nilLit := Expr{node: &literalNode{value: nil}}
	cases := []struct {
		name    string
		expr    Expr
		wantSQL string
	}{
		{"col = nil → IS NULL", Col("x").Eq(nilLit), `("x" IS NULL)`},
		{"col != nil → IS NOT NULL", Col("x").Ne(nilLit), `("x" IS NOT NULL)`},
		{"nil = col → IS NULL (left-nil form)", nilLit.Eq(Col("x")), `("x" IS NULL)`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sql, args, ok := ExprToSQL(tc.expr)
			if !ok {
				t.Fatal("ok=false")
			}
			if sql != tc.wantSQL {
				t.Errorf("sql = %q, want %q", sql, tc.wantSQL)
			}
			if len(args) != 0 {
				t.Errorf("null-rewrite should have no bind args, got %v", args)
			}
		})
	}
}

// TestExprToSQL_QuoteIdent covers identifier quoting: names with
// embedded double-quotes must double them per ANSI SQL. Also
// verifies a plain name comes back with straight double-quotes
// (not backticks — MySQL isn't a target).
func TestExprToSQL_QuoteIdent(t *testing.T) {
	// Plain identifier.
	sql, _, _ := ExprToSQL(Col("region").Eq(Lit("US")))
	if sql != `("region" = ?)` {
		t.Errorf("plain: %q", sql)
	}
	// Weird identifier with an embedded quote.
	sql, _, _ = ExprToSQL(Col(`weird"col`).Eq(Lit("x")))
	if sql != `("weird""col" = ?)` {
		t.Errorf("quoted: %q", sql)
	}
}

// TestExprToSQL_Rejects_UntranslatableNodes verifies that unsupported
// node shapes come back with ok=false so callers know to fall back
// to executor-side evaluation.
func TestExprToSQL_Rejects_UntranslatableNodes(t *testing.T) {
	// nil-valued Expr.
	if _, _, ok := ExprToSQL(Expr{}); ok {
		t.Error("empty Expr should return ok=false")
	}
	// Custom node — arbitrary user extension, can't be translated.
	if _, _, ok := ExprToSQL(Custom(fakeCustomNode{})); ok {
		t.Error("Custom node should return ok=false")
	}
}

// fakeCustomNode is a minimal ExprNode used to prove Custom paths
// route to the unsupported branch. It's not registered anywhere,
// just needs to satisfy the interface.
type fakeCustomNode struct{}

func (fakeCustomNode) Eval(*Frame) (Series, error)                  { return Series{}, nil }
func (fakeCustomNode) Type(*arrow.Schema) (arrow.DataType, error)   { return arrow.FixedWidthTypes.Boolean, nil }
func (fakeCustomNode) Children() []Expr                             { return nil }
func (fakeCustomNode) String() string                               { return "custom" }
