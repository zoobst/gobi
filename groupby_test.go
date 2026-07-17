package gobi

import (
	"errors"
	"testing"

	"github.com/apache/arrow/go/v18/arrow"
	"github.com/apache/arrow/go/v18/arrow/array"
	"github.com/apache/arrow/go/v18/arrow/memory"
)

// salesFrame returns a frame:
//   region string, product string, revenue float64, units int64
func salesFrame(t *testing.T) *Frame {
	t.Helper()
	pool := memory.DefaultAllocator
	region := array.NewStringBuilder(pool)
	defer region.Release()
	region.AppendValues([]string{"NA", "NA", "EU", "EU", "NA", "APAC"}, nil)
	product := array.NewStringBuilder(pool)
	defer product.Release()
	product.AppendValues([]string{"A", "B", "A", "A", "A", "B"}, nil)
	revenue := array.NewFloat64Builder(pool)
	defer revenue.Release()
	revenue.AppendValues([]float64{100, 200, 300, 150, 50, 400}, nil)
	units := array.NewInt64Builder(pool)
	defer units.Release()
	units.AppendValues([]int64{10, 20, 30, 15, 5, 40}, nil)

	fields := []arrow.Field{
		{Name: "region", Type: arrow.BinaryTypes.String, Nullable: false},
		{Name: "product", Type: arrow.BinaryTypes.String, Nullable: false},
		{Name: "revenue", Type: arrow.PrimitiveTypes.Float64, Nullable: true},
		{Name: "units", Type: arrow.PrimitiveTypes.Int64, Nullable: true},
	}
	schema := arrow.NewSchema(fields, nil)
	arrays := []arrow.Array{region.NewArray(), product.NewArray(), revenue.NewArray(), units.NewArray()}
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
	f, err := NewFrame(schema, cols)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestGroupBy_SingleKey_Sum(t *testing.T) {
	f := salesFrame(t)
	gb, err := f.GroupBy("region")
	if err != nil {
		t.Fatal(err)
	}
	out, err := gb.Agg(
		Aggregation{Kind: AggCount},
		Aggregation{Column: "revenue", Kind: AggSum},
	)
	if err != nil {
		t.Fatal(err)
	}
	r, c := out.Shape()
	if r != 3 || c != 3 {
		t.Fatalf("shape: (%d, %d) want (3, 3)", r, c)
	}
	// Sorted alphabetically by key: APAC, EU, NA
	regions, _ := out.Column("region")
	arr := regions.col.Data().Chunks()[0].(*array.String)
	if arr.Value(0) != "APAC" || arr.Value(1) != "EU" || arr.Value(2) != "NA" {
		t.Fatalf("group order: %v", []string{arr.Value(0), arr.Value(1), arr.Value(2)})
	}
	// APAC revenue = 400, EU = 450, NA = 350
	rev, _ := out.Column("revenue_sum")
	revArr := rev.col.Data().Chunks()[0].(*array.Float64)
	if revArr.Value(0) != 400 || revArr.Value(1) != 450 || revArr.Value(2) != 350 {
		t.Fatalf("revenue sums: %v %v %v",
			revArr.Value(0), revArr.Value(1), revArr.Value(2))
	}
}

func TestGroupBy_MultipleKeys_MinMaxMean(t *testing.T) {
	f := salesFrame(t)
	gb, err := f.GroupBy("region", "product")
	if err != nil {
		t.Fatal(err)
	}
	out, err := gb.Agg(
		Aggregation{Column: "revenue", Kind: AggMean, Alias: "avg_rev"},
		Aggregation{Column: "units", Kind: AggMin, Alias: "min_units"},
		Aggregation{Column: "units", Kind: AggMax, Alias: "max_units"},
	)
	if err != nil {
		t.Fatal(err)
	}
	r, c := out.Shape()
	// Groups: (APAC,B), (EU,A), (NA,A), (NA,B) → 4 rows, 2+3 cols
	if r != 4 || c != 5 {
		t.Fatalf("shape: (%d, %d), want (4, 5)", r, c)
	}
	// (NA, A) has revenue 100 and 50 → mean 75; units 10, 5 → min 5, max 10.
	regions, _ := out.Column("region")
	products, _ := out.Column("product")
	regArr := regions.col.Data().Chunks()[0].(*array.String)
	prodArr := products.col.Data().Chunks()[0].(*array.String)
	naA := -1
	for i := range r {
		if regArr.Value(i) == "NA" && prodArr.Value(i) == "A" {
			naA = i
			break
		}
	}
	if naA < 0 {
		t.Fatalf("no (NA, A) group")
	}
	avg, _ := out.Column("avg_rev")
	avgV := avg.col.Data().Chunks()[0].(*array.Float64).Value(naA)
	if avgV != 75 {
		t.Fatalf("(NA,A) avg_rev = %v, want 75", avgV)
	}
	minU, _ := out.Column("min_units")
	if v := minU.col.Data().Chunks()[0].(*array.Float64).Value(naA); v != 5 {
		t.Fatalf("(NA,A) min_units = %v, want 5", v)
	}
	maxU, _ := out.Column("max_units")
	if v := maxU.col.Data().Chunks()[0].(*array.Float64).Value(naA); v != 10 {
		t.Fatalf("(NA,A) max_units = %v, want 10", v)
	}
}

func TestGroupBy_MissingKey(t *testing.T) {
	f := salesFrame(t)
	_, err := f.GroupBy("nope")
	if !errors.Is(err, ErrColumnNotFound) {
		t.Fatalf("want ErrColumnNotFound, got %v", err)
	}
}

func TestGroupBy_NonHashableKey(t *testing.T) {
	f := smallFrame(t) // has a Binary geometry column
	_, err := f.GroupBy("geom")
	if err == nil {
		t.Fatalf("expected error grouping by Binary column")
	}
}

func TestGroupBy_NoAggregations(t *testing.T) {
	f := salesFrame(t)
	gb, _ := f.GroupBy("region")
	out, err := gb.Agg()
	if err != nil {
		t.Fatal(err)
	}
	if r, c := out.Shape(); r != 3 || c != 1 {
		t.Fatalf("shape: (%d, %d), want (3, 1)", r, c)
	}
}
