package gobi

import (
	"testing"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// benchMicros is a 1M-row Timestamp[µs, UTC] Series, a second apart
// from 2024-01-01, every 16th null.
func benchMicros(b *testing.B) Series {
	b.Helper()
	typ := &arrow.TimestampType{Unit: arrow.Microsecond, TimeZone: "UTC"}
	tb := array.NewTimestampBuilder(memory.DefaultAllocator, typ)
	defer tb.Release()
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC).UnixMicro()
	for i := range 1_000_000 {
		if i%16 == 0 {
			tb.AppendNull()
			continue
		}
		tb.Append(arrow.Timestamp(base + int64(i)*1e6))
	}
	return SeriesFromArray(arrow.Field{Name: "ts", Type: typ, Nullable: true}, tb.NewArray())
}

func BenchmarkSeries_TimeOps_1M(b *testing.B) {
	s := benchMicros(b)
	defer s.col.Release()
	for _, tc := range []struct {
		name string
		fn   func() (Series, error)
	}{
		{"AddDuration", func() (Series, error) { return s.AddDuration(time.Hour) }},
		{"TruncateTo_hour", func() (Series, error) { return s.TruncateTo(UnitHour) }},
		{"DateTruncate_day", func() (Series, error) { return s.DateTruncate("day") }},
		{"LtTime", func() (Series, error) { return s.LtTime(time.Date(2024, 1, 5, 0, 0, 0, 0, time.UTC)) }},
		{"RollingBy_1m_sum", func() (Series, error) {
			a := NewFloat64Series("a", make([]float64, s.Len()), nil)
			defer a.col.Release()
			f, err := NewFrameFromSeries(s, a)
			if err != nil {
				return Series{}, err
			}
			defer f.Release()
			r, err := f.RollingBy("ts", time.Minute)
			if err != nil {
				return Series{}, err
			}
			return r.Agg("a", AggSum)
		}},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				out, err := tc.fn()
				if err != nil {
					b.Fatal(err)
				}
				out.col.Release()
			}
		})
	}
}
