package parquetio

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zoobst/gobi"
)

// TestSerialColumnDecode_ReachesArrow — the option sets arrow-go's
// per-column Parallel flag (default on).
func TestSerialColumnDecode_ReachesArrow(t *testing.T) {
	type row struct{ A, B int64 }
	f, err := gobi.FromStructs([]row{{1, 2}})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Release()
	path := filepath.Join(t.TempDir(), "p.parquet")
	if err := WriteFile(f, path, nil); err != nil {
		t.Fatal(err)
	}
	for _, serial := range []bool{false, true} {
		fh, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		rc, err := openReaderFromRS(fh, fh, &ReadOptions{SerialColumnDecode: serial})
		if err != nil {
			t.Fatal(err)
		}
		if rc.reader.Props.Parallel == serial {
			t.Errorf("SerialColumnDecode=%v: arrow Parallel=%v", serial, rc.reader.Props.Parallel)
		}
		rc.close()
	}
}
