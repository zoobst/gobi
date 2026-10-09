package gobi

import (
	"fmt"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// wideOverBenchFrame builds a fixture with:
//   - `nCols` Float64 columns (payload — most unused by the Over inner)
//   - 1 "id" partition-key column
//   - 1 "v" value column that the Shift.Over reads
//
// nRows rows split across nGroups partitions. Used to measure how much
// the "project mini-Frame down to referenced columns" optimization
// saves on wide inputs.
func wideOverBenchFrame(b testing.TB, nRows, nGroups, nCols int) *Frame {
	b.Helper()
	pool := memory.DefaultAllocator

	// id key (contiguous groups for aligned-path candidate).
	idB := array.NewInt64Builder(pool)
	defer idB.Release()
	rowsPerGroup := nRows / nGroups
	for g := range nGroups {
		for range rowsPerGroup {
			idB.Append(int64(g))
		}
	}

	// v — value column, the one Shift reads.
	vB := array.NewFloat64Builder(pool)
	defer vB.Release()
	for i := range nRows {
		vB.Append(float64(i))
	}

	// nCols extra payload columns (unread by the Over inner).
	payloadArrs := make([]arrow.Array, nCols)
	payloadFields := make([]arrow.Field, nCols)
	for c := range nCols {
		pB := array.NewFloat64Builder(pool)
		for i := range nRows {
			pB.Append(float64(c*1000 + i))
		}
		payloadArrs[c] = pB.NewArray()
		payloadFields[c] = arrow.Field{
			Name:     fmt.Sprintf("pad_%d", c),
			Type:     arrow.PrimitiveTypes.Float64,
			Nullable: false,
		}
		pB.Release()
	}
	defer func() {
		for _, a := range payloadArrs {
			a.Release()
		}
	}()

	fields := append([]arrow.Field{
		{Name: "id", Type: arrow.PrimitiveTypes.Int64, Nullable: false},
		{Name: "v", Type: arrow.PrimitiveTypes.Float64, Nullable: false},
	}, payloadFields...)
	schema := arrow.NewSchema(fields, nil)

	idArr := idB.NewArray()
	defer idArr.Release()
	vArr := vB.NewArray()
	defer vArr.Release()

	cols := make([]arrow.Column, len(fields))
	cols[0] = *arrow.NewColumn(fields[0], arrow.NewChunked(idArr.DataType(), []arrow.Array{idArr}))
	cols[1] = *arrow.NewColumn(fields[1], arrow.NewChunked(vArr.DataType(), []arrow.Array{vArr}))
	for c := range nCols {
		cols[2+c] = *arrow.NewColumn(fields[2+c], arrow.NewChunked(payloadArrs[c].DataType(), []arrow.Array{payloadArrs[c]}))
	}
	f, err := NewFrame(schema, cols)
	if err != nil {
		b.Fatal(err)
	}
	return f
}

// BenchmarkOver_ShapePreserving_WideFrame runs Col("v").Shift(1).Over("id")
// on a Frame with many payload columns. The projection optimization
// should reduce per-partition mini-Frame allocation from ~all-columns
// to just {v}.
func BenchmarkOver_ShapePreserving_WideFrame(b *testing.B) {
	f := wideOverBenchFrame(b, 10000, 100, 20) // 100 groups × 100 rows × 20 payload cols
	b.ReportAllocs()

	for b.Loop() {
		_, err := f.WithColumnExpr("prev_v", Col("v").Shift(1).Over("id"))
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkOver_ShapePreserving_AlignedSlice runs the same shape but
// with a PartitionMetadata assertion that unlocks the aligned +
// contiguous-range fast path. Should slice instead of take, reducing
// per-partition allocation further.
func BenchmarkOver_ShapePreserving_AlignedSlice(b *testing.B) {
	f := wideOverBenchFrame(b, 10000, 100, 20)
	f.WithPartitionMeta(&PartitionMetadata{
		Columns:      []string{"id"},
		HashFn:       "test/v1",
		SortedBy:     []SortKey{{Column: "id"}},
		SortEnforced: true,
	})
	b.ReportAllocs()
	for b.Loop() {
		_, err := f.WithColumnExpr("prev_v", Col("v").Shift(1).Over("id"))
		if err != nil {
			b.Fatal(err)
		}
	}
}
