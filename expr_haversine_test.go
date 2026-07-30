package gobi

import (
	"errors"
	"math"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/zoobst/gobi/geometry"
)

// haversineFrame builds a Frame with four Float64 columns (lat1,
// lon1, lat2, lon2) from paired slices. The fixture the new
// PointExpr-shaped HaversineExpr consumes.
func haversineFrame(t testing.TB, lat1, lon1, lat2, lon2 []float64) *Frame {
	t.Helper()
	pool := memory.DefaultAllocator
	cols := []struct {
		name string
		vals []float64
	}{
		{"lat1", lat1}, {"lon1", lon1}, {"lat2", lat2}, {"lon2", lon2},
	}
	fields := make([]arrow.Field, len(cols))
	arrs := make([]arrow.Array, len(cols))
	for i, c := range cols {
		b := array.NewFloat64Builder(pool)
		b.AppendValues(c.vals, nil)
		arrs[i] = b.NewArray()
		b.Release()
		fields[i] = arrow.Field{Name: c.name, Type: arrow.PrimitiveTypes.Float64, Nullable: false}
	}
	defer func() {
		for _, a := range arrs {
			a.Release()
		}
	}()
	arrowCols := make([]arrow.Column, len(fields))
	for i, a := range arrs {
		arrowCols[i] = *arrow.NewColumn(fields[i],
			arrow.NewChunked(a.DataType(), []arrow.Array{a}))
	}
	f, err := NewFrame(arrow.NewSchema(fields, nil), arrowCols)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// TestHaversineExpr_Basic — NYC → London ~5570 km. Verifies the
// expression matches the scalar geometry.Haversine bit-for-bit
// (same math kernel, hoisted constant).
func TestHaversineExpr_Basic(t *testing.T) {
	f := haversineFrame(t,
		[]float64{40.7484}, []float64{-73.9857}, // NYC
		[]float64{51.5074}, []float64{-0.1276},  // London
	)
	out, err := f.WithColumnExpr("dist_km", HaversineExpr(
		PointExpr{Lat: Col("lat1"), Lon: Col("lon1")},
		PointExpr{Lat: Col("lat2"), Lon: Col("lon2")},
		geometry.UnitKilometers,
	))
	if err != nil {
		t.Fatal(err)
	}
	col, _ := out.Column("dist_km")
	if col.DataType().ID() != arrow.FLOAT64 {
		t.Fatalf("dtype = %s, want FLOAT64", col.DataType())
	}
	got := col.col.Data().Chunks()[0].(*array.Float64).Value(0)
	want, err := geometry.Haversine(
		geometry.Point{X: -73.9857, Y: 40.7484},
		geometry.Point{X: -0.1276, Y: 51.5074},
		geometry.UnitKilometers,
	)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("dist = %v, want %v (scalar geometry.Haversine)", got, want)
	}
	if got < 5000 || got > 6000 {
		t.Errorf("dist %v km out of expected NYC→London range", got)
	}
}

// TestHaversineExpr_VectorizedMultiRow — several rows in one Eval,
// zero nulls, hits the tight zero-copy fast path.
func TestHaversineExpr_VectorizedMultiRow(t *testing.T) {
	f := haversineFrame(t,
		[]float64{90, 0, 40.7484},
		[]float64{0, 0, -73.9857},
		[]float64{0, 0, 34.0522},
		[]float64{0, 1, -118.2437},
	)
	out, err := f.WithColumnExpr("d", HaversineExpr(
		PointExpr{Lat: Col("lat1"), Lon: Col("lon1")},
		PointExpr{Lat: Col("lat2"), Lon: Col("lon2")},
		geometry.UnitKilometers,
	))
	if err != nil {
		t.Fatal(err)
	}
	arr := out.mustCol("d").col.Data().Chunks()[0].(*array.Float64)
	if arr.Value(0) < 9900 || arr.Value(0) > 10100 {
		t.Errorf("pole→equator = %v km, want ~10007", arr.Value(0))
	}
	if arr.Value(1) < 110 || arr.Value(1) > 112 {
		t.Errorf("one-deg-east = %v km, want ~111", arr.Value(1))
	}
	if arr.Value(2) < 3900 || arr.Value(2) > 4000 {
		t.Errorf("NYC→LA = %v km, want ~3936", arr.Value(2))
	}
}

// TestHaversineExpr_NullPropagation — a null in any of the four
// underlying operand columns produces a null output row.
func TestHaversineExpr_NullPropagation(t *testing.T) {
	pool := memory.DefaultAllocator
	lat1B := array.NewFloat64Builder(pool)
	defer lat1B.Release()
	lat1B.AppendValues([]float64{40.7484, 40.7484}, []bool{true, false})
	lon1B := array.NewFloat64Builder(pool)
	defer lon1B.Release()
	lon1B.AppendValues([]float64{-73.9857, -73.9857}, nil)
	lat2B := array.NewFloat64Builder(pool)
	defer lat2B.Release()
	lat2B.AppendValues([]float64{51.5074, 51.5074}, nil)
	lon2B := array.NewFloat64Builder(pool)
	defer lon2B.Release()
	lon2B.AppendValues([]float64{-0.1276, -0.1276}, nil)

	arrs := []arrow.Array{lat1B.NewArray(), lon1B.NewArray(), lat2B.NewArray(), lon2B.NewArray()}
	defer func() {
		for _, a := range arrs {
			a.Release()
		}
	}()
	fields := []arrow.Field{
		{Name: "lat1", Type: arrow.PrimitiveTypes.Float64, Nullable: true},
		{Name: "lon1", Type: arrow.PrimitiveTypes.Float64, Nullable: true},
		{Name: "lat2", Type: arrow.PrimitiveTypes.Float64, Nullable: true},
		{Name: "lon2", Type: arrow.PrimitiveTypes.Float64, Nullable: true},
	}
	cols := make([]arrow.Column, len(fields))
	for i, a := range arrs {
		cols[i] = *arrow.NewColumn(fields[i], arrow.NewChunked(a.DataType(), []arrow.Array{a}))
	}
	f, err := NewFrame(arrow.NewSchema(fields, nil), cols)
	if err != nil {
		t.Fatal(err)
	}
	out, err := f.WithColumnExpr("d", HaversineExpr(
		PointExpr{Lat: Col("lat1"), Lon: Col("lon1")},
		PointExpr{Lat: Col("lat2"), Lon: Col("lon2")},
		geometry.UnitKilometers,
	))
	if err != nil {
		t.Fatal(err)
	}
	arr := out.mustCol("d").col.Data().Chunks()[0].(*array.Float64)
	if arr.IsNull(0) {
		t.Errorf("row 0 should be non-null (all inputs valid)")
	}
	if !arr.IsNull(1) {
		t.Errorf("row 1 should be null (lat1 is null)")
	}
}

// TestHaversineExpr_RejectsNonFloat — non-Float64 operand errors
// with ExprTypeMismatch at Type-check.
func TestHaversineExpr_RejectsNonFloat(t *testing.T) {
	pool := memory.DefaultAllocator
	makeF64 := func(v float64) arrow.Array {
		b := array.NewFloat64Builder(pool)
		defer b.Release()
		b.AppendValues([]float64{v}, nil)
		return b.NewArray()
	}
	iB := array.NewInt64Builder(pool)
	defer iB.Release()
	iB.AppendValues([]int64{1}, nil)
	arrs := []arrow.Array{iB.NewArray(), makeF64(1), makeF64(2), makeF64(3)}
	defer func() {
		for _, a := range arrs {
			a.Release()
		}
	}()
	fields := []arrow.Field{
		{Name: "lat1", Type: arrow.PrimitiveTypes.Int64, Nullable: false},
		{Name: "lon1", Type: arrow.PrimitiveTypes.Float64, Nullable: false},
		{Name: "lat2", Type: arrow.PrimitiveTypes.Float64, Nullable: false},
		{Name: "lon2", Type: arrow.PrimitiveTypes.Float64, Nullable: false},
	}
	cols := make([]arrow.Column, len(fields))
	for i, a := range arrs {
		cols[i] = *arrow.NewColumn(fields[i], arrow.NewChunked(a.DataType(), []arrow.Array{a}))
	}
	f, err := NewFrame(arrow.NewSchema(fields, nil), cols)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WithColumnExpr("d", HaversineExpr(
		PointExpr{Lat: Col("lat1"), Lon: Col("lon1")},
		PointExpr{Lat: Col("lat2"), Lon: Col("lon2")},
		geometry.UnitKilometers,
	))
	if err == nil {
		t.Fatal("expected error for Int64 lat1")
	}
	if !errors.Is(err, ErrExprTypeMismatch) {
		t.Errorf("error should wrap ErrExprTypeMismatch, got %v", err)
	}
}
