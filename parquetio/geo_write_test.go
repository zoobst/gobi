package parquetio_test

import (
	"bytes"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/apache/arrow-go/v18/parquet/file"
	"github.com/zoobst/gobi"
	"github.com/zoobst/gobi/geometry"
	"github.com/zoobst/gobi/parquetio"
)

// pointFrame builds rows with lon / lat columns and a point geometry
// at (lon + dx, lat). dx = 0 makes lon / lat a valid covering.
func pointFrame(t *testing.T, lons, lats []float64, dx float64) *gobi.Frame {
	t.Helper()
	pool := memory.DefaultAllocator
	lonB := array.NewFloat64Builder(pool)
	defer lonB.Release()
	latB := array.NewFloat64Builder(pool)
	defer latB.Release()
	gB := array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary)
	defer gB.Release()
	for i := range lons {
		lonB.Append(lons[i])
		latB.Append(lats[i])
		gB.Append(geometry.WKB(geometry.Point{X: lons[i] + dx, Y: lats[i]}))
	}
	fields := []arrow.Field{
		{Name: "lon", Type: arrow.PrimitiveTypes.Float64},
		{Name: "lat", Type: arrow.PrimitiveTypes.Float64},
		gobi.GeometryField("geometry", int32(geometry.PseudoMercator.EPSG)),
	}
	arrs := []arrow.Array{lonB.NewArray(), latB.NewArray(), gB.NewArray()}
	cols := make([]arrow.Column, len(fields))
	for i, a := range arrs {
		ch := arrow.NewChunked(a.DataType(), []arrow.Array{a})
		a.Release()
		cols[i] = *arrow.NewColumn(fields[i], ch)
		ch.Release()
	}
	f, err := gobi.NewFrame(arrow.NewSchema(fields, nil), cols)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.Release)
	return f
}

// twoClusters: 100 points near (10, 10) then 100 near (5000, 5000).
func twoClusters(t *testing.T) *gobi.Frame {
	var lons, lats []float64
	for i := range 200 {
		c := 10.0
		if i >= 100 {
			c = 5000
		}
		lons = append(lons, c+float64(i%100)*0.01)
		lats = append(lats, c+float64(i%100)*0.01)
	}
	return pointFrame(t, lons, lats, 0)
}

var pointCovering = map[string]parquetio.Covering{"geometry": parquetio.PointCovering("lon", "lat")}

func aoiAround(x, y float64) geometry.Polygon {
	return geometry.SimplePolygon([]geometry.Point{
		{X: x - 100, Y: y - 100}, {X: x + 100, Y: y - 100}, {X: x + 100, Y: y + 100},
		{X: x - 100, Y: y + 100}, {X: x - 100, Y: y - 100},
	}, geometry.PseudoMercator)
}

// TestCovering_PointsUseExistingColumns — no generated bbox columns,
// the geo covering names lon / lat, lon / lat stay visible on read,
// and spatial pushdown prunes from their stats.
func TestCovering_PointsUseExistingColumns(t *testing.T) {
	df := twoClusters(t)
	path := filepath.Join(t.TempDir(), "points.parquet")
	if err := parquetio.WriteFile(df, path, &parquetio.WriteOptions{
		RowGroupRows: 100,
		Coverings:    pointCovering,
	}); err != nil {
		t.Fatal(err)
	}

	all, err := parquetio.ReadFile(path, &parquetio.ReadOptions{IncludeCoveringColumns: true})
	if err != nil {
		t.Fatal(err)
	}
	defer all.Release()
	if cols := all.ColumnNames(); slices.ContainsFunc(cols, func(c string) bool { return strings.Contains(c, "_bbox_") }) {
		t.Errorf("generated covering columns written: %v", cols)
	}

	raw := footerValues(t, path, gobi.GeoParquetMetadataKey)
	if len(raw) != 1 {
		t.Fatalf("geo entries = %d", len(raw))
	}
	meta, err := gobi.ParseGeoParquetMetadata(raw[0])
	if err != nil {
		t.Fatal(err)
	}
	bb := meta.Columns["geometry"].Covering.Bbox
	if bb.Xmin[0] != "lon" || bb.Ymin[0] != "lat" || bb.Xmax[0] != "lon" || bb.Ymax[0] != "lat" {
		t.Errorf("covering = %+v, want lon/lat", bb)
	}

	// Default read: lon / lat are real data and must stay visible.
	def, err := parquetio.ReadFile(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer def.Release()
	if cols := def.ColumnNames(); !slices.Contains(cols, "lon") || !slices.Contains(cols, "lat") {
		t.Errorf("default read hid declared covering columns: %v", cols)
	}

	// Pushdown: the AOI only touches cluster A's row group.
	out, err := parquetio.ReadFile(path, &parquetio.ReadOptions{
		Predicate: gobi.Col("geometry").GeomIntersects(gobi.Lit(aoiAround(10, 10))),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer out.Release()
	if n := out.NumRows(); n != 100 {
		t.Errorf("rows with pushdown = %d, want 100 (cluster B not pruned via lon/lat stats)", n)
	}
}

// TestCovering_MatchesGeneratedPruning — a declared point covering
// prunes exactly like the generated columns, with fewer bytes.
func TestCovering_MatchesGeneratedPruning(t *testing.T) {
	df := twoClusters(t)
	dir := t.TempDir()
	gen, decl := filepath.Join(dir, "gen.parquet"), filepath.Join(dir, "decl.parquet")
	if err := parquetio.WriteFile(df, gen, &parquetio.WriteOptions{RowGroupRows: 100}); err != nil {
		t.Fatal(err)
	}
	if err := parquetio.WriteFile(df, decl, &parquetio.WriteOptions{RowGroupRows: 100, Coverings: pointCovering}); err != nil {
		t.Fatal(err)
	}
	for _, aoi := range []geometry.Polygon{aoiAround(10, 10), aoiAround(5000, 5000), aoiAround(-9000, -9000)} {
		count := func(path string) int {
			f, err := parquetio.ReadFile(path, &parquetio.ReadOptions{
				Predicate: gobi.Col("geometry").GeomIntersects(gobi.Lit(aoi)),
			})
			if err != nil {
				t.Fatal(err)
			}
			defer f.Release()
			return f.NumRows()
		}
		if g, d := count(gen), count(decl); g != d {
			t.Errorf("AOI %v: generated covering read %d rows, declared read %d", aoi.Bounds(), g, d)
		}
	}
}

// TestCovering_RejectsCoveringThatMisses — a geometry outside its
// declared covering would be pruned away by readers; the write fails.
func TestCovering_RejectsCoveringThatMisses(t *testing.T) {
	df := pointFrame(t, []float64{1, 2, 3}, []float64{1, 2, 3}, 0.5) // geometry x = lon + 0.5
	err := parquetio.Write(df, &bytes.Buffer{}, &parquetio.WriteOptions{Coverings: pointCovering})
	if err == nil || !strings.Contains(err.Error(), "not inside covering") {
		t.Fatalf("err = %v, want covering violation", err)
	}

	// Writer: the bad batch is rejected, the writer stays usable.
	good := pointFrame(t, []float64{1}, []float64{1}, 0)
	w, err := parquetio.NewWriter(&bytes.Buffer{}, good.Schema(), &parquetio.WriteOptions{Coverings: pointCovering})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Write(df); err == nil {
		t.Error("Writer.Write with a missing covering: want error")
	}
	if err := w.Write(good); err != nil {
		t.Errorf("Writer.Write after a rejected batch: %v", err)
	}
	if _, err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestCovering_OptionValidation — bad Coverings / KeyValueMetadata
// fail before any work.
func TestCovering_OptionValidation(t *testing.T) {
	df := pointFrame(t, []float64{1}, []float64{1}, 0)
	cases := map[string]*parquetio.WriteOptions{
		"not a geometry column": {Coverings: map[string]parquetio.Covering{"lon": parquetio.PointCovering("lon", "lat")}},
		"missing geometry":      {Coverings: map[string]parquetio.Covering{"nope": parquetio.PointCovering("lon", "lat")}},
		"missing covering col":  {Coverings: map[string]parquetio.Covering{"geometry": parquetio.PointCovering("x", "lat")}},
		"non-numeric covering":  {Coverings: map[string]parquetio.Covering{"geometry": parquetio.PointCovering("geometry", "lat")}},
		"ARROW:schema key":      {KeyValueMetadata: map[string]string{"ARROW:schema": "x"}},
		"geo key with geometry": {KeyValueMetadata: map[string]string{"geo": "{}"}},
	}
	for name, opts := range cases {
		if err := parquetio.Write(df, &bytes.Buffer{}, opts); err == nil {
			t.Errorf("Write %s: want error", name)
		}
		if _, err := parquetio.NewWriter(&bytes.Buffer{}, df.Schema(), opts); err == nil {
			t.Errorf("NewWriter %s: want error", name)
		}
	}
}

// TestKeyValueMetadata_Footer — entries land once each, replace a
// same-named schema key, and a custom "geo" is allowed when the frame
// has no geometry column.
func TestKeyValueMetadata_Footer(t *testing.T) {
	df := pointFrame(t, []float64{1}, []float64{1}, 0)
	md := arrow.NewMetadata([]string{"owner", "keep"}, []string{"schema-owner", "schema-keep"})
	cols := make([]arrow.Column, df.NumCols())
	for i, name := range df.ColumnNames() {
		s, _ := df.Column(name)
		cols[i] = *s.Column()
	}
	tagged, err := gobi.NewFrame(arrow.NewSchema(df.Schema().Fields(), &md), cols)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "kv.parquet")
	if err := parquetio.WriteFile(tagged, path, &parquetio.WriteOptions{
		KeyValueMetadata: map[string]string{"owner": "etl", "iceberg.schema": "{}"},
	}); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{"owner": "etl", "keep": "schema-keep", "iceberg.schema": "{}"} {
		if got := footerValues(t, path, key); len(got) != 1 || got[0] != want {
			t.Errorf("footer %q = %v, want [%s]", key, got, want)
		}
	}
	if got := footerValues(t, path, gobi.GeoParquetMetadataKey); len(got) != 1 {
		t.Errorf("geo entries = %d, want 1", len(got))
	}

	// Writer carries KeyValueMetadata to the footer too.
	var buf bytes.Buffer
	w, err := parquetio.NewWriter(&buf, df.Schema(), &parquetio.WriteOptions{KeyValueMetadata: map[string]string{"k": "v"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Write(df); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if got := footerKV(t, buf.Bytes(), "k"); len(got) != 1 || got[0] != "v" {
		t.Errorf("Writer footer k = %v", got)
	}

	// No geometry column: a hand-built "geo" entry is the caller's to set.
	type plain struct{ X float64 }
	pf, err := gobi.FromStructs([]plain{{1}})
	if err != nil {
		t.Fatal(err)
	}
	defer pf.Release()
	p2 := filepath.Join(t.TempDir(), "custom_geo.parquet")
	custom := `{"version":"1.1.0","primary_column":"geometry","columns":{}}`
	if err := parquetio.WriteFile(pf, p2, &parquetio.WriteOptions{
		KeyValueMetadata: map[string]string{"geo": custom},
	}); err != nil {
		t.Fatalf("custom geo without geometry column: %v", err)
	}
	if got := footerValues(t, p2, "geo"); len(got) != 1 || got[0] != custom {
		t.Errorf("custom geo footer = %v", got)
	}
}

// TestCovering_MixedAndHilbert — one geometry column declared, another
// generated; HilbertSort still works with a declared covering.
func TestCovering_MixedAndHilbert(t *testing.T) {
	df := twoClusters(t)
	// Add a second geometry column (copy of the first) that keeps the
	// generated covering.
	g, _ := df.Column("geometry")
	fields := append(slices.Clone(df.Schema().Fields()),
		gobi.GeometryField("geom2", int32(geometry.PseudoMercator.EPSG)))
	cols := make([]arrow.Column, 0, len(fields))
	for _, name := range df.ColumnNames() {
		s, _ := df.Column(name)
		cols = append(cols, *s.Column())
	}
	cols = append(cols, *arrow.NewColumn(fields[len(fields)-1], g.Column().Data()))
	two, err := gobi.NewFrame(arrow.NewSchema(fields, nil), cols)
	if err != nil {
		t.Fatal(err)
	}

	for _, hilbert := range []bool{false, true} {
		path := filepath.Join(t.TempDir(), "mixed.parquet")
		if err := parquetio.WriteFile(two, path, &parquetio.WriteOptions{
			Coverings:   pointCovering,
			HilbertSort: hilbert,
		}); err != nil {
			t.Fatalf("hilbert=%v: %v", hilbert, err)
		}
		f, err := parquetio.ReadFile(path, &parquetio.ReadOptions{IncludeCoveringColumns: true})
		if err != nil {
			t.Fatal(err)
		}
		names := f.ColumnNames()
		f.Release()
		if slices.Contains(names, "geometry_bbox_xmin") || !slices.Contains(names, "geom2_bbox_xmin") {
			t.Errorf("hilbert=%v: columns = %v, want geom2 covering only", hilbert, names)
		}
		meta, _ := gobi.ParseGeoParquetMetadata(footerValues(t, path, gobi.GeoParquetMetadataKey)[0])
		if meta.Columns["geometry"].Covering.Bbox.Xmin[0] != "lon" ||
			meta.Columns["geom2"].Covering.Bbox.Xmin[0] != "geom2_bbox_xmin" {
			t.Errorf("hilbert=%v: coverings = %+v / %+v", hilbert,
				meta.Columns["geometry"].Covering.Bbox, meta.Columns["geom2"].Covering.Bbox)
		}
	}
}

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
