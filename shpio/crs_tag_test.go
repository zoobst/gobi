package shpio_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/zoobst/gobi"
	"github.com/zoobst/gobi/geometry"
	"github.com/zoobst/gobi/shpio"
)

// TestWrite_UnreadableCRSTag — a malformed gobi:crs_epsg tag fails the
// write rather than silently omitting the .prj.
func TestWrite_UnreadableCRSTag(t *testing.T) {
	b := array.NewBinaryBuilder(memory.DefaultAllocator, arrow.BinaryTypes.Binary)
	b.Append(geometry.WKB(geometry.NewPoint(1, 2, geometry.CRS{})))
	md := arrow.NewMetadata([]string{gobi.MetaGeometryType, gobi.MetaGeometryCRS}, []string{"WKB", "EPSG:4326"})
	g := gobi.SeriesFromArray(arrow.Field{Name: "geometry", Type: arrow.BinaryTypes.Binary, Nullable: true, Metadata: md}, b.NewArray())
	b.Release()
	f, err := gobi.NewFrameFromSeries(g)
	g.Column().Release()
	if err != nil {
		t.Fatal(err)
	}
	defer f.Release()
	base := filepath.Join(t.TempDir(), "pts.shp")
	if err := shpio.WriteFile(f, base, nil); !errors.Is(err, gobi.ErrInvalidGeometryCRS) {
		t.Errorf("err = %v, want ErrInvalidGeometryCRS", err)
	}
	for _, ext := range []string{".shp", ".shx", ".dbf", ".prj"} {
		if _, err := os.Stat(filepath.Join(filepath.Dir(base), "pts"+ext)); err == nil {
			t.Errorf("%s written despite the unreadable tag", ext)
		}
	}
}
