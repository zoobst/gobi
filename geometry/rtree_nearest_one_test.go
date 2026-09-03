package geometry

import (
	"math/rand"
	"testing"
)

// TestRTree_NearestOne_Empty — empty tree returns ok=false.
func TestRTree_NearestOne_Empty(t *testing.T) {
	tree := NewRTree(nil)
	if _, ok := tree.NearestOne(0, 0); ok {
		t.Errorf("empty tree NearestOne ok = true, want false")
	}
}

// TestRTree_NearestOne_SingleItem — one-item tree returns that item
// regardless of query location.
func TestRTree_NearestOne_SingleItem(t *testing.T) {
	tree := NewRTree([]Bounds{{MinX: 5, MinY: 5, MaxX: 5, MaxY: 5}})
	for _, q := range []struct{ x, y float64 }{
		{0, 0}, {100, 100}, {-50, 5}, {5, 5},
	} {
		id, ok := tree.NearestOne(q.x, q.y)
		if !ok {
			t.Errorf("query (%v,%v): ok = false", q.x, q.y)
		}
		if id != 0 {
			t.Errorf("query (%v,%v): id = %d, want 0", q.x, q.y, id)
		}
	}
}

// TestRTree_NearestOne_MatchesNearest — NearestOne must return the
// same item as Nearest(x, y, 1)[0] on any query. Runs a randomized
// battery to catch divergences.
func TestRTree_NearestOne_MatchesNearest(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	const N = 500
	bounds := make([]Bounds, N)
	for i := range bounds {
		x := rng.Float64() * 1000
		y := rng.Float64() * 1000
		bounds[i] = Bounds{
			MinX: x, MinY: y,
			MaxX: x + rng.Float64()*5,
			MaxY: y + rng.Float64()*5,
		}
	}
	tree := NewRTree(bounds)
	for iter := range 200 {
		qx := rng.Float64() * 1000
		qy := rng.Float64() * 1000
		gotID, ok := tree.NearestOne(qx, qy)
		if !ok {
			t.Fatalf("iter %d: NearestOne ok = false on non-empty tree", iter)
		}
		want := tree.Nearest(qx, qy, 1)
		if len(want) != 1 {
			t.Fatalf("iter %d: Nearest(k=1) returned %d IDs", iter, len(want))
		}
		if gotID != want[0] {
			// Tie-break case: the two closest items may be at the
			// exact same distance and both are valid answers.
			// Compare distances directly to avoid false failures.
			gotD := tree.itemBboxDist(gotID, qx, qy)
			wantD := tree.itemBboxDist(want[0], qx, qy)
			if gotD != wantD {
				t.Errorf("iter %d @ (%v,%v): NearestOne=%d (d=%v) Nearest[0]=%d (d=%v)",
					iter, qx, qy, gotID, gotD, want[0], wantD)
			}
		}
	}
}

// TestRTree_NearestOne_ZeroAllocations — the whole point of the
// fast path. Runs NearestOne in a tight loop and asserts no heap
// allocations after warmup.
func TestRTree_NearestOne_ZeroAllocations(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	bounds := make([]Bounds, 1000)
	for i := range bounds {
		x := rng.Float64() * 1000
		y := rng.Float64() * 1000
		bounds[i] = Bounds{MinX: x, MinY: y, MaxX: x + 1, MaxY: y + 1}
	}
	tree := NewRTree(bounds)

	// Warm up the query so any inlining / stack-growth is done.
	tree.NearestOne(500, 500)

	allocs := testing.AllocsPerRun(200, func() {
		tree.NearestOne(rng.Float64()*1000, rng.Float64()*1000)
	})
	if allocs != 0 {
		t.Errorf("NearestOne: %v allocs/op, want 0", allocs)
	}
}

// BenchmarkRTree_NearestOne_vs_Nearest quantifies the fast-path
// win on a realistic-sized index. 100k items, 10k queries per
// iteration — the shape a snap-to-graph workflow sees at scale.
func BenchmarkRTree_NearestOne_vs_Nearest(b *testing.B) {
	rng := rand.New(rand.NewSource(1))
	const N = 100_000
	bounds := make([]Bounds, N)
	for i := range bounds {
		x := rng.Float64() * 10_000
		y := rng.Float64() * 10_000
		bounds[i] = Bounds{MinX: x, MinY: y, MaxX: x + 1, MaxY: y + 1}
	}
	tree := NewRTree(bounds)

	queries := make([][2]float64, 10_000)
	for i := range queries {
		queries[i] = [2]float64{rng.Float64() * 10_000, rng.Float64() * 10_000}
	}

	b.Run("NearestOne", func(b *testing.B) {
		b.ReportAllocs()
		var sink int32
		for i := 0; i < b.N; i++ {
			for _, q := range queries {
				id, _ := tree.NearestOne(q[0], q[1])
				sink ^= id
			}
		}
		_ = sink
	})

	b.Run("Nearest_k1", func(b *testing.B) {
		b.ReportAllocs()
		var sink int32
		for i := 0; i < b.N; i++ {
			for _, q := range queries {
				out := tree.Nearest(q[0], q[1], 1)
				if len(out) > 0 {
					sink ^= out[0]
				}
			}
		}
		_ = sink
	})
}
