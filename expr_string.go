package gobi

import (
	"fmt"
	"regexp"

	"github.com/apache/arrow-go/v18/arrow"
)

// String-op Expr methods — thin wrappers that delegate to the
// Series-level driver so composition into Filter / WithColumn /
// Select chains works uniformly. Every method's semantics match
// the corresponding Series.StrX method (see series_string.go).
//
// Example:
//
//	// Filter to rows where name contains "Angeles", case-insensitive.
//	lf.Filter(gobi.Col("name").StrLower().StrContains("angeles")).Collect()

// strOp identifies which string operation a strOpNode dispatches
// to. Typed uint8 (matches binOpKind / geometry.Predicate style)
// so the compiler catches typos and the switch statement's
// exhaustiveness is checkable at a glance.
type strOp uint8

const (
	strOpLower strOp = iota
	strOpUpper
	strOpTrim
	strOpTrimLeft
	strOpTrimRight
	strOpLen
	strOpContains
	strOpStartsWith
	strOpEndsWith
	strOpReplace
	strOpSlice
	strOpConcat
	strOpRegexMatch
	strOpRegexReplace
)

// String returns the snake-case name of the op. Used by
// Explain() and error messages; kept in one place so future
// renames stay consistent across the whole layer.
func (o strOp) String() string {
	switch o {
	case strOpLower:
		return "lower"
	case strOpUpper:
		return "upper"
	case strOpTrim:
		return "trim"
	case strOpTrimLeft:
		return "trim_left"
	case strOpTrimRight:
		return "trim_right"
	case strOpLen:
		return "len"
	case strOpContains:
		return "contains"
	case strOpStartsWith:
		return "starts_with"
	case strOpEndsWith:
		return "ends_with"
	case strOpReplace:
		return "replace"
	case strOpSlice:
		return "slice"
	case strOpConcat:
		return "concat"
	case strOpRegexMatch:
		return "regex_match"
	case strOpRegexReplace:
		return "regex_replace"
	}
	return fmt.Sprintf("strOp(%d)", o)
}

// returnsBool reports whether this op emits a Boolean Series
// (predicates: Contains / StartsWith / EndsWith / RegexMatch).
// Called by strOpNode.Type to route the output arrow type.
func (o strOp) returnsBool() bool {
	switch o {
	case strOpContains, strOpStartsWith, strOpEndsWith, strOpRegexMatch:
		return true
	}
	return false
}

// StrLower returns e.StrLower() as an Expr.
func (e Expr) StrLower() Expr { return strUnaryExpr(e.node, strOpLower) }

// StrUpper returns e.StrUpper() as an Expr.
func (e Expr) StrUpper() Expr { return strUnaryExpr(e.node, strOpUpper) }

// StrTrim returns e.StrTrim() as an Expr (strip whitespace both ends).
func (e Expr) StrTrim() Expr { return strUnaryExpr(e.node, strOpTrim) }

// StrTrimLeft returns e with each value's leading `cutset` codepoints removed.
func (e Expr) StrTrimLeft(cutset string) Expr {
	return Expr{node: &strOpNode{op: strOpTrimLeft, inner: e.node, args: []string{cutset}}}
}

// StrTrimRight returns e with each value's trailing `cutset` codepoints removed.
func (e Expr) StrTrimRight(cutset string) Expr {
	return Expr{node: &strOpNode{op: strOpTrimRight, inner: e.node, args: []string{cutset}}}
}

// StrLen returns an Int64 Expr with each value's Unicode-codepoint count.
func (e Expr) StrLen() Expr {
	return Expr{node: &strOpNode{op: strOpLen, inner: e.node}}
}

// StrContains returns a Boolean Expr: true when the row's value
// contains `substr`.
func (e Expr) StrContains(substr string) Expr {
	return Expr{node: &strOpNode{op: strOpContains, inner: e.node, args: []string{substr}}}
}

// StrStartsWith returns a Boolean Expr: true when the row's value
// begins with `prefix`.
func (e Expr) StrStartsWith(prefix string) Expr {
	return Expr{node: &strOpNode{op: strOpStartsWith, inner: e.node, args: []string{prefix}}}
}

// StrEndsWith returns a Boolean Expr: true when the row's value
// ends with `suffix`.
func (e Expr) StrEndsWith(suffix string) Expr {
	return Expr{node: &strOpNode{op: strOpEndsWith, inner: e.node, args: []string{suffix}}}
}

// StrReplace returns e with every literal occurrence of `find`
// replaced by `replacement`.
func (e Expr) StrReplace(find, replacement string) Expr {
	return Expr{node: &strOpNode{op: strOpReplace, inner: e.node, args: []string{find, replacement}}}
}

// StrSlice returns each value sliced by codepoint index [start, end).
// See Series.StrSlice for the negative-index / end-zero convention.
func (e Expr) StrSlice(start, end int) Expr {
	return Expr{node: &strOpNode{op: strOpSlice, inner: e.node, ints: []int{start, end}}}
}

// StrConcat returns each value followed by `suffix`.
func (e Expr) StrConcat(suffix string) Expr {
	return Expr{node: &strOpNode{op: strOpConcat, inner: e.node, args: []string{suffix}}}
}

// StrRegexMatch returns a Boolean Expr: true when the row's value
// matches `pattern` (RE2 semantics, MatchString shape).
//
// Pattern is compiled ONCE at Expr-build time and the compiled
// *regexp.Regexp is cached on the executor node — a Filter chain
// evaluated across N batches pays the compile cost once, not N
// times. Invalid patterns don't panic; the compile error is stored
// on the node and surfaced at Eval (same pattern as Lit's
// unsupported-type handling).
func (e Expr) StrRegexMatch(pattern string) Expr {
	re, err := regexp.Compile(pattern)
	return Expr{node: &strOpNode{
		op:    strOpRegexMatch,
		inner: e.node,
		args:  []string{pattern}, // kept for String() display
		re:    re,
		reErr: err,
	}}
}

// StrRegexReplace returns e with every regex match of `pattern`
// replaced by `replacement` (ReplaceAllString semantics). Same
// compile-once caching as StrRegexMatch.
func (e Expr) StrRegexReplace(pattern, replacement string) Expr {
	re, err := regexp.Compile(pattern)
	return Expr{node: &strOpNode{
		op:    strOpRegexReplace,
		inner: e.node,
		args:  []string{pattern, replacement},
		re:    re,
		reErr: err,
	}}
}

// strUnaryExpr is the shared constructor for the arg-less string
// unary ops (lower / upper / trim). Individual named methods make
// the API-level docstrings explicit while sharing one node type.
func strUnaryExpr(inner ExprNode, op strOp) Expr {
	return Expr{node: &strOpNode{op: op, inner: inner}}
}

// strOpNode is the executor node for all Expr.StrX methods. `op`
// dispatches to the matching Series driver at Eval time; `args`
// / `ints` carry op-specific scalar parameters (regex pattern,
// slice indices, etc.). Kept as one node type rather than 14 to
// avoid boilerplate — the ExprNode interface (Eval / Type /
// Children / String) is identical across ops modulo the dispatch
// switch.
//
// `re` / `reErr` cache the compiled regex for the RegexMatch /
// RegexReplace ops. Compiled once at Expr-build time; a batched
// streaming Filter shares the same *regexp.Regexp across every
// batch's Eval instead of recompiling. `reErr` carries any
// compile error and surfaces at Eval — matches the Lit node's
// deferred-error style.
type strOpNode struct {
	op    strOp
	inner ExprNode
	args  []string
	ints  []int
	re    *regexp.Regexp
	reErr error
}

func (n *strOpNode) Eval(input *Frame) (Series, error) {
	if n.inner == nil {
		return Series{}, fmt.Errorf("gobi: str.%s on nil inner expression", n.op)
	}
	// Surface any regex-compile error from Expr-build time (before
	// touching the input Frame — bad pattern is a static bug).
	if n.reErr != nil {
		return Series{}, fmt.Errorf("gobi: str.%s: %w", n.op, n.reErr)
	}
	s, err := n.inner.Eval(input)
	if err != nil {
		return Series{}, err
	}
	switch n.op {
	case strOpLower:
		return s.StrLower()
	case strOpUpper:
		return s.StrUpper()
	case strOpTrim:
		return s.StrTrim()
	case strOpTrimLeft:
		return s.StrTrimLeft(n.args[0])
	case strOpTrimRight:
		return s.StrTrimRight(n.args[0])
	case strOpLen:
		return s.StrLen()
	case strOpContains:
		return s.StrContains(n.args[0])
	case strOpStartsWith:
		return s.StrStartsWith(n.args[0])
	case strOpEndsWith:
		return s.StrEndsWith(n.args[0])
	case strOpReplace:
		return s.StrReplace(n.args[0], n.args[1])
	case strOpSlice:
		return s.StrSlice(n.ints[0], n.ints[1])
	case strOpConcat:
		return s.StrConcat(n.args[0])
	case strOpRegexMatch:
		// Use the precompiled regex cached on the node, not
		// s.StrRegexMatch's re-compile-per-call path.
		return s.strRegexMatchCompiled(n.re)
	case strOpRegexReplace:
		return s.strRegexReplaceCompiled(n.re, n.args[1])
	}
	return Series{}, fmt.Errorf("gobi: strOpNode: unhandled op %s", n.op)
}

func (n *strOpNode) Type(schema *arrow.Schema) (arrow.DataType, error) {
	if n.inner == nil {
		return nil, fmt.Errorf("gobi: str.%s on nil inner expression", n.op)
	}
	if n.reErr != nil {
		return nil, fmt.Errorf("gobi: str.%s: %w", n.op, n.reErr)
	}
	innerType, err := n.inner.Type(schema)
	if err != nil {
		return nil, err
	}
	if innerType.ID() != arrow.STRING {
		return nil, fmt.Errorf("%w: str.%s requires a String column, got %s",
			ErrExprTypeMismatch, n.op, innerType)
	}
	switch {
	case n.op == strOpLen:
		return arrow.PrimitiveTypes.Int64, nil
	case n.op.returnsBool():
		return arrow.FixedWidthTypes.Boolean, nil
	default:
		return arrow.BinaryTypes.String, nil
	}
}

func (n *strOpNode) Children() []Expr { return []Expr{{node: n.inner}} }

func (n *strOpNode) String() string {
	switch len(n.args) {
	case 0:
		if len(n.ints) == 2 {
			return fmt.Sprintf("%s.str_%s(%d, %d)", n.inner, n.op, n.ints[0], n.ints[1])
		}
		return fmt.Sprintf("%s.str_%s()", n.inner, n.op)
	case 1:
		return fmt.Sprintf("%s.str_%s(%q)", n.inner, n.op, n.args[0])
	case 2:
		return fmt.Sprintf("%s.str_%s(%q, %q)", n.inner, n.op, n.args[0], n.args[1])
	}
	return fmt.Sprintf("%s.str_%s(...)", n.inner, n.op)
}
