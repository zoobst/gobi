package gobi

import (
	"strings"
	"testing"
)

// -- Strategy labels: each op maps to the right Streaming/Materialize prefix.

func TestExplainPhysical_StreamingFilterProjectLimit(t *testing.T) {
	df := lazyFrame(t)
	got := df.Lazy().
		Filter(Col("active")).
		Select(Col("id")).
		Limit(3).
		ExplainPhysical()

	// All three ops stream. Scan is a Frame (in-memory) source.
	wants := []string{
		"StreamingLimit(3)",
		"StreamingProject(",
		"StreamingFilter(",
		"ScanFrame(5 rows",
	}
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("missing %q in:\n%s", w, got)
		}
	}
}

func TestExplainPhysical_AggregateBuiltInIsStreaming(t *testing.T) {
	df := lazyFrame(t)
	got := df.Lazy().
		GroupBy("region").
		Agg(Aggregation{Column: "price", Kind: AggSum}).
		ExplainPhysical()
	if !strings.Contains(got, "StreamingAggregate(") {
		t.Fatalf("built-in aggs should be StreamingAggregate:\n%s", got)
	}
	if strings.Contains(got, "MaterializeAggregate(") {
		t.Fatalf("built-in aggs should not materialize:\n%s", got)
	}
}

func TestExplainPhysical_AggregateCustomFnMaterializes(t *testing.T) {
	// A custom Fn aggregator forces the compiler onto the materialize
	// path. ExplainPhysical should reflect that.
	df := lazyFrame(t)
	got := df.Lazy().
		GroupBy("region").
		Agg(Aggregation{Column: "region", Fn: countDistinctAggregator{}}).
		ExplainPhysical()
	if !strings.Contains(got, "MaterializeAggregate(") {
		t.Fatalf("custom Fn agg should be MaterializeAggregate:\n%s", got)
	}
}

func TestExplainPhysical_JoinLeftDrivenKindsStream(t *testing.T) {
	left := lazyFrame(t)
	right := lazyRegions(t)
	for _, k := range []JoinType{JoinInner, JoinLeft, JoinSemi, JoinAnti} {
		got := left.Lazy().Join(right.Lazy(), "region", "region", k).ExplainPhysical()
		if !strings.Contains(got, "StreamingJoin(") {
			t.Errorf("kind=%v: expected StreamingJoin:\n%s", k, got)
		}
	}
}

func TestExplainPhysical_JoinRightAndFullMaterialize(t *testing.T) {
	left := lazyFrame(t)
	right := lazyRegions(t)
	for _, k := range []JoinType{JoinRight, JoinFull} {
		got := left.Lazy().Join(right.Lazy(), "region", "region", k).ExplainPhysical()
		if !strings.Contains(got, "MaterializeJoin(") {
			t.Errorf("kind=%v: expected MaterializeJoin:\n%s", k, got)
		}
	}
}

func TestExplainPhysical_SortAlwaysMaterializes(t *testing.T) {
	df := lazyFrame(t)
	got := df.Lazy().SortBy(SortKey{Column: "price"}).ExplainPhysical()
	if !strings.Contains(got, "MaterializeSort(") {
		t.Fatalf("sort should always materialize:\n%s", got)
	}
}

func TestExplainPhysical_TailMaterializes(t *testing.T) {
	df := lazyFrame(t)
	got := df.Lazy().Tail(2).ExplainPhysical()
	if !strings.Contains(got, "MaterializeTail(2)") {
		t.Fatalf("tail should materialize:\n%s", got)
	}
}

func TestExplainPhysical_EmptyNode(t *testing.T) {
	// Filter(false) collapses to emptyNode via the optimizer.
	df := lazyFrame(t)
	got := df.Lazy().Filter(Lit(false)).ExplainPhysical()
	if !strings.Contains(got, "Empty") {
		t.Fatalf("filter(false) should collapse to Empty:\n%s", got)
	}
	// And should not still show a Filter.
	if strings.Contains(got, "StreamingFilter(") {
		t.Fatalf("empty cascade left a filter behind:\n%s", got)
	}
}

// -- Tree formatting: indent per depth, order matches evaluation order.

func TestExplainPhysical_IndentAndOrder(t *testing.T) {
	df := lazyFrame(t)
	got := df.Lazy().
		Filter(Col("active")).
		Select(Col("id")).
		Limit(3).
		ExplainPhysical()
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("expected 4 lines (Limit, Project, Filter, Scan), got %d:\n%s",
			len(lines), got)
	}
	// Outermost first, no indent.
	if strings.HasPrefix(lines[0], " ") {
		t.Errorf("line 0 should have no leading space: %q", lines[0])
	}
	// Each subsequent line indents by 2 more spaces.
	for i := 1; i < len(lines); i++ {
		wantPrefix := strings.Repeat("  ", i)
		if !strings.HasPrefix(lines[i], wantPrefix) {
			t.Errorf("line %d = %q; want prefix %q", i, lines[i], wantPrefix)
		}
	}
}

// -- Cross-check with logical Explain: node COUNT should match

func TestExplainPhysical_NodeCountMatchesLogical(t *testing.T) {
	// The two explain outputs describe the same tree, just labeled
	// differently. Line counts should agree.
	df := lazyFrame(t)
	lf := df.Lazy().
		Filter(Col("active")).
		Select(Col("id"), Col("price")).
		SortBy(SortKey{Column: "id"}).
		Limit(3)

	logical := strings.TrimRight(lf.ExplainOptimized(), "\n")
	physical := strings.TrimRight(lf.ExplainPhysical(), "\n")

	lLines := len(strings.Split(logical, "\n"))
	pLines := len(strings.Split(physical, "\n"))
	if lLines != pLines {
		t.Fatalf("logical=%d lines, physical=%d lines\nlogical:\n%s\n\nphysical:\n%s",
			lLines, pLines, logical, physical)
	}
}
