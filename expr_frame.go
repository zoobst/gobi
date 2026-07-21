package gobi

import (
	"fmt"

	"github.com/apache/arrow-go/v18/arrow"
)

// FilterExpr returns a new Frame containing the rows for which e
// evaluates to true. e must produce a Boolean Series; null entries in
// the mask are treated as false (SQL-style — matches Frame.Filter).
//
// Example:
//
//	// Keep rows where (price * 1.08) > 100.
//	e := gobi.Col("price").Mul(gobi.Lit(1.08)).Gt(gobi.Lit(100.0))
//	out, err := df.FilterExpr(e)
func (f *Frame) FilterExpr(e Expr) (*Frame, error) {
	if e.node == nil {
		return nil, fmt.Errorf("%w: nil expression", ErrExprTypeMismatch)
	}
	mask, err := e.node.Eval(f)
	if err != nil {
		return nil, err
	}
	if mask.DataType() == nil || mask.DataType().ID() != arrow.BOOL {
		return nil, fmt.Errorf("%w: filter expression must produce Boolean, got %s",
			ErrExprTypeMismatch, mask.DataType())
	}
	return f.Filter(mask)
}

// WithColumnExpr returns a new Frame with e evaluated and attached
// under name. If a column named name already exists, it is replaced in
// place; otherwise it is appended.
//
// Example:
//
//	// Add a computed column: usd_price = eur_price * 1.08
//	out, err := df.WithColumnExpr("usd_price",
//	    gobi.Col("eur_price").Mul(gobi.Lit(1.08)),
//	)
func (f *Frame) WithColumnExpr(name string, e Expr) (*Frame, error) {
	if e.node == nil {
		return nil, fmt.Errorf("%w: nil expression", ErrExprTypeMismatch)
	}
	s, err := e.node.Eval(f)
	if err != nil {
		return nil, err
	}
	// The mask/derived column produced by Eval carries whatever name
	// the innermost node chose (usually empty or the column it read).
	// WithColumn will rename via NewSeries under the caller's name.
	if s.Len() != f.NumRows() && f.NumRows() > 0 {
		return nil, fmt.Errorf("%w: expression produced %d rows, frame has %d",
			ErrColumnLenMismatch, s.Len(), f.NumRows())
	}
	// Derived columns are conservatively nullable — future ops like
	// divide-by-zero handling produce nulls even from fully-populated
	// inputs, so we don't want WithColumn refusing them on a
	// Nullable=false schema mismatch.
	s = markNullable(s)
	return f.WithColumn(name, s)
}

// markNullable returns s with its arrow.Field.Nullable set to true.
// Derived columns are conservatively nullable so downstream ops don't
// choke on a null-mask mismatch — arithmetic on a fully-populated
// input might still produce nulls (e.g. divide-by-zero handling in
// future casts).
func markNullable(s Series) Series {
	field := s.field
	if field.Nullable {
		return s
	}
	field.Nullable = true
	col := arrow.NewColumn(field, s.col.Data())
	return NewSeries(col)
}
