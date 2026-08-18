package gobi

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

func timestampFrame(t testing.TB, unit arrow.TimeUnit, vals []int64) *Frame {
	t.Helper()
	pool := memory.DefaultAllocator
	ts := &arrow.TimestampType{Unit: unit}
	b := array.NewTimestampBuilder(pool, ts)
	defer b.Release()
	tsVals := make([]arrow.Timestamp, len(vals))
	for i, v := range vals {
		tsVals[i] = arrow.Timestamp(v)
	}
	b.AppendValues(tsVals, nil)
	arr := b.NewArray()
	defer arr.Release()
	field := arrow.Field{Name: "ts", Type: ts, Nullable: false}
	col := arrow.NewColumn(field, arrow.NewChunked(ts, []arrow.Array{arr}))
	schema := arrow.NewSchema([]arrow.Field{field}, nil)
	f, err := NewFrame(schema, []arrow.Column{*col})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// TestExpr_UnixNano_Normalizes — Timestamp source normalizes to
// int64 nanoseconds regardless of TimeUnit.
func TestExpr_UnixNano_Normalizes(t *testing.T) {
	cases := []struct {
		name string
		unit arrow.TimeUnit
		raw  int64
		want int64
	}{
		{"nanos", arrow.Nanosecond, 1_500_000_000, 1_500_000_000},
		{"micros", arrow.Microsecond, 1_500_000, 1_500_000_000},
		{"millis", arrow.Millisecond, 1_500, 1_500_000_000},
		{"seconds", arrow.Second, 1, 1_000_000_000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := timestampFrame(t, tc.unit, []int64{tc.raw})
			out, err := f.WithColumnExpr("ns", Col("ts").UnixNano())
			if err != nil {
				t.Fatal(err)
			}
			col, _ := out.Column("ns")
			if col.DataType().ID() != arrow.INT64 {
				t.Fatalf("dtype = %s, want INT64", col.DataType())
			}
			got := col.col.Data().Chunks()[0].(*array.Int64).Value(0)
			if got != tc.want {
				t.Errorf("%s: got %d, want %d", tc.name, got, tc.want)
			}
		})
	}
}

// TestExpr_UnixNano_ChainedToHours — timestamp → nanos → float64 →
// hours-since-epoch. This is the shape callers who need a hours
// column inline use.
func TestExpr_UnixNano_ChainedToHours(t *testing.T) {
	// 2 hours past epoch, in nanosecond precision.
	twoHoursNs := int64(2 * time.Hour)
	f := timestampFrame(t, arrow.Nanosecond, []int64{twoHoursNs})

	out, err := f.WithColumnExpr("hours",
		Col("ts").UnixNano().Cast(arrow.PrimitiveTypes.Float64).
			Div(Lit(float64(time.Hour))))
	if err != nil {
		t.Fatal(err)
	}
	col, _ := out.Column("hours")
	if col.DataType().ID() != arrow.FLOAT64 {
		t.Fatalf("dtype = %s, want FLOAT64", col.DataType())
	}
	got := col.col.Data().Chunks()[0].(*array.Float64).Value(0)
	if got != 2.0 {
		t.Errorf("hours = %v, want 2.0", got)
	}
}

// TestExpr_UnixNano_RejectsNonTimestamp — non-Timestamp source
// errors with ExprTypeMismatch.
func TestExpr_UnixNano_RejectsNonTimestamp(t *testing.T) {
	pool := memory.DefaultAllocator
	b := array.NewInt64Builder(pool)
	defer b.Release()
	b.AppendValues([]int64{1, 2, 3}, nil)
	arr := b.NewArray()
	defer arr.Release()
	field := arrow.Field{Name: "x", Type: arrow.PrimitiveTypes.Int64, Nullable: false}
	col := arrow.NewColumn(field, arrow.NewChunked(arr.DataType(), []arrow.Array{arr}))
	schema := arrow.NewSchema([]arrow.Field{field}, nil)
	f, _ := NewFrame(schema, []arrow.Column{*col})
	_, err := f.WithColumnExpr("bad", Col("x").UnixNano())
	if err == nil {
		t.Fatal("expected error for UnixNano on Int64 column")
	}
	if !errors.Is(err, ErrExprTypeMismatch) {
		t.Errorf("error should wrap ErrExprTypeMismatch, got %v", err)
	}
}

// TestExpr_LitTime_Type — Lit(time.Time) constructs a Timestamp[ns]
// literal (matching from_structs.go's mapping for time.Time struct
// fields) without erroring at construction.
func TestExpr_LitTime_Type(t *testing.T) {
	when := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	e := Lit(when)
	dt, err := e.node.Type(nil)
	if err != nil {
		t.Fatalf("Type: %v", err)
	}
	ts, ok := dt.(*arrow.TimestampType)
	if !ok {
		t.Fatalf("dtype = %s, want *TimestampType", dt)
	}
	if ts.Unit != arrow.Nanosecond {
		t.Errorf("unit = %s, want nanosecond", ts.Unit)
	}
}

// TestExpr_LitTime_Broadcast — Lit(time.Time) used via WithColumnExpr
// broadcasts to a Timestamp column of the frame's row count.
func TestExpr_LitTime_Broadcast(t *testing.T) {
	when := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	f := timestampFrame(t, arrow.Nanosecond, []int64{0, 1_000, 2_000})
	out, err := f.WithColumnExpr("stamp", Lit(when))
	if err != nil {
		t.Fatalf("WithColumnExpr: %v", err)
	}
	col, _ := out.Column("stamp")
	if _, ok := col.DataType().(*arrow.TimestampType); !ok {
		t.Fatalf("dtype = %s, want Timestamp", col.DataType())
	}
	arr := col.col.Data().Chunks()[0].(*array.Timestamp)
	if arr.Len() != 3 {
		t.Fatalf("len = %d, want 3", arr.Len())
	}
	want := arrow.Timestamp(when.UnixNano())
	for i := range arr.Len() {
		if arr.Value(i) != want {
			t.Errorf("row %d = %v, want %v", i, arr.Value(i), want)
		}
	}
}

// TestExpr_TimestampCol_GeLit — Col(ts).Ge(Lit(cutoff)) filters
// correctly. Uses the scalar Timestamp fast path in binOpNode.Eval.
func TestExpr_TimestampCol_GeLit(t *testing.T) {
	// Frame rows: 2026-01-01, 2026-06-01, 2027-01-01 (nanoseconds).
	rows := []int64{
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano(),
		time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC).UnixNano(),
		time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano(),
	}
	f := timestampFrame(t, arrow.Nanosecond, rows)
	cutoff := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	out, err := f.FilterExpr(Col("ts").Ge(Lit(cutoff)))
	if err != nil {
		t.Fatalf("FilterExpr: %v", err)
	}
	if out.NumRows() != 2 {
		t.Fatalf("rows = %d, want 2 (>= 2026-06-01)", out.NumRows())
	}
}

// TestExpr_TimestampCol_LtLit_TypeInference — Type() on
// (Timestamp col < Timestamp lit) reports Boolean, not an error.
func TestExpr_TimestampCol_LtLit_TypeInference(t *testing.T) {
	f := timestampFrame(t, arrow.Nanosecond, []int64{0})
	cutoff := time.Now()
	dt, err := Col("ts").Lt(Lit(cutoff)).node.Type(f.Schema())
	if err != nil {
		t.Fatalf("Type: %v", err)
	}
	if dt.ID() != arrow.BOOL {
		t.Errorf("dtype = %s, want BOOL", dt)
	}
}

// TestExpr_TimestampUnitMismatch_ErrorsClearly — comparing a
// millisecond-unit Timestamp column against a nanosecond Lit
// surfaces a clear unit-mismatch error at type-check time rather
// than silently producing wrong results.
func TestExpr_TimestampUnitMismatch_ErrorsClearly(t *testing.T) {
	f := timestampFrame(t, arrow.Millisecond, []int64{1000})
	_, err := Col("ts").Ge(Lit(time.Now())).node.Type(f.Schema())
	if err == nil {
		t.Fatal("expected unit-mismatch error")
	}
	if !errors.Is(err, ErrExprTypeMismatch) {
		t.Errorf("error should wrap ErrExprTypeMismatch, got %v", err)
	}
	if !strings.Contains(err.Error(), "unit mismatch") {
		t.Errorf("error should mention unit mismatch, got %v", err)
	}
}

// TestExpr_TimestampFusedFilter — Col(ts).Ge(Lit(a)).And(Col(ts).Lt(Lit(b)))
// hits the fused-filter path (two Timestamp cmp leaves) and returns
// the correct row window. Regression against parseFusedLeaf's older
// blanket rejection of *array.Timestamp.
func TestExpr_TimestampFusedFilter(t *testing.T) {
	// Frame rows: 2026-01-01, 2026-06-01, 2026-09-01, 2027-01-01.
	rows := []int64{
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano(),
		time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC).UnixNano(),
		time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC).UnixNano(),
		time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano(),
	}
	f := timestampFrame(t, arrow.Nanosecond, rows)
	lo := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	hi := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	// Range filter: lo <= ts < hi. Matches rows[1] and rows[2].
	out, err := f.FilterExpr(Col("ts").Ge(Lit(lo)).And(Col("ts").Lt(Lit(hi))))
	if err != nil {
		t.Fatalf("FilterExpr: %v", err)
	}
	if out.NumRows() != 2 {
		t.Fatalf("rows = %d, want 2 ([lo, hi))", out.NumRows())
	}
	col, _ := out.Column("ts")
	arr := col.col.Data().Chunks()[0].(*array.Timestamp)
	if int64(arr.Value(0)) != rows[1] || int64(arr.Value(1)) != rows[2] {
		t.Errorf("rows = [%d, %d], want [%d, %d]",
			int64(arr.Value(0)), int64(arr.Value(1)), rows[1], rows[2])
	}
}

// TestExpr_TimestampFusedFilter_ParseFusedLeaf — direct check that
// parseFusedLeaf accepts a Timestamp cmp Timestamp-lit leaf (kind=3)
// and rejects Timestamp cmp non-Timestamp-lit (unit or type
// mismatch would misorder).
func TestExpr_TimestampFusedFilter_ParseFusedLeaf(t *testing.T) {
	f := timestampFrame(t, arrow.Nanosecond, []int64{0, 1, 2})

	// Accept: Timestamp col cmp Timestamp lit.
	e := Col("ts").Ge(Lit(time.Unix(0, 1))).node
	leaf, ok := parseFusedLeaf(f, e)
	if !ok {
		t.Fatal("expected parseFusedLeaf to accept Timestamp/Timestamp leaf")
	}
	if leaf.kind != 3 {
		t.Errorf("kind = %d, want 3 (timestamp)", leaf.kind)
	}
	if leaf.scalarI != 1 {
		t.Errorf("scalarI = %d, want 1", leaf.scalarI)
	}

	// Reject: Timestamp col cmp int-lit (would silently misorder if
	// accepted — user must cast explicitly).
	e2 := Col("ts").Ge(Lit(int64(1))).node
	if _, ok := parseFusedLeaf(f, e2); ok {
		t.Error("expected parseFusedLeaf to reject Timestamp/Int64 leaf")
	}
}

// TestExpr_TimestampFusedFilter_UnitMismatchRejected — a Timestamp col
// with millisecond unit compared to a nanosecond Lit(time.Time) must
// NOT be accepted by parseFusedLeaf; the general path errors at
// Type() with a clear unit-mismatch message.
func TestExpr_TimestampFusedFilter_UnitMismatchRejected(t *testing.T) {
	f := timestampFrame(t, arrow.Millisecond, []int64{1000, 2000})
	e := Col("ts").Ge(Lit(time.Now())).node
	if _, ok := parseFusedLeaf(f, e); ok {
		t.Error("expected parseFusedLeaf to reject unit-mismatched Timestamp leaf")
	}
}

// TestExpr_Cast_TimestampToInt64 — direct Cast of Timestamp to
// Int64 emits the raw underlying value in the source unit (unlike
// UnixNano, which normalizes to nanoseconds).
func TestExpr_Cast_TimestampToInt64(t *testing.T) {
	f := timestampFrame(t, arrow.Millisecond, []int64{1_500, 2_500})
	out, err := f.WithColumnExpr("i", Col("ts").Cast(arrow.PrimitiveTypes.Int64))
	if err != nil {
		t.Fatal(err)
	}
	col, _ := out.Column("i")
	arr := col.col.Data().Chunks()[0].(*array.Int64)
	// Millisecond unit — raw values pass through unchanged.
	if arr.Value(0) != 1_500 || arr.Value(1) != 2_500 {
		t.Errorf("raw millis = [%d, %d], want [1500, 2500]", arr.Value(0), arr.Value(1))
	}
}
