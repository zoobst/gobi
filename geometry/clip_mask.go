package geometry

import (
	"fmt"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
)

// ClipMask is a Polygon or MultiPolygon prepared for clipping many
// geometries against it. Clip(subject, mask) builds one for a single
// call; build your own with NewClipMask to clip a whole column or
// collection, so the work that depends only on the mask is done once:
//
//   - for point subjects, the mask's member bounds and ring views
//     (see Prepare), so a point is tested only against the members
//     whose bounds it is in.
//   - for line subjects, a table of the mask's edges, and once enough
//     lines have been clipped to pay for it, an R-tree over them. A
//     segment is then compared with the edges near it, not with every
//     edge of every member it reaches.
//
// A ClipMask is safe for concurrent use.
type ClipMask struct {
	mask  Geometry
	polys []Polygon
	// convex is set when the mask is one convex polygon without
	// holes. Lines are then clipped by LineString.Clip (Cyrus-Beck).
	convex bool

	prepOnce sync.Once
	prep     PreparedGeometry

	edgesOnce sync.Once
	edges     *maskEdges
}

// NewClipMask prepares mask, which must be a Polygon or MultiPolygon.
func NewClipMask(mask Geometry) (*ClipMask, error) {
	if mask == nil {
		return nil, fmt.Errorf("geometry: boolean op: nil geometry")
	}
	if err := requirePolygonal(mask); err != nil {
		return nil, err
	}
	m := &ClipMask{mask: mask, polys: appendPolygons(nil, mask)}
	m.convex = len(m.polys) == 1 && m.polys[0].IsConvex()
	return m, nil
}

// Clip returns the part of subject that lies inside the mask. It is
// Clip(subject, mask) for the mask this ClipMask was built from; see
// Clip for the subjects it takes and what each gives.
func (m *ClipMask) Clip(subject Geometry) (Geometry, error) {
	switch t := subject.(type) {
	case Point:
		crs, err := m.subjectCRS(subject)
		if err != nil {
			return nil, err
		}
		if m.containsPoint(t) {
			t.CRSValue = crs
			return t, nil
		}
		return MultiPoint{CRSValue: crs, HasZ: t.HasZ}, nil
	case MultiPoint:
		crs, err := m.subjectCRS(subject)
		if err != nil {
			return nil, err
		}
		out := MultiPoint{CRSValue: crs, HasZ: t.HasZ}
		for _, p := range t.Points {
			if m.containsPoint(p) {
				out.Points = append(out.Points, p)
			}
		}
		return out, nil
	case LineString:
		crs, err := m.subjectCRS(subject)
		if err != nil {
			return nil, err
		}
		pieces := m.clipLine(t, nil)
		for i := range pieces {
			pieces[i].CRSValue = crs
		}
		switch len(pieces) {
		case 0:
			return LineString{CRSValue: crs, HasZ: t.HasZ}, nil
		case 1:
			return pieces[0], nil
		}
		return MultiLineString{Lines: pieces, CRSValue: crs, HasZ: t.HasZ}, nil
	case MultiLineString:
		crs, err := m.subjectCRS(subject)
		if err != nil {
			return nil, err
		}
		out := MultiLineString{CRSValue: crs, HasZ: t.HasZ}
		for _, l := range t.Lines {
			out.Lines = m.clipLine(l, out.Lines)
		}
		for i := range out.Lines {
			out.Lines[i].CRSValue = crs
		}
		return out, nil
	case Polygon, MultiPolygon:
		return Boolean(subject, m.mask, OpIntersection, ClipOptions{})
	}
	if subject == nil {
		return nil, fmt.Errorf("geometry: boolean op: nil geometry")
	}
	return nil, fmt.Errorf("geometry: clip: unsupported subject %T", subject)
}

// subjectCRS checks that a point or line subject can be clipped by
// the mask (same projected CRS, or one or both unset) and returns the
// CRS for the result.
func (m *ClipMask) subjectCRS(subject Geometry) (CRS, error) {
	if err := validateClipCRS(subject, m.mask); err != nil {
		return CRS{}, err
	}
	return resolveClipCRS(subject, m.mask), nil
}

// containsPoint reports whether p is inside some member of the mask
// or on its boundary.
func (m *ClipMask) containsPoint(p Point) bool {
	m.prepOnce.Do(func() { m.prep = Prepare(m.mask) })
	return TestPointPrepared(PredIntersects, p.X, p.Y, m.prep)
}

// clipLine appends the pieces of ls inside the mask to dst, in order
// along ls.
//
// A mask that is one convex polygon is LineString.Clip. Any other is
// treated as one region, not member by member: clipping to each
// member in turn and joining the results would cut the line wherever
// it passes from one member into an adjoining one, return a stretch
// along a shared edge once per member, and group the pieces by member
// instead of along the line. So each segment of ls is split where it
// meets any ring of any member, and each stretch between two splits
// is kept if its midpoint is inside some member or on its boundary.
func (m *ClipMask) clipLine(ls LineString, dst []LineString) []LineString {
	if len(ls.Points) < 2 || len(m.polys) == 0 {
		return dst
	}
	if m.convex {
		return append(dst, ls.Clip(m.polys[0])...)
	}
	m.edgesOnce.Do(func() { m.edges = newMaskEdges(m.polys) })
	e := m.edges
	if !ls.Bounds().Intersects(e.all) {
		return dst
	}
	var (
		cur     lineBuilder
		ts      = make([]float64, 0, 8)
		scratch maskScratch
	)
	for i := range len(ls.Points) - 1 {
		a, b := ls.Points[i], ls.Points[i+1]
		if a.X == b.X && a.Y == b.Y {
			continue
		}
		ts = append(ts[:0], 0, 1)
		seg := Bounds{MinX: min(a.X, b.X), MinY: min(a.Y, b.Y), MaxX: max(a.X, b.X), MaxY: max(a.Y, b.Y)}
		for _, id := range e.near(seg, &scratch) {
			c, d := Point{X: e.ax[id], Y: e.ay[id]}, Point{X: e.bx[id], Y: e.by[id]}
			if t, ok := segSegParam(a, b, c, d); ok && t > 0 && t < 1 {
				ts = append(ts, t)
			}
		}
		sort.Float64s(ts)
		// The segment's own ends are used as they are; only a point
		// part-way along is interpolated.
		at := func(t float64) Point {
			switch t {
			case 0:
				return a
			case 1:
				return b
			}
			return lerp(a, b, t)
		}
		// end is where the current inside run on this segment stops.
		// It is only written out when the run ends, so a split the
		// line passes straight through (from one member into the
		// next) adds no vertex.
		end, open := 0.0, false
		for k := 0; k+1 < len(ts); k++ {
			t0, t1 := ts[k], ts[k+1]
			if t1-t0 < 1e-15 {
				continue
			}
			mid := lerp(a, b, (t0+t1)/2)
			if !e.contains(mid.X, mid.Y, &scratch) {
				if open {
					cur.append(at(end))
					open = false
				}
				cur.flush(&dst, ls)
				continue
			}
			if !open && cur.empty() {
				cur.append(at(t0))
			}
			end, open = t1, true
		}
		if open {
			cur.append(at(end))
		}
	}
	cur.flush(&dst, ls)
	return dst
}

// maskEdges is every edge of every ring of a mask, in flat arrays,
// for the two questions line clipping asks: which edges come near
// this segment, and is this point in the mask.
type maskEdges struct {
	ax, ay, bx, by []float64
	bounds         []Bounds // one per edge
	ring           []int32  // the ring each edge belongs to
	// Rings are numbered polygon by polygon, each polygon's shell
	// before its holes.
	ringPoly []int32
	ringHole []bool
	all      Bounds // every edge

	// tree indexes bounds. It is built once the edges scanned without
	// it add up to more than building it would have cost (see near),
	// so a single short line against a small mask never pays for it
	// and a long line, or many lines, soon stop scanning.
	tree     atomic.Pointer[RTree]
	treeOnce sync.Once
	scanned  atomic.Int64
}

// maskScratch holds the buffers one goroutine reuses across queries.
type maskScratch struct {
	ids   []int32
	rings []int32
}

const (
	// maskIndexMinEdges is the smallest edge table worth indexing: a
	// scan of fewer edges is as fast as a tree descent.
	maskIndexMinEdges = 64
	// maskIndexPayoff is how many full scans of the edge table are
	// allowed before the tree is built instead.
	maskIndexPayoff = 32
)

func newMaskEdges(polys []Polygon) *maskEdges {
	e := &maskEdges{all: EmptyBounds()}
	for pi, p := range polys {
		for ri, ring := range p.Rings {
			// pointInRing, which contains mirrors, ignores a ring of
			// fewer than three points.
			if len(ring) < 3 {
				continue
			}
			r := openRing(ring)
			id := int32(len(e.ringPoly))
			e.ringPoly = append(e.ringPoly, int32(pi))
			e.ringHole = append(e.ringHole, ri > 0)
			for k := range r {
				a, b := r[k], r[(k+1)%len(r)]
				bb := Bounds{MinX: min(a.X, b.X), MinY: min(a.Y, b.Y), MaxX: max(a.X, b.X), MaxY: max(a.Y, b.Y)}
				e.ax, e.ay = append(e.ax, a.X), append(e.ay, a.Y)
				e.bx, e.by = append(e.bx, b.X), append(e.by, b.Y)
				e.bounds = append(e.bounds, bb)
				e.ring = append(e.ring, id)
				e.all = e.all.Union(bb)
			}
		}
	}
	return e
}

// near returns the edges whose bounds meet q, in s.ids. The tree and
// the scan apply the same test, so which one answers never changes
// the result, only its order.
func (e *maskEdges) near(q Bounds, s *maskScratch) []int32 {
	tree := e.tree.Load()
	if tree == nil && len(e.bounds) >= maskIndexMinEdges &&
		e.scanned.Add(int64(len(e.bounds))) > maskIndexPayoff*int64(len(e.bounds)) {
		e.treeOnce.Do(func() { e.tree.Store(NewRTree(e.bounds)) })
		tree = e.tree.Load()
	}
	if tree != nil {
		s.ids = tree.SearchInto(s.ids, q)
		return s.ids
	}
	s.ids = s.ids[:0]
	for id, b := range e.bounds {
		if !(q.MinX > b.MaxX || b.MinX > q.MaxX || q.MinY > b.MaxY || b.MinY > q.MaxY) {
			s.ids = append(s.ids, int32(id))
		}
	}
	return s.ids
}

// contains reports whether (x, y) is inside some polygon of the mask
// or on its boundary. It gives the answer pointInPolygon gives for
// each member (inside the shell and outside every hole, by the same
// crossing rule as pointInRing, or on any ring edge), but looks only
// at the edges a ray from the point towards +X can reach.
func (e *maskEdges) contains(x, y float64, s *maskScratch) bool {
	if x < e.all.MinX || x > e.all.MaxX || y < e.all.MinY || y > e.all.MaxY {
		return false
	}
	s.rings = s.rings[:0]
	// Every edge the point lies on, and every edge the ray crosses,
	// has bounds that meet the ray's.
	for _, id := range e.near(Bounds{MinX: x, MinY: y, MaxX: e.all.MaxX, MaxY: y}, s) {
		ax, ay, bx, by := e.ax[id], e.ay[id], e.bx[id], e.by[id]
		if pointOnSegmentInline(x, y, ax, ay, bx, by) {
			return true
		}
		if (ay > y) != (by > y) && x < (bx-ax)*(y-ay)/(by-ay)+ax {
			s.rings = append(s.rings, e.ring[id])
		}
	}
	// A ring crossed an odd number of times has the point inside it.
	// Sorted, a polygon's rings are together with its shell first.
	slices.Sort(s.rings)
	poly, inShell, inHole := int32(-1), false, false
	for i := 0; i < len(s.rings); {
		r := s.rings[i]
		j := i
		for j < len(s.rings) && s.rings[j] == r {
			j++
		}
		odd := (j-i)%2 == 1
		i = j
		if e.ringPoly[r] != poly {
			if inShell && !inHole {
				return true
			}
			poly, inShell, inHole = e.ringPoly[r], false, false
		}
		if !odd {
			continue
		}
		if e.ringHole[r] {
			inHole = true
		} else {
			inShell = true
		}
	}
	return inShell && !inHole
}
