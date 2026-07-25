package gobi

import (
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// filteredAggFrame builds a small frame with a "source" branch label,
// used to demonstrate the "unified pipeline" pattern:
//
//	region  source   provider    count
//	NA      "seg"    "att"       1
//	NA      "seg"    "verizon"   2
//	NA      "ping"   "att"       10   ← filtered out from seg-set
//	EU      "seg"    "vodafone"  3
//	EU      "ping"   "orange"    20   ← filtered out from seg-set
//	EU      "ping"   null        30
func filteredAggFrame(t *testing.T) *Frame {
	t.Helper()
	pool := memory.DefaultAllocator

	regionB := array.NewStringBuilder(pool)
	defer regionB.Release()
	regionB.AppendValues([]string{"NA", "NA", "NA", "EU", "EU", "EU"}, nil)

	sourceB := array.NewStringBuilder(pool)
	defer sourceB.Release()
	sourceB.AppendValues([]string{"seg", "seg", "ping", "seg", "ping", "ping"}, nil)

	providerB := array.NewStringBuilder(pool)
	defer providerB.Release()
	providerB.Append("att")
	providerB.Append("verizon")
	providerB.Append("att")
	providerB.Append("vodafone")
	providerB.Append("orange")
	providerB.AppendNull()

	countB := array.NewInt64Builder(pool)
	defer countB.Release()
	countB.AppendValues([]int64{1, 2, 10, 3, 20, 30}, nil)

	fields := []arrow.Field{
		{Name: "region", Type: arrow.BinaryTypes.String, Nullable: false},
		{Name: "source", Type: arrow.BinaryTypes.String, Nullable: false},
		{Name: "provider", Type: arrow.BinaryTypes.String, Nullable: true},
		{Name: "count", Type: arrow.PrimitiveTypes.Int64, Nullable: false},
	}
	schema := arrow.NewSchema(fields, nil)
	arrs := []arrow.Array{regionB.NewArray(), sourceB.NewArray(), providerB.NewArray(), countB.NewArray()}
	defer func() {
		for _, a := range arrs {
			a.Release()
		}
	}()
	cols := make([]arrow.Column, len(fields))
	for i, a := range arrs {
		cols[i] = *arrow.NewColumn(fields[i], arrow.NewChunked(a.DataType(), []arrow.Array{a}))
	}
	f, err := NewFrame(schema, cols)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestFilteredAgg_SumOnMatchingRows(t *testing.T) {
	f := filteredAggFrame(t)
	// Sum of count where source=="seg", per region.
	gb, err := f.GroupBy("region")
	if err != nil {
		t.Fatal(err)
	}
	out, err := gb.Agg(Aggregation{
		Column: "count", Kind: AggSum, Alias: "seg_count",
		Filter: Col("source").Eq(Lit("seg")),
	})
	if err != nil {
		t.Fatalf("filtered agg failed: %v", err)
	}
	if r, _ := out.Shape(); r != 2 {
		t.Fatalf("row count = %d, want 2", r)
	}
	// Sorted region: EU, NA
	//   EU seg-rows: {3}          → 3
	//   NA seg-rows: {1, 2}       → 3
	regions, _ := out.Column("region")
	rArr := regions.col.Data().Chunks()[0].(*array.String)
	sums, _ := out.Column("seg_count")
	sArr := sums.col.Data().Chunks()[0].(*array.Float64)
	if rArr.Value(0) != "EU" || sArr.Value(0) != 3 {
		t.Fatalf("EU seg_count = %v, want 3", sArr.Value(0))
	}
	if rArr.Value(1) != "NA" || sArr.Value(1) != 3 {
		t.Fatalf("NA seg_count = %v, want 3", sArr.Value(1))
	}
}

func TestFilteredAgg_CollectSetSkipsExcluded(t *testing.T) {
	f := filteredAggFrame(t)
	// This is the pipeline the user's workaround was designed for:
	// collect_set(provider) where source == "seg", per region.
	gb, _ := f.GroupBy("region")
	out, err := gb.Agg(Aggregation{
		Column: "provider",
		Fn:     NewStringSetAggregator(),
		Alias:  "seg_providers",
		Filter: Col("source").Eq(Lit("seg")),
	})
	if err != nil {
		t.Fatalf("filtered collect_set: %v", err)
	}
	providers, _ := out.Column("seg_providers")
	la := providers.col.Data().Chunks()[0].(*array.List)
	values := la.ListValues().(*array.String)
	getRow := func(row int) []string {
		start, end := la.ValueOffsets(row)
		out := make([]string, end-start)
		for j := start; j < end; j++ {
			out[j-start] = values.Value(int(j))
		}
		return out
	}
	// EU seg-rows have provider "vodafone".
	if got := getRow(0); len(got) != 1 || got[0] != "vodafone" {
		t.Fatalf("EU seg_providers = %v, want [vodafone]", got)
	}
	// NA seg-rows have "att" and "verizon".
	if got := getRow(1); len(got) != 2 || got[0] != "att" || got[1] != "verizon" {
		t.Fatalf("NA seg_providers = %v, want [att verizon]", got)
	}
}

// A single Agg call with two different Filter clauses — each agg
// independently narrows its own row set.
func TestFilteredAgg_MultipleIndependentFilters(t *testing.T) {
	f := filteredAggFrame(t)
	gb, _ := f.GroupBy("region")
	out, err := gb.Agg(
		Aggregation{
			Column: "count", Kind: AggSum, Alias: "seg_sum",
			Filter: Col("source").Eq(Lit("seg")),
		},
		Aggregation{
			Column: "count", Kind: AggSum, Alias: "ping_sum",
			Filter: Col("source").Eq(Lit("ping")),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	// EU: seg_sum=3, ping_sum=20+30=50
	// NA: seg_sum=1+2=3, ping_sum=10
	segArr := out.series[1].col.Data().Chunks()[0].(*array.Float64)
	pingArr := out.series[2].col.Data().Chunks()[0].(*array.Float64)
	if segArr.Value(0) != 3 || pingArr.Value(0) != 50 {
		t.Fatalf("EU: seg=%v ping=%v, want (3, 50)", segArr.Value(0), pingArr.Value(0))
	}
	if segArr.Value(1) != 3 || pingArr.Value(1) != 10 {
		t.Fatalf("NA: seg=%v ping=%v, want (3, 10)", segArr.Value(1), pingArr.Value(1))
	}
}

// Filter that produces a null result should treat null as false
// (SQL FILTER WHERE semantics — the row is excluded).
func TestFilteredAgg_NullFilterTreatedAsFalse(t *testing.T) {
	f := filteredAggFrame(t)
	// Filter: provider IsNotNull. Rows where provider is null are
	// excluded from the aggregation. In EU, row 5 has null provider.
	gb, _ := f.GroupBy("region")
	out, err := gb.Agg(Aggregation{
		Column: "count", Kind: AggSum, Alias: "known_sum",
		Filter: Col("provider").IsNotNull(),
	})
	if err != nil {
		t.Fatal(err)
	}
	sumArr := out.series[1].col.Data().Chunks()[0].(*array.Float64)
	// EU non-null provider rows: 3 (seg vodafone) + 20 (ping orange) = 23
	// NA non-null provider rows: 1 + 2 + 10 = 13
	if sumArr.Value(0) != 23 {
		t.Fatalf("EU known_sum = %v, want 23", sumArr.Value(0))
	}
	if sumArr.Value(1) != 13 {
		t.Fatalf("NA known_sum = %v, want 13", sumArr.Value(1))
	}
}

// End-to-end via LazyFrame.Collect — the filter path routes through
// the materializing fallback (allBuiltInAggs rejects filtered aggs).
func TestFilteredAgg_LazyPipeline(t *testing.T) {
	f := filteredAggFrame(t)
	out, err := f.Lazy().
		GroupBy("region").
		Agg(Aggregation{
			Column: "count", Kind: AggSum, Alias: "seg_sum",
			Filter: Col("source").Eq(Lit("seg")),
		}).
		Collect()
	if err != nil {
		t.Fatalf("lazy filtered agg: %v", err)
	}
	if r, _ := out.Shape(); r != 2 {
		t.Fatalf("row count = %d, want 2", r)
	}
}

func TestFilteredAgg_NonBooleanFilterErrors(t *testing.T) {
	f := filteredAggFrame(t)
	gb, _ := f.GroupBy("region")
	// Filter that produces a non-Boolean column.
	_, err := gb.Agg(Aggregation{
		Column: "count", Kind: AggSum,
		Filter: Col("count"), // Int64, not Boolean
	})
	if err == nil {
		t.Fatal("expected error for non-Boolean filter")
	}
}
