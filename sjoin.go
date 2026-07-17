package gobi

import (
	"fmt"
	"sync"

	"github.com/apache/arrow/go/v18/arrow"
	"github.com/apache/arrow/go/v18/arrow/array"
	"github.com/apache/arrow/go/v18/arrow/memory"

	"github.com/zoobst/gobi/geometry"
)

// SJoinMinParallelRows is the smallest left-frame size at which SJoin will
// spawn goroutines. Below this, the sequential path is used to avoid
// scheduling overhead swamping the useful work.
const SJoinMinParallelRows = 1024

// SpatialPredicate names a binary spatial predicate for SJoin.
type SpatialPredicate uint8

const (
	// SPIntersects matches when the left and right geometries share any point.
	SPIntersects SpatialPredicate = iota
	// SPContains matches when the left geometry fully contains the right.
	SPContains
	// SPWithin matches when the left geometry lies fully within the right.
	SPWithin
)

func (p SpatialPredicate) String() string {
	switch p {
	case SPIntersects:
		return "intersects"
	case SPContains:
		return "contains"
	case SPWithin:
		return "within"
	default:
		return "unknown"
	}
}

func (p SpatialPredicate) toGeometry() geometry.Predicate {
	switch p {
	case SPContains:
		return geometry.PredContains
	case SPWithin:
		return geometry.PredWithin
	default:
		return geometry.PredIntersects
	}
}

// SJoin performs a spatial join of f (left) and right by evaluating pred on
// each pair of geometries from leftGeomCol and rightGeomCol. Only rows where
// pred holds are emitted. The output frame contains all columns from the
// left frame, followed by all columns from the right frame except its
// geometry column (analogous to Frame.Join dropping the right join key).
// Right-side column names that collide with left-side names get a "_right"
// suffix.
//
// Under the hood, an R-tree is built over the right frame's geometry bounds
// so each left row scans only overlapping candidates. Parallelism follows
// the priority order documented on resolveWorkers: Workers(n) > package
// SetMaxParallelism > GOMAXPROCS.
func (f *Frame) SJoin(right *Frame, leftGeomCol, rightGeomCol string, pred SpatialPredicate, opts ...Option) (*Frame, error) {
	lGeom, err := f.Column(leftGeomCol)
	if err != nil {
		return nil, err
	}
	if !lGeom.IsGeometry() {
		return nil, fmt.Errorf("%w: left column %q is not a geometry column",
			ErrNotGeometry, leftGeomCol)
	}
	rGeom, err := right.Column(rightGeomCol)
	if err != nil {
		return nil, err
	}
	if !rGeom.IsGeometry() {
		return nil, fmt.Errorf("%w: right column %q is not a geometry column",
			ErrNotGeometry, rightGeomCol)
	}

	// Decode every geometry column once. Right geometries feed the R-tree;
	// left geometries are cached so we never re-decode them across rows.
	rightGeoms, err := decodeGeometryColumn(rGeom)
	if err != nil {
		return nil, err
	}
	rightBounds := make([]geometry.Bounds, len(rightGeoms))
	for i, g := range rightGeoms {
		if g == nil {
			continue
		}
		rightBounds[i] = g.Bounds()
	}
	tree := geometry.NewRTree(rightBounds)

	leftGeoms, err := decodeGeometryColumn(lGeom)
	if err != nil {
		return nil, err
	}

	geomPred := pred.toGeometry()

	workers := resolveWorkers(opts...)
	leftIdxs, rightIdxs := sjoinScan(leftGeoms, rightGeoms, tree, geomPred, workers)

	return assembleJoinedFrame(f, right, leftIdxs, rightIdxs, rightGeomCol)
}

// sjoinScan runs the per-left-row candidate loop. Small inputs or workers==1
// use the sequential path; larger inputs shard across the requested worker
// count and merge results in left-row order.
func sjoinScan(
	leftGeoms, rightGeoms []geometry.Geometry,
	tree *geometry.RTree,
	geomPred geometry.Predicate,
	workers int,
) (leftIdxs, rightIdxs []int) {
	n := len(leftGeoms)
	if workers <= 1 || n < SJoinMinParallelRows {
		return sjoinScanRange(leftGeoms, rightGeoms, tree, geomPred, 0, n, nil)
	}

	if workers > n {
		workers = n
	}
	chunk := (n + workers - 1) / workers

	type shard struct {
		l, r []int
	}
	shards := make([]shard, workers)

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		start := w * chunk
		end := min(start+chunk, n)
		if start >= end {
			continue
		}
		wg.Add(1)
		go func(idx, s, e int) {
			defer wg.Done()
			// Each worker owns its scratch buffer for R-tree candidates.
			var scratch []int32
			l, r := sjoinScanRange(leftGeoms, rightGeoms, tree, geomPred, s, e, scratch)
			shards[idx] = shard{l: l, r: r}
		}(w, start, end)
	}
	wg.Wait()

	var total int
	for _, sh := range shards {
		total += len(sh.l)
	}
	leftIdxs = make([]int, 0, total)
	rightIdxs = make([]int, 0, total)
	for _, sh := range shards {
		leftIdxs = append(leftIdxs, sh.l...)
		rightIdxs = append(rightIdxs, sh.r...)
	}
	return leftIdxs, rightIdxs
}

// sjoinScanRange evaluates the predicate for left rows in [start, end) and
// returns the matched (left, right) index pairs. scratch is a reusable
// R-tree candidate buffer; pass nil to allocate one.
func sjoinScanRange(
	leftGeoms, rightGeoms []geometry.Geometry,
	tree *geometry.RTree,
	geomPred geometry.Predicate,
	start, end int,
	scratch []int32,
) (leftIdxs, rightIdxs []int) {
	// Modest capacity hint scaled to this worker's slice.
	nHint := end - start
	leftIdxs = make([]int, 0, nHint)
	rightIdxs = make([]int, 0, nHint)

	for lRow := start; lRow < end; lRow++ {
		lg := leftGeoms[lRow]
		if lg == nil {
			continue
		}
		scratch = tree.SearchInto(scratch, lg.Bounds())
		for _, rIdx := range scratch {
			rg := rightGeoms[rIdx]
			if rg == nil {
				continue
			}
			if !geometry.Test(geomPred, lg, rg) {
				continue
			}
			leftIdxs = append(leftIdxs, lRow)
			rightIdxs = append(rightIdxs, int(rIdx))
		}
	}
	return leftIdxs, rightIdxs
}

// decodeGeometryColumn walks every chunk of a geometry Series and returns
// the decoded geometries (nil where the WKB was null).
func decodeGeometryColumn(s Series) ([]geometry.Geometry, error) {
	out := make([]geometry.Geometry, 0, s.Len())
	for _, chunk := range s.col.Data().Chunks() {
		bin, ok := chunk.(*array.Binary)
		if !ok {
			return nil, fmt.Errorf("%w: geometry column not Binary (%T)",
				ErrColumnTypeMismatch, chunk)
		}
		for i := range bin.Len() {
			if bin.IsNull(i) {
				out = append(out, nil)
				continue
			}
			g, err := geometry.ParseWKB(bin.Value(i))
			if err != nil {
				return nil, err
			}
			out = append(out, g)
		}
	}
	return out, nil
}

// assembleJoinedFrame builds the output frame by taking the requested rows
// from each side. The right frame's geometry column is dropped, and any
// remaining right column whose name collides with a left column gets a
// "_right" suffix.
func assembleJoinedFrame(f, right *Frame, leftIdxs, rightIdxs []int, rightGeomCol string) (*Frame, error) {
	pool := memory.DefaultAllocator

	leftNames := f.ColumnNames()
	leftNameSet := map[string]struct{}{}
	for _, n := range leftNames {
		leftNameSet[n] = struct{}{}
	}

	outFields := make([]arrow.Field, 0, len(f.series)+len(right.series))
	outColumns := make([]arrow.Column, 0, cap(outFields))

	// Left columns via take.
	for _, s := range f.series {
		arr, err := takeArray(pool, s, leftIdxs)
		if err != nil {
			return nil, err
		}
		defer arr.Release()
		chunked := arrow.NewChunked(arr.DataType(), []arrow.Array{arr})
		outFields = append(outFields, s.field)
		outColumns = append(outColumns, *arrow.NewColumn(s.field, chunked))
	}

	// Right columns via take (skip the join geometry column).
	for _, s := range right.series {
		if s.name == rightGeomCol {
			continue
		}
		arr, err := takeArrayWithNulls(pool, s, rightIdxs)
		if err != nil {
			return nil, err
		}
		defer arr.Release()
		field := s.field
		if _, clash := leftNameSet[s.name]; clash {
			field.Name = s.name + "_right"
		}
		chunked := arrow.NewChunked(arr.DataType(), []arrow.Array{arr})
		outFields = append(outFields, field)
		outColumns = append(outColumns, *arrow.NewColumn(field, chunked))
	}

	schema := arrow.NewSchema(outFields, nil)
	return NewFrame(schema, outColumns)
}
