package gobi

import (
	"testing"

	"github.com/apache/arrow/go/v18/arrow/array"
	"github.com/apache/arrow/go/v18/arrow/memory"
)

// sink prevents the compiler from eliminating benchmark bodies.
var (
	sinkSeries Series
	sinkFloat  float64
)

const benchN = 1 << 20 // 1,048,576 rows

func benchFloatSeries(name string, n int) Series {
	b := array.NewFloat64Builder(memory.DefaultAllocator)
	defer b.Release()
	vals := make([]float64, n)
	for i := range vals {
		vals[i] = float64(i) * 0.5
	}
	b.AppendValues(vals, nil)
	return newSeriesFromArray(name, b.NewArray())
}

func benchInt64Series(name string, n int) Series {
	b := array.NewInt64Builder(memory.DefaultAllocator)
	defer b.Release()
	vals := make([]int64, n)
	for i := range vals {
		vals[i] = int64(i)
	}
	b.AppendValues(vals, nil)
	return newSeriesFromArray(name, b.NewArray())
}

func BenchmarkSeries_Add_Float64_1M(b *testing.B) {
	a := benchFloatSeries("a", benchN)
	c := benchFloatSeries("c", benchN)
	b.ResetTimer()
	b.ReportAllocs()
	for range b.N {
		out, err := a.Add(c)
		if err != nil {
			b.Fatal(err)
		}
		sinkSeries = out
	}
}

func BenchmarkSeries_Add_Int64_1M(b *testing.B) {
	a := benchInt64Series("a", benchN)
	c := benchInt64Series("c", benchN)
	b.ResetTimer()
	b.ReportAllocs()
	for range b.N {
		out, err := a.Add(c)
		if err != nil {
			b.Fatal(err)
		}
		sinkSeries = out
	}
}

func BenchmarkSeries_Mul_Float64_1M(b *testing.B) {
	a := benchFloatSeries("a", benchN)
	c := benchFloatSeries("c", benchN)
	b.ResetTimer()
	b.ReportAllocs()
	for range b.N {
		out, err := a.Mul(c)
		if err != nil {
			b.Fatal(err)
		}
		sinkSeries = out
	}
}

func BenchmarkSeries_Sum_Float64_1M(b *testing.B) {
	a := benchFloatSeries("a", benchN)
	b.ResetTimer()
	b.ReportAllocs()
	for range b.N {
		v, err := a.Sum()
		if err != nil {
			b.Fatal(err)
		}
		sinkFloat = v
	}
}

func BenchmarkSeries_GtScalar_Float64_1M(b *testing.B) {
	a := benchFloatSeries("a", benchN)
	b.ResetTimer()
	b.ReportAllocs()
	for range b.N {
		out, err := a.GtScalar(1000)
		if err != nil {
			b.Fatal(err)
		}
		sinkSeries = out
	}
}
