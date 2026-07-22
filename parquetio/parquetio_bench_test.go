package parquetio_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zoobst/gobi"
	"github.com/zoobst/gobi/parquetio"
)

var sinkFrame *gobi.Frame

// benchFixture resolves the shared benchmarks/bench.parquet fixture built
// by benchmarks/generate_fixture.go. Skips the benchmark if the fixture
// isn't present so `go test -bench` in CI without pre-generation is a
// no-op rather than a failure.
func benchFixture(b *testing.B) string {
	b.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		b.Fatal(err)
	}
	// From parquetio/, the fixture is one level up under benchmarks/.
	path := filepath.Join(cwd, "..", "benchmarks", "bench.parquet")
	if _, err := os.Stat(path); err != nil {
		b.Skipf("fixture missing (%v) — run `go run generate_fixture.go` in benchmarks/", err)
	}
	return path
}

// BenchmarkReadFile_1M measures whole-file materialization of the
// 1,048,576-row bench.parquet. Compare peak allocs and ns/op against
// BenchmarkStream_1M below to see the memory story of streaming.
func BenchmarkReadFile_1M(b *testing.B) {
	path := benchFixture(b)
	b.ResetTimer()
	b.ReportAllocs()
	for b.Loop() {
		df, err := parquetio.ReadFile(path, nil)
		if err != nil {
			b.Fatal(err)
		}
		sinkFrame = df
	}
}

// BenchmarkReadFile_1M_Projected reads only two columns of the four.
// The delta vs BenchmarkReadFile_1M is the projection savings.
func BenchmarkReadFile_1M_Projected(b *testing.B) {
	path := benchFixture(b)
	b.ResetTimer()
	b.ReportAllocs()
	for b.Loop() {
		df, err := parquetio.ReadFile(path, &parquetio.ReadOptions{
			Columns: []string{"id", "value_a"},
		})
		if err != nil {
			b.Fatal(err)
		}
		sinkFrame = df
	}
}

// BenchmarkStream_1M measures record-batch streaming of the same 1M-row
// fixture. Each callback invocation receives one arrow batch's worth of
// rows; only that batch is materialized at a time.
func BenchmarkStream_1M(b *testing.B) {
	path := benchFixture(b)
	b.ResetTimer()
	b.ReportAllocs()
	var total int
	for b.Loop() {
		total = 0
		err := parquetio.ReadFileChunksFunc(path, nil, func(f *gobi.Frame) error {
			total += f.NumRows()
			return nil
		})
		if err != nil {
			b.Fatal(err)
		}
	}
	if total == 0 {
		b.Fatal("streamed 0 rows")
	}
}

// -- LazyFrame + optimizer benchmarks --------------------------------------
//
// These three benchmarks isolate the value the optimizer's projection-
// pushdown rule delivers. All three read {id, value_a} from bench.parquet;
// what differs is how the projection reaches the reader.
//
//   Lazy_Optimized : ScanFile(path).Select(id, value_a).Collect()
//                    → optimizer pushes projection into the scan; the reader
//                      only decodes 2 of 4 columns.
//
//   Lazy_Raw       : ScanFile(path).Select(id, value_a).CollectRaw()
//                    → optimizer bypassed; ScanFile reads all 4 columns,
//                      then Project drops two after materialization.
//
//   Eager_Projected: parquetio.ReadFile(path, ReadOptions{Columns: [id, value_a]})
//                    → same as Lazy_Optimized but bypassing LazyFrame entirely,
//                      the baseline for what "perfect pushdown" costs.
//
// Expected relationship at steady state: Lazy_Optimized ≈ Eager_Projected,
// Lazy_Raw ≥ BenchmarkReadFile_1M. Delta between Optimized and Raw is the
// pushdown win.

func BenchmarkLazy_ProjectionPushdown_Optimized(b *testing.B) {
	path := benchFixture(b)
	b.ResetTimer()
	b.ReportAllocs()
	for b.Loop() {
		df, err := parquetio.ScanFile(path, nil).
			Select(gobi.Col("id"), gobi.Col("value_a")).
			Collect()
		if err != nil {
			b.Fatal(err)
		}
		sinkFrame = df
	}
}

func BenchmarkLazy_ProjectionPushdown_Raw(b *testing.B) {
	path := benchFixture(b)
	b.ResetTimer()
	b.ReportAllocs()
	for b.Loop() {
		df, err := parquetio.ScanFile(path, nil).
			Select(gobi.Col("id"), gobi.Col("value_a")).
			CollectRaw()
		if err != nil {
			b.Fatal(err)
		}
		sinkFrame = df
	}
}

// BenchmarkOptimize_Only isolates the cost of the rule loop itself — no
// I/O, no execution. Builds a moderately deep plan (5 nodes over a
// scan-frame source) and times Optimize repeatedly. If the number is
// meaningful compared to Collect's wall time, we should know.
func BenchmarkOptimize_Only(b *testing.B) {
	// Build once, outside the timed loop.
	path := benchFixture(b)
	df, err := parquetio.ReadFile(path, nil)
	if err != nil {
		b.Fatal(err)
	}
	// A representative pipeline: filter → sort → filter → project → limit.
	lf := df.Lazy().
		Filter(gobi.Col("value_a").Gt(gobi.Lit(1_000.0))).
		SortBy(gobi.SortKey{Column: "id"}).
		Filter(gobi.Col("value_b").Lt(gobi.Lit(999_000.0))).
		Select(gobi.Col("id"), gobi.Col("value_a"), gobi.Col("key")).
		Limit(100)
	plan := lf.Plan()

	b.ResetTimer()
	b.ReportAllocs()
	var sink gobi.LogicalPlan
	for b.Loop() {
		sink = gobi.Optimize(plan)
	}
	_ = sink
}
