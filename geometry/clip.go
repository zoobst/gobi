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
	// Fast paths for OpIntersection on Polygon × Polygon.
	//
	//   1. Bbox disjoint → empty (trivial reject, before either
	//      IsConvex walk).
	//   2. Convex containment (Slice 18): if either operand is a
	//      convex single-ring polygon AND the OTHER operand's
	//      exterior is fully inside the convex one, the
	//      intersection is the other operand unchanged.
	//   3. Relaxed Sutherland-Hodgman (Slice 19): if either
	//      operand is a convex single-ring polygon AND the OTHER
	//      operand is a single-ring polygon (may be concave) AND
	//      the intersection is guaranteed simply connected
	//      (subject-vertex transition count ≤ 2 against the
	//      convex clipper), run SH directly. Extends the fast
	//      path from convex×convex to convex×concave for the
	//      "AOI cuts through a coastline peninsula" shape.
	//
	// MultiPolygon inputs and cases where the intersection has
	// multiple components (transitions ≥ 4) fall through to the
	// general sweep.
	if op == OpIntersection {
		aPoly, aOK := a.(Polygon)
		bPoly, bOK := b.(Polygon)
		if aOK && bOK {
			ab, bb := aPoly.Bounds(), bPoly.Bounds()
			if !ab.Intersects(bb) {
				return Polygon{CRSValue: crs}, nil
			}
			bConvex := bPoly.IsConvex()
			// Case 1: b convex, a's exterior ⊆ b → intersection = a.
			if bConvex && boundsInsideBounds(ab, bb) {
				if aExt := aPoly.Exterior(); len(aExt) > 0 &&
					allVerticesInsideConvexRing(aExt, bPoly.Rings[0]) {
					out := aPoly
					out.CRSValue = crs
					return out, nil
				}
			}
			aConvex := aPoly.IsConvex()
			// Case 2: a convex, b's exterior ⊆ a → intersection = b.
			if aConvex && boundsInsideBounds(bb, ab) {
				if bExt := bPoly.Exterior(); len(bExt) > 0 &&
					allVerticesInsideConvexRing(bExt, aPoly.Rings[0]) {
					out := bPoly
					out.CRSValue = crs
					return out, nil
				}
			}
			// Case 3 (Slice 19): relaxed SH. Fires when exactly one
			// side is a convex single-ring clipper and the other is
			// single-ring (concavity permitted). SH is correct iff
			// the intersection is simply connected — verified via
			// intersectionSimplyConnected before running SH.
			//
			// Polygons with holes fall through (SH doesn't preserve
			// hole semantics on the subject side). aPoly / bPoly
			// single-ring check via len(Rings) == 1.
			if bConvex && len(aPoly.Rings) == 1 {
				clip := openRing(bPoly.Rings[0])
				ccw := ringSignedArea(clip) > 0
				_, safe := intersectionSimplyConnected(aPoly.Rings[0], clip, ccw)
				if safe {
					return sutherlandHodgman(aPoly.Rings[0], bPoly.Rings[0], crs), nil
				}
			}
			if aConvex && len(bPoly.Rings) == 1 {
				clip := openRing(aPoly.Rings[0])
				ccw := ringSignedArea(clip) > 0
				_, safe := intersectionSimplyConnected(bPoly.Rings[0], clip, ccw)
				if safe {
					return sutherlandHodgman(bPoly.Rings[0], aPoly.Rings[0], crs), nil
				}
			}
		}
	}
	// Fast paths for OpUnion on Polygon × Polygon (Slice 20a).
	//
	// Convex containment: if either operand is a convex single-
	// ring polygon that fully contains the other operand's
	// exterior, the union is the containing (convex) operand
	// unchanged. Union with any subset of a convex region is
	// still that region. Skips the sweep entirely.
	//
	// Holes on the contained side are absorbed — the union
	// swallows the hole shape (a point in b's hole is not in b
	// but IS in a, so it stays in the union = a). Correct
	// semantics preserved.
	if op == OpUnion {
		aPoly, aOK := a.(Polygon)
		bPoly, bOK := b.(Polygon)
		if aOK && bOK {
			ab, bb := aPoly.Bounds(), bPoly.Bounds()
			if bPoly.IsConvex() && boundsInsideBounds(ab, bb) {
				if aExt := aPoly.Exterior(); len(aExt) > 0 &&
					allVerticesInsideConvexRing(aExt, bPoly.Rings[0]) {
					out := bPoly
					out.CRSValue = crs
					return out, nil
				}
			}
			if aPoly.IsConvex() && boundsInsideBounds(bb, ab) {
				if bExt := bPoly.Exterior(); len(bExt) > 0 &&
					allVerticesInsideConvexRing(bExt, aPoly.Rings[0]) {
					out := aPoly
					out.CRSValue = crs
					return out, nil
				}
			}
		}
	}
	// Fast path for OpDifference on Polygon × Polygon (Slice 20b).
	//
	// If b is convex AND a's exterior is fully inside b → a ⊆ b →
	// a - b = empty. Skips the sweep.
	//
	// The "a ⊇ b → a - b = a with a b-shaped hole" case is NOT a
	// fast path — constructing the result polygon-with-hole
	// requires the sweep's ring reconnection logic (or a
	// duplicate implementation), and the wins wouldn't justify
	// the code.
	if op == OpDifference {
		aPoly, aOK := a.(Polygon)
		bPoly, bOK := b.(Polygon)
		if aOK && bOK {
			ab, bb := aPoly.Bounds(), bPoly.Bounds()
			if bPoly.IsConvex() && boundsInsideBounds(ab, bb) {
				if aExt := aPoly.Exterior(); len(aExt) > 0 &&
					allVerticesInsideConvexRing(aExt, bPoly.Rings[0]) {
					return Polygon{CRSValue: crs}, nil
				}
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
