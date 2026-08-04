package csvio_test

import (
	"strings"
	"testing"

	"github.com/zoobst/gobi/csvio"
)

type csvStructRow struct {
	ID     int64  `csv:"id"`
	Name   string `csv:"name" gobi:"name_fallback"`
	Fallback string `gobi:"fallback"` // no csv tag → gobi wins
	Skip   string `csv:"-"`         // omitted
}

func TestCSVReadStructs_Roundtrip(t *testing.T) {
	// Fallback column comes from gobi tag; Skip's column absent from CSV.
	data := `id,name,fallback
1,alice,f-alice
2,bob,f-bob
`
	rows, err := csvio.ReadStructsReader[csvStructRow](strings.NewReader(data), nil)
	if err != nil {
		t.Fatalf("ReadStructsReader: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("len = %d, want 2", len(rows))
	}
	if rows[0].ID != 1 || rows[0].Name != "alice" || rows[0].Fallback != "f-alice" {
		t.Errorf("row 0 = %+v", rows[0])
	}
	if rows[0].Skip != "" {
		t.Errorf("Skip should be zero value (csv:\"-\"), got %q", rows[0].Skip)
	}
}
