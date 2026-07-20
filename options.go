package gobi

import (
	"runtime"
	"sync/atomic"
)

// globalMaxParallelism carries the package-level default worker count. A
// value of 0 means "defer to GOMAXPROCS at call time." Access is atomic so
// concurrent gobi operations see a consistent value without a lock.
var globalMaxParallelism atomic.Int64

// Option configures a parallel gobi operation. Values are applied to an
// internal options struct in the order the caller supplies them, so later
// options override earlier ones.
//
// Every parallel entry point (currently SJoin; more to come) accepts a
// trailing `...Option`. Callers that don't care can omit them entirely.
type Option interface {
	apply(*options)
}

// options is the internal, aggregated configuration a parallel operation
// runs with. Zero-valued fields fall back to the package-level defaults set
// by SetMaxParallelism, which in turn defer to GOMAXPROCS.
type options struct {
	workers int // 0 = defer to package default; <0 also treated as unset
}

// -- Workers ------------------------------------------------------------

type workersOpt struct{ n int }

func (o workersOpt) apply(s *options) { s.workers = o.n }

// Workers overrides the maximum number of parallel workers used by the
// operation it is passed to.
//
//   - Workers(n) with n > 0 caps parallelism at n workers.
//   - Workers(0) or Workers(-1) means "use the package default" (see
//     SetMaxParallelism); if that too is unset, GOMAXPROCS is used.
//   - Workers(1) forces sequential execution.
func Workers(n int) Option { return workersOpt{n: n} }

// -- Global default -----------------------------------------------------

// SetMaxParallelism sets the default max worker count for every parallel
// gobi operation across the process. Pass 0 (the default) to defer to
// GOMAXPROCS. Individual calls can override with the Workers option.
//
// Negative values are treated as zero.
func SetMaxParallelism(n int) {
	if n < 0 {
		n = 0
	}
	globalMaxParallelism.Store(int64(n))
}

// MaxParallelism returns the current package-level default worker count.
// Returns 0 if unset (in which case operations use GOMAXPROCS).
func MaxParallelism() int { return int(globalMaxParallelism.Load()) }

// resolveWorkers folds opts into an effective worker count using this
// priority:
//
//  1. Workers(n) with n > 0 wins.
//  2. Otherwise, MaxParallelism() if > 0.
//  3. Otherwise, GOMAXPROCS.
//
// The result is guaranteed to be >= 1.
func resolveWorkers(opts ...Option) int {
	var cfg options
	for _, o := range opts {
		o.apply(&cfg)
	}
	n := cfg.workers
	if n <= 0 {
		n = MaxParallelism()
	}
	if n <= 0 {
		n = runtime.GOMAXPROCS(0)
	}
	if n < 1 {
		n = 1
	}
	return n
}
