package gobi

import (
	"reflect"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
)

// partitionedTestScan builds a scan carrying a PartitionMetadata
// claim, matching the shape athenaio's UnloadAndRead will emit
// after Iceberg CTAS + read-back verification. Reused across every
// propagation subtest so all rules exercise the same source shape.
func partitionedTestScan(t *testing.T, meta *PartitionMetadata) LogicalPlan {
	t.Helper()
	schema := arrow.NewSchema([]arrow.Field{
		{Name: "id", Type: arrow.PrimitiveTypes.Int64, Nullable: false},
		{Name: "ts", Type: arrow.FixedWidthTypes.Timestamp_ns, Nullable: false},
		{Name: "v", Type: arrow.PrimitiveTypes.Float64, Nullable: false},
	}, nil)
	return NewScanNode(
		"Scan[test]",
		schema,
		func() (*Frame, error) { return nil, nil },
		WithPartitionMetadata(meta),
	)
}

// icebergMeta is the "fully claimed" metadata used as the default
// input to propagation tests — hash-partitioned on id, sorted on ts,
// enforced by the writer (matching an Iceberg CTAS shape).
func icebergMeta() *PartitionMetadata {
	return &PartitionMetadata{
		Columns:      []string{"id"},
		HashFn:       "athenaio/iceberg/murmur3-32/v1",
		SortedBy:     []SortKey{{Column: "ts", Descending: false}},
		SortEnforced: true,
	}
}

func TestPropagate_Filter_PassThrough(t *testing.T) {
	scan := partitionedTestScan(t, icebergMeta())
	filter := &filterNode{input: scan, cond: Lit(true)}
	got := filter.PartitionMetadata()
	if !reflect.DeepEqual(got, icebergMeta()) {
		t.Fatalf("Filter should pass metadata through unchanged:\n got: %+v\n want: %+v",
			got, icebergMeta())
	}
}

func TestPropagate_Filter_NilInput(t *testing.T) {
	scan := partitionedTestScan(t, nil)
	filter := &filterNode{input: scan, cond: Lit(true)}
	if got := filter.PartitionMetadata(); got != nil {
		t.Errorf("Filter on unpartitioned input = %+v, want nil", got)
	}
}

func TestPropagate_WithColumn_PassThrough(t *testing.T) {
	scan := partitionedTestScan(t, icebergMeta())
	wc := newWithColumnNode(scan, "v2", Col("v"))
	if !reflect.DeepEqual(wc.PartitionMetadata(), icebergMeta()) {
		t.Errorf("WithColumn should not disturb metadata (only appends columns)")
	}
}

func TestPropagate_Project_AllColumnsSurvive(t *testing.T) {
	scan := partitionedTestScan(t, icebergMeta())
	// Project all three columns — partition + sort keys survive.
	proj := newProjectNode(scan, []Expr{Col("id"), Col("ts"), Col("v")})
	got := proj.PartitionMetadata()
	if !reflect.DeepEqual(got, icebergMeta()) {
		t.Errorf("Project keeping all cols dropped metadata: %+v", got)
	}
}

func TestPropagate_Project_DropsPartitionColumn(t *testing.T) {
	scan := partitionedTestScan(t, icebergMeta())
	// Project drops "id" — partition column vanishes, everything goes.
	proj := newProjectNode(scan, []Expr{Col("ts"), Col("v")})
	if got := proj.PartitionMetadata(); got != nil {
		t.Errorf("dropping partition col should nil metadata, got %+v", got)
	}
}

func TestPropagate_Project_TruncatesSortedByPrefix(t *testing.T) {
	// SortedBy is [ts, v] — Project drops "v", surviving prefix is
	// [ts], SortEnforced preserved.
	meta := &PartitionMetadata{
		Columns: []string{"id"},
		HashFn:  "athenaio/iceberg/murmur3-32/v1",
		SortedBy: []SortKey{
			{Column: "ts", Descending: false},
			{Column: "v", Descending: false},
		},
		SortEnforced: true,
	}
	scan := partitionedTestScan(t, meta)
	proj := newProjectNode(scan, []Expr{Col("id"), Col("ts")})
	got := proj.PartitionMetadata()
	if got == nil {
		t.Fatal("Project should retain partition claim + truncated SortedBy")
	}
	if !stringSlicesEqual(got.Columns, []string{"id"}) {
		t.Errorf("Columns wrong: %v", got.Columns)
	}
	if len(got.SortedBy) != 1 || got.SortedBy[0].Column != "ts" {
		t.Errorf("SortedBy prefix wrong: %+v", got.SortedBy)
	}
	if !got.SortEnforced {
		t.Errorf("SortEnforced dropped alongside prefix truncation (should carry)")
	}
}

func TestPropagate_Project_DropsAllSortedBy(t *testing.T) {
	// SortedBy [ts]; Project drops ts entirely — no prefix survives,
	// SortEnforced dropped too.
	scan := partitionedTestScan(t, icebergMeta())
	proj := newProjectNode(scan, []Expr{Col("id"), Col("v")})
	got := proj.PartitionMetadata()
	if got == nil {
		t.Fatal("partition claim on id should survive")
	}
	if got.SortedBy != nil || got.SortEnforced {
		t.Errorf("SortedBy should be dropped when no prefix survives: %+v enforced=%v",
			got.SortedBy, got.SortEnforced)
	}
}

func TestPropagate_Drop_PartitionColumn(t *testing.T) {
	scan := partitionedTestScan(t, icebergMeta())
	drop := newDropNode(scan, "id")
	if got := drop.PartitionMetadata(); got != nil {
		t.Errorf("Drop of partition col should nil metadata, got %+v", got)
	}
}

func TestPropagate_Drop_SortedByColumn(t *testing.T) {
	scan := partitionedTestScan(t, icebergMeta())
	drop := newDropNode(scan, "ts")
	got := drop.PartitionMetadata()
	if got == nil {
		t.Fatal("partition claim on id should survive dropping ts")
	}
	if got.SortedBy != nil || got.SortEnforced {
		t.Errorf("SortedBy should be gone after dropping its only column: %+v",
			got.SortedBy)
	}
}

func TestPropagate_Drop_UnrelatedColumn(t *testing.T) {
	scan := partitionedTestScan(t, icebergMeta())
	drop := newDropNode(scan, "v")
	got := drop.PartitionMetadata()
	if !reflect.DeepEqual(got, icebergMeta()) {
		t.Errorf("Drop of unrelated col disturbed metadata: %+v", got)
	}
}

func TestPropagate_Limit_EnforcedSortSurvives(t *testing.T) {
	scan := partitionedTestScan(t, icebergMeta())
	l := &limitNode{input: scan, n: 100}
	got := l.PartitionMetadata()
	if !reflect.DeepEqual(got, icebergMeta()) {
		t.Errorf("Limit on enforced-sorted input should preserve everything: %+v", got)
	}
}

func TestPropagate_Limit_HintSortStripped(t *testing.T) {
	meta := icebergMeta()
	meta.SortEnforced = false // hint only (Hive-shaped)
	scan := partitionedTestScan(t, meta)
	l := &limitNode{input: scan, n: 100}
	got := l.PartitionMetadata()
	if got == nil {
		t.Fatal("partition claim should survive; only SortedBy stripped")
	}
	if got.SortedBy != nil {
		t.Errorf("hint-only SortedBy should be stripped: %+v", got.SortedBy)
	}
	if !stringSlicesEqual(got.Columns, []string{"id"}) {
		t.Errorf("partition Columns wrong: %v", got.Columns)
	}
}

func TestPropagate_Tail_SameShapeAsLimit(t *testing.T) {
	// Tail should behave identically to Limit — row subset from the
	// other end. Sanity-check by comparing outputs on both branches.
	meta := icebergMeta()
	meta.SortEnforced = false
	scan := partitionedTestScan(t, meta)
	l := &limitNode{input: scan, n: 100}
	tail := &tailNode{input: scan, n: 100}
	if !reflect.DeepEqual(l.PartitionMetadata(), tail.PartitionMetadata()) {
		t.Errorf("Tail should mirror Limit propagation: limit=%+v tail=%+v",
			l.PartitionMetadata(), tail.PartitionMetadata())
	}
}

func TestPropagate_Sort_DropsPartitionSetsSorted(t *testing.T) {
	scan := partitionedTestScan(t, icebergMeta())
	s := &sortNode{input: scan, keys: []SortKey{{Column: "v", Descending: true}}}
	got := s.PartitionMetadata()
	if got == nil {
		t.Fatal("Sort should emit explicit no-partitioning claim, not nil")
	}
	if len(got.Columns) != 0 || got.HashFn != "" {
		t.Errorf("Sort should drop partition Columns+HashFn, got %+v", got)
	}
	if len(got.SortedBy) != 1 || got.SortedBy[0].Column != "v" || !got.SortedBy[0].Descending {
		t.Errorf("SortedBy should reflect the new sort keys: %+v", got.SortedBy)
	}
	if !got.SortEnforced {
		t.Errorf("gobi's Sort is a real sort, SortEnforced should be true")
	}
}

func TestPropagate_Aggregate_KeyAlignedPreserves(t *testing.T) {
	// GroupBy(id) on input partitioned by id — output should carry
	// the same partition claim (each group's row belongs to the
	// partition its constituent rows came from).
	scan := partitionedTestScan(t, icebergMeta())
	agg := newAggregateNode(scan, []string{"id"}, []Aggregation{
		{Column: "v", Kind: AggSum},
	})
	got := agg.PartitionMetadata()
	if got == nil {
		t.Fatal("Aggregate on partition-aligned input should preserve claim")
	}
	if !stringSlicesEqual(got.Columns, []string{"id"}) ||
		got.HashFn != "athenaio/iceberg/murmur3-32/v1" {
		t.Errorf("preserved metadata wrong: %+v", got)
	}
	if got.SortedBy != nil {
		t.Errorf("SortedBy should not survive aggregation: %+v", got.SortedBy)
	}
}

func TestPropagate_Aggregate_KeyMismatchDrops(t *testing.T) {
	// GroupBy(v) on input partitioned by id — misaligned, drop claim.
	scan := partitionedTestScan(t, icebergMeta())
	agg := newAggregateNode(scan, []string{"v"}, []Aggregation{
		{Column: "id", Kind: AggCount},
	})
	if got := agg.PartitionMetadata(); got != nil {
		t.Errorf("misaligned group-by should nil metadata: %+v", got)
	}
}

func TestPropagate_Aggregate_NilInputStaysNil(t *testing.T) {
	scan := partitionedTestScan(t, nil)
	agg := newAggregateNode(scan, []string{"id"}, []Aggregation{
		{Column: "v", Kind: AggSum},
	})
	if got := agg.PartitionMetadata(); got != nil {
		t.Errorf("no input claim → no output claim, got %+v", got)
	}
}

func TestPropagate_Join_InnerPreservesLeft(t *testing.T) {
	left := partitionedTestScan(t, icebergMeta())
	right := partitionedTestScan(t, nil) // right has no claim
	j := newJoinNode(left, right, "id", "id", JoinInner)
	got := j.PartitionMetadata()
	if got == nil {
		t.Fatal("Inner join should preserve left partition claim")
	}
	if !stringSlicesEqual(got.Columns, []string{"id"}) ||
		got.HashFn != "athenaio/iceberg/murmur3-32/v1" {
		t.Errorf("preserved partition claim wrong: %+v", got)
	}
	if got.SortedBy != nil || got.SortEnforced {
		t.Errorf("hash-join destroys within-partition order, SortedBy should be dropped: %+v",
			got.SortedBy)
	}
}

func TestPropagate_Join_LeftPreservesLeft(t *testing.T) {
	left := partitionedTestScan(t, icebergMeta())
	right := partitionedTestScan(t, nil)
	j := newJoinNode(left, right, "id", "id", JoinLeft)
	got := j.PartitionMetadata()
	if got == nil || !stringSlicesEqual(got.Columns, []string{"id"}) {
		t.Errorf("Left join should preserve left partition claim, got %+v", got)
	}
}

func TestPropagate_Join_SemiAntiPreserveLeft(t *testing.T) {
	left := partitionedTestScan(t, icebergMeta())
	right := partitionedTestScan(t, nil)
	for _, kind := range []JoinType{JoinSemi, JoinAnti} {
		j := newJoinNode(left, right, "id", "id", kind)
		got := j.PartitionMetadata()
		if got == nil || !stringSlicesEqual(got.Columns, []string{"id"}) {
			t.Errorf("%v should preserve left partition claim, got %+v", kind, got)
		}
	}
}

func TestPropagate_Join_RightAndFullDrop(t *testing.T) {
	left := partitionedTestScan(t, icebergMeta())
	right := partitionedTestScan(t, icebergMeta())
	for _, kind := range []JoinType{JoinRight, JoinFull} {
		j := newJoinNode(left, right, "id", "id", kind)
		if got := j.PartitionMetadata(); got != nil {
			t.Errorf("%v should drop partition claim, got %+v", kind, got)
		}
	}
}

func TestPropagate_LazyFrame_DeepChain(t *testing.T) {
	// End-to-end: scan → filter → withColumn → limit. Every operator
	// preserves partition (Filter/WithColumn/Limit all pass-through
	// or preserve for enforced sort). LazyFrame.PartitionMetadata()
	// walks the root and reports the propagated claim.
	scan := partitionedTestScan(t, icebergMeta())
	lf := NewLazyFrame(scan).
		Filter(Col("v").Gt(Lit(0.0))).
		WithColumn("v2", Col("v").Mul(Lit(2.0))).
		Limit(1000)
	got := lf.PartitionMetadata()
	if !reflect.DeepEqual(got, icebergMeta()) {
		t.Errorf("deep chain should preserve enforced-sort partitioned metadata:\n got: %+v\n want: %+v",
			got, icebergMeta())
	}
}
