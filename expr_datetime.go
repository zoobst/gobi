package gobi

import (
	"fmt"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
)

// Datetime-op Expr methods — thin wrappers over the Series-level
// datetime driver (see series_time.go + series_time_transforms.go)
// so composition into Filter / WithColumn / Select chains works
// uniformly. Every method's semantics match the corresponding
// Series.X method.
//
// Existing scalar convenience `Expr.UnixNano()` (see
// expr_timestamp.go) predates this file and stays as-is —
// canonical way to convert Timestamp → Int64 nanoseconds for
// arithmetic.
//
// Example:
//
//	// Rows in Q4 of 2026.
//	lf.Filter(
//	    Col("ts").Year().Eq(Lit(int64(2026))).
//	        And(Col("ts").Month().Ge(Lit(int64(10)))),
//	).Collect()

// dtOp identifies which datetime operation a dtOpNode dispatches
// to. Typed uint8 (matches binOpKind / strOp style) so the
// compiler catches typos and the dispatch switch stays
// exhaustive.
type dtOp uint8

const (
	dtOpYear dtOp = iota
	dtOpMonth
	dtOpDay
	dtOpHour
	dtOpMinute
	dtOpSecond
	dtOpNanosecond
	dtOpWeekday
	dtOpDayOfYear
	dtOpTruncate
	dtOpFormat
	dtOpAdd
	dtOpSub
)

// String returns the snake-case name of the op. Used by
// Explain() and error messages.
func (o dtOp) String() string {
	switch o {
	case dtOpYear:
		return "year"
	case dtOpMonth:
		return "month"
	case dtOpDay:
		return "day"
	case dtOpHour:
		return "hour"
	case dtOpMinute:
		return "minute"
	case dtOpSecond:
		return "second"
	case dtOpNanosecond:
		return "nanosecond"
	case dtOpWeekday:
		return "weekday"
	case dtOpDayOfYear:
		return "day_of_year"
	case dtOpTruncate:
		return "trunc"
	case dtOpFormat:
		return "format"
	case dtOpAdd:
		return "add"
	case dtOpSub:
		return "sub"
	}
	return fmt.Sprintf("dtOp(%d)", o)
}

// Year returns the calendar year of each row's timestamp as Int64.
func (e Expr) Year() Expr { return dtExtractExpr(e.node, dtOpYear) }

// Month returns the calendar month (1..12) as Int64.
func (e Expr) Month() Expr { return dtExtractExpr(e.node, dtOpMonth) }

// Day returns the day-of-month (1..31) as Int64.
func (e Expr) Day() Expr { return dtExtractExpr(e.node, dtOpDay) }

// Hour returns the hour-of-day (0..23) as Int64.
func (e Expr) Hour() Expr { return dtExtractExpr(e.node, dtOpHour) }

// Minute returns the minute-of-hour (0..59) as Int64.
func (e Expr) Minute() Expr { return dtExtractExpr(e.node, dtOpMinute) }

// Second returns the second-of-minute (0..59) as Int64.
func (e Expr) Second() Expr { return dtExtractExpr(e.node, dtOpSecond) }

// Nanosecond returns the sub-second nanosecond component
// (0..999_999_999) as Int64.
func (e Expr) Nanosecond() Expr { return dtExtractExpr(e.node, dtOpNanosecond) }

// Weekday returns the day-of-week as Int64 (0=Sunday..6=Saturday).
func (e Expr) Weekday() Expr { return dtExtractExpr(e.node, dtOpWeekday) }

// DayOfYear returns the day-of-year (1..366) as Int64.
func (e Expr) DayOfYear() Expr { return dtExtractExpr(e.node, dtOpDayOfYear) }

// DateTruncate returns a Timestamp Expr with each value truncated
// down to the start of `unit`. Supported: "year", "month", "day",
// "hour", "minute", "second". Truncation is calendar-aware for
// year/month/day and wall-clock aligned for hour/minute/second.
func (e Expr) DateTruncate(unit string) Expr {
	return Expr{node: &dtOpNode{op: dtOpTruncate, inner: e.node, arg: unit}}
}

// DateFormat returns a String Expr formatting each value with the
// given Go time layout (RFC3339 default when layout is empty).
func (e Expr) DateFormat(layout string) Expr {
	return Expr{node: &dtOpNode{op: dtOpFormat, inner: e.node, arg: layout}}
}

// AddDuration returns a Timestamp Expr with `d` added to each
// value. Negative durations subtract.
func (e Expr) AddDuration(d time.Duration) Expr {
	return Expr{node: &dtOpNode{op: dtOpAdd, inner: e.node, dur: d}}
}

// SubDuration returns a Timestamp Expr with `d` subtracted from
// each value. Equivalent to AddDuration(-d).
func (e Expr) SubDuration(d time.Duration) Expr {
	return Expr{node: &dtOpNode{op: dtOpSub, inner: e.node, dur: d}}
}

// dtExtractExpr is the shared constructor for the arg-less
// Timestamp → Int64 extractors (Year / Month / ... / DayOfYear).
func dtExtractExpr(inner ExprNode, op dtOp) Expr {
	return Expr{node: &dtOpNode{op: op, inner: inner}}
}

// dtOpNode is the executor node for all Expr datetime methods.
// `op` dispatches to the matching Series method at Eval time; the
// `arg` (unit name / layout string) or `dur` (time.Duration) carry
// op-specific parameters. One node type keeps the file compact
// vs. eleven near-identical node structs.
type dtOpNode struct {
	op    dtOp
	inner ExprNode
	arg   string        // unit name for trunc, layout for format
	dur   time.Duration // duration for add / sub
}

func (n *dtOpNode) Eval(input *Frame) (Series, error) {
	if n.inner == nil {
		return Series{}, fmt.Errorf("gobi: dt.%s on nil inner expression", n.op)
	}
	s, err := n.inner.Eval(input)
	if err != nil {
		return Series{}, err
	}
	switch n.op {
	case dtOpYear:
		return s.Year()
	case dtOpMonth:
		return s.Month()
	case dtOpDay:
		return s.Day()
	case dtOpHour:
		return s.Hour()
	case dtOpMinute:
		return s.Minute()
	case dtOpSecond:
		return s.Second()
	case dtOpNanosecond:
		return s.Nanosecond()
	case dtOpWeekday:
		return s.Weekday()
	case dtOpDayOfYear:
		return s.DayOfYear()
	case dtOpTruncate:
		return s.DateTruncate(n.arg)
	case dtOpFormat:
		return s.DateFormat(n.arg)
	case dtOpAdd:
		return s.AddDuration(n.dur)
	case dtOpSub:
		return s.SubDuration(n.dur)
	}
	return Series{}, fmt.Errorf("gobi: dtOpNode: unhandled op %s", n.op)
}

func (n *dtOpNode) Type(schema *arrow.Schema) (arrow.DataType, error) {
	if n.inner == nil {
		return nil, fmt.Errorf("gobi: dt.%s on nil inner expression", n.op)
	}
	innerType, err := n.inner.Type(schema)
	if err != nil {
		return nil, err
	}
	if _, ok := innerType.(*arrow.TimestampType); !ok {
		return nil, fmt.Errorf("%w: dt.%s requires a Timestamp column, got %s",
			ErrExprTypeMismatch, n.op, innerType)
	}
	switch n.op {
	case dtOpTruncate, dtOpAdd, dtOpSub:
		return innerType, nil // Timestamp with same unit / timezone
	case dtOpFormat:
		return arrow.BinaryTypes.String, nil
	default:
		return arrow.PrimitiveTypes.Int64, nil // all extractors
	}
}

func (n *dtOpNode) Children() []Expr { return []Expr{{node: n.inner}} }

func (n *dtOpNode) String() string {
	switch n.op {
	case dtOpTruncate, dtOpFormat:
		return fmt.Sprintf("%s.%s(%q)", n.inner, n.op, n.arg)
	case dtOpAdd, dtOpSub:
		return fmt.Sprintf("%s.%s(%s)", n.inner, n.op, n.dur)
	default:
		return fmt.Sprintf("%s.%s()", n.inner, n.op)
	}
}
