package gobi

import (
	"fmt"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// benchStrings is a 1M-row String Series of short distinct values,
// every 16th null.
func benchStrings(b *testing.B) Series {
	b.Helper()
	sb := array.NewStringBuilder(memory.DefaultAllocator)
	defer sb.Release()
	for i := range 1_000_000 {
		if i%16 == 0 {
			sb.AppendNull()
			continue
		}
		sb.Append(fmt.Sprintf("row-%07d", i))
	}
	return SeriesFromArray(arrow.Field{Name: "s", Type: arrow.BinaryTypes.String, Nullable: true}, sb.NewArray())
}

func BenchmarkSeries_StrContains_1M(b *testing.B) {
	s := benchStrings(b)
	defer s.col.Release()
	b.ReportAllocs()
	for b.Loop() {
		out, err := s.StrContains("99")
		if err != nil {
			b.Fatal(err)
		}
		out.col.Release()
	}
}

func BenchmarkSeries_StrLen_1M(b *testing.B) {
	s := benchStrings(b)
	defer s.col.Release()
	b.ReportAllocs()
	for b.Loop() {
		out, err := s.StrLen()
		if err != nil {
			b.Fatal(err)
		}
		out.col.Release()
	}
}

// BenchmarkSeries_StrToDate_1M parses a column that repeats one value
// (a partition key: hits the one-entry memo) and one where every row
// differs (parses every row).
func BenchmarkSeries_StrToDate_1M(b *testing.B) {
	for _, tc := range []struct {
		name string
		at   func(i int) string
	}{
		{"constant", func(int) string { return "2024-01-02" }},
		{"distinct", func(i int) string { return fmt.Sprintf("%04d-%02d-%02d", 1000+i%9000, 1+i%12, 1+i%28) }},
	} {
		b.Run(tc.name, func(b *testing.B) {
			sb := array.NewStringBuilder(memory.DefaultAllocator)
			for i := range 1_000_000 {
				sb.Append(tc.at(i))
			}
			s := SeriesFromArray(arrow.Field{Name: "dt", Type: arrow.BinaryTypes.String}, sb.NewArray())
			sb.Release()
			defer s.col.Release()
			b.ReportAllocs()
			for b.Loop() {
				out, err := s.StrToDate("")
				if err != nil {
					b.Fatal(err)
				}
				out.col.Release()
			}
		})
	}
}
