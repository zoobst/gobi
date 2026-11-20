package h3_test

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/zoobst/gobi"
	"github.com/zoobst/gobi/geometry"
	"github.com/zoobst/gobi/h3"

	uh3 "github.com/uber/h3-go/v4"
)

func uh3LatLngToCell(lat, lng float64, res int) (int64, error) {
	c, err := uh3.LatLngToCell(uh3.NewLatLng(lat, lng), res)
	return int64(c), err
}

// golden is testdata/golden.json, written by gen_golden.py from h3-py
// (the H3 C library), so it checks h3-go's Go port independently.
type golden struct {
	H3Py   string `json:"h3_py"`
	Points []struct {
		Lat   float64 `json:"lat"`
		Lng   float64 `json:"lng"`
		Cells []int64 `json:"cells"` // index = resolution
	} `json:"points"`
	Cells []struct {
		Cell     int64        `json:"cell"`
		Res      int32        `json:"res"`
		Parents  []int64      `json:"parents"` // index = parent resolution
		Lat      float64      `json:"lat"`
		Lng      float64      `json:"lng"`
		Boundary [][2]float64 `json:"boundary"` // (lat, lng)
		Disk1    []int64      `json:"disk1"`    // sorted
		Disk2    []int64      `json:"disk2"`    // sorted; nil when not generated
		Str      string       `json:"str"`
	} `json:"cells"`
	Invalid []int64 `json:"invalid"`
}

func loadGolden(t *testing.T) golden {
	t.Helper()
	raw, err := os.ReadFile("testdata/golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var g golden
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	return g
}

// frame builds a Frame from series, releasing each caller reference
// and the Frame at test cleanup.
func frame(t *testing.T, series ...gobi.Series) *gobi.Frame {
	t.Helper()
	f, err := gobi.NewFrameFromSeries(series...)
	for _, s := range series {
		if s.Column() != nil {
			s.Column().Release()
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.Release)
	return f
}

// eval runs e over f and returns its single result column.
func eval(t *testing.T, f *gobi.Frame, e gobi.Expr) gobi.Series {
	t.Helper()
	out, err := f.Lazy().Select(e).Collect()
	if err != nil {
		t.Fatalf("%s: %v", e, err)
	}
	t.Cleanup(out.Release)
	s, err := out.Column(out.ColumnNames()[0])
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func pointsFrame(t *testing.T, g golden) *gobi.Frame {
	t.Helper()
	lat := make([]float64, len(g.Points))
	lng := make([]float64, len(g.Points))
	gb := array.NewBinaryBuilder(memory.DefaultAllocator, arrow.BinaryTypes.Binary)
	defer gb.Release()
	for i, p := range g.Points {
		lat[i], lng[i] = p.Lat, p.Lng
		gb.Append(geometry.WKB(geometry.NewPoint(p.Lng, p.Lat, geometry.WGS84)))
	}
	return frame(t,
		gobi.NewFloat64Series("lat", lat, nil),
		gobi.NewFloat64Series("lon", lng, nil),
		gobi.SeriesFromArray(gobi.GeometryField("geometry", 4326), gb.NewArray()),
	)
}

func TestLatLngToCell_Golden(t *testing.T) {
	g := loadGolden(t)
	f := pointsFrame(t, g)
	pt := gobi.PointExpr{Lat: gobi.Col("lat"), Lon: gobi.Col("lon")}
	for res := 0; res <= h3.MaxResolution; res++ {
		for name, e := range map[string]gobi.Expr{
			"LatLngToCell": h3.LatLngToCell(pt, res),
			"GeomToCell":   h3.GeomToCell(gobi.Col("geometry"), res),
		} {
			s := eval(t, f, e)
			got, err := s.Int64s()
			if err != nil {
				t.Fatal(err)
			}
			bad := 0
			for i, p := range g.Points {
				if got[i] != p.Cells[res] {
					if bad < 3 {
						t.Errorf("%s res %d: (%v, %v) = %x, h3-py %x", name, res, p.Lat, p.Lng, got[i], p.Cells[res])
					}
					bad++
				}
			}
			if bad > 0 {
				t.Errorf("%s res %d: %d of %d points differ", name, res, bad, len(g.Points))
			}
		}
	}
	t.Logf("%d points × 16 resolutions match h3-py %s", len(g.Points), g.H3Py)
}

func TestCellFunctions_Golden(t *testing.T) {
	g := loadGolden(t)
	ids := make([]int64, len(g.Cells))
	for i, c := range g.Cells {
		ids[i] = c.Cell
	}
	f := frame(t, gobi.NewInt64Series("cell", ids, nil))
	cell := gobi.Col("cell")

	res, _ := eval(t, f, h3.Resolution(cell)).Int32s()
	valid, _ := eval(t, f, h3.IsValid(cell)).Bools()
	strs, _ := eval(t, f, h3.CellToString(cell)).Strings()
	points := eval(t, f, h3.CellToPoint(cell))
	bounds := eval(t, f, h3.CellToBoundary(cell))
	disk1 := listsOf(t, eval(t, f, h3.GridDisk(cell, 1)))
	disk2 := listsOf(t, eval(t, f, h3.GridDisk(cell, 2)))
	for i, c := range g.Cells {
		// Degrees. Within 0.1° of a pole, h3-py 4.5 (H3 C 4.5.0) takes
		// latitude from asin, which cancels there; h3-go 4.6 uses atan2
		// (uber/h3-go#167, upstream in C 4.6) and is the more accurate
		// side, by up to 1.8e-7° at resolution 15.
		tol := 1e-9
		if math.Abs(c.Lat) > 89.9 {
			tol = 1e-6
		}
		if res[i] != c.Res || !valid[i] || strs[i] != c.Str {
			t.Errorf("cell %x: res %d valid %v str %q, want %d true %q", c.Cell, res[i], valid[i], strs[i], c.Res, c.Str)
		}
		pg, err := points.Geometry(i)
		if err != nil {
			t.Fatal(err)
		}
		p := pg.(geometry.Point)
		if arcDeg(p.Y, p.X, c.Lat, c.Lng) > tol {
			t.Errorf("cell %x: center (%v, %v), want (%v, %v)", c.Cell, p.Y, p.X, c.Lat, c.Lng)
		}
		bg, err := bounds.Geometry(i)
		if err != nil {
			t.Fatal(err)
		}
		ring := bg.(geometry.Polygon).Rings[0]
		if len(ring) != len(c.Boundary)+1 || ring[0] != ring[len(ring)-1] {
			t.Errorf("cell %x: ring of %d points for %d vertices", c.Cell, len(ring), len(c.Boundary))
			continue
		}
		for j, v := range c.Boundary {
			if arcDeg(ring[j].Y, ring[j].X, v[0], v[1]) > tol {
				t.Errorf("cell %x vertex %d: (%v, %v), want (%v, %v)", c.Cell, j, ring[j].Y, ring[j].X, v[0], v[1])
			}
		}
		if !slices.Equal(sorted(disk1[i]), c.Disk1) {
			t.Errorf("cell %x: disk1 %v, want %v", c.Cell, sorted(disk1[i]), c.Disk1)
		}
		if c.Disk2 != nil && !slices.Equal(sorted(disk2[i]), c.Disk2) {
			t.Errorf("cell %x: disk2 differs", c.Cell)
		}
		for pr, want := range c.Parents {
			s := eval(t, f, h3.Parent(cell, pr))
			got, _ := s.Int64s()
			if got[i] != want {
				t.Errorf("cell %x: parent res %d = %x, want %x", c.Cell, pr, got[i], want)
			}
		}
	}
}

// arcDeg is the great-circle distance between two points, in degrees
// of arc — the right measure near a pole, where longitude stops
// meaning anything.
func arcDeg(lat1, lng1, lat2, lng2 float64) float64 {
	const r = math.Pi / 180
	dLat, dLng := (lat2-lat1)*r, (lng2-lng1)*r
	h := math.Sin(dLat/2)*math.Sin(dLat/2) + math.Cos(lat1*r)*math.Cos(lat2*r)*math.Sin(dLng/2)*math.Sin(dLng/2)
	return 2 * math.Asin(math.Sqrt(min(h, 1))) / r
}

func listsOf(t *testing.T, s gobi.Series) [][]int64 {
	t.Helper()
	a := s.Column().Data().Chunk(0).(*array.List)
	vals := a.ListValues().(*array.Int64)
	out := make([][]int64, a.Len())
	for i := range out {
		lo, hi := a.ValueOffsets(i)
		out[i] = slices.Clone(vals.Int64Values()[lo:hi])
	}
	return out
}

func sorted(xs []int64) []int64 { xs = slices.Clone(xs); slices.Sort(xs); return xs }

func TestNullsAndInvalid(t *testing.T) {
	g := loadGolden(t)
	good := g.Cells[0].Cell
	vals := append([]int64{good, 0}, g.Invalid...)
	valid := make([]bool, len(vals))
	for i := range valid {
		valid[i] = i != 1
	}
	f := frame(t, gobi.NewInt64Series("cell", vals, valid))
	cell := gobi.Col("cell")

	isValid := eval(t, f, h3.IsValid(cell))
	v, _ := isValid.Bools()
	if !v[0] || !isValid.Nulls()[1] {
		t.Errorf("IsValid: %v nulls %v", v, isValid.Nulls())
	}
	for i := 2; i < len(vals); i++ {
		if v[i] {
			t.Errorf("IsValid(%x) = true", vals[i])
		}
	}
	// Everything else: null for the null row and for invalid cells.
	for _, e := range []gobi.Expr{h3.Resolution(cell), h3.Parent(cell, 0), h3.CellToString(cell),
		h3.CellToPoint(cell), h3.CellToBoundary(cell), h3.GridDisk(cell, 1)} {
		nulls := eval(t, f, e).Nulls()
		if nulls[0] || slices.Contains(nulls[1:], false) {
			t.Errorf("%s: nulls %v", e, nulls)
		}
	}
	// A parent finer than the cell is null.
	if !eval(t, f, h3.Parent(cell, 15)).Nulls()[0] && g.Cells[0].Res < 15 {
		t.Error("finer parent not null")
	}

	// Uint64 cells work like Int64.
	ub := array.NewUint64Builder(memory.DefaultAllocator)
	ub.Append(uint64(good))
	uf := frame(t, gobi.SeriesFromArray(arrow.Field{Name: "u", Type: arrow.PrimitiveTypes.Uint64, Nullable: true}, ub.NewArray()))
	ub.Release()
	if r, _ := eval(t, uf, h3.Resolution(gobi.Col("u"))).Int32s(); r[0] != g.Cells[0].Res {
		t.Errorf("uint64 resolution = %d", r[0])
	}

	// Null / non-finite coordinates give null cells.
	pf := frame(t,
		gobi.NewFloat64Series("lat", []float64{10, math.NaN(), 10}, []bool{true, true, false}),
		gobi.NewFloat64Series("lon", []float64{20, 20, 20}, nil),
	)
	nulls := eval(t, pf, h3.LatLngToCell(gobi.PointExpr{Lat: gobi.Col("lat"), Lon: gobi.Col("lon")}, 8)).Nulls()
	if !slices.Equal(nulls, []bool{false, true, true}) {
		t.Errorf("LatLngToCell nulls %v", nulls)
	}
}

func TestErrors(t *testing.T) {
	ptf := func(epsg int32, x, y float64) *gobi.Frame {
		gb := array.NewBinaryBuilder(memory.DefaultAllocator, arrow.BinaryTypes.Binary)
		defer gb.Release()
		gb.Append(geometry.WKB(geometry.NewPoint(x, y, geometry.CRS{})))
		return frame(t, gobi.SeriesFromArray(gobi.GeometryField("geometry", epsg), gb.NewArray()))
	}
	lineFrame := func() *gobi.Frame {
		gb := array.NewBinaryBuilder(memory.DefaultAllocator, arrow.BinaryTypes.Binary)
		defer gb.Release()
		gb.Append(geometry.WKB(geometry.NewLineString([]geometry.Point{{X: 0, Y: 0}, {X: 1, Y: 1}}, geometry.WGS84)))
		return frame(t, gobi.SeriesFromArray(gobi.GeometryField("geometry", 4326), gb.NewArray()))
	}
	nums := frame(t, gobi.NewFloat64Series("lat", []float64{1}, nil), gobi.NewStringSeries("s", []string{"x"}, nil),
		gobi.NewInt64Series("cell", []int64{0}, nil))
	pt := gobi.PointExpr{Lat: gobi.Col("lat"), Lon: gobi.Col("lat")}

	for name, tc := range map[string]struct {
		f    *gobi.Frame
		e    gobi.Expr
		want error
	}{
		"res 16":              {nums, h3.LatLngToCell(pt, 16), h3.ErrResolution},
		"res -1":              {nums, h3.Parent(gobi.Col("cell"), -1), h3.ErrResolution},
		"negative k":          {nums, h3.GridDisk(gobi.Col("cell"), -1), nil},
		"string cell":         {nums, h3.Resolution(gobi.Col("s")), gobi.ErrExprTypeMismatch},
		"string lat":          {nums, h3.LatLngToCell(gobi.PointExpr{Lat: gobi.Col("s"), Lon: gobi.Col("lat")}, 8), gobi.ErrExprTypeMismatch},
		"projected CRS":       {ptf(3857, 500000, 6000000), h3.GeomToCell(gobi.Col("geometry"), 8), h3.ErrNotWGS84},
		"unlabeled projected": {ptf(0, 500000, 6000000), h3.GeomToCell(gobi.Col("geometry"), 8), h3.ErrNotLonLat},
		"non-point":           {lineFrame(), h3.GeomToCell(gobi.Col("geometry"), 8), nil},
		"not geometry":        {nums, h3.GeomToCell(gobi.Col("s"), 8), gobi.ErrExprTypeMismatch},
	} {
		_, err := tc.f.Lazy().Select(tc.e).Collect()
		if err == nil || (tc.want != nil && !errors.Is(err, tc.want)) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
	}
}

func TestNamesAndLazyGroupBy(t *testing.T) {
	g := loadGolden(t)
	f := pointsFrame(t, g)
	pt := gobi.PointExpr{Lat: gobi.Col("lat"), Lon: gobi.Col("lon")}
	out, err := f.Lazy().
		WithColumn("cell", h3.LatLngToCell(pt, 3)).
		Select(gobi.Col("cell"), h3.Parent(gobi.Col("cell"), 1), h3.Resolution(gobi.Col("cell")), h3.CellToBoundary(gobi.Col("cell"))).
		Collect()
	if err != nil {
		t.Fatal(err)
	}
	defer out.Release()
	if want := []string{"cell", "h3_1", "cell_res", "cell_boundary"}; !slices.Equal(out.ColumnNames(), want) {
		t.Errorf("names %v, want %v", out.ColumnNames(), want)
	}
	if b, _ := out.Column("cell_boundary"); !b.IsGeometry() {
		t.Error("boundary column not tagged as geometry")
	}
	// Declared schema and a zero-row result carry the tags too.
	lf := f.Lazy().
		WithColumn("cell", h3.LatLngToCell(pt, 3)).
		Select(h3.CellToPoint(gobi.Col("cell")), h3.CellToBoundary(gobi.Col("cell")))
	empty, err := lf.Filter(gobi.Col("cell_point").IsNull()).Collect()
	if err != nil {
		t.Fatal(err)
	}
	defer empty.Release()
	for name, sch := range map[string]*arrow.Schema{"Schema()": lf.Schema(), "zero-row": empty.Schema()} {
		for i := range 2 {
			if v, _ := sch.Field(i).Metadata.GetValue(gobi.MetaGeometryCRS); v != "4326" {
				t.Errorf("%s: field %s not tagged EPSG:4326: %v", name, sch.Field(i).Name, sch.Field(i).Metadata)
			}
		}
	}

	counts, err := f.Lazy().
		WithColumn("cell", h3.LatLngToCell(pt, 0)).
		GroupBy("cell").
		Agg(gobi.Aggregation{Kind: gobi.AggCount, Alias: "n"}).
		Collect()
	if err != nil {
		t.Fatal(err)
	}
	defer counts.Release()
	n, _ := counts.Column("n")
	total := int64(0)
	ns, _ := n.Int64s()
	for _, x := range ns {
		total += x
	}
	if total != int64(len(g.Points)) || counts.NumRows() < 2 {
		t.Errorf("groupby: %d groups, %d rows counted", counts.NumRows(), total)
	}
}

// TestReviewRegressions — a huge GridDisk radius, an unset PointExpr
// component, and an infinite coordinate.
func TestReviewRegressions(t *testing.T) {
	g := loadGolden(t)
	cells := frame(t, gobi.NewInt64Series("cell", []int64{g.Cells[0].Cell}, nil))
	for _, k := range []int{h3.MaxGridDiskK + 1, math.MaxInt32} {
		if _, err := cells.Lazy().Select(h3.GridDisk(gobi.Col("cell"), k)).Collect(); err == nil {
			t.Errorf("GridDisk(k=%d): no error", k)
		}
	}

	pts := frame(t, gobi.NewFloat64Series("lat", []float64{1}, nil))
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("unset Lon: panicked: %v", r)
			}
		}()
		lf := pts.Lazy().Select(h3.LatLngToCell(gobi.PointExpr{Lat: gobi.Lit(1.0)}, 8))
		if _, err := lf.Collect(); err == nil {
			t.Error("unset Lon: no error")
		}
	}()

	gb := array.NewBinaryBuilder(memory.DefaultAllocator, arrow.BinaryTypes.Binary)
	gb.Append(geometry.WKB(geometry.NewPoint(1, math.Inf(1), geometry.WGS84)))
	gb.Append(geometry.WKB(geometry.NewPoint(math.Inf(-1), 1, geometry.WGS84)))
	inf := frame(t, gobi.SeriesFromArray(gobi.GeometryField("geometry", 4326), gb.NewArray()))
	gb.Release()
	s := eval(t, inf, h3.GeomToCell(gobi.Col("geometry"), 8))
	if !slices.Equal(s.Nulls(), []bool{true, true}) {
		t.Errorf("infinite coordinates: nulls %v, want all null", s.Nulls())
	}
}

// scalarNode is a custom expression that returns one value for every
// row, as ExprNode's contract allows.
type scalarNode struct{ v float64 }

func (n scalarNode) Eval(*gobi.Frame) (gobi.Series, error) {
	return gobi.NewFloat64Series("s", []float64{n.v}, nil), nil
}
func (n scalarNode) Type(*arrow.Schema) (arrow.DataType, error) {
	return arrow.PrimitiveTypes.Float64, nil
}
func (n scalarNode) Children() []gobi.Expr { return nil }
func (n scalarNode) String() string        { return "scalar" }

// TestReviewRegressions2 — unregistered CRS codes, scalar operands.
func TestReviewRegressions2(t *testing.T) {
	gb := array.NewBinaryBuilder(memory.DefaultAllocator, arrow.BinaryTypes.Binary)
	gb.Append(geometry.WKB(geometry.NewPoint(100, 50, geometry.CRS{})))
	mercator := frame(t, gobi.SeriesFromArray(gobi.GeometryField("geometry", 3395), gb.NewArray()))
	gb.Release()
	_, err := mercator.Lazy().Select(h3.GeomToCell(gobi.Col("geometry"), 8)).Collect()
	if !errors.Is(err, h3.ErrNotWGS84) || !errors.Is(err, geometry.ErrUnknownCRS) {
		t.Errorf("EPSG:3395: err = %v, want ErrNotWGS84 wrapping geometry.ErrUnknownCRS", err)
	}

	// A registered geographic CRS that isn't WGS 84 degrees (EPSG:4807,
	// grads from Paris) is still refused.
	geometry.RegisterCRS(4807, "NTF (Paris)", false)
	gb = array.NewBinaryBuilder(memory.DefaultAllocator, arrow.BinaryTypes.Binary)
	gb.Append(geometry.WKB(geometry.NewPoint(2, 50, geometry.CRS{})))
	paris := frame(t, gobi.SeriesFromArray(gobi.GeometryField("geometry", 4807), gb.NewArray()))
	gb.Release()
	if _, err := paris.Lazy().Select(h3.GeomToCell(gobi.Col("geometry"), 8)).Collect(); !errors.Is(err, h3.ErrNotWGS84) {
		t.Errorf("EPSG:4807: err = %v, want ErrNotWGS84", err)
	}

	pts := frame(t, gobi.NewFloat64Series("lat", []float64{10, 20}, nil))
	got, err := pts.Lazy().Select(h3.LatLngToCell(gobi.PointExpr{Lat: gobi.Col("lat"), Lon: gobi.Custom(scalarNode{30})}, 5)).Collect()
	if err != nil {
		t.Fatalf("scalar lon: %v", err)
	}
	defer got.Release()
	cells, _ := got.Column(got.ColumnNames()[0])
	vals, _ := cells.Int64s()
	want := make([]int64, 2)
	for i, lat := range []float64{10, 20} {
		c, _ := uh3LatLngToCell(lat, 30, 5)
		want[i] = c
	}
	if !slices.Equal(vals, want) {
		t.Errorf("scalar lon: %x, want %x", vals, want)
	}
}

// TestScalarOperandsBroadcastToFrameRows — one-value operands fill
// every row of the frame, whatever the other operands' lengths.
func TestScalarOperandsBroadcastToFrameRows(t *testing.T) {
	f := frame(t, gobi.NewFloat64Series("lat", []float64{10, 20, 30}, nil))
	want, _ := uh3LatLngToCell(10, 30, 5)

	both := h3.LatLngToCell(gobi.PointExpr{Lat: gobi.Custom(scalarNode{10}), Lon: gobi.Custom(scalarNode{30})}, 5)
	out, err := f.Lazy().Select(both).Collect()
	if err != nil {
		t.Fatal(err)
	}
	defer out.Release()
	s, _ := out.Column(out.ColumnNames()[0])
	if v, _ := s.Int64s(); !slices.Equal(v, []int64{want, want, want}) {
		t.Errorf("two scalars, Select: %x", v)
	}
	wc, err := f.Lazy().WithColumn("cell", both).Collect()
	if err != nil {
		t.Fatalf("two scalars, WithColumn: %v", err)
	}
	defer wc.Release()
	if wc.NumRows() != 3 {
		t.Errorf("WithColumn rows = %d", wc.NumRows())
	}

	// One-value cell and geometry inputs broadcast too.
	cell := gobi.Custom(cellScalar{want})
	res, err := f.Lazy().Select(h3.Resolution(cell)).Collect()
	if err != nil {
		t.Fatal(err)
	}
	defer res.Release()
	if r, _ := res.Column(res.ColumnNames()[0]); r.Len() != 3 {
		t.Errorf("one-value cell: %d rows", r.Len())
	}

	// A length that is neither the frame's nor 1 is an error.
	short := gobi.Custom(lenNode{2})
	if _, err := f.Lazy().Select(h3.LatLngToCell(gobi.PointExpr{Lat: short, Lon: gobi.Col("lat")}, 5)).Collect(); !errors.Is(err, gobi.ErrColumnLenMismatch) {
		t.Errorf("2-row lat on 3-row frame: err = %v", err)
	}
}

type cellScalar struct{ c int64 }

func (n cellScalar) Eval(*gobi.Frame) (gobi.Series, error) {
	return gobi.NewInt64Series("c", []int64{n.c}, nil), nil
}
func (n cellScalar) Type(*arrow.Schema) (arrow.DataType, error) {
	return arrow.PrimitiveTypes.Int64, nil
}
func (n cellScalar) Children() []gobi.Expr { return nil }
func (n cellScalar) String() string        { return "cell" }

type lenNode struct{ n int }

func (n lenNode) Eval(*gobi.Frame) (gobi.Series, error) {
	return gobi.NewFloat64Series("l", make([]float64, n.n), nil), nil
}
func (n lenNode) Type(*arrow.Schema) (arrow.DataType, error) {
	return arrow.PrimitiveTypes.Float64, nil
}
func (n lenNode) Children() []gobi.Expr { return nil }
func (n lenNode) String() string        { return "len" }

// TestGeomToCell_UnreadableCRS — a gobi:crs_epsg value that isn't a
// plain decimal code is refused, never read as "unset".
func TestGeomToCell_UnreadableCRS(t *testing.T) {
	for _, v := range []string{"EPSG:3857", "3857abc", " 4326", "4326.0", "abc"} {
		gb := array.NewBinaryBuilder(memory.DefaultAllocator, arrow.BinaryTypes.Binary)
		gb.Append(geometry.WKB(geometry.NewPoint(100, 50, geometry.CRS{})))
		md := arrow.NewMetadata([]string{gobi.MetaGeometryType, gobi.MetaGeometryCRS}, []string{"WKB", v})
		f := frame(t, gobi.SeriesFromArray(arrow.Field{Name: "geometry", Type: arrow.BinaryTypes.Binary, Nullable: true, Metadata: md}, gb.NewArray()))
		gb.Release()
		_, err := f.Lazy().Select(h3.GeomToCell(gobi.Col("geometry"), 8)).Collect()
		if !errors.Is(err, h3.ErrNotWGS84) || !strings.Contains(err.Error(), "unreadable") {
			t.Errorf("crs_epsg %q: err = %v, want ErrNotWGS84 (unreadable)", v, err)
		}
	}
	// Missing, empty and "0" are unset: accepted, with the range check.
	for _, v := range []string{"", "0"} {
		gb := array.NewBinaryBuilder(memory.DefaultAllocator, arrow.BinaryTypes.Binary)
		gb.Append(geometry.WKB(geometry.NewPoint(10, 50, geometry.CRS{})))
		md := arrow.NewMetadata([]string{gobi.MetaGeometryType, gobi.MetaGeometryCRS}, []string{"WKB", v})
		f := frame(t, gobi.SeriesFromArray(arrow.Field{Name: "geometry", Type: arrow.BinaryTypes.Binary, Nullable: true, Metadata: md}, gb.NewArray()))
		gb.Release()
		if _, err := f.Lazy().Select(h3.GeomToCell(gobi.Col("geometry"), 8)).Collect(); err != nil {
			t.Errorf("crs_epsg %q: %v", v, err)
		}
	}
}
