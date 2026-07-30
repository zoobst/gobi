package gobi

import (
	"fmt"
	"math"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/zoobst/gobi/geometry"
)

// PointExpr bundles a Lat + Lon Expr pair for use with
// HaversineExpr. Kept as a named struct so the two coordinate
// components can't be swapped at the call site — a bug class that
// plagued the older four-argument positional signature (lat/lon
// vs lon/lat is a classic geospatial footgun).
//
// PointExpr is a value type, not an ExprNode — it doesn't compose
// with .Add / .Sub / etc. Use it only where an API explicitly
// asks for a PointExpr; individual coordinate columns still use
// plain Expr elsewhere.
type PointExpr struct {
	Lat, Lon Expr
}

// HaversineExpr returns an expression that computes the great-circle
// distance between two lat/lon point-column pairs, in the requested
// unit. Composes with Shift(1).Over(K) for prev-row coordinate
// lookups, so per-segment ground-track distance is expressible
// entirely in LazyFrame land:
//
//	speedKMH := HaversineExpr(
//	    PointExpr{Lat: Col("lat"), Lon: Col("lon")},
//	    PointExpr{
//	        Lat: Col("lat").Shift(1).Over("eid"),
//	        Lon: Col("lon").Shift(1).Over("eid"),
//	    },
//	    geometry.UnitKilometers,
//	).Div(Col("delta_hours"))
//
// Coordinate components are named-field to eliminate the "did I
// pass lat first or lon first?" bug class. All four underlying Expr
// operands must be Float64 columns; types are checked at Compile.
// Nulls in any of the four inputs propagate to a null output row.
//
// Perf shape: zero-copy `Float64Values()` fast path when the four
// operand columns are single-chunk (the common shape post-scan or
// post-materialize); multi-chunk fallback via `Float64s()`. Inner
// loop uses hoisted scale constant + inline trig — no per-row
// function-call boundary into the scalar `geometry.Haversine`.
//
// Breaking change in v0.2.16: previously took four positional Expr
// args (lat1, lon1, lat2, lon2). Migration:
//
//	// Before
//	HaversineExpr(Col("lat"), Col("lon"), Col("lat2"), Col("lon2"), u)
//	// After
//	HaversineExpr(
//	    PointExpr{Lat: Col("lat"),  Lon: Col("lon")},
//	    PointExpr{Lat: Col("lat2"), Lon: Col("lon2")},
//	    u,
//	)
func HaversineExpr(from, to PointExpr, u geometry.Unit) Expr {
	return Expr{node: &haversineNode{
		lat1: from.Lat.node, lon1: from.Lon.node,
		lat2: to.Lat.node, lon2: to.Lon.node,
		unit: u,
	}}
}

type haversineNode struct {
	lat1, lon1, lat2, lon2 ExprNode
	unit                   geometry.Unit
}

func (n *haversineNode) Eval(input *Frame) (Series, error) {
	lat1, err := n.evalFloat64(input, n.lat1, "lat1")
	if err != nil {
		return Series{}, err
	}
	lon1, err := n.evalFloat64(input, n.lon1, "lon1")
	if err != nil {
		return Series{}, err
	}
	lat2, err := n.evalFloat64(input, n.lat2, "lat2")
	if err != nil {
		return Series{}, err
	}
	lon2, err := n.evalFloat64(input, n.lon2, "lon2")
	if err != nil {
		return Series{}, err
	}
	nRows := input.NumRows()
	if lat1.Len() != nRows || lon1.Len() != nRows ||
		lat2.Len() != nRows || lon2.Len() != nRows {
		return Series{}, fmt.Errorf(
			"%w: HaversineExpr operand length mismatch (lat1=%d lon1=%d lat2=%d lon2=%d, expected %d)",
			ErrColumnLenMismatch, lat1.Len(), lon1.Len(), lat2.Len(), lon2.Len(), nRows)
	}

	// Hoist the unit-scale conversion out of the loop.
	perM, err := geometry.MetersPerUnit(n.unit)
	if err != nil {
		return Series{}, err
	}
	scale := geometry.EarthRadiusKM * 1000 / perM

	// Prefer zero-copy views; fall back to Nulls masks for null
	// gating. All-valid inputs skip null checks entirely.
	lat1Vals, lat1Ok := lat1.Float64Values()
	lon1Vals, lon1Ok := lon1.Float64Values()
	lat2Vals, lat2Ok := lat2.Float64Values()
	lon2Vals, lon2Ok := lon2.Float64Values()
	if !(lat1Ok && lon1Ok && lat2Ok && lon2Ok) {
		return n.evalMultiChunk(lat1, lon1, lat2, lon2, scale)
	}

	pool := memory.DefaultAllocator
	b := array.NewFloat64Builder(pool)
	defer b.Release()

	anyNull := lat1.HasNulls() || lon1.HasNulls() || lat2.HasNulls() || lon2.HasNulls()
	if !anyNull {
		for i := range nRows {
			b.Append(haversineKernel(lon1Vals[i], lat1Vals[i], lon2Vals[i], lat2Vals[i], scale))
		}
		return arrayToSeries(pool, "haversine", arrow.PrimitiveTypes.Float64, b.NewArray())
	}
	lat1N := lat1.Nulls()
	lon1N := lon1.Nulls()
	lat2N := lat2.Nulls()
	lon2N := lon2.Nulls()
	for i := range nRows {
		if isNullAtMask(lat1N, i) || isNullAtMask(lon1N, i) ||
			isNullAtMask(lat2N, i) || isNullAtMask(lon2N, i) {
			b.AppendNull()
			continue
		}
		b.Append(haversineKernel(lon1Vals[i], lat1Vals[i], lon2Vals[i], lat2Vals[i], scale))
	}
	return arrayToSeries(pool, "haversine", arrow.PrimitiveTypes.Float64, b.NewArray())
}

// evalMultiChunk is the fallback for the rare case where an operand
// Series isn't Float64 single-chunk (e.g., a downstream op left the
// column with multiple chunks). Uses the copy extractor and the
// vectorized Nulls mask — same output as the fast path, one extra
// allocation per operand.
func (n *haversineNode) evalMultiChunk(lat1, lon1, lat2, lon2 Series, scale float64) (Series, error) {
	lat1V, err := lat1.Float64s()
	if err != nil {
		return Series{}, fmt.Errorf("HaversineExpr lat1: %w", err)
	}
	lon1V, err := lon1.Float64s()
	if err != nil {
		return Series{}, fmt.Errorf("HaversineExpr lon1: %w", err)
	}
	lat2V, err := lat2.Float64s()
	if err != nil {
		return Series{}, fmt.Errorf("HaversineExpr lat2: %w", err)
	}
	lon2V, err := lon2.Float64s()
	if err != nil {
		return Series{}, fmt.Errorf("HaversineExpr lon2: %w", err)
	}
	lat1N := lat1.Nulls()
	lon1N := lon1.Nulls()
	lat2N := lat2.Nulls()
	lon2N := lon2.Nulls()
	pool := memory.DefaultAllocator
	b := array.NewFloat64Builder(pool)
	defer b.Release()
	for i := range lat1V {
		if isNullAtMask(lat1N, i) || isNullAtMask(lon1N, i) ||
			isNullAtMask(lat2N, i) || isNullAtMask(lon2N, i) {
			b.AppendNull()
			continue
		}
		b.Append(haversineKernel(lon1V[i], lat1V[i], lon2V[i], lat2V[i], scale))
	}
	return arrayToSeries(pool, "haversine", arrow.PrimitiveTypes.Float64, b.NewArray())
}

// haversineKernel is the raw math body — same trig loop
// geometry.HaversineBatch runs, factored so both loops emit identical
// bytecode. Caller pre-computes `scale = EarthRadiusKM * 1000 / metersPerUnit(u)`
// once outside the loop.
func haversineKernel(fromLon, fromLat, toLon, toLat, scale float64) float64 {
	const deg2rad = math.Pi / 180
	phi1 := fromLat * deg2rad
	phi2 := toLat * deg2rad
	dphi := (toLat - fromLat) * deg2rad
	dlam := (toLon - fromLon) * deg2rad
	sinHP := math.Sin(dphi / 2)
	sinHL := math.Sin(dlam / 2)
	a := sinHP*sinHP + math.Cos(phi1)*math.Cos(phi2)*sinHL*sinHL
	c := 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
	return scale * c
}

func (n *haversineNode) evalFloat64(input *Frame, node ExprNode, label string) (Series, error) {
	if node == nil {
		return Series{}, fmt.Errorf("gobi: HaversineExpr %s is nil", label)
	}
	s, err := node.Eval(input)
	if err != nil {
		return Series{}, fmt.Errorf("HaversineExpr %s: %w", label, err)
	}
	if s.DataType().ID() != arrow.FLOAT64 {
		return Series{}, fmt.Errorf(
			"%w: HaversineExpr %s must be Float64, got %s",
			ErrExprTypeMismatch, label, s.DataType())
	}
	return s, nil
}

func (n *haversineNode) Type(schema *arrow.Schema) (arrow.DataType, error) {
	for _, sub := range []struct {
		node  ExprNode
		label string
	}{
		{n.lat1, "lat1"}, {n.lon1, "lon1"},
		{n.lat2, "lat2"}, {n.lon2, "lon2"},
	} {
		if sub.node == nil {
			return nil, fmt.Errorf("gobi: HaversineExpr %s is nil", sub.label)
		}
		dt, err := sub.node.Type(schema)
		if err != nil {
			return nil, err
		}
		if dt.ID() != arrow.FLOAT64 {
			return nil, fmt.Errorf(
				"%w: HaversineExpr %s must be Float64, got %s",
				ErrExprTypeMismatch, sub.label, dt)
		}
	}
	return arrow.PrimitiveTypes.Float64, nil
}

func (n *haversineNode) Children() []Expr {
	return []Expr{
		{node: n.lat1}, {node: n.lon1},
		{node: n.lat2}, {node: n.lon2},
	}
}

func (n *haversineNode) String() string {
	return fmt.Sprintf("haversine(lat1=%s lon1=%s lat2=%s lon2=%s, %s)",
		n.lat1, n.lon1, n.lat2, n.lon2, n.unit)
}

// isNullAtMask is the local Nulls-mask gate for the Haversine
// operands — a nil mask (Series with no null bitmap) treats every
// index as valid; a shorter mask likewise treats missing indices
// as valid (defensive against off-by-one).
func isNullAtMask(mask []bool, i int) bool {
	return i < len(mask) && mask[i]
}
