package geometry

import (
	"errors"
	"fmt"
)

// ErrGeographicCRS is returned by boolean-op functions when either input is
// in a geographic (non-projected) CRS. The current engine is planar-only;
// geographic inputs must be reprojected to a projected CRS first (see
// EstimateUTMCRS / ToCRS).
var ErrGeographicCRS = errors.New("geometry: boolean ops require a projected CRS")

// Clip returns the geometric intersection of subject and mask. Both must be
// Polygon or MultiPolygon in the same projected CRS. Non-polygonal inputs
// return an error. An empty result is a Polygon with no rings.
func Clip(subject, mask Geometry) (Geometry, error) {
	return Boolean(subject, mask, OpIntersection, ClipOptions{})
}

// Union returns the geometric union of a and b. See Clip for input rules.
func Union(a, b Geometry) (Geometry, error) {
	return Boolean(a, b, OpUnion, ClipOptions{})
}

// Difference returns a minus b (points in a that are not in b). See Clip
// for input rules.
func Difference(a, b Geometry) (Geometry, error) {
	return Boolean(a, b, OpDifference, ClipOptions{})
}

// SymDifference returns the symmetric difference of a and b (points in
// exactly one of them). See Clip for input rules.
func SymDifference(a, b Geometry) (Geometry, error) {
	return Boolean(a, b, OpSymDifference, ClipOptions{})
}

// Boolean applies op to a and b using the given options. Boolean is the
// shared entry point that Clip/Union/Difference/SymDifference route through.
func Boolean(a, b Geometry, op BoolOp, opts ClipOptions) (Geometry, error) {
	if err := validateClipInputs(a, b); err != nil {
		return nil, err
	}
	crs := resolveClipCRS(a, b)
	// Fast path: intersection of two convex single-ring polygons. The
	// Sutherland-Hodgman clipper is O(n+m) with no allocations for the
	// sweep-line status structure or event queue, versus the general
	// Martinez-Rueda sweep's O((n+m) log (n+m)). Both inputs must be
	// single-ring Polygons; MultiPolygon inputs fall through.
	if op == OpIntersection {
		if aPoly, ok := a.(Polygon); ok {
			if bPoly, ok := b.(Polygon); ok && aPoly.IsConvex() && bPoly.IsConvex() {
				if !aPoly.Bounds().Intersects(bPoly.Bounds()) {
					return Polygon{CRSValue: crs}, nil
				}
				return sutherlandHodgman(aPoly.Rings[0], bPoly.Rings[0], crs), nil
			}
		}
	}
	session := clipSession{op: op, tol: opts.tolerance()}
	defer session.done()
	if err := enqueueGeometry(&session, a, roleSubject); err != nil {
		return nil, err
	}
	if err := enqueueGeometry(&session, b, roleClipping); err != nil {
		return nil, err
	}
	// Trivial-reject cases: bboxes disjoint.
	if session.queue.Len() == 0 {
		return Polygon{CRSValue: crs}, nil
	}
	ab, bb := a.Bounds(), b.Bounds()
	if !ab.Intersects(bb) {
		return trivialReject(a, b, op, crs), nil
	}
	sorted := session.sweep()
	rings := session.connectContours(sorted)
	return assemble(rings, crs), nil
}

// trivialReject returns the correct output for op when a's bbox is disjoint
// from b's, without running the sweepline.
func trivialReject(a, b Geometry, op BoolOp, crs CRS) Geometry {
	switch op {
	case OpIntersection:
		return Polygon{CRSValue: crs}
	case OpDifference:
		return copyWithCRS(a, crs)
	case OpUnion, OpSymDifference:
		// Result is the two operands as a MultiPolygon.
		return combineDisjoint(a, b, crs)
	}
	return Polygon{CRSValue: crs}
}

// combineDisjoint returns a MultiPolygon (or Polygon if only one operand)
// containing every polygon component of a and b. Both must be Polygon or
// MultiPolygon per validateClipInputs.
func combineDisjoint(a, b Geometry, crs CRS) Geometry {
	polys := make([]Polygon, 0, 2)
	polys = appendPolygons(polys, a)
	polys = appendPolygons(polys, b)
	if len(polys) == 0 {
		return Polygon{CRSValue: crs}
	}
	if len(polys) == 1 {
		p := polys[0]
		p.CRSValue = crs
		return p
	}
	for i := range polys {
		polys[i].CRSValue = crs
	}
	return MultiPolygon{Polygons: polys, CRSValue: crs}
}

func appendPolygons(dst []Polygon, g Geometry) []Polygon {
	switch t := g.(type) {
	case Polygon:
		if len(t.Rings) > 0 {
			dst = append(dst, t)
		}
	case MultiPolygon:
		for _, p := range t.Polygons {
			if len(p.Rings) > 0 {
				dst = append(dst, p)
			}
		}
	}
	return dst
}

func copyWithCRS(g Geometry, crs CRS) Geometry {
	switch t := g.(type) {
	case Polygon:
		t.CRSValue = crs
		return t
	case MultiPolygon:
		t.CRSValue = crs
		return t
	}
	return g
}

// enqueueGeometry pushes every ring of g onto the session's queue under the
// given role. Accepts Polygon or MultiPolygon.
func enqueueGeometry(s *clipSession, g Geometry, role polyRole) error {
	switch t := g.(type) {
	case Polygon:
		s.enqueuePolygon(t, role)
		return nil
	case MultiPolygon:
		s.enqueueMultiPolygon(t, role)
		return nil
	}
	return fmt.Errorf("geometry: boolean op requires Polygon or MultiPolygon, got %T", g)
}

// validateClipInputs checks input shape and CRS consistency.
func validateClipInputs(a, b Geometry) error {
	if a == nil || b == nil {
		return fmt.Errorf("geometry: boolean op: nil geometry")
	}
	if err := requirePolygonal(a); err != nil {
		return err
	}
	if err := requirePolygonal(b); err != nil {
		return err
	}
	ac := a.CRS()
	bc := b.CRS()
	if !ac.Zero() && !bc.Zero() && !ac.Equal(bc) {
		return ErrCRSMismatch
	}
	crs := ac
	if crs.Zero() {
		crs = bc
	}
	if !crs.Zero() && !crs.Projected {
		return fmt.Errorf("%w: got %s", ErrGeographicCRS, crs)
	}
	return nil
}

func requirePolygonal(g Geometry) error {
	switch g.(type) {
	case Polygon, MultiPolygon:
		return nil
	}
	return fmt.Errorf("geometry: boolean op requires Polygon or MultiPolygon, got %T", g)
}

// resolveClipCRS picks the CRS to attach to the output. Callers with two
// operands agree on a CRS (validateClipInputs enforces it); an unset CRS on
// one side defers to the other.
func resolveClipCRS(a, b Geometry) CRS {
	if c := a.CRS(); !c.Zero() {
		return c
	}
	return b.CRS()
}
