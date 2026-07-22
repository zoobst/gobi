package gobi

import (
	"fmt"
	"sort"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// Pivot reshapes a long-form Frame into a wide-form one.
//
//   - index: column whose distinct values become the rows.
//   - columns: column whose distinct values become the new columns.
//   - values: column supplying the cell values.
//   - agg: how to reduce when multiple input rows map to the same
//     (index, columns) cell. Use AggFirst / AggLast if you're sure
//     there's no collision; use AggSum / AggMean / etc. to reduce.
//     Any built-in AggKind is accepted.
//
// The output schema is:
//
//	<index column> | <col_value_1> | <col_value_2> | ...
//
// Column values are stringified for use as arrow field names — the
// header row is always a string. The value column's arrow type
// follows aggOutputType(Aggregation{Kind: agg, ...}); it's Int64 for
// Count/NUnique and Float64 for the rest, matching what GroupBy.Agg
// would produce.
//
// Cells with no matching input rows are emitted as null. Output rows
// are ordered by the sorted index value; output columns are ordered
// by the sorted distinct columns-column value so the shape is
// deterministic across runs.
//
// Equivalent to `pandas.DataFrame.pivot_table(index, columns, values,
// aggfunc)` / polars' `DataFrame.pivot`. Multi-column pivot indices
// aren't supported yet — pass a single index column.
func (f *Frame) Pivot(index, columns, values string, agg AggKind) (*Frame, error) {
	if f == nil {
		return nil, fmt.Errorf("gobi: Frame.Pivot on nil frame")
	}
	if index == "" || columns == "" || values == "" {
		return nil, fmt.Errorf("gobi: Pivot: index, columns, and values are all required")
	}
	if index == columns || index == values || columns == values {
		return nil, fmt.Errorf("gobi: Pivot: index, columns, and values must name distinct columns")
	}

	// Reduce to long form: (index, columns, values_agg). Reuse the
	// GroupBy machinery so aggregation semantics + null handling
	// stay consistent with the rest of the API.
	gb, err := f.GroupBy(index, columns)
	if err != nil {
		return nil, err
	}
	long, err := gb.Agg(Aggregation{Column: values, Kind: agg, Alias: values})
	if err != nil {
		return nil, err
	}

	idxCol, err := long.Column(index)
	if err != nil {
		return nil, err
	}
	colCol, err := long.Column(columns)
	if err != nil {
		return nil, err
	}
	valCol, err := long.Column(values)
	if err != nil {
		return nil, err
	}

	// Bucket the long-form rows into a nested map:
	//   idxKey  → colHeader → row-index in long
	// idxKey is the composite-key byte encoding of the index scalar,
	// so numeric bit-equal values collapse identically. colHeader is
	// the stringified column-column value — becomes an arrow field
	// name in the output.
	nLong := long.NumRows()
	type cellPos struct {
		row int // row index in the long frame
	}
	byIdx := make(map[string]map[string]cellPos)
	// idxOrder captures first-seen order for stable output shape
	// before we sort it.
	var idxOrder []string
	// Preserve the original index scalar per idxKey so we can emit
	// it in the output's first column with the source's arrow type
	// intact.
	idxScalar := make(map[string]any)
	// Same for column headers, so the sorted output columns look
	// sensible regardless of input type.
	colHeaders := make(map[string]struct{})

	idxKeys := []Series{idxCol}
	var idxScratch []byte
	for row := 0; row < nLong; row++ {
		buf, err := composeCompositeKeyInto(idxScratch[:0], idxKeys, row)
		if err != nil {
			return nil, err
		}
		idxScratch = buf
		ik := string(buf)
		if _, ok := byIdx[ik]; !ok {
			byIdx[ik] = make(map[string]cellPos)
			idxOrder = append(idxOrder, ik)
			v, err := readScalarAt(idxCol, row)
			if err != nil {
				return nil, err
			}
			idxScalar[ik] = v
		}
		header, err := pivotColumnHeader(colCol, row)
		if err != nil {
			return nil, err
		}
		byIdx[ik][header] = cellPos{row: row}
		colHeaders[header] = struct{}{}
	}

	// Deterministic output shape: sort idx keys and column headers.
	sort.Strings(idxOrder)
	headers := make([]string, 0, len(colHeaders))
	for h := range colHeaders {
		headers = append(headers, h)
	}
	sort.Strings(headers)

	// Build output builders.
	pool := memory.DefaultAllocator
	idxFieldType := idxCol.DataType()
	idxBuilder, err := builderForType(pool, idxFieldType)
	if err != nil {
		return nil, fmt.Errorf("gobi: Pivot: %w", err)
	}
	defer idxBuilder.Release()

	valOutType := aggOutputType(Aggregation{Kind: agg, Column: values})
	valBuilders := make([]array.Builder, len(headers))
	for i := range headers {
		b, err := builderForType(pool, valOutType)
		if err != nil {
			return nil, fmt.Errorf("gobi: Pivot: %w", err)
		}
		valBuilders[i] = b
	}
	defer releaseBuilders(valBuilders)

	// Emit rows.
	for _, ik := range idxOrder {
		if err := appendCustomValue(idxBuilder, idxScalar[ik]); err != nil {
			return nil, fmt.Errorf("gobi: Pivot: writing index value: %w", err)
		}
		row := byIdx[ik]
		for hi, h := range headers {
			pos, ok := row[h]
			if !ok {
				valBuilders[hi].AppendNull()
				continue
			}
			v, err := readScalarAt(valCol, pos.row)
			if err != nil {
				return nil, err
			}
			if v == nil {
				valBuilders[hi].AppendNull()
				continue
			}
			if err := appendCustomValue(valBuilders[hi], v); err != nil {
				return nil, fmt.Errorf("gobi: Pivot: writing cell (%s, %s): %w",
					ik, h, err)
			}
		}
	}

	// Materialize columns + assemble the output Frame.
	fields := make([]arrow.Field, 0, 1+len(headers))
	cols := make([]arrow.Column, 0, 1+len(headers))
	fields = append(fields, arrow.Field{Name: index, Type: idxFieldType, Nullable: false})
	idxArr := idxBuilder.NewArray()
	defer idxArr.Release()
	cols = append(cols, *arrow.NewColumn(fields[0], arrow.NewChunked(idxFieldType, []arrow.Array{idxArr})))
	for i, h := range headers {
		fld := arrow.Field{Name: h, Type: valOutType, Nullable: aggOutputNullable(Aggregation{Kind: agg})}
		fields = append(fields, fld)
		a := valBuilders[i].NewArray()
		defer a.Release()
		cols = append(cols, *arrow.NewColumn(fld, arrow.NewChunked(valOutType, []arrow.Array{a})))
	}
	schema := arrow.NewSchema(fields, nil)
	return NewFrame(schema, cols)
}

// pivotColumnHeader stringifies the columns-column value at row for
// use as an arrow field name. Supported types match the hashable
// key set — anything else is rejected. Nulls become the literal
// string "null", matching the GroupBy behavior on null keys.
func pivotColumnHeader(s Series, row int) (string, error) {
	null, err := isNullAtSeries(s, row)
	if err != nil {
		return "", err
	}
	if null {
		return "null", nil
	}
	v, err := readScalarAt(s, row)
	if err != nil {
		return "", err
	}
	switch x := v.(type) {
	case string:
		return x, nil
	case bool:
		if x {
			return "true", nil
		}
		return "false", nil
	case int64:
		return fmt.Sprintf("%d", x), nil
	case int32:
		return fmt.Sprintf("%d", x), nil
	case uint64:
		return fmt.Sprintf("%d", x), nil
	case uint32:
		return fmt.Sprintf("%d", x), nil
	case float64:
		return fmt.Sprintf("%g", x), nil
	case float32:
		return fmt.Sprintf("%g", x), nil
	case arrow.Timestamp:
		return fmt.Sprintf("%d", int64(x)), nil
	}
	return "", fmt.Errorf("gobi: Pivot: columns column value type %T not supported as a header", v)
}
