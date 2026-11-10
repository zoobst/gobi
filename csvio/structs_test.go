package csvio_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zoobst/gobi"
	"github.com/zoobst/gobi/csvio"
)

type csvStructRow struct {
	ID       int64  `csv:"id"`
	Name     string `csv:"name" gobi:"name_fallback"`
	Fallback string `gobi:"fallback"` // no csv tag → gobi wins
	Skip     string `csv:"-"`         // omitted
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

// TestStructs_RejectNulls — CSV can't tell "" from a missing value: an
// empty string written by WriteStructs reads back as NULL, so a plain
// string field is rejected and an `optional` one isn't.
func TestStructs_RejectNulls(t *testing.T) {
	type row struct {
		ID   int64  `csv:"id"`
		Name string `csv:"name"`
	}
	path := filepath.Join(t.TempDir(), "rows.csv")
	if err := csvio.WriteStructs([]row{{ID: 1, Name: ""}}, path, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := csvio.ReadStructs[row](path, nil, gobi.StructRejectNulls()); !errors.Is(err, gobi.ErrStructFieldNull) || !strings.Contains(err.Error(), `"name"`) {
		t.Errorf("plain string: err = %v", err)
	}
	type lenient struct {
		ID   int64  `csv:"id"`
		Name string `csv:"name,optional"`
	}
	rows, err := csvio.ReadStructs[lenient](path, nil, gobi.StructRejectNulls())
	if err != nil || len(rows) != 1 || rows[0].ID != 1 {
		t.Errorf("optional: %+v, %v", rows, err)
	}
	// An empty numeric cell is a missing number: rejected.
	if err := os.WriteFile(path, []byte("id,name\n,x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := csvio.ReadStructs[lenient](path, nil, gobi.StructRejectNulls()); !errors.Is(err, gobi.ErrStructFieldNull) || !strings.Contains(err.Error(), `"id"`) {
		t.Errorf("empty number: err = %v", err)
	}
}
