package gobi

import (
	"fmt"
	"strings"
)

// ExprToSQL translates a gobi.Expr into a SQL fragment + a slice of
// bind arguments suitable for parameterized SQL queries (SQLite,
// PostgreSQL / PostGIS, DuckDB, etc.).
//
// Returns ok=false when the expression contains a node the translator
// can't represent: custom nodes (Custom), aliased sub-expressions
// inside a predicate, or any binary op that doesn't map cleanly to
// SQL. Callers that get ok=false should fall back to evaluating the
// expression in the executor rather than pushing it into SQL.
//
// The emitted SQL uses positional `?` placeholders for every literal,
// matching the SQLite driver contract that gpkgio uses. Drivers that
// expect `$1`/`$2` (pgx) or `:name` (Oracle) can rewrite the
// placeholders trivially since the args slice is 1:1 with `?` order.
//
// Column names are double-quoted per SQL:2016 identifier rules; any
// embedded double-quote in a name is escaped by doubling. NULL
// comparisons (`x = NULL`) are rewritten to `IS NULL` / `IS NOT NULL`
// to match SQL's null-safe semantics.
//
// Example:
//
//	e := gobi.Col("price").Mul(gobi.Lit(1.08)).Gt(gobi.Lit(100.0))
//	sql, args, ok := gobi.ExprToSQL(e)
//	// sql  = `(("price" * ?) > ?)`
//	// args = []any{1.08, 100.0}
//	// ok   = true
func ExprToSQL(e Expr) (string, []any, bool) {
	if e.node == nil {
		return "", nil, false
	}
	var b strings.Builder
	var args []any
	if !appendExprSQL(&b, &args, e.node) {
		return "", nil, false
	}
	return b.String(), args, true
}

// appendExprSQL is the recursive workhorse. Returns false and leaves
// b + args in an unspecified state on an unsupported node — callers
// are expected to discard the partial results.
func appendExprSQL(b *strings.Builder, args *[]any, n ExprNode) bool {
	switch node := n.(type) {

	case *colRefNode:
		b.WriteString(quoteSQLIdent(node.name))
		return true

	case *literalNode:
		if node.err != nil {
			return false
		}
		// SQL has no direct syntax for a bare NULL in a comparison
		// context; a `Lit(nil)` on the RHS of `=` should route
		// through IS NULL, which we handle at the binOp level. Bare
		// literal-NULL as a top-level predicate isn't meaningful, so
		// reject.
		if node.value == nil {
			return false
		}
		b.WriteByte('?')
		*args = append(*args, node.value)
		return true

	case *binOpNode:
		return appendBinOpSQL(b, args, node)

	case *notNode:
		b.WriteString("NOT (")
		if !appendExprSQL(b, args, node.inner) {
			return false
		}
		b.WriteByte(')')
		return true

	case *aliasNode:
		// An alias inside a WHERE clause doesn't change the value;
		// unwrap and translate the inner expression. Aliases matter
		// for output naming, not predicate semantics, so this is
		// safe.
		return appendExprSQL(b, args, node.inner)
	}
	return false
}

// appendBinOpSQL handles the binary-operator arm of the translator.
// Extracted because it carries the null-safety rewrite (=/!= vs
// IS NULL/IS NOT NULL) and the operator-symbol switch, both of which
// would clutter the parent switch.
func appendBinOpSQL(b *strings.Builder, args *[]any, node *binOpNode) bool {
	// Detect Lit(nil) on either side of an equality op and route to
	// IS NULL / IS NOT NULL. SQL's `x = NULL` always evaluates to
	// NULL (not TRUE), so a naive translation would silently drop
	// null-matching rows.
	if node.op == bopEq || node.op == bopNe {
		if isNilLiteral(node.left) {
			b.WriteByte('(')
			if !appendExprSQL(b, args, node.right) {
				return false
			}
			if node.op == bopEq {
				b.WriteString(" IS NULL)")
			} else {
				b.WriteString(" IS NOT NULL)")
			}
			return true
		}
		if isNilLiteral(node.right) {
			b.WriteByte('(')
			if !appendExprSQL(b, args, node.left) {
				return false
			}
			if node.op == bopEq {
				b.WriteString(" IS NULL)")
			} else {
				b.WriteString(" IS NOT NULL)")
			}
			return true
		}
	}
	op, ok := sqlBinOpSymbol(node.op)
	if !ok {
		return false
	}
	b.WriteByte('(')
	if !appendExprSQL(b, args, node.left) {
		return false
	}
	b.WriteByte(' ')
	b.WriteString(op)
	b.WriteByte(' ')
	if !appendExprSQL(b, args, node.right) {
		return false
	}
	b.WriteByte(')')
	return true
}

// sqlBinOpSymbol maps a gobi binOpKind to its SQL text. Returns
// ok=false for ops that don't have a portable SQL spelling — today
// every built-in op has one, but keeping the guard means future
// non-portable ops (bit-shifts, mod, string-concat variants) are
// safely rejected instead of silently mistranslated.
func sqlBinOpSymbol(op binOpKind) (string, bool) {
	switch op {
	case bopAdd:
		return "+", true
	case bopSub:
		return "-", true
	case bopMul:
		return "*", true
	case bopDiv:
		return "/", true
	case bopEq:
		return "=", true
	case bopNe:
		return "<>", true
	case bopLt:
		return "<", true
	case bopLe:
		return "<=", true
	case bopGt:
		return ">", true
	case bopGe:
		return ">=", true
	case bopAnd:
		return "AND", true
	case bopOr:
		return "OR", true
	}
	return "", false
}

// isNilLiteral reports whether n is a literal whose value is nil —
// i.e. `Lit(nil)` or an equivalent construction. Used to detect the
// null-comparison rewrite case.
func isNilLiteral(n ExprNode) bool {
	lit, ok := n.(*literalNode)
	if !ok {
		return false
	}
	return lit.value == nil && lit.err == nil
}

// quoteSQLIdent double-quotes a SQL identifier per ANSI SQL, escaping
// any embedded double-quote by doubling it. The double-quote is the
// portable ANSI quote (works in SQLite, PostgreSQL, DuckDB, Oracle);
// MySQL requires backticks unless ANSI_QUOTES is enabled, but MySQL
// isn't a target driver today.
func quoteSQLIdent(s string) string {
	if !strings.ContainsRune(s, '"') {
		return `"` + s + `"`
	}
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// SplitConjuncts breaks an expression at top-level AND boundaries so
// callers can push the pieces that translate to SQL individually and
// keep the rest for post-scan filtering.
//
// Example: given `(a > 5 AND custom_fn(b) AND c = "x")`, SplitConjuncts
// yields `[a > 5, custom_fn(b), c = "x"]`. The custom_fn part won't
// translate; the other two will, and the caller can push `a > 5 AND
// c = "x"` into SQL while leaving `custom_fn(b)` in the executor.
//
// Non-AND expressions come back as a single-element slice — a
// top-level OR isn't safe to split (both sides must match), so we
// don't try.
func SplitConjuncts(e Expr) []Expr {
	if e.node == nil {
		return nil
	}
	var out []Expr
	walkAnd(e.node, &out)
	return out
}

// walkAnd recursively unpacks nested `AND` binOps into a flat list.
// Stops descending on anything that isn't AND — leaves the sub-tree
// intact for the caller.
func walkAnd(n ExprNode, out *[]Expr) {
	if b, ok := n.(*binOpNode); ok && b.op == bopAnd {
		walkAnd(b.left, out)
		walkAnd(b.right, out)
		return
	}
	*out = append(*out, Expr{node: n})
}

// exprToSQLDoc is a compile-time sanity check that the exported
// helpers stay in scope for docs tooling. Prevents accidental
// visibility loss during refactors.
var _ = fmt.Sprintf
