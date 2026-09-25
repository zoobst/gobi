package parquetio_test

import (
	"path/filepath"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/parquet/file"

	"github.com/zoobst/gobi"
	"github.com/zoobst/gobi/parquetio"
)

// footerValues returns every footer value stored under key, in order.
func footerValues(t *testing.T, path, key string) []string {
	t.Helper()
	pf, err := file.OpenParquetFile(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer pf.Close()
	var out []string
	for _, kv := range pf.MetaData().KeyValueMetadata() {
		if kv.Key == key {
			out = append(out, kv.GetValue())
		}
	}
	return out
}

// TestWrite_RoundTripKeepsOneGeoKey — reading a GeoParquet file and
// writing it back must leave exactly one "geo" footer entry, and it
// must describe the new file. The read side puts the source file's
// "geo" into the frame's schema metadata; before, pqarrow copied that
// into the new footer and gobi appended a fresh one after it, so
// readers (which take the first) saw the stale entry.
func TestWrite_RoundTripKeepsOneGeoKey(t *testing.T) {
	type row struct {
		ID   int64  `parquet:"id"`
		Geom string `parquet:"geometry" geom:"true"`
	}
	f, err := gobi.FromStructs([]row{{1, "POINT(1 2)"}, {2, "POINT(3 4)"}}, gobi.StructTagFormat("parquet"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Release()
	dir := t.TempDir()
	src := filepath.Join(dir, "src.parquet")
	if err := parquetio.WriteFile(f, src, nil); err != nil { // with bbox covering
		t.Fatal(err)
	}

	back, err := parquetio.ReadFile(src, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer back.Release()
	// Rewrite without the covering: a stale entry would still claim it.
	dst := filepath.Join(dir, "dst.parquet")
	if err := parquetio.WriteFile(back, dst, &parquetio.WriteOptions{SkipBboxCovering: true}); err != nil {
		t.Fatal(err)
	}

	geos := footerValues(t, dst, gobi.GeoParquetMetadataKey)
	if len(geos) != 1 {
		t.Fatalf("dst has %d geo footer entries, want 1", len(geos))
	}
	meta, err := gobi.ParseGeoParquetMetadata(geos[0])
	if err != nil {
		t.Fatal(err)
	}
	if cm, ok := meta.Columns["geometry"]; !ok || cm.Covering != nil {
		t.Errorf("dst geo = %+v, want geometry column with no covering (stale entry leaked)", meta.Columns)
	}
	// The source did have a covering, so the test would catch a leak.
	srcMeta, _ := gobi.ParseGeoParquetMetadata(footerValues(t, src, gobi.GeoParquetMetadataKey)[0])
	if srcMeta.Columns["geometry"].Covering == nil {
		t.Fatal("test setup: source file has no covering")
	}
}

// TestWrite_NonGeoSchemaMetadataPreserved — only "geo" is replaced;
// other schema-level keys still reach the footer.
func TestWrite_NonGeoSchemaMetadataPreserved(t *testing.T) {
	type row struct {
		ID   int64  `parquet:"id"`
		Geom string `parquet:"geometry" geom:"true"`
	}
	f, err := gobi.FromStructs([]row{{1, "POINT(1 2)"}}, gobi.StructTagFormat("parquet"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Release()
	md := arrow.NewMetadata([]string{"owner", gobi.GeoParquetMetadataKey}, []string{"etl", `{"stale":true}`})
	cols := make([]arrow.Column, f.NumCols())
	for i := range cols {
		s, _ := f.Column(f.ColumnNames()[i])
		cols[i] = *s.Column()
	}
	tagged, err := gobi.NewFrame(arrow.NewSchema(f.Schema().Fields(), &md), cols)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "md.parquet")
	if err := parquetio.WriteFile(tagged, path, nil); err != nil {
		t.Fatal(err)
	}
	if got := footerValues(t, path, "owner"); len(got) != 1 || got[0] != "etl" {
		t.Errorf("owner footer = %v, want [etl]", got)
	}
	geos := footerValues(t, path, gobi.GeoParquetMetadataKey)
	if len(geos) != 1 || geos[0] == `{"stale":true}` {
		t.Errorf("geo footer = %v, want one gobi-generated entry", geos)
	}
}
