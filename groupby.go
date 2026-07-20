package gobi

import (
	"fmt"
	"sort"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// AggKind identifies an aggregation operation.
type AggKind uint8

const (
	AggCount AggKind = iota
	AggSum
	AggMean
	AggMin
	AggMax
)

func (k AggKind) String() string {
	switch k {
	case AggCount:
		return "count"
	case AggSum:
		return "sum"
	case AggMean:
		return "mean"
	case AggMin:
		return "min"
	case AggMax:
		return "max"
	default:
		return "unknown"
	}
}

// Aggregation names a column to aggregate and how to aggregate it. The
// resulting column in the aggregated frame is named Alias (or, if empty,
// "<column>_<kind>" for built-in kinds, "<column>_<Fn.Name()>" for
// custom aggregators).
//
// When Fn is non-nil, it takes precedence over Kind: the aggregation is
// user-defined and Fn is called once per group. Otherwise Kind selects
// a built-in aggregation.
type Aggregation struct {
	Column string
	Kind   AggKind
	Alias  string
	Fn     Aggregator
}

// Aggregator is a user-defined aggregation function called once per
// group during GroupBy.Agg. Implementations reduce the rows of a Series
// to a single scalar value.
//
// Typical implementations:
//
//	weighted mean, mode, percentile / quantile, log-sum-exp, first / last,
//	geospatial reductions (H3 cell of the centroid, dominant hex),
//	string aggregations (concat, longest common prefix).
//
// Return nil from Aggregate to emit an Arrow null for the group. The
// declared Type must match the concrete Go type Aggregate returns:
//
//	Float64 → float64        Uint64  → uint64      String    → string
//	Float32 → float32        Uint32  → uint32      Binary    → []byte
//	Int64   → int64          Boolean → bool        Timestamp → arrow.Timestamp
//	Int32   → int32
//
// If the returned dynamic type doesn't match Type, Agg reports an error
// naming the offending Aggregation.
//
// Aggregate is called sequentially per group; the same Aggregator
// instance is reused across groups. Implementations that need per-group
// scratch space should allocate it inside Aggregate rather than as
// receiver fields.
type Aggregator interface {
	Aggregate(s Series, rows []int) (any, error)
	Type() arrow.DataType
	Name() string
}

// GroupBy partitions a Frame by the values in one or more key columns. The
// keys must be of a hashable Arrow type (String, Int64, Int32, Bool, Float64).
type GroupBy struct {
	frame *Frame
	keys  []Series
}

// GroupBy returns a GroupBy over the given key column names.
func (f *Frame) GroupBy(keys ...string) (*GroupBy, error) {
	if len(keys) == 0 {
		return nil, fmt.Errorf("gobi: GroupBy requires at least one key column")
	}
	ks := make([]Series, len(keys))
	for i, k := range keys {
		s, err := f.Column(k)
		if err != nil {
			return nil, err
		}
		if !isHashable(s.DataType()) {
			return nil, fmt.Errorf("gobi: key column %q of type %s is not hashable",
				k, s.DataType())
		}
		ks[i] = s
	}
	return &GroupBy{frame: f, keys: ks}, nil
}

// Agg computes the requested aggregations over each group, returning a Frame
// whose first N columns are the group keys (in the order passed to GroupBy)
// and whose remaining columns are the aggregations in order.
//
// When the group-by uses exactly one hashable key column with a single
// Arrow chunk and every aggregation column is a single-chunk primitive
// numeric type, the fast path (aggFast) is taken — it avoids the per-row
// byte-slice hashing and chunk walks that dominate the general path. All
// other shapes fall back to the multi-key path below.
func (g *GroupBy) Agg(aggs ...Aggregation) (*Frame, error) {
	if f, ok, err := g.aggFast(aggs); err != nil {
		return nil, err
	} else if ok {
		return f, nil
	}
	// Compute a stable, deterministic order over group keys: build a
	// canonical string for each row, then sort the unique ones.
	rowCount := g.frame.NumRows()
	rowKeys := make([]string, rowCount)
	for row := range rowCount {
		k, err := g.rowKey(row)
		if err != nil {
			return nil, err
		}
		rowKeys[row] = k
	}
	groups := map[string][]int{}
	var order []string
	for row, k := range rowKeys {
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		groups[k] = append(groups[k], row)
	}
	sort.Strings(order)

	pool := memory.DefaultAllocator
	keyBuilders, err := makeKeyBuilders(pool, g.keys)
	if err != nil {
		return nil, err
	}
	defer releaseBuilders(keyBuilders)

	// Aggregation output builders.
	aggBuilders := make([]array.Builder, len(aggs))
	aggFields := make([]arrow.Field, len(aggs))
	for i, a := range aggs {
		if a.Fn != nil {
			if _, err := g.frame.Column(a.Column); err != nil {
				return nil, err
			}
			b, err := builderForType(pool, a.Fn.Type())
			if err != nil {
				return nil, fmt.Errorf("gobi: aggregation %d (%s): %w",
					i, aggName(a), err)
			}
			aggBuilders[i] = b
			aggFields[i] = arrow.Field{
				Name: aggName(a), Type: a.Fn.Type(), Nullable: true,
			}
			continue
		}
		if a.Kind == AggCount {
			aggBuilders[i] = array.NewInt64Builder(pool)
			aggFields[i] = arrow.Field{
				Name: aggName(a), Type: arrow.PrimitiveTypes.Int64, Nullable: false,
			}
			continue
		}
		if _, err := g.frame.Column(a.Column); err != nil {
			return nil, err
		}
		aggBuilders[i] = array.NewFloat64Builder(pool)
		aggFields[i] = arrow.Field{
			Name: aggName(a), Type: arrow.PrimitiveTypes.Float64, Nullable: true,
		}
	}
	defer releaseBuilders(aggBuilders)

	// For each group, emit keys + aggregations.
	for _, gk := range order {
		rows := groups[gk]
		if err := appendKeyRow(keyBuilders, g.keys, rows[0]); err != nil {
			return nil, err
		}
		for i, a := range aggs {
			if err := g.appendAgg(aggBuilders[i], a, rows); err != nil {
				return nil, err
			}
		}
	}

	// Build the output frame.
	keyFields := make([]arrow.Field, len(g.keys))
	for i, k := range g.keys {
		keyFields[i] = arrow.Field{Name: k.name, Type: k.DataType(), Nullable: false}
	}
	fields := append(append([]arrow.Field{}, keyFields...), aggFields...)
	schema := arrow.NewSchema(fields, nil)

	arrays := make([]arrow.Array, 0, len(fields))
	for _, b := range keyBuilders {
		arrays = append(arrays, b.NewArray())
	}
	for _, b := range aggBuilders {
		arrays = append(arrays, b.NewArray())
	}
	defer func() {
		for _, a := range arrays {
			a.Release()
		}
	}()

	cols := make([]arrow.Column, len(fields))
	for i, a := range arrays {
		chunked := arrow.NewChunked(a.DataType(), []arrow.Array{a})
		cols[i] = *arrow.NewColumn(fields[i], chunked)
	}
	return NewFrame(schema, cols)
}

func aggName(a Aggregation) string {
	if a.Alias != "" {
		return a.Alias
	}
	if a.Fn != nil {
		return fmt.Sprintf("%s_%s", a.Column, a.Fn.Name())
	}
	if a.Kind == AggCount && a.Column == "" {
		return "count"
	}
	return fmt.Sprintf("%s_%s", a.Column, a.Kind)
}

// builderForType returns an empty Arrow builder matching t. Types not
// listed here are rejected by custom aggregators — callers should widen
// Aggregator.Type() to one of the supported outputs.
func builderForType(pool memory.Allocator, t arrow.DataType) (array.Builder, error) {
	switch t.ID() {
	case arrow.FLOAT64:
		return array.NewFloat64Builder(pool), nil
	case arrow.FLOAT32:
		return array.NewFloat32Builder(pool), nil
	case arrow.INT64:
		return array.NewInt64Builder(pool), nil
	case arrow.INT32:
		return array.NewInt32Builder(pool), nil
	case arrow.UINT64:
		return array.NewUint64Builder(pool), nil
	case arrow.UINT32:
		return array.NewUint32Builder(pool), nil
	case arrow.BOOL:
		return array.NewBooleanBuilder(pool), nil
	case arrow.STRING:
		return array.NewStringBuilder(pool), nil
	case arrow.BINARY:
		return array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary), nil
	case arrow.TIMESTAMP:
		return array.NewTimestampBuilder(pool, t.(*arrow.TimestampType)), nil
	default:
		return nil, fmt.Errorf("unsupported Aggregator output type %s", t)
	}
}

// appendCustomValue appends v to b, matching v's dynamic Go type against
// b's arrow builder type. Returns an error naming the mismatch if the
// types don't align — this is how misdeclared Aggregator.Type() surfaces
// at aggregation time rather than as a silent panic.
func appendCustomValue(b array.Builder, v any) error {
	if v == nil {
		b.AppendNull()
		return nil
	}
	switch tb := b.(type) {
	case *array.Float64Builder:
		x, ok := v.(float64)
		if !ok {
			return fmt.Errorf("value %T does not match declared Float64", v)
		}
		tb.Append(x)
	case *array.Float32Builder:
		x, ok := v.(float32)
		if !ok {
			return fmt.Errorf("value %T does not match declared Float32", v)
		}
		tb.Append(x)
	case *array.Int64Builder:
		x, ok := v.(int64)
		if !ok {
			return fmt.Errorf("value %T does not match declared Int64", v)
		}
		tb.Append(x)
	case *array.Int32Builder:
		x, ok := v.(int32)
		if !ok {
			return fmt.Errorf("value %T does not match declared Int32", v)
		}
		tb.Append(x)
	case *array.Uint64Builder:
		x, ok := v.(uint64)
		if !ok {
			return fmt.Errorf("value %T does not match declared Uint64", v)
		}
		tb.Append(x)
	case *array.Uint32Builder:
		x, ok := v.(uint32)
		if !ok {
			return fmt.Errorf("value %T does not match declared Uint32", v)
		}
		tb.Append(x)
	case *array.BooleanBuilder:
		x, ok := v.(bool)
		if !ok {
			return fmt.Errorf("value %T does not match declared Boolean", v)
		}
		tb.Append(x)
	case *array.StringBuilder:
		x, ok := v.(string)
		if !ok {
			return fmt.Errorf("value %T does not match declared String", v)
		}
		tb.Append(x)
	case *array.BinaryBuilder:
		x, ok := v.([]byte)
		if !ok {
			return fmt.Errorf("value %T does not match declared Binary", v)
		}
		tb.Append(x)
	case *array.TimestampBuilder:
		x, ok := v.(arrow.Timestamp)
		if !ok {
			return fmt.Errorf("value %T does not match declared Timestamp", v)
		}
		tb.Append(x)
	default:
		return fmt.Errorf("unhandled builder type %T", b)
	}
	return nil
}

func (g *GroupBy) rowKey(row int) (string, error) {
	var b []byte
	for i, s := range g.keys {
		if i > 0 {
			b = append(b, 0x1F)
		}
		v, err := keyOf(s, row)
		if err != nil {
			return "", err
		}
		b = append(b, v...)
	}
	return string(b), nil
}

func keyOf(s Series, row int) ([]byte, error) {
	offset := 0
	for _, chunk := range s.col.Data().Chunks() {
		if row < offset+chunk.Len() {
			local := row - offset
			if chunk.IsNull(local) {
				return []byte{0x00}, nil
			}
			switch a := chunk.(type) {
			case *array.String:
				return append([]byte{0x01}, []byte(a.Value(local))...), nil
			case *array.Int64:
				return append([]byte{0x02}, i64Bytes(a.Value(local))...), nil
			case *array.Int32:
				return append([]byte{0x03}, i64Bytes(int64(a.Value(local)))...), nil
			case *array.Boolean:
				if a.Value(local) {
					return []byte{0x04, 0x01}, nil
				}
				return []byte{0x04, 0x00}, nil
			case *array.Float64:
				return append([]byte{0x05}, i64Bytes(int64(a.Value(local)*1e9))...), nil
			case *array.Uint64:
				return append([]byte{0x06}, i64Bytes(int64(a.Value(local)))...), nil
			case *array.Uint32:
				return append([]byte{0x07}, i64Bytes(int64(a.Value(local)))...), nil
			case *array.Timestamp:
				return append([]byte{0x08}, i64Bytes(int64(a.Value(local)))...), nil
			default:
				return nil, fmt.Errorf("gobi: key type %T not hashable", chunk)
			}
		}
		offset += chunk.Len()
	}
	return nil, fmt.Errorf("%w: %d", ErrRowOutOfRange, row)
}

func i64Bytes(v int64) []byte {
	return []byte{
		byte(v >> 56), byte(v >> 48), byte(v >> 40), byte(v >> 32),
		byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v),
	}
}

func isHashable(t arrow.DataType) bool {
	switch t.ID() {
	case arrow.STRING, arrow.INT64, arrow.INT32, arrow.BOOL, arrow.FLOAT64,
		arrow.UINT64, arrow.UINT32, arrow.TIMESTAMP:
		return true
	default:
		return false
	}
}

func makeKeyBuilders(pool memory.Allocator, keys []Series) ([]array.Builder, error) {
	out := make([]array.Builder, len(keys))
	for i, k := range keys {
		switch dt := k.DataType(); dt.ID() {
		case arrow.STRING:
			out[i] = array.NewStringBuilder(pool)
		case arrow.INT64:
			out[i] = array.NewInt64Builder(pool)
		case arrow.INT32:
			out[i] = array.NewInt32Builder(pool)
		case arrow.BOOL:
			out[i] = array.NewBooleanBuilder(pool)
		case arrow.FLOAT64:
			out[i] = array.NewFloat64Builder(pool)
		case arrow.UINT64:
			out[i] = array.NewUint64Builder(pool)
		case arrow.UINT32:
			out[i] = array.NewUint32Builder(pool)
		case arrow.TIMESTAMP:
			out[i] = array.NewTimestampBuilder(pool, dt.(*arrow.TimestampType))
		default:
			return nil, fmt.Errorf("gobi: unsupported key type %s", dt)
		}
	}
	return out, nil
}

func releaseBuilders(bs []array.Builder) {
	for _, b := range bs {
		b.Release()
	}
}

func appendKeyRow(builders []array.Builder, keys []Series, row int) error {
	for i, s := range keys {
		if err := appendPrimitiveAt(s, row, builders[i]); err != nil {
			return err
		}
	}
	return nil
}

func (g *GroupBy) appendAgg(b array.Builder, agg Aggregation, rows []int) error {
	if agg.Fn != nil {
		s, err := g.frame.Column(agg.Column)
		if err != nil {
			return err
		}
		v, err := agg.Fn.Aggregate(s, rows)
		if err != nil {
			return fmt.Errorf("gobi: aggregation %s: %w", aggName(agg), err)
		}
		if err := appendCustomValue(b, v); err != nil {
			return fmt.Errorf("gobi: aggregation %s: %w", aggName(agg), err)
		}
		return nil
	}
	if agg.Kind == AggCount {
		if agg.Column == "" {
			b.(*array.Int64Builder).Append(int64(len(rows)))
			return nil
		}
		s, err := g.frame.Column(agg.Column)
		if err != nil {
			return err
		}
		var n int64
		for _, row := range rows {
			_, ok, err := s.numericAt(row)
			if err != nil {
				return err
			}
			if ok {
				n++
			}
		}
		b.(*array.Int64Builder).Append(n)
		return nil
	}
	s, err := g.frame.Column(agg.Column)
	if err != nil {
		return err
	}
	if !s.isNumeric() {
		return fmt.Errorf("%w: %s", ErrNotNumeric, agg.Column)
	}
	var (
		sum, minV, maxV float64
		n               int
	)
	for _, row := range rows {
		v, ok, err := s.numericAt(row)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		if n == 0 {
			minV, maxV = v, v
		} else {
			if v < minV {
				minV = v
			}
			if v > maxV {
				maxV = v
			}
		}
		sum += v
		n++
	}
	fb := b.(*array.Float64Builder)
	if n == 0 {
		fb.AppendNull()
		return nil
	}
	switch agg.Kind {
	case AggSum:
		fb.Append(sum)
	case AggMean:
		fb.Append(sum / float64(n))
	case AggMin:
		fb.Append(minV)
	case AggMax:
		fb.Append(maxV)
	default:
		return fmt.Errorf("gobi: unknown aggregation kind %d", agg.Kind)
	}
	return nil
}
