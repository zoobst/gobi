package parquetio_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/apache/arrow-go/v18/parquet/file"

	"github.com/zoobst/gobi"
	"github.com/zoobst/gobi/parquetio"
)

type structTestRow struct {
	ID      int64   `parquet:"id"`
	Name    string  `parquet:"name" gobi:"name_fallback"`
	Score   float64 `gobi:"score"` // no parquet tag; gobi: is used
	Ignored string  `parquet:"-"`  // skipped
}

func TestWriteReadStructs_Roundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "structs.parquet")

	rows := []structTestRow{
		{ID: 1, Name: "alice", Score: 3.14, Ignored: "skipme"},
		{ID: 2, Name: "bob", Score: 2.71, Ignored: "gone"},
	}
	if err := parquetio.WriteStructs(rows, path, nil); err != nil {
		t.Fatalf("WriteStructs: %v", err)
	}

	got, err := parquetio.ReadStructs[structTestRow](path, nil)
	if err != nil {
		t.Fatalf("ReadStructs: %v", err)
	}
	if len(got) != len(rows) {
		t.Fatalf("row count = %d, want %d", len(got), len(rows))
	}
	for i, r := range got {
		if r.ID != rows[i].ID {
			t.Errorf("row %d ID = %v, want %v", i, r.ID, rows[i].ID)
		}
		if r.Name != rows[i].Name {
			t.Errorf("row %d Name = %q, want %q", i, r.Name, rows[i].Name)
		}
		if r.Score != rows[i].Score {
			t.Errorf("row %d Score = %v, want %v", i, r.Score, rows[i].Score)
		}
		// Ignored was omitted from the file — after read it's the zero value.
		if r.Ignored != "" {
			t.Errorf("row %d Ignored = %q, want empty (field was parquet:\"-\")", i, r.Ignored)
		}
	}
}

// TestWriteStructs_UsesParquetTagOverCsv verifies the parquet: tag
// wins over a csv: tag on the same field. This is the whole point of
// per-io tag namespaces.
func TestWriteStructs_UsesParquetTagOverCsv(t *testing.T) {
	type Row struct {
		Val int64 `parquet:"pq_col" csv:"csv_col"`
	}
	rows := []Row{{Val: 42}}
	dir := t.TempDir()
	path := filepath.Join(dir, "tag_priority.parquet")

	if err := parquetio.WriteStructs(rows, path, nil); err != nil {
		t.Fatalf("WriteStructs: %v", err)
	}

	// Peek at the schema — column should be "pq_col", not "csv_col".
	schema, err := parquetio.ReadSchema(path, nil)
	if err != nil {
		t.Fatalf("ReadSchema: %v", err)
	}
	if _, ok := schema.FieldsByName("pq_col"); !ok {
		t.Errorf("column pq_col not found; parquet: tag should win over csv:")
	}
	if _, ok := schema.FieldsByName("csv_col"); ok {
		t.Errorf("column csv_col present; csv: tag should have been shadowed by parquet:")
	}
}

func TestReadStructs_MissingFile(t *testing.T) {
	_, err := parquetio.ReadStructs[structTestRow]("/does/not/exist.parquet", nil)
	if err == nil {
		t.Fatal("expected error for missing file")
	}
	// Sanity: the file we referenced really doesn't exist.
	if _, statErr := os.Stat("/does/not/exist.parquet"); statErr == nil {
		t.Fatalf("test precondition failed: file exists")
	}
}

// TestWriteStructs_TimestampTag — the parquet-go `timestamp(unit)`
// option lands as the matching parquet logical type, UTC-adjusted by
// default, and round-trips.
func TestWriteStructs_TimestampTag(t *testing.T) {
	type row struct {
		Micros time.Time `parquet:"micros,timestamp(microsecond)"`
		Local  time.Time `parquet:"local,timestamp(millisecond:local)"`
		Plain  time.Time `parquet:"plain"`
	}
	ts := time.Date(2024, 3, 15, 9, 30, 0, 123456789, time.UTC)
	path := filepath.Join(t.TempDir(), "ts.parquet")
	if err := parquetio.WriteStructs([]row{{ts, ts, ts}}, path, nil); err != nil {
		t.Fatalf("WriteStructs: %v", err)
	}
	lt := parquetLogicalTypes(t, path)
	want := map[string]string{
		"micros": "Timestamp(isAdjustedToUTC=true, timeUnit=microseconds",
		"local":  "Timestamp(isAdjustedToUTC=false, timeUnit=milliseconds",
		"plain":  "Timestamp(isAdjustedToUTC=false, timeUnit=nanoseconds",
	}
	for col, prefix := range want {
		if !strings.HasPrefix(lt[col], prefix) {
			t.Errorf("%s logical type = %q, want prefix %q", col, lt[col], prefix)
		}
	}
	got, err := parquetio.ReadStructs[row](path, nil)
	if err != nil {
		t.Fatalf("ReadStructs: %v", err)
	}
	if !got[0].Micros.Equal(ts.Truncate(time.Microsecond)) ||
		!got[0].Local.Equal(ts.Truncate(time.Millisecond)) ||
		!got[0].Plain.Equal(ts) {
		t.Errorf("got %+v", got[0])
	}
}

// TestWrite_CoerceTimestamps — frames not built from structs (csvio,
// Expr output) carry Timestamp[ns]; CoerceTimestamps rewrites them.
func TestWrite_CoerceTimestamps(t *testing.T) {
	type row struct {
		TS time.Time `parquet:"ts"`
	}
	exact := time.Date(2024, 3, 15, 9, 30, 0, 123456000, time.UTC) // whole µs
	lossy := exact.Add(789 * time.Nanosecond)

	write := func(ts time.Time, opts *parquetio.WriteOptions) (string, error) {
		f, err := gobi.FromStructs([]row{{ts}}, gobi.StructTagFormat("parquet"))
		if err != nil {
			t.Fatal(err)
		}
		defer f.Release()
		path := filepath.Join(t.TempDir(), "c.parquet")
		return path, parquetio.WriteFile(f, path, opts)
	}

	path, err := write(exact, &parquetio.WriteOptions{CoerceTimestamps: parquetio.TimestampMicros})
	if err != nil {
		t.Fatalf("exact: %v", err)
	}
	if lt := parquetLogicalTypes(t, path)["ts"]; !strings.Contains(lt, "timeUnit=microseconds") {
		t.Errorf("logical type = %q, want microseconds", lt)
	}
	got, err := parquetio.ReadStructs[row](path, nil)
	if err != nil || !got[0].TS.Equal(exact) {
		t.Errorf("read back = %v, %v; want %v", got, err, exact)
	}

	if _, err := write(lossy, &parquetio.WriteOptions{CoerceTimestamps: parquetio.TimestampMicros}); err == nil {
		t.Error("lossy coerce without AllowTruncatedTimestamps: want error")
	}
	path, err = write(lossy, &parquetio.WriteOptions{
		CoerceTimestamps: parquetio.TimestampMicros, AllowTruncatedTimestamps: true,
	})
	if err != nil {
		t.Fatalf("lossy + allow: %v", err)
	}
	if got, _ := parquetio.ReadStructs[row](path, nil); !got[0].TS.Equal(exact) {
		t.Errorf("truncated read back = %v, want %v", got[0].TS, exact)
	}

	if _, err := write(exact, &parquetio.WriteOptions{CoerceTimestamps: "seconds"}); err == nil {
		t.Error("invalid unit: want error")
	}
}

// parquetLogicalTypes maps each top-level column to its parquet
// logical-type string.
func parquetLogicalTypes(t *testing.T, path string) map[string]string {
	t.Helper()
	pf, err := file.OpenParquetFile(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer pf.Close()
	sc := pf.MetaData().Schema
	out := make(map[string]string, sc.NumColumns())
	for i := range sc.NumColumns() {
		c := sc.Column(i)
		out[c.Name()] = c.LogicalType().String()
	}
	return out
}
