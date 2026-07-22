package gobi

import (
	"math"
	"testing"

	"github.com/apache/arrow-go/v18/arrow/array"
)

// TestAggKinds_ParityEagerVsStreaming verifies the new Aggregation
// Kinds (First / Last / Std / Var / NUnique) return identical
// results whether executed via the eager `GroupBy.Agg` path
// (`CollectRaw`) or the streaming aggregate (`Collect`). Uses the
// shared lazyFrame() fixture — 5 rows, 2 regions.
func TestAggKinds_ParityEagerVsStreaming(t *testing.T) {
	df := lazyFrame(t)

	aggs := []Aggregation{
		{Column: "id", Kind: AggFirst, Alias: "id_first"},
		{Column: "id", Kind: AggLast, Alias: "id_last"},
		{Column: "price", Kind: AggStd, Alias: "price_std"},
		{Column: "price", Kind: AggVar, Alias: "price_var"},
		{Column: "region", Kind: AggNUnique, Alias: "region_nunique"},
	}

	lf := df.Lazy().GroupBy("region").Agg(aggs...)
	streamed, err := lf.Collect()
	if err != nil {
		t.Fatalf("streaming Collect: %v", err)
	}
	eager, err := lf.CollectRaw()
	if err != nil {
		t.Fatalf("eager CollectRaw: %v", err)
	}
	if streamed.NumRows() != eager.NumRows() {
		t.Fatalf("row count: streaming=%d eager=%d",
			streamed.NumRows(), eager.NumRows())
	}
	for _, col := range []string{"region", "id_first", "id_last", "price_std", "price_var", "region_nunique"} {
		s1, err := streamed.Column(col)
		if err != nil {
			t.Fatalf("streamed missing %q", col)
		}
		s2, err := eager.Column(col)
		if err != nil {
			t.Fatalf("eager missing %q", col)
		}
		compareSeriesValues(t, col, s1, s2)
	}
}

// TestAggKinds_FirstLastValues locks in exact expected values so a
// refactor of the row-order logic won't silently swap first/last.
// EU rows are ids [2, 4]; US rows are ids [1, 3, 5]. Streaming +
// eager paths must both agree.
func TestAggKinds_FirstLastValues(t *testing.T) {
	df := lazyFrame(t)
	lf := df.Lazy().GroupBy("region").Agg(
		Aggregation{Column: "id", Kind: AggFirst, Alias: "first"},
		Aggregation{Column: "id", Kind: AggLast, Alias: "last"},
	)
	out, err := lf.Collect()
	if err != nil {
		t.Fatal(err)
	}
	// Streaming aggregate emits groups in sorted key order — EU, US.
	regionCol, _ := out.Column("region")
	firstCol, _ := out.Column("first")
	lastCol, _ := out.Column("last")
	regions := regionCol.Column().Data().Chunks()[0].(*array.String)
	firsts := firstCol.Column().Data().Chunks()[0].(*array.Int64)
	lasts := lastCol.Column().Data().Chunks()[0].(*array.Int64)

	type want struct {
		region      string
		first, last int64
	}
	wanted := []want{{"EU", 2, 4}, {"US", 1, 5}}
	for i, w := range wanted {
		if regions.Value(i) != w.region {
			t.Fatalf("row %d region = %q, want %q", i, regions.Value(i), w.region)
		}
		if firsts.Value(i) != w.first || lasts.Value(i) != w.last {
			t.Errorf("row %d [%s]: first=%d last=%d, want first=%d last=%d",
				i, w.region, firsts.Value(i), lasts.Value(i), w.first, w.last)
		}
	}
}

// TestAggKinds_StdVarValues checks Welford's produces the correct
// sample std/var (Bessel-corrected, n-1 denominator). EU prices
// [20, 40] → variance = 200, std = ~14.142. US prices [10, 30, 50]
// → variance = 400, std = 20.
func TestAggKinds_StdVarValues(t *testing.T) {
	df := lazyFrame(t)
	lf := df.Lazy().GroupBy("region").Agg(
		Aggregation{Column: "price", Kind: AggStd, Alias: "std"},
		Aggregation{Column: "price", Kind: AggVar, Alias: "var"},
	)
	out, err := lf.Collect()
	if err != nil {
		t.Fatal(err)
	}
	stdCol, _ := out.Column("std")
	varCol, _ := out.Column("var")
	stds := stdCol.Column().Data().Chunks()[0].(*array.Float64)
	vars := varCol.Column().Data().Chunks()[0].(*array.Float64)

	// Order: EU first (n=2 → std=√200), US (n=3 → std=20).
	wantStd := []float64{math.Sqrt(200), 20}
	wantVar := []float64{200, 400}
	for i := 0; i < 2; i++ {
		if math.Abs(stds.Value(i)-wantStd[i]) > 1e-9 {
			t.Errorf("row %d std=%v, want %v", i, stds.Value(i), wantStd[i])
		}
		if math.Abs(vars.Value(i)-wantVar[i]) > 1e-9 {
			t.Errorf("row %d var=%v, want %v", i, vars.Value(i), wantVar[i])
		}
	}
}

// TestAggKinds_NUniqueDistinctValues confirms the distinct-value
// counter collapses bit-equal values and skips nulls. EU rows
// have distinct id set {2, 4} → 2; US has {1, 3, 5} → 3.
func TestAggKinds_NUniqueDistinctValues(t *testing.T) {
	df := lazyFrame(t)
	lf := df.Lazy().GroupBy("region").Agg(
		Aggregation{Column: "id", Kind: AggNUnique, Alias: "n"},
	)
	out, err := lf.Collect()
	if err != nil {
		t.Fatal(err)
	}
	nCol, _ := out.Column("n")
	ns := nCol.Column().Data().Chunks()[0].(*array.Int64)
	// Sorted region order: EU (2 distinct), US (3 distinct).
	if ns.Value(0) != 2 || ns.Value(1) != 3 {
		t.Fatalf("n = [%d, %d], want [2, 3]", ns.Value(0), ns.Value(1))
	}
}

// TestAggKinds_StdVarSingleRowGroupsEmitNull mirrors polars / pandas
// semantics: sample std/var over a single row is undefined; the
// aggregate must emit null rather than 0 or NaN.
func TestAggKinds_StdVarSingleRowGroupsEmitNull(t *testing.T) {
	// GroupBy id — every group is size 1. std/var must all be null.
	df := lazyFrame(t)
	lf := df.Lazy().GroupBy("id").Agg(
		Aggregation{Column: "price", Kind: AggStd, Alias: "std"},
		Aggregation{Column: "price", Kind: AggVar, Alias: "var"},
	)
	out, err := lf.Collect()
	if err != nil {
		t.Fatal(err)
	}
	stdCol, _ := out.Column("std")
	varCol, _ := out.Column("var")
	stdArr := stdCol.Column().Data().Chunks()[0].(*array.Float64)
	varArr := varCol.Column().Data().Chunks()[0].(*array.Float64)
	for i := 0; i < int(out.NumRows()); i++ {
		if !stdArr.IsNull(i) {
			t.Errorf("row %d: std should be null (single-row group), got %v", i, stdArr.Value(i))
		}
		if !varArr.IsNull(i) {
			t.Errorf("row %d: var should be null (single-row group), got %v", i, varArr.Value(i))
		}
	}
}
