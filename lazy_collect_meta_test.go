package gobi

import (
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// simplePartitionedFrame builds a Frame sorted by (key, val) with a
// single-chunk String key column and Int64 value column. Used to
// verify PartitionMetadata propagation across Collect.
func simplePartitionedFrame(t testing.TB) *Frame {
	t.Helper()
	pool := memory.DefaultAllocator
	kb := array.NewStringBuilder(pool)
	defer kb.Release()
	kb.AppendValues([]string{"a", "a", "b", "b", "c"}, nil)
	vb := array.NewInt64Builder(pool)
	defer vb.Release()
	vb.AppendValues([]int64{1, 2, 10, 20, 100}, nil)
	kArr := kb.NewArray()
	defer kArr.Release()
	vArr := vb.NewArray()
	defer vArr.Release()
	fields := []arrow.Field{
		{Name: "key", Type: arrow.BinaryTypes.String, Nullable: false},
		{Name: "val", Type: arrow.PrimitiveTypes.Int64, Nullable: false},
	}
	cols := []arrow.Column{
		*arrow.NewColumn(fields[0], arrow.NewChunked(kArr.DataType(), []arrow.Array{kArr})),
		*arrow.NewColumn(fields[1], arrow.NewChunked(vArr.DataType(), []arrow.Array{vArr})),
	}
	f, err := NewFrame(arrow.NewSchema(fields, nil), cols)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// TestCollect_PropagatesPartitionMetadata — a LazyFrame carrying an
// assertion via WithPartitionAssertion should have that claim survive
// the Collect boundary onto the returned Frame. Without this, users
// chaining `frame.Lazy()` for further ops silently lose alignment
// and fall through to the general (unaligned) Over/GroupBy/Join
// paths — the entire point of the assertion.
func TestCollect_PropagatesPartitionMetadata(t *testing.T) {
	f := simplePartitionedFrame(t)
	meta := &PartitionMetadata{
		Columns:      []string{"key"},
		HashFn:       "test/identity/v1",
		SortedBy:     []SortKey{{Column: "key"}, {Column: "val"}},
		SortEnforced: true,
	}
	lf, err := f.Lazy().WithPartitionAssertion(meta)
	if err != nil {
		t.Fatal(err)
	}
	out, err := lf.Collect()
	if err != nil {
		t.Fatal(err)
	}
	got := out.PartitionMetadata()
	if got == nil {
		t.Fatal("Collect dropped the PartitionMetadata claim (expected to propagate)")
	}
	if got.HashFn != meta.HashFn {
		t.Errorf("HashFn = %q, want %q", got.HashFn, meta.HashFn)
	}
	if !got.SortEnforced {
		t.Errorf("SortEnforced = false, want true")
	}
	if len(got.Columns) != 1 || got.Columns[0] != "key" {
		t.Errorf("Columns = %v, want [key]", got.Columns)
	}
}

// TestCollect_PropagatesThroughStreamingPipeline — the propagation
// must survive an ops chain that runs through the streaming
// executor (not just a raw scan). Filter is streaming and preserves
// PartitionMetadata; WithColumn is streaming and preserves it too.
// The final Collect'd Frame should still carry the claim.
func TestCollect_PropagatesThroughStreamingPipeline(t *testing.T) {
	f := simplePartitionedFrame(t)
	meta := &PartitionMetadata{
		Columns:      []string{"key"},
		HashFn:       "test/identity/v1",
		SortedBy:     []SortKey{{Column: "key"}},
		SortEnforced: true,
	}
	lf, err := f.Lazy().WithPartitionAssertion(meta)
	if err != nil {
		t.Fatal(err)
	}
	out, err := lf.
		Filter(Col("val").Gt(Lit(int64(0)))).
		WithColumn("val_plus_1", Col("val").Add(Lit(int64(1)))).
		Collect()
	if err != nil {
		t.Fatal(err)
	}
	if out.PartitionMetadata() == nil {
		t.Fatal("PartitionMetadata dropped across Filter + WithColumn streaming pipeline")
	}
}

// TestCollect_NoMetadataStillWorks — Frames built via NewFrame have
// nil PartitionMetadata by default. Collect should return a Frame
// with nil metadata, not fabricate a claim.
func TestCollect_NoMetadataStillWorks(t *testing.T) {
	f := simplePartitionedFrame(t)
	out, err := f.Lazy().Collect()
	if err != nil {
		t.Fatal(err)
	}
	if out.PartitionMetadata() != nil {
		t.Errorf("Collect fabricated a PartitionMetadata claim on a nil-claim plan: %+v",
			out.PartitionMetadata())
	}
}

// TestCollect_MetadataAgreementAcrossCollectAndCollectRaw — a
// documented invariant on Frame.PartitionMetadata says both entry
// points behave the same. Lock that in.
func TestCollect_MetadataAgreementAcrossCollectAndCollectRaw(t *testing.T) {
	f := simplePartitionedFrame(t)
	meta := &PartitionMetadata{
		Columns:      []string{"key"},
		HashFn:       "test/identity/v1",
		SortedBy:     []SortKey{{Column: "key"}},
		SortEnforced: true,
	}
	lf, err := f.Lazy().WithPartitionAssertion(meta)
	if err != nil {
		t.Fatal(err)
	}
	collectOut, err := lf.Collect()
	if err != nil {
		t.Fatal(err)
	}
	rawOut, err := lf.CollectRaw()
	if err != nil {
		t.Fatal(err)
	}
	a, b := collectOut.PartitionMetadata(), rawOut.PartitionMetadata()
	if a == nil || b == nil {
		t.Fatalf("expected both to propagate; Collect=%v CollectRaw=%v", a, b)
	}
	if a.HashFn != b.HashFn || a.SortEnforced != b.SortEnforced {
		t.Errorf("Collect vs CollectRaw metadata disagree: %+v vs %+v", a, b)
	}
}
