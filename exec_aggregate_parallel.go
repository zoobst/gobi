package gobi

import (
	"context"
	"io"
	"sort"
	"strings"
	"sync"

	"github.com/apache/arrow-go/v18/arrow"
)

// partitionMsg carries one batch's rows to one worker. The reader
// retains the batch once per recipient before sending; the worker
// Releases when it's done processing the message.
type partitionMsg struct {
	batch arrow.RecordBatch
	rows  []int // row indices in batch belonging to this worker
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
//	its own map[string]*aggGroup. Because rows are partitioned by
//	hash of the same key encoding used to index the map, no key ever
//	lands in more than one worker's map — the final merge is a
//	simple union with no value-level combine step.
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

	// Per-worker output — one flavor is populated per invocation
	// depending on keyMode. String maps for String1 + Composite,
	// int64 maps for Int641. The other stays nil so the merge step
	// can skip empty branches without extra bookkeeping.
	workerGroups := make([]map[string]*aggGroup, workers)
	workerOrder := make([][]string, workers)
	workerGroupsInt := make([]map[int64]*aggGroup, workers)
	workerOrderInt := make([][]int64, workers)

	var wg sync.WaitGroup
	wg.Add(workers)
	for i := range workers {
		i := i
		go func() {
			defer wg.Done()
			var groups map[string]*aggGroup
			var order []string
			var groupsInt map[int64]*aggGroup
			var orderInt []int64
			if e.keyMode == keyModeInt641 {
				groupsInt = make(map[int64]*aggGroup)
			} else {
				groups = make(map[string]*aggGroup)
			}
			// Worker-local scratch for composite key encoding.
			// Reused across every row this worker consumes so key
			// computation is zero-alloc except at first-touch inserts.
			var scratch []byte
			for msg := range inboxes[i] {
				// If the pipeline was cancelled we still need to drain
				// remaining messages to unblock the reader; skip work.
				if wctx.Err() == nil {
					if err := workerConsume(msg, e.keys, e.aggs, e.keyMode, groups, &order, groupsInt, &orderInt, &scratch); err != nil {
						setErr(err)
					}
				}
				msg.batch.Release()
			}
			workerGroups[i] = groups
			workerOrder[i] = order
			workerGroupsInt[i] = groupsInt
			workerOrderInt[i] = orderInt
		}()
	}

	// Reader loop.
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
			if err := e.dispatchBatch(wctx, batch, inboxes); err != nil {
				batch.Release()
				return err
			}
			batch.Release()
		}
	}()
	if readErr != nil {
		setErr(readErr)
	}

	wg.Wait()

	if firstErr != nil {
		return firstErr
	}

	// Merge: keys are disjoint across workers (hash-partitioned), so
	// union then sort. Only the container flavor matching keyMode
	// has entries; the other stays untouched.
	if e.keyMode == keyModeInt641 {
		total := 0
		for _, o := range workerOrderInt {
			total += len(o)
		}
		e.groupsInt64 = make(map[int64]*aggGroup, total)
		e.orderInt64 = make([]int64, 0, total)
		for i := range workerGroupsInt {
			for k, g := range workerGroupsInt[i] {
				e.groupsInt64[k] = g
			}
			e.orderInt64 = append(e.orderInt64, workerOrderInt[i]...)
		}
		sort.Slice(e.orderInt64, func(i, j int) bool { return e.orderInt64[i] < e.orderInt64[j] })
		return nil
	}
	total := 0
	for _, o := range workerOrder {
		total += len(o)
	}
	e.groups = make(map[string]*aggGroup, total)
	e.order = make([]string, 0, total)
	for i := range workerGroups {
		for k, g := range workerGroups[i] {
			e.groups[k] = g
		}
		e.order = append(e.order, workerOrder[i]...)
	}
	sort.Strings(e.order)
	return nil
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
	// Per-partition scratch, one entry per worker. Preallocate to
	// nRows/workers with slack so the first few appends don't grow.
	guess := nRows/workers + 8
	partRows := make([][]int, workers)
	for i := range workers {
		partRows[i] = make([]int, 0, guess)
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
		for row := 0; row < nRows; row++ {
			s := strArr.value(row)
			w := int(fnvHashString1(s) % uint64(workers))
			partRows[w] = append(partRows[w], row)
		}
	case keyModeInt641:
		// Fast path: widen arrow value to int64, splatter through
		// FNV so ints with a tiny value range still spread across
		// workers. (A raw `key mod N` would collide all consecutive
		// int keys onto the same worker on ordered data.) Nulls
		// aren't routed anywhere — workerConsume drops them for the
		// same reason the serial path does.
		intArr, err := resolveIntArray(keySeries[0])
		if err != nil {
			return err
		}
		for row := 0; row < nRows; row++ {
			k, ok := intArr.value(row)
			if !ok {
				continue
			}
			w := int(fnvHashInt64(k) % uint64(workers))
			partRows[w] = append(partRows[w], row)
		}
	default:
		for row := 0; row < nRows; row++ {
			scratch, err := composeCompositeKeyInto(e.dispatchScratch[:0], keySeries, row)
			if err != nil {
				return err
			}
			e.dispatchScratch = scratch
			w := int(fnvHashBytes(scratch) % uint64(workers))
			partRows[w] = append(partRows[w], row)
		}
	}

	// Send to workers that received at least one row. Retain the
	// batch once per recipient so each worker's Release balances.
	for i := range workers {
		if len(partRows[i]) == 0 {
			continue
		}
		batch.Retain()
		msg := partitionMsg{batch: batch, rows: partRows[i]}
		select {
		case inboxes[i] <- msg:
		case <-ctx.Done():
			// Undo the retain we just did — no worker will receive
			// this message.
			batch.Release()
			return ctx.Err()
		}
	}
	return nil
}

// workerConsume is the per-worker equivalent of consumeBatch: given
// a row slice assigned by the reader's hash partitioning, updates
// this worker's own groups map. Recomputes composite keys off the
// batch via the worker-local scratch buffer — nothing key-shaped
// crosses the channel from the reader.
//
// Same allocation profile as the serial consumeBatch: one
// string-copy per NEW group at first-touch, zero allocations per
// row on the probe path (compiler recognizes map[string(scratch)]).
func workerConsume(
	msg partitionMsg,
	keys []string,
	aggs []Aggregation,
	mode keyMode,
	// Container pair for string-keyed modes (String1 + Composite):
	groups map[string]*aggGroup,
	order *[]string,
	// Container pair for int-keyed mode (Int641):
	groupsInt map[int64]*aggGroup,
	orderInt *[]int64,
	scratch *[]byte,
) error {
	frame, err := batchToFrame(msg.batch)
	if err != nil {
		return err
	}
	keySeries := make([]Series, len(keys))
	for i, k := range keys {
		s, err := frame.Column(k)
		if err != nil {
			return err
		}
		keySeries[i] = s
	}
	aggCols := make([]Series, len(aggs))
	for i, a := range aggs {
		if a.Column == "" {
			continue
		}
		s, err := frame.Column(a.Column)
		if err != nil {
			return err
		}
		aggCols[i] = s
	}

	// Bucket rows by group pointer. See consumeBatch for the
	// rationale on why bucketing keys off *aggGroup instead of the
	// key. Three paths depending on keyMode. Only one of the two
	// container pairs (string vs int) is populated per invocation.
	buckets := make(map[*aggGroup][]int)
	switch mode {
	case keyModeString1:
		strArr, err := resolveStringArray(keySeries[0])
		if err != nil {
			return err
		}
		for _, row := range msg.rows {
			s := strArr.value(row)
			g, ok := groups[s]
			if !ok {
				ks := strings.Clone(s)
				g, err = newAggGroupString1(ks, aggs)
				if err != nil {
					return err
				}
				groups[ks] = g
				*order = append(*order, ks)
			}
			buckets[g] = append(buckets[g], row)
		}
	case keyModeInt641:
		intArr, err := resolveIntArray(keySeries[0])
		if err != nil {
			return err
		}
		for _, row := range msg.rows {
			k, ok := intArr.value(row)
			if !ok {
				// dispatchBatch also skips null-keyed rows, but a
				// second guard here is cheap defense in depth.
				continue
			}
			g, ok := groupsInt[k]
			if !ok {
				g, err = newAggGroup(keySeries, row, aggs)
				if err != nil {
					return err
				}
				groupsInt[k] = g
				*orderInt = append(*orderInt, k)
			}
			buckets[g] = append(buckets[g], row)
		}
	default:
		for _, row := range msg.rows {
			buf, err := composeCompositeKeyInto((*scratch)[:0], keySeries, row)
			if err != nil {
				return err
			}
			*scratch = buf
			g, ok := groups[string(buf)]
			if !ok {
				g, err = newAggGroup(keySeries, row, aggs)
				if err != nil {
					return err
				}
				ks := string(buf)
				groups[ks] = g
				*order = append(*order, ks)
			}
			buckets[g] = append(buckets[g], row)
		}
	}

	// Update accumulators one group at a time.
	for g, groupRows := range buckets {
		for i, a := range aggs {
			if a.Column == "" {
				if err := g.accs[i].Update(Series{}, groupRows); err != nil {
					return err
				}
				continue
			}
			if err := g.accs[i].Update(aggCols[i], groupRows); err != nil {
				return err
			}
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

