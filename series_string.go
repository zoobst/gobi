package gobi

import (
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// ------------------------------------------------------------------------
// String Series methods — row-wise UTF-8 string ops. All methods
// operate on Arrow String-typed columns (utf8 type) and preserve
// nulls: a null input row produces a null output row.
//
// Under the hood every op walks the column's chunks once, applies
// a Go stdlib string function (strings / regexp) to each non-null
// value, and emits a new column. Shared drivers keep the individual
// methods thin (~4 lines each).
// ------------------------------------------------------------------------

// StrLower returns a copy of s with every value lowercased.
// Unicode-aware (delegates to strings.ToLower).
func (s Series) StrLower() (Series, error) {
	return strMapString(s, strings.ToLower, "_lower")
}

// StrUpper returns a copy of s with every value uppercased.
// Unicode-aware (delegates to strings.ToUpper).
func (s Series) StrUpper() (Series, error) {
	return strMapString(s, strings.ToUpper, "_upper")
}

// StrTrim returns a copy of s with leading and trailing whitespace
// removed from every value (Unicode-aware, matches strings.TrimSpace).
func (s Series) StrTrim() (Series, error) {
	return strMapString(s, strings.TrimSpace, "_trim")
}

// StrTrimLeft returns a copy of s with any leading Unicode code
// points in `cutset` removed from every value.
func (s Series) StrTrimLeft(cutset string) (Series, error) {
	return strMapString(s, func(v string) string {
		return strings.TrimLeft(v, cutset)
	}, "_trim_left")
}

// StrTrimRight returns a copy of s with any trailing Unicode code
// points in `cutset` removed from every value.
func (s Series) StrTrimRight(cutset string) (Series, error) {
	return strMapString(s, func(v string) string {
		return strings.TrimRight(v, cutset)
	}, "_trim_right")
}

// StrLen returns an Int64 column with the Unicode-codepoint count
// of every value. Distinct from byte length: a 3-character emoji
// string returns 3, not the number of bytes. Use `.Cast(Int64)`
// after a byte-length op if you need bytes.
func (s Series) StrLen() (Series, error) {
	return strMapInt64(s, func(v string) int64 {
		return int64(utf8.RuneCountInString(v))
	}, "_len")
}

// StrContains returns a Boolean column: true when the row's value
// contains `substr`, false otherwise. Case-sensitive.
func (s Series) StrContains(substr string) (Series, error) {
	return strMapBool(s, func(v string) bool {
		return strings.Contains(v, substr)
	}, "_contains")
}

// StrStartsWith returns a Boolean column: true when the row's value
// begins with `prefix`. Case-sensitive.
func (s Series) StrStartsWith(prefix string) (Series, error) {
	return strMapBool(s, func(v string) bool {
		return strings.HasPrefix(v, prefix)
	}, "_starts_with")
}

// StrEndsWith returns a Boolean column: true when the row's value
// ends with `suffix`. Case-sensitive.
func (s Series) StrEndsWith(suffix string) (Series, error) {
	return strMapBool(s, func(v string) bool {
		return strings.HasSuffix(v, suffix)
	}, "_ends_with")
}

// StrReplace returns a copy of s with every occurrence of `find`
// replaced by `replacement` in every value. Literal (non-regex)
// replacement. Empty `find` inserts `replacement` between every
// pair of characters (matches strings.ReplaceAll semantics).
func (s Series) StrReplace(find, replacement string) (Series, error) {
	return strMapString(s, func(v string) string {
		return strings.ReplaceAll(v, find, replacement)
	}, "_replace")
}

// StrSlice returns a substring of each value by Unicode-codepoint
// index. Negative `start` or `end` counts from the right (Python
// convention: -1 is the last codepoint). end == 0 means "to the
// end of the string." Out-of-range indices clamp to the string's
// extent — no panics.
//
// Example: "hello".StrSlice(1, 4) → "ell".
func (s Series) StrSlice(start, end int) (Series, error) {
	return strMapString(s, func(v string) string {
		return sliceCodepoints(v, start, end)
	}, "_slice")
}

// StrConcat returns a copy of s where every value is followed by
// the same `suffix` string. Pairwise concat against another String
// column isn't currently exposed — build it explicitly via a Custom
// ExprNode or a Frame.WithColumn expression until the pairwise
// variant lands (v0.3.7 candidate).
func (s Series) StrConcat(suffix string) (Series, error) {
	return strMapString(s, func(v string) string {
		return v + suffix
	}, "_concat")
}

// StrRegexMatch returns a Boolean column: true when the row's
// value matches `pattern` (RE2 syntax, matches regexp.MatchString
// semantics — searches for the pattern anywhere in the value).
// Anchor with ^ / $ for full-string match. The pattern is compiled
// once inside this call.
//
// Streaming pipelines that filter with the same regex across
// many batches should prefer Expr.StrRegexMatch — that path
// compiles the pattern once at Expr-build time and reuses the
// compiled *regexp.Regexp across every batch's Eval.
func (s Series) StrRegexMatch(pattern string) (Series, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return Series{}, fmt.Errorf("gobi: StrRegexMatch: %w", err)
	}
	return s.strRegexMatchCompiled(re)
}

// StrRegexReplace returns a copy of s with every match of
// `pattern` replaced by `replacement` in every value. Uses
// regexp.ReplaceAllString semantics — `replacement` may reference
// capture groups via $1 / $2 / ${name}. Pattern compiled once
// inside this call; use Expr.StrRegexReplace for streaming
// pipelines that need across-batch caching.
func (s Series) StrRegexReplace(pattern, replacement string) (Series, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return Series{}, fmt.Errorf("gobi: StrRegexReplace: %w", err)
	}
	return s.strRegexReplaceCompiled(re, replacement)
}

// strRegexMatchCompiled is the pre-compiled-regex variant that
// Expr.StrRegexMatch dispatches to. Kept unexported since the
// public entry point + Expr layer cover the two natural call
// shapes.
func (s Series) strRegexMatchCompiled(re *regexp.Regexp) (Series, error) {
	return strMapBool(s, re.MatchString, "_regex_match")
}

// strRegexReplaceCompiled is the pre-compiled-regex variant that
// Expr.StrRegexReplace dispatches to.
func (s Series) strRegexReplaceCompiled(re *regexp.Regexp, replacement string) (Series, error) {
	return strMapString(s, func(v string) string {
		return re.ReplaceAllString(v, replacement)
	}, "_regex_replace")
}

// newStrToTimeType returns StrToTime's output type, fresh per call so
// no caller can change another's by mutating it. Microseconds rather
// than gobi's usual nanoseconds: every year time.Parse accepts
// (0000-9999) fits, where nanoseconds stop at 1677-2262.
func newStrToTimeType() *arrow.TimestampType {
	return &arrow.TimestampType{Unit: arrow.Microsecond, TimeZone: "UTC"}
}

// StrToTime parses each value with the Go time layout (see the time
// package; "" means time.RFC3339, matching DateFormat) and returns a
// Timestamp[µs, UTC] column. The column may be String, LargeString or
// StringView.
//
//   - A value with a UTC offset or "Z" becomes that instant in UTC; a
//     layout without a zone reads values as UTC, on every host.
//   - A zone abbreviation other than UTC or GMT has no fixed offset
//     ("EST", "CST" and "IST" each mean several), so a layout that
//     has one without a numeric offset rejects such values: use a
//     numeric offset ("-0700") instead.
//   - Digits past the microsecond round to the nearest microsecond,
//     halves away from zero, as AddDuration and the other timestamp
//     operators round.
//   - Nulls stay null. Any other value that doesn't match, including
//     "", fails with ErrTimeParse: no partial column, no silent nulls.
//     The error quotes the value and gives its index in the column
//     being parsed; inside a LazyFrame that is the current batch, so
//     search for the value rather than counting rows.
//
// To keep placeholders such as Hive's __HIVE_DEFAULT_PARTITION__ from
// failing the parse, turn them into nulls first. If evaluates both
// branches on every row, so the condition goes inside the parse, not
// around it:
//
//	dt := gobi.Col("dt")
//	gobi.If(dt.Eq(gobi.Lit("__HIVE_DEFAULT_PARTITION__")),
//	    gobi.LitNull(arrow.BinaryTypes.String), dt).StrToDate("")
//
// Each parse is remembered until the value changes, so a column that
// repeats one value per file (a partition key) parses once per run of
// equal values.
//
//	s.StrToTime("2006-01-02 15:04:05")   // "2024-01-02 03:04:05"
//	s.StrToTime("")                      // "2024-01-02T03:04:05-05:00"
func (s Series) StrToTime(layout string) (Series, error) {
	if layout == "" {
		layout = time.RFC3339
	}
	if err := requireStringLike(s, "StrToTime"); err != nil {
		return Series{}, err
	}
	typ := newStrToTimeType()
	pool := memory.DefaultAllocator
	b := array.NewTimestampBuilder(pool, typ)
	defer b.Release()
	b.Reserve(s.Len())
	p := newTimeParser(s.name, "StrToTime", layout, func(t time.Time) arrow.Timestamp {
		// time.Parse's years (0000-9999) always fit microseconds.
		v, _ := timeToUnit(t, arrow.Microsecond, timeRoundHalfAway)
		return v
	})
	err := strEach(s, b.AppendNull, func(i int, v string) error {
		x, err := p.parse(i, v)
		if err != nil {
			return err
		}
		b.UnsafeAppend(x)
		return nil
	})
	if err != nil {
		return Series{}, err
	}
	return arrayToSeries(pool, s.name+"_to_time", typ, b.NewArray())
}

// StrToDate parses each value with the Go time layout ("" means
// time.DateOnly, "2006-01-02") and returns a Date32 column holding
// the calendar date as written: "2024-01-02T23:00:00-05:00" is
// 2024-01-02, not the UTC date of that instant, and a zone
// abbreviation doesn't matter. Any time of day is dropped. Column
// types, nulls, errors and placeholders are as in StrToTime.
func (s Series) StrToDate(layout string) (Series, error) {
	if layout == "" {
		layout = time.DateOnly
	}
	if err := requireStringLike(s, "StrToDate"); err != nil {
		return Series{}, err
	}
	pool := memory.DefaultAllocator
	b := array.NewDate32Builder(pool)
	defer b.Release()
	b.Reserve(s.Len())
	p := newTimeParser(s.name, "StrToDate", layout, func(t time.Time) arrow.Date32 {
		// The wall-clock date, as midnight UTC: Date32FromTime
		// truncates the instant, so it needs the date rebuilt first
		// (as Expr.IsIn does).
		y, m, d := t.Date()
		return arrow.Date32FromTime(time.Date(y, m, d, 0, 0, 0, 0, time.UTC))
	})
	p.anyZone = true // the date as written doesn't depend on it
	err := strEach(s, b.AppendNull, func(i int, v string) error {
		x, err := p.parse(i, v)
		if err != nil {
			return err
		}
		b.UnsafeAppend(x)
		return nil
	})
	if err != nil {
		return Series{}, err
	}
	return arrayToSeries(pool, s.name+"_to_date", arrow.FixedWidthTypes.Date32, b.NewArray())
}

// timeParser parses one column's values for StrToTime / StrToDate
// and converts each to the output value T, remembering the last
// string and its value so a run of equal strings (a partition key)
// costs a comparison per row.
type timeParser[T any] struct {
	col, op, layout string
	conv            func(time.Time) T
	// anyZone accepts a zone abbreviation with no known offset. Off
	// (for StrToTime) when the layout names a zone only by
	// abbreviation, so "EST" can't silently read as UTC.
	anyZone bool

	last   string
	lastX  T
	cached bool
}

func newTimeParser[T any](col, op, layout string, conv func(time.Time) T) *timeParser[T] {
	// Every numeric-offset element of a layout contains "-07" or
	// "Z07"; "MST" is the abbreviation element.
	abbrevOnly := strings.Contains(layout, "MST") &&
		!strings.Contains(layout, "-07") && !strings.Contains(layout, "Z07")
	return &timeParser[T]{col: col, op: op, layout: layout, conv: conv, anyZone: !abbrevOnly}
}

func (p *timeParser[T]) parse(i int, v string) (T, error) {
	if p.cached && v == p.last {
		return p.lastX, nil
	}
	// In UTC rather than time.Parse's Local, so an abbreviation can't
	// resolve differently from one host to the next.
	t, err := time.ParseInLocation(p.layout, v, time.UTC)
	if err == nil && !p.anyZone {
		// Unresolvable abbreviations parse as a zero-offset zone of
		// that name; UTC itself, and GMT / GMT+h (whose offset is in
		// the name), are the ones that are right.
		if name, _ := t.Zone(); name != "UTC" && !strings.HasPrefix(name, "GMT") {
			err = fmt.Errorf("zone abbreviation %q in %q has no fixed offset; use a layout with a numeric offset (-0700)", name, v)
		}
	}
	if err != nil {
		var zero T
		return zero, fmt.Errorf("%w: %s: column %q, index %d: %w", ErrTimeParse, p.op, p.col, i, err)
	}
	p.last, p.lastX, p.cached = v, p.conv(t), true
	return p.lastX, nil
}

// ------------------------------------------------------------------------
// Shared drivers
// ------------------------------------------------------------------------

// stringArray is the read side shared by String, LargeString and
// StringView arrays.
type stringArray interface {
	arrow.Array
	Value(int) string
}

// strEach walks string column s in order, calling null for each null
// row and val with the row's index in s and its value for the rest.
// String, LargeString and StringView chunks are all read; callers
// that accept fewer check the type first. An error from val stops the
// walk.
func strEach(s Series, null func(), val func(i int, v string) error) error {
	row := 0
	for _, chunk := range s.col.Data().Chunks() {
		var err error
		switch arr := chunk.(type) {
		case *array.String:
			// The common case, on the concrete type so Value inlines.
			row, err = strEachChunk(arr, row, null, val)
		case stringArray:
			row, err = strEachChunk(arr, row, null, val)
		default:
			return fmt.Errorf("%w: expected a string array, got %T",
				ErrColumnTypeMismatch, chunk)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// strEachChunk is strEach's loop over one chunk whose first row is
// row; it returns the row after the chunk.
func strEachChunk[A stringArray](arr A, row int, null func(), val func(int, string) error) (int, error) {
	for i := range arr.Len() {
		if arr.IsNull(i) {
			null()
		} else if err := val(row, arr.Value(i)); err != nil {
			return row, err
		}
		row++
	}
	return row, nil
}

// strMapString is the shared driver for String → String ops. Type-
// checks the input column, walks it once, applies fn to every non-null
// value, and returns a String column named s.name+suffix. Nulls pass
// through as nulls.
func strMapString(s Series, fn func(string) string, suffix string) (Series, error) {
	if err := requireString(s); err != nil {
		return Series{}, err
	}
	pool := memory.DefaultAllocator
	b := array.NewStringBuilder(pool)
	defer b.Release()
	if err := strEach(s, b.AppendNull, func(_ int, v string) error {
		b.Append(fn(v))
		return nil
	}); err != nil {
		return Series{}, err
	}
	return arrayToSeries(pool, s.name+suffix, arrow.BinaryTypes.String, b.NewArray())
}

// strMapInt64 is the shared driver for String → Int64 ops (StrLen).
func strMapInt64(s Series, fn func(string) int64, suffix string) (Series, error) {
	if err := requireString(s); err != nil {
		return Series{}, err
	}
	pool := memory.DefaultAllocator
	b := array.NewInt64Builder(pool)
	defer b.Release()
	b.Reserve(s.Len())
	if err := strEach(s, b.AppendNull, func(_ int, v string) error {
		b.UnsafeAppend(fn(v))
		return nil
	}); err != nil {
		return Series{}, err
	}
	return arrayToSeries(pool, s.name+suffix, arrow.PrimitiveTypes.Int64, b.NewArray())
}

// strMapBool is the shared driver for String → Bool ops (predicates).
func strMapBool(s Series, fn func(string) bool, suffix string) (Series, error) {
	if err := requireString(s); err != nil {
		return Series{}, err
	}
	pool := memory.DefaultAllocator
	b := array.NewBooleanBuilder(pool)
	defer b.Release()
	b.Reserve(s.Len())
	if err := strEach(s, b.AppendNull, func(_ int, v string) error {
		b.UnsafeAppend(fn(v))
		return nil
	}); err != nil {
		return Series{}, err
	}
	return arrayToSeries(pool, s.name+suffix, arrow.FixedWidthTypes.Boolean, b.NewArray())
}

// requireString errors when s isn't an Arrow String column.
// Called at the top of every string driver.
func requireString(s Series) error {
	if s.DataType().ID() != arrow.STRING {
		return fmt.Errorf("%w: expected String column, got %s",
			ErrColumnTypeMismatch, s.DataType())
	}
	return nil
}

// isStringLike reports whether id is a string type strEach reads.
func isStringLike(id arrow.Type) bool {
	return id == arrow.STRING || id == arrow.LARGE_STRING || id == arrow.STRING_VIEW
}

// requireStringLike is requireString for the ops that also read
// LargeString and StringView.
func requireStringLike(s Series, op string) error {
	if !isStringLike(s.DataType().ID()) {
		return fmt.Errorf("%w: %s: expected a String, LargeString or StringView column, got %s",
			ErrColumnTypeMismatch, op, s.DataType())
	}
	return nil
}

// sliceCodepoints returns the substring of v between codepoint
// indices [start, end). Python-style negative indexing supported.
// end == 0 is treated as "through the last codepoint" — callers
// who really want a zero-length slice can pass end=start. Out-of-
// range indices clamp rather than panic.
func sliceCodepoints(v string, start, end int) string {
	runes := []rune(v)
	n := len(runes)
	if start < 0 {
		start += n
	}
	switch {
	case end == 0:
		end = n // "to end" convention
	case end < 0:
		end += n
	}
	if start < 0 {
		start = 0
	}
	end = min(end, n)
	start = min(start, n)
	if start >= end {
		return ""
	}
	return string(runes[start:end])
}
