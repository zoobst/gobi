package parquetio

import (
	"os"
	"runtime"

	"github.com/apache/arrow-go/v18/parquet/file"

	"github.com/zoobst/gobi"
)

// partitionRowGroups peeks at path's row-group count and splits it
// into `workers` contiguous ranges. Each range gets its own read
// closure that runs ReadFileChunksFunc restricted to that range
// via ReadOptions.RowGroups.
//
// Returns nil when parallel scan doesn't apply: file can't be
// opened (bubble the real error at Collect via the serial path),
// only one row-group present, or ScanWorkers explicitly set to 1.
//
// Worker count resolution:
//
//	opts.ScanWorkers == 0 → runtime.GOMAXPROCS(0), capped at NumRowGroups
//	opts.ScanWorkers == 1 → nil (caller falls back to serial WithStreamRead)
//	opts.ScanWorkers >= 2 → min(opts.ScanWorkers, NumRowGroups)
func partitionRowGroups(path string, opts *ReadOptions) []func(cb func(*gobi.Frame) error) error {
	// Peek at NumRowGroups. If any step fails, fall back to serial
	// — the serial WithStreamRead callback will surface the real
	// error at Collect time.
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	pf, err := file.NewParquetReader(f)
	if err != nil {
		_ = f.Close()
		return nil
	}
	numRG := pf.NumRowGroups()
	_ = pf.Close()
	_ = f.Close()

	workers := effectiveScanWorkers(opts, numRG)
	if workers <= 1 {
		return nil
	}

	subs := make([]func(cb func(*gobi.Frame) error) error, workers)
	perWorker := numRG / workers
	remainder := numRG % workers

	start := 0
	for i := range workers {
		end := start + perWorker
		if i < remainder {
			end++ // spread the odd row-groups across the first few workers
		}
		s, e := start, end
		subs[i] = func(cb func(*gobi.Frame) error) error {
			// Copy opts so we can override RowGroups without racing
			// with sibling workers (opts.ScanWorkers is int, safe
			// to read; but we don't want to write to a shared opts).
			workerOpts := ReadOptions{}
			if opts != nil {
				workerOpts = *opts
			}
			rgs := make([]int, 0, e-s)
			for j := s; j < e; j++ {
				rgs = append(rgs, j)
			}
			workerOpts.RowGroups = rgs
			// Streaming aggregate/filter/project downstream doesn't
			// care about batch order across workers — no need to
			// preserve it here.
			return ReadFileChunksFunc(path, &workerOpts, cb)
		}
		start = end
	}
	return subs
}

// effectiveScanWorkers resolves the actual worker count from
// ReadOptions.ScanWorkers + the file's row-group count. See
// ReadOptions.ScanWorkers for the resolution rules.
func effectiveScanWorkers(opts *ReadOptions, numRowGroups int) int {
	if numRowGroups < 2 {
		return 1
	}
	requested := 0
	if opts != nil {
		requested = opts.ScanWorkers
	}
	switch requested {
	case 0:
		// Auto: use all cores, capped at row-group count.
		w := min(runtime.GOMAXPROCS(0), numRowGroups)
		return w
	case 1:
		return 1
	default:
		if requested > numRowGroups {
			return numRowGroups
		}
		return requested
	}
}
