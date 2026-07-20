package gobi

import (
	"fmt"
	"slices"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

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

	// Pre-extract numeric views for the aggregation columns.
	aggViews := make([]numericView, len(aggs))
	for i, a := range aggs {
		if a.Kind == AggCount && a.Column == "" {
			continue
		}
		colS, err := r.frame.Column(a.Column)
		if err != nil {
			return nil, err
		}
		v, ok := viewNumeric(colS)
		if !ok {
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

	aggBs, aggFields := makeAggBuilders(pool, aggs)
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
	for i, a := range aggArrs {
		c := arrow.NewChunked(a.DataType(), []arrow.Array{a})
		cols[i+1] = *arrow.NewColumn(fields[i+1], c)
	}
	return NewFrame(schema, cols)
}
