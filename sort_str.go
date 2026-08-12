package gobi

import (
	"fmt"
	"math"
	"sort"

	"github.com/apache/arrow-go/v18/arrow/array"

	"github.com/zoobst/gobi/geometry"
)

// STRDefaultLeafSize is the target row-group size when
// SortBySTR is called with leafSize <= 0. Matches typical
// GeoParquet row-group defaults (5000).
const STRDefaultLeafSize = 5000

// SortBySTR returns a new Frame with rows reordered using the
// Sort-Tile-Recursive (STR) leaf-level ordering. Alternative to
// SortByHilbert with different locality tradeoffs.
//
// STR partitions the N centroids into ⌈√(N/leafSize)⌉ vertical
// strips sorted by x, then sorts each strip's centroids by y —
// producing groups of leafSize consecutive rows that share both a
// tight X range (they're in the same strip) AND a tight Y range
// (they're consecutive-in-y within the strip). Sub-groups of
// leafSize consecutive rows come out spatially rectangular.
//
// leafSize is the target row-group size (typically the same value
// you'll pass to WriteOptions.RowGroupRows). A value <= 0 falls
// back to STRDefaultLeafSize.
//
// **When to prefer STR over Hilbert:**
//
//   - Axis-aligned AOI queries (rectangular bboxes with sides
//     parallel to X/Y): STR row groups are rectangles, so an
//     axis-aligned AOI cleanly overlaps at most a strip's worth
//     of them. Hilbert curves cross tile boundaries at odd
//     angles, so a rectangular AOI can straddle more row groups.
//   - Static datasets partitioned into predictable strips
//     (latitude bands, admin regions, time-series windows).
//
// **When to stick with Hilbert:**
//
//   - Point queries or diagonal-aligned AOIs — Hilbert's curve
//     preserves 2D locality symmetrically; STR privileges the X
//     axis over Y (arbitrary but structural).
//   - Multi-file / cross-partition sorts — Hilbert indices in a
//     shared reference frame merge cleanly, whereas STR needs a
//     custom merge policy.
//
// Sort is stable within a leaf. Null geometries sort last.
func (f *Frame) SortBySTR(geomCol string, leafSize int) (*Frame, error) {
	s, err := f.Column(geomCol)
	if err != nil {
		return nil, err
	}
	if !s.IsGeometry() {
		return nil, fmt.Errorf("%w: %s is not a geometry column", ErrNotGeometry, geomCol)
	}

	n := f.NumRows()
	if n == 0 {
		f.Retain()
		return f, nil
	}
	if leafSize <= 0 {
		leafSize = STRDefaultLeafSize
	}

	// Extract centroids + null mask.
	type row struct {
		idx  int
		x, y float64
		null bool
	}
	rows := make([]row, n)
	idx := 0
	for _, chunk := range s.col.Data().Chunks() {
		bin, ok := chunk.(*array.Binary)
		if !ok {
			return nil, fmt.Errorf("%w: geometry column not Binary (%T)",
				ErrColumnTypeMismatch, chunk)
		}
		for i := range bin.Len() {
			rows[idx].idx = idx
			if bin.IsNull(i) {
				rows[idx].null = true
				idx++
				continue
			}
			g, err := geometry.ParseWKB(bin.Value(i))
			if err != nil {
				return nil, fmt.Errorf("row %d: %w", idx, err)
			}
			c := g.Centroid()
			rows[idx].x = c.X
			rows[idx].y = c.Y
			idx++
		}
	}

	// Count non-null rows for strip-count computation.
	nonNull := 0
	for _, r := range rows {
		if !r.null {
			nonNull++
		}
	}

	// Number of vertical strips: ⌈√(N/leafSize)⌉ per the STR
	// algorithm. Ceiling ensures every strip has at most leafSize
	// worth of rows once we further sort by y.
	numStrips := 1
	if nonNull > leafSize {
		numStrips = int(math.Ceil(math.Sqrt(float64(nonNull) / float64(leafSize))))
	}
	if numStrips < 1 {
		numStrips = 1
	}
	rowsPerStrip := (nonNull + numStrips - 1) / numStrips

	// Pass 1: stable-sort by (null, x) so null rows sit at the end
	// and non-nulls form left-to-right strips.
	sort.SliceStable(rows, func(a, b int) bool {
		if rows[a].null != rows[b].null {
			return !rows[a].null
		}
		if rows[a].null {
			return false // stable within nulls
		}
		return rows[a].x < rows[b].x
	})

	// Pass 2: within each strip of rowsPerStrip non-nulls, stable-
	// sort by y.
	for start := 0; start < nonNull; start += rowsPerStrip {
		end := min(start+rowsPerStrip, nonNull)
		strip := rows[start:end]
		sort.SliceStable(strip, func(a, b int) bool {
			return strip[a].y < strip[b].y
		})
	}

	perm := make([]int, n)
	for i, r := range rows {
		perm[i] = r.idx
	}
	return f.take(perm)
}
