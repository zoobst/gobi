package kmlio_test

import (
	"path/filepath"
	"testing"

	"github.com/zoobst/gobi/geometry"
	"github.com/zoobst/gobi/kmlio"
)

type kmlStructRow struct {
	Name        string `kml:"name" gobi:"name_generic"`
	Description string `kml:"description"`
	Skip        string `kml:"-"`
	Geom        string `kml:"geometry" geom:"true"`
}

func TestKMLReadWriteStructs_Roundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.kml")
	rows := []kmlStructRow{
		{
			Name: "NYC", Description: "Big Apple", Skip: "gone",
			Geom: geometry.Point{X: -74.006, Y: 40.7128, CRSValue: geometry.WGS84}.WKT(),
		},
	}
	if err := kmlio.WriteStructs(rows, path, nil); err != nil {
		t.Fatalf("WriteStructs: %v", err)
	}
	got, err := kmlio.ReadStructs[kmlStructRow](path, nil)
	if err != nil {
		t.Fatalf("ReadStructs: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if got[0].Name != "NYC" {
		t.Errorf("Name = %q, want NYC", got[0].Name)
	}
	if got[0].Description != "Big Apple" {
		t.Errorf("Description = %q, want Big Apple", got[0].Description)
	}
	if got[0].Skip != "" {
		t.Errorf("Skip = %q, want empty (kml:\"-\")", got[0].Skip)
	}
}
