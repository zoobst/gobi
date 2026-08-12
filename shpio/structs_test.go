package shpio_test

import (
	"path/filepath"
	"testing"

	"github.com/zoobst/gobi/geometry"
	"github.com/zoobst/gobi/shpio"
)

type shpStructRow struct {
	// shp tag aliases to 10-char-safe DBF field names. DBF stores
	// numeric fields as float64 regardless of source Go type, so we
	// use float64 here for a clean round-trip.
	ID         float64 `shp:"OBJECTID"`
	Name       string  `shp:"NAME"`
	Population float64 `shp:"POP10" gobi:"pop_generic"` // shp: wins over gobi:
	Skip       string  `shp:"-"`                        // omitted
	// Geometry stored as WKB string round-trippable via gobi.
	Geom string `shp:"geometry" geom:"true"`
}

func TestSHPReadWriteStructs_Roundtrip(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "features")

	// A single Point per row so we exercise the geometry pipeline
	// without pulling in polygon-orientation edge cases.
	rows := []shpStructRow{
		{
			ID: 1, Name: "NYC", Population: 8804190,
			Skip: "gone",
			Geom: geometry.Point{X: -74.006, Y: 40.7128, CRSValue: geometry.WGS84}.WKT(),
		},
		{
			ID: 2, Name: "LA", Population: 3898747,
			Skip: "gone",
			Geom: geometry.Point{X: -118.2437, Y: 34.0522, CRSValue: geometry.WGS84}.WKT(),
		},
	}
	if err := shpio.WriteStructs(rows, base, nil); err != nil {
		t.Fatalf("WriteStructs: %v", err)
	}
	got, err := shpio.ReadStructs[shpStructRow](base, nil)
	if err != nil {
		t.Fatalf("ReadStructs: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].Name != "NYC" || got[1].Name != "LA" {
		t.Errorf("names: %+v", got)
	}
	// shp: tag should have written the column as POP10, not "Population"
	// or "pop_generic". ReadStructs matches back via the same shp: tag.
	if got[0].Population != 8804190 {
		t.Errorf("NYC pop = %v, want 8804190", got[0].Population)
	}
	if got[0].Skip != "" {
		t.Errorf("Skip = %q, want empty (shp:\"-\")", got[0].Skip)
	}
}
