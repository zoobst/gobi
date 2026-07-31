package athenaio

import (
	"runtime"
	"testing"
	"time"
	"unsafe"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// TestStatsRegistry_DoesNotPinLazyFrame guards the invariant that
// registerStats does not hold a strong reference to its LazyFrame —
// so once the caller drops lf, GC is free to collect it and (via
// the runtime.AddCleanup callback) drop the corresponding registry
// entry.
//
// Before v0.1.8 the registry was keyed by *gobi.LazyFrame, which
// meant the map itself kept every athenaio-produced LazyFrame — and
// transitively its scanFrameNode.frame and every arrow column
// beneath it — pinned for the process's lifetime. On long-lived
// clients running many UnloadAndRead calls that compounded into
// multi-GB of unreleasable arrow memory. The fix switched the key
// to uintptr (an opaque integer to the GC) and installed a
// runtime.AddCleanup on each registered lf that removes the entry
// when GC finds lf unreachable.
//
// Verification: register N stats, drop every Go reference to the
// registered LazyFrames, force GC + poll for cleanup callbacks to
// fire, and assert every one of our specific keys drained from the
// registry. With the pre-fix pointer-keyed map, the entries stay
// for the process's lifetime and the assertion fails with a count.
func TestStatsRegistry_DoesNotPinLazyFrame(t *testing.T) {
	// Track the specific registry keys created by this test so the
	// assertion is insensitive to concurrent registrations by other
	// tests in the package (UnloadAndRead*, RawCTAS*, etc. all
	// register stats too). We only care that OUR registrations
	// eventually drain — not that the registry hits any absolute size.
	const n = 8
	var myKeys []uintptr

	func() {
		pool := memory.NewCheckedAllocator(memory.DefaultAllocator)
		for i := range n {
			f := buildI64FrameForTest(t, pool, "x", []int64{int64(i)})
			lf := f.Lazy()
			registerStats(lf, QueryStats{QueryExecutionID: "leak-test"})
			// Same key computation registerStats uses — records our
			// own entries so the assertion is insensitive to unrelated
			// registrations by concurrent tests.
			myKeys = append(myKeys, uintptr(unsafe.Pointer(lf)))
		}
	}()

	// runtime.AddCleanup schedules cleanups on a separate goroutine
	// once GC finds the target unreachable. Poll for our keys to
	// drain — the runtime doesn't expose a synchronous "run pending
	// cleanups" hook.
	waitForOurKeysToDrain(myKeys)

	remaining := countKeysRemaining(myKeys)
	if remaining != 0 {
		t.Fatalf("stats registry pinned %d/%d LazyFrames past their caller's scope; "+
			"expected auto-cleanup to drain the entries", remaining, n)
	}
}

// TestStatsRegistry_ClearStatsWorks guards the explicit-cleanup path
// (callers who want deterministic map-entry removal before GC).
func TestStatsRegistry_ClearStatsWorks(t *testing.T) {
	pool := memory.NewCheckedAllocator(memory.DefaultAllocator)
	defer pool.AssertSize(t, 0)

	f := buildI64FrameForTest(t, pool, "x", []int64{1, 2, 3, 4})
	defer f.Release()

	lf := f.Lazy()
	registerStats(lf, QueryStats{QueryExecutionID: "clear-test"})
	if _, ok := StatsFor(lf); !ok {
		t.Fatal("registerStats did not install an entry")
	}

	ClearStats(lf)
	if _, ok := StatsFor(lf); ok {
		t.Fatal("ClearStats did not drop the entry")
	}
}

func waitForOurKeysToDrain(keys []uintptr) {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		runtime.GC()
		runtime.GC()
		time.Sleep(10 * time.Millisecond)
		if countKeysRemaining(keys) == 0 {
			return
		}
	}
}

func countKeysRemaining(keys []uintptr) int {
	statsMu.RLock()
	defer statsMu.RUnlock()
	n := 0
	for _, k := range keys {
		if _, ok := statsRegistry[k]; ok {
			n++
		}
	}
	return n
}

// Import guard — arrow imports only referenced through the test
// helpers pulled from unload_refcount_test.go. If those helpers ever
// move packages, this file needs updating in lockstep.
var _ = arrow.PrimitiveTypes.Int64
var _ = array.NewInt64Builder
