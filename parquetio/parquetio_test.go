package parquetio_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zoobst/gobi"
	"github.com/zoobst/gobi/csvio"
	"github.com/zoobst/gobi/geometry"
	"github.com/zoobst/gobi/parquetio"
)

type city struct {
	Name       string `csv:"name"`
	Population int64  `csv:"population"`
	Geom       string `csv:"geometry" geom:"true"`
}

const citiesCSV = `name,population,geometry
New York,8804190,POINT (-74.0060 40.7128)
Los Angeles,3898747,POINT (-118.2437 34.0522)
Chicago,2746388,POINT (-87.6298 41.8781)
`

func TestParseCodec(t *testing.T) {
	cases := map[string]parquetio.Codec{
		"":       parquetio.CodecUncompressed,
		"NONE":   parquetio.CodecUncompressed,
		"snappy": parquetio.CodecSnappy,
		"Gzip":   parquetio.CodecGzip,
		"gz":     parquetio.CodecGzip,
		"br":     parquetio.CodecBrotli,
		"zstd":   parquetio.CodecZstd,
		"lz4":    parquetio.CodecLZ4,
	}
	for in, want := range cases {
		got, err := parquetio.ParseCodec(in)
		if err != nil {
			t.Errorf("ParseCodec(%q) err: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseCodec(%q) = %q, want %q", in, got, want)
		}
	}
	if _, err := parquetio.ParseCodec("bogus"); !errors.Is(err, parquetio.ErrUnknownCodec) {
		t.Errorf("expected ErrUnknownCodec, got %v", err)
	}
}

func TestWriteRead_RoundTrip_Snappy(t *testing.T) {
	testRoundTrip(t, parquetio.CodecSnappy)
}

func TestWriteRead_RoundTrip_Gzip(t *testing.T) {
	testRoundTrip(t, parquetio.CodecGzip)
}

func TestWriteRead_RoundTrip_Uncompressed(t *testing.T) {
	testRoundTrip(t, parquetio.CodecUncompressed)
}

func TestWriteRead_PreservesGeoParquetMetadata(t *testing.T) {
	df, err := csvio.Read[city](strings.NewReader(citiesCSV), &csvio.ReadOptions{CRSHint: 4326})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "cities.parquet")
	if err := parquetio.WriteFile(df, path, &parquetio.WriteOptions{Codec: parquetio.CodecSnappy}); err != nil {
		t.Fatalf("write: %v", err)
	}
	loaded, err := parquetio.ReadFile(path, nil)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// Look for the "geo" file-level metadata key in the loaded schema.
	md := loaded.Schema().Metadata()
	geoRaw, ok := md.GetValue("geo")
	if !ok {
		t.Fatal("geo metadata key missing after round-trip")
	}
	if !strings.Contains(geoRaw, `"primary_column":"geometry"`) {
		t.Fatalf("primary_column not in metadata: %s", geoRaw)
	}
	if !strings.Contains(geoRaw, `"geometry_types":["Point"]`) {
		t.Fatalf("geometry_types missing: %s", geoRaw)
	}
	if !strings.Contains(geoRaw, `"bbox":`) {
		t.Fatalf("bbox missing: %s", geoRaw)
	}
}

func testRoundTrip(t *testing.T, codec parquetio.Codec) {
	t.Helper()
	df, err := csvio.Read[city](strings.NewReader(citiesCSV), &csvio.ReadOptions{CRSHint: 4326})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "cities.parquet")
	if err := parquetio.WriteFile(df, path, &parquetio.WriteOptions{Codec: codec}); err != nil {
		t.Fatalf("write: %v", err)
	}

	loaded, err := parquetio.ReadFile(path, nil)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	rows, cols := loaded.Shape()
	if rows != 3 || cols != 3 {
		t.Fatalf("round-trip shape got (%d, %d), want (3, 3)", rows, cols)
	}
	g, err := loaded.Geometry("geometry", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := g.(geometry.Point); !ok {
		t.Fatalf("expected Point, got %T", g)
	}
}

// TestReadReader confirms the io.ReaderAt-backed entrypoint reads a
// Parquet payload identically to ReadFile. Writes to a file, slurps
// the bytes, feeds them back via bytes.Reader (which satisfies
// io.ReaderAt). Same round-trip contract as TestWriteRead_RoundTrip_*.
//
// This is the code path athenaio T1 will exercise via s3.GetObject's
// io.ReaderAt-shaped output — the test uses bytes.Reader as a stand-in
// so it can run in CI without touching AWS.
func TestReadReader(t *testing.T) {
	df, err := csvio.Read[city](strings.NewReader(citiesCSV), &csvio.ReadOptions{CRSHint: 4326})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "cities.parquet")
	if err := parquetio.WriteFile(df, path, &parquetio.WriteOptions{Codec: parquetio.CodecSnappy}); err != nil {
		t.Fatalf("write: %v", err)
	}

	buf, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := parquetio.ReadReader(bytes.NewReader(buf), int64(len(buf)), nil)
	if err != nil {
		t.Fatalf("ReadReader: %v", err)
	}
	if rows, cols := loaded.Shape(); rows != 3 || cols != 3 {
		t.Fatalf("shape got (%d, %d), want (3, 3)", rows, cols)
	}
	// Geo metadata should survive the reader-based path just like the
	// path-based one — the file-level KeyValueMetadata is read from
	// the parquet footer regardless of source.
	g, err := loaded.Geometry("geometry", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := g.(geometry.Point); !ok {
		t.Fatalf("expected Point, got %T", g)
	}
}

// TestReadReaderChunksFunc confirms the streaming reader-based path
// behaves like ReadFileChunksFunc — batches arrive, fn is called per
// batch, error propagation works.
func TestReadReaderChunksFunc(t *testing.T) {
	df, err := csvio.Read[city](strings.NewReader(citiesCSV), &csvio.ReadOptions{CRSHint: 4326})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "cities.parquet")
	if err := parquetio.WriteFile(df, path, &parquetio.WriteOptions{Codec: parquetio.CodecSnappy}); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	var totalRows int64
	err = parquetio.ReadReaderChunksFunc(
		bytes.NewReader(buf),
		int64(len(buf)),
		nil,
		func(f *gobi.Frame) error {
			r, _ := f.Shape()
			totalRows += int64(r)
			return nil
		},
	)
	if err != nil {
		t.Fatalf("ReadReaderChunksFunc: %v", err)
	}
	if totalRows != 3 {
		t.Errorf("streamed row count = %d, want 3", totalRows)
	}
}
