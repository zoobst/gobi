package gpkgio_test

import (
	"path/filepath"
	"testing"

	"github.com/zoobst/gobi/geometry"
	"github.com/zoobst/gobi/gpkgio"

	_ "modernc.org/sqlite"
)

type gpkgStructRow struct {
	ID    int64   `gpkg:"fid" gobi:"id_generic"` // gpkg: wins
	Name  string  `gpkg:"name"`
	Value float64 `gobi:"value"`   // no gpkg tag → gobi fallback
	Skip  string  `gpkg:"-"`
	Geom  string  `gpkg:"geom" geom:"true"`
}

func TestGPKGReadWriteStructs_Roundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "structs.gpkg")

	rows := []gpkgStructRow{
		{ID: 1, Name: "a", Value: 1.5, Skip: "gone",
			Geom: geometry.Point{X: 0, Y: 0, CRSValue: geometry.WGS84}.WKT()},
		{ID: 2, Name: "b", Value: 2.5, Skip: "gone",
			Geom: geometry.Point{X: 1, Y: 1, CRSValue: geometry.WGS84}.WKT()},
	}
	if err := gpkgio.WriteStructs(rows, path, &gpkgio.WriteOptions{Layer: "features"}); err != nil {
		t.Fatalf("WriteStructs: %v", err)
	}
	got, err := gpkgio.ReadStructs[gpkgStructRow](path, &gpkgio.ReadOptions{Layer: "features"})
	if err != nil {
		t.Fatalf("ReadStructs: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].ID != 1 || got[0].Name != "a" || got[0].Value != 1.5 {
		t.Errorf("row 0 = %+v", got[0])
	}
	if got[0].Skip != "" {
		t.Errorf("Skip = %q, want empty (gpkg:\"-\")", got[0].Skip)
	}
}
