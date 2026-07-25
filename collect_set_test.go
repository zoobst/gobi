package gobi

import (
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// setAggFrame builds a small frame for set-aggregator tests:
//
//	region  provider   count  ip_int  h3_cell (uint64)
//	NA      "att"       1     101      100
//	NA      "verizon"   2     102      100
//	NA      "att"       3     101      200   // dup provider + ip, distinct cell
//	EU      "vodafone"  4     201      300
//	EU      null        5     202      300   // null provider skipped
//	NA      null        6     null     100   // both nulls
func setAggFrame(t *testing.T) *Frame {
	t.Helper()
	pool := memory.DefaultAllocator

	regionB := array.NewStringBuilder(pool)
	defer regionB.Release()
	regionB.AppendValues([]string{"NA", "NA", "NA", "EU", "EU", "NA"}, nil)

	providerB := array.NewStringBuilder(pool)
	defer providerB.Release()
	providerB.Append("att")
	providerB.Append("verizon")
	providerB.Append("att")
	providerB.Append("vodafone")
	providerB.AppendNull()
	providerB.AppendNull()

	countB := array.NewInt64Builder(pool)
	defer countB.Release()
	countB.AppendValues([]int64{1, 2, 3, 4, 5, 6}, nil)

	ipB := array.NewInt32Builder(pool)
	defer ipB.Release()
	ipB.Append(101)
	ipB.Append(102)
	ipB.Append(101)
	ipB.Append(201)
	ipB.Append(202)
	ipB.AppendNull()

	cellB := array.NewUint64Builder(pool)
	defer cellB.Release()
	cellB.AppendValues([]uint64{100, 100, 200, 300, 300, 100}, nil)

	fields := []arrow.Field{
		{Name: "region", Type: arrow.BinaryTypes.String, Nullable: false},
		{Name: "provider", Type: arrow.BinaryTypes.String, Nullable: true},
		{Name: "count", Type: arrow.PrimitiveTypes.Int64, Nullable: false},
		{Name: "ip_int", Type: arrow.PrimitiveTypes.Int32, Nullable: true},
		{Name: "h3_cell", Type: arrow.PrimitiveTypes.Uint64, Nullable: false},
	}
	schema := arrow.NewSchema(fields, nil)
	arrs := []arrow.Array{
		regionB.NewArray(), providerB.NewArray(), countB.NewArray(),
		ipB.NewArray(), cellB.NewArray(),
	}
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

func TestSetAggregator_StringDistinctPerGroup(t *testing.T) {
	f := setAggFrame(t)
	gb, err := f.GroupBy("region")
	if err != nil {
		t.Fatal(err)
	}
	out, err := gb.Agg(Aggregation{
		Column: "provider",
		Fn:     NewStringSetAggregator(),
		Alias:  "providers",
	})
	if err != nil {
		t.Fatalf("StringSetAggregator: %v", err)
	}
	providers, err := out.Column("providers")
	if err != nil {
		t.Fatal(err)
	}
	if providers.DataType().ID() != arrow.LIST {
		t.Fatalf("providers type = %s, want LIST", providers.DataType())
	}
	// Sorted groups: EU (row 0), NA (row 1).
	listArr := providers.col.Data().Chunks()[0].(*array.List)
	values := listArr.ListValues().(*array.String)

	getRow := func(row int) []string {
		start, end := listArr.ValueOffsets(row)
		out := make([]string, end-start)
		for i := start; i < end; i++ {
			out[i-start] = values.Value(int(i))
		}
		return out
	}
	// EU: providers = {vodafone}; null skipped.
	if got := getRow(0); len(got) != 1 || got[0] != "vodafone" {
		t.Fatalf("EU providers = %v, want [vodafone]", got)
	}
	// NA: providers = {att, verizon} (sorted); duplicate att collapses; null skipped.
	if got := getRow(1); len(got) != 2 || got[0] != "att" || got[1] != "verizon" {
		t.Fatalf("NA providers = %v, want [att verizon]", got)
	}
}

func TestSetAggregator_Uint64H3CellShape(t *testing.T) {
	f := setAggFrame(t)
	gb, _ := f.GroupBy("region")
	out, err := gb.Agg(Aggregation{
		Column: "h3_cell",
		Fn:     NewUint64SetAggregator(),
		Alias:  "cells",
	})
	if err != nil {
		t.Fatalf("Uint64SetAggregator: %v", err)
	}
	cells, _ := out.Column("cells")
	if cells.DataType().ID() != arrow.LIST {
		t.Fatalf("cells type = %s, want LIST", cells.DataType())
	}
	listArr := cells.col.Data().Chunks()[0].(*array.List)
	values := listArr.ListValues().(*array.Uint64)
	getRow := func(row int) []uint64 {
		start, end := listArr.ValueOffsets(row)
		out := make([]uint64, end-start)
		for i := start; i < end; i++ {
			out[i-start] = values.Value(int(i))
		}
		return out
	}
	// EU: cells = {300}
	if got := getRow(0); len(got) != 1 || got[0] != 300 {
		t.Fatalf("EU cells = %v, want [300]", got)
	}
	// NA: cells = {100, 200} sorted
	if got := getRow(1); len(got) != 2 || got[0] != 100 || got[1] != 200 {
		t.Fatalf("NA cells = %v, want [100 200]", got)
	}
}

func TestSetAggregator_Int32WithNulls(t *testing.T) {
	f := setAggFrame(t)
	gb, _ := f.GroupBy("region")
	out, err := gb.Agg(Aggregation{
		Column: "ip_int",
		Fn:     NewInt32SetAggregator(),
		Alias:  "ips",
	})
	if err != nil {
		t.Fatalf("Int32SetAggregator: %v", err)
	}
	ips, _ := out.Column("ips")
	listArr := ips.col.Data().Chunks()[0].(*array.List)
	values := listArr.ListValues().(*array.Int32)
	getRow := func(row int) []int32 {
		start, end := listArr.ValueOffsets(row)
		out := make([]int32, end-start)
		for i := start; i < end; i++ {
			out[i-start] = values.Value(int(i))
		}
		return out
	}
	// EU ip_ints: 201, 202. sorted = [201, 202]
	if got := getRow(0); len(got) != 2 || got[0] != 201 || got[1] != 202 {
		t.Fatalf("EU ips = %v, want [201 202]", got)
	}
	// NA ip_ints: 101, 102, 101, null. Set = {101, 102}.
	if got := getRow(1); len(got) != 2 || got[0] != 101 || got[1] != 102 {
		t.Fatalf("NA ips = %v, want [101 102]", got)
	}
}

// TestSetAggregator_Merge — call Merge directly to verify the peer
// state combines correctly. Set semantics: union of both peers'
// distinct values.
func TestSetAggregator_Merge(t *testing.T) {
	a := NewStringSetAggregator().(*setAggregator[string])
	b := NewStringSetAggregator().(*setAggregator[string])
	a.seen = map[string]struct{}{"x": {}, "y": {}}
	b.seen = map[string]struct{}{"y": {}, "z": {}}
	if err := a.Merge(b); err != nil {
		t.Fatal(err)
	}
	if len(a.seen) != 3 {
		t.Fatalf("merged set size = %d, want 3", len(a.seen))
	}
	for _, want := range []string{"x", "y", "z"} {
		if _, ok := a.seen[want]; !ok {
			t.Fatalf("missing %q in merged set", want)
		}
	}
}

// TestSetAggregator_MergePeerMismatch — Merge with a peer of a
// different generic instantiation must error, not silently succeed.
func TestSetAggregator_MergePeerMismatch(t *testing.T) {
	a := NewStringSetAggregator()
	b := NewInt64SetAggregator()
	if err := a.Merge(b); err == nil {
		t.Fatal("expected type-mismatch error merging String with Int64")
	}
}

// TestSetAggregator_TypeMismatchColumn — feeding a column whose arrow
// type doesn't match the aggregator's expected type surfaces a clear
// error at Aggregate time (not a silent nil / empty set).
func TestSetAggregator_TypeMismatchColumn(t *testing.T) {
	f := setAggFrame(t)
	gb, _ := f.GroupBy("region")
	// Route a String column through the Int64 aggregator.
	_, err := gb.Agg(Aggregation{
		Column: "provider",
		Fn:     NewInt64SetAggregator(),
		Alias:  "bad",
	})
	if err == nil {
		t.Fatal("expected error routing String column through Int64 aggregator")
	}
}

// TestSetAggregator_AllNullsInGroup — a group with only null values
// emits an empty (non-null) list, matching Spark's collect_set / polars
// unique-on-all-null semantics.
func TestSetAggregator_AllNullsInGroup(t *testing.T) {
	pool := memory.DefaultAllocator
	regionB := array.NewStringBuilder(pool)
	defer regionB.Release()
	regionB.AppendValues([]string{"only-nulls", "only-nulls"}, nil)
	valB := array.NewStringBuilder(pool)
	defer valB.Release()
	valB.AppendNull()
	valB.AppendNull()

	fields := []arrow.Field{
		{Name: "region", Type: arrow.BinaryTypes.String, Nullable: false},
		{Name: "val", Type: arrow.BinaryTypes.String, Nullable: true},
	}
	schema := arrow.NewSchema(fields, nil)
	arrs := []arrow.Array{regionB.NewArray(), valB.NewArray()}
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
	gb, _ := f.GroupBy("region")
	out, err := gb.Agg(Aggregation{Column: "val", Fn: NewStringSetAggregator(), Alias: "s"})
	if err != nil {
		t.Fatalf("all-nulls group: %v", err)
	}
	s, _ := out.Column("s")
	listArr := s.col.Data().Chunks()[0].(*array.List)
	start, end := listArr.ValueOffsets(0)
	if start != end {
		t.Fatalf("all-nulls group produced non-empty list (%d values)", end-start)
	}
}

// TestSetAggregator_LazyPipeline — end-to-end through LazyFrame.Collect,
// which routes custom aggregators through the materializing exec op
// (streaming aggregate rejects custom Fn today per CLAUDE.md).
func TestSetAggregator_LazyPipeline(t *testing.T) {
	f := setAggFrame(t)
	out, err := f.Lazy().
		GroupBy("region").
		Agg(
			Aggregation{Column: "provider", Fn: NewStringSetAggregator(), Alias: "providers"},
			Aggregation{Column: "h3_cell", Fn: NewUint64SetAggregator(), Alias: "cells"},
		).
		Collect()
	if err != nil {
		t.Fatalf("lazy pipeline: %v", err)
	}
	if r, _ := out.Shape(); r != 2 {
		t.Fatalf("row count = %d, want 2", r)
	}
	// Both list columns should be present with the expected types.
	prov, _ := out.Column("providers")
	if prov.DataType().ID() != arrow.LIST {
		t.Fatalf("providers type = %s, want LIST", prov.DataType())
	}
	cells, _ := out.Column("cells")
	if cells.DataType().ID() != arrow.LIST {
		t.Fatalf("cells type = %s, want LIST", cells.DataType())
	}
}
