package gobi

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// bitRow covers every integer width, with negatives in the signed
// columns and nulls via pointers.
type bitRow struct {
	K   string
	I8  *int8
	I16 *int16
	I32 *int32
	I64 *int64
	U8  *uint8
	U16 *uint16
	U32 *uint32
	U64 *uint64
}

func ptr[T any](v T) *T { return &v }

func mkBitRow(k string, v int64) bitRow {
	return bitRow{
		K: k, I8: ptr(int8(v)), I16: ptr(int16(v)), I32: ptr(int32(v)), I64: ptr(v),
		U8: ptr(uint8(v)), U16: ptr(uint16(v)), U32: ptr(uint32(v)), U64: ptr(uint64(v)),
	}
}

// bitFixture: group "a" mixes signs and a null row; "b" is a single
// value; "z" is all-null (must emit null).
func bitFixture() ([]bitRow, map[string][]int64) {
	rows := []bitRow{
		mkBitRow("a", 0b0101),
		mkBitRow("b", 0x7f),
		mkBitRow("a", -3), // ...11111101
		{K: "a"},          // all-null row: skipped
		mkBitRow("a", 0b1100),
		{K: "z"},
	}
	vals := map[string][]int64{"a": {0b0101, -3, 0b1100}, "b": {0x7f}, "z": nil}
	return rows, vals
}

// fold is the reference: plain Go bitwise ops at the target width.
func fold[T int8 | int16 | int32 | int64 | uint8 | uint16 | uint32 | uint64](kind AggKind, vs []int64) (T, bool) {
	if len(vs) == 0 {
		return 0, false
	}
	acc := T(vs[0])
	for _, v := range vs[1:] {
		switch kind {
		case AggBitOr:
			acc |= T(v)
		case AggBitAnd:
			acc &= T(v)
		case AggBitXor:
			acc ^= T(v)
		}
	}
	return acc, true
}

func wantBits(kind AggKind, col string, vs []int64) (any, bool) {
	switch col {
	case "I8":
		v, ok := fold[int8](kind, vs)
		return v, ok
	case "I16":
		v, ok := fold[int16](kind, vs)
		return v, ok
	case "I32":
		v, ok := fold[int32](kind, vs)
		return v, ok
	case "I64":
		v, ok := fold[int64](kind, vs)
		return v, ok
	case "U8":
		v, ok := fold[uint8](kind, vs)
		return v, ok
	case "U16":
		v, ok := fold[uint16](kind, vs)
		return v, ok
	case "U32":
		v, ok := fold[uint32](kind, vs)
		return v, ok
	}
	v, ok := fold[uint64](kind, vs)
	return v, ok
}

var bitCols = []string{"I8", "I16", "I32", "I64", "U8", "U16", "U32", "U64"}
var bitKinds = []AggKind{AggBitOr, AggBitAnd, AggBitXor}

func bitAggs() []Aggregation {
	var aggs []Aggregation
	for _, k := range bitKinds {
		for _, c := range bitCols {
			aggs = append(aggs, Aggregation{Column: c, Kind: k})
		}
	}
	return aggs
}

// checkBitResult verifies every (kind, column) output of an
// aggregated frame against the reference fold, by key.
func checkBitResult(t *testing.T, label string, src, out *Frame, vals map[string][]int64) {
	t.Helper()
	keys, err := out.Column("K")
	if err != nil {
		t.Fatal(err)
	}
	if keys.Len() != len(vals) {
		t.Fatalf("%s: %d groups, want %d", label, keys.Len(), len(vals))
	}
	for _, kind := range bitKinds {
		for _, c := range bitCols {
			name := fmt.Sprintf("%s_%s", c, kind)
			s, err := out.Column(name)
			if err != nil {
				t.Fatalf("%s: %v", label, err)
			}
			srcCol, _ := src.Column(c)
			if !arrow.TypeEqual(s.DataType(), srcCol.DataType()) {
				t.Errorf("%s: %s type = %s, want source type %s", label, name, s.DataType(), srcCol.DataType())
			}
			for r := range keys.Len() {
				k, _ := readScalarAt(keys, r)
				got, err := readScalarAt(s, r)
				if err != nil {
					t.Fatal(err)
				}
				want, ok := wantBits(kind, c, vals[k.(string)])
				if !ok {
					if got != nil {
						t.Errorf("%s: %s[%s] = %v, want null (all-null group)", label, name, k, got)
					}
					continue
				}
				if got != want {
					t.Errorf("%s: %s[%s] = %v (%T), want %v (%T)", label, name, k, got, got, want, want)
				}
			}
		}
	}
}

// TestAggBitwise_EagerAndStreaming — eager GroupBy and the streaming
// lazy executor agree with the reference fold for every width, and
// preserve the source type.
func TestAggBitwise_EagerAndStreaming(t *testing.T) {
	rows, vals := bitFixture()
	f, err := FromStructs(rows)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Release()

	gb, err := f.GroupBy("K")
	if err != nil {
		t.Fatal(err)
	}
	eager, err := gb.Agg(bitAggs()...)
	if err != nil {
		t.Fatalf("eager Agg: %v", err)
	}
	checkBitResult(t, "eager", f, eager, vals)

	lf := f.Lazy().GroupBy("K").Agg(bitAggs()...)
	if plan := lf.ExplainPhysical(); !strings.Contains(plan, "Streaming") {
		t.Fatalf("lazy plan not streaming: %s", plan)
	}
	streamed, err := lf.Collect()
	if err != nil {
		t.Fatalf("streaming Collect: %v", err)
	}
	checkBitResult(t, "streaming", f, streamed, vals)

	// Schema() must agree with what Collect produced.
	for i, fld := range lf.Schema().Fields() {
		if got := streamed.Schema().Field(i).Type; !arrow.TypeEqual(fld.Type, got) {
			t.Errorf("planned %s type %s != collected %s", fld.Name, fld.Type, got)
		}
	}
}

// TestAggBitwise_MultiChunk — eager frames built by Concat are
// multi-chunk; the per-row cursor path must match.
func TestAggBitwise_MultiChunk(t *testing.T) {
	rows, vals := bitFixture()
	a, err := FromStructs(rows[:3])
	if err != nil {
		t.Fatal(err)
	}
	defer a.Release()
	b, err := FromStructs(rows[3:])
	if err != nil {
		t.Fatal(err)
	}
	defer b.Release()
	f, err := Concat(a, b)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Release()
	if c, _ := f.Column("I8"); len(c.Column().Data().Chunks()) < 2 {
		t.Fatal("test setup: Concat produced a single chunk")
	}
	gb, err := f.GroupBy("K")
	if err != nil {
		t.Fatal(err)
	}
	out, err := gb.Agg(bitAggs()...)
	if err != nil {
		t.Fatal(err)
	}
	checkBitResult(t, "multi-chunk", f, out, vals)
}

// TestAggBitwise_IdempotentMerge — OR / AND over already-merged rows
// plus the originals gives the same result, so re-running a bitmask
// compaction is safe.
func TestAggBitwise_IdempotentMerge(t *testing.T) {
	type r struct {
		K    string
		Mask int32
	}
	src := []r{{"a", 0b0001}, {"a", 0b0100}, {"b", 0b1000}, {"a", 0b0001}}
	f, err := FromStructs(src)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Release()
	merge := func(in *Frame, kind AggKind) map[string]int32 {
		gb, err := in.GroupBy("K")
		if err != nil {
			t.Fatal(err)
		}
		out, err := gb.Agg(Aggregation{Column: "Mask", Kind: kind, Alias: "Mask"})
		if err != nil {
			t.Fatal(err)
		}
		got, err := ToStructs[r](out)
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]int32{}
		for _, x := range got {
			m[x.K] = x.Mask
		}
		return m
	}
	for _, kind := range []AggKind{AggBitOr, AggBitAnd} {
		once := merge(f, kind)
		// Re-merge the merged rows together with the originals.
		var again []r
		for k, v := range once {
			again = append(again, r{k, v})
		}
		again = append(again, src...)
		g, err := FromStructs(again)
		if err != nil {
			t.Fatal(err)
		}
		twice := merge(g, kind)
		g.Release()
		for k, v := range once {
			if twice[k] != v {
				t.Errorf("%s: key %s once=%b twice=%b", kind, k, v, twice[k])
			}
		}
	}
}

// TestAggBitwise_RejectsNonInteger — float / string columns fail up
// front on every path.
func TestAggBitwise_RejectsNonInteger(t *testing.T) {
	type r struct {
		K string
		F float64
		S string
	}
	f, err := FromStructs([]r{{"a", 1.5, "x"}})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Release()
	gb, err := f.GroupBy("K")
	if err != nil {
		t.Fatal(err)
	}
	for _, col := range []string{"F", "S"} {
		agg := Aggregation{Column: col, Kind: AggBitOr}
		if _, err := gb.Agg(agg); !errors.Is(err, ErrNotNumeric) {
			t.Errorf("eager %s: err = %v, want ErrNotNumeric", col, err)
		}
		if _, err := f.Lazy().GroupBy("K").Agg(agg).Collect(); !errors.Is(err, ErrNotNumeric) {
			t.Errorf("streaming %s: err = %v, want ErrNotNumeric", col, err)
		}
		if _, err := f.WithColumnExpr("o", Col(col).BitOrAgg().Over("K")); !errors.Is(err, ErrNotNumeric) {
			t.Errorf("Over %s: err = %v, want ErrNotNumeric", col, err)
		}
	}
}

// TestAggBitwise_FilterOverPivotRolling — the surfaces that reuse
// the aggregation kinds.
func TestAggBitwise_FilterOverPivotRolling(t *testing.T) {
	type r struct {
		K    string
		C    string
		Mask int16
		Keep bool
	}
	f, err := FromStructs([]r{
		{"a", "x", 0b001, true}, {"a", "x", 0b010, false}, {"a", "y", 0b100, true}, {"b", "x", 0b1000, true},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Release()

	// Filtered aggregation: the Keep=false row is excluded.
	gb, err := f.GroupBy("K")
	if err != nil {
		t.Fatal(err)
	}
	out, err := gb.Agg(Aggregation{Column: "Mask", Kind: AggBitOr, Alias: "m", Filter: Col("Keep")})
	if err != nil {
		t.Fatalf("filtered Agg: %v", err)
	}
	got := map[string]any{}
	ks, _ := out.Column("K")
	ms, _ := out.Column("m")
	for i := range ks.Len() {
		k, _ := readScalarAt(ks, i)
		m, _ := readScalarAt(ms, i)
		got[k.(string)] = m
	}
	if got["a"] != int16(0b101) || got["b"] != int16(0b1000) {
		t.Errorf("filtered bit_or = %v, want a=0b101 b=0b1000", got)
	}

	// Over: per-partition OR broadcast to each row, source type kept.
	w, err := f.WithColumnExpr("m", Col("Mask").BitOrAgg().Over("K"))
	if err != nil {
		t.Fatalf("Over: %v", err)
	}
	m, _ := w.Column("m")
	if m.DataType().ID() != arrow.INT16 {
		t.Errorf("Over type = %s, want int16", m.DataType())
	}
	for i, want := range []int16{0b111, 0b111, 0b111, 0b1000} {
		if v, _ := readScalarAt(m, i); v != want {
			t.Errorf("Over row %d = %v, want %b", i, v, want)
		}
	}

	// Pivot: cells keep the source type.
	p, err := f.Pivot("K", "C", "Mask", AggBitOr)
	if err != nil {
		t.Fatalf("Pivot bit_or: %v", err)
	}
	x, _ := p.Column("x")
	if x.DataType().ID() != arrow.INT16 {
		t.Errorf("Pivot cell type = %s, want int16", x.DataType())
	}
	if v, _ := readScalarAt(x, 0); v != int16(0b011) {
		t.Errorf("Pivot a/x = %v, want 0b011", v)
	}
	// Same fix lets First on a string column pivot.
	if _, err := f.Pivot("K", "C", "Keep", AggFirst); err != nil {
		t.Errorf("Pivot AggFirst on bool: %v", err)
	}

	// Rolling rejects bitwise kinds.
	type tr struct {
		T    time.Time
		Mask int64
	}
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rf, err := FromStructs([]tr{{t0, 1}, {t0.Add(time.Minute), 2}})
	if err != nil {
		t.Fatal(err)
	}
	defer rf.Release()
	roll, err := rf.RollingBy("T", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := roll.Agg("Mask", AggBitOr); err == nil {
		t.Error("TimeRolling bit_or: want error")
	}
}

// TestAggBitwise_Names — String and default aggregate column names.
func TestAggBitwise_Names(t *testing.T) {
	for k, want := range map[AggKind]string{AggBitOr: "bit_or", AggBitAnd: "bit_and", AggBitXor: "bit_xor"} {
		if k.String() != want {
			t.Errorf("%d.String() = %q, want %q", k, k.String(), want)
		}
		if n := aggName(Aggregation{Column: "m", Kind: k}); n != "m_"+want {
			t.Errorf("aggName = %q, want m_%s", n, want)
		}
	}
}

// TestAggBitwise_AlignedPath — a frame whose PartitionMetadata claims
// it is partitioned and sorted on the group key takes the linear-scan
// aggAligned path; results must match the reference too.
func TestAggBitwise_AlignedPath(t *testing.T) {
	rows, vals := bitFixture()
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].K < rows[j].K })
	f, err := FromStructs(rows)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Release()
	f.WithPartitionMeta(&PartitionMetadata{
		Columns:      []string{"K"},
		HashFn:       "test/hash",
		SortedBy:     []SortKey{{Column: "K"}},
		SortEnforced: true,
	})
	if !groupByFastPathApplicable(f.PartitionMetadata(), []string{"K"}) {
		t.Fatal("test setup: aligned path not applicable")
	}
	gb, err := f.GroupBy("K")
	if err != nil {
		t.Fatal(err)
	}
	out, err := gb.Agg(bitAggs()...)
	if err != nil {
		t.Fatalf("aligned Agg: %v", err)
	}
	checkBitResult(t, "aligned", f, out, vals)
}

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
