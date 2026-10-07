package gobi

import (
	"context"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/compute"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// IsIn returns a Boolean expression that is true where e's value is
// one of values, false where it isn't, and null where e is null (SQL
// `IN` / Polars `is_in`; a filter drops null rows either way).
//
// values are Go scalars — any int / uint width, float32 / float64,
// string, bool, time.Time — or a single slice of them, so both forms
// work:
//
//	gobi.Col("state").IsIn("CA", "NV")
//	gobi.Col("id").IsIn(ids) // ids []int64
//
// Each value is converted to the column's type, as a cast would:
//
//   - Integer columns: a value the type can't hold never matches —
//     1.5 against int64, 300 against int8, -1 against uint32.
//   - Float columns: values round to the column's precision, so 0.1
//     matches a float32 0.1.
//   - Timestamp columns: a time with precision finer than the unit
//     never matches (5.0001s against Timestamp[ms]).
//   - Date columns: a time.Time matches its calendar date in its own
//     location, whatever the time of day.
//
// A value of the wrong kind — a string against a numeric column — is
// a type error, as is a nil value (use IsNull). An empty list is not
// an error: false for every row, null for null rows. Dictionary-
// encoded columns match on their values.
//
// Row-group pruning: as a lazy Filter, or as a read predicate
// (parquetio.ReadOptions.Predicate), IsIn skips Parquet row groups
// whose min/max range holds none of the values. ExprToSQL renders it
// as `col IN (…)` for database pushdown.
func (e Expr) IsIn(values ...any) Expr {
	n := &isInNode{inner: e.node}
	src := reflect.ValueOf(slices.Clone(values))
	if len(values) == 1 {
		if rv := reflect.ValueOf(values[0]); rv.Kind() == reflect.Slice {
			if rv.Type().Elem().Kind() == reflect.Uint8 {
				// []byte and []uint8 are one Go type: a binary value or
				// a list of small ints? Refuse to guess.
				n.err = fmt.Errorf("%w: IsIn: a []byte / []uint8 argument is ambiguous; pass uint8 values as a []int, or individually", ErrUnsupportedLiteral)
				return Expr{node: n}
			}
			// Copied, so a caller reusing its slice can't change a built
			// plan — one typed copy, no per-element boxing.
			src = reflect.MakeSlice(rv.Type(), rv.Len(), rv.Len())
			reflect.Copy(src, rv)
		}
	}
	n.src = src
	n.norm, n.err = normalizeIsIn(src)
	n.prune = newIsInPrune(n.norm)
	return Expr{node: n}
}

// normalizeIsIn converts every value once, with direct loops for the
// common slice types and reflection for the rest.
func normalizeIsIn(src reflect.Value) ([]isInValue, error) {
	switch vals := src.Interface().(type) {
	case []int64:
		return mapIsIn(vals, func(x int64) isInValue { return isInValue{kind: kindInt, i: x, u: uint64(x), f: float64(x)} }), nil
	case []int:
		return mapIsIn(vals, func(x int) isInValue { return isInValue{kind: kindInt, i: int64(x), u: uint64(x), f: float64(x)} }), nil
	case []int32:
		return mapIsIn(vals, func(x int32) isInValue { return isInValue{kind: kindInt, i: int64(x), u: uint64(x), f: float64(x)} }), nil
	case []uint64:
		return mapIsIn(vals, func(x uint64) isInValue {
			return isInValue{kind: kindInt, i: int64(x), u: x, unsigned: true, f: float64(x)}
		}), nil
	case []uint32:
		return mapIsIn(vals, func(x uint32) isInValue {
			return isInValue{kind: kindInt, i: int64(x), u: uint64(x), unsigned: true, f: float64(x)}
		}), nil
	case []float64:
		return mapIsIn(vals, func(x float64) isInValue { return isInValue{kind: kindFloat, f: x} }), nil
	case []string:
		return mapIsIn(vals, func(x string) isInValue { return isInValue{kind: kindString, s: x} }), nil
	}
	out := make([]isInValue, src.Len())
	for i := range out {
		v := src.Index(i).Interface()
		if v == nil {
			return nil, fmt.Errorf("%w: IsIn: nil value (use IsNull)", ErrExprTypeMismatch)
		}
		nv, ok := isInKind(v)
		if !ok {
			return nil, fmt.Errorf("%w: IsIn: unsupported value type %T", ErrUnsupportedLiteral, v)
		}
		out[i] = nv
	}
	return out, nil
}

func mapIsIn[T any](vals []T, f func(T) isInValue) []isInValue {
	out := make([]isInValue, len(vals))
	for i, v := range vals {
		out[i] = f(v)
	}
	return out
}

// isInNode keeps IsIn's values twice: src (a private copy of the
// caller's values, for String and SQL arguments) and norm (normalized
// once, for matching and pruning). intSet / strSet are Go hash sets
// for Int64 and String columns, built on first use and shared by
// every batch (and goroutine) that evaluates the node.
type isInNode struct {
	inner ExprNode
	src   reflect.Value
	norm  []isInValue
	prune isInPrune
	err   error

	intOnce sync.Once
	intSet  map[int64]struct{}
	strOnce sync.Once
	strSet  map[string]struct{}
}

// rawAt returns the caller's i-th value.
func (n *isInNode) rawAt(i int) any { return n.src.Index(i).Interface() }

// isInPrune is norm arranged for row-group pruning: sorted per kind so
// a [min, max] check is a binary search.
type isInPrune struct {
	nums   []float64 // numeric values as float64 (monotonic), sorted, no NaN
	nums32 []float64 // nums rounded to float32 as Eval does for a float32 column, sorted
	nan    bool      // NaN is listed: is_in matches NaN rows, which min/max ignore
	strs   []string  // sorted
	bools  [2]bool   // bools[0]: false is listed; bools[1]: true is
	times  bool      // a time value is listed; stats can't be ordered against it
}

func newIsInPrune(vals []isInValue) isInPrune {
	var p isInPrune
	for _, v := range vals {
		switch v.kind {
		case kindInt, kindFloat:
			if math.IsNaN(v.f) {
				p.nan = true
			} else {
				p.nums = append(p.nums, v.f)
				p.nums32 = append(p.nums32, float64(float32(v.f)))
			}
		case kindString:
			p.strs = append(p.strs, v.s)
		case kindBool:
			if v.b {
				p.bools[1] = true
			} else {
				p.bools[0] = true
			}
		case kindTime:
			p.times = true
		}
	}
	slices.Sort(p.nums)
	slices.Sort(p.nums32)
	slices.Sort(p.strs)
	return p
}

func (n *isInNode) Eval(input *Frame) (Series, error) {
	if n.err != nil {
		return Series{}, n.err
	}
	s, err := n.inner.Eval(input)
	if err != nil {
		return Series{}, err
	}
	if err := isInCheck(s.DataType(), n.norm, n.rawAt); err != nil {
		return Series{}, err
	}
	// The common ID-column types probe a Go set built once per node.
	// arrow-go's is_in would rebuild its hash table from the whole
	// value list on every batch.
	switch s.DataType().ID() {
	case arrow.INT64:
		return n.evalInt64(s)
	case arrow.STRING, arrow.LARGE_STRING:
		return n.evalString(s)
	}
	set, err := isInValueSet(s.DataType(), n.norm)
	if err != nil {
		return Series{}, err
	}
	defer set.Release()
	if s.DataType().ID() == arrow.BOOL {
		// arrow-go's is_in has no Boolean kernel; two possible values
		// make it a direct lookup.
		return isInBool(s, set.(*array.Boolean), n.name())
	}
	ctx := context.Background()
	out, err := compute.IsIn(ctx, compute.SetOptions{
		ValueSet:     compute.NewDatumWithoutOwning(set),
		NullBehavior: compute.NullMatchingEmitNull,
	}, compute.NewDatumWithoutOwning(s.col.Data()))
	if err != nil {
		return Series{}, fmt.Errorf("%w: IsIn on %s: %v", ErrExprTypeMismatch, s.DataType(), err)
	}
	defer out.Release()
	var arr arrow.Array
	switch d := out.(type) {
	case *compute.ArrayDatum:
		arr = d.MakeArray()
	case *compute.ChunkedDatum:
		if arr, err = arrayFromChunked(d.Value); err != nil {
			return Series{}, err
		}
	default:
		return Series{}, fmt.Errorf("gobi: IsIn: unexpected result %s", out.Kind())
	}
	return arrayToSeries(memory.DefaultAllocator, n.name(), arrow.FixedWidthTypes.Boolean, arr)
}

// evalInt64 matches an Int64 column against the cached int64 set.
func (n *isInNode) evalInt64(s Series) (Series, error) {
	n.intOnce.Do(func() {
		n.intSet = make(map[int64]struct{}, len(n.norm))
		for _, v := range n.norm {
			if x, ok := isInInt64(v); ok {
				n.intSet[x] = struct{}{}
			}
		}
	})
	b := array.NewBooleanBuilder(memory.DefaultAllocator)
	defer b.Release()
	b.Reserve(s.Len())
	for _, chunk := range s.col.Data().Chunks() {
		a := chunk.(*array.Int64)
		for i, x := range a.Int64Values() {
			if a.IsNull(i) {
				b.UnsafeAppendBoolToBitmap(false)
				continue
			}
			_, ok := n.intSet[x]
			b.UnsafeAppend(ok)
		}
	}
	return arrayToSeries(memory.DefaultAllocator, n.name(), arrow.FixedWidthTypes.Boolean, b.NewArray())
}

// evalString matches a String / LargeString column against the cached
// string set.
func (n *isInNode) evalString(s Series) (Series, error) {
	n.strOnce.Do(func() {
		n.strSet = make(map[string]struct{}, len(n.norm))
		for _, v := range n.norm {
			n.strSet[v.s] = struct{}{} // isInCheck: all kindString
		}
	})
	b := array.NewBooleanBuilder(memory.DefaultAllocator)
	defer b.Release()
	b.Reserve(s.Len())
	for _, chunk := range s.col.Data().Chunks() {
		a := chunk.(interface {
			arrow.Array
			Value(int) string
		})
		for i := range a.Len() {
			if a.IsNull(i) {
				b.UnsafeAppendBoolToBitmap(false)
				continue
			}
			_, ok := n.strSet[a.Value(i)]
			b.UnsafeAppend(ok)
		}
	}
	return arrayToSeries(memory.DefaultAllocator, n.name(), arrow.FixedWidthTypes.Boolean, b.NewArray())
}

// name is the result column's name, <input>_is_in like IsNull's
// <input>_is_null — not String(), which spells out every value.
func (n *isInNode) name() string {
	if inner, ok := n.inner.(Namer); ok {
		if s := inner.OutputName(); s != "" {
			return s + "_is_in"
		}
	}
	return "expr_is_in"
}

// OutputName so Select picks the derived name automatically.
func (n *isInNode) OutputName() string { return n.name() }

func isInBool(s Series, set *array.Boolean, name string) (Series, error) {
	var want [2]bool // want[0]: false is in the set; want[1]: true is
	for i := range set.Len() {
		if set.Value(i) {
			want[1] = true
		} else {
			want[0] = true
		}
	}
	b := array.NewBooleanBuilder(memory.DefaultAllocator)
	defer b.Release()
	b.Reserve(s.Len())
	for _, chunk := range s.col.Data().Chunks() {
		a := chunk.(*array.Boolean)
		for i := range a.Len() {
			switch {
			case a.IsNull(i):
				b.AppendNull()
			case a.Value(i):
				b.Append(want[1])
			default:
				b.Append(want[0])
			}
		}
	}
	return arrayToSeries(memory.DefaultAllocator, name, arrow.FixedWidthTypes.Boolean, b.NewArray())
}

func (n *isInNode) Type(schema *arrow.Schema) (arrow.DataType, error) {
	if n.err != nil {
		return nil, n.err
	}
	dt, err := n.inner.Type(schema)
	if err != nil {
		return nil, err
	}
	if err := isInCheck(dt, n.norm, n.rawAt); err != nil {
		return nil, err
	}
	return arrow.FixedWidthTypes.Boolean, nil
}

func (n *isInNode) Children() []Expr { return []Expr{{node: n.inner}} }

func (n *isInNode) String() string {
	parts := make([]string, len(n.norm))
	for i := range parts {
		v := n.rawAt(i)
		if s, ok := v.(string); ok {
			parts[i] = fmt.Sprintf("%q", s)
		} else {
			parts[i] = fmt.Sprint(v)
		}
	}
	return fmt.Sprintf("%s.is_in([%s])", n.inner, strings.Join(parts, ", "))
}

// isInValue is a Go value normalized for matching: exactly one field
// is meaningful, per kind.
type isInValue struct {
	kind     isInValueKind
	i        int64  // kindInt: the value, when it came from a signed type
	u        uint64 // kindInt: the value as uint64 (two's complement if signed)
	unsigned bool   // kindInt: came from an unsigned type
	f        float64
	s        string
	b        bool
	t        time.Time
}

type isInValueKind uint8

const (
	kindInt isInValueKind = iota // any integer width, signed or unsigned
	kindFloat
	kindString
	kindBool
	kindTime
)

// isInKind normalizes v. Integers keep their exact value (i / u) and
// an approximation in f.
func isInKind(v any) (isInValue, bool) {
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		x := rv.Int()
		return isInValue{kind: kindInt, i: x, u: uint64(x), f: float64(x)}, true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		x := rv.Uint()
		return isInValue{kind: kindInt, i: int64(x), u: x, unsigned: true, f: float64(x)}, true
	case reflect.Float32, reflect.Float64:
		return isInValue{kind: kindFloat, f: rv.Float()}, true
	case reflect.String:
		return isInValue{kind: kindString, s: rv.String()}, true
	case reflect.Bool:
		return isInValue{kind: kindBool, b: rv.Bool()}, true
	}
	if t, ok := v.(time.Time); ok {
		return isInValue{kind: kindTime, t: t}, true
	}
	return isInValue{}, false
}

// isInCheck reports a value whose kind can't apply to a column of dt
// (or of a dictionary's value type), or a column type IsIn doesn't
// support.
func isInCheck(dt arrow.DataType, values []isInValue, rawAt func(int) any) error {
	if d, ok := dt.(*arrow.DictionaryType); ok {
		dt = d.ValueType
	}
	var want func(isInValueKind) bool
	switch dt.ID() {
	case arrow.INT8, arrow.INT16, arrow.INT32, arrow.INT64,
		arrow.UINT8, arrow.UINT16, arrow.UINT32, arrow.UINT64,
		arrow.FLOAT32, arrow.FLOAT64:
		want = func(k isInValueKind) bool { return k == kindInt || k == kindFloat }
	case arrow.STRING, arrow.LARGE_STRING, arrow.STRING_VIEW:
		want = func(k isInValueKind) bool { return k == kindString }
	case arrow.BOOL:
		want = func(k isInValueKind) bool { return k == kindBool }
	case arrow.TIMESTAMP, arrow.DATE32, arrow.DATE64:
		want = func(k isInValueKind) bool { return k == kindTime }
	default:
		return fmt.Errorf("%w: IsIn on a %s column", ErrExprTypeMismatch, dt)
	}
	for i, v := range values {
		if !want(v.kind) {
			raw := rawAt(i)
			return fmt.Errorf("%w: IsIn: %T value %v against a %s column", ErrExprTypeMismatch, raw, raw, dt)
		}
	}
	return nil
}

// isInValueSet builds the lookup set as an array of exactly the
// column's value type (a dictionary column's value type), keeping only
// the values that type can hold. Kinds are already checked
// (isInCheck).
func isInValueSet(dt arrow.DataType, values []isInValue) (arrow.Array, error) {
	if d, ok := dt.(*arrow.DictionaryType); ok {
		dt = d.ValueType
	}
	pool := memory.DefaultAllocator
	b := array.NewBuilder(pool, dt)
	defer b.Release()
	for _, v := range values {
		switch dt.ID() {
		case arrow.INT8, arrow.INT16, arrow.INT32, arrow.INT64,
			arrow.UINT8, arrow.UINT16, arrow.UINT32, arrow.UINT64:
			appendIsInInt(b, dt.ID(), v)
		case arrow.FLOAT32, arrow.FLOAT64:
			// Rounded to the column's precision, as a cast would.
			if fb, ok := b.(*array.Float32Builder); ok {
				fb.Append(float32(v.f))
			} else {
				b.(*array.Float64Builder).Append(v.f)
			}
		case arrow.STRING, arrow.LARGE_STRING, arrow.STRING_VIEW:
			b.(interface{ Append(string) }).Append(v.s)
		case arrow.BOOL:
			b.(*array.BooleanBuilder).Append(v.b)
		case arrow.TIMESTAMP:
			if ts, ok := timeInUnit(v.t, dt.(*arrow.TimestampType).Unit); ok {
				b.(*array.TimestampBuilder).Append(arrow.Timestamp(ts))
			}
		case arrow.DATE32, arrow.DATE64:
			// The value's calendar date in its own location.
			y, m, d := v.t.Date()
			u := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
			if db, ok := b.(*array.Date32Builder); ok {
				db.Append(arrow.Date32FromTime(u))
			} else {
				b.(*array.Date64Builder).Append(arrow.Date64FromTime(u))
			}
		default:
			return nil, fmt.Errorf("%w: IsIn on a %s column", ErrExprTypeMismatch, dt)
		}
	}
	return b.NewArray(), nil
}

// isInInt64 is v as an exact int64, if it has one.
func isInInt64(v isInValue) (int64, bool) {
	switch v.kind {
	case kindInt:
		if v.unsigned && v.u > math.MaxInt64 {
			return 0, false
		}
		return v.i, true
	case kindFloat:
		if v.f != math.Trunc(v.f) || v.f < -0x1p63 || v.f >= 0x1p63 {
			return 0, false
		}
		return int64(v.f), true
	}
	return 0, false
}

// appendIsInInt appends v to an integer builder when the column's type
// holds it exactly; anything else can't match and is skipped.
func appendIsInInt(b array.Builder, id arrow.Type, v isInValue) {
	// Exact integer as (neg, i, u): i is valid unless big (a value
	// above MaxInt64), u unless neg.
	var neg, big bool
	var i int64
	var u uint64
	switch v.kind {
	case kindInt:
		neg, i, u = v.i < 0 && !v.unsigned, v.i, v.u
		big = v.unsigned && v.u > math.MaxInt64
	case kindFloat:
		if v.f != math.Trunc(v.f) || v.f < -0x1p63 || v.f >= 0x1p64 {
			return
		}
		if neg = v.f < 0; neg {
			i = int64(v.f)
		} else {
			u = uint64(v.f)
			big = u > math.MaxInt64
			i = int64(u)
		}
	}
	signed := func(lo, hi int64) bool { return !big && i >= lo && i <= hi }
	unsigned := func(hi uint64) bool { return !neg && u <= hi }
	switch id {
	case arrow.INT8:
		if signed(math.MinInt8, math.MaxInt8) {
			b.(*array.Int8Builder).Append(int8(i))
		}
	case arrow.INT16:
		if signed(math.MinInt16, math.MaxInt16) {
			b.(*array.Int16Builder).Append(int16(i))
		}
	case arrow.INT32:
		if signed(math.MinInt32, math.MaxInt32) {
			b.(*array.Int32Builder).Append(int32(i))
		}
	case arrow.INT64:
		if signed(math.MinInt64, math.MaxInt64) {
			b.(*array.Int64Builder).Append(i)
		}
	case arrow.UINT8:
		if unsigned(math.MaxUint8) {
			b.(*array.Uint8Builder).Append(uint8(u))
		}
	case arrow.UINT16:
		if unsigned(math.MaxUint16) {
			b.(*array.Uint16Builder).Append(uint16(u))
		}
	case arrow.UINT32:
		if unsigned(math.MaxUint32) {
			b.(*array.Uint32Builder).Append(uint32(u))
		}
	case arrow.UINT64:
		if unsigned(math.MaxUint64) {
			b.(*array.Uint64Builder).Append(u)
		}
	}
}

// timeInUnit converts t to a count of unit since the epoch, reporting
// false when t has precision finer than unit or doesn't fit int64.
func timeInUnit(t time.Time, unit arrow.TimeUnit) (int64, bool) {
	nsPerUnit := unitToNanos(unit)
	perSec := int64(1e9) / nsPerUnit
	sec, nsec := t.Unix(), int64(t.Nanosecond())
	if nsec%nsPerUnit != 0 {
		return 0, false
	}
	frac := nsec / nsPerUnit
	if sec > math.MaxInt64/perSec || sec < math.MinInt64/perSec || sec*perSec > math.MaxInt64-frac {
		return 0, false
	}
	return sec*perSec + frac, true
}
