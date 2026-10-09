package gobi

import (
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// -----------------------------------------------------------------------------
// Time-based rolling windows over a Frame.
//
// A "period" is a time.Duration. At each row i, the window is the set of
// rows j <= i whose time is within (t[i] - period, t[i]]. This is the
// standard "trailing window" convention (right-anchored, exclusive left).
// The time column must be sorted non-decreasing; otherwise RollingBy
// returns ErrNotMonotonic.
// -----------------------------------------------------------------------------

// TimeRolling represents a right-anchored, time-based rolling window over
// a Frame. Use Frame.RollingBy to construct one.
type TimeRolling struct {
	frame   *Frame
	timeCol string
	period  time.Duration
	// bucketStarts[i] and bucketEnds[i] delimit the source-row range that
	// row i's window contains: rows in [start, end] inclusive. Populated
	// lazily on the first Agg call.
	starts []int
}

// -----------------------------------------------------------------------------
// Fixed-size (integer) rolling windows on a single Series.
//
// The window at row i covers rows [i-window+1 .. i]. Rows before the first
// full window are emitted as null. Nulls inside the window are excluded
// from the aggregation (min/max ignore them, count returns non-null count,
// mean divides by the non-null count only). If every row in a window is
// null, the output at that position is null.
// -----------------------------------------------------------------------------

// RollingSum returns a Float64 Series where row i is the sum of s over
// the closed window [i-window+1 .. i]. window must be >= 1.
func (s Series) RollingSum(window int) (Series, error) {
	return s.rollingReduce(window, func(vals []float64, valid []bool) (float64, bool) {
		var total float64
		any := false
		for i, v := range vals {
			if !valid[i] {
				continue
			}
			total += v
			any = true
		}
		return total, any
	})
}

// RollingMean returns a Float64 Series holding the arithmetic mean of s
// over each window (non-null values only).
func (s Series) RollingMean(window int) (Series, error) {
	return s.rollingReduce(window, func(vals []float64, valid []bool) (float64, bool) {
		var total float64
		var n int
		for i, v := range vals {
			if !valid[i] {
				continue
			}
			total += v
			n++
		}
		if n == 0 {
			return 0, false
		}
		return total / float64(n), true
	})
}

// RollingMin returns a Float64 Series holding the minimum non-null value
// over each window.
func (s Series) RollingMin(window int) (Series, error) {
	return s.rollingReduce(window, func(vals []float64, valid []bool) (float64, bool) {
		m := math.Inf(1)
		any := false
		for i, v := range vals {
			if !valid[i] {
				continue
			}
			any = true
			if v < m {
				m = v
			}
		}
		return m, any
	})
}

// RollingMax returns a Float64 Series holding the maximum non-null value
// over each window.
func (s Series) RollingMax(window int) (Series, error) {
	return s.rollingReduce(window, func(vals []float64, valid []bool) (float64, bool) {
		m := math.Inf(-1)
		any := false
		for i, v := range vals {
			if !valid[i] {
				continue
			}
			any = true
			if v > m {
				m = v
			}
		}
		return m, any
	})
}

// RollingCount returns an Int64 Series holding the count of non-null
// values in each window. Rows before the first full window are null.
func (s Series) RollingCount(window int) (Series, error) {
	if !s.isNumeric() {
		return Series{}, ErrNotNumeric
	}
	if window < 1 {
		return Series{}, fmt.Errorf("gobi: rolling window must be >= 1, got %d", window)
	}
	n := s.Len()
	out := make([]int64, n)
	validity := make([]bool, n)
	// Slide the window and count valid entries.
	for i := range n {
		if i+1 < window {
			continue
		}
		start := i - window + 1
		var c int64
		for j := start; j <= i; j++ {
			if _, ok, err := s.numericAt(j); err != nil {
				return Series{}, err
			} else if ok {
				c++
			}
		}
		out[i] = c
		validity[i] = true
	}
	return buildInt64Series(s.name+"_rolling_count", out, validity), nil
}

// rollingReduce is the shared driver for RollingSum/Mean/Min/Max. Extracts
// the values buffer once, walks the window, applies the reducer.
func (s Series) rollingReduce(window int, reduce func(vals []float64, valid []bool) (float64, bool)) (Series, error) {
	if !s.isNumeric() {
		return Series{}, ErrNotNumeric
	}
	if window < 1 {
		return Series{}, fmt.Errorf("gobi: rolling window must be >= 1, got %d", window)
	}
	n := s.Len()
	// Materialize into a []float64 + []bool once — cheaper than calling
	// numericAt window times per row for the fast path.
	vals, valid, err := materializeF64(s)
	if err != nil {
		return Series{}, err
	}
	out := make([]float64, n)
	validity := make([]bool, n)
	winVals := make([]float64, window)
	winValid := make([]bool, window)
	for i := range n {
		if i+1 < window {
			continue
		}
		start := i - window + 1
		copy(winVals, vals[start:i+1])
		copy(winValid, valid[start:i+1])
		v, ok := reduce(winVals[:i+1-start], winValid[:i+1-start])
		if ok {
			out[i] = v
			validity[i] = true
		}
	}
	return buildFloat64Series(s.name+"_rolling", out, validity), nil
}

// materializeF64 extracts s's values into (vals, valid) — a []float64
// buffer plus a per-row validity slice. Handles both single-chunk fast
// paths and the general per-row path.
func materializeF64(s Series) ([]float64, []bool, error) {
	n := s.Len()
	vals := make([]float64, n)
	valid := make([]bool, n)
	if a, arr, ok := s.singleF64(); ok {
		copy(vals, a)
		if arr.NullN() == 0 {
			for i := range valid {
				valid[i] = true
			}
		} else {
			for i := range n {
				valid[i] = !arr.IsNull(i)
			}
		}
		return vals, valid, nil
	}
	if a, arr, ok := s.singleI64(); ok {
		for i, v := range a {
			vals[i] = float64(v)
		}
		if arr.NullN() == 0 {
			for i := range valid {
				valid[i] = true
			}
		} else {
			for i := range n {
				valid[i] = !arr.IsNull(i)
			}
		}
		return vals, valid, nil
	}
	for i := range n {
		v, ok, err := s.numericAt(i)
		if err != nil {
			return nil, nil, err
		}
		if !ok {
			continue
		}
		vals[i] = v
		valid[i] = true
	}
	return vals, valid, nil
}

// RollingBy returns a TimeRolling. The time column must be a Timestamp
// series in non-decreasing order.
func (f *Frame) RollingBy(timeCol string, period time.Duration) (*TimeRolling, error) {
	if period <= 0 {
		return nil, fmt.Errorf("gobi: RollingBy period must be > 0, got %v", period)
	}
	timeSer, err := f.Column(timeCol)
	if err != nil {
		return nil, err
	}
	if !timeSer.IsDateTime() {
		return nil, fmt.Errorf("%w: column %q", ErrNotDateTime, timeCol)
	}
	view, ok := viewTimestamp(timeSer)
	if !ok {
		return nil, fmt.Errorf("gobi: RollingBy requires a single-chunk timestamp column")
	}
	n := timeSer.Len()
	// Precompute the window's left boundary (first included row) for each row i.
	starts := make([]int, n)
	periodNs := int64(period)
	var lastNs int64
	monotonic := true
	left := 0
	for i := range n {
		t, valid := view.at(i)
		if !valid {
			// null time makes the ordering assumption impossible to check;
			// treat this row's window as empty by leaving start > i.
			starts[i] = i + 1
			continue
		}
		ns := t.UnixNano()
		if i > 0 && ns < lastNs {
			monotonic = false
		}
		lastNs = ns
		// Advance left pointer while values are older than ns - period.
		for left <= i {
			lt, lv := view.at(left)
			if !lv {
				left++
				continue
			}
			if ns-lt.UnixNano() > periodNs {
				left++
				continue
			}
			break
		}
		starts[i] = left
	}
	if !monotonic {
		return nil, ErrNotMonotonic
	}
	return &TimeRolling{frame: f, timeCol: timeCol, period: period, starts: starts}, nil
}

// Agg computes a single aggregation over each row's trailing window and
// returns a Series of the aggregated values. Rows whose window is empty
// (e.g. all-null time or empty column) produce nulls.
func (r *TimeRolling) Agg(column string, kind AggKind) (Series, error) {
	switch kind {
	case AggCount, AggSum, AggMean, AggMin, AggMax:
	default:
		// Checked up front: the per-row switch below only runs for
		// non-empty windows, so an unsupported kind on all-empty
		// input would otherwise return nulls instead of an error.
		return Series{}, fmt.Errorf("gobi: rolling agg kind %v not supported", kind)
	}
	src, err := r.frame.Column(column)
	if err != nil {
		return Series{}, err
	}
	if kind != AggCount && !src.isNumeric() {
		return Series{}, fmt.Errorf("%w: %s", ErrNotNumeric, column)
	}
	vals, valid, err := materializeF64(src)
	if err != nil && kind != AggCount {
		return Series{}, err
	}
	n := src.Len()
	if kind == AggCount {
		out := make([]int64, n)
		validity := make([]bool, n)
		for i := range n {
			start := r.starts[i]
			if start > i {
				continue
			}
			var c int64
			for j := start; j <= i; j++ {
				if valid != nil && valid[j] {
					c++
				}
			}
			out[i] = c
			validity[i] = true
		}
		return buildInt64Series(column+"_rolling_count", out, validity), nil
	}
	out := make([]float64, n)
	validity := make([]bool, n)
	for i := range n {
		start := r.starts[i]
		if start > i {
			continue
		}
		var (
			total, mn, mx float64
			count         int
		)
		mn = math.Inf(1)
		mx = math.Inf(-1)
		for j := start; j <= i; j++ {
			if !valid[j] {
				continue
			}
			v := vals[j]
			total += v
			if v < mn {
				mn = v
			}
			if v > mx {
				mx = v
			}
			count++
		}
		if count == 0 {
			continue
		}
		switch kind {
		case AggSum:
			out[i] = total
		case AggMean:
			out[i] = total / float64(count)
		case AggMin:
			out[i] = mn
		case AggMax:
			out[i] = mx
		default:
			return Series{}, fmt.Errorf("gobi: rolling agg kind %v not supported", kind)
		}
		validity[i] = true
	}
	return buildFloat64Series(column+"_rolling", out, validity), nil
}

// -----------------------------------------------------------------------------
// arrow-package glue kept local to this file: currently unused but here
// so future rolling ops can grow without touching series_ops.go.
// -----------------------------------------------------------------------------

var _ = arrow.Nanosecond
var _ = array.NewFloat64Builder
var _ = memory.DefaultAllocator

// ErrNotMonotonic is returned by ops that require a time column sorted in
// non-decreasing order (currently: Frame.RollingBy).
var ErrNotMonotonic = fmt.Errorf("gobi: time column must be monotonically non-decreasing")

// Resampler bins rows of a Frame by fixed time intervals aligned to the
// Unix epoch. Use Frame.ResampleEvery to construct one, then call Agg to
// produce a downsampled Frame.
//
// Bucket alignment: bucketStart = floor(unix_ns / interval_ns) * interval_ns.
// A 1-hour resample produces buckets that start on the hour in UTC —
// regardless of any timezone label on the input series. If you need
// calendar-aligned resampling (e.g. daily buckets that start at midnight
// New York time), truncate to a calendar boundary first and group on that.
type Resampler struct {
	frame    *Frame
	timeCol  string
	interval time.Duration
}

// ResampleEvery returns a Resampler that will group f's rows into
// interval-wide buckets by the values in timeCol. The interval must be
// positive.
func (f *Frame) ResampleEvery(timeCol string, interval time.Duration) (*Resampler, error) {
	if interval <= 0 {
		return nil, fmt.Errorf("gobi: ResampleEvery interval must be > 0, got %v", interval)
	}
	s, err := f.Column(timeCol)
	if err != nil {
		return nil, err
	}
	if !s.IsDateTime() {
		return nil, fmt.Errorf("%w: column %q", ErrNotDateTime, timeCol)
	}
	return &Resampler{frame: f, timeCol: timeCol, interval: interval}, nil
}

// Agg computes the requested aggregations over each non-empty bucket. The
// output frame has one row per bucket, ordered by bucket start, with the
// time column named the same as the input's time column followed by one
// column per aggregation. Empty buckets are omitted (this mirrors
// GroupBy's behavior).
func (r *Resampler) Agg(aggs ...Aggregation) (*Frame, error) {
	timeSer, _ := r.frame.Column(r.timeCol)
	tsView, ok := viewTimestamp(timeSer)
	if !ok {
		return nil, fmt.Errorf("gobi: ResampleEvery requires a single-chunk timestamp column")
	}

	// Bucket each row. bucket key is the bucket-start nanoseconds.
	n := timeSer.Len()
	intervalNs := int64(r.interval)
	buckets := make(map[int64][]int, 64)
	var order []int64
	for i := 0; i < n; i++ {
		t, valid := tsView.at(i)
		if !valid {
			// null time value → skip (not put in any bucket)
			continue
		}
		ns := t.UnixNano()
		floor := (ns / intervalNs) * intervalNs
		if floor > ns { // negative timestamp integer-truncation correction
			floor -= intervalNs
		}
		if _, seen := buckets[floor]; !seen {
			order = append(order, floor)
		}
		buckets[floor] = append(buckets[floor], i)
	}
	slices.Sort(order)

	// Pre-extract views for the aggregation columns. Resample restricts
	// callers to numeric aggregations — Timestamp Min/Max + NUnique are
	// reachable through the fast-path aggView builder but not through
	// this entry point today. Reject non-numeric shapes explicitly
	// rather than silently mis-dispatch.
	aggViews := make([]aggView, len(aggs))
	for i, a := range aggs {
		v, ok, err := buildAggView(r.frame, a)
		if err != nil {
			return nil, err
		}
		if !ok || (v.kind != aggViewCountStar && v.kind != aggViewNumeric) {
			return nil, fmt.Errorf("gobi: ResampleEvery.Agg requires single-chunk numeric aggregation column, %q is not", a.Column)
		}
		aggViews[i] = v
	}

	pool := memory.DefaultAllocator
	// Emit bucket-start timestamps in the same tz as the input.
	origType := timeSer.DataType().(*arrow.TimestampType)
	tsType := &arrow.TimestampType{Unit: arrow.Nanosecond, TimeZone: origType.TimeZone}
	tsB := array.NewTimestampBuilder(pool, tsType)
	defer tsB.Release()

	aggBs, aggFields := makeAggBuilders(pool, aggs, aggViews)
	defer releaseBuilders(aggBs)

	for _, bucketStart := range order {
		tsB.Append(arrow.Timestamp(bucketStart))
		rows := buckets[bucketStart]
		for i, a := range aggs {
			appendFastAgg(aggBs[i], a, aggViews[i], rows)
		}
	}

	// Assemble output frame.
	fields := make([]arrow.Field, 0, 1+len(aggFields))
	fields = append(fields, arrow.Field{Name: r.timeCol, Type: tsType, Nullable: false})
	fields = append(fields, aggFields...)

	tsArr := tsB.NewArray()
	defer tsArr.Release()
	aggArrs := make([]arrow.Array, len(aggBs))
	for i, b := range aggBs {
		aggArrs[i] = b.NewArray()
	}
	defer func() {
		for _, a := range aggArrs {
			a.Release()
		}
	}()

	schema := arrow.NewSchema(fields, nil)
	cols := make([]arrow.Column, len(fields))
	tsChunked := arrow.NewChunked(tsArr.DataType(), []arrow.Array{tsArr})
	cols[0] = *arrow.NewColumn(fields[0], tsChunked)
	tsChunked.Release()
	for i, a := range aggArrs {
		c := arrow.NewChunked(a.DataType(), []arrow.Array{a})
		cols[i+1] = *arrow.NewColumn(fields[i+1], c)
		c.Release()
	}
	return NewFrame(schema, cols)
}
