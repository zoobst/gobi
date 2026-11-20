// Package h3 adds Uber's H3 hexagonal grid to gobi as expressions:
// points to cells, cell hierarchy, cells back to geometry, and
// neighborhoods. Each function returns a gobi.Expr, usable in Select,
// WithColumn, Filter and GroupBy keys, eager or lazy:
//
//	df.Lazy().
//	    WithColumn("cell", h3.LatLngToCell(gobi.PointExpr{Lat: gobi.Col("lat"), Lon: gobi.Col("lon")}, 8)).
//	    GroupBy("cell").
//	    Agg(gobi.Aggregation{Kind: gobi.AggCount, Alias: "n"})
//
// It is built on github.com/uber/h3-go/v4, which is pure Go from
// v4.6.0 (no cgo, cross-compiles to wasm). Results match the H3 C
// library: see h3-go's conformance contract, and this package's
// golden tests against h3-py.
//
// # Representation
//
// Cells are Int64 columns. Every valid H3 index has its top bit clear,
// so it fits int64 exactly, and int64 is what Athena, Hive, Iceberg
// and Spark can store (they have no unsigned 64-bit type). Functions
// that take a cell also accept Uint64 columns, as polars-h3 and DuckDB
// produce.
//
// # Nulls and invalid input
//
// A null input gives a null output. So does a value H3 rejects — an
// invalid cell index, a non-finite coordinate, a parent resolution
// finer than the cell's — rather than failing the whole batch. Use
// IsValid to find invalid cells. Misuse that can't be per-row data is
// an error: a resolution outside 0..15, a negative k, a column of the
// wrong type, or (GeomToCell) a geometry column in a projected CRS.
//
// # Ordering for pruning
//
// Cells within one H3 base cell share their leading bits, so a Parquet
// file sorted by its cell column keeps nearby cells in the same row
// groups, and gobi's IsIn prunes row groups on a cell list:
//
//	parquetio.ScanFile(path, nil).Filter(gobi.Col("cell").IsIn(cells))
package h3

import (
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
	uh3 "github.com/uber/h3-go/v4"

	"github.com/zoobst/gobi"
	"github.com/zoobst/gobi/geometry"
)

// MaxResolution is H3's finest resolution; resolutions run 0..15.
const MaxResolution = 15

// Errors.
var (
	// ErrResolution: a resolution outside 0..MaxResolution.
	ErrResolution = errors.New("gobi/h3: resolution must be in 0..15")
	// ErrProjectedCRS: GeomToCell on a geometry column whose CRS is
	// projected; H3 needs longitude / latitude. Transform first.
	ErrProjectedCRS = errors.New("gobi/h3: geometry column is in a projected CRS; H3 needs longitude/latitude (EPSG:4326)")
	// ErrNotLonLat: GeomToCell found a coordinate outside
	// longitude/latitude range, which usually means projected data in a
	// column with no CRS recorded.
	ErrNotLonLat = errors.New("gobi/h3: coordinate outside longitude/latitude range")
)

// LatLngToCell returns the Int64 cell containing each point at
// resolution res. The point's components are named fields, as in
// gobi.HaversineExpr, so latitude and longitude can't be swapped; both
// must be Float64 columns, in degrees. The result column is named
// h3_<res>.
func LatLngToCell(p gobi.PointExpr, res int) gobi.Expr {
	n := &node{
		fn: "h3_latlng_to_cell", args: fmt.Sprintf("%d", res),
		inputs: []gobi.ExprNode{p.Lat.Node(), p.Lon.Node()},
		name:   func([]string) string { return fmt.Sprintf("h3_%d", res) },
		err:    checkRes(res),
		want:   []typeCheck{wantFloat64, wantFloat64},
		out:    arrow.PrimitiveTypes.Int64,
	}
	n.eval = func(in []gobi.Series, pool memory.Allocator) (arrow.Array, error) {
		lat, latNull, err := floats(in[0])
		if err != nil {
			return nil, err
		}
		lng, lngNull, err := floats(in[1])
		if err != nil {
			return nil, err
		}
		b := array.NewInt64Builder(pool)
		defer b.Release()
		b.Reserve(len(lat))
		for i := range lat {
			if latNull[i] || lngNull[i] {
				b.AppendNull()
				continue
			}
			c, err := uh3.LatLngToCell(uh3.NewLatLng(lat[i], lng[i]), res)
			appendCell(b, c, err)
		}
		return b.NewArray(), nil
	}
	return gobi.Custom(n)
}

// GeomToCell returns the Int64 cell containing each point of a point
// geometry column at resolution res. The column must be in a
// geographic CRS (EPSG:4326 or unset); a projected CRS is
// ErrProjectedCRS rather than an implicit reprojection. A coordinate
// outside longitude/latitude range is ErrNotLonLat, since it usually
// means unlabeled projected data. A non-point geometry is an error; an
// empty point gives null. The result column is named h3_<res>.
func GeomToCell(geom gobi.Expr, res int) gobi.Expr {
	n := &node{
		fn: "h3_geom_to_cell", args: fmt.Sprintf("%d", res),
		inputs: []gobi.ExprNode{geom.Node()},
		name:   func([]string) string { return fmt.Sprintf("h3_%d", res) },
		err:    checkRes(res),
		want:   []typeCheck{wantBinary},
		out:    arrow.PrimitiveTypes.Int64,
	}
	n.eval = func(in []gobi.Series, pool memory.Allocator) (arrow.Array, error) {
		s := in[0]
		if !s.IsGeometry() {
			return nil, fmt.Errorf("%w: GeomToCell: %q is not a geometry column", gobi.ErrNotGeometry, s.Name())
		}
		if crs := geometryCRS(s); crs.Projected() {
			return nil, fmt.Errorf("%w (column %q is EPSG:%d)", ErrProjectedCRS, s.Name(), crs.EPSG)
		}
		b := array.NewInt64Builder(pool)
		defer b.Release()
		b.Reserve(s.Len())
		row := 0
		for _, chunk := range s.Column().Data().Chunks() {
			a := chunk.(*array.Binary)
			for i := range a.Len() {
				if a.IsNull(i) {
					b.AppendNull()
					row++
					continue
				}
				x, y, _, ok, err := geometry.PointXYZFromWKB(a.Value(i))
				if err != nil {
					return nil, fmt.Errorf("gobi/h3: GeomToCell row %d: %w", row, err)
				}
				if !ok {
					return nil, fmt.Errorf("gobi/h3: GeomToCell row %d: not a point geometry", row)
				}
				if math.IsNaN(x) || math.IsNaN(y) { // empty point
					b.AppendNull()
					row++
					continue
				}
				if math.Abs(y) > 90 || math.Abs(x) > 180 {
					return nil, fmt.Errorf("%w: GeomToCell row %d: (%g, %g)", ErrNotLonLat, row, x, y)
				}
				c, err := uh3.LatLngToCell(uh3.NewLatLng(y, x), res)
				appendCell(b, c, err)
				row++
			}
		}
		return b.NewArray(), nil
	}
	return gobi.Custom(n)
}

// Parent returns each cell's ancestor at resolution res, as Int64. A
// res finer than the cell's own is null. The result column is named
// h3_<res>.
func Parent(cell gobi.Expr, res int) gobi.Expr {
	n := cellNode("h3_parent", fmt.Sprintf("%d", res), cell, arrow.PrimitiveTypes.Int64,
		func(string) string { return fmt.Sprintf("h3_%d", res) })
	n.err = checkRes(res)
	n.eval = mapCells(n, func(b array.Builder, c uh3.Cell) {
		p, err := c.Parent(res)
		appendCell(b.(*array.Int64Builder), p, err)
	})
	return gobi.Custom(n)
}

// Resolution returns each cell's resolution (0..15), as Int32. The
// result column is named <input>_res.
func Resolution(cell gobi.Expr) gobi.Expr {
	n := cellNode("h3_resolution", "", cell, arrow.PrimitiveTypes.Int32, suffix("_res"))
	n.eval = mapCells(n, func(b array.Builder, c uh3.Cell) {
		b.(*array.Int32Builder).Append(int32(c.Resolution()))
	})
	return gobi.Custom(n)
}

// IsValid reports whether each value is a valid H3 cell index, as
// Boolean; null stays null. The result column is named <input>_is_valid.
func IsValid(cell gobi.Expr) gobi.Expr {
	n := cellNode("h3_is_valid", "", cell, arrow.FixedWidthTypes.Boolean, suffix("_is_valid"))
	n.keepInvalid = true
	n.eval = mapCells(n, func(b array.Builder, c uh3.Cell) {
		b.(*array.BooleanBuilder).Append(c.IsValid())
	})
	return gobi.Custom(n)
}

// CellToPoint returns each cell's center as a point geometry column in
// EPSG:4326. The result column is named <input>_point.
func CellToPoint(cell gobi.Expr) gobi.Expr {
	n := cellNode("h3_cell_to_point", "", cell, arrow.BinaryTypes.Binary, suffix("_point"))
	n.geomOut = true
	n.eval = mapCells(n, func(b array.Builder, c uh3.Cell) {
		ll, err := c.LatLng()
		if err != nil {
			b.AppendNull()
			return
		}
		b.(*array.BinaryBuilder).Append(geometry.WKB(geometry.NewPoint(ll.Lng, ll.Lat, geometry.WGS84)))
	})
	return gobi.Custom(n)
}

// CellToBoundary returns each cell's outline as a polygon geometry
// column in EPSG:4326 (closed ring, H3's vertex order). Longitudes stay
// in -180..180, as H3 reports them, so a cell straddling the
// antimeridian comes back as a polygon spanning the globe in planar
// terms; split those with geometry.SplitAtAntimeridian when that
// matters. The result column is named <input>_boundary.
func CellToBoundary(cell gobi.Expr) gobi.Expr {
	n := cellNode("h3_cell_to_boundary", "", cell, arrow.BinaryTypes.Binary, suffix("_boundary"))
	n.geomOut = true
	n.eval = mapCells(n, func(b array.Builder, c uh3.Cell) {
		cb, err := c.Boundary()
		if err != nil {
			b.AppendNull()
			return
		}
		ring := make([]geometry.Point, 0, len(cb)+1)
		for _, v := range cb {
			ring = append(ring, geometry.NewPoint(v.Lng, v.Lat, geometry.WGS84))
		}
		ring = append(ring, ring[0])
		b.(*array.BinaryBuilder).Append(geometry.WKB(geometry.NewPolygon([][]geometry.Point{ring}, geometry.WGS84)))
	})
	return gobi.Custom(n)
}

// GridDisk returns, for each cell, the cells within k grid steps of it
// (itself included), as List<Int64> in H3's order. Pair it with
// Explode for one row per neighbor. k must be >= 0. The result column
// is named <input>_disk<k>.
func GridDisk(cell gobi.Expr, k int) gobi.Expr {
	n := cellNode("h3_grid_disk", fmt.Sprintf("%d", k), cell, arrow.ListOf(arrow.PrimitiveTypes.Int64), suffix(fmt.Sprintf("_disk%d", k)))
	if k < 0 {
		n.err = fmt.Errorf("gobi/h3: GridDisk: k must be >= 0, got %d", k)
	}
	n.eval = mapCells(n, func(b array.Builder, c uh3.Cell) {
		cells, err := c.GridDisk(k)
		if err != nil {
			b.AppendNull()
			return
		}
		lb := b.(*array.ListBuilder)
		lb.Append(true)
		vb := lb.ValueBuilder().(*array.Int64Builder)
		for _, d := range cells {
			vb.Append(int64(d))
		}
	})
	return gobi.Custom(n)
}

// CellToString returns each cell's hexadecimal string form (e.g.
// "8828308281fffff"), as String. The result column is named
// <input>_str.
func CellToString(cell gobi.Expr) gobi.Expr {
	n := cellNode("h3_cell_to_string", "", cell, arrow.BinaryTypes.String, suffix("_str"))
	n.eval = mapCells(n, func(b array.Builder, c uh3.Cell) {
		b.(*array.StringBuilder).Append(uh3.CellToString(c))
	})
	return gobi.Custom(n)
}

// -----------------------------------------------------------------------------
// node: the gobi.ExprNode behind every function
// -----------------------------------------------------------------------------

type typeCheck func(arrow.DataType) bool

func wantFloat64(dt arrow.DataType) bool { return dt.ID() == arrow.FLOAT64 }
func wantBinary(dt arrow.DataType) bool  { return dt.ID() == arrow.BINARY }
func wantCell(dt arrow.DataType) bool {
	return dt.ID() == arrow.INT64 || dt.ID() == arrow.UINT64
}

type node struct {
	fn     string // name in String()
	args   string // trailing String() arguments
	inputs []gobi.ExprNode
	want   []typeCheck
	name   func(inputNames []string) string
	out    arrow.DataType
	err    error // construction-time error (bad resolution, k)
	// geomOut: the output is a geometry column (EPSG:4326).
	geomOut bool
	// keepInvalid: pass invalid cells to the per-cell function instead
	// of emitting null (IsValid).
	keepInvalid bool
	eval        func(in []gobi.Series, pool memory.Allocator) (arrow.Array, error)
}

func (n *node) Eval(input *gobi.Frame) (gobi.Series, error) {
	if n.err != nil {
		return gobi.Series{}, n.err
	}
	in := make([]gobi.Series, len(n.inputs))
	for i, x := range n.inputs {
		s, err := x.Eval(input)
		if err != nil {
			return gobi.Series{}, err
		}
		if !n.want[i](s.DataType()) {
			return gobi.Series{}, n.typeErr(i, s.DataType())
		}
		in[i] = s
	}
	arr, err := n.eval(in, memory.DefaultAllocator)
	if err != nil {
		return gobi.Series{}, err
	}
	return gobi.SeriesFromArray(n.field(), arr), nil
}

func (n *node) Type(schema *arrow.Schema) (arrow.DataType, error) {
	if n.err != nil {
		return nil, n.err
	}
	for i, x := range n.inputs {
		dt, err := x.Type(schema)
		if err != nil {
			return nil, err
		}
		if !n.want[i](dt) {
			return nil, n.typeErr(i, dt)
		}
	}
	return n.out, nil
}

func (n *node) typeErr(i int, dt arrow.DataType) error {
	want := "a cell (Int64 / Uint64)"
	switch {
	case n.want[i](arrow.PrimitiveTypes.Float64):
		want = "Float64 degrees"
	case n.want[i](arrow.BinaryTypes.Binary):
		want = "a geometry"
	}
	return fmt.Errorf("%w: %s: input %d is %s, want %s", gobi.ErrExprTypeMismatch, n.fn, i, dt, want)
}

func (n *node) field() arrow.Field {
	if n.geomOut {
		return gobi.GeometryField(n.OutputName(), int32(geometry.WGS84.EPSG))
	}
	return arrow.Field{Name: n.OutputName(), Type: n.out, Nullable: true}
}

// OutputFieldMetadata declares the geometry tags of CellToPoint /
// CellToBoundary output, so plan schemas show them (gobi.FieldMetadataProvider).
func (n *node) OutputFieldMetadata(*arrow.Schema) (arrow.Metadata, bool) {
	if !n.geomOut {
		return arrow.Metadata{}, false
	}
	return n.field().Metadata, true
}

func (n *node) Children() []gobi.Expr {
	out := make([]gobi.Expr, len(n.inputs))
	for i, x := range n.inputs {
		out[i] = gobi.Custom(x)
	}
	return out
}

func (n *node) String() string {
	parts := make([]string, 0, len(n.inputs)+1)
	for _, x := range n.inputs {
		parts = append(parts, x.String())
	}
	if n.args != "" {
		parts = append(parts, n.args)
	}
	return fmt.Sprintf("%s(%s)", n.fn, strings.Join(parts, ", "))
}

// OutputName names the result column (gobi.Namer).
func (n *node) OutputName() string {
	names := make([]string, len(n.inputs))
	for i, x := range n.inputs {
		if nm, ok := x.(gobi.Namer); ok {
			names[i] = nm.OutputName()
		}
	}
	return n.name(names)
}

// cellNode is a node over one cell input.
func cellNode(fn, args string, cell gobi.Expr, out arrow.DataType, name func(string) string) *node {
	return &node{
		fn: fn, args: args,
		inputs: []gobi.ExprNode{cell.Node()},
		want:   []typeCheck{wantCell},
		out:    out,
		name:   func(in []string) string { return name(in[0]) },
	}
}

// suffix names a result <input><sfx>, or h3<sfx> for an unnamed input.
func suffix(sfx string) func(string) string {
	return func(in string) string {
		if in == "" {
			in = "h3"
		}
		return in + sfx
	}
}

// mapCells runs f over each non-null cell of the node's input, with a
// null for a null input or (unless keepInvalid) an invalid cell.
func mapCells(n *node, f func(b array.Builder, c uh3.Cell)) func([]gobi.Series, memory.Allocator) (arrow.Array, error) {
	return func(in []gobi.Series, pool memory.Allocator) (arrow.Array, error) {
		b := array.NewBuilder(pool, n.out)
		defer b.Release()
		b.Reserve(in[0].Len())
		for _, chunk := range in[0].Column().Data().Chunks() {
			for i := range chunk.Len() {
				if chunk.IsNull(i) {
					b.AppendNull()
					continue
				}
				var c uh3.Cell
				switch a := chunk.(type) {
				case *array.Int64:
					c = uh3.Cell(a.Value(i))
				case *array.Uint64:
					c = uh3.Cell(a.Value(i))
				}
				if !n.keepInvalid && !c.IsValid() {
					b.AppendNull()
					continue
				}
				f(b, c)
			}
		}
		return b.NewArray(), nil
	}
}

// appendCell appends c, or null when H3 returned an error.
func appendCell(b *array.Int64Builder, c uh3.Cell, err error) {
	if err != nil {
		b.AppendNull()
		return
	}
	b.Append(int64(c))
}

func checkRes(res int) error {
	if res < 0 || res > MaxResolution {
		return fmt.Errorf("%w, got %d", ErrResolution, res)
	}
	return nil
}

// floats copies a Float64 column's values and null mask out of its
// chunks, so two inputs line up row for row whatever their chunking.
func floats(s gobi.Series) ([]float64, []bool, error) {
	vals, err := s.Float64s()
	if err != nil {
		return nil, nil, err
	}
	return vals, s.Nulls(), nil
}

// geometryCRS reads a geometry column's recorded CRS (EPSG 0 if unset).
func geometryCRS(s gobi.Series) geometry.CRS {
	f := s.Column().Field()
	v, _ := f.Metadata.GetValue(gobi.MetaGeometryCRS)
	var epsg int32
	if _, err := fmt.Sscan(v, &epsg); err != nil {
		return geometry.CRS{}
	}
	return geometry.CRS{EPSG: epsg}
}
