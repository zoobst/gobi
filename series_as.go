package gobi

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/compute"

	"github.com/zoobst/gobi/geometry"
)

// -----------------------------------------------------------------------------
// Lenient extraction — the As* methods convert across types instead of
// erroring on a type mismatch like Float64s / Strings / Timestamps do.
//
// Every method:
//   - Returns (values, nulls, err). nulls[i] is true when row i has no
//     value — a null in the source, or a value that couldn't be
//     converted (e.g. "n/a" in AsFloat64s) — and values[i] is then the
//     zero value. To find conversion failures, compare with Nulls().
//   - Errors only when the column's type has no conversion at all
//     (e.g. a list column in AsFloat64s).
//   - Reads dictionary-encoded columns through their dictionary.
//   - Returns new slices, safe to keep after the Frame is Released.
//
// Where arrow-go's compute.CastArray gives the same result it does the
// conversion (vectorized, and it unpacks dictionaries). Its cast is
// strict — one unparseable string fails the whole column — so when it
// errors, the column is converted row by row instead, turning the bad
// values into nulls.
// -----------------------------------------------------------------------------

// AsStrings returns every value as text:
//
//   - strings as-is (surrounding space kept)
//   - integers in base 10; floats in the shortest form that round-trips,
//     in fixed notation for 1e-7 <= |x| < 1e21 and exponent form outside
//     it (JavaScript's Number.prototype.toString rule). An integral
//     float prints as an integer — 366999001.0 is "366999001" — so IDs
//     stored as float64 (a pandas int column with nulls) match the same
//     IDs read from an int64 column.
//   - booleans as "true" / "false"
//   - timestamps as RFC 3339 with nanoseconds, in the column's time zone
//     (UTC when it has none); dates as "2006-01-02"
//   - geometry columns as WKT
//   - anything else in Arrow's own text form (arrow.Array.ValueStr)
//
// Every type converts; the error is for an unreadable geometry value.
func (s Series) AsStrings() ([]string, []bool, error) {
	loc := valueTimeLocation(s)
	geom := s.IsGeometry()
	// Arrow formats floats with 'g' and timestamps its own way, knows
	// nothing of geometry, and casts binary only when it happens to be
	// valid UTF-8; everything else it casts to the same text.
	if s.col != nil && !geom && !rowWiseString(s.DataType()) {
		vals := make([]string, s.Len())
		nulls := make([]bool, s.Len())
		if castChunks(s, arrow.BinaryTypes.String, func(arr arrow.Array, row int) {
			a := arr.(*array.String)
			for i := range a.Len() {
				if nulls[row+i] = a.IsNull(i); !nulls[row+i] {
					vals[row+i] = a.Value(i)
				}
			}
		}) {
			return vals, nulls, nil
		}
	}
	return asValues(s, func(arr arrow.Array, i int) (string, bool, error) {
		switch a := arr.(type) {
		case *array.String:
			return a.Value(i), true, nil
		case *array.LargeString:
			return a.Value(i), true, nil
		case *array.StringView:
			return a.Value(i), true, nil
		case *array.Boolean:
			return strconv.FormatBool(a.Value(i)), true, nil
		case *array.Float64:
			return formatFloat(a.Value(i), 64), true, nil
		case *array.Float32:
			return formatFloat(float64(a.Value(i)), 32), true, nil
		case *array.Float16:
			return formatFloat(float64(a.Value(i).Float32()), 32), true, nil
		case *array.Timestamp:
			unit := a.DataType().(*arrow.TimestampType).Unit
			return a.Value(i).ToTime(unit).In(loc).Format(time.RFC3339Nano), true, nil
		case *array.Date32:
			return a.Value(i).ToTime().Format(time.DateOnly), true, nil
		case *array.Date64:
			return a.Value(i).ToTime().Format(time.DateOnly), true, nil
		case *array.Binary:
			if geom {
				g, err := geometry.ParseWKB(a.Value(i))
				if err != nil {
					return "", false, fmt.Errorf("gobi: AsStrings: row geometry: %w", err)
				}
				return g.WKT(), true, nil
			}
		}
		if v, ok := intValue(arr, i); ok {
			return strconv.FormatInt(v, 10), true, nil
		}
		if u, ok := arr.(*array.Uint64); ok {
			return strconv.FormatUint(u.Value(i), 10), true, nil
		}
		return arr.ValueStr(i), true, nil
	})
}

// AsFloat64s returns every value as float64:
//
//   - integers and floats converted (integers beyond 2^53 round)
//   - booleans as 1 / 0
//   - decimals through their exact text form
//   - strings parsed with strconv.ParseFloat after trimming space;
//     a string that doesn't parse ("", "n/a", "1,234") is null
//
// Timestamps, dates, binary and nested types return an error.
func (s Series) AsFloat64s() ([]float64, []bool, error) {
	if err := asSupported(s, "AsFloat64s", func(id arrow.Type) bool {
		switch id {
		case arrow.STRING, arrow.LARGE_STRING, arrow.STRING_VIEW, arrow.BOOL,
			arrow.FLOAT64, arrow.FLOAT32, arrow.FLOAT16,
			arrow.INT8, arrow.INT16, arrow.INT32, arrow.INT64,
			arrow.UINT8, arrow.UINT16, arrow.UINT32, arrow.UINT64,
			arrow.DECIMAL32, arrow.DECIMAL64, arrow.DECIMAL128, arrow.DECIMAL256:
			return true
		}
		return false
	}); err != nil {
		return nil, nil, err
	}
	vals := make([]float64, s.Len())
	nulls := make([]bool, s.Len())
	if castChunks(s, arrow.PrimitiveTypes.Float64, func(arr arrow.Array, row int) {
		a := arr.(*array.Float64)
		copy(vals[row:], a.Float64Values())
		for i := range a.Len() {
			if a.IsNull(i) {
				nulls[row+i], vals[row+i] = true, 0
			}
		}
	}) {
		return vals, nulls, nil
	}
	return asValues(s, func(arr arrow.Array, i int) (float64, bool, error) {
		switch a := arr.(type) {
		case *array.Float64:
			return a.Value(i), true, nil
		case *array.Float32:
			return float64(a.Value(i)), true, nil
		case *array.Boolean:
			if a.Value(i) {
				return 1, true, nil
			}
			return 0, true, nil
		case *array.Uint64:
			return float64(a.Value(i)), true, nil
		case *array.String, *array.LargeString, *array.StringView:
			v, err := strconv.ParseFloat(strings.TrimSpace(arr.ValueStr(i)), 64)
			if err != nil && !isRangeErr(err) {
				return 0, false, nil
			}
			return v, true, nil // out-of-range parses as ±Inf / ±0
		}
		if v, ok := intValue(arr, i); ok {
			return float64(v), true, nil
		}
		// Float16 and decimals: their text form is exact.
		v, err := strconv.ParseFloat(arr.ValueStr(i), 64)
		if err != nil && !isRangeErr(err) {
			return 0, false, nil
		}
		return v, true, nil
	})
}

// defaultTimeLayouts is AsTimes' layout list when none is given.
var defaultTimeLayouts = []string{
	time.RFC3339Nano,
	"2006-01-02T15:04:05.999999999",
	"2006-01-02 15:04:05.999999999Z07:00",
	"2006-01-02 15:04:05.999999999",
	time.DateOnly,
}

// AsTimes returns every value as a time.Time:
//
//   - timestamps in the column's time zone (UTC when it has none)
//   - dates as midnight UTC
//   - strings parsed with the first matching layout, after trimming
//     space; a string no layout matches is null. Without layouts the
//     list is RFC 3339 (with or without a fraction), the same with a
//     space for the 'T', both of those without an offset (read as UTC),
//     and a bare "2006-01-02".
//
// Numbers aren't read as epoch offsets — the unit would be a guess.
// Build those with NewTimestampSeriesUnit. Other types return an error.
func (s Series) AsTimes(layouts ...string) ([]time.Time, []bool, error) {
	if err := asSupported(s, "AsTimes", func(id arrow.Type) bool {
		switch id {
		case arrow.STRING, arrow.LARGE_STRING, arrow.STRING_VIEW,
			arrow.TIMESTAMP, arrow.DATE32, arrow.DATE64:
			return true
		}
		return false
	}); err != nil {
		return nil, nil, err
	}
	if len(layouts) == 0 {
		layouts = defaultTimeLayouts
	}
	loc := valueTimeLocation(s)
	return asValues(s, func(arr arrow.Array, i int) (time.Time, bool, error) {
		switch a := arr.(type) {
		case *array.Timestamp:
			unit := a.DataType().(*arrow.TimestampType).Unit
			return a.Value(i).ToTime(unit).In(loc), true, nil
		case *array.Date32:
			return a.Value(i).ToTime(), true, nil
		case *array.Date64:
			return a.Value(i).ToTime(), true, nil
		}
		str := strings.TrimSpace(arr.ValueStr(i))
		for _, layout := range layouts {
			if t, err := time.Parse(layout, str); err == nil {
				return t, true, nil
			}
		}
		return time.Time{}, false, nil
	})
}

// asValues walks s row by row, resolving dictionaries, and collects
// conv's results. conv returns ok=false for a value it can't convert.
func asValues[T any](s Series, conv func(arr arrow.Array, i int) (T, bool, error)) ([]T, []bool, error) {
	if s.col == nil {
		return nil, nil, fmt.Errorf("gobi: nil column")
	}
	vals := make([]T, s.Len())
	nulls := make([]bool, s.Len())
	row := 0
	for _, chunk := range s.col.Data().Chunks() {
		for i := range chunk.Len() {
			arr, j := resolveDictionary(chunk, i)
			if arr.IsNull(j) {
				nulls[row] = true
			} else {
				v, ok, err := conv(arr, j)
				if err != nil {
					return nil, nil, fmt.Errorf("%w (row %d)", err, row)
				}
				vals[row], nulls[row] = v, !ok
			}
			row++
		}
	}
	return vals, nulls, nil
}

// asSupported checks the column's value type (the dictionary's value
// type for a dictionary column) against ok.
func asSupported(s Series, method string, ok func(arrow.Type) bool) error {
	if s.col == nil {
		return fmt.Errorf("gobi: %s: nil column", method)
	}
	dt := s.DataType()
	for {
		d, isDict := dt.(*arrow.DictionaryType)
		if !isDict {
			break
		}
		dt = d.ValueType
	}
	if !ok(dt.ID()) {
		return fmt.Errorf("%w: Series.%s can't convert %s", ErrColumnTypeMismatch, method, s.DataType())
	}
	return nil
}

// intValue reads any signed or unsigned integer except uint64 (which
// can exceed int64) as int64.
func intValue(arr arrow.Array, i int) (int64, bool) {
	switch a := arr.(type) {
	case *array.Int64:
		return a.Value(i), true
	case *array.Int32:
		return int64(a.Value(i)), true
	case *array.Int16:
		return int64(a.Value(i)), true
	case *array.Int8:
		return int64(a.Value(i)), true
	case *array.Uint32:
		return int64(a.Value(i)), true
	case *array.Uint16:
		return int64(a.Value(i)), true
	case *array.Uint8:
		return int64(a.Value(i)), true
	}
	return 0, false
}

// formatFloat writes v (of the given bit size) in the shortest form
// that round-trips: fixed notation for 1e-7 <= |v| < 1e21 (and zero),
// exponent form otherwise, including NaN and ±Inf.
func formatFloat(v float64, bits int) string {
	if a := math.Abs(v); a == 0 || (a >= 1e-7 && a < 1e21) {
		return strconv.FormatFloat(v, 'f', -1, bits)
	}
	return strconv.FormatFloat(v, 'g', -1, bits)
}

// valueTimeLocation is timeLocation for the As* methods: it also reads
// the zone of a dictionary-encoded timestamp column, whose DataType is
// the DictionaryType rather than the TimestampType.
func valueTimeLocation(s Series) *time.Location {
	dt := s.DataType()
	if d, ok := dt.(*arrow.DictionaryType); ok {
		dt = d.ValueType
	}
	if ts, ok := dt.(*arrow.TimestampType); ok && ts.TimeZone != "" {
		if loc, err := time.LoadLocation(ts.TimeZone); err == nil {
			return loc
		}
	}
	return time.UTC
}

func isRangeErr(err error) bool {
	ne, ok := err.(*strconv.NumError)
	return ok && ne.Err == strconv.ErrRange
}

// castChunks casts each chunk of s to target with compute.CastArray
// and hands read each result with its first row's index. It reports
// false — leaving the caller to convert row by row — as soon as a
// chunk fails to cast.
func castChunks(s Series, target arrow.DataType, read func(arr arrow.Array, row int)) bool {
	ctx := context.Background()
	opts := compute.UnsafeCastOptions(target)
	row := 0
	for _, chunk := range s.col.Data().Chunks() {
		out, err := compute.CastArray(ctx, chunk, opts)
		if err != nil {
			return false
		}
		read(out, row)
		row += out.Len()
		out.Release()
	}
	return true
}

// rowWiseString reports whether AsStrings must format dt (or a
// dictionary's value type) itself rather than through CastArray.
func rowWiseString(dt arrow.DataType) bool {
	if d, ok := dt.(*arrow.DictionaryType); ok {
		dt = d.ValueType
	}
	switch dt.ID() {
	case arrow.FLOAT16, arrow.FLOAT32, arrow.FLOAT64, arrow.TIMESTAMP,
		arrow.BINARY, arrow.LARGE_BINARY, arrow.BINARY_VIEW, arrow.FIXED_SIZE_BINARY:
		return true
	}
	return false
}
