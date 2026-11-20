package gobi

import (
	"errors"
	"strings"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/zoobst/gobi/geometry"
)

func TestGeometryFieldCRS(t *testing.T) {
	field := func(v string, set bool) arrow.Field {
		f := arrow.Field{Name: "g", Type: arrow.BinaryTypes.Binary}
		if set {
			f.Metadata = arrow.NewMetadata([]string{MetaGeometryType, MetaGeometryCRS}, []string{"WKB", v})
		}
		return f
	}
	for v, want := range map[string]int32{"4326": 4326, "3857": 3857, "0": 0, "": 0} {
		if got, err := GeometryFieldCRS(field(v, true)); err != nil || got != want {
			t.Errorf("%q: %d, %v; want %d", v, got, err, want)
		}
	}
	if got, err := GeometryFieldCRS(field("", false)); err != nil || got != 0 {
		t.Errorf("missing tag: %d, %v", got, err)
	}
	for _, v := range []string{"EPSG:3857", "3857abc", " 4326", "4326.0", "abc", "99999999999"} {
		if _, err := GeometryFieldCRS(field(v, true)); !errors.Is(err, ErrInvalidGeometryCRS) {
			t.Errorf("%q: err = %v, want ErrInvalidGeometryCRS", v, err)
		}
	}
}

// TestGeometryOps_RejectUnreadableCRS — geometry operations, column-
// vs-column predicates and Frame.String on a malformed CRS tag.
func TestGeometryOps_RejectUnreadableCRS(t *testing.T) {
	b := array.NewBinaryBuilder(memory.DefaultAllocator, arrow.BinaryTypes.Binary)
	b.Append(geometry.WKB(geometry.NewPolygon([][]geometry.Point{{{X: 0, Y: 0}, {X: 1, Y: 0}, {X: 1, Y: 1}, {X: 0, Y: 0}}}, geometry.CRS{})))
	md := arrow.NewMetadata([]string{MetaGeometryType, MetaGeometryCRS}, []string{"WKB", "EPSG:3857"})
	g := SeriesFromArray(arrow.Field{Name: "g", Type: arrow.BinaryTypes.Binary, Nullable: true, Metadata: md}, b.NewArray())
	b.Release()
	f, err := NewFrameFromSeries(g)
	g.Column().Release()
	if err != nil {
		t.Fatal(err)
	}
	defer f.Release()
	s, _ := f.Column("g")

	if _, err := s.GeomArea(geometry.UnitMeters); !errors.Is(err, ErrInvalidGeometryCRS) {
		t.Errorf("GeomArea: err = %v", err)
	}
	if _, err := s.GeomCentroid(); !errors.Is(err, ErrInvalidGeometryCRS) {
		t.Errorf("GeomCentroid: err = %v", err)
	}
	if _, err := f.Lazy().Select(Col("g").GeomIntersects(Col("g"))).Collect(); !errors.Is(err, ErrInvalidGeometryCRS) {
		t.Errorf("column-vs-column predicate: err = %v", err)
	}
	if _, err := f.Lazy().Select(Col("g").GeomForceZ(1)).Collect(); !errors.Is(err, ErrInvalidGeometryCRS) {
		t.Errorf("GeomForceZ: err = %v", err)
	}
	if str := f.String(); !strings.Contains(str, `unreadable CRS "EPSG:3857"`) {
		t.Errorf("String() = %q", str)
	}
}
