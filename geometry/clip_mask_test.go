package geometry

import (
	"math"
	"math/rand/v2"
	"reflect"
	"testing"
)

// starMask builds a mask of n star-shaped polygons on a grid, every
// other one with a hole, plus a row of square tiles that share edges.
func starMask(r *rand.Rand, n, vertices int) MultiPolygon {
	var polys []Polygon
	for i := range n {
		cx, cy := float64(i%6)*120, float64(i/6)*120
		shell := make([]Point, 0, vertices+1)
		for k := range vertices {
			a := 2 * math.Pi * (float64(k) + 0.8*r.Float64()) / float64(vertices)
			rad := 20 + 30*r.Float64()
			shell = append(shell, pt(cx+rad*math.Cos(a), cy+rad*math.Sin(a)))
		}
		shell = append(shell, shell[0])
		rings := [][]Point{shell}
		if i%2 == 0 {
			rings = append(rings, []Point{pt(cx-4, cy-3), pt(cx+5, cy-2), pt(cx+1, cy+6), pt(cx-4, cy-3)})
		}
		polys = append(polys, Polygon{Rings: rings})
	}
	for i := range 5 {
		polys = append(polys, unitSquare(float64(i)*40, -200, 40))
	}
	return MultiPolygon{Polygons: polys}
}

// The edge table answers "is this point in the mask" exactly as
// pointInPolygon does member by member, and gives the same clipped
// lines whether its R-tree has been built or not.
func TestClipMask_IndexMatchesScan(t *testing.T) {
	r := rand.New(rand.NewPCG(11, 4))
	mask := starMask(r, 18, 40)

	scan, err := NewClipMask(mask)
	if err != nil {
		t.Fatal(err)
	}
	indexed, err := NewClipMask(mask)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []*ClipMask{scan, indexed} {
		m.edgesOnce.Do(func() { m.edges = newMaskEdges(m.polys) })
	}
	scan.edges.scanned.Store(math.MinInt64 / 2) // never reaches the payoff
	indexed.edges.treeOnce.Do(func() { indexed.edges.tree.Store(NewRTree(indexed.edges.bounds)) })
	if len(scan.edges.bounds) < maskIndexMinEdges {
		t.Fatalf("mask has only %d edges; the test needs enough to index", len(scan.edges.bounds))
	}

	// Points: random ones, every mask vertex, and every edge midpoint
	// of the axis-aligned tiles (exactly on the boundary).
	points := make([]Point, 0, 6000)
	for range 4000 {
		points = append(points, pt(r.Float64()*800-100, r.Float64()*600-260))
	}
	for _, p := range mask.Polygons {
		for _, ring := range p.Rings {
			points = append(points, ring...)
		}
	}
	for i := range 5 {
		x := float64(i) * 40
		points = append(points, pt(x+20, -200), pt(x+40, -180), pt(x+20, -160), pt(x, -180))
	}
	var s maskScratch
	inside := 0
	for _, p := range points {
		want := false
		for _, poly := range mask.Polygons {
			want = want || pointInPolygon(p, poly)
		}
		if want {
			inside++
		}
		if got := scan.edges.contains(p.X, p.Y, &s); got != want {
			t.Fatalf("scan: contains(%v, %v) = %v, pointInPolygon says %v", p.X, p.Y, got, want)
		}
		if got := indexed.edges.contains(p.X, p.Y, &s); got != want {
			t.Fatalf("indexed: contains(%v, %v) = %v, pointInPolygon says %v", p.X, p.Y, got, want)
		}
		// The point path (Prepare) agrees too.
		got, err := indexed.Clip(p)
		if err != nil {
			t.Fatal(err)
		}
		if _, kept := got.(Point); kept != want {
			t.Fatalf("Clip(point %v, %v) kept = %v, want %v", p.X, p.Y, kept, want)
		}
	}
	if inside < 500 || inside > len(points)-500 {
		t.Fatalf("%d of %d points inside: the test isn't exercising both answers", inside, len(points))
	}

	// Lines: random walks across the whole mask.
	for range 200 {
		line := LineString{Points: []Point{pt(r.Float64()*800-100, r.Float64()*600-260)}}
		for range 2 + r.IntN(30) {
			last := line.Points[len(line.Points)-1]
			line.Points = append(line.Points, pt(last.X+r.Float64()*160-80, last.Y+r.Float64()*160-80))
		}
		a, err := scan.Clip(line)
		if err != nil {
			t.Fatal(err)
		}
		b, err := indexed.Clip(line)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(a, b) {
			t.Fatalf("scan and index disagree on %v:\n scan  %+v\n index %+v", line.Points, a, b)
		}
		// Every piece's midpoints are in the mask.
		for _, piece := range appendLines(nil, a) {
			for i := 1; i < len(piece.Points); i++ {
				mid := lerp(piece.Points[i-1], piece.Points[i], 0.5)
				if !scan.edges.contains(mid.X, mid.Y, &s) {
					t.Fatalf("piece %v has a stretch outside the mask at %v", piece.Points, mid)
				}
			}
		}
	}
}

func appendLines(dst []LineString, g Geometry) []LineString {
	switch t := g.(type) {
	case LineString:
		if len(t.Points) > 0 {
			dst = append(dst, t)
		}
	case MultiLineString:
		dst = append(dst, t.Lines...)
	}
	return dst
}

// The R-tree is built only once the scans it would have saved add up:
// never for one short line, and part-way through a long one.
func TestClipMask_BuildsIndexWhenItPays(t *testing.T) {
	r := rand.New(rand.NewPCG(2, 2))
	mask := starMask(r, 6, 60)

	m, err := NewClipMask(mask)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Clip(LineString{Points: []Point{pt(-50, 0), pt(50, 0)}}); err != nil {
		t.Fatal(err)
	}
	if m.edges.tree.Load() != nil {
		t.Error("one two-point line built the index")
	}
	long := LineString{}
	for i := range 400 {
		long.Points = append(long.Points, pt(float64(i%7)*100-50, float64(i%5)*100-50))
	}
	if _, err := m.Clip(long); err != nil {
		t.Fatal(err)
	}
	if m.edges.tree.Load() == nil {
		t.Error("a 400-vertex line did not build the index")
	}

	// A single convex polygon needs neither the edge table nor a tree.
	box, err := NewClipMask(unitSquare(0, 0, 10))
	if err != nil {
		t.Fatal(err)
	}
	got, err := box.Clip(LineString{Points: []Point{pt(-5, 5), pt(15, 5)}})
	if err != nil {
		t.Fatal(err)
	}
	if l, ok := got.(LineString); !ok || len(l.Points) != 2 || l.Points[0].X != 0 || l.Points[1].X != 10 {
		t.Errorf("convex mask: got %T %+v, want the piece from x=0 to x=10", got, got)
	}
	if box.edges != nil {
		t.Error("a convex mask built the edge table")
	}

	// NewClipMask takes the same masks Clip does.
	if _, err := NewClipMask(LineString{}); err == nil {
		t.Error("NewClipMask(LineString): expected an error")
	}
	if _, err := NewClipMask(nil); err == nil {
		t.Error("NewClipMask(nil): expected an error")
	}
	// And a polygon subject is the ordinary intersection.
	got, err = box.Clip(unitSquare(5, 5, 10))
	if err != nil {
		t.Fatal(err)
	}
	if a := clipTotalArea(got); math.Abs(a-25) > 1e-9 {
		t.Errorf("polygon subject: area = %v, want 25", a)
	}
}

// BenchmarkClipMask_LongLine clips a 10k-vertex line by a 100k-vertex
// coastline-like polygon whose bounds cover it: the case where
// comparing every segment with every edge costs a billion edge tests.
// "scan" is that, on a tenth of the line (so it finishes); "indexed"
// is Clip as it ships, which builds the edge R-tree part-way through.
func BenchmarkClipMask_LongLine(b *testing.B) {
	const edges, vertices = 100_000, 10_000
	shell := make([]Point, 0, edges+1)
	for k := range edges {
		a := 2 * math.Pi * float64(k) / edges
		rad := 1000 + 40*math.Sin(7*a) + 5*math.Sin(131*a)
		shell = append(shell, pt(rad*math.Cos(a), rad*math.Sin(a)))
	}
	shell = append(shell, shell[0])
	mask := Polygon{Rings: [][]Point{shell}}
	line := LineString{}
	for i := range vertices {
		a := 2 * math.Pi * float64(i) / 97
		rad := 1100 * float64(i) / vertices
		line.Points = append(line.Points, pt(rad*math.Cos(a), rad*math.Sin(a)))
	}
	b.Run("indexed", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := Clip(line, mask); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("scan_tenth_of_line", func(b *testing.B) {
		// The outer tenth: the part that reaches the coastline.
		tail := LineString{Points: line.Points[vertices-vertices/10:]}
		for b.Loop() {
			m, err := NewClipMask(mask)
			if err != nil {
				b.Fatal(err)
			}
			m.edgesOnce.Do(func() { m.edges = newMaskEdges(m.polys) })
			m.edges.scanned.Store(math.MinInt64 / 2)
			if _, err := m.Clip(tail); err != nil {
				b.Fatal(err)
			}
		}
	})
}
