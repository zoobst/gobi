package gobi

import (
	"context"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// streamingAggregateExec is a native streaming hash aggregate.
//
// Pulls its input one batch at a time, maintaining an in-memory map
// from composite key bytes → per-group accumulator state. Rows are
// consumed as batches arrive; no accumulated input is held in memory
// beyond the (typically small) result hash table. On EOF, iterates
// groups in deterministic key order and emits one output row per
// group.
//
// Only handles Aggregations with built-in Kinds (Count / Sum / Mean /
// Min / Max). Custom Fn Aggregators require the whole input at once
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

	built  bool
	groups map[string]*aggGroup // final merged groups (keyModeString1 + keyModeComposite)
	order  []string             // sorted keys for deterministic output
	// int64-keyed parallel state, used when keyMode == keyModeInt641.
	// Kept as a separate map (rather than shoehorning ints into
	// `groups`) so the fast path pays no string-conversion cost and
	// so Go's runtime picks its native int64-hash intrinsics.
	groupsInt64 map[int64]*aggGroup
	orderInt64  []int64

	// keyScratch is a reusable []byte the serial build path grows
	// once and rewrites per row via composeCompositeKeyInto. Combined
	// with the map[string(scratch)] compiler idiom, this cuts per-row
	// key allocations from ~2/row to ~1/new-group.
	keyScratch []byte

	// dispatchScratch is the parallel reader's per-row composite key
	// buffer. Only the reader goroutine touches it, so no
	// synchronization is needed. Workers do not use this — they
	// recompute keys off the batch with their own local scratch on
	// the receive side, so nothing key-shaped crosses the channel.
	dispatchScratch []byte
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
// each adds a specialized branch to consumeBatch / workerConsume
// and dispatchBatch.
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
	// is used directly as the map key (probe zero-copy via the
	// compiler's map[stringVar] optimization; insert clones for
	// durability past the batch's lifetime).
	keyModeString1
	// keyModeInt641 is the specialized single-column integer-ish
	// path. Handles a single Int64, Int32, Uint64, Uint32,
	// Timestamp, or Bool key — every one fits losslessly in an
	// int64 for hash-table purposes. Skips the composite byte
	// encoding entirely: the arrow value is read once per row and
	// used directly as a `map[int64]*aggGroup` key. Faster hash
	// (word-at-a-time vs byte-loop) and no scratch allocation.
	keyModeInt641
)

// aggGroup holds one group's key values and one accumulator per
// aggregation. Key values are the raw Go scalars captured when the
// group was first seen — appended into the output when the result
// is emitted.
type aggGroup struct {
	keyVals []any            // one per key column
	accs    []aggAccumulator // one per Aggregation
}

// aggAccumulator is the streaming counterpart to the built-in Aggregation
// Kinds. Update() consumes a batch's rows for one aggregation column;
// Finalize() produces the group's aggregated value.
type aggAccumulator interface {
	// Update accepts a full column and the row indices within it
	// that belong to the current group. Called at most once per
	// input batch per group.
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

	if e.workers > 1 {
		if err := e.buildParallel(ctx); err != nil {
			return err
		}
	} else {
		if err := e.buildSerial(ctx); err != nil {
			return err
		}
	}
	rb, err := e.buildResultBatch()
	if err != nil {
		return err
	}
	e.resultBatch = rb
	return nil
}

// buildSerial is the original single-goroutine, single-map build.
// Kept as the fast path for workers <= 1 — the partitioned path pays
// for goroutine + channel + hash overhead that isn't worth it when
// there's no CPU parallelism to unlock.
func (e *streamingAggregateExec) buildSerial(ctx context.Context) error {
	// Initialize the group container that matches the chosen
	// keyMode. keyModeInt641 uses e.groupsInt64 + e.orderInt64;
	// every other mode uses e.groups + e.order. consumeBatch
	// dispatches on the same enum.
	if e.keyMode == keyModeInt641 {
		e.groupsInt64 = make(map[int64]*aggGroup)
	} else {
		e.groups = make(map[string]*aggGroup)
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		batch, err := e.input.Next(ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if err := e.consumeBatch(batch); err != nil {
			batch.Release()
			return err
		}
		batch.Release()
	}
	if e.keyMode == keyModeInt641 {
		sort.Slice(e.orderInt64, func(i, j int) bool { return e.orderInt64[i] < e.orderInt64[j] })
	} else {
		sort.Strings(e.order)
	}
	return nil
}

// consumeBatch buckets one batch's rows by composite key, then updates
// each affected group's accumulators for each aggregation.
//
// Hot path: uses e.keyScratch as a reusable []byte for composite key
// encoding, and the compiler-recognized `map[string(scratch)]` idiom
// to probe the group map without allocating per row. The only
// per-new-group allocation is the `string(scratch)` conversion at
// first-touch, which forces the heap copy that guarantees the map
// key outlives future scratch overwrites.
func (e *streamingAggregateExec) consumeBatch(batch arrow.RecordBatch) error {
	if batch.NumRows() == 0 {
		return nil
	}
	frame, err := batchToFrame(batch)
	if err != nil {
		return err
	}

	// Resolve key columns and per-aggregation source columns once
	// for this batch.
	keySeries := make([]Series, len(e.keys))
	for i, k := range e.keys {
		s, err := frame.Column(k)
		if err != nil {
			return err
		}
		keySeries[i] = s
	}
	aggCols := make([]Series, len(e.aggs))
	for i, a := range e.aggs {
		if a.Column == "" {
			continue // Count-star: no source column needed
		}
		s, err := frame.Column(a.Column)
		if err != nil {
			return err
		}
		aggCols[i] = s
	}

	// Bucket rows by group pointer. Three paths depending on keyMode:
	//   - keyModeString1: read the arrow value directly, use as
	//     `map[string]*aggGroup` key. Skips composite encoding.
	//   - keyModeInt641: widen the arrow value to int64, use as
	//     `map[int64]*aggGroup` key. Skips composite encoding AND
	//     the string-hash path — Go's runtime uses word-at-a-time
	//     hashing on int keys.
	//   - keyModeComposite: build encoded bytes into e.keyScratch,
	//     use string(scratch) as `map[string]*aggGroup` key.
	//
	// All three paths use *aggGroup as the bucket key so the per-row
	// map write doesn't allocate a string (the compiler's
	// map[string(bytes)] optimization only applies to reads).
	buckets := make(map[*aggGroup][]int)
	rows := int(batch.NumRows())
	switch e.keyMode {
	case keyModeString1:
		strArr, err := resolveStringArray(keySeries[0])
		if err != nil {
			return err
		}
		for row := 0; row < rows; row++ {
			s := strArr.value(row) // zero-copy alias to arrow buffer
			g, ok := e.groups[s]
			if !ok {
				// strings.Clone forces a durable heap copy that
				// outlives the batch's release; needed because Go
				// stores string headers, not underlying bytes.
				ks := strings.Clone(s)
				g, err = newAggGroupString1(ks, e.aggs)
				if err != nil {
					return err
				}
				e.groups[ks] = g
				e.order = append(e.order, ks)
			}
			buckets[g] = append(buckets[g], row)
		}
	case keyModeInt641:
		intArr, err := resolveIntArray(keySeries[0])
		if err != nil {
			return err
		}
		if e.groupsInt64 == nil {
			e.groupsInt64 = make(map[int64]*aggGroup)
		}
		for row := 0; row < rows; row++ {
			k, ok := intArr.value(row)
			if !ok {
				// Skip null-keyed rows in the fast path. The int64
				// map key can't losslessly represent "null" — using
				// a sentinel would collide with a real value like
				// math.MinInt64. Composite encoding does support a
				// null-key group (via the 0x00 tag) but users who
				// need that should GroupBy(...) on a column that
				// routes to the composite path (e.g. by adding a
				// second key column).
				continue
			}
			g, ok := e.groupsInt64[k]
			if !ok {
				// Use the generic newAggGroup so the group's
				// keyVals stores the source column's arrow-typed
				// scalar (Int32, Timestamp, Bool, etc.), not the
				// widened int64. That's what appendCustomValue
				// expects at buildResultBatch time — the output
				// key column matches the input type.
				g, err = newAggGroup(keySeries, row, e.aggs)
				if err != nil {
					return err
				}
				e.groupsInt64[k] = g
				e.orderInt64 = append(e.orderInt64, k)
			}
			buckets[g] = append(buckets[g], row)
		}
	default:
		for row := 0; row < rows; row++ {
			scratch, err := composeCompositeKeyInto(e.keyScratch[:0], keySeries, row)
			if err != nil {
				return err
			}
			e.keyScratch = scratch
			g, ok := e.groups[string(scratch)]
			if !ok {
				g, err = newAggGroup(keySeries, row, e.aggs)
				if err != nil {
					return err
				}
				ks := string(scratch)
				e.groups[ks] = g
				e.order = append(e.order, ks)
			}
			buckets[g] = append(buckets[g], row)
		}
	}

	// Update each group's accumulators with its bucket's rows.
	for g, groupRows := range buckets {
		for i, a := range e.aggs {
			src := aggCols[i]
			if a.Column == "" {
				// Count-star: accumulator ignores col, uses len(rows).
				if err := g.accs[i].Update(Series{}, groupRows); err != nil {
					return err
				}
				continue
			}
			if err := g.accs[i].Update(src, groupRows); err != nil {
				return err
			}
		}
	}
	return nil
}

// composeCompositeKey builds a byte-encoded composite of multi-column
// key values for a single row. Reuses the same encoding as
// GroupBy.rowKey so the streaming and eager engines agree on group
// identity (important for keeping test expectations aligned).
//
// Allocates a fresh []byte per call. The streaming aggregate hot
// paths use composeCompositeKeyInto with a reusable scratch buffer
// instead; keep this thin wrapper for the eager/tests callers that
// don't need to bother with scratch management.
func composeCompositeKey(keys []Series, row int) ([]byte, error) {
	return composeCompositeKeyInto(nil, keys, row)
}

// composeCompositeKeyInto appends the byte-encoded composite key
// for row into dst and returns the resulting slice. Byte-for-byte
// identical to composeCompositeKey(keys, row); callers pass
// scratch[:0] to reuse a buffer across rows.
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

// newAggGroupString1 is the specialized first-touch group
// constructor for keyModeString1. Skips the readScalarAt round-trip
// through the generic keyVals []any — the caller already has the
// key string (from strings.Clone) so we just capture it directly.
func newAggGroupString1(key string, aggs []Aggregation) (*aggGroup, error) {
	accs := make([]aggAccumulator, len(aggs))
	for i, a := range aggs {
		acc, err := newAccumulator(a)
		if err != nil {
			return nil, err
		}
		accs[i] = acc
	}
	return &aggGroup{keyVals: []any{key}, accs: accs}, nil
}

// newAggGroup allocates a group's state on first-touch: captures the
// key values from the current row and constructs one accumulator per
// aggregation.
func newAggGroup(keys []Series, row int, aggs []Aggregation) (*aggGroup, error) {
	keyVals := make([]any, len(keys))
	for i, k := range keys {
		v, err := readScalarAt(k, row)
		if err != nil {
			return nil, err
		}
		keyVals[i] = v
	}
	accs := make([]aggAccumulator, len(aggs))
	for i, a := range aggs {
		acc, err := newAccumulator(a)
		if err != nil {
			return nil, err
		}
		accs[i] = acc
	}
	return &aggGroup{keyVals: keyVals, accs: accs}, nil
}

// newAccumulator constructs the accumulator for one Aggregation kind.
// Custom Fn accumulators are not supported here — the compiler routes
// custom aggs to the materializing fallback.
func newAccumulator(a Aggregation) (aggAccumulator, error) {
	if a.Fn != nil {
		return nil, fmt.Errorf("gobi: streaming aggregate does not support custom Aggregator Fn")
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
		return &nUniqueAcc{seen: make(map[string]struct{})}, nil
	}
	return nil, fmt.Errorf("gobi: streaming aggregate: unknown Kind %d", a.Kind)
}

// buildResultBatch produces the single result RecordBatch by iterating
// groups in sorted order and appending to per-column builders. Uses
// the same builder types as the existing eager Agg path so the two
// engines produce structurally identical output.
func (e *streamingAggregateExec) buildResultBatch() (arrow.RecordBatch, error) {
	pool := memory.DefaultAllocator

	// Key column builders — one per key column, typed by the input
	// schema's field type.
	keyBuilders := make([]array.Builder, len(e.keys))
	for i := range e.keys {
		field := e.outSchema.Field(i)
		b, err := builderForType(pool, field.Type)
		if err != nil {
			return nil, fmt.Errorf("key col %s: %w", e.keys[i], err)
		}
		keyBuilders[i] = b
	}
	defer releaseBuilders(keyBuilders)

	// Agg column builders — one per Aggregation, typed by the
	// accumulator's OutputType.
	aggBuilders := make([]array.Builder, len(e.aggs))
	for i, a := range e.aggs {
		// Use the schema's field type so builder + emit path agree.
		field := e.outSchema.Field(len(e.keys) + i)
		b, err := builderForType(pool, field.Type)
		if err != nil {
			return nil, fmt.Errorf("agg %s: %w", aggName(a), err)
		}
		aggBuilders[i] = b
	}
	defer releaseBuilders(aggBuilders)

	// Emit one row per group. Walks e.orderInt64 in the int64 fast
	// path, e.order in every other mode — the two containers are
	// mutually exclusive per keyMode selection.
	emitGroup := func(g *aggGroup) error {
		for i, b := range keyBuilders {
			if err := appendCustomValue(b, g.keyVals[i]); err != nil {
				return fmt.Errorf("emit key %s: %w", e.keys[i], err)
			}
		}
		for i, b := range aggBuilders {
			v := g.accs[i].Finalize()
			if err := appendCustomValue(b, v); err != nil {
				return fmt.Errorf("emit agg %s: %w", aggName(e.aggs[i]), err)
			}
		}
		return nil
	}
	if e.keyMode == keyModeInt641 {
		for _, k := range e.orderInt64 {
			if err := emitGroup(e.groupsInt64[k]); err != nil {
				return nil, err
			}
		}
	} else {
		for _, ks := range e.order {
			if err := emitGroup(e.groups[ks]); err != nil {
				return nil, err
			}
		}
	}

	// Materialize each builder as an arrow.Array and assemble a
	// RecordBatch.
	total := len(keyBuilders) + len(aggBuilders)
	arrs := make([]arrow.Array, 0, total)
	defer func() {
		for _, a := range arrs {
			a.Release()
		}
	}()
	for _, b := range keyBuilders {
		arrs = append(arrs, b.NewArray())
	}
	for _, b := range aggBuilders {
		arrs = append(arrs, b.NewArray())
	}
	nRows := int64(len(e.order))
	if e.keyMode == keyModeInt641 {
		nRows = int64(len(e.orderInt64))
	}
	// array.NewRecord retains each array; we defer-Release ours.
	return array.NewRecord(e.outSchema, arrs, nRows), nil
}

// readScalarAt extracts a Go-typed scalar from a Series at row for
// use as a key value. Matches the types builderForType and
// appendCustomValue expect.
func readScalarAt(s Series, row int) (any, error) {
	if s.col == nil {
		return nil, fmt.Errorf("readScalarAt: nil column")
	}
	offset := 0
	for _, chunk := range s.col.Data().Chunks() {
		if row < offset+chunk.Len() {
			local := row - offset
			if chunk.IsNull(local) {
				return nil, nil
			}
			switch a := chunk.(type) {
			case *array.Int64:
				return a.Value(local), nil
			case *array.Int32:
				return a.Value(local), nil
			case *array.Uint64:
				return a.Value(local), nil
			case *array.Uint32:
				return a.Value(local), nil
			case *array.Float64:
				return a.Value(local), nil
			case *array.Float32:
				return a.Value(local), nil
			case *array.Boolean:
				return a.Value(local), nil
			case *array.String:
				return a.Value(local), nil
			case *array.LargeString:
				return a.Value(local), nil
			case *array.Timestamp:
				return a.Value(local), nil
			}
			return nil, fmt.Errorf("readScalarAt: unsupported type %T", chunk)
		}
		offset += chunk.Len()
	}
	return nil, fmt.Errorf("readScalarAt: row %d out of range", row)
}

// -----------------------------------------------------------------------------
// Built-in accumulators
//
// Deliberately match the eager Agg's output types + null semantics so
// the two engines produce byte-identical output on the same input:
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

func (a *countAcc) Finalize() any             { return a.n }
func (a *countAcc) OutputType() arrow.DataType { return arrow.PrimitiveTypes.Int64 }

// sumAcc keeps a running sum of numeric values. Skips nulls. Empty
// group finalizes to nil (null in output).
type sumAcc struct {
	sum    float64
	seen   bool
}

func (a *sumAcc) Update(col Series, rows []int) error {
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
type minMaxAcc struct {
	isMin   bool
	extreme float64
	seen    bool
}

func (a *minMaxAcc) Update(col Series, rows []int) error {
	for _, row := range rows {
		v, ok, err := col.numericAt(row)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		if !a.seen {
			a.extreme = v
			a.seen = true
			continue
		}
		if a.isMin && v < a.extreme {
			a.extreme = v
		} else if !a.isMin && v > a.extreme {
			a.extreme = v
		}
	}
	return nil
}

func (a *minMaxAcc) Finalize() any {
	if !a.seen {
		return nil
	}
	return a.extreme
}
func (a *minMaxAcc) OutputType() arrow.DataType { return arrow.PrimitiveTypes.Float64 }

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
	wantStd    bool
	n          int64
	mean, m2   float64
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

// nUniqueAcc counts distinct non-null values seen. Uses the same
// keyOfAppend byte encoding as GroupBy itself so bit-equal numeric
// values collapse identically. Memory is O(distinct-values-per-group);
// on high-cardinality columns this can blow up — no different from
// polars / pandas nunique in that respect. The scratch buffer is
// per-acc (per-group) which is acceptable for typical group sizes.
type nUniqueAcc struct {
	seen    map[string]struct{}
	scratch []byte
}

func (a *nUniqueAcc) Update(col Series, rows []int) error {
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
		if _, ok := a.seen[string(buf)]; !ok {
			a.seen[string(buf)] = struct{}{}
		}
	}
	return nil
}

func (a *nUniqueAcc) Finalize() any             { return int64(len(a.seen)) }
func (a *nUniqueAcc) OutputType() arrow.DataType { return arrow.PrimitiveTypes.Int64 }

// isNullAtSeries: null-check for a row without knowing the column
// type. Used by countAcc when the source is non-numeric (e.g.
// counting non-null string values).
func isNullAtSeries(s Series, row int) (bool, error) {
	offset := 0
	for _, chunk := range s.col.Data().Chunks() {
		if row < offset+chunk.Len() {
			return chunk.IsNull(row - offset), nil
		}
		offset += chunk.Len()
	}
	return false, fmt.Errorf("isNullAtSeries: row %d out of range", row)
}
