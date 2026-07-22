package parquetio_test

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

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
		"":            parquetio.CodecUncompressed,
		"NONE":        parquetio.CodecUncompressed,
		"snappy":      parquetio.CodecSnappy,
		"Gzip":        parquetio.CodecGzip,
		"gz":          parquetio.CodecGzip,
		"br":          parquetio.CodecBrotli,
		"zstd":        parquetio.CodecZstd,
		"lz4":         parquetio.CodecLZ4,
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
