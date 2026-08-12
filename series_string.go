package gobi

import (
	"fmt"
	"regexp"
	"strings"
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

// ------------------------------------------------------------------------
// Shared drivers
// ------------------------------------------------------------------------

// strMapString is the shared driver for String → String ops. Type-
// checks the input column, walks each chunk once, applies fn to
// every non-null value, and returns a String column named
// s.name+suffix. Nulls pass through as nulls.
func strMapString(s Series, fn func(string) string, suffix string) (Series, error) {
	if err := requireString(s); err != nil {
		return Series{}, err
	}
	pool := memory.DefaultAllocator
	b := array.NewStringBuilder(pool)
	defer b.Release()
	for _, chunk := range s.col.Data().Chunks() {
		arr, ok := chunk.(*array.String)
		if !ok {
			return Series{}, fmt.Errorf("%w: expected *array.String, got %T",
				ErrColumnTypeMismatch, chunk)
		}
		for i := range arr.Len() {
			if arr.IsNull(i) {
				b.AppendNull()
				continue
			}
			b.Append(fn(arr.Value(i)))
		}
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
	for _, chunk := range s.col.Data().Chunks() {
		arr, ok := chunk.(*array.String)
		if !ok {
			return Series{}, fmt.Errorf("%w: expected *array.String, got %T",
				ErrColumnTypeMismatch, chunk)
		}
		for i := range arr.Len() {
			if arr.IsNull(i) {
				b.AppendNull()
				continue
			}
			b.Append(fn(arr.Value(i)))
		}
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
	for _, chunk := range s.col.Data().Chunks() {
		arr, ok := chunk.(*array.String)
		if !ok {
			return Series{}, fmt.Errorf("%w: expected *array.String, got %T",
				ErrColumnTypeMismatch, chunk)
		}
		for i := range arr.Len() {
			if arr.IsNull(i) {
				b.AppendNull()
				continue
			}
			b.Append(fn(arr.Value(i)))
		}
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
