package gobi

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
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
