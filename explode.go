package gobi

import (
	"fmt"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/zoobst/gobi/geometry"
)

// Explode returns a new Frame where each multi-geometry row of geomCol has
// been split into one row per component. Single geometries (Point,
// LineString, Polygon) pass through unchanged. Null geometries are kept
// (one output row per null, unchanged). GeometryCollection is expanded to
// one row per contained geometry.
//
// All non-geometry columns are duplicated across the exploded rows so
// per-row attributes propagate to every component.
func (f *Frame) Explode(geomCol string) (*Frame, error) {
	s, err := f.Column(geomCol)
	if err != nil {
		return nil, err
	}
	if !s.IsGeometry() {
		return nil, fmt.Errorf("%w: %q is not a geometry column", ErrNotGeometry, geomCol)
	}

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
		for i := 0; i < bin.Len(); i++ {
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

	// Assemble the output frame column-by-column. For the geometry column,
	// use the freshly-built componentB array; for every other column, take
	// the values at parentIdx.
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
