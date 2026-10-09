package gobi

import (
	"context"
	"fmt"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// buildBenchGroupFrame returns a frame with n rows, `groups` distinct string
// keys, and two numeric columns (revenue float64, units int64).
func buildBenchGroupFrame(b *testing.B, n, groups int) *Frame {
	b.Helper()
	pool := memory.DefaultAllocator
	keyB := array.NewStringBuilder(pool)
	defer keyB.Release()
	revB := array.NewFloat64Builder(pool)
	defer revB.Release()
	uniB := array.NewInt64Builder(pool)
	defer uniB.Release()

	for i := range n {
		keyB.Append(fmt.Sprintf("k%d", i%groups))
		revB.Append(float64(i) * 0.5)
		uniB.Append(int64(i))
	}
	fields := []arrow.Field{
		{Name: "key", Type: arrow.BinaryTypes.String, Nullable: false},
		{Name: "revenue", Type: arrow.PrimitiveTypes.Float64, Nullable: true},
		{Name: "units", Type: arrow.PrimitiveTypes.Int64, Nullable: true},
	}
	schema := arrow.NewSchema(fields, nil)
	arrs := []arrow.Array{keyB.NewArray(), revB.NewArray(), uniB.NewArray()}
	defer func() {
		for _, a := range arrs {
			a.Release()
		}
	}()
	cols := make([]arrow.Column, 3)
	for i, a := range arrs {
		chunked := arrow.NewChunked(a.DataType(), []arrow.Array{a})
		cols[i] = *arrow.NewColumn(fields[i], chunked)
	}
	f, err := NewFrame(schema, cols)
	if err != nil {
		b.Fatal(err)
	}
	return f
}

func BenchmarkGroupBy_100k_by_100(b *testing.B) {
	f := buildBenchGroupFrame(b, 100_000, 100)

	b.ReportAllocs()
	for b.Loop() {
		gb, err := f.GroupBy("key")
		if err != nil {
			b.Fatal(err)
		}
		out, err := gb.Agg(
			Aggregation{Column: "revenue", Kind: AggSum},
			Aggregation{Column: "revenue", Kind: AggMean},
			Aggregation{Column: "units", Kind: AggMax},
		)
		if err != nil {
			b.Fatal(err)
		}
		sinkFrame = out
	}
}

// aggBenchFrame builds a fixture with nGroups distinct group keys and
// rowsPerGroup rows per group, each with a nullable provider string
// drawn from a small set. Used to compare the streaming vs
// materializing paths for collect-set aggregation.
func aggBenchFrame(b testing.TB, nGroups, rowsPerGroup int) *Frame {
	b.Helper()
	pool := memory.DefaultAllocator

	regionB := array.NewStringBuilder(pool)
	defer regionB.Release()
	providerB := array.NewStringBuilder(pool)
	defer providerB.Release()

	providers := []string{"att", "verizon", "tmobile", "sprint"}
	for g := range nGroups {
		region := fmt.Sprintf("g%06d", g)
		for r := range rowsPerGroup {
			regionB.Append(region)
			providerB.Append(providers[r%len(providers)])
		}
	}

	fields := []arrow.Field{
		{Name: "region", Type: arrow.BinaryTypes.String, Nullable: false},
		{Name: "provider", Type: arrow.BinaryTypes.String, Nullable: false},
	}
	schema := arrow.NewSchema(fields, nil)
	arrs := []arrow.Array{regionB.NewArray(), providerB.NewArray()}
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
		b.Fatal(err)
	}
	return f
}

// legacyOnlyAggregator wraps setAggregator[string] behind an
// Aggregator-only interface — deliberately hides the
// IncrementalAggregator methods so the pipeline routes through the
// materializing fallback. Used to benchmark the pre-unification
// baseline against the new streaming path.
type legacyOnlyAggregator struct {
	inner *setAggregator[string]
}

func newLegacyOnly() Aggregator {
	return &legacyOnlyAggregator{inner: NewStringSetAggregator().(*setAggregator[string])}
}

func (a *legacyOnlyAggregator) Aggregate(s Series, rows []int) (any, error) {
	return a.inner.Aggregate(s, rows)
}
func (a *legacyOnlyAggregator) Merge(other Aggregator) error {
	o, ok := other.(*legacyOnlyAggregator)
	if !ok {
		return fmt.Errorf("legacyOnlyAggregator.Merge: peer is %T", other)
	}
	return a.inner.Merge(o.inner)
}
func (a *legacyOnlyAggregator) Type() arrow.DataType { return a.inner.Type() }
func (a *legacyOnlyAggregator) Name() string         { return a.inner.Name() }

// BenchmarkAggregator_CollectSet_Materialize routes through the
// materializing fallback via legacyOnlyAggregator (which hides
// IncrementalAggregator methods). Baseline for the streaming win.
func BenchmarkAggregator_CollectSet_Materialize(b *testing.B) {
	f := aggBenchFrame(b, 100, 1000) // 100k rows, 100 groups
	ctx := context.Background()
	b.ReportAllocs()

	for b.Loop() {
		op, err := Compile(Optimize(f.Lazy().
			GroupBy("region").
			Agg(Aggregation{Column: "provider", Fn: newLegacyOnly(), Alias: "s"}).
			Plan()))
		if err != nil {
			b.Fatal(err)
		}
		out, err := Execute(ctx, op)
		if err != nil {
			b.Fatal(err)
		}
		_ = out
	}
}

// BenchmarkAggregator_CollectSet_Streaming routes through the
// streaming aggregate executor via NewStringSetAggregator (which
// implements IncrementalAggregator). Should show substantially lower
// allocations vs the materialize path.
func BenchmarkAggregator_CollectSet_Streaming(b *testing.B) {
	f := aggBenchFrame(b, 100, 1000) // 100k rows, 100 groups
	ctx := context.Background()
	b.ReportAllocs()

	for b.Loop() {
		op, err := Compile(Optimize(f.Lazy().
			GroupBy("region").
			Agg(Aggregation{Column: "provider", Fn: NewStringSetAggregator(), Alias: "s"}).
			Plan()))
		if err != nil {
			b.Fatal(err)
		}
		out, err := Execute(ctx, op)
		if err != nil {
			b.Fatal(err)
		}
		_ = out
	}
}
