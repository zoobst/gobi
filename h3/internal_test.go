package h3

import (
	"strings"
	"testing"

	"github.com/zoobst/gobi"
)

// TestGridDisk_ListOffsetLimit — a batch whose disks together pass the
// List<Int64> child limit is an error, not an offset overflow. The
// limit is lowered so the test needn't allocate 2^31 cells.
func TestGridDisk_ListOffsetLimit(t *testing.T) {
	defer func(old int) { listChildLimit = old }(listChildLimit)
	listChildLimit = 20 // a k=1 disk is 7 cells: rows 1-2 fit, row 3 doesn't

	cell := int64(0x8828308281fffff)
	f, err := gobi.NewFrameFromSeries(gobi.NewInt64Series("cell", []int64{cell, cell, cell}, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Release()
	_, err = f.Lazy().Select(GridDisk(gobi.Col("cell"), 1)).Collect()
	if err == nil || !strings.Contains(err.Error(), "List<Int64> limit") {
		t.Errorf("err = %v", err)
	}
	two, err := f.Take([]int{0, 1})
	if err != nil {
		t.Fatal(err)
	}
	defer two.Release()
	if _, err := two.Lazy().Select(GridDisk(gobi.Col("cell"), 1)).Collect(); err != nil {
		t.Errorf("within limit: %v", err)
	}
}
