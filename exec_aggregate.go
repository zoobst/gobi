package gobi

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"sort"
	"strings"
	"sync"
	"unsafe"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/bitutil"
	"github.com/apache/arrow-go/v18/arrow/compute"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// streamingAggregateExec is a native streaming hash aggregate.
//
// Pulls its input one batch at a time into groupTables (dense group
// ids, per-aggregation state slabs — see groupTable). Rows are
// consumed as batches arrive; no accumulated input is held in memory
// beyond the group state. On EOF, emits one output row per group, in
// key order unless unordered.
//
// Handles the built-in Kinds and custom IncrementalAggregators.
// Plain custom Fn Aggregators require the whole input at once
// (their interface signature is Aggregate(Series, []int)) so the
// compiler routes those to the materializing fallback instead.
//
// No disk spilling — if the hash table doesn't fit in RAM, the
// process OOMs. That's the design.
type streamingAggregateExec struct {
	input     ExecOperator
	keys      []string
	aggs      []Aggregation
	outSchema *arrow.Schema

	// workers: number of partitioned aggregate workers. 1 = serial
	// build (single goroutine, single map — original behavior). >1 =
	// partitioned build (N goroutines, N disjoint maps hash-sharded
	// by composite key, merged into one sorted result on EOF). Set at
	// Compile time via resolveWorkers; runtime pins to this value so
	// Explain and Execute stay in sync.
	workers int

	// keyMode selects the key-encoding hot path. Set at Compile time
	// based on the number and types of key columns. See keyMode
	// constants for the specific fast paths.
	keyMode keyMode

	built bool
	// unordered skips sorting the output by key (LazyGroupBy.Unordered).
	unordered bool
	// tables holds the build's group state: one table for the serial
	// build, one per worker for the parallel build. buildResultBatch
	// consumes and releases them.
	tables []*groupTable

	// dispatchScratch is the parallel reader's per-row composite key
	// buffer. Only the reader goroutine touches it, so no
	// synchronization is needed. Workers recompute keys off the batch
	// with their own table's scratch, so nothing key-shaped crosses
	// the channel.
	dispatchScratch []byte

	// rowsPool recycles the []int payloads that dispatchBatch sends
	// through the worker inboxes as partitionMsg.rows. Every batch
	// the reader partitions rows into `workers` slices and hands
	// each to a worker; the worker returns its slice to this pool
	// after processing. Without pooling, 1BRC allocates ~5-8 GB of
	// []int garbage on this path (~500K dispatches × workers ×
	// per-row appends). The pool caps in-flight slices at
	// (workers × chanBuf) ≈ 44 for the default config.
	//
	// Stored as `*[]int` (not `[]int`) to avoid the boxing
	// allocation that `sync.Pool.Put(anySliceValue)` would incur:
	// passing a slice header through the untyped `any` interface
	// allocates a small object to hold the (ptr, len, cap) triple.
	// Storing pointers to slice headers dodges that allocation on
	// every Put — meaningful given this path runs hundreds of
	// thousands of times per full 1BRC-scale query.
	rowsPool sync.Pool
	// One-shot emit: buildIfNeeded produces the whole result batch,
	// Next hands it out, subsequent Next calls return io.EOF.
	resultBatch arrow.RecordBatch
	emitted     bool
}

// pickKeyMode inspects an aggregateNode's input schema and returns
// the specialized keyMode that applies, or keyModeComposite when no
// fast path matches. Called once at Compile time; the runtime
// dispatches to the specialized consume-loop branch based on the
// value stored on the exec.
//
// Fast paths currently recognized:
//   - Single String / LargeString key → keyModeString1
//   - Single Int64 / Int32 / Uint64 / Uint32 / Timestamp / Bool key
//     → keyModeInt641 (all fit losslessly in int64 for map-key
//     purposes; the group's emitted key value is reconstructed
//     back to the original arrow type at buildResultBatch time)
//
// More can be added (single float64) once the payoff is measured;
// each adds a specialized branch to groupTable.consume and
// dispatchBatch.
func pickKeyMode(n *aggregateNode) keyMode {
	if len(n.keys) != 1 {
		return keyModeComposite
	}
	inSchema := n.input.Schema()
	fs, ok := inSchema.FieldsByName(n.keys[0])
	if !ok || len(fs) == 0 {
		return keyModeComposite
	}
	switch fs[0].Type.ID() {
	case arrow.STRING, arrow.LARGE_STRING:
		return keyModeString1
	case arrow.INT64, arrow.INT32, arrow.UINT64, arrow.UINT32,
		arrow.TIMESTAMP, arrow.BOOL:
		return keyModeInt641
	}
	return keyModeComposite
}

// keyMode selects the key-encoding hot path in the streaming
// aggregate. Chosen once at Compile time based on the number and
// types of key columns; runtime dispatches to the specialized
// consume-loop branch.
//
// The fast paths exist because the generic composite encoding
// (keyOfAppend into a scratch []byte, then map[string(scratch)])
// costs a per-row memcpy of the raw key bytes plus a tag byte,
// and shows up as ~14s cumulative on the 1BRC CPU profile. The
// single-column fast paths skip both by reading the arrow value
// once and using it directly as the map key or its bit-pattern.
type keyMode uint8

const (
	// keyModeComposite is the default path — supports any number of
	// key columns of any hashable type. Uses composeCompositeKeyInto
	// with a per-worker scratch buffer.
	keyModeComposite keyMode = iota
	// keyModeString1 is the specialized single-column string path.
	// Handles a single String or LargeString key. The arrow value
	// is used directly as the groupTable.strIdx key (probed with a
	// string aliasing the batch, no copy; insert copies it into the
	// table's arena).
	keyModeString1
	// keyModeInt641 is the specialized single-column integer-ish
	// path. Handles a single Int64, Int32, Uint64, Uint32,
	// Timestamp, or Bool key — every one fits losslessly in an
	// int64 for hash-table purposes. Skips the composite byte
	// encoding entirely: the arrow value is read once per row and
	// used directly as the groupTable.intIdx key. Faster hash
	// (word-at-a-time vs byte-loop) and no scratch allocation.
	keyModeInt641
)

// aggAccumulator is the streaming counterpart to the built-in Aggregation
// Kinds. Update() consumes a batch's rows for one aggregation column;
// Finalize() produces the group's aggregated value.
type aggAccumulator interface {
	// Update accepts a full column and the row indices within it
	// that belong to the current group. Called at most once per
	// input batch per group.
	//
	// Contract: implementations MUST NOT retain the `rows` slice
	// past the Update call. The caller reuses the underlying
	// backing array (the table's aggBatch) across batches — a
	// retained alias would see it overwritten with the next
	// batch's row indices, silently corrupting downstream reads.
	// Copy any subset you need to hold onto (rare — the built-in
	// accumulators all consume rows synchronously).
	//
	// Implementations MAY retain values extracted FROM the column
	// (e.g., firstLastAcc keeps the first non-null scalar), but
	// arrow buffer aliases (like strings via arr.Value(i)) must
	// be cloned since the batch's arrow arrays are released after
	// Update returns. See nUniqueAcc.updateString for the pattern.
	Update(col Series, rows []int) error
	// Finalize returns the aggregated value as any. Nil signals a
	// null output for this group (empty groups on numeric aggs).
	Finalize() any
	// OutputType is the arrow type the finalized value maps to.
	OutputType() arrow.DataType
}

func (e *streamingAggregateExec) Schema() *arrow.Schema { return e.outSchema }
func (e *streamingAggregateExec) Close() error          { return e.input.Close() }

func (e *streamingAggregateExec) Next(ctx context.Context) (arrow.RecordBatch, error) {
	if err := e.buildIfNeeded(ctx); err != nil {
		return nil, err
	}
	if e.emitted {
		return nil, io.EOF
	}
	e.emitted = true
	// resultBatch's ref goes to the caller.
	return e.resultBatch, nil
}

// buildIfNeeded consumes the entire input stream, building groups.
// After the first call, subsequent calls are a no-op.
//
// Dispatches to the serial or partitioned parallel path based on
// e.workers. workers == 1 (or unset) → single-goroutine serial build
// with the original single-map behavior. workers > 1 → partitioned
// build across N goroutines with hash-sharded per-worker maps, then
// a union merge (see exec_aggregate_parallel.go).
func (e *streamingAggregateExec) buildIfNeeded(ctx context.Context) error {
	if e.built {
		return nil
	}
	e.built = true

	build := e.buildSerial
	if e.workers > 1 {
		build = e.buildParallel
	}
	if err := build(ctx); err != nil {
		// buildResultBatch won't run: free the captured arrays here.
		for _, t := range e.tables {
			t.release()
		}
		e.tables = nil
		return err
	}
	rb, err := e.buildResultBatch()
	if err != nil {
		return err
	}
	e.resultBatch = rb
	return nil
}

// buildSerial folds every batch into one groupTable. Used for
// workers <= 1 — the partitioned path pays for goroutine + channel +
// hash overhead that isn't worth it without CPU parallelism to unlock.
func (e *streamingAggregateExec) buildSerial(ctx context.Context) error {
	t, err := newGroupTable(e.keyMode, e.keys, e.aggs, e.outSchema)
	if err != nil {
		return err
	}
	e.tables = []*groupTable{t}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		batch, err := e.input.Next(ctx)
		if err == io.EOF {
			t.finish(e.unordered, false)
			return nil
		}
		if err != nil {
			return err
		}
		err = t.consume(batch, nil)
		batch.Release()
		if err != nil {
			return err
		}
	}
}

// composeCompositeKeyInto appends the byte-encoded composite key
// for row into dst and returns the resulting slice. Callers pass
// scratch[:0] to reuse a buffer across rows — the streaming
// aggregate hot paths thread one scratch buffer through every row
// to avoid per-row allocation.
func composeCompositeKeyInto(dst []byte, keys []Series, row int) ([]byte, error) {
	for i, s := range keys {
		if i > 0 {
			dst = append(dst, 0x1F)
		}
		var err error
		dst, err = keyOfAppend(dst, s, row)
		if err != nil {
			return nil, err
		}
	}
	return dst, nil
}

// stringArrayView is the small interface the keyModeString1 fast
// path needs from a Series: value-by-row lookup that returns a
// zero-copy Go string aliased to the arrow buffer. Both
// *array.String and *array.LargeString satisfy this via their
// (unexported) shims defined below.
type stringArrayView interface {
	value(row int) string
}

// resolveStringArray extracts a stringArrayView from a Series known
// (by Compile-time keyMode selection) to be String or LargeString.
// Panics-safe: if the series has multiple chunks, they're spliced
// into a single stringArrayView that dispatches per-row. The 1BRC
// fixture is single-chunk per batch so the fast-single-chunk path
// dominates.
func resolveStringArray(s Series) (stringArrayView, error) {
	chunks := s.col.Data().Chunks()
	if len(chunks) == 1 {
		switch a := chunks[0].(type) {
		case *array.String:
			return stringArrView{a: a}, nil
		case *array.LargeString:
			return largeStringArrView{a: a}, nil
		}
		return nil, fmt.Errorf("resolveStringArray: chunk type %T not a string array", chunks[0])
	}
	// Multi-chunk: build a per-row dispatcher that scans chunk
	// offsets. Cheap struct; the extra branch is worth avoiding
	// full composite-key fallback for the multi-chunk case.
	view := &chunkedStringView{chunks: make([]stringArrayView, len(chunks)), lens: make([]int, len(chunks))}
	for i, c := range chunks {
		switch a := c.(type) {
		case *array.String:
			view.chunks[i] = stringArrView{a: a}
		case *array.LargeString:
			view.chunks[i] = largeStringArrView{a: a}
		default:
			return nil, fmt.Errorf("resolveStringArray: chunk[%d] type %T not a string array", i, c)
		}
		view.lens[i] = c.Len()
	}
	return view, nil
}

type stringArrView struct{ a *array.String }

func (v stringArrView) value(row int) string { return v.a.Value(row) }

type largeStringArrView struct{ a *array.LargeString }

func (v largeStringArrView) value(row int) string { return v.a.Value(row) }

type chunkedStringView struct {
	chunks []stringArrayView
	lens   []int
}

func (v *chunkedStringView) value(row int) string {
	offset := 0
	for i, l := range v.lens {
		if row < offset+l {
			return v.chunks[i].value(row - offset)
		}
		offset += l
	}
	return ""
}

// intArrayView is the small interface the keyModeInt641 fast path
// needs from a Series: an int64-shaped value-by-row lookup + a
// null probe. Every arrow type that pickKeyMode routes to
// keyModeInt641 (Int64, Int32, Uint64, Uint32, Timestamp, Bool)
// has a lossless-widening view here — the arrow type is
// remembered separately (via the source Series' Field) so the
// group can emit the original type at result-emit time.
type intArrayView interface {
	value(row int) (int64, bool) // v, ok=false if null
}

// resolveIntArray extracts an intArrayView from a Series known
// (by Compile-time keyMode selection) to be an int-ish type.
// Handles Int64/Int32/Uint64/Uint32/Timestamp/Bool. Multi-chunk
// series get a per-row dispatcher, matching the string variant.
func resolveIntArray(s Series) (intArrayView, error) {
	chunks := s.col.Data().Chunks()
	if len(chunks) == 1 {
		return intViewFor(chunks[0])
	}
	view := &chunkedIntView{chunks: make([]intArrayView, len(chunks)), lens: make([]int, len(chunks))}
	for i, c := range chunks {
		v, err := intViewFor(c)
		if err != nil {
			return nil, fmt.Errorf("resolveIntArray: chunk[%d]: %w", i, err)
		}
		view.chunks[i] = v
		view.lens[i] = c.Len()
	}
	return view, nil
}

// intViewFor returns the per-type single-chunk implementation. Each
// concrete view widens its native arrow value to int64 losslessly.
func intViewFor(chunk arrow.Array) (intArrayView, error) {
	switch a := chunk.(type) {
	case *array.Int64:
		return intArrViewI64{a: a}, nil
	case *array.Int32:
		return intArrViewI32{a: a}, nil
	case *array.Uint64:
		return intArrViewU64{a: a}, nil
	case *array.Uint32:
		return intArrViewU32{a: a}, nil
	case *array.Timestamp:
		return intArrViewTS{a: a}, nil
	case *array.Boolean:
		return intArrViewBool{a: a}, nil
	}
	return nil, fmt.Errorf("intViewFor: chunk type %T not int-ish", chunk)
}

type intArrViewI64 struct{ a *array.Int64 }

func (v intArrViewI64) value(row int) (int64, bool) {
	if v.a.IsNull(row) {
		return 0, false
	}
	return v.a.Value(row), true
}

type intArrViewI32 struct{ a *array.Int32 }

func (v intArrViewI32) value(row int) (int64, bool) {
	if v.a.IsNull(row) {
		return 0, false
	}
	return int64(v.a.Value(row)), true
}

type intArrViewU64 struct{ a *array.Uint64 }

func (v intArrViewU64) value(row int) (int64, bool) {
	if v.a.IsNull(row) {
		return 0, false
	}
	// Preserve bits — the group only cares about identity, not sign.
	// Emit-time reconstruction (newAggGroupInt641) casts back via the
	// Series' declared arrow type.
	return int64(v.a.Value(row)), true
}

type intArrViewU32 struct{ a *array.Uint32 }

func (v intArrViewU32) value(row int) (int64, bool) {
	if v.a.IsNull(row) {
		return 0, false
	}
	return int64(v.a.Value(row)), true
}

type intArrViewTS struct{ a *array.Timestamp }

func (v intArrViewTS) value(row int) (int64, bool) {
	if v.a.IsNull(row) {
		return 0, false
	}
	return int64(v.a.Value(row)), true
}

type intArrViewBool struct{ a *array.Boolean }

func (v intArrViewBool) value(row int) (int64, bool) {
	if v.a.IsNull(row) {
		return 0, false
	}
	if v.a.Value(row) {
		return 1, true
	}
	return 0, true
}

type chunkedIntView struct {
	chunks []intArrayView
	lens   []int
}

func (v *chunkedIntView) value(row int) (int64, bool) {
	offset := 0
	for i, l := range v.lens {
		if row < offset+l {
			return v.chunks[i].value(row - offset)
		}
		offset += l
	}
	return 0, false
}

// newAccumulator constructs the accumulator for one Aggregation kind.
// Custom Fn accumulators route through this path when they implement
// IncrementalAggregator — a fresh clone is instantiated per group.
// Plain Aggregator-only custom Fns are still rejected here; the
// compiler routes them to the materializing fallback via
// allBuiltInAggs.
func newAccumulator(a Aggregation) (aggAccumulator, error) {
	if a.Fn != nil {
		inc, ok := a.Fn.(IncrementalAggregator)
		if !ok {
			return nil, fmt.Errorf("gobi: streaming aggregate does not support custom Aggregator Fn (implement IncrementalAggregator for streaming support)")
		}
		return &customIncrementalAcc{inner: inc.Clone(), outType: a.Fn.Type()}, nil
	}
	switch a.Kind {
	case AggCount:
		return &countAcc{starCount: a.Column == ""}, nil
	case AggSum:
		return &sumAcc{}, nil
	case AggMean:
		return &meanAcc{}, nil
	case AggMin:
		return &minMaxAcc{isMin: true, extreme: math.Inf(+1)}, nil
	case AggMax:
		return &minMaxAcc{isMin: false, extreme: math.Inf(-1)}, nil
	case AggFirst:
		return &firstLastAcc{keepFirst: true}, nil
	case AggLast:
		return &firstLastAcc{keepFirst: false}, nil
	case AggStd:
		return &stdVarAcc{wantStd: true}, nil
	case AggVar:
		return &stdVarAcc{wantStd: false}, nil
	case AggNUnique:
		// Maps are lazy-initialized on first Update after kind
		// selection — the zero value is a valid accumulator.
		return &nUniqueAcc{}, nil
	case AggMedian:
		return &medianAcc{}, nil
	case AggMode:
		return &modeAcc{counts: make(map[string]int64)}, nil
	case AggBitOr, AggBitAnd, AggBitXor:
		return newBitAcc(a.Kind), nil
	}
	return nil, fmt.Errorf("gobi: streaming aggregate: unknown Kind %d", a.Kind)
}

// customIncrementalAcc bridges a user-supplied IncrementalAggregator
// to the streaming executor's aggAccumulator interface. Each group
// owns its own inner instance (produced by IncrementalAggregator.Clone
// at group first-touch), so per-group state stays isolated.
type customIncrementalAcc struct {
	inner   IncrementalAggregator
	outType arrow.DataType
}

func (a *customIncrementalAcc) Update(col Series, rows []int) error {
	return a.inner.Update(col, rows)
}
func (a *customIncrementalAcc) Finalize() any              { return a.inner.Finalize() }
func (a *customIncrementalAcc) OutputType() arrow.DataType { return a.outType }

// -----------------------------------------------------------------------------
// groupTable: columnar hash-aggregation state
//
// A group is a dense id (gid), assigned in first-seen order. The key
// index maps each key to its gid, and every aggregation keeps its
// state in slices indexed by gid (sums in a []float64, first-value
// slots in a []int32, …), so a new group costs no allocation beyond
// amortized slice and map growth. Key bytes are copied into an arena
// for the same reason. Key columns are captured with one vectorized
// take per batch over the rows that opened groups, and emitted with
// one more take in output order.
//
// The serial build uses one table; the parallel build one per worker
// (keys are hash-partitioned, so tables never share a key).
// -----------------------------------------------------------------------------

type groupTable struct {
	mode keyMode
	keys []string
	aggs []Aggregation

	strIdx  map[string]int32 // keyModeString1 + keyModeComposite
	intIdx  map[int64]int32  // keyModeInt641
	sortStr []string         // per gid: the key (String1) or its encoding (Composite)
	sortInt []int64          // per gid: the widened key (Int641)
	arena   strArena
	keyCaps []valueCapture // per key column; unused for keyModeString1
	states  []aggState
	n       int32   // groups so far
	order   []int32 // gids in output order, set by finish
	// The keys of order, laid out in that order, for merging the
	// tables' sorted runs (set by finish when the table is merged).
	runStr []string
	runInt []int64

	// Per-batch scratch, reused across batches.
	scratch []byte
	batch   aggBatch
	newRows []int // rows that opened a group in the current batch
	allRows []int // 0..n-1, for the serial build
}

// newGroupTable builds an empty table for output schema out: the key
// columns, then one column per aggregation, with their planned types.
func newGroupTable(mode keyMode, keys []string, aggs []Aggregation, out *arrow.Schema) (*groupTable, error) {
	t := &groupTable{mode: mode, keys: keys, aggs: aggs}
	if mode == keyModeInt641 {
		t.intIdx = make(map[int64]int32)
	} else {
		t.strIdx = make(map[string]int32)
	}
	if mode != keyModeString1 {
		t.keyCaps = make([]valueCapture, len(keys))
		for i := range t.keyCaps {
			t.keyCaps[i].dt = out.Field(i).Type
		}
	}
	t.states = make([]aggState, len(aggs))
	for i, a := range aggs {
		st, err := newAggState(a, out.Field(len(keys)+i).Type)
		if err != nil {
			t.release()
			return nil, err
		}
		t.states[i] = st
	}
	return t, nil
}

// release frees the Arrow arrays the table captured.
func (t *groupTable) release() {
	for i := range t.keyCaps {
		t.keyCaps[i].release()
	}
	for _, st := range t.states {
		if st != nil {
			st.release()
		}
	}
}

// consume folds batch into the table. rows selects the batch rows to
// fold (the parallel build's partition); nil means every row.
func (t *groupTable) consume(batch arrow.RecordBatch, rows []int) error {
	nb := int(batch.NumRows())
	if nb == 0 {
		return nil
	}
	if rows == nil {
		for len(t.allRows) < nb {
			t.allRows = append(t.allRows, len(t.allRows))
		}
		rows = t.allRows[:nb]
	}
	frame, err := batchToFrame(batch)
	if err != nil {
		return err
	}
	keySeries := make([]Series, len(t.keys))
	for i, k := range t.keys {
		if keySeries[i], err = frame.Column(k); err != nil {
			return err
		}
	}
	aggCols := make([]Series, len(t.aggs))
	for i, a := range t.aggs {
		if a.Column == "" {
			continue // count(*): no source column
		}
		if aggCols[i], err = frame.Column(a.Column); err != nil {
			return err
		}
	}

	b := &t.batch
	b.reset()
	t.newRows = t.newRows[:0]
	switch t.mode {
	case keyModeString1:
		strArr, err := resolveStringArray(keySeries[0])
		if err != nil {
			return err
		}
		for _, row := range rows {
			s := strArr.value(row) // aliases the batch; cloned on insert
			g, ok := t.strIdx[s]
			if !ok {
				g = t.n
				t.n++
				ks := t.arena.string(s)
				t.strIdx[ks] = g
				t.sortStr = appendDoubling(t.sortStr, ks)
			}
			b.rows = append(b.rows, row)
			b.gids = append(b.gids, g)
		}
	case keyModeInt641:
		intArr, err := resolveIntArray(keySeries[0])
		if err != nil {
			return err
		}
		for _, row := range rows {
			k, ok := intArr.value(row)
			if !ok {
				// Null keys are dropped in this mode: an int64 map key
				// can't represent null without colliding with a value.
				continue
			}
			g, ok := t.intIdx[k]
			if !ok {
				g = t.n
				t.n++
				t.intIdx[k] = g
				t.sortInt = appendDoubling(t.sortInt, k)
				t.newRows = append(t.newRows, row)
			}
			b.rows = append(b.rows, row)
			b.gids = append(b.gids, g)
		}
	default:
		for _, row := range rows {
			buf, err := composeCompositeKeyInto(t.scratch[:0], keySeries, row)
			if err != nil {
				return err
			}
			t.scratch = buf
			g, ok := t.strIdx[string(buf)]
			if !ok {
				g = t.n
				t.n++
				ks := t.arena.bytes(buf)
				t.strIdx[ks] = g
				t.sortStr = appendDoubling(t.sortStr, ks)
				t.newRows = append(t.newRows, row)
			}
			b.rows = append(b.rows, row)
			b.gids = append(b.gids, g)
		}
	}
	b.groups = int(t.n)

	// Capture the key values of the groups this batch opened. Groups
	// open in gid order, so capture slot == gid.
	if len(t.newRows) > 0 {
		for i := range t.keyCaps {
			if _, err := t.keyCaps[i].capture(keySeries[i], t.newRows); err != nil {
				return fmt.Errorf("key %s: %w", t.keys[i], err)
			}
		}
	}
	for i, st := range t.states {
		if err := st.grow(int(t.n)); err != nil {
			return err
		}
		if err := st.update(aggCols[i], b); err != nil {
			return fmt.Errorf("gobi: aggregation %s: %w", aggName(t.aggs[i]), err)
		}
	}
	return nil
}

// finish sorts the table's groups into output order: by key — the
// String1 string, the composite encoding, or the widened int64 — or
// first-seen order when unordered. The parallel build calls it on
// each worker, so the tables sort concurrently. merged says outputOrder
// will merge this table with others, which needs the keys copied out
// in sorted order (runInt / runStr); a lone table skips that copy.
func (t *groupTable) finish(unordered, merged bool) {
	t.order = make([]int32, t.n)
	for i := range t.order {
		t.order[i] = int32(i)
	}
	if unordered {
		return
	}
	if t.mode == keyModeInt641 {
		k := t.sortInt
		slices.SortFunc(t.order, func(a, b int32) int { return cmp.Compare(k[a], k[b]) })
		if !merged {
			return
		}
		t.runInt = make([]int64, len(t.order))
		for i, g := range t.order {
			t.runInt[i] = k[g]
		}
	} else {
		k := t.sortStr
		slices.SortFunc(t.order, func(a, b int32) int { return strings.Compare(k[a], k[b]) })
		if !merged {
			return
		}
		t.runStr = make([]string, len(t.order))
		for i, g := range t.order {
			t.runStr[i] = k[g]
		}
	}
}

// rowRef addresses one group: table t, gid g.
type rowRef struct{ t, g int32 }

// outputOrder lists every group of the finished tables in output
// order: the tables' sorted runs merged by key (keys are disjoint
// across tables, so the merge is total), or, when unordered, table by
// table.
func outputOrder(tables []*groupTable, mode keyMode, unordered bool) []rowRef {
	if unordered || len(tables) == 1 {
		total := 0
		for _, t := range tables {
			total += len(t.order)
		}
		order := make([]rowRef, 0, total)
		for ti, t := range tables {
			for _, g := range t.order {
				order = append(order, rowRef{int32(ti), g})
			}
		}
		return order
	}
	if mode == keyModeInt641 {
		runs := make([]sortedRun[int64], len(tables))
		for ti, t := range tables {
			runs[ti] = newSortedRun(int32(ti), t.order, t.runInt)
		}
		return mergeRuns(runs)
	}
	runs := make([]sortedRun[string], len(tables))
	for ti, t := range tables {
		runs[ti] = newSortedRun(int32(ti), t.order, t.runStr)
	}
	return mergeRuns(runs)
}

// sortedRun is one table's groups in key order, keys alongside.
type sortedRun[K cmp.Ordered] struct {
	refs []rowRef
	keys []K
}

func newSortedRun[K cmp.Ordered](t int32, order []int32, keys []K) sortedRun[K] {
	refs := make([]rowRef, len(order))
	for i, g := range order {
		refs[i] = rowRef{t, g}
	}
	return sortedRun[K]{refs: refs, keys: keys}
}

// mergeRuns merges sorted runs pairwise, each round's merges running
// concurrently, until one run is left.
func mergeRuns[K cmp.Ordered](runs []sortedRun[K]) []rowRef {
	for len(runs) > 1 {
		next := make([]sortedRun[K], (len(runs)+1)/2)
		var wg sync.WaitGroup
		for i := 0; i+1 < len(runs); i += 2 {
			wg.Go(func() { next[i/2] = mergeTwo(runs[i], runs[i+1]) })
		}
		if len(runs)%2 == 1 {
			next[len(next)-1] = runs[len(runs)-1]
		}
		wg.Wait()
		runs = next
	}
	if len(runs) == 0 {
		return nil
	}
	return runs[0].refs
}

func mergeTwo[K cmp.Ordered](a, b sortedRun[K]) sortedRun[K] {
	n := len(a.refs) + len(b.refs)
	out := sortedRun[K]{refs: make([]rowRef, 0, n), keys: make([]K, 0, n)}
	i, j := 0, 0
	for i < len(a.keys) && j < len(b.keys) {
		if b.keys[j] < a.keys[i] {
			out.refs = append(out.refs, b.refs[j])
			out.keys = append(out.keys, b.keys[j])
			j++
		} else {
			out.refs = append(out.refs, a.refs[i])
			out.keys = append(out.keys, a.keys[i])
			i++
		}
	}
	out.refs = append(append(out.refs, a.refs[i:]...), b.refs[j:]...)
	out.keys = append(append(out.keys, a.keys[i:]...), b.keys[j:]...)
	return out
}

// buildResultBatch emits one row per group across the build's tables,
// in outputOrder. Each column is written once, straight in final
// order: slab states append from their slices, captured values (keys,
// First / Last) are gathered with one take.
func (e *streamingAggregateExec) buildResultBatch() (arrow.RecordBatch, error) {
	pool := memory.DefaultAllocator
	tables := e.tables
	defer func() {
		for _, t := range tables {
			t.release()
		}
	}()
	order := outputOrder(tables, e.keyMode, e.unordered)
	out := make([]arrow.Array, 0, e.outSchema.NumFields())
	defer func() { releaseArrays(out) }()

	for i, k := range e.keys {
		dt := e.outSchema.Field(i).Type
		var arr arrow.Array
		var err error
		if e.keyMode == keyModeString1 {
			arr, err = emitStringKeys(pool, dt, tables, order)
		} else {
			caps := make([]*valueCapture, len(tables))
			for ti, t := range tables {
				caps[ti] = &t.keyCaps[i]
			}
			arr, err = takeCaptures(pool, dt, caps, order, func(r rowRef) int32 { return r.g })
		}
		if err != nil {
			return nil, fmt.Errorf("key col %s: %w", k, err)
		}
		out = append(out, arr)
	}
	for i, a := range e.aggs {
		peers := make([]aggState, len(tables))
		for ti, t := range tables {
			peers[ti] = t.states[i]
		}
		arr, err := peers[0].emit(pool, e.outSchema.Field(len(e.keys)+i).Type, peers, order)
		if err != nil {
			return nil, fmt.Errorf("agg %s: %w", aggName(a), err)
		}
		out = append(out, arr)
	}
	return array.NewRecordBatch(e.outSchema, out, int64(len(order))), nil
}

// emitStringKeys writes the String1 key column from the tables' key
// strings.
func emitStringKeys(pool memory.Allocator, dt arrow.DataType, tables []*groupTable, order []rowRef) (arrow.Array, error) {
	total := 0
	for _, t := range tables {
		for _, k := range t.sortStr[:t.n] {
			total += len(k)
		}
	}
	return buildBytesArray(pool, dt, len(order), total, func(i int) (string, bool) {
		r := order[i]
		return tables[r.t].sortStr[r.g], true
	})
}

// buildBytesArray builds a String / LargeString / Binary / LargeBinary
// array of n values straight into its buffers — get(i) returns value
// i, ok=false for null — skipping the builder's per-value bookkeeping.
// total is the byte length of all non-null values; get is called once
// per row, in order.
func buildBytesArray(pool memory.Allocator, dt arrow.DataType, n, total int, get func(int) (string, bool)) (arrow.Array, error) {
	large := false
	switch dt.ID() {
	case arrow.STRING, arrow.BINARY:
	case arrow.LARGE_STRING, arrow.LARGE_BINARY:
		large = true
	default:
		return nil, fmt.Errorf("byte values into %s", dt)
	}
	if !large && total > math.MaxInt32 {
		return nil, fmt.Errorf("%d bytes overflow %s offsets", total, dt)
	}
	offBuf := memory.NewResizableBuffer(pool)
	dataBuf := memory.NewResizableBuffer(pool)
	var bitmap *memory.Buffer
	defer func() {
		offBuf.Release()
		dataBuf.Release()
		if bitmap != nil {
			bitmap.Release()
		}
	}()
	width := 4
	if large {
		width = 8
	}
	offBuf.Resize((n + 1) * width)
	dataBuf.Resize(total)
	var nulls int
	var err error
	if large {
		bitmap, nulls, err = fillBytes(pool, arrow.Int64Traits.CastFromBytes(offBuf.Bytes()), dataBuf.Bytes(), n, get)
	} else {
		bitmap, nulls, err = fillBytes(pool, arrow.Int32Traits.CastFromBytes(offBuf.Bytes()), dataBuf.Bytes(), n, get)
	}
	if err != nil {
		return nil, err
	}
	d := array.NewData(dt, n, []*memory.Buffer{bitmap, offBuf, dataBuf}, nil, nulls, 0)
	defer d.Release()
	return array.MakeFromData(d), nil
}

// fillBytes writes buildBytesArray's offsets and data. The validity
// bitmap is allocated at the first null, with every earlier row marked
// valid; it stays nil when there are none.
func fillBytes[O int32 | int64](pool memory.Allocator, offs []O, data []byte, n int, get func(int) (string, bool)) (bitmap *memory.Buffer, nulls int, err error) {
	var bits []byte
	pos := 0
	for i := range n {
		offs[i] = O(pos)
		v, ok := get(i)
		if !ok {
			if bits == nil {
				bitmap = memory.NewResizableBuffer(pool)
				bitmap.Resize(int(bitutil.BytesForBits(int64(n))))
				bits = bitmap.Bytes()
				clear(bits)
				bitutil.SetBitsTo(bits, 0, int64(i), true)
			}
			nulls++
			continue
		}
		if bits != nil {
			bitutil.SetBit(bits, i)
		}
		if len(v) > len(data)-pos {
			if bitmap != nil {
				bitmap.Release()
			}
			return nil, 0, fmt.Errorf("byte values exceed the %d-byte total", len(data))
		}
		pos += copy(data[pos:], v)
	}
	offs[n] = O(pos)
	return bitmap, nulls, nil
}

// takeCaptures gathers captured values in order: slot(r) is the
// group's slot in its table's capture, -1 for null. The captures are
// read in place as one multi-chunk column, without concatenating.
func takeCaptures(pool memory.Allocator, dt arrow.DataType, caps []*valueCapture, order []rowRef, slot func(rowRef) int32) (arrow.Array, error) {
	base := make([]int, len(caps))
	var pieces []arrow.Array
	n := 0
	for i, c := range caps {
		base[i] = n
		n += c.n
		pieces = append(pieces, c.pieces...)
	}
	if len(pieces) == 0 {
		return array.MakeArrayOfNull(pool, dt, len(order)), nil
	}
	idx := make([]int, len(order))
	for i, r := range order {
		if s := slot(r); s >= 0 {
			idx[i] = base[r.t] + int(s)
		} else {
			idx[i] = -1
		}
	}
	chunked := arrow.NewChunked(dt, pieces)
	defer chunked.Release()
	col := arrow.NewColumn(arrow.Field{Name: "captured", Type: dt, Nullable: true}, chunked)
	defer col.Release()
	return takeArrayWithNulls(pool, NewSeries(col), idx)
}

func releaseArrays(arrs []arrow.Array) {
	for _, a := range arrs {
		a.Release()
	}
}

// strArena hands out strings backed by shared byte blocks, so storing
// a group's key costs no allocation of its own. Blocks start small and
// double up to strArenaBlock, so a table with few groups stays small.
// Bytes are never written after a string is cut from them.
type strArena struct {
	block []byte
}

const strArenaBlock = 64 << 10

func (a *strArena) bytes(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	if len(b) > strArenaBlock/4 {
		return string(b) // large keys get their own allocation
	}
	if cap(a.block)-len(a.block) < len(b) {
		size := min(max(2*cap(a.block), 256, len(b)), strArenaBlock)
		a.block = make([]byte, 0, size)
	}
	start := len(a.block)
	a.block = append(a.block, b...)
	return unsafe.String(&a.block[start], len(b))
}

func (a *strArena) string(s string) string {
	return a.bytes(unsafe.Slice(unsafe.StringData(s), len(s)))
}

// valueCapture copies selected cells of a column into owned Arrow
// arrays, one vectorized take per capture call. Captured values are
// addressed by slot: their position across the captures laid end to
// end.
type valueCapture struct {
	dt     arrow.DataType // planned type: captures of another type are cast to it
	pieces []arrow.Array
	n      int
}

// capture appends col's rows and returns the slot of the first one.
func (c *valueCapture) capture(col Series, rows []int) (int, error) {
	base := c.n
	if len(rows) == 0 {
		return base, nil
	}
	// takeArray, not compute.Take: arrow-go's variable-width take sizes
	// its output from the whole input, which over-allocates badly when
	// a few rows are taken from each batch.
	arr, err := takeArray(memory.DefaultAllocator, col, rows)
	if err != nil {
		return 0, err
	}
	if c.dt != nil && !arrow.TypeEqual(arr.DataType(), c.dt) {
		// The batch drifted from the planned schema (String vs
		// LargeString, a time zone label, …): cast so every piece has
		// the output type.
		cast, err := compute.CastArray(context.Background(), arr, compute.SafeCastOptions(c.dt))
		arr.Release()
		if err != nil {
			return 0, fmt.Errorf("%w: %s planned, column is %s: %v", ErrColumnTypeMismatch, c.dt, col.DataType(), err)
		}
		arr = cast
	}
	c.pieces = append(c.pieces, arr)
	c.n += len(rows)
	c.mergeTail()
	return base, nil
}

// mergeTail keeps the piece count logarithmic: while the next-to-last
// piece is no more than twice the last, the two are concatenated.
// Sizes then shrink geometrically along the list, so a value is copied
// O(log n) times in all, and a capture fed many small batches doesn't
// pile up thousands of pieces for every lookup to search. Adjacent
// pieces merge in order, so slots don't change.
func (c *valueCapture) mergeTail() {
	for n := len(c.pieces); n >= 2 && c.pieces[n-2].Len() <= 2*c.pieces[n-1].Len(); n = len(c.pieces) {
		merged, err := array.Concatenate(c.pieces[n-2:], memory.DefaultAllocator)
		if err != nil {
			return // e.g. dictionaries that don't unify: leave them apart
		}
		c.pieces[n-2].Release()
		c.pieces[n-1].Release()
		c.pieces = append(c.pieces[:n-2], merged)
	}
}

// compact keeps only the values slots still refer to, renumbering
// slots, so a capture rewritten many times (Last) doesn't grow without
// bound.
func (c *valueCapture) compact(dt arrow.DataType, slots []int32) error {
	keep := make([]int, 0, len(slots))
	for i, s := range slots {
		if s >= 0 {
			keep = append(keep, int(s))
			slots[i] = int32(len(keep) - 1)
		}
	}
	chunked := arrow.NewChunked(dt, c.pieces)
	col := arrow.NewColumn(arrow.Field{Name: "captured", Type: dt, Nullable: true}, chunked)
	arr, err := takeArray(memory.DefaultAllocator, NewSeries(col), keep)
	col.Release()
	chunked.Release()
	if err != nil {
		return err
	}
	c.release()
	c.pieces = []arrow.Array{arr}
	c.n = len(keep)
	return nil
}

func (c *valueCapture) release() {
	releaseArrays(c.pieces)
	c.pieces = nil
	c.n = 0
}

// aggBatch is one batch's rows routed to a table: rows[i] belongs to
// group gids[i]. byGroup lazily buckets the rows by group for the
// per-group accumulators.
type aggBatch struct {
	rows   []int
	gids   []int32
	groups int

	bucketed bool
	touched  []int32 // groups with rows in this batch, first-seen order
	start    []int32 // per gid: offset into sorted (valid for touched)
	count    []int32 // per gid: rows in this batch, zeroed on reset
	sorted   []int
}

func (b *aggBatch) reset() {
	b.rows = b.rows[:0]
	b.gids = b.gids[:0]
	if b.bucketed {
		for _, g := range b.touched {
			b.count[g] = 0
		}
		b.bucketed = false
	}
}

// byGroup buckets the batch's rows by group with a counting sort,
// keeping each group's rows in batch order.
func (b *aggBatch) byGroup() {
	if b.bucketed {
		return
	}
	b.bucketed = true
	for len(b.count) < b.groups {
		b.count = append(b.count, 0)
		b.start = append(b.start, 0)
	}
	b.touched = b.touched[:0]
	for _, g := range b.gids {
		if b.count[g] == 0 {
			b.touched = append(b.touched, g)
		}
		b.count[g]++
	}
	off := int32(0)
	for _, g := range b.touched {
		b.start[g] = off
		off += b.count[g]
		b.count[g] = 0 // reused below as the fill cursor
	}
	b.sorted = slices.Grow(b.sorted[:0], len(b.rows))[:len(b.rows)]
	for i, g := range b.gids {
		b.sorted[b.start[g]+b.count[g]] = b.rows[i]
		b.count[g]++
	}
}

// groupRows returns group g's rows in this batch; byGroup must have
// run.
func (b *aggBatch) groupRows(g int32) []int {
	return b.sorted[b.start[g] : b.start[g]+b.count[g]]
}

// -----------------------------------------------------------------------------
// Aggregation state
// -----------------------------------------------------------------------------

// aggState is one aggregation's state across every group of a table.
type aggState interface {
	// grow extends the state to n groups.
	grow(n int) error
	// update folds a batch's rows; col is zero for count(*).
	update(col Series, b *aggBatch) error
	// emit returns the results for the groups in order, as dt.
	// peers holds this aggregation's state in every table of the
	// build (including the receiver); order addresses them.
	emit(pool memory.Allocator, dt arrow.DataType, peers []aggState, order []rowRef) (arrow.Array, error)
	release()
}

// newAggState picks the columnar state for the built-in kinds that
// have one, and the per-group accumulator adapter for the rest.
// dt is the aggregation's planned output type (aggResultType); the
// columnar states store and emit exactly that, and reject a batch
// whose column doesn't fit it.
func newAggState(a Aggregation, dt arrow.DataType) (aggState, error) {
	if a.Fn == nil {
		switch a.Kind {
		case AggCount:
			return &countState{star: a.Column == ""}, nil
		case AggSum, AggMean:
			s := &sumState{mean: a.Kind == AggMean}
			switch dt.ID() {
			case arrow.INT64:
				s.integer, s.signed = true, true
			case arrow.UINT64:
				s.integer = true
			}
			return s, nil
		case AggMin, AggMax:
			_, ts := dt.(*arrow.TimestampType)
			return &minMaxState{isMin: a.Kind == AggMin, ts: ts}, nil
		case AggFirst, AggLast:
			s := &firstLastState{first: a.Kind == AggFirst, dt: dt, vals: newFLValues(dt)}
			if s.vals == nil {
				s.capt = &flCapture{last: !s.first}
				s.capt.cap.dt = dt
			}
			return s, nil
		}
	}
	if _, err := newAccumulator(a); err != nil {
		return nil, err
	}
	return &accState{agg: a}, nil
}

// appendDoubling is append with capacity doubling at every size
// (append grows large slices by ~1.25×, recopying them more often).
func appendDoubling[T any](s []T, v T) []T {
	if len(s) == cap(s) {
		s = slices.Grow(s, max(cap(s), 8))
	}
	return append(s, v)
}

// growTo extends s to n elements of fill, doubling the capacity when
// it has to grow (append's 1.25× growth for large slices would recopy
// a group slab many times over).
func growTo[T any](s []T, n int, fill T) []T {
	old := len(s)
	if old >= n {
		return s
	}
	if cap(s) < n {
		s = slices.Grow(s, max(n, 2*cap(s))-old)
	}
	s = s[:n]
	for i := old; i < n; i++ {
		s[i] = fill
	}
	return s
}

// countState: count(*) or count of non-null values.
type countState struct {
	star bool
	n    []int64
}

func (s *countState) grow(n int) error { s.n = growTo(s.n, n, 0); return nil }
func (s *countState) release()         {}

func (s *countState) update(col Series, b *aggBatch) error {
	if s.star {
		for _, g := range b.gids {
			s.n[g]++
		}
		return nil
	}
	if col.col == nil {
		return nil
	}
	cur := newChunkCursor(col)
	for i, row := range b.rows {
		null, err := cur.isNull(row)
		if err != nil {
			return err
		}
		if !null {
			s.n[b.gids[i]]++
		}
	}
	return nil
}

func (s *countState) emit(pool memory.Allocator, dt arrow.DataType, peers []aggState, order []rowRef) (arrow.Array, error) {
	ps := peersOf[*countState](peers)
	bld := array.NewInt64Builder(pool)
	defer bld.Release()
	bld.Reserve(len(order))
	for _, r := range order {
		bld.UnsafeAppend(ps[r.t].n[r.g])
	}
	return bld.NewArray(), nil
}

// sumState: Sum and Mean, in the planned output type. Float64 adds
// each value as float64 in row order — the same arithmetic the
// per-group accumulators used, so results match bit for bit. Int64 /
// Uint64 (Sum over an integer column) is an overflow-checked integer
// sum.
type sumState struct {
	mean    bool
	integer bool // Int64 or Uint64 output
	signed  bool // Int64 output
	sum     []float64
	isum    []int64
	usum    []uint64
	n       []int64 // non-null values; 0 → null output
}

func (s *sumState) grow(n int) error {
	s.n = growTo(s.n, n, 0)
	switch {
	case !s.integer:
		s.sum = growTo(s.sum, n, 0)
	case s.signed:
		s.isum = growTo(s.isum, n, 0)
	default:
		s.usum = growTo(s.usum, n, 0)
	}
	return nil
}
func (s *sumState) release() {}

func (s *sumState) update(col Series, b *aggBatch) error {
	if col.col == nil {
		return nil
	}
	if !s.integer {
		return foldNumeric(col, b, func(g int32, v float64) {
			s.sum[g] += v
			s.n[g]++
		})
	}
	if intSumType(col.DataType()) == nil {
		return fmt.Errorf("%w: integer sum planned, column is %s", ErrColumnTypeMismatch, col.DataType())
	}
	chunks := col.col.Data().Chunks()
	if len(chunks) == 1 {
		switch a := chunks[0].(type) {
		case *array.Int64:
			return foldInts(s, a, a.Int64Values(), b)
		case *array.Int32:
			return foldInts(s, a, a.Int32Values(), b)
		case *array.Int16:
			return foldInts(s, a, a.Int16Values(), b)
		case *array.Int8:
			return foldInts(s, a, a.Int8Values(), b)
		case *array.Uint64:
			return foldInts(s, a, a.Uint64Values(), b)
		case *array.Uint32:
			return foldInts(s, a, a.Uint32Values(), b)
		case *array.Uint16:
			return foldInts(s, a, a.Uint16Values(), b)
		case *array.Uint8:
			return foldInts(s, a, a.Uint8Values(), b)
		}
	}
	loc := newChunkLocator(col)
	for i, row := range b.rows {
		chunk, local, ok := loc.locate(row)
		if !ok {
			return fmt.Errorf("%w: row %d", ErrRowOutOfRange, row)
		}
		if chunk.IsNull(local) {
			continue
		}
		v, neg, err := intCell(chunk, local)
		if err != nil {
			return err
		}
		if err := s.add(b.gids[i], v, neg); err != nil {
			return err
		}
	}
	return nil
}

// add folds one integer value into group g's sum: v is its magnitude
// bits as uint64 and neg its sign (v is then the two's-complement
// int64). A value outside the output type — negative into Uint64,
// above MaxInt64 into Int64 — is an overflow.
func (s *sumState) add(g int32, v uint64, neg bool) error {
	var ok bool
	if s.signed {
		if !neg && v > math.MaxInt64 {
			return ErrSumOverflow
		}
		s.isum[g], ok = addInt64(s.isum[g], int64(v))
	} else {
		if neg {
			return ErrSumOverflow
		}
		s.usum[g], ok = addUint64(s.usum[g], v)
	}
	if !ok {
		return ErrSumOverflow
	}
	s.n[g]++
	return nil
}

// foldInts folds a single-chunk integer column of any width into the
// sum's Int64 or Uint64 accumulator.
func foldInts[T int64 | int32 | int16 | int8 | uint64 | uint32 | uint16 | uint8](s *sumState, a arrow.Array, vals []T, b *aggBatch) error {
	nulls := a.NullN() > 0
	for i, row := range b.rows {
		if nulls && a.IsNull(row) {
			continue
		}
		v := vals[row]
		if err := s.add(b.gids[i], uint64(v), v < 0); err != nil {
			return err
		}
	}
	return nil
}

func (s *sumState) emit(pool memory.Allocator, dt arrow.DataType, peers []aggState, order []rowRef) (arrow.Array, error) {
	ps := peersOf[*sumState](peers)
	switch {
	case s.integer && s.signed:
		bld := array.NewInt64Builder(pool)
		defer bld.Release()
		bld.Reserve(len(order))
		for _, r := range order {
			p := ps[r.t]
			if p.n[r.g] == 0 {
				bld.UnsafeAppendBoolToBitmap(false)
				continue
			}
			bld.UnsafeAppend(p.isum[r.g])
		}
		return bld.NewArray(), nil
	case s.integer:
		bld := array.NewUint64Builder(pool)
		defer bld.Release()
		bld.Reserve(len(order))
		for _, r := range order {
			p := ps[r.t]
			if p.n[r.g] == 0 {
				bld.UnsafeAppendBoolToBitmap(false)
				continue
			}
			bld.UnsafeAppend(p.usum[r.g])
		}
		return bld.NewArray(), nil
	}
	bld := array.NewFloat64Builder(pool)
	defer bld.Release()
	bld.Reserve(len(order))
	for _, r := range order {
		p := ps[r.t]
		switch {
		case p.n[r.g] == 0:
			bld.UnsafeAppendBoolToBitmap(false)
		case s.mean:
			bld.UnsafeAppend(p.sum[r.g] / float64(p.n[r.g]))
		default:
			bld.UnsafeAppend(p.sum[r.g])
		}
	}
	return bld.NewArray(), nil
}

// foldNumeric calls fn(gid, value) for every non-null numeric cell of
// the batch's rows, in row order. Single-chunk Float64 / Int64 /
// Float32 / Int32 columns read their value slices directly.
func foldNumeric(col Series, b *aggBatch, fn func(int32, float64)) error {
	chunks := col.col.Data().Chunks()
	if len(chunks) == 1 {
		switch a := chunks[0].(type) {
		case *array.Float64:
			foldTyped(a, a.Float64Values(), b, fn)
			return nil
		case *array.Int64:
			foldTyped(a, a.Int64Values(), b, fn)
			return nil
		case *array.Float32:
			foldTyped(a, a.Float32Values(), b, fn)
			return nil
		case *array.Int32:
			foldTyped(a, a.Int32Values(), b, fn)
			return nil
		}
	}
	for i, row := range b.rows {
		v, ok, err := col.numericAt(row)
		if err != nil {
			return err
		}
		if ok {
			fn(b.gids[i], v)
		}
	}
	return nil
}

func foldTyped[T int64 | int32 | float64 | float32](a arrow.Array, vals []T, b *aggBatch, fn func(int32, float64)) {
	if a.NullN() == 0 {
		for i, row := range b.rows {
			fn(b.gids[i], float64(vals[row]))
		}
		return
	}
	for i, row := range b.rows {
		if !a.IsNull(row) {
			fn(b.gids[i], float64(vals[row]))
		}
	}
}

// minMaxState: Min / Max in the planned output type: Float64, or the
// source Timestamp type (compared as int64, so nanosecond precision
// survives).
type minMaxState struct {
	isMin bool
	ts    bool // Timestamp output
	ext   []float64
	tsExt []int64
	seen  []bool
}

func (s *minMaxState) grow(n int) error {
	if s.ts {
		s.tsExt = growTo(s.tsExt, n, 0)
	} else {
		s.ext = growTo(s.ext, n, 0)
	}
	s.seen = growTo(s.seen, n, false)
	return nil
}
func (s *minMaxState) release() {}

func (s *minMaxState) update(col Series, b *aggBatch) error {
	if col.col == nil {
		return nil
	}
	if !s.ts {
		// Same rule as minMaxAcc: the first value is taken as is, after
		// that only a strictly smaller (larger) one replaces it, so a
		// NaN never displaces a number.
		return foldNumeric(col, b, func(g int32, v float64) {
			switch {
			case !s.seen[g]:
				s.ext[g], s.seen[g] = v, true
			case s.isMin && v < s.ext[g], !s.isMin && v > s.ext[g]:
				s.ext[g] = v
			}
		})
	}
	if _, ok := col.DataType().(*arrow.TimestampType); !ok {
		return fmt.Errorf("%w: timestamp min/max planned, column is %s", ErrColumnTypeMismatch, col.DataType())
	}
	for i, row := range b.rows {
		v, null, err := timestampAt(col, row)
		if err != nil {
			return err
		}
		if null {
			continue
		}
		g := b.gids[i]
		switch {
		case !s.seen[g]:
			s.tsExt[g], s.seen[g] = int64(v), true
		case s.isMin && int64(v) < s.tsExt[g], !s.isMin && int64(v) > s.tsExt[g]:
			s.tsExt[g] = int64(v)
		}
	}
	return nil
}

func (s *minMaxState) emit(pool memory.Allocator, dt arrow.DataType, peers []aggState, order []rowRef) (arrow.Array, error) {
	ps := peersOf[*minMaxState](peers)
	if s.ts {
		bld := array.NewTimestampBuilder(pool, dt.(*arrow.TimestampType))
		defer bld.Release()
		bld.Reserve(len(order))
		for _, r := range order {
			p := ps[r.t]
			if !p.seen[r.g] {
				bld.UnsafeAppendBoolToBitmap(false)
				continue
			}
			bld.UnsafeAppend(arrow.Timestamp(p.tsExt[r.g]))
		}
		return bld.NewArray(), nil
	}
	bld := array.NewFloat64Builder(pool)
	defer bld.Release()
	bld.Reserve(len(order))
	for _, r := range order {
		p := ps[r.t]
		if !p.seen[r.g] {
			bld.UnsafeAppendBoolToBitmap(false)
			continue
		}
		bld.UnsafeAppend(p.ext[r.g])
	}
	return bld.NewArray(), nil
}

// firstLastState: First / Last non-null value, in the source type.
// Strings and binary are stored per group in an arena, and common
// fixed-width types in a typed slice, so each value is copied once on
// the way in and once on the way out. Other types (dictionaries,
// decimals, nested, …) go through capture: one vectorized take per
// batch of the rows to keep. Last overwrites, so its arena and capture
// are compacted when stale values pile up.
type firstLastState struct {
	first bool
	dt    arrow.DataType // planned output type = source type
	set   []bool         // per gid: has a value
	nset  int            // groups with a value
	vals  flValues       // typed storage; nil when capt is used
	capt  *flCapture     // take-based storage
}

func (s *firstLastState) grow(n int) error {
	s.set = growTo(s.set, n, false)
	if s.vals != nil {
		s.vals.grow(n)
	} else {
		s.capt.grow(n)
	}
	return nil
}

func (s *firstLastState) release() {
	if s.capt != nil {
		s.capt.cap.release()
	}
}

func (s *firstLastState) update(col Series, b *aggBatch) error {
	if col.col == nil {
		return nil
	}
	if s.first && s.nset == len(s.set) {
		return nil // every group has its first value
	}
	if s.capt != nil {
		added, err := s.capt.update(col, b, s.first, s.set)
		s.nset += added
		return err
	}
	if !s.vals.holds(col.DataType()) {
		return fmt.Errorf("%w: %s first/last planned, column is %s", ErrColumnTypeMismatch, s.dt, col.DataType())
	}
	loc := newChunkLocator(col)
	single := len(loc.chunks) == 1
	noNulls := single && loc.chunks[0].NullN() == 0
	for i, row := range b.rows {
		g := b.gids[i]
		if s.first && s.set[g] {
			continue
		}
		chunk, local := loc.chunks[0], row
		if !single {
			var ok bool
			if chunk, local, ok = loc.locate(row); !ok {
				return fmt.Errorf("%w: row %d", ErrRowOutOfRange, row)
			}
		}
		if !noNulls && chunk.IsNull(local) {
			continue
		}
		s.vals.put(g, chunk, local)
		if !s.set[g] {
			s.set[g] = true
			s.nset++
		}
	}
	if !s.first {
		s.vals.compact(s.set)
	}
	return nil
}

func (s *firstLastState) emit(pool memory.Allocator, dt arrow.DataType, peers []aggState, order []rowRef) (arrow.Array, error) {
	ps := peersOf[*firstLastState](peers)
	if s.capt != nil {
		caps := make([]*valueCapture, len(ps))
		for i, p := range ps {
			caps[i] = &p.capt.cap
		}
		return takeCaptures(pool, dt, caps, order, func(r rowRef) int32 { return ps[r.t].capt.slot[r.g] })
	}
	if _, ok := s.vals.(*flBytes); ok {
		bs := make([]*flBytes, len(ps))
		total := 0
		for i, p := range ps {
			bs[i] = p.vals.(*flBytes)
		}
		for _, r := range order {
			if ps[r.t].set[r.g] {
				total += len(bs[r.t].vals[r.g])
			}
		}
		return buildBytesArray(pool, dt, len(order), total, func(i int) (string, bool) {
			r := order[i]
			if !ps[r.t].set[r.g] {
				return "", false
			}
			return bs[r.t].vals[r.g], true
		})
	}
	// array.NewBuilder covers every fixed-width type flFixed stores
	// (builderForType doesn't know Date32 / Date64).
	bld := array.NewBuilder(pool, dt)
	defer bld.Release()
	vs := make([]flValues, len(ps))
	for i, p := range ps {
		vs[i] = p.vals
	}
	if err := s.vals.emit(bld, vs, ps, order); err != nil {
		return nil, err
	}
	return bld.NewArray(), nil
}

// flValues is firstLastState's typed per-group storage.
type flValues interface {
	// holds reports whether values of a column of type dt can be
	// stored — e.g. String and LargeString share byte storage, and a
	// timestamp's zone label doesn't matter.
	holds(dt arrow.DataType) bool
	grow(n int)
	// put stores chunk[i] (non-null) as group g's value.
	put(g int32, chunk arrow.Array, i int)
	// compact reclaims storage overwritten values left behind.
	compact(set []bool)
	// emit appends every group in order, nulls where unset; peers are
	// the same aggregation's storage in every table.
	emit(bld array.Builder, peers []flValues, states []*firstLastState, order []rowRef) error
}

// newFLValues returns typed storage for dt, or nil when dt needs the
// take-based capture.
func newFLValues(dt arrow.DataType) flValues {
	switch dt.ID() {
	case arrow.STRING, arrow.LARGE_STRING, arrow.BINARY, arrow.LARGE_BINARY:
		return &flBytes{}
	case arrow.INT64:
		return &flFixed[int64]{}
	case arrow.INT32:
		return &flFixed[int32]{}
	case arrow.INT16:
		return &flFixed[int16]{}
	case arrow.INT8:
		return &flFixed[int8]{}
	case arrow.UINT64:
		return &flFixed[uint64]{}
	case arrow.UINT32:
		return &flFixed[uint32]{}
	case arrow.UINT16:
		return &flFixed[uint16]{}
	case arrow.UINT8:
		return &flFixed[uint8]{}
	case arrow.FLOAT64:
		return &flFixed[float64]{}
	case arrow.FLOAT32:
		return &flFixed[float32]{}
	case arrow.BOOL:
		return &flFixed[bool]{}
	case arrow.TIMESTAMP:
		return &flFixed[arrow.Timestamp]{}
	case arrow.DATE32:
		return &flFixed[arrow.Date32]{}
	case arrow.DATE64:
		return &flFixed[arrow.Date64]{}
	}
	return nil
}

// flBytes stores string / binary values in an arena.
type flBytes struct {
	vals  []string
	arena strArena
	used  int // arena bytes handed out
	live  int // bytes of the current values
}

func (v *flBytes) grow(n int) { v.vals = growTo(v.vals, n, "") }

func (v *flBytes) holds(dt arrow.DataType) bool {
	switch dt.ID() {
	case arrow.STRING, arrow.LARGE_STRING, arrow.BINARY, arrow.LARGE_BINARY:
		return true
	}
	return false
}

func (v *flBytes) put(g int32, chunk arrow.Array, i int) {
	var s string
	switch a := chunk.(type) {
	case *array.String:
		s = a.Value(i)
	case *array.LargeString:
		s = a.Value(i)
	case *array.Binary:
		s = unsafe.String(unsafe.SliceData(a.Value(i)), len(a.Value(i)))
	case *array.LargeBinary:
		s = unsafe.String(unsafe.SliceData(a.Value(i)), len(a.Value(i)))
	}
	v.live += len(s) - len(v.vals[g])
	v.used += len(s)
	v.vals[g] = v.arena.string(s)
}

func (v *flBytes) compact(set []bool) {
	if v.used < 2*v.live+strArenaBlock {
		return
	}
	var fresh strArena
	for g, ok := range set {
		if ok {
			v.vals[g] = fresh.string(v.vals[g])
		}
	}
	v.arena, v.used = fresh, v.live
}

func (v *flBytes) emit(bld array.Builder, peers []flValues, states []*firstLastState, order []rowRef) error {
	return errBytesEmit // flBytes is emitted by firstLastState.emit
}

var errBytesEmit = errors.New("gobi: internal: flBytes emits through buildBytesArray")

// flFixed stores fixed-width values in a typed slice.
type flFixed[T any] struct {
	vals []T
}

func (v *flFixed[T]) holds(dt arrow.DataType) bool {
	b := array.NewBuilder(memory.DefaultAllocator, dt)
	defer b.Release()
	arr := b.NewArray()
	defer arr.Release()
	_, ok := arr.(interface{ Value(int) T })
	return ok
}

func (v *flFixed[T]) grow(n int) {
	var zero T
	v.vals = growTo(v.vals, n, zero)
}

func (v *flFixed[T]) put(g int32, chunk arrow.Array, i int) {
	v.vals[g] = chunk.(interface{ Value(int) T }).Value(i)
}

func (v *flFixed[T]) compact([]bool) {}

func (v *flFixed[T]) emit(bld array.Builder, peers []flValues, states []*firstLastState, order []rowRef) error {
	b, ok := bld.(interface {
		Append(T)
		AppendNull()
		Reserve(int)
	})
	if !ok {
		return fmt.Errorf("value %T into %T", *new(T), bld)
	}
	b.Reserve(len(order))
	for _, r := range order {
		if !states[r.t].set[r.g] {
			b.AppendNull()
			continue
		}
		b.Append(peers[r.t].(*flFixed[T]).vals[r.g])
	}
	return nil
}

// flCapture is the take-based storage for types without typed
// storage: per batch, one take of the rows to keep.
type flCapture struct {
	last   bool    // Last: keep each batch's last row per group
	slot   []int32 // per gid: capture slot, -1 = none
	cap    valueCapture
	pick   []int   // scratch: rows to capture this batch
	pickG  []int32 // scratch: their groups
	lastAt []int32 // scratch (Last): per gid, index into pick, -1 = none
}

func (c *flCapture) grow(n int) {
	c.slot = growTo(c.slot, n, -1)
	if c.last {
		c.lastAt = growTo(c.lastAt, n, -1)
	}
}

// update captures the batch's values and marks their groups in set,
// returning how many groups got their first value.
func (c *flCapture) update(col Series, b *aggBatch, first bool, set []bool) (int, error) {
	cur := newChunkCursor(col)
	c.pick, c.pickG = c.pick[:0], c.pickG[:0]
	for i, row := range b.rows {
		g := b.gids[i]
		if first && (set[g] || c.slot[g] == -2) {
			continue // captured, or picked earlier in this batch
		}
		null, err := cur.isNull(row)
		if err != nil {
			return 0, err
		}
		if null {
			continue
		}
		switch {
		case first:
			c.slot[g] = -2
			c.pick = append(c.pick, row)
			c.pickG = append(c.pickG, g)
		case c.lastAt[g] < 0:
			c.lastAt[g] = int32(len(c.pick))
			c.pick = append(c.pick, row)
			c.pickG = append(c.pickG, g)
		default:
			c.pick[c.lastAt[g]] = row
		}
	}
	base, err := c.cap.capture(col, c.pick)
	if err != nil {
		return 0, err
	}
	added := 0
	for j, g := range c.pickG {
		c.slot[g] = int32(base + j)
		if !set[g] {
			set[g] = true
			added++
		}
		if c.last {
			c.lastAt[g] = -1
		}
	}
	if c.last && c.cap.n > 2*len(c.slot)+1024 {
		return added, c.cap.compact(col.DataType(), c.slot)
	}
	return added, nil
}

// accState runs one per-group aggAccumulator per group, for the kinds
// without a columnar state (and custom IncrementalAggregators).
type accState struct {
	agg  Aggregation
	accs []aggAccumulator
}

func (s *accState) grow(n int) error {
	for len(s.accs) < n {
		acc, err := newAccumulator(s.agg)
		if err != nil {
			return err
		}
		s.accs = append(s.accs, acc)
	}
	return nil
}
func (s *accState) release() {}

func (s *accState) update(col Series, b *aggBatch) error {
	b.byGroup()
	for _, g := range b.touched {
		if err := s.accs[g].Update(col, b.groupRows(g)); err != nil {
			return err
		}
	}
	return nil
}

func (s *accState) emit(pool memory.Allocator, dt arrow.DataType, peers []aggState, order []rowRef) (arrow.Array, error) {
	ps := peersOf[*accState](peers)
	bld, err := builderForType(pool, dt)
	if err != nil {
		return nil, err
	}
	defer bld.Release()
	for _, r := range order {
		if err := appendCustomValue(bld, ps[r.t].accs[r.g].Finalize()); err != nil {
			return nil, err
		}
	}
	return bld.NewArray(), nil
}

// peersOf asserts every table's state for one aggregation to its
// concrete type (all tables build the same kind).
func peersOf[T aggState](peers []aggState) []T {
	out := make([]T, len(peers))
	for i, p := range peers {
		out[i] = p.(T)
	}
	return out
}

// readScalarAt extracts a Go-typed scalar from a Series at row. For
// plain columns the value types match what builderForType and
// appendCustomValue expect. Dictionary-encoded cells resolve to their
// value type (see readArrayScalar), but builderForType and
// keyOfAppend don't accept a dictionary *column* type, so grouping or
// aggregating into a dictionary column still errors up front.
//
// Walks the chunk list from the start on every call — fine for a
// one-off read, O(chunks) per row inside a loop. Row loops over
// possibly multi-chunk columns should build a chunkCursor once and
// call its scalarAt instead.
func readScalarAt(s Series, row int) (any, error) {
	if s.col == nil {
		return nil, fmt.Errorf("readScalarAt: nil column")
	}
	chunk, local, ok := locateRowInChunks(s.col.Data().Chunks(), row)
	if !ok {
		return nil, fmt.Errorf("readScalarAt: row %d out of range", row)
	}
	return readArrayScalar(chunk, local)
}

// -----------------------------------------------------------------------------
// Per-group accumulators
//
// One object per group per aggregation. The streaming GroupBy uses
// them (through accState) only for the kinds without a columnar state
// — Std / Var / NUnique / Median / Mode / bitwise / custom — and Over
// uses them for every kind. Over's sums stay Float64:
//
//   Count → Int64, non-null (0 for empty group counts non-null cols)
//   Sum   → Float64, null on empty group
//   Mean  → Float64, null on empty group
//   Min   → Float64, null on empty group
//   Max   → Float64, null on empty group
// -----------------------------------------------------------------------------

// countAcc counts either all rows in the group (starCount=true) or
// only non-null values in the passed column.
type countAcc struct {
	starCount bool
	n         int64
}

func (a *countAcc) Update(col Series, rows []int) error {
	if a.starCount {
		a.n += int64(len(rows))
		return nil
	}
	for _, row := range rows {
		_, ok, err := col.numericAt(row)
		if err != nil {
			// For non-numeric columns, fall back to any-value read
			// via a null probe: if the chunk reports null at row,
			// skip; otherwise count.
			null, err2 := isNullAtSeries(col, row)
			if err2 != nil {
				return err
			}
			if !null {
				a.n++
			}
			continue
		}
		if ok {
			a.n++
		}
	}
	return nil
}

func (a *countAcc) Finalize() any              { return a.n }
func (a *countAcc) OutputType() arrow.DataType { return arrow.PrimitiveTypes.Int64 }

// sumAcc keeps a running sum of numeric values. Skips nulls. Empty
// group finalizes to nil (null in output).
type sumAcc struct {
	sum  float64
	seen bool
}

func (a *sumAcc) Update(col Series, rows []int) error {
	if col.col == nil {
		return nil
	}
	// Single-chunk fast paths: type-switch once on the chunk and
	// iterate rows with a direct typed accessor. Avoids the per-row
	// interface dispatch + chunk-walker in numericAt, which
	// consistently shows up in profiles as one of the hottest spots
	// on aggregation-heavy workloads.
	chunks := col.col.Data().Chunks()
	if len(chunks) == 1 {
		switch arr := chunks[0].(type) {
		case *array.Float64:
			for _, row := range rows {
				if arr.IsNull(row) {
					continue
				}
				a.sum += arr.Value(row)
				a.seen = true
			}
			return nil
		case *array.Int64:
			for _, row := range rows {
				if arr.IsNull(row) {
					continue
				}
				a.sum += float64(arr.Value(row))
				a.seen = true
			}
			return nil
		case *array.Float32:
			for _, row := range rows {
				if arr.IsNull(row) {
					continue
				}
				a.sum += float64(arr.Value(row))
				a.seen = true
			}
			return nil
		case *array.Int32:
			for _, row := range rows {
				if arr.IsNull(row) {
					continue
				}
				a.sum += float64(arr.Value(row))
				a.seen = true
			}
			return nil
		}
	}
	// Multi-chunk or non-numeric-shortlist column: fall back to the
	// generic walker.
	for _, row := range rows {
		v, ok, err := col.numericAt(row)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		a.sum += v
		a.seen = true
	}
	return nil
}

func (a *sumAcc) Finalize() any {
	if !a.seen {
		return nil
	}
	return a.sum
}
func (a *sumAcc) OutputType() arrow.DataType { return arrow.PrimitiveTypes.Float64 }

// meanAcc keeps (sum, count) and finalizes to sum/count.
type meanAcc struct {
	sum float64
	n   int64
}

func (a *meanAcc) Update(col Series, rows []int) error {
	if col.col == nil {
		return nil
	}
	chunks := col.col.Data().Chunks()
	if len(chunks) == 1 {
		switch arr := chunks[0].(type) {
		case *array.Float64:
			for _, row := range rows {
				if arr.IsNull(row) {
					continue
				}
				a.sum += arr.Value(row)
				a.n++
			}
			return nil
		case *array.Int64:
			for _, row := range rows {
				if arr.IsNull(row) {
					continue
				}
				a.sum += float64(arr.Value(row))
				a.n++
			}
			return nil
		case *array.Float32:
			for _, row := range rows {
				if arr.IsNull(row) {
					continue
				}
				a.sum += float64(arr.Value(row))
				a.n++
			}
			return nil
		case *array.Int32:
			for _, row := range rows {
				if arr.IsNull(row) {
					continue
				}
				a.sum += float64(arr.Value(row))
				a.n++
			}
			return nil
		}
	}
	for _, row := range rows {
		v, ok, err := col.numericAt(row)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		a.sum += v
		a.n++
	}
	return nil
}

func (a *meanAcc) Finalize() any {
	if a.n == 0 {
		return nil
	}
	return a.sum / float64(a.n)
}
func (a *meanAcc) OutputType() arrow.DataType { return arrow.PrimitiveTypes.Float64 }

// minMaxAcc tracks a running min OR max. extreme is initialized to
// +Inf for min and -Inf for max so the first non-null value always
// replaces it.
//
// Timestamp columns: on first Update the accumulator captures the
// source's arrow.TimestampType and switches to an int64 fast path
// (tsExtreme). Finalize returns arrow.Timestamp in that case, and
// OutputType returns the captured type so builderForType stands up
// a matching TimestampBuilder in the LazyFrame executor and in
// scalarAggNode.Eval (Over-scalar path). The Timestamp case never
// mixes with the numeric case within one accumulator — a single
// column drives a single accumulator, and column types don't shift
// across batches.
type minMaxAcc struct {
	isMin       bool
	extreme     float64
	seen        bool
	isTimestamp bool
	tsExtreme   arrow.Timestamp
	tsType      arrow.DataType
}

func (a *minMaxAcc) Update(col Series, rows []int) error {
	if col.col == nil {
		return nil
	}
	// Timestamp branch (detected once on first non-empty Update).
	if a.tsType == nil && !a.seen {
		if _, isTS := col.DataType().(*arrow.TimestampType); isTS {
			a.isTimestamp = true
			a.tsType = col.DataType()
		}
	}
	if a.isTimestamp {
		return a.updateTimestamp(col, rows)
	}
	chunks := col.col.Data().Chunks()
	if len(chunks) == 1 {
		switch arr := chunks[0].(type) {
		case *array.Float64:
			return a.updateFloat64(arr, rows)
		case *array.Int64:
			return a.updateInt64(arr, rows)
		case *array.Float32:
			return a.updateFloat32(arr, rows)
		case *array.Int32:
			return a.updateInt32(arr, rows)
		}
	}
	for _, row := range rows {
		v, ok, err := col.numericAt(row)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		a.update(v)
	}
	return nil
}

// updateTimestamp compares raw int64 timestamp values without
// float64 conversion — preserves nanosecond precision beyond 2^53
// (matters for timestamps far from epoch).
func (a *minMaxAcc) updateTimestamp(col Series, rows []int) error {
	for _, row := range rows {
		v, null, err := timestampAt(col, row)
		if err != nil {
			return err
		}
		if null {
			continue
		}
		if !a.seen {
			a.tsExtreme = v
			a.seen = true
			continue
		}
		if a.isMin && v < a.tsExtreme {
			a.tsExtreme = v
		} else if !a.isMin && v > a.tsExtreme {
			a.tsExtreme = v
		}
	}
	return nil
}

// update merges a single non-null value into the running extreme.
// Inlined by the single-chunk fast paths and the generic fallback.
func (a *minMaxAcc) update(v float64) {
	if !a.seen {
		a.extreme = v
		a.seen = true
		return
	}
	if a.isMin && v < a.extreme {
		a.extreme = v
	} else if !a.isMin && v > a.extreme {
		a.extreme = v
	}
}

func (a *minMaxAcc) updateFloat64(arr *array.Float64, rows []int) error {
	for _, row := range rows {
		if arr.IsNull(row) {
			continue
		}
		a.update(arr.Value(row))
	}
	return nil
}
func (a *minMaxAcc) updateInt64(arr *array.Int64, rows []int) error {
	for _, row := range rows {
		if arr.IsNull(row) {
			continue
		}
		a.update(float64(arr.Value(row)))
	}
	return nil
}
func (a *minMaxAcc) updateFloat32(arr *array.Float32, rows []int) error {
	for _, row := range rows {
		if arr.IsNull(row) {
			continue
		}
		a.update(float64(arr.Value(row)))
	}
	return nil
}
func (a *minMaxAcc) updateInt32(arr *array.Int32, rows []int) error {
	for _, row := range rows {
		if arr.IsNull(row) {
			continue
		}
		a.update(float64(arr.Value(row)))
	}
	return nil
}

func (a *minMaxAcc) Finalize() any {
	if !a.seen {
		return nil
	}
	if a.isTimestamp {
		return a.tsExtreme
	}
	return a.extreme
}
func (a *minMaxAcc) OutputType() arrow.DataType {
	if a.isTimestamp && a.tsType != nil {
		return a.tsType
	}
	return arrow.PrimitiveTypes.Float64
}

// firstLastAcc captures the first (keepFirst=true) or last
// (keepFirst=false) non-null value seen. Update is called in row
// order per batch, and batches arrive in stream order — so
// "first non-null across all rows we ever see" and "last non-null
// across all rows we ever see" fall out naturally by:
//   - First: on the first non-null row we see, capture and set seen.
//     Skip further updates.
//   - Last: on each non-null row, overwrite the captured value.
//
// The captured value's type follows the source column; the schema
// declares that type at Compile time so buildResultBatch's builder
// matches.
type firstLastAcc struct {
	keepFirst bool
	seen      bool
	value     any
}

func (a *firstLastAcc) Update(col Series, rows []int) error {
	if a.keepFirst && a.seen {
		// First: already captured, nothing more to do for any batch.
		return nil
	}
	for _, row := range rows {
		null, err := isNullAtSeries(col, row)
		if err != nil {
			return err
		}
		if null {
			continue
		}
		v, err := readScalarAt(col, row)
		if err != nil {
			return err
		}
		a.value = v
		a.seen = true
		if a.keepFirst {
			return nil
		}
	}
	return nil
}

func (a *firstLastAcc) Finalize() any {
	if !a.seen {
		return nil
	}
	return a.value
}

// OutputType is a fallback — the schema field type takes precedence
// in buildResultBatch. Returning Float64 here just documents that
// this accumulator can hold any scalar; the actual arrow builder
// is picked from the plan's schema, not from this.
func (a *firstLastAcc) OutputType() arrow.DataType { return arrow.PrimitiveTypes.Float64 }

// stdVarAcc computes sample standard deviation (wantStd=true) or
// sample variance (wantStd=false) via Welford's online algorithm.
// Streaming-safe — no second pass, numerically stable for large
// batches, one accumulator per group. Undefined for n<2; Finalize
// returns nil in that case (matches pandas / polars).
type stdVarAcc struct {
	wantStd  bool
	n        int64
	mean, m2 float64
}

func (a *stdVarAcc) Update(col Series, rows []int) error {
	for _, row := range rows {
		v, ok, err := col.numericAt(row)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		a.n++
		delta := v - a.mean
		a.mean += delta / float64(a.n)
		a.m2 += delta * (v - a.mean)
	}
	return nil
}

func (a *stdVarAcc) Finalize() any {
	if a.n < 2 {
		return nil
	}
	variance := a.m2 / float64(a.n-1)
	if a.wantStd {
		return math.Sqrt(variance)
	}
	return variance
}
func (a *stdVarAcc) OutputType() arrow.DataType { return arrow.PrimitiveTypes.Float64 }

// nUniqueAcc counts distinct non-null values seen. Type-specializes
// on first non-empty Update so subsequent rows skip the keyOfAppend
// byte-encoding for String / Int64 / Float64 columns — the common
// cases — and store into a native Go map keyed on the arrow value
// directly. Other column shapes (multi-chunk in a single batch,
// non-primitive types) fall through to the byte-encoded generic
// path, which matches GroupBy's own key encoding so bit-equal
// numeric values still collapse identically.
//
// Memory is O(distinct-values-per-group); no different from polars /
// pandas nunique in that respect. String path clones on first-seen
// (arrow string values alias into the batch buffer, which is
// released before Finalize walks the map).
type nUniqueAcc struct {
	// kind is set on first Update from a non-empty batch. Subsequent
	// Updates dispatch on it without re-inspecting col.
	kind nuKind

	// Populated when kind matches. Only one of the four is used per
	// accumulator instance.
	seenStr   map[string]struct{}
	seenI64   map[int64]struct{}
	seenF64   map[float64]struct{}
	seenBytes map[string]struct{}
	scratch   []byte
}

type nuKind uint8

const (
	nuKindUnset nuKind = iota
	nuKindString
	nuKindInt64
	nuKindFloat64
	nuKindBytes
)

func (a *nUniqueAcc) Update(col Series, rows []int) error {
	if a.kind == nuKindUnset && len(rows) > 0 && col.col != nil {
		a.pickKind(col)
	}
	switch a.kind {
	case nuKindString:
		if err := a.updateString(col, rows); err == errNUniqueFallback {
			return a.updateBytes(col, rows)
		} else if err != nil {
			return err
		}
	case nuKindInt64:
		if err := a.updateInt64(col, rows); err == errNUniqueFallback {
			return a.updateBytes(col, rows)
		} else if err != nil {
			return err
		}
	case nuKindFloat64:
		if err := a.updateFloat64(col, rows); err == errNUniqueFallback {
			return a.updateBytes(col, rows)
		} else if err != nil {
			return err
		}
	default:
		return a.updateBytes(col, rows)
	}
	return nil
}

// errNUniqueFallback is an internal sentinel: the type-specialized
// Update saw a column shape it can't handle (e.g. a mid-stream batch
// has multiple chunks, or a mid-stream batch's chunk type doesn't
// match the specialization). Not a user-visible error — the caller
// retries via the byte-encoded generic path so the accumulator's
// final count remains correct even in mixed-shape streams.
var errNUniqueFallback = fmt.Errorf("nUniqueAcc: fallback to byte-encoded path")

// pickKind inspects col's first chunk and selects the specialization.
// Multi-chunk or unsupported types go to the byte-encoded fallback.
func (a *nUniqueAcc) pickKind(col Series) {
	chunks := col.col.Data().Chunks()
	if len(chunks) != 1 {
		a.kind = nuKindBytes
		a.seenBytes = make(map[string]struct{})
		return
	}
	switch chunks[0].(type) {
	case *array.String:
		a.kind = nuKindString
		a.seenStr = make(map[string]struct{})
	case *array.Int64:
		a.kind = nuKindInt64
		a.seenI64 = make(map[int64]struct{})
	case *array.Float64:
		a.kind = nuKindFloat64
		a.seenF64 = make(map[float64]struct{})
	default:
		a.kind = nuKindBytes
		a.seenBytes = make(map[string]struct{})
	}
}

func (a *nUniqueAcc) updateString(col Series, rows []int) error {
	chunks := col.col.Data().Chunks()
	if len(chunks) != 1 {
		return errNUniqueFallback
	}
	arr, ok := chunks[0].(*array.String)
	if !ok {
		return errNUniqueFallback
	}
	for _, row := range rows {
		if arr.IsNull(row) {
			continue
		}
		v := arr.Value(row)
		if _, seen := a.seenStr[v]; !seen {
			// arr.Value aliases into the batch's arrow buffer, which
			// is Released before Finalize walks the map. Clone only
			// on first-seen so the alloc is O(distinct-values), not
			// O(rows).
			a.seenStr[strings.Clone(v)] = struct{}{}
		}
	}
	return nil
}

func (a *nUniqueAcc) updateInt64(col Series, rows []int) error {
	chunks := col.col.Data().Chunks()
	if len(chunks) != 1 {
		return errNUniqueFallback
	}
	arr, ok := chunks[0].(*array.Int64)
	if !ok {
		return errNUniqueFallback
	}
	values := arr.Int64Values()
	for _, row := range rows {
		if arr.IsNull(row) {
			continue
		}
		a.seenI64[values[row]] = struct{}{}
	}
	return nil
}

func (a *nUniqueAcc) updateFloat64(col Series, rows []int) error {
	chunks := col.col.Data().Chunks()
	if len(chunks) != 1 {
		return errNUniqueFallback
	}
	arr, ok := chunks[0].(*array.Float64)
	if !ok {
		return errNUniqueFallback
	}
	values := arr.Float64Values()
	for _, row := range rows {
		if arr.IsNull(row) {
			continue
		}
		a.seenF64[values[row]] = struct{}{}
	}
	return nil
}

// updateBytes is the generic path: byte-encode each value via
// keyOfAppend + insert into a map[string] keyed on the encoding.
// Matches GroupBy's own key encoding so bit-equal numerics collapse
// identically to how the group column would collapse them.
func (a *nUniqueAcc) updateBytes(col Series, rows []int) error {
	if a.seenBytes == nil {
		a.seenBytes = make(map[string]struct{})
	}
	for _, row := range rows {
		null, err := isNullAtSeries(col, row)
		if err != nil {
			return err
		}
		if null {
			continue
		}
		buf, err := keyOfAppend(a.scratch[:0], col, row)
		if err != nil {
			return err
		}
		a.scratch = buf
		if _, ok := a.seenBytes[string(buf)]; !ok {
			a.seenBytes[string(buf)] = struct{}{}
		}
	}
	return nil
}

func (a *nUniqueAcc) Finalize() any {
	switch a.kind {
	case nuKindString:
		return int64(len(a.seenStr))
	case nuKindInt64:
		return int64(len(a.seenI64))
	case nuKindFloat64:
		return int64(len(a.seenF64))
	default:
		return int64(len(a.seenBytes))
	}
}
func (a *nUniqueAcc) OutputType() arrow.DataType { return arrow.PrimitiveTypes.Int64 }

// medianAcc buffers every non-null numeric value in the group and
// finalizes to the sample median (Float64). On even-sized groups
// the two middle values are averaged; on odd-sized groups the
// single middle value is returned. Empty groups (or all-null
// groups) finalize to nil (null in output).
//
// Memory is O(rows-per-group) — no way to compute an exact median
// in bounded space. For approximate-quantile workloads the future
// t-digest aggregator will trade exactness for memory.
type medianAcc struct {
	values []float64
}

func (a *medianAcc) Update(col Series, rows []int) error {
	for _, row := range rows {
		v, ok, err := col.numericAt(row)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		a.values = append(a.values, v)
	}
	return nil
}

func (a *medianAcc) Finalize() any {
	n := len(a.values)
	if n == 0 {
		return nil
	}
	sort.Float64s(a.values)
	if n%2 == 1 {
		return a.values[n/2]
	}
	return (a.values[n/2-1] + a.values[n/2]) / 2
}
func (a *medianAcc) OutputType() arrow.DataType { return arrow.PrimitiveTypes.Float64 }

// modeAcc tracks per-value counts and finalizes to the most-frequent
// non-null value. Ties are broken by first-seen order (deterministic
// across runs on the same input). Empty groups or all-null groups
// finalize to nil.
//
// The captured value's type follows the source column; the schema
// declares that type at Compile time so buildResultBatch's builder
// matches (same pattern as firstLastAcc).
//
// Memory is O(distinct-values-per-group) — no different from
// nUniqueAcc in that respect.
type modeAcc struct {
	counts   map[string]int64
	firstIdx map[string]int64
	// Parallel arrays: values[i] is the first-seen scalar for the
	// key whose encoded bytes hash-collided into counts[keys[i]].
	// firstIdx[key] indexes into values.
	values  []any
	scratch []byte
	nextIdx int64
}

func (a *modeAcc) Update(col Series, rows []int) error {
	if a.firstIdx == nil {
		a.firstIdx = make(map[string]int64)
	}
	for _, row := range rows {
		null, err := isNullAtSeries(col, row)
		if err != nil {
			return err
		}
		if null {
			continue
		}
		buf, err := keyOfAppend(a.scratch[:0], col, row)
		if err != nil {
			return err
		}
		a.scratch = buf
		key := string(buf)
		if _, ok := a.firstIdx[key]; !ok {
			v, err := readScalarAt(col, row)
			if err != nil {
				return err
			}
			a.firstIdx[key] = a.nextIdx
			a.values = append(a.values, v)
			a.nextIdx++
		}
		a.counts[key]++
	}
	return nil
}

func (a *modeAcc) Finalize() any {
	if len(a.counts) == 0 {
		return nil
	}
	var bestCount int64 = -1
	var bestIdx int64 = math.MaxInt64
	for k, c := range a.counts {
		idx := a.firstIdx[k]
		if c > bestCount || (c == bestCount && idx < bestIdx) {
			bestCount = c
			bestIdx = idx
		}
	}
	return a.values[bestIdx]
}

// OutputType is a fallback — the schema field type takes precedence
// in buildResultBatch (mode preserves the source column type).
func (a *modeAcc) OutputType() arrow.DataType { return arrow.PrimitiveTypes.Float64 }

// isNullAtSeries: null-check for a row without knowing the column
// type. Used by countAcc when the source is non-numeric (e.g.
// counting non-null string values).
func isNullAtSeries(s Series, row int) (bool, error) {
	if s.col == nil {
		return false, fmt.Errorf("isNullAtSeries: nil column")
	}
	chunk, local, ok := locateRowInChunks(s.col.Data().Chunks(), row)
	if !ok {
		return false, fmt.Errorf("isNullAtSeries: row %d out of range", row)
	}
	// isNullArr, not chunk.IsNull: a dictionary cell whose index is
	// valid but points at a null entry is null, and readScalarAt
	// returns nil for it. Callers that check null then read must see
	// the two agree.
	return isNullArr(chunk, local), nil
}
