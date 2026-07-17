package geometry

import (
	"math/rand"
	"sort"
	"testing"
)

func TestRTree_Empty(t *testing.T) {
	tree := NewRTree(nil)
	if tree.Len() != 0 {
		t.Fatalf("len = %d", tree.Len())
	}
	if got := tree.Search(Bounds{MinX: 0, MinY: 0, MaxX: 1, MaxY: 1}); len(got) != 0 {
		t.Fatalf("empty search: %v", got)
	}
	if got := tree.Nearest(0, 0, 5); len(got) != 0 {
		t.Fatalf("empty nearest: %v", got)
	}
}

func TestRTree_SingleItem(t *testing.T) {
	tree := NewRTree([]Bounds{{MinX: 0, MinY: 0, MaxX: 1, MaxY: 1}})
	got := tree.Search(Bounds{MinX: 0.5, MinY: 0.5, MaxX: 0.6, MaxY: 0.6})
	if len(got) != 1 || got[0] != 0 {
		t.Fatalf("search: %v", got)
	}
	got = tree.Search(Bounds{MinX: 2, MinY: 2, MaxX: 3, MaxY: 3})
	if len(got) != 0 {
		t.Fatalf("disjoint search returned: %v", got)
	}
}

func TestRTree_SearchGridMatchesBruteForce(t *testing.T) {
	// 10x10 grid of unit boxes; query rectangle picks a 3x3 window.
	var bounds []Bounds
	for y := 0; y < 10; y++ {
		for x := 0; x < 10; x++ {
			bounds = append(bounds, Bounds{
				MinX: float64(x), MinY: float64(y),
				MaxX: float64(x) + 0.9, MaxY: float64(y) + 0.9,
			})
		}
	}
	tree := NewRTree(bounds)
	q := Bounds{MinX: 3.2, MinY: 3.2, MaxX: 5.8, MaxY: 5.8}
	got := tree.Search(q)
	sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })

	var want []int32
	for i, b := range bounds {
		if b.Intersects(q) {
			want = append(want, int32(i))
		}
	}
	if !equalInt32(got, want) {
		t.Fatalf("search got %v want %v", got, want)
	}
}

func TestRTree_LargeRandomSearchMatchesBruteForce(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	n := 500
	bounds := make([]Bounds, n)
	for i := 0; i < n; i++ {
		x := rng.Float64() * 100
		y := rng.Float64() * 100
		w := rng.Float64() * 3
		h := rng.Float64() * 3
		bounds[i] = Bounds{MinX: x, MinY: y, MaxX: x + w, MaxY: y + h}
	}
	tree := NewRTree(bounds)

	for iter := 0; iter < 20; iter++ {
		qx := rng.Float64() * 100
		qy := rng.Float64() * 100
		q := Bounds{MinX: qx, MinY: qy, MaxX: qx + 5, MaxY: qy + 5}
		got := tree.Search(q)
		sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })

		var want []int32
		for i, b := range bounds {
			if b.Intersects(q) {
				want = append(want, int32(i))
			}
		}
		if !equalInt32(got, want) {
			t.Fatalf("iter %d: got %v want %v", iter, got, want)
		}
	}
}

func TestRTree_NearestReturnsSortedIDs(t *testing.T) {
	// Place points along y=0 at x = 0, 1, 2, ..., 9. Query from (0.5, 0)
	// — the two closest should be items 0 and 1.
	var bounds []Bounds
	for i := 0; i < 10; i++ {
		bounds = append(bounds, Bounds{
			MinX: float64(i), MinY: 0, MaxX: float64(i), MaxY: 0,
		})
	}
	tree := NewRTree(bounds)
	got := tree.Nearest(0.5, 0, 3)
	// Distances: 0 → 0.5, 1 → 0.5, 2 → 1.5, …
	// The first two (order between 0 and 1 doesn't matter) should be {0,1}.
	if len(got) != 3 {
		t.Fatalf("nearest count = %d, want 3", len(got))
	}
	set := map[int32]bool{got[0]: true, got[1]: true}
	if !set[0] || !set[1] {
		t.Fatalf("first two nearest = %v, want {0,1}", got[:2])
	}
	if got[2] != 2 {
		t.Fatalf("third nearest = %d, want 2", got[2])
	}
}

func TestRTree_NearestKGreaterThanN(t *testing.T) {
	tree := NewRTree([]Bounds{
		{MinX: 0, MinY: 0, MaxX: 0, MaxY: 0},
		{MinX: 5, MinY: 5, MaxX: 5, MaxY: 5},
	})
	got := tree.Nearest(0, 0, 100)
	if len(got) != 2 {
		t.Fatalf("got %d, want 2", len(got))
	}
}

func equalInt32(a, b []int32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
