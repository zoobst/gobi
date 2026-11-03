package gpkgio_test

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/zoobst/gobi"
	"github.com/zoobst/gobi/geometry"
	"github.com/zoobst/gobi/gpkgio"
)

// frameOf builds a Frame from JSON column values, plus a point
// geometry column "geom" with one point per row.
func frameOf(t *testing.T, fields []arrow.Field, values ...string) *gobi.Frame {
	t.Helper()
	pool := memory.DefaultAllocator
	arrs := make([]arrow.Array, 0, len(fields)+1)
	for i, f := range fields {
		a, _, err := array.FromJSON(pool, f.Type, strings.NewReader(values[i]))
		if err != nil {
			t.Fatal(err)
		}
		arrs = append(arrs, a)
	}
	gb := array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary)
	for i := range arrs[0].Len() {
		gb.Append(geometry.WKB(geometry.Point{X: float64(i), Y: float64(i)}))
	}
	arrs = append(arrs, gb.NewArray())
	gb.Release()
	fields = append(fields, gobi.GeometryField("geom", 4326))
	cols := make([]arrow.Column, len(arrs))
	for i, a := range arrs {
		ch := arrow.NewChunked(a.DataType(), []arrow.Array{a})
		cols[i] = *arrow.NewColumn(fields[i], ch)
		ch.Release()
		a.Release()
	}
	f, err := gobi.NewFrame(arrow.NewSchema(fields, nil), cols)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func colJSON(t *testing.T, f *gobi.Frame, name string) string {
	t.Helper()
	s, err := f.Column(name)
	if err != nil {
		t.Fatal(err)
	}
	a, err := array.Concatenate(s.Column().Data().Chunks(), memory.DefaultAllocator)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Release()
	b, err := a.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestDateTime_WrittenPerSpec — timestamps are DATETIME columns of
// ISO-8601 UTC text to the millisecond, dates DATE columns of
// "YYYY-MM-DD", whatever the arrow unit or time zone.
func TestDateTime_WrittenPerSpec(t *testing.T) {
	// 1704164645 s = 2024-01-02T03:04:05Z; 19724 days = 2024-01-02.
	df := frameOf(t, []arrow.Field{
		{Name: "ts_ns", Type: &arrow.TimestampType{Unit: arrow.Nanosecond}, Nullable: true},
		{Name: "ts_ny", Type: &arrow.TimestampType{Unit: arrow.Second, TimeZone: "America/New_York"}, Nullable: true},
		{Name: "d32", Type: arrow.FixedWidthTypes.Date32, Nullable: true},
		{Name: "d64", Type: arrow.FixedWidthTypes.Date64, Nullable: true},
	},
		`[1704164645123456789, null]`,
		`[1704164645, 0]`,
		`[19724, null]`,
		`[1704153600000, 0]`,
	)
	defer df.Release()
	path := filepath.Join(t.TempDir(), "dt.gpkg")
	if err := gpkgio.WriteFile(df, path, &gpkgio.WriteOptions{Layer: "events"}); err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	decl := map[string]string{}
	rows, err := db.Query(`SELECT name, type FROM pragma_table_info('events')`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var n, ty string
		if err := rows.Scan(&n, &ty); err != nil {
			t.Fatal(err)
		}
		decl[n] = ty
	}
	rows.Close()
	for col, want := range map[string]string{"ts_ns": "DATETIME", "ts_ny": "DATETIME", "d32": "DATE", "d64": "DATE"} {
		if decl[col] != want {
			t.Errorf("%s declared %q, want %q", col, decl[col], want)
		}
	}

	// typeof + the stored text, bypassing the driver's time parsing.
	var got []string
	rows, err = db.Query(`SELECT typeof(ts_ns) || ' ' || coalesce(CAST(ts_ns AS TEXT), 'NULL'),
		CAST(ts_ny AS TEXT), CAST(d32 AS TEXT), CAST(d64 AS TEXT) FROM events ORDER BY fid`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var a string
		var b, c, d sql.NullString
		if err := rows.Scan(&a, &b, &c, &d); err != nil {
			t.Fatal(err)
		}
		got = append(got, a, b.String, c.String, d.String)
	}
	rows.Close()
	want := []string{
		"text 2024-01-02T03:04:05.123Z", "2024-01-02T03:04:05.000Z", "2024-01-02", "2024-01-02",
		"null NULL", "1970-01-01T00:00:00.000Z", "", "1970-01-01",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("stored:\n got %q\nwant %q", got, want)
	}

	// Reading back: DATETIME → Timestamp[ms, UTC], DATE → Date32.
	back, err := gpkgio.ReadFile(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer back.Release()
	for col, want := range map[string]string{
		"ts_ns": "timestamp[ms, tz=UTC]", "ts_ny": "timestamp[ms, tz=UTC]", "d32": "date32", "d64": "date32",
	} {
		s, err := back.Column(col)
		if err != nil {
			t.Fatal(err)
		}
		if got := s.DataType().String(); got != want {
			t.Errorf("%s read as %s, want %s", col, got, want)
		}
	}
	for col, want := range map[string]string{
		"ts_ns": `["2024-01-02T03:04:05.123Z",null]`,
		"d32":   `["2024-01-02",null]`,
		"d64":   `["2024-01-02","1970-01-01"]`,
	} {
		if got := colJSON(t, back, col); got != want {
			t.Errorf("%s = %s, want %s", col, got, want)
		}
	}
}

// TestDateTime_ReadsOtherWriters — DATETIME / DATE columns as GDAL and
// QGIS write them read as Timestamp / Date32, including other ISO-8601
// spellings; a number in a DATETIME column is an error, not a guess.
func TestDateTime_ReadsOtherWriters(t *testing.T) {
	df := frameOf(t, []arrow.Field{{Name: "id", Type: arrow.PrimitiveTypes.Int64}}, `[1, 2, 3]`)
	defer df.Release()
	path := filepath.Join(t.TempDir(), "gdal.gpkg")
	if err := gpkgio.WriteFile(df, path, &gpkgio.WriteOptions{Layer: "l"}); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{
		`ALTER TABLE l ADD COLUMN t DATETIME`,
		`ALTER TABLE l ADD COLUMN d DATE`,
		`UPDATE l SET t = '2024-01-02T03:04:05.123Z', d = '2024-01-02' WHERE id = 1`,
		`UPDATE l SET t = '2024-01-02T05:04:05+02:00' WHERE id = 2`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	back, err := gpkgio.ReadFile(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := colJSON(t, back, "t") + colJSON(t, back, "d"); got != `["2024-01-02T03:04:05.123Z","2024-01-02T03:04:05Z",null]["2024-01-02",null,null]` {
		t.Errorf("t, d = %s", got)
	}
	back.Release()

	if _, err := db.Exec(`UPDATE l SET t = 1704164645 WHERE id = 3`); err != nil {
		t.Fatal(err)
	}
	if _, err := gpkgio.ReadFile(path, nil); err == nil || !strings.Contains(err.Error(), "not ISO-8601") {
		t.Errorf("numeric DATETIME: err = %v, want a not-ISO-8601 error", err)
	}
}
