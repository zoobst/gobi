package gobi

import (
	"context"
	"io"
	"math"
	"sync"

	"github.com/apache/arrow-go/v18/arrow"
)

// partitionMsg carries one batch's rows to one worker. The reader
// retains the batch once per recipient before sending; the worker
// Releases when it's done processing the message.
//
// rows is a pointer (*[]int, not []int) so the pool-recycled header
// travels through the channel without allocating. sync.Pool.Put on
// a bare []int would box the 3-word header into an interface — a
// per-Put heap allocation. Storing pointers, and threading the same
// pointer Get → dispatch → worker → Put, amortizes header
// allocation across the pool's steady-state cap (workers × chanBuf
// pointers total). Without this, the reader emits ~500K messages
// per 1BRC-scale query — 500K × workers header allocations, each
// putting pressure on GC.
type partitionMsg struct {
	batch arrow.RecordBatch
	rows  *[]int // pool-managed; worker returns to pool post-consume
}

// buildParallel is the partitioned build path for streamingAggregateExec.
// Called from buildIfNeeded when e.workers > 1.
//
// Model:
//
//	one reader goroutine (this func) pulls batches from e.input,
//	computes composite key + hash for each row, partitions rows into
//	e.workers buckets by hash mod N, and dispatches (batch, rows,
//	keys) messages to the workers that own rows. Each worker keeps
//	its own groupTable. Because rows are partitioned by hash of the
//	same key encoding the tables index on, no key ever lands in more
//	than one worker's table — buildResultBatch combines them with no
//	value-level combine step.
//
// Retain/release: the reader is the batch's owner. For each fan-out
// to K recipients, the reader Retains the batch (K-1) additional
// times so every worker's Release balances a live ref. The reader's
// own ref is released after all K sends complete.
//
// Errors + cancellation: the first worker error cancels the derived
// context so the reader stops pulling upstream. Ctx cancellation
// from above (Collect abort) propagates the same way. Workers drain
// their inboxes on cancellation to unblock the reader.
func (e *streamingAggregateExec) buildParallel(ctx context.Context) error {
	workers := e.workers
	if workers < 2 {
		return e.buildSerial(ctx)
	}

	// Small buffer per worker: enough to keep workers warm across
	// small batch-size hiccups without letting the reader outrun
	// them and blow up memory.
	const chanBuf = 4
	inboxes := make([]chan partitionMsg, workers)
	for i := range inboxes {
		inboxes[i] = make(chan partitionMsg, chanBuf)
	}

	wctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var firstErr error
	var errOnce sync.Once
	setErr := func(err error) {
		errOnce.Do(func() {
			firstErr = err
			cancel()
		})
	}

	// One group table per worker; buildResultBatch combines them.
	tables := make([]*groupTable, workers)
	for i := range tables {
		t, err := newGroupTable(e.keyMode, e.keys, e.aggs, e.input.Schema())
		if err != nil {
			for _, t := range tables[:i] {
				t.release()
			}
			return err
		}
		tables[i] = t
	}
	e.tables = tables

	var wg sync.WaitGroup
	wg.Add(workers)
	for i := range workers {
		go func() {
			defer wg.Done()
			for msg := range inboxes[i] {
				// If the pipeline was cancelled we still need to drain
				// remaining messages to unblock the reader; skip work.
				if wctx.Err() == nil {
					if err := tables[i].consume(msg.batch, *msg.rows); err != nil {
						setErr(err)
					}
				}
				msg.batch.Release()
				// Return the *[]int payload to the dispatcher's pool,
				// truncated so the next dispatchBatch reuses its
				// capacity. The same pointer flows Get → dispatch →
				// channel → here → Put, so Put allocates no header —
				// see rowsPool docs.
				*msg.rows = (*msg.rows)[:0]
				e.rowsPool.Put(msg.rows)
			}
			if wctx.Err() == nil {
				tables[i].finish(e.unordered, len(tables) > 1)
			}
		}()
	}

	// Reader loop.
	cols := conformCols(e.keys, e.aggs)
	readErr := func() error {
		defer func() {
			for _, ch := range inboxes {
				close(ch)
			}
		}()
		for {
			if err := wctx.Err(); err != nil {
				return err
			}
			batch, err := e.input.Next(wctx)
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return err
			}
			if batch.NumRows() == 0 {
				batch.Release()
				continue
			}
			// Conform before hashing: a drifted key type would hash
			// (and group) apart from the planned one.
			conformed, err := conformBatch(batch, e.input.Schema(), cols)
			batch.Release()
			if err != nil {
				return err
			}
			err = e.dispatchBatch(wctx, conformed, inboxes)
			conformed.Release()
			if err != nil {
				return err
			}
		}
	}()
	if readErr != nil {
		setErr(readErr)
	}

	wg.Wait()

	if firstErr != nil {
		return firstErr
	}
	// A cancellation after the reader reached EOF sets no error, but
	// the workers stopped early: report it rather than a partial result.
	return ctx.Err()
}

// dispatchBatch partitions one batch's rows across worker inboxes by
// hash(compositeKey) mod N, retains the batch once per recipient,
// and sends the resulting messages. Returns early on ctx cancel.
//
// The reader uses e.dispatchScratch to build each row's composite
// key without allocating (single-goroutine reuse — no race). The
// hash is folded directly from the scratch bytes; no intermediate
// string materialization. Workers recompute keys off the batch on
// the receive side, so nothing key-shaped crosses the channel.
func (e *streamingAggregateExec) dispatchBatch(ctx context.Context, batch arrow.RecordBatch, inboxes []chan partitionMsg) error {
	frame, err := batchToFrame(batch)
	if err != nil {
		return err
	}

	// Resolve key columns from the batch schema once per batch.
	keySeries := make([]Series, len(e.keys))
	for i, k := range e.keys {
		s, err := frame.Column(k)
		if err != nil {
			return err
		}
		keySeries[i] = s
	}

	nRows := int(batch.NumRows())
	workers := len(inboxes)
	// Per-partition scratch, one *[]int per worker. Pointers pulled
	// from e.rowsPool — reused across batches once workers return
	// them post-consume. First-batch cost is a heap-allocated []int
	// header + backing array per worker at the guess capacity;
	// steady-state cost is a pool Get + a `*p = (*p)[:0]` truncate.
	//
	// The pool stores `*[]int`, not `[]int`. The pointer identity
	// flows Get → dispatch → channel → worker Update → Put back
	// unchanged, so no header re-allocation occurs on the hot path.
	// Passing bare `[]int` through `sync.Pool.Put` would box the
	// 3-word slice header into an `any` interface every Put, and
	// creating a fresh `*[]int` per Put (via `p := s; &p`) would
	// heap-allocate the header — both defeat the pool's purpose on
	// high-cardinality streaming workloads. See rowsPool docs.
	// e.rowsPool.New is set at exec construction (see compile.go)
	// so Get always returns a non-nil *[]int — no fallback branch
	// needed. First-batch pointers come out at the pool's default
	// initial cap (128); subsequent batches reuse whatever the
	// worker Put'd back after the last consume.
	_ = nRows // guess capacity is applied by the pool's New; keep for future dispatch tuning
	partRows := make([]*[]int, workers)
	for i := range workers {
		p := e.rowsPool.Get().(*[]int)
		*p = (*p)[:0]
		partRows[i] = p
	}

	switch e.keyMode {
	case keyModeString1:
		// Fast path: hash arrow string bytes directly, no scratch,
		// no tag byte. Uses the same fnvHashBytes function as the
		// composite path via an unsafe []byte view of the string —
		// zero-copy since strings and byte slices share the same
		// backing memory shape in Go's runtime.
		strArr, err := resolveStringArray(keySeries[0])
		if err != nil {
			return err
		}
		for row := range nRows {
			// A null routes as "" — see groupTable.nullGroup.
			s, _ := strArr.value(row)
			w := int(fnvHashString1(s) % uint64(workers))
			*partRows[w] = append(*partRows[w], row)
		}
	case keyModeInt641:
		// Fast path: widen arrow value to int64, splatter through
		// FNV so ints with a tiny value range still spread across
		// workers. (A raw `key mod N` would collide all consecutive
		// int keys onto the same worker on ordered data.) A null key
		// routes as MinInt64 — see groupTable.nullGroup.
		intArr, err := resolveIntArray(keySeries[0])
		if err != nil {
			return err
		}
		for row := range nRows {
			k, ok := intArr.value(row)
			if !ok {
				k = math.MinInt64
			}
			w := int(fnvHashInt64(k) % uint64(workers))
			*partRows[w] = append(*partRows[w], row)
		}
	default:
		for row := range nRows {
			scratch, err := composeCompositeKeyInto(e.dispatchScratch[:0], keySeries, row)
			if err != nil {
				return err
			}
			e.dispatchScratch = scratch
			w := int(fnvHashBytes(scratch) % uint64(workers))
			*partRows[w] = append(*partRows[w], row)
		}
	}

	// Send to workers that received at least one row. Retain the
	// batch once per recipient so each worker's Release balances.
	// Slices for empty partitions go straight back to the pool
	// rather than into the channel — the worker never sees them.
	// The same *[]int flows through: no per-call header allocation.
	for i := range workers {
		if len(*partRows[i]) == 0 {
			e.rowsPool.Put(partRows[i])
			continue
		}
		batch.Retain()
		msg := partitionMsg{batch: batch, rows: partRows[i]}
		select {
		case inboxes[i] <- msg:
		case <-ctx.Done():
			// Undo the retain we just did — no worker will receive
			// this message. Return the pool pointer ourselves since
			// the worker won't. Also return every remaining
			// partition's pointer — otherwise those slices' capacity
			// escapes the pool on cancellation and gets GC'd
			// unnecessarily.
			batch.Release()
			e.rowsPool.Put(partRows[i])
			for j := i + 1; j < workers; j++ {
				e.rowsPool.Put(partRows[j])
			}
			return ctx.Err()
		}
	}
	return nil
}

// FNV-1a constants (64-bit).
const (
	fnvOffset64 = 14695981039346656037
	fnvPrime64  = 1099511628211
)

// fnvHashBytes is a cheap zero-alloc byte-slice hash used to shard
// composite keys across parallel aggregate workers. FNV-1a inlined:
// fast, no allocation, good distribution for the arbitrary-byte key
// shapes composeCompositeKeyInto produces (mixed types with 0x1F
// separators). Hot path — called once per row on the reader.
func fnvHashBytes(b []byte) uint64 {
	h := uint64(fnvOffset64)
	for _, c := range b {
		h ^= uint64(c)
		h *= fnvPrime64
	}
	return h
}

// fnvHashString1 hashes a string via FNV-1a without going through a
// []byte conversion. Go's range-over-string yields runes, but
// indexing a string returns raw bytes, so a classical for-i loop
// hashes the underlying bytes directly — no allocation, same result
// as fnvHashBytes([]byte(s)).
func fnvHashString1(s string) uint64 {
	h := uint64(fnvOffset64)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= fnvPrime64
	}
	return h
}

// fnvHashInt64 hashes an int64 via FNV-1a on its 8 little-endian
// bytes. Used by dispatchBatch to shard the keyModeInt641 fast
// path — a raw `k % N` would clump consecutive keys on the same
// worker for ordered inputs (very common in time-series data), so
// we run the value through FNV first to splatter neighbors across
// workers.
func fnvHashInt64(k int64) uint64 {
	u := uint64(k)
	h := uint64(fnvOffset64)
	h ^= u & 0xff
	h *= fnvPrime64
	h ^= (u >> 8) & 0xff
	h *= fnvPrime64
	h ^= (u >> 16) & 0xff
	h *= fnvPrime64
	h ^= (u >> 24) & 0xff
	h *= fnvPrime64
	h ^= (u >> 32) & 0xff
	h *= fnvPrime64
	h ^= (u >> 40) & 0xff
	h *= fnvPrime64
	h ^= (u >> 48) & 0xff
	h *= fnvPrime64
	h ^= (u >> 56) & 0xff
	h *= fnvPrime64
	return h
}
