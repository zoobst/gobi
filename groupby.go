package gobi

import (
	"fmt"
	"sort"

	"github.com/apache/arrow/go/v18/arrow"
	"github.com/apache/arrow/go/v18/arrow/array"
	"github.com/apache/arrow/go/v18/arrow/memory"
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

// Aggregation names a column to aggregate and how to aggregate it. The
// resulting column in the aggregated frame is named Alias (or, if empty,
// "<column>_<kind>").
type Aggregation struct {
	Column string
	Kind   AggKind
	Alias  string
}

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
	if a.Kind == AggCount && a.Column == "" {
		return "count"
	}
	return fmt.Sprintf("%s_%s", a.Column, a.Kind)
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
	case arrow.STRING, arrow.INT64, arrow.INT32, arrow.BOOL, arrow.FLOAT64:
		return true
	default:
		return false
	}
}

func makeKeyBuilders(pool memory.Allocator, keys []Series) ([]array.Builder, error) {
	out := make([]array.Builder, len(keys))
	for i, k := range keys {
		switch k.DataType().ID() {
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
		default:
			return nil, fmt.Errorf("gobi: unsupported key type %s", k.DataType())
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
