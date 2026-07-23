package gobi

import (
	"fmt"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// -----------------------------------------------------------------------------
// Fluent constructors
// -----------------------------------------------------------------------------

// ListLen returns an expression that evaluates each row's list length.
// e must produce a List column. Null lists → null; empty lists → 0.
func (e Expr) ListLen() Expr {
	return Expr{node: &listLenNode{inner: e.node}}
}

// ListGet returns an expression that reads the element at index i
// from each row's list. Negative i counts from the end (-1 = last).
// Out-of-range indices, null lists, and null elements all evaluate to
// null. The result type is the list's element type.
func (e Expr) ListGet(i int) Expr {
	return Expr{node: &listGetNode{inner: e.node, index: i}}
}

// ListSlice returns an expression that slices each row's list to
// [start, stop). Python-style semantics: negative indices count from
// the end; out-of-range endpoints clamp to the list bounds. The
// result is still a List column.
func (e Expr) ListSlice(start, stop int) Expr {
	return Expr{node: &listSliceNode{inner: e.node, start: start, stop: stop}}
}

// ListContains returns an expression that reports whether each row's
// list contains elem. Null lists → null; otherwise Boolean. elem is a
// Go scalar (bool, int/int64, float32/float64, string) matching the
// list's element type.
func (e Expr) ListContains(elem any) Expr {
	return Expr{node: &listContainsNode{inner: e.node, elem: elem}}
}

// ListSum returns an expression that sums each row's list elements.
// Null lists and empty lists produce null. Null elements are skipped
// (polars-parity). Signed integer element types widen to Int64;
// unsigned to Uint64; floats to Float64.
func (e Expr) ListSum() Expr { return Expr{node: &listAggNode{inner: e.node, kind: lakSum}} }

// ListMean returns the arithmetic mean of each row's non-null list
// elements. Always Float64. Null / empty lists produce null.
func (e Expr) ListMean() Expr { return Expr{node: &listAggNode{inner: e.node, kind: lakMean}} }

// ListMin returns the minimum non-null element of each row's list.
// Output widens to Int64 / Uint64 / Float64 per element category.
// Null / empty lists produce null.
func (e Expr) ListMin() Expr { return Expr{node: &listAggNode{inner: e.node, kind: lakMin}} }

// ListMax returns the maximum non-null element of each row's list.
// Output widens like ListMin.
func (e Expr) ListMax() Expr { return Expr{node: &listAggNode{inner: e.node, kind: lakMax}} }

// ListFirst returns the first element of each row's list. Alias for
// ListGet(0) — kept as a distinct constructor to match polars/pandas
// naming conventions.
func (e Expr) ListFirst() Expr { return e.ListGet(0) }

// ListLast returns the last element of each row's list. Alias for
// ListGet(-1).
func (e Expr) ListLast() Expr { return e.ListGet(-1) }

// -----------------------------------------------------------------------------
// listLenNode
// -----------------------------------------------------------------------------

type listLenNode struct {
	inner ExprNode
}

func (n *listLenNode) Eval(input *Frame) (Series, error) {
	s, err := n.inner.Eval(input)
	if err != nil {
		return Series{}, err
	}
	if s.DataType().ID() != arrow.LIST {
		return Series{}, fmt.Errorf("%w: list_len requires List column, got %s",
			ErrExprTypeMismatch, s.DataType())
	}
	b := array.NewInt64Builder(memory.DefaultAllocator)
	defer b.Release()
	for _, chunk := range s.col.Data().Chunks() {
		la, ok := chunk.(*array.List)
		if !ok {
			return Series{}, fmt.Errorf("list column chunk not *array.List (%T)", chunk)
		}
		for i := 0; i < la.Len(); i++ {
			if la.IsNull(i) {
				b.AppendNull()
				continue
			}
			start, end := la.ValueOffsets(i)
			b.Append(end - start)
		}
	}
	return buildSeries("list_len", arrow.PrimitiveTypes.Int64, b.NewArray()), nil
}

func (n *listLenNode) Type(schema *arrow.Schema) (arrow.DataType, error) {
	t, err := n.inner.Type(schema)
	if err != nil {
		return nil, err
	}
	if t.ID() != arrow.LIST {
		return nil, fmt.Errorf("%w: list_len requires List column, got %s",
			ErrExprTypeMismatch, t)
	}
	return arrow.PrimitiveTypes.Int64, nil
}

func (n *listLenNode) Children() []Expr { return []Expr{{node: n.inner}} }
func (n *listLenNode) String() string   { return fmt.Sprintf("list_len(%s)", n.inner) }

// -----------------------------------------------------------------------------
// listGetNode
// -----------------------------------------------------------------------------

type listGetNode struct {
	inner ExprNode
	index int
}

func (n *listGetNode) Eval(input *Frame) (Series, error) {
	s, err := n.inner.Eval(input)
	if err != nil {
		return Series{}, err
	}
	lt, ok := s.DataType().(*arrow.ListType)
	if !ok {
		return Series{}, fmt.Errorf("%w: list_get requires List column, got %s",
			ErrExprTypeMismatch, s.DataType())
	}
	pool := memory.DefaultAllocator
	b, err := builderForType(pool, lt.Elem())
	if err != nil {
		return Series{}, fmt.Errorf("list_get: %w", err)
	}
	defer b.Release()
	for _, chunk := range s.col.Data().Chunks() {
		la, ok := chunk.(*array.List)
		if !ok {
			return Series{}, fmt.Errorf("list column chunk not *array.List (%T)", chunk)
		}
		values := la.ListValues()
		for i := 0; i < la.Len(); i++ {
			if la.IsNull(i) {
				b.AppendNull()
				continue
			}
			start, end := la.ValueOffsets(i)
			length := int(end - start)
			idx := n.index
			if idx < 0 {
				idx += length
			}
			if idx < 0 || idx >= length {
				b.AppendNull()
				continue
			}
			abs := int(start) + idx
			if values.IsNull(abs) {
				b.AppendNull()
				continue
			}
			if err := appendArrayValueAt(b, values, abs); err != nil {
				return Series{}, fmt.Errorf("list_get elem: %w", err)
			}
		}
	}
	return buildSeries("list_get", lt.Elem(), b.NewArray()), nil
}

func (n *listGetNode) Type(schema *arrow.Schema) (arrow.DataType, error) {
	t, err := n.inner.Type(schema)
	if err != nil {
		return nil, err
	}
	lt, ok := t.(*arrow.ListType)
	if !ok {
		return nil, fmt.Errorf("%w: list_get requires List column, got %s",
			ErrExprTypeMismatch, t)
	}
	return lt.Elem(), nil
}

func (n *listGetNode) Children() []Expr { return []Expr{{node: n.inner}} }
func (n *listGetNode) String() string   { return fmt.Sprintf("list_get(%s, %d)", n.inner, n.index) }

// -----------------------------------------------------------------------------
// listSliceNode
// -----------------------------------------------------------------------------

type listSliceNode struct {
	inner       ExprNode
	start, stop int
}

func (n *listSliceNode) Eval(input *Frame) (Series, error) {
	s, err := n.inner.Eval(input)
	if err != nil {
		return Series{}, err
	}
	lt, ok := s.DataType().(*arrow.ListType)
	if !ok {
		return Series{}, fmt.Errorf("%w: list_slice requires List column, got %s",
			ErrExprTypeMismatch, s.DataType())
	}
	pool := memory.DefaultAllocator
	lb := array.NewListBuilder(pool, lt.Elem())
	defer lb.Release()
	inner := lb.ValueBuilder()
	for _, chunk := range s.col.Data().Chunks() {
		la, ok := chunk.(*array.List)
		if !ok {
			return Series{}, fmt.Errorf("list column chunk not *array.List (%T)", chunk)
		}
		values := la.ListValues()
		for i := range la.Len() {
			if la.IsNull(i) {
				lb.AppendNull()
				continue
			}
			startOff, endOff := la.ValueOffsets(i)
			length := int(endOff - startOff)
			lo, hi := clampSlice(n.start, n.stop, length)
			lb.Append(true)
			for j := lo; j < hi; j++ {
				abs := int(startOff) + j
				if values.IsNull(abs) {
					inner.AppendNull()
					continue
				}
				if err := appendArrayValueAt(inner, values, abs); err != nil {
					return Series{}, fmt.Errorf("list_slice elem: %w", err)
				}
			}
		}
	}
	return buildSeries("list_slice", arrow.ListOf(lt.Elem()), lb.NewArray()), nil
}

func (n *listSliceNode) Type(schema *arrow.Schema) (arrow.DataType, error) {
	t, err := n.inner.Type(schema)
	if err != nil {
		return nil, err
	}
	if t.ID() != arrow.LIST {
		return nil, fmt.Errorf("%w: list_slice requires List column, got %s",
			ErrExprTypeMismatch, t)
	}
	return t, nil
}

func (n *listSliceNode) Children() []Expr { return []Expr{{node: n.inner}} }
func (n *listSliceNode) String() string {
	return fmt.Sprintf("list_slice(%s, %d, %d)", n.inner, n.start, n.stop)
}

// clampSlice translates Python-style [start:stop) with negative-index
// support into a validated [lo, hi) range within [0, length].
func clampSlice(start, stop, length int) (int, int) {
	if start < 0 {
		start += length
	}
	if stop < 0 {
		stop += length
	}
	if start < 0 {
		start = 0
	}
	if stop > length {
		stop = length
	}
	if stop < start {
		stop = start
	}
	return start, stop
}

// -----------------------------------------------------------------------------
// listContainsNode
// -----------------------------------------------------------------------------

type listContainsNode struct {
	inner ExprNode
	elem  any
}

func (n *listContainsNode) Eval(input *Frame) (Series, error) {
	s, err := n.inner.Eval(input)
	if err != nil {
		return Series{}, err
	}
	if _, ok := s.DataType().(*arrow.ListType); !ok {
		return Series{}, fmt.Errorf("%w: list_contains requires List column, got %s",
			ErrExprTypeMismatch, s.DataType())
	}
	b := array.NewBooleanBuilder(memory.DefaultAllocator)
	defer b.Release()
	for _, chunk := range s.col.Data().Chunks() {
		la, ok := chunk.(*array.List)
		if !ok {
			return Series{}, fmt.Errorf("list column chunk not *array.List (%T)", chunk)
		}
		values := la.ListValues()
		for i := range la.Len() {
			if la.IsNull(i) {
				b.AppendNull()
				continue
			}
			startOff, endOff := la.ValueOffsets(i)
			found := false
			for j := int(startOff); j < int(endOff); j++ {
				if values.IsNull(j) {
					continue
				}
				eq, err := arrayValueEquals(values, j, n.elem)
				if err != nil {
					return Series{}, fmt.Errorf("list_contains: %w", err)
				}
				if eq {
					found = true
					break
				}
			}
			b.Append(found)
		}
	}
	return buildSeries("list_contains", arrow.FixedWidthTypes.Boolean, b.NewArray()), nil
}

func (n *listContainsNode) Type(schema *arrow.Schema) (arrow.DataType, error) {
	t, err := n.inner.Type(schema)
	if err != nil {
		return nil, err
	}
	if t.ID() != arrow.LIST {
		return nil, fmt.Errorf("%w: list_contains requires List column, got %s",
			ErrExprTypeMismatch, t)
	}
	return arrow.FixedWidthTypes.Boolean, nil
}

func (n *listContainsNode) Children() []Expr { return []Expr{{node: n.inner}} }
func (n *listContainsNode) String() string {
	return fmt.Sprintf("list_contains(%s, %v)", n.inner, n.elem)
}

// -----------------------------------------------------------------------------
// Shared helpers
// -----------------------------------------------------------------------------

// buildSeries wraps a freshly-built arrow.Array in a Series. The Array
// is Released after being installed into a chunked column, so the
// caller shouldn't touch it after this returns.
func buildSeries(name string, dtype arrow.DataType, arr arrow.Array) Series {
	field := arrow.Field{Name: name, Type: dtype, Nullable: true}
	chunked := arrow.NewChunked(dtype, []arrow.Array{arr})
	col := arrow.NewColumn(field, chunked)
	arr.Release()
	chunked.Release()
	return NewSeries(col)
}

// arrayValueEquals reports whether arr[idx] equals elem, using
// arrow-typed comparison for the common scalar types. int/int64
// literals compare against every integer width; float32 promotes to
// float64.
func arrayValueEquals(arr arrow.Array, idx int, elem any) (bool, error) {
	switch a := arr.(type) {
	case *array.String:
		s, ok := elem.(string)
		if !ok {
			return false, fmt.Errorf("expected string, got %T", elem)
		}
		return a.Value(idx) == s, nil
	case *array.Boolean:
		b, ok := elem.(bool)
		if !ok {
			return false, fmt.Errorf("expected bool, got %T", elem)
		}
		return a.Value(idx) == b, nil
	case *array.Int64:
		v, ok := toInt64(elem)
		if !ok {
			return false, fmt.Errorf("expected int, got %T", elem)
		}
		return a.Value(idx) == v, nil
	case *array.Int32:
		v, ok := toInt64(elem)
		if !ok {
			return false, fmt.Errorf("expected int, got %T", elem)
		}
		return int64(a.Value(idx)) == v, nil
	case *array.Int16:
		v, ok := toInt64(elem)
		if !ok {
			return false, fmt.Errorf("expected int, got %T", elem)
		}
		return int64(a.Value(idx)) == v, nil
	case *array.Int8:
		v, ok := toInt64(elem)
		if !ok {
			return false, fmt.Errorf("expected int, got %T", elem)
		}
		return int64(a.Value(idx)) == v, nil
	case *array.Uint64:
		v, ok := toUint64(elem)
		if !ok {
			return false, fmt.Errorf("expected uint, got %T", elem)
		}
		return a.Value(idx) == v, nil
	case *array.Uint32:
		v, ok := toUint64(elem)
		if !ok {
			return false, fmt.Errorf("expected uint, got %T", elem)
		}
		return uint64(a.Value(idx)) == v, nil
	case *array.Uint16:
		v, ok := toUint64(elem)
		if !ok {
			return false, fmt.Errorf("expected uint, got %T", elem)
		}
		return uint64(a.Value(idx)) == v, nil
	case *array.Uint8:
		v, ok := toUint64(elem)
		if !ok {
			return false, fmt.Errorf("expected uint, got %T", elem)
		}
		return uint64(a.Value(idx)) == v, nil
	case *array.Float64:
		v, ok := toFloat64(elem)
		if !ok {
			return false, fmt.Errorf("expected float, got %T", elem)
		}
		return a.Value(idx) == v, nil
	case *array.Float32:
		v, ok := toFloat64(elem)
		if !ok {
			return false, fmt.Errorf("expected float, got %T", elem)
		}
		return float64(a.Value(idx)) == v, nil
	}
	return false, fmt.Errorf("unsupported list element array %T", arr)
}

func toInt64(v any) (int64, bool) {
	switch x := v.(type) {
	case int:
		return int64(x), true
	case int8:
		return int64(x), true
	case int16:
		return int64(x), true
	case int32:
		return int64(x), true
	case int64:
		return x, true
	}
	return 0, false
}

func toUint64(v any) (uint64, bool) {
	switch x := v.(type) {
	case uint:
		return uint64(x), true
	case uint8:
		return uint64(x), true
	case uint16:
		return uint64(x), true
	case uint32:
		return uint64(x), true
	case uint64:
		return x, true
	}
	return 0, false
}

// -----------------------------------------------------------------------------
// listAggNode — per-element reductions: sum/mean/min/max
// -----------------------------------------------------------------------------

type listAggKind uint8

const (
	lakSum listAggKind = iota
	lakMean
	lakMin
	lakMax
)

func (k listAggKind) String() string {
	switch k {
	case lakSum:
		return "list_sum"
	case lakMean:
		return "list_mean"
	case lakMin:
		return "list_min"
	case lakMax:
		return "list_max"
	}
	return fmt.Sprintf("list_agg(%d)", k)
}

type listAggNode struct {
	inner ExprNode
	kind  listAggKind
}

func (n *listAggNode) Eval(input *Frame) (Series, error) {
	s, err := n.inner.Eval(input)
	if err != nil {
		return Series{}, err
	}
	lt, ok := s.DataType().(*arrow.ListType)
	if !ok {
		return Series{}, fmt.Errorf("%w: %s requires List column, got %s",
			ErrExprTypeMismatch, n.kind, s.DataType())
	}
	outType, err := listAggOutputType(n.kind, lt.Elem())
	if err != nil {
		return Series{}, err
	}
	pool := memory.DefaultAllocator
	b, err := builderForType(pool, outType)
	if err != nil {
		return Series{}, fmt.Errorf("%s: %w", n.kind, err)
	}
	defer b.Release()

	for _, chunk := range s.col.Data().Chunks() {
		la, ok := chunk.(*array.List)
		if !ok {
			return Series{}, fmt.Errorf("list column chunk not *array.List (%T)", chunk)
		}
		values := la.ListValues()
		for i := 0; i < la.Len(); i++ {
			if la.IsNull(i) {
				b.AppendNull()
				continue
			}
			start, end := la.ValueOffsets(i)
			if start == end {
				// Empty list — aggregate-of-empty-set is null.
				b.AppendNull()
				continue
			}
			if err := reduceListRow(b, n.kind, values, int(start), int(end)); err != nil {
				return Series{}, err
			}
		}
	}
	return buildSeries(n.kind.String(), outType, b.NewArray()), nil
}

func (n *listAggNode) Type(schema *arrow.Schema) (arrow.DataType, error) {
	t, err := n.inner.Type(schema)
	if err != nil {
		return nil, err
	}
	lt, ok := t.(*arrow.ListType)
	if !ok {
		return nil, fmt.Errorf("%w: %s requires List column, got %s",
			ErrExprTypeMismatch, n.kind, t)
	}
	return listAggOutputType(n.kind, lt.Elem())
}

func (n *listAggNode) Children() []Expr { return []Expr{{node: n.inner}} }
func (n *listAggNode) String() string   { return fmt.Sprintf("%s(%s)", n.kind, n.inner) }

// listAggOutputType picks the widened output type for a per-element
// aggregation. Signed integers widen to Int64; unsigned to Uint64;
// floats stay Float64. Mean is always Float64.
func listAggOutputType(kind listAggKind, elem arrow.DataType) (arrow.DataType, error) {
	if kind == lakMean {
		if !isNumericArrowType(elem) {
			return nil, fmt.Errorf("%w: list_mean requires numeric elements, got %s",
				ErrExprTypeMismatch, elem)
		}
		return arrow.PrimitiveTypes.Float64, nil
	}
	switch elem.ID() {
	case arrow.INT8, arrow.INT16, arrow.INT32, arrow.INT64:
		return arrow.PrimitiveTypes.Int64, nil
	case arrow.UINT8, arrow.UINT16, arrow.UINT32, arrow.UINT64:
		return arrow.PrimitiveTypes.Uint64, nil
	case arrow.FLOAT32, arrow.FLOAT64:
		return arrow.PrimitiveTypes.Float64, nil
	}
	return nil, fmt.Errorf("%w: %s requires numeric elements, got %s",
		ErrExprTypeMismatch, kind, elem)
}

func isNumericArrowType(t arrow.DataType) bool {
	switch t.ID() {
	case arrow.INT8, arrow.INT16, arrow.INT32, arrow.INT64,
		arrow.UINT8, arrow.UINT16, arrow.UINT32, arrow.UINT64,
		arrow.FLOAT32, arrow.FLOAT64:
		return true
	}
	return false
}

// reduceListRow walks values[start:end] (skipping nulls) and appends
// the aggregate to b. If every element is null, appends null.
func reduceListRow(b array.Builder, kind listAggKind, values arrow.Array, start, end int) error {
	if getter := intGetter(values); getter != nil {
		return reduceIntRow(b, kind, getter, values, start, end)
	}
	if getter := uintGetter(values); getter != nil {
		return reduceUintRow(b, kind, getter, values, start, end)
	}
	if getter := floatGetter(values); getter != nil {
		return reduceFloatRow(b, kind, getter, values, start, end)
	}
	return fmt.Errorf("%w: %s unsupported element type %T", ErrExprTypeMismatch, kind, values)
}

func intGetter(arr arrow.Array) func(int) int64 {
	switch a := arr.(type) {
	case *array.Int64:
		return func(i int) int64 { return a.Value(i) }
	case *array.Int32:
		return func(i int) int64 { return int64(a.Value(i)) }
	case *array.Int16:
		return func(i int) int64 { return int64(a.Value(i)) }
	case *array.Int8:
		return func(i int) int64 { return int64(a.Value(i)) }
	}
	return nil
}

func uintGetter(arr arrow.Array) func(int) uint64 {
	switch a := arr.(type) {
	case *array.Uint64:
		return func(i int) uint64 { return a.Value(i) }
	case *array.Uint32:
		return func(i int) uint64 { return uint64(a.Value(i)) }
	case *array.Uint16:
		return func(i int) uint64 { return uint64(a.Value(i)) }
	case *array.Uint8:
		return func(i int) uint64 { return uint64(a.Value(i)) }
	}
	return nil
}

func floatGetter(arr arrow.Array) func(int) float64 {
	switch a := arr.(type) {
	case *array.Float64:
		return func(i int) float64 { return a.Value(i) }
	case *array.Float32:
		return func(i int) float64 { return float64(a.Value(i)) }
	}
	return nil
}

func reduceIntRow(b array.Builder, kind listAggKind, getter func(int) int64, values arrow.Array, start, end int) error {
	var (
		sum   int64
		count int
		minV  int64
		maxV  int64
	)
	for j := start; j < end; j++ {
		if values.IsNull(j) {
			continue
		}
		v := getter(j)
		if count == 0 {
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
		count++
	}
	if count == 0 {
		b.AppendNull()
		return nil
	}
	switch kind {
	case lakSum:
		b.(*array.Int64Builder).Append(sum)
	case lakMean:
		b.(*array.Float64Builder).Append(float64(sum) / float64(count))
	case lakMin:
		b.(*array.Int64Builder).Append(minV)
	case lakMax:
		b.(*array.Int64Builder).Append(maxV)
	default:
		return fmt.Errorf("%w: %s not handled for int list", ErrExprTypeMismatch, kind)
	}
	return nil
}

func reduceUintRow(b array.Builder, kind listAggKind, getter func(int) uint64, values arrow.Array, start, end int) error {
	var (
		sum   uint64
		count int
		minV  uint64
		maxV  uint64
	)
	for j := start; j < end; j++ {
		if values.IsNull(j) {
			continue
		}
		v := getter(j)
		if count == 0 {
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
		count++
	}
	if count == 0 {
		b.AppendNull()
		return nil
	}
	switch kind {
	case lakSum:
		b.(*array.Uint64Builder).Append(sum)
	case lakMean:
		b.(*array.Float64Builder).Append(float64(sum) / float64(count))
	case lakMin:
		b.(*array.Uint64Builder).Append(minV)
	case lakMax:
		b.(*array.Uint64Builder).Append(maxV)
	default:
		return fmt.Errorf("%w: %s not handled for uint list", ErrExprTypeMismatch, kind)
	}
	return nil
}

func reduceFloatRow(b array.Builder, kind listAggKind, getter func(int) float64, values arrow.Array, start, end int) error {
	var (
		sum   float64
		count int
		minV  float64
		maxV  float64
	)
	for j := start; j < end; j++ {
		if values.IsNull(j) {
			continue
		}
		v := getter(j)
		if count == 0 {
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
		count++
	}
	if count == 0 {
		b.AppendNull()
		return nil
	}
	fb := b.(*array.Float64Builder)
	switch kind {
	case lakSum:
		fb.Append(sum)
	case lakMean:
		fb.Append(sum / float64(count))
	case lakMin:
		fb.Append(minV)
	case lakMax:
		fb.Append(maxV)
	default:
		return fmt.Errorf("%w: %s not handled for float list", ErrExprTypeMismatch, kind)
	}
	return nil
}
