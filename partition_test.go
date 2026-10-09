package gobi

import (
	"reflect"
	"strings"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// partitionMetaEqual reports value-equality between two metadata
// pointers. Both nil = equal; one nil = unequal.
func partitionMetaEqual(a, b *PartitionMetadata) bool {
	if a == nil || b == nil {
		return a == b
	}
	return reflect.DeepEqual(a, b)
}

// newTinyFrame builds a one-row Int64 frame matching schema, used
// as a runtime fixture for the assertion-transparency Collect test.
func newTinyFrame(t *testing.T, schema *arrow.Schema, ids []int64) *Frame {
	t.Helper()
	pool := memory.DefaultAllocator
	b := array.NewInt64Builder(pool)
	defer b.Release()
	b.AppendValues(ids, nil)
	arr := b.NewArray()
	defer arr.Release()
	chunked := arrow.NewChunked(arr.DataType(), []arrow.Array{arr})
	col := arrow.NewColumn(schema.Field(0), chunked)
	f, err := NewFrame(schema, []arrow.Column{*col})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// TestPartitionMetadata_CloneDeepCopy verifies Clone doesn't share
// slice backings with the source — a subtle invariant for the
// propagation walkers landing in step 2, which mutate metadata
// copies while walking the tree.
func TestPartitionMetadata_CloneDeepCopy(t *testing.T) {
	original := &PartitionMetadata{
		Columns:      []string{"a", "b"},
		HashFn:       "athenaio/iceberg/murmur3-32/v1",
		SortedBy:     []SortKey{{Column: "ts", Descending: false}},
		SortEnforced: true,
	}
	clone := original.Clone()
	if !reflect.DeepEqual(original, clone) {
		t.Fatalf("clone diverged from original:\n orig: %+v\n copy: %+v", original, clone)
	}
	// Mutate the clone; original must remain intact.
	clone.Columns[0] = "MUTATED"
	clone.SortedBy[0].Descending = true
	if original.Columns[0] != "a" {
		t.Errorf("Columns shared backing: original[0] = %q", original.Columns[0])
	}
	if original.SortedBy[0].Descending {
		t.Errorf("SortedBy shared backing: descending flipped on original")
	}
}

// TestPartitionMetadata_CloneNil confirms Clone on a nil receiver
// returns nil (not a zero-value struct).
func TestPartitionMetadata_CloneNil(t *testing.T) {
	var m *PartitionMetadata
	if clone := m.Clone(); clone != nil {
		t.Errorf("Clone(nil) = %+v, want nil", clone)
	}
}

// TestPartitionMetadata_NilVsEmpty documents the load-bearing
// distinction called out in partition.go's doc comment: a nil
// *PartitionMetadata means "no claim made"; a non-nil pointer with
// Columns == nil is an explicit "no partitioning" claim. This test
// pins the gobi-specific paths that must preserve the distinction:
// scan-side attach, LazyFrame accessor round-trip, and Clone. The
// step-3 alignment predicate will treat both as "unaligned" but
// via different reasoning, so any code that silently collapses
// them here would let that predicate lie later.
func TestPartitionMetadata_NilVsEmpty(t *testing.T) {
	schema := arrow.NewSchema([]arrow.Field{
		{Name: "id", Type: arrow.PrimitiveTypes.Int64, Nullable: false},
	}, nil)
	mkScan := func(meta *PartitionMetadata) LogicalPlan {
		return NewScanNode(
			"Scan[test]",
			schema,
			func() (*Frame, error) { return nil, nil },
			WithPartitionMetadata(meta),
		)
	}

	cases := []struct {
		name         string
		attach       *PartitionMetadata
		wantNil      bool // LazyFrame.PartitionMetadata() should report nil
		wantColsZero bool // if non-nil, Columns should be empty
	}{
		{
			name:    "nil claim (no partitioning info)",
			attach:  nil,
			wantNil: true,
		},
		{
			name:         "explicit no-partitioning claim",
			attach:       &PartitionMetadata{},
			wantNil:      false,
			wantColsZero: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lf := NewLazyFrame(mkScan(tc.attach))
			got := lf.PartitionMetadata()
			if (got == nil) != tc.wantNil {
				t.Fatalf("LazyFrame.PartitionMetadata() nil=%v, want nil=%v",
					got == nil, tc.wantNil)
			}
			if tc.wantNil {
				return
			}
			if len(got.Columns) != 0 {
				t.Errorf("explicit no-partitioning claim leaked Columns=%v", got.Columns)
			}
			// Clone must preserve the distinction: a Clone of a
			// non-nil empty metadata is a non-nil empty clone, not
			// silently collapsed to nil. Fatalf on the collapse
			// case — the next check dereferences clone and would
			// panic otherwise.
			clone := got.Clone()
			if clone == nil {
				t.Fatalf("Clone of non-nil empty metadata returned nil (collapse bug)")
			}
			if len(clone.Columns) != 0 {
				t.Errorf("Clone leaked Columns onto empty source: %v", clone.Columns)
			}
		})
	}

	// Cross-check: Clone on a nil receiver returns nil (not a
	// zero-value struct that would confuse callers introspecting
	// `meta == nil` after a defensive copy).
	var nilMeta *PartitionMetadata
	if got := nilMeta.Clone(); got != nil {
		t.Errorf("(*PartitionMetadata)(nil).Clone() = %+v, want nil", got)
	}
}

// TestScanFileNode_PartitionMetadataDefaultNil confirms a scan
// constructed without WithPartitionMetadata reports nil (no claim),
// preserving the pre-v0.3 semantics for existing callers.
func TestScanFileNode_PartitionMetadataDefaultNil(t *testing.T) {
	schema := arrow.NewSchema([]arrow.Field{
		{Name: "id", Type: arrow.PrimitiveTypes.Int64, Nullable: false},
	}, nil)
	scan := NewScanNode("Scan[test]", schema, func() (*Frame, error) { return nil, nil })
	if got := scan.PartitionMetadata(); got != nil {
		t.Errorf("default scan PartitionMetadata = %+v, want nil", got)
	}
}

// TestScanFileNode_WithPartitionMetadata attaches a claim via the
// scan option and confirms it surfaces via LazyFrame.PartitionMetadata()
// unchanged.
func TestScanFileNode_WithPartitionMetadata(t *testing.T) {
	schema := arrow.NewSchema([]arrow.Field{
		{Name: "id", Type: arrow.PrimitiveTypes.Int64, Nullable: false},
	}, nil)
	meta := &PartitionMetadata{
		Columns:      []string{"id"},
		HashFn:       "athenaio/iceberg/murmur3-32/v1",
		SortedBy:     []SortKey{{Column: "ts", Descending: false}},
		SortEnforced: true,
	}
	scan := NewScanNode(
		"Scan[test]",
		schema,
		func() (*Frame, error) { return nil, nil },
		WithPartitionMetadata(meta),
	)
	got := scan.PartitionMetadata()
	if got == nil {
		t.Fatal("got nil, want attached metadata")
	}
	if !reflect.DeepEqual(got, meta) {
		t.Errorf("scan metadata = %+v, want %+v", got, meta)
	}

	// Route through a LazyFrame to confirm the accessor surfaces
	// the same pointer.
	lf := NewLazyFrame(scan)
	if got := lf.PartitionMetadata(); !reflect.DeepEqual(got, meta) {
		t.Errorf("lf.PartitionMetadata() = %+v, want %+v", got, meta)
	}
}

// TestAligned covers the single-source alignment predicate — the
// shape .Over(K) / GroupBy(K)-alignment-check consumers will use.
// Refuses aliasing, reordering, subset matches, nil claims, and
// explicit no-partitioning claims (non-nil Columns == nil).
func TestAligned(t *testing.T) {
	fullMeta := &PartitionMetadata{
		Columns: []string{"a", "b"},
		HashFn:  "athenaio/iceberg/murmur3-32/v1",
	}
	cases := []struct {
		name    string
		meta    *PartitionMetadata
		cols    []string
		aligned bool
	}{
		{"nil meta never aligns", nil, []string{"a", "b"}, false},
		{"exact match", fullMeta, []string{"a", "b"}, true},
		{"empty request refused",
			&PartitionMetadata{Columns: []string{}, HashFn: ""},
			[]string{},
			false},
		{"reorder refused",
			fullMeta,
			[]string{"b", "a"},
			false},
		{"subset (Over(a) on hash(a, b)) refused",
			fullMeta,
			[]string{"a"},
			false},
		{"superset (Over(a, b, c) on hash(a, b)) refused",
			fullMeta,
			[]string{"a", "b", "c"},
			false},
		{"different columns refused",
			fullMeta,
			[]string{"c"},
			false},
		{"value-partitioning (HashFn=\"\") still aligns on same cols",
			&PartitionMetadata{Columns: []string{"a"}, HashFn: ""},
			[]string{"a"},
			true},
		{"explicit no-partitioning (empty Columns) never aligns",
			&PartitionMetadata{HashFn: "athenaio/iceberg/murmur3-32/v1"},
			[]string{"a"},
			false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Aligned(tc.meta, tc.cols); got != tc.aligned {
				t.Errorf("Aligned(%+v, %v) = %v, want %v",
					tc.meta, tc.cols, got, tc.aligned)
			}
		})
	}
}

// TestAlignedWith covers the two-source alignment predicate — the
// shape partition-wise Join uses to prove both sides share a
// hash-partition scheme. Cross-tag hashes never align even when
// columns match; empty-Columns claims never align.
func TestAlignedWith(t *testing.T) {
	ib := &PartitionMetadata{
		Columns: []string{"id"},
		HashFn:  "athenaio/iceberg/murmur3-32/v1",
	}
	cases := []struct {
		name    string
		l, r    *PartitionMetadata
		aligned bool
	}{
		{"both nil never aligns", nil, nil, false},
		{"one nil never aligns", ib, nil, false},
		{"identical claims align",
			&PartitionMetadata{Columns: []string{"id"}, HashFn: "gobi/xxhash64/v1"},
			&PartitionMetadata{Columns: []string{"id"}, HashFn: "gobi/xxhash64/v1"},
			true},
		{"same columns different HashFn refused",
			&PartitionMetadata{Columns: []string{"id"}, HashFn: "athenaio/iceberg/murmur3-32/v1"},
			&PartitionMetadata{Columns: []string{"id"}, HashFn: "gobi/xxhash64/v1"},
			false},
		{"same HashFn different columns refused",
			&PartitionMetadata{Columns: []string{"id"}, HashFn: "gobi/xxhash64/v1"},
			&PartitionMetadata{Columns: []string{"user_id"}, HashFn: "gobi/xxhash64/v1"},
			false},
		{"reordered columns refused",
			&PartitionMetadata{Columns: []string{"a", "b"}, HashFn: "gobi/xxhash64/v1"},
			&PartitionMetadata{Columns: []string{"b", "a"}, HashFn: "gobi/xxhash64/v1"},
			false},
		{"empty-Columns explicit-no-partitioning refuses on either side",
			&PartitionMetadata{HashFn: "gobi/xxhash64/v1"},
			&PartitionMetadata{Columns: []string{"id"}, HashFn: "gobi/xxhash64/v1"},
			false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := AlignedWith(tc.l, tc.r); got != tc.aligned {
				t.Errorf("AlignedWith = %v, want %v", got, tc.aligned)
			}
		})
	}
}

// TestWithPartitionAssertion_ValidNarrowing exercises the accepted
// shapes: nil source (opaque source, any assertion allowed), nil
// assertion (narrowing to no claim), SortedBy prefix truncation,
// and SortEnforced downgrade.
func TestWithPartitionAssertion_ValidNarrowing(t *testing.T) {
	full := icebergMeta() // {id}, iceberg-murmur3, SortedBy=[ts], SortEnforced=true

	cases := []struct {
		name      string
		src       *PartitionMetadata
		assertion *PartitionMetadata
		wantOut   *PartitionMetadata // what LazyFrame.PartitionMetadata() should return
	}{
		{
			name:      "opaque source accepts any assertion",
			src:       nil,
			assertion: full,
			wantOut:   full,
		},
		{
			name:      "nil assertion narrows any source to nil",
			src:       full,
			assertion: nil,
			wantOut:   nil,
		},
		{
			name: "SortedBy prefix truncation (2 keys -> 1)",
			src: &PartitionMetadata{
				Columns:      []string{"id"},
				HashFn:       "athenaio/iceberg/murmur3-32/v1",
				SortedBy:     []SortKey{{Column: "ts"}, {Column: "v"}},
				SortEnforced: true,
			},
			assertion: &PartitionMetadata{
				Columns:      []string{"id"},
				HashFn:       "athenaio/iceberg/murmur3-32/v1",
				SortedBy:     []SortKey{{Column: "ts"}},
				SortEnforced: true,
			},
			wantOut: &PartitionMetadata{
				Columns:      []string{"id"},
				HashFn:       "athenaio/iceberg/murmur3-32/v1",
				SortedBy:     []SortKey{{Column: "ts"}},
				SortEnforced: true,
			},
		},
		{
			name: "SortEnforced downgrade true -> false",
			src:  full,
			assertion: &PartitionMetadata{
				Columns:      []string{"id"},
				HashFn:       "athenaio/iceberg/murmur3-32/v1",
				SortedBy:     []SortKey{{Column: "ts"}},
				SortEnforced: false,
			},
			wantOut: &PartitionMetadata{
				Columns:      []string{"id"},
				HashFn:       "athenaio/iceberg/murmur3-32/v1",
				SortedBy:     []SortKey{{Column: "ts"}},
				SortEnforced: false,
			},
		},
		{
			name: "SortedBy narrowed to empty (drop sort claim entirely)",
			src:  full,
			assertion: &PartitionMetadata{
				Columns: []string{"id"},
				HashFn:  "athenaio/iceberg/murmur3-32/v1",
			},
			wantOut: &PartitionMetadata{
				Columns: []string{"id"},
				HashFn:  "athenaio/iceberg/murmur3-32/v1",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scan := partitionedTestScan(t, tc.src)
			lf, err := NewLazyFrame(scan).WithPartitionAssertion(tc.assertion)
			if err != nil {
				t.Fatalf("valid narrowing rejected: %v", err)
			}
			got := lf.PartitionMetadata()
			if !partitionMetaEqual(got, tc.wantOut) {
				t.Errorf("PartitionMetadata after assertion =\n got: %+v\n want: %+v", got, tc.wantOut)
			}
		})
	}
}

// TestWithPartitionAssertion_RejectedWidening exercises every path
// that widens the source claim — must return an error with a
// diagnostic message naming the widening.
func TestWithPartitionAssertion_RejectedWidening(t *testing.T) {
	src := icebergMeta()

	cases := []struct {
		name       string
		assertion  *PartitionMetadata
		wantSubstr string
	}{
		{
			name: "different Columns rejected",
			assertion: &PartitionMetadata{
				Columns: []string{"user_id"},
				HashFn:  "athenaio/iceberg/murmur3-32/v1",
			},
			wantSubstr: "cannot change Columns",
		},
		{
			name: "reordered Columns rejected",
			assertion: &PartitionMetadata{
				Columns: []string{"id", "extra"},
				HashFn:  "athenaio/iceberg/murmur3-32/v1",
			},
			wantSubstr: "cannot change Columns",
		},
		{
			name: "different HashFn rejected",
			assertion: &PartitionMetadata{
				Columns: []string{"id"},
				HashFn:  "gobi/xxhash64/v1",
			},
			wantSubstr: "cannot change HashFn",
		},
		{
			name: "non-prefix SortedBy rejected",
			assertion: &PartitionMetadata{
				Columns:  []string{"id"},
				HashFn:   "athenaio/iceberg/murmur3-32/v1",
				SortedBy: []SortKey{{Column: "region"}}, // src has [ts]; not a prefix
			},
			wantSubstr: "SortedBy must be a prefix",
		},
		{
			name: "SortEnforced upgrade rejected",
			assertion: func() *PartitionMetadata {
				a := icebergMeta()
				a.SortEnforced = true // src's SortEnforced=true too; force downgrade path
				// Build a src that's SortEnforced=false and test upgrade below.
				return a
			}(),
			wantSubstr: "", // dummy — actual upgrade case in subtest below
		},
	}

	for _, tc := range cases[:4] { // first 4 have real subst checks
		t.Run(tc.name, func(t *testing.T) {
			scan := partitionedTestScan(t, src)
			_, err := NewLazyFrame(scan).WithPartitionAssertion(tc.assertion)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Errorf("error should mention %q, got: %v", tc.wantSubstr, err)
			}
		})
	}

	// SortEnforced upgrade needs a src with SortEnforced=false — separate case.
	t.Run("SortEnforced upgrade rejected", func(t *testing.T) {
		hintSrc := icebergMeta()
		hintSrc.SortEnforced = false
		scan := partitionedTestScan(t, hintSrc)
		upgraded := icebergMeta() // SortEnforced=true
		_, err := NewLazyFrame(scan).WithPartitionAssertion(upgraded)
		if err == nil {
			t.Fatal("SortEnforced upgrade should be rejected")
		}
		if !strings.Contains(err.Error(), "cannot upgrade SortEnforced") {
			t.Errorf("error should mention SortEnforced upgrade, got: %v", err)
		}
	})
}

// TestWithPartitionAssertion_CollectStillWorks confirms the
// assertion node is runtime-transparent — Collect() returns the
// same Frame as the un-asserted plan.
func TestWithPartitionAssertion_CollectStillWorks(t *testing.T) {
	schema := arrow.NewSchema([]arrow.Field{
		{Name: "id", Type: arrow.PrimitiveTypes.Int64, Nullable: false},
	}, nil)
	scan := NewScanNode(
		"Scan[test]",
		schema,
		func() (*Frame, error) {
			// One row: id = 42.
			return newTinyFrame(t, schema, []int64{42}), nil
		},
	)
	lf, err := NewLazyFrame(scan).WithPartitionAssertion(&PartitionMetadata{
		Columns: []string{"id"},
		HashFn:  "gobi/xxhash64/v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	f, err := lf.Collect()
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if r, c := f.Shape(); r != 1 || c != 1 {
		t.Fatalf("shape = (%d, %d), want (1, 1)", r, c)
	}
	// Metadata should still be exposed post-Collect on the LazyFrame.
	if got := lf.PartitionMetadata(); got == nil || got.HashFn != "gobi/xxhash64/v1" {
		t.Errorf("metadata lost through Collect boundary: %+v", got)
	}
}

// TestLogicalPlan_NoClaimSourcesReturnNil confirms the plan nodes
// that have no way to synthesize a partition claim (scanFrame with
// no source metadata, emptyNode as a constant leaf) still return
// nil — step 2 doesn't invent claims where none exist. Nodes with
// propagation logic (Filter, Project, etc.) have their own tests in
// partition_propagation_test.go.
func TestLogicalPlan_NoClaimSourcesReturnNil(t *testing.T) {
	schema := arrow.NewSchema([]arrow.Field{
		{Name: "id", Type: arrow.PrimitiveTypes.Int64, Nullable: false},
	}, nil)
	nodes := []LogicalPlan{
		&emptyNode{schema: schema},
		&scanFrameNode{frame: nil},
	}
	for _, n := range nodes {
		if got := n.PartitionMetadata(); got != nil {
			t.Errorf("%T.PartitionMetadata() = %+v, want nil", n, got)
		}
	}
}

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
