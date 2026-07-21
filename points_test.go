package gobi

import (
	"errors"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/zoobst/gobi/geometry"
)

// makeFloat64Series builds a single-chunk Float64 Series named name
// with the given values. valid=nil means all values are valid.
func makeFloat64Series(t *testing.T, name string, vals []float64, valid []bool) Series {
	t.Helper()
	pool := memory.DefaultAllocator
	b := array.NewFloat64Builder(pool)
	defer b.Release()
	b.AppendValues(vals, valid)
	arr := b.NewArray()
	defer arr.Release()
	field := arrow.Field{Name: name, Type: arrow.PrimitiveTypes.Float64, Nullable: true}
	chunked := arrow.NewChunked(field.Type, []arrow.Array{arr})
	return NewSeries(arrow.NewColumn(field, chunked))
}

func TestPointsFromXY_BasicRoundTrip(t *testing.T) {
	// Longitude, latitude for NYC, LA, Chicago.
	lng := makeFloat64Series(t, "lng", []float64{-74.006, -118.2437, -87.6298}, nil)
	lat := makeFloat64Series(t, "lat", []float64{40.7128, 34.0522, 41.8781}, nil)

	geom, err := PointsFromXY(lng, lat, 4326)
	if err != nil {
		t.Fatal(err)
	}
	if geom.Len() != 3 {
		t.Fatalf("len = %d, want 3", geom.Len())
	}
	if !geom.IsGeometry() {
		t.Fatal("returned Series should be tagged as geometry")
	}

	// WKB survives: decode each row and confirm coordinates.
	arr := geom.Column().Data().Chunks()[0].(*array.Binary)
	for i, want := range []geometry.Point{
		{X: -74.006, Y: 40.7128},
		{X: -118.2437, Y: 34.0522},
		{X: -87.6298, Y: 41.8781},
	} {
		g, err := geometry.ParseWKB(arr.Value(i))
		if err != nil {
			t.Fatalf("row %d WKB parse: %v", i, err)
		}
		p, ok := g.(geometry.Point)
		if !ok {
			t.Fatalf("row %d not a Point: %T", i, g)
		}
		if p.X != want.X || p.Y != want.Y {
			t.Errorf("row %d = (%v, %v), want (%v, %v)",
				i, p.X, p.Y, want.X, want.Y)
		}
	}
}

func TestPointsFromXY_CRSStamped(t *testing.T) {
	lng := makeFloat64Series(t, "lng", []float64{0}, nil)
	lat := makeFloat64Series(t, "lat", []float64{0}, nil)

	geom, err := PointsFromXY(lng, lat, 3857)
	if err != nil {
		t.Fatal(err)
	}
	if got := geometryCRSFromField(geom.field); got != 3857 {
		t.Fatalf("stamped CRS = %d, want 3857", got)
	}
}

func TestPointsFromXY_NullInputProducesNullGeometry(t *testing.T) {
	// Row 1 has a null lng; row 2 has a null lat. Both rows should
	// produce a null geometry.
	lng := makeFloat64Series(t, "lng",
		[]float64{-74, 0, -87},
		[]bool{true, false, true},
	)
	lat := makeFloat64Series(t, "lat",
		[]float64{40, 34, 0},
		[]bool{true, true, false},
	)

	geom, err := PointsFromXY(lng, lat, 4326)
	if err != nil {
		t.Fatal(err)
	}
	arr := geom.Column().Data().Chunks()[0].(*array.Binary)
	if arr.IsNull(0) {
		t.Errorf("row 0 should be non-null")
	}
	if !arr.IsNull(1) {
		t.Errorf("row 1 should be null (lng null)")
	}
	if !arr.IsNull(2) {
		t.Errorf("row 2 should be null (lat null)")
	}
}

func TestPointsFromXY_LenMismatch(t *testing.T) {
	lng := makeFloat64Series(t, "lng", []float64{0, 1}, nil)
	lat := makeFloat64Series(t, "lat", []float64{0}, nil)
	_, err := PointsFromXY(lng, lat, 4326)
	if !errors.Is(err, ErrColumnLenMismatch) {
		t.Fatalf("want ErrColumnLenMismatch, got %v", err)
	}
}

func TestPointsFromXY_MixedNumericTypesPromoteToFloat64(t *testing.T) {
	// lng is Float64, lat is Int64. numericAt promotes int64→float64
	// silently, so PointsFromXY should accept the mix.
	pool := memory.DefaultAllocator
	lng := makeFloat64Series(t, "lng", []float64{-74.5}, nil)

	latB := array.NewInt64Builder(pool)
	defer latB.Release()
	latB.AppendValues([]int64{40}, nil)
	latArr := latB.NewArray()
	defer latArr.Release()
	latField := arrow.Field{Name: "lat", Type: arrow.PrimitiveTypes.Int64, Nullable: true}
	latChunked := arrow.NewChunked(latField.Type, []arrow.Array{latArr})
	lat := NewSeries(arrow.NewColumn(latField, latChunked))

	geom, err := PointsFromXY(lng, lat, 4326)
	if err != nil {
		t.Fatal(err)
	}
	arr := geom.Column().Data().Chunks()[0].(*array.Binary)
	g, err := geometry.ParseWKB(arr.Value(0))
	if err != nil {
		t.Fatal(err)
	}
	p := g.(geometry.Point)
	if p.X != -74.5 || p.Y != 40 {
		t.Fatalf("got (%v, %v), want (-74.5, 40)", p.X, p.Y)
	}
}

func TestPointsFromXY_NonNumericErrors(t *testing.T) {
	// Pass a String column as x — should error out with ErrNotNumeric.
	pool := memory.DefaultAllocator
	sb := array.NewStringBuilder(pool)
	defer sb.Release()
	sb.AppendValues([]string{"a", "b"}, nil)
	strArr := sb.NewArray()
	defer strArr.Release()
	strField := arrow.Field{Name: "x", Type: arrow.BinaryTypes.String, Nullable: true}
	strChunked := arrow.NewChunked(strField.Type, []arrow.Array{strArr})
	strCol := NewSeries(arrow.NewColumn(strField, strChunked))

	y := makeFloat64Series(t, "y", []float64{1, 2}, nil)
	_, err := PointsFromXY(strCol, y, 4326)
	if !errors.Is(err, ErrNotNumeric) {
		t.Fatalf("want ErrNotNumeric, got %v", err)
	}
}

func TestPointsFromXY_ComposesWithWithColumn(t *testing.T) {
	// End-to-end: build a Frame with lng/lat columns, derive a
	// geometry column via PointsFromXY, attach with WithColumn.
	pool := memory.DefaultAllocator
	nameB := array.NewStringBuilder(pool)
	defer nameB.Release()
	nameB.AppendValues([]string{"NYC", "LA"}, nil)
	lngB := array.NewFloat64Builder(pool)
	defer lngB.Release()
	lngB.AppendValues([]float64{-74.006, -118.2437}, nil)
	latB := array.NewFloat64Builder(pool)
	defer latB.Release()
	latB.AppendValues([]float64{40.7128, 34.0522}, nil)

	fields := []arrow.Field{
		{Name: "name", Type: arrow.BinaryTypes.String, Nullable: false},
		{Name: "lng", Type: arrow.PrimitiveTypes.Float64, Nullable: false},
		{Name: "lat", Type: arrow.PrimitiveTypes.Float64, Nullable: false},
	}
	schema := arrow.NewSchema(fields, nil)
	arrs := []arrow.Array{nameB.NewArray(), lngB.NewArray(), latB.NewArray()}
	defer func() {
		for _, a := range arrs {
			a.Release()
		}
	}()
	cols := make([]arrow.Column, len(fields))
	for i, a := range arrs {
		chunked := arrow.NewChunked(a.DataType(), []arrow.Array{a})
		cols[i] = *arrow.NewColumn(fields[i], chunked)
	}
	df, err := NewFrame(schema, cols)
	if err != nil {
		t.Fatal(err)
	}

	lng, _ := df.Column("lng")
	lat, _ := df.Column("lat")
	geom, err := PointsFromXY(lng, lat, 4326)
	if err != nil {
		t.Fatal(err)
	}
	out, err := df.WithColumn("geometry", geom)
	if err != nil {
		t.Fatal(err)
	}
	if out.NumCols() != 4 {
		t.Fatalf("cols = %d, want 4", out.NumCols())
	}
	geomCol, _ := out.Column("geometry")
	if !geomCol.IsGeometry() {
		t.Fatal("attached column should be geometry-tagged")
	}
	// Frame.Geometry decode still works.
	g, err := out.Geometry("geometry", 0)
	if err != nil {
		t.Fatal(err)
	}
	p := g.(geometry.Point)
	if p.X != -74.006 {
		t.Fatalf("row 0 X = %v, want -74.006", p.X)
	}
}

func TestPointsFromXYZ_ProducesXYZPoint(t *testing.T) {
	x := makeFloat64Series(t, "x", []float64{1, 2}, nil)
	y := makeFloat64Series(t, "y", []float64{3, 4}, nil)
	z := makeFloat64Series(t, "z", []float64{5, 6}, nil)

	geom, err := PointsFromXYZ(x, y, z, 4326)
	if err != nil {
		t.Fatal(err)
	}
	arr := geom.Column().Data().Chunks()[0].(*array.Binary)
	g, err := geometry.ParseWKB(arr.Value(0))
	if err != nil {
		t.Fatal(err)
	}
	p := g.(geometry.Point)
	if !p.HasZ {
		t.Fatal("Point should have HasZ=true")
	}
	if p.X != 1 || p.Y != 3 || p.Z != 5 {
		t.Fatalf("row 0 = (%v, %v, %v), want (1, 3, 5)", p.X, p.Y, p.Z)
	}
}

func TestPointsFromXYZ_NullZProducesNullPoint(t *testing.T) {
	x := makeFloat64Series(t, "x", []float64{1, 2}, nil)
	y := makeFloat64Series(t, "y", []float64{3, 4}, nil)
	z := makeFloat64Series(t, "z", []float64{5, 0}, []bool{true, false})

	geom, err := PointsFromXYZ(x, y, z, 4326)
	if err != nil {
		t.Fatal(err)
	}
	arr := geom.Column().Data().Chunks()[0].(*array.Binary)
	if arr.IsNull(0) {
		t.Errorf("row 0 should be non-null")
	}
	if !arr.IsNull(1) {
		t.Errorf("row 1 should be null (z null)")
	}
}
