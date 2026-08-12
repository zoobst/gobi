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
