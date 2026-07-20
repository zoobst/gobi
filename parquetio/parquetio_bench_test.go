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
		df, err := parquetio.ReadFile(path, &parquetio.Options{
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
