package athenaio

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"time"
	"unsafe"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/athena"
	athenatypes "github.com/aws/aws-sdk-go-v2/service/athena/types"

	"github.com/zoobst/gobi"
	"github.com/zoobst/gobi/parquetio"
)

// RawQuery is the T1 entry point: submit sql to Athena, poll for
// completion, download the result Parquet file from S3 via
// parquetio.ReadReader, and return the resulting Frame wrapped as a
// LazyFrame with no partition claim. QueryStats for the run is
// stashed and retrievable via StatsFor(lf).
//
// User owns the SQL — no wrapping, no partition spec, no CTAS. For
// partition-aware writes use UnloadAndRead (T3, ships separately).
//
// Athena returns SELECT results as a single Parquet result file
// under <ResultLocation>/<query-id>.csv or .parquet depending on
// workgroup config. This T1 slice assumes Parquet output — set the
// workgroup's output format accordingly, or use RawUnload for
// explicit format control.
func (c *Client) RawQuery(ctx context.Context, sql string) (*gobi.LazyFrame, error) {
	start := time.Now()

	queryID, err := c.submit(ctx, sql)
	if err != nil {
		return nil, fmt.Errorf("athenaio: submit: %w", err)
	}

	exec, err := c.pollUntilDone(ctx, queryID)
	if err != nil {
		return nil, err
	}

	// Result file location. Athena reports it explicitly on the
	// completed query — no need to parse ResultLocation + query-id
	// manually.
	if exec.ResultConfiguration == nil || exec.ResultConfiguration.OutputLocation == nil {
		return nil, fmt.Errorf("athenaio: query %s completed with no OutputLocation", queryID)
	}
	outputURI := *exec.ResultConfiguration.OutputLocation

	bucket, key, err := parseS3URI(outputURI)
	if err != nil {
		return nil, fmt.Errorf("athenaio: query %s: %w", queryID, err)
	}

	ra, size, err := newS3ReaderAt(ctx, c.s3, bucket, key)
	if err != nil {
		return nil, fmt.Errorf("athenaio: query %s: %w", queryID, err)
	}
	frame, err := parquetio.ReadReader(ra, size, nil)
	if err != nil {
		return nil, fmt.Errorf("athenaio: query %s: read result: %w", queryID, err)
	}

	stats := QueryStats{
		QueryExecutionID: queryID,
		ResultPrefix:     outputURI,
		ScannedBytes:     scannedBytes(exec),
		EngineTime:       engineTime(exec),
		TotalTime:        time.Since(start),
	}
	lf := frame.Lazy()
	registerStats(lf, stats)
	return lf, nil
}

// submit runs StartQueryExecution against the Client's default
// ResultLocation. Used for non-CTAS query paths (RawQuery prepass,
// GetQueryResults-style manifest reads) that don't need a per-query
// override.
func (c *Client) submit(ctx context.Context, sql string) (string, error) {
	return c.submitTo(ctx, sql, c.cfg.ResultLocation)
}

// submitTo runs StartQueryExecution with a caller-supplied
// ResultConfiguration.OutputLocation. CTAS paths use this to steer
// data files to their per-query external prefix — that hint replaces
// the CTAS WITH-clause `external_location` / `location` property
// dropped in v0.1.2. Workgroups with EnforceWorkGroupConfiguration=true
// may still override this value; callers handle the fallout via
// resolveActualLocation.
func (c *Client) submitTo(ctx context.Context, sql, outputLocation string) (string, error) {
	if outputLocation == "" {
		outputLocation = c.cfg.ResultLocation
	}
	in := &athena.StartQueryExecutionInput{
		QueryString: aws.String(sql),
		WorkGroup:   aws.String(c.cfg.Workgroup),
		ResultConfiguration: &athenatypes.ResultConfiguration{
			OutputLocation: aws.String(outputLocation),
		},
	}
	if c.cfg.Database != "" {
		in.QueryExecutionContext = &athenatypes.QueryExecutionContext{
			Database: aws.String(c.cfg.Database),
		}
	}
	out, err := c.athena.StartQueryExecution(ctx, in)
	if err != nil {
		return "", err
	}
	if out.QueryExecutionId == nil {
		return "", fmt.Errorf("athenaio: StartQueryExecution returned nil QueryExecutionId")
	}
	return *out.QueryExecutionId, nil
}

// pollUntilDone loops GetQueryExecution until the state is terminal,
// ctx cancels, or MaxPollDuration elapses. Uses exponential backoff
// starting at PollInterval, capped at defaultPollMax.
func (c *Client) pollUntilDone(ctx context.Context, queryID string) (*athenatypes.QueryExecution, error) {
	interval := c.cfg.PollInterval
	if interval <= 0 {
		interval = defaultPollInterval
	}
	deadline := time.Time{}
	if c.cfg.MaxPollDuration > 0 {
		deadline = time.Now().Add(c.cfg.MaxPollDuration)
	}

	for {
		out, err := c.athena.GetQueryExecution(ctx, &athena.GetQueryExecutionInput{
			QueryExecutionId: aws.String(queryID),
		})
		if err != nil {
			return nil, fmt.Errorf("athenaio: GetQueryExecution %s: %w", queryID, err)
		}
		if out.QueryExecution == nil {
			return nil, fmt.Errorf("athenaio: GetQueryExecution %s: nil QueryExecution", queryID)
		}
		var state athenatypes.QueryExecutionState
		if out.QueryExecution.Status != nil {
			state = out.QueryExecution.Status.State
		}
		done, ok := queryStateTerminal(state)
		if done {
			if !ok {
				reason := ""
				if out.QueryExecution.Status != nil && out.QueryExecution.Status.StateChangeReason != nil {
					reason = *out.QueryExecution.Status.StateChangeReason
				}
				return nil, fmt.Errorf("%w: %s: state=%s reason=%s",
					ErrQueryFailed, queryID, state, reason)
			}
			return out.QueryExecution, nil
		}
		// Not terminal — sleep + backoff.
		if !deadline.IsZero() && time.Now().After(deadline) {
			return nil, fmt.Errorf("%w: %s after %s (last state=%s)",
				ErrQueryTimeout, queryID, c.cfg.MaxPollDuration, state)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(interval):
		}
		if interval < defaultPollMax {
			interval *= 2
			if interval > defaultPollMax {
				interval = defaultPollMax
			}
		}
	}
}

// scannedBytes pulls DataScannedInBytes off a completed query. Zero
// if the field is unset (some Athena workgroup configs suppress
// statistics — unclear when, so guard).
func scannedBytes(exec *athenatypes.QueryExecution) int64 {
	if exec.Statistics == nil || exec.Statistics.DataScannedInBytes == nil {
		return 0
	}
	return *exec.Statistics.DataScannedInBytes
}

// engineTime pulls EngineExecutionTimeInMillis and converts to a
// time.Duration. Zero when the field is unset.
func engineTime(exec *athenatypes.QueryExecution) time.Duration {
	if exec.Statistics == nil || exec.Statistics.EngineExecutionTimeInMillis == nil {
		return 0
	}
	return time.Duration(*exec.Statistics.EngineExecutionTimeInMillis) * time.Millisecond
}

// -----------------------------------------------------------------------------
// QueryStats registry — associates a LazyFrame with the query stats
// produced by the RawQuery / UnloadAndRead call that created it.
// Retrieved via StatsFor(lf).
//
// Implementation: a package-scope map keyed by the LazyFrame's raw
// address (uintptr, not *gobi.LazyFrame). Using the pointer as a Go
// map key would keep every athenaio-produced LazyFrame — and
// transitively its entire source Frame, arrow columns, and buffers —
// alive for the process's lifetime, since the map holds a strong
// reference to any pointer-typed key. That was the multi-GB reader
// leak surfacing on long-lived clients that ran many UnloadAndRead
// calls.
//
// uintptr keys are opaque integers as far as the garbage collector
// is concerned, so they don't pin lf. A `runtime.AddCleanup` on lf
// removes its map entry when the caller drops the LazyFrame,
// making lf and its source Frame eligible for collection.
//
// ClearStats remains available for callers that want deterministic
// cleanup before GC gets to it.
// -----------------------------------------------------------------------------

var (
	statsMu       sync.RWMutex
	statsRegistry = map[uintptr]QueryStats{}
)

// registerStats stashes stats for lf. The map entry auto-drops when
// lf becomes unreachable — the map key is a raw address (no GC
// reference), and runtime.AddCleanup fires the delete when GC finds
// lf otherwise unreferenced.
func registerStats(lf *gobi.LazyFrame, stats QueryStats) {
	key := uintptr(unsafe.Pointer(lf))
	statsMu.Lock()
	statsRegistry[key] = stats
	statsMu.Unlock()
	runtime.AddCleanup(lf, func(k uintptr) {
		statsMu.Lock()
		delete(statsRegistry, k)
		statsMu.Unlock()
	}, key)
}

// StatsFor returns the QueryStats associated with lf, or the zero
// value + false if no stats are known (lf wasn't produced by an
// athenaio call, or ClearStats was invoked).
func StatsFor(lf *gobi.LazyFrame) (QueryStats, bool) {
	key := uintptr(unsafe.Pointer(lf))
	statsMu.RLock()
	stats, ok := statsRegistry[key]
	statsMu.RUnlock()
	return stats, ok
}

// ClearStats drops the QueryStats entry for lf. Optional — the
// runtime cleanup attached in registerStats will do the same once
// GC finds lf unreachable. Callers that want the map entry gone
// deterministically (before GC runs) can invoke this directly.
func ClearStats(lf *gobi.LazyFrame) {
	key := uintptr(unsafe.Pointer(lf))
	statsMu.Lock()
	delete(statsRegistry, key)
	statsMu.Unlock()
}
