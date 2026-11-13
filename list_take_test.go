package gobi

import (
	"slices"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// Overture-style nested column: sources is List<Struct<dataset, confidence>>.
// The hand-written list take copies primitive elements only; structs must
// go through compute.Take instead of failing.

var sourceType = arrow.StructOf(
	arrow.Field{Name: "dataset", Type: arrow.BinaryTypes.String, Nullable: true},
	arrow.Field{Name: "confidence", Type: arrow.PrimitiveTypes.Float64, Nullable: true},
)

// sourcesChunk builds one List<Struct> chunk; nil marks a null list.
func sourcesChunk(t *testing.T, rows [][]string) arrow.Array {
	t.Helper()
	lb := array.NewListBuilder(memory.DefaultAllocator, sourceType)
	defer lb.Release()
	sb := lb.ValueBuilder().(*array.StructBuilder)
	for _, row := range rows {
		if row == nil {
			lb.AppendNull()
			continue
		}
		lb.Append(true)
		for _, ds := range row {
			sb.Append(true)
			sb.FieldBuilder(0).(*array.StringBuilder).Append(ds)
			sb.FieldBuilder(1).(*array.Float64Builder).Append(0.5)
		}
	}
	return lb.NewArray()
}

// sourcesFrame is (id int64, sources List<Struct>) with the given chunking.
func sourcesFrame(t *testing.T, ids []int64, chunks ...[][]string) *Frame {
	t.Helper()
	listType := arrow.ListOf(sourceType)
	var arrs []arrow.Array
	for _, c := range chunks {
		arrs = append(arrs, sourcesChunk(t, c))
	}
	ch := arrow.NewChunked(listType, arrs)
	for _, a := range arrs {
		a.Release()
	}
	defer ch.Release()
	fields := []arrow.Field{
		{Name: "id", Type: arrow.PrimitiveTypes.Int64},
		{Name: "sources", Type: listType, Nullable: true},
	}
	idArr := NewInt64Series("id", ids, nil)
	idCol := idArr.col
	srcCol := arrow.NewColumn(fields[1], ch)
	f, err := NewFrame(arrow.NewSchema(fields, nil), []arrow.Column{*idCol, *srcCol})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// datasetsAt returns the dataset names in col at row, or nil if the list is null.
func datasetsAt(t *testing.T, f *Frame, col string, row int) []string {
	t.Helper()
	s, err := f.Column(col)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range s.col.Data().Chunks() {
		if row >= c.Len() {
			row -= c.Len()
			continue
		}
		l := c.(*array.List)
		if l.IsNull(row) {
			return nil
		}
		st := l.ListValues().(*array.Struct)
		names := st.Field(0).(*array.String)
		start, end := l.ValueOffsets(row)
		out := []string{}
		for j := start; j < end; j++ {
			out = append(out, names.Value(int(j)))
		}
		return out
	}
	t.Fatalf("row out of range")
	return nil
}

func TestTake_ListOfStruct(t *testing.T) {
	rows := [][]string{{"osm"}, {"msft", "google"}, nil, {"esri"}}
	for _, tc := range []struct {
		name   string
		chunks [][][]string
	}{
		{"one chunk", [][][]string{rows}},
		{"two chunks", [][][]string{rows[:2], rows[2:]}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := sourcesFrame(t, []int64{0, 1, 2, 3}, tc.chunks...)
			defer f.Release()
			out, err := f.Take([]int{3, 1, 2, 0})
			if err != nil {
				t.Fatalf("Take: %v", err)
			}
			defer out.Release()
			want := [][]string{{"esri"}, {"msft", "google"}, nil, {"osm"}}
			for i, w := range want {
				if got := datasetsAt(t, out, "sources", i); !slices.Equal(got, w) || (got == nil) != (w == nil) {
					t.Errorf("row %d = %v, want %v", i, got, w)
				}
			}
		})
	}
}

func TestJoin_LeftCarriesListOfStructWithNulls(t *testing.T) {
	left := sourcesFrame(t, []int64{1, 2}, [][]string{{"osm"}, {"msft"}})
	defer left.Release()
	right := sourcesFrame(t, []int64{2, 3}, [][]string{{"google"}, {"esri"}})
	defer right.Release()
	right2, err := right.Rename("sources", "sources_r")
	if err != nil {
		t.Fatal(err)
	}
	defer right2.Release()
	out, err := left.Join(right2, "id", "id", JoinLeft)
	if err != nil {
		t.Fatalf("Left join with List<Struct>: %v", err)
	}
	defer out.Release()
	ids, _ := out.Column("id")
	idArr := ids.col.Data().Chunks()[0].(*array.Int64)
	for i := range out.NumRows() {
		got := datasetsAt(t, out, "sources_r", i)
		switch idArr.Value(i) {
		case 1:
			if got != nil {
				t.Errorf("id 1 has no right match; sources_r = %v, want null", got)
			}
		case 2:
			if !slices.Equal(got, []string{"google"}) {
				t.Errorf("id 2 sources_r = %v, want [google]", got)
			}
		}
	}
}
