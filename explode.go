package gobi

import (
	"fmt"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/zoobst/gobi/geometry"
)

// Explode returns a new Frame where each row of col has been split into
// one row per component.
//
// Two column shapes are accepted:
//
//   - Geometry column: multi-part geometries (MultiPoint, MultiLineString,
//     MultiPolygon, GeometryCollection) expand to one row per contained
//     geometry. Single geometries pass through unchanged. Null rows are
//     kept as a single null row.
//   - List<T> column: each element becomes its own row. The exploded
//     column's arrow type becomes the list's element type. Null lists
//     and empty lists both produce a single output row with a null
//     value in the exploded column (polars-parity).
//
// All other columns are duplicated across the exploded rows so per-row
// attributes propagate to every component.
func (f *Frame) Explode(col string) (*Frame, error) {
	s, err := f.Column(col)
	if err != nil {
		return nil, err
	}
	if s.IsGeometry() {
		return f.explodeGeometry(s, col)
	}
	if s.DataType().ID() == arrow.LIST {
		return f.explodeList(s, col)
	}
	return nil, fmt.Errorf("%w: %q is not a geometry or list column", ErrNotGeometry, col)
}

// explodeGeometry handles the multi-geometry expansion path.
func (f *Frame) explodeGeometry(s Series, geomCol string) (*Frame, error) {
	// First pass: build the parent-row-index → component-geometry mapping.
	// We collect (parentIdx, componentWKB) pairs, which then feed take-array
	// helpers to duplicate non-geometry columns.
	var (
		parentIdx  []int
		componentB = array.NewBinaryBuilder(memory.DefaultAllocator, arrow.BinaryTypes.Binary)
	)
	defer componentB.Release()

	rowIdx := 0
	for _, chunk := range s.col.Data().Chunks() {
		bin, ok := chunk.(*array.Binary)
		if !ok {
			return nil, fmt.Errorf("%w: geometry column not Binary (%T)",
				ErrColumnTypeMismatch, chunk)
		}
		for i := range bin.Len() {
			if bin.IsNull(i) {
				parentIdx = append(parentIdx, rowIdx)
				componentB.AppendNull()
				rowIdx++
				continue
			}
			g, err := geometry.ParseWKB(bin.Value(i))
			if err != nil {
				return nil, err
			}
			switch t := g.(type) {
			case geometry.MultiPoint:
				for _, p := range t.Points {
					parentIdx = append(parentIdx, rowIdx)
					componentB.Append(geometry.WKB(p))
				}
			case geometry.MultiLineString:
				for _, l := range t.Lines {
					parentIdx = append(parentIdx, rowIdx)
					componentB.Append(geometry.WKB(l))
				}
			case geometry.MultiPolygon:
				for _, p := range t.Polygons {
					parentIdx = append(parentIdx, rowIdx)
					componentB.Append(geometry.WKB(p))
				}
			case geometry.GeometryCollection:
				for _, inner := range t.Geometries {
					parentIdx = append(parentIdx, rowIdx)
					componentB.Append(geometry.WKB(inner))
				}
			default:
				// Point / LineString / Polygon — passthrough.
				parentIdx = append(parentIdx, rowIdx)
				componentB.Append(bin.Value(i))
			}
			rowIdx++
		}
	}

	pool := memory.DefaultAllocator
	componentArr := componentB.NewArray()
	defer componentArr.Release()

	outFields := make([]arrow.Field, 0, len(f.series))
	outCols := make([]arrow.Column, 0, len(f.series))
	for _, colS := range f.series {
		if colS.name == geomCol {
			chunked := arrow.NewChunked(componentArr.DataType(), []arrow.Array{componentArr})
			outFields = append(outFields, colS.field)
			outCols = append(outCols, *arrow.NewColumn(colS.field, chunked))
			continue
		}
		arr, err := takeArray(pool, colS, parentIdx)
		if err != nil {
			return nil, err
		}
		defer arr.Release()
		chunked := arrow.NewChunked(arr.DataType(), []arrow.Array{arr})
		outFields = append(outFields, colS.field)
		outCols = append(outCols, *arrow.NewColumn(colS.field, chunked))
	}

	schema := arrow.NewSchema(outFields, nil)
	return NewFrame(schema, outCols)
}

// explodeList handles the List<T> → T expansion path. Empty and null
// lists both produce a single null row (polars-parity).
func (f *Frame) explodeList(s Series, listCol string) (*Frame, error) {
	lt, ok := s.DataType().(*arrow.ListType)
	if !ok {
		return nil, fmt.Errorf("%w: list column %q not *arrow.ListType (%T)",
			ErrColumnTypeMismatch, listCol, s.DataType())
	}
	pool := memory.DefaultAllocator
	elemBuilder, err := builderForType(pool, lt.Elem())
	if err != nil {
		return nil, fmt.Errorf("explode list %q: %w", listCol, err)
	}
	defer elemBuilder.Release()

	var parentIdx []int
	rowIdx := 0
	for _, chunk := range s.col.Data().Chunks() {
		la, ok := chunk.(*array.List)
		if !ok {
			return nil, fmt.Errorf("%w: list column %q chunk not *array.List (%T)",
				ErrColumnTypeMismatch, listCol, chunk)
		}
		values := la.ListValues()
		for i := range la.Len() {
			if la.IsNull(i) {
				parentIdx = append(parentIdx, rowIdx)
				elemBuilder.AppendNull()
				rowIdx++
				continue
			}
			start, end := la.ValueOffsets(i)
			n := int(end - start)
			if n == 0 {
				// Empty list — polars-parity: one output row with null
				// element.
				parentIdx = append(parentIdx, rowIdx)
				elemBuilder.AppendNull()
				rowIdx++
				continue
			}
			for j := range n {
				idx := int(start) + j
				parentIdx = append(parentIdx, rowIdx)
				if values.IsNull(idx) {
					elemBuilder.AppendNull()
					continue
				}
				if err := appendArrayValueAt(elemBuilder, values, idx); err != nil {
					return nil, fmt.Errorf("explode list %q elem %d: %w", listCol, idx, err)
				}
			}
			rowIdx++
		}
	}

	elemArr := elemBuilder.NewArray()
	defer elemArr.Release()

	outFields := make([]arrow.Field, 0, len(f.series))
	outCols := make([]arrow.Column, 0, len(f.series))
	for _, colS := range f.series {
		if colS.name == listCol {
			// Replace the list field with a scalar element field.
			elemField := arrow.Field{Name: colS.name, Type: lt.Elem(), Nullable: true}
			chunked := arrow.NewChunked(elemArr.DataType(), []arrow.Array{elemArr})
			outFields = append(outFields, elemField)
			outCols = append(outCols, *arrow.NewColumn(elemField, chunked))
			continue
		}
		arr, err := takeArray(pool, colS, parentIdx)
		if err != nil {
			return nil, err
		}
		defer arr.Release()
		chunked := arrow.NewChunked(arr.DataType(), []arrow.Array{arr})
		outFields = append(outFields, colS.field)
		outCols = append(outCols, *arrow.NewColumn(colS.field, chunked))
	}

	schema := arrow.NewSchema(outFields, nil)
	return NewFrame(schema, outCols)
}

// appendArrayValueAt copies one non-null element from an arrow.Array
// into a matching Builder. Dispatch mirrors the read-side switch in
// from_structs.go's assignListElement.
func appendArrayValueAt(b array.Builder, arr arrow.Array, idx int) error {
	switch a := arr.(type) {
	case *array.String:
		b.(*array.StringBuilder).Append(a.Value(idx))
	case *array.Boolean:
		b.(*array.BooleanBuilder).Append(a.Value(idx))
	case *array.Int64:
		b.(*array.Int64Builder).Append(a.Value(idx))
	case *array.Int32:
		b.(*array.Int32Builder).Append(a.Value(idx))
	case *array.Int16:
		b.(*array.Int16Builder).Append(a.Value(idx))
	case *array.Int8:
		b.(*array.Int8Builder).Append(a.Value(idx))
	case *array.Uint64:
		b.(*array.Uint64Builder).Append(a.Value(idx))
	case *array.Uint32:
		b.(*array.Uint32Builder).Append(a.Value(idx))
	case *array.Uint16:
		b.(*array.Uint16Builder).Append(a.Value(idx))
	case *array.Uint8:
		b.(*array.Uint8Builder).Append(a.Value(idx))
	case *array.Float64:
		b.(*array.Float64Builder).Append(a.Value(idx))
	case *array.Float32:
		b.(*array.Float32Builder).Append(a.Value(idx))
	case *array.Binary:
		b.(*array.BinaryBuilder).Append(a.Value(idx))
	case *array.Timestamp:
		b.(*array.TimestampBuilder).Append(a.Value(idx))
	default:
		return fmt.Errorf("unsupported list element array type %T", arr)
	}
	return nil
}
