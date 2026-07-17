package geometry

// Bounds is an axis-aligned 2D bounding box: (MinX, MinY, MaxX, MaxY).
type Bounds struct {
	MinX, MinY, MaxX, MaxY float64
}

// EmptyBounds returns a bounds value that will always be extended by the first
// point passed to Extend.
func EmptyBounds() Bounds {
	return Bounds{MinX: 1, MinY: 1, MaxX: -1, MaxY: -1} // deliberately inverted
}

// Empty reports whether the bounds are the zero-extent inverted sentinel.
func (b Bounds) Empty() bool { return b.MinX > b.MaxX || b.MinY > b.MaxY }

// Extend returns a Bounds enlarged to include (x, y).
func (b Bounds) Extend(x, y float64) Bounds {
	if b.Empty() {
		return Bounds{MinX: x, MinY: y, MaxX: x, MaxY: y}
	}
	if x < b.MinX {
		b.MinX = x
	}
	if x > b.MaxX {
		b.MaxX = x
	}
	if y < b.MinY {
		b.MinY = y
	}
	if y > b.MaxY {
		b.MaxY = y
	}
	return b
}

// Union returns the smallest Bounds containing both b and o.
func (b Bounds) Union(o Bounds) Bounds {
	if b.Empty() {
		return o
	}
	if o.Empty() {
		return b
	}
	return Bounds{
		MinX: min(b.MinX, o.MinX),
		MinY: min(b.MinY, o.MinY),
		MaxX: max(b.MaxX, o.MaxX),
		MaxY: max(b.MaxY, o.MaxY),
	}
}

// Contains reports whether the bounds contain (x, y). The upper edges are
// inclusive.
func (b Bounds) Contains(x, y float64) bool {
	return !b.Empty() && x >= b.MinX && x <= b.MaxX && y >= b.MinY && y <= b.MaxY
}

// Intersects reports whether the two bounds overlap.
func (b Bounds) Intersects(o Bounds) bool {
	if b.Empty() || o.Empty() {
		return false
	}
	return !(b.MaxX < o.MinX || b.MinX > o.MaxX || b.MaxY < o.MinY || b.MinY > o.MaxY)
}
