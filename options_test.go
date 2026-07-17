package gobi

import (
	"runtime"
	"sync"
	"testing"
)

func TestResolveWorkers_DefaultUsesGOMAXPROCS(t *testing.T) {
	SetMaxParallelism(0)
	got := resolveWorkers()
	want := runtime.GOMAXPROCS(0)
	if got != want {
		t.Fatalf("default = %d, want GOMAXPROCS %d", got, want)
	}
}

func TestResolveWorkers_GlobalDefault(t *testing.T) {
	SetMaxParallelism(3)
	t.Cleanup(func() { SetMaxParallelism(0) })
	if got := resolveWorkers(); got != 3 {
		t.Fatalf("resolveWorkers() = %d, want 3", got)
	}
	if got := MaxParallelism(); got != 3 {
		t.Fatalf("MaxParallelism() = %d, want 3", got)
	}
}

func TestResolveWorkers_PerOpOverride(t *testing.T) {
	SetMaxParallelism(3)
	t.Cleanup(func() { SetMaxParallelism(0) })
	if got := resolveWorkers(Workers(7)); got != 7 {
		t.Fatalf("Workers(7) override = %d, want 7", got)
	}
}

func TestResolveWorkers_WorkersZeroFallsBackToDefault(t *testing.T) {
	SetMaxParallelism(5)
	t.Cleanup(func() { SetMaxParallelism(0) })
	if got := resolveWorkers(Workers(0)); got != 5 {
		t.Fatalf("Workers(0) with default 5 = %d, want 5", got)
	}
	if got := resolveWorkers(Workers(-1)); got != 5 {
		t.Fatalf("Workers(-1) with default 5 = %d, want 5", got)
	}
}

func TestResolveWorkers_WorkersOneForcesSequential(t *testing.T) {
	SetMaxParallelism(0)
	if got := resolveWorkers(Workers(1)); got != 1 {
		t.Fatalf("Workers(1) = %d, want 1", got)
	}
}

func TestResolveWorkers_NegativeGlobalTreatedAsZero(t *testing.T) {
	SetMaxParallelism(-4)
	t.Cleanup(func() { SetMaxParallelism(0) })
	if got := MaxParallelism(); got != 0 {
		t.Fatalf("MaxParallelism after SetMaxParallelism(-4) = %d, want 0", got)
	}
}

func TestResolveWorkers_LastOptionWins(t *testing.T) {
	SetMaxParallelism(0)
	got := resolveWorkers(Workers(2), Workers(9))
	if got != 9 {
		t.Fatalf("last-option-wins = %d, want 9", got)
	}
}

func TestSJoin_ResultsEqualAcrossWorkerCounts(t *testing.T) {
	// Build a point cloud + polygon grid large enough to exercise the
	// parallel path (>= SJoinMinParallelRows). The predicate is
	// deterministic per (left, right) pair, so identical row counts under
	// Workers(1) and Workers(N) imply identical match sets.
	polygons := buildBenchPolygonGrid(t, 20) // 400 unit polygons
	points := buildBenchPointCloud(t, 4096, 20)

	seq, err := points.SJoin(polygons, "geometry", "geometry", SPIntersects, Workers(1))
	if err != nil {
		t.Fatal(err)
	}
	par, err := points.SJoin(polygons, "geometry", "geometry", SPIntersects, Workers(8))
	if err != nil {
		t.Fatal(err)
	}
	if a, b := seq.NumRows(), par.NumRows(); a != b {
		t.Fatalf("row counts differ: seq=%d, par=%d", a, b)
	}
	if seq.NumRows() == 0 {
		t.Fatal("test produced zero matches; fixture too sparse to be meaningful")
	}
}

func TestSJoin_ConcurrentCallsWithBoundedWorkers(t *testing.T) {
	// Multiple concurrent SJoins each capped at Workers(2) should not race,
	// crash, or produce inconsistent results.
	polygons := buildBenchPolygonGrid(t, 15)
	points := buildBenchPointCloud(t, 4096, 15)

	expected, err := points.SJoin(polygons, "geometry", "geometry", SPIntersects, Workers(1))
	if err != nil {
		t.Fatal(err)
	}
	want := expected.NumRows()

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			out, err := points.SJoin(polygons, "geometry", "geometry", SPIntersects, Workers(2))
			if err != nil {
				t.Errorf("concurrent SJoin: %v", err)
				return
			}
			if got := out.NumRows(); got != want {
				t.Errorf("concurrent row count = %d, want %d", got, want)
			}
		})
	}
	wg.Wait()
}
