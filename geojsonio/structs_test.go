package geojsonio_test

import (
	"path/filepath"
	"testing"

	"github.com/zoobst/gobi/geojsonio"
)

type gjRow struct {
	Name   string `geojson:"name" gobi:"name_generic"`
	Pop    int64  `geojson:"population"`
	Note   string `gobi:"note"` // no geojson tag → gobi fallback
	Ignore string `geojson:"-"` // skipped
	Geom   []byte `geom:"true"`
}

func TestGeoJSONReadWriteStructs_Roundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.geojson")
	rows := []gjRow{
		{Name: "A", Pop: 10, Note: "hello", Ignore: "gone", Geom: []byte("dontcare")},
		{Name: "B", Pop: 20, Note: "world", Ignore: "gone", Geom: []byte("dontcare")},
	}
	// Zero-out geometry — using pure attribute round-trip; geometry
	// serialization is exercised elsewhere.
	for i := range rows {
		rows[i].Geom = nil
	}
	if err := geojsonio.WriteStructs(rows, path, nil); err != nil {
		t.Fatalf("WriteStructs: %v", err)
	}
	got, err := geojsonio.ReadStructs[gjRow](path, nil)
	if err != nil {
		t.Fatalf("ReadStructs: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	for i := range rows {
		if got[i].Name != rows[i].Name || got[i].Pop != rows[i].Pop {
			t.Errorf("row %d = %+v, want name/pop like %+v", i, got[i], rows[i])
		}
		if got[i].Note != rows[i].Note {
			t.Errorf("row %d Note = %q, want %q (gobi tag)", i, got[i].Note, rows[i].Note)
		}
		if got[i].Ignore != "" {
			t.Errorf("row %d Ignore = %q, want empty (geojson:\"-\")", i, got[i].Ignore)
		}
	}
}
