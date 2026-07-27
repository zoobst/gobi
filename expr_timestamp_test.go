package gobi

import (
	"errors"
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
