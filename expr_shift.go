package gobi

import (
	"fmt"

	"github.com/apache/arrow-go/v18/arrow"
)

// Shift returns an expression that shifts the values of e by n
// positions. Positive n shifts forward (the first n rows become null,
// each output row i takes the source value from row i-n). Negative n
// shifts backward. Matches Series.Shift semantics.
//
// Composes naturally with WithColumn / Select for lag / lead
// computations:
//
//	// prior period's price
//	lf.WithColumn("prev_price", Col("price").Shift(1))
//
//	// running delta
//	lf.WithColumn("delta", Col("price").Sub(Col("price").Shift(1)))
//
// Per-group shifts via .Over(K) are on the roadmap; today Over requires
// a scalar aggregate as its immediate inner, so shift-then-partition
// isn't wired up (see CLAUDE.md's Track 3 note on ordered-partition
// windows).
func (e Expr) Shift(n int) Expr {
	return Expr{node: &shiftNode{inner: e.node, n: n}}
}

// shiftNode evaluates its inner expression against the input Frame and
// then applies Series.Shift(n) to the result. Type inference passes
// through — Shift preserves the source's arrow type.
type shiftNode struct {
	inner ExprNode
	n     int
}

func (s *shiftNode) Eval(input *Frame) (Series, error) {
	if s.inner == nil {
		return Series{}, fmt.Errorf("gobi: Shift on nil inner expression")
	}
	src, err := s.inner.Eval(input)
	if err != nil {
		return Series{}, err
	}
	return src.Shift(s.n)
}

func (s *shiftNode) Type(schema *arrow.Schema) (arrow.DataType, error) {
	if s.inner == nil {
		return nil, fmt.Errorf("gobi: Shift on nil inner expression")
	}
	return s.inner.Type(schema)
}

func (s *shiftNode) Children() []Expr { return []Expr{{node: s.inner}} }
func (s *shiftNode) String() string   { return fmt.Sprintf("%s.shift(%d)", s.inner, s.n) }
