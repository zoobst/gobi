package parquetio

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/zoobst/gobi"
)

// A parallel scan with Metadata counts row groups from it: the file
// isn't opened (here it doesn't even exist) and its footer isn't read.
func TestCountRowGroups_usesMetadata(t *testing.T) {
	ids := make([]int64, 3_000)
	f, err := gobi.NewFrameFromSeries(gobi.NewInt64Series("id", ids, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Release()
	var buf bytes.Buffer
	if err := Write(f, &buf, &WriteOptions{RowGroupRows: 1_000}); err != nil {
		t.Fatal(err)
	}
	md, err := ReadMetadata(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "absent.parquet")
	if n, ok := countRowGroups(missing, &ReadOptions{Metadata: md}); !ok || n != 3 {
		t.Errorf("countRowGroups = %d, %v; want 3 from the metadata", n, ok)
	}
	if _, ok := countRowGroups(missing, nil); ok {
		t.Error("without Metadata, a missing file must fall back to the serial path")
	}
	if subs := partitionRowGroups(missing, &ReadOptions{Metadata: md, ScanWorkers: 3}); len(subs) != 3 {
		t.Errorf("partitionRowGroups made %d workers, want 3", len(subs))
	}
}
