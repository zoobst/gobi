package pgio

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/zoobst/gobi"
)

// TestTimestampColumns_Microseconds — timestamp, timestamptz and date
// read as Timestamp[µs], PostgreSQL's own precision, so a 9999-12-31
// sentinel (or year 1) reads exactly; past the µs range is an error,
// not a wrapped value.
func TestTimestampColumns_Microseconds(t *testing.T) {
	for _, oid := range []uint32{pgtype.TimestampOID, pgtype.TimestamptzOID, pgtype.DateOID} {
		dt, err := oidToArrowType(oid)
		if err != nil {
			t.Fatal(err)
		}
		if dt.String() != "timestamp[us, tz=UTC]" {
			t.Errorf("OID %d → %s, want timestamp[us, tz=UTC]", oid, dt)
		}
	}
	b := array.NewTimestampBuilder(memory.DefaultAllocator, arrow.FixedWidthTypes.Timestamp_us.(*arrow.TimestampType))
	defer b.Release()
	in := []time.Time{
		time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC),
		time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	for _, v := range in {
		if err := appendPGValue(b, v); err != nil {
			t.Fatal(err)
		}
	}
	arr := b.NewTimestampArray()
	defer arr.Release()
	for i, want := range in {
		if got := arr.Value(i).ToTime(arrow.Microsecond); !got.Equal(want) {
			t.Errorf("row %d = %s, want %s", i, got, want)
		}
	}
	// PostgreSQL's top years (to 294276 AD, counted from 2000) are
	// past what int64 µs from 1970 reaches.
	if err := appendPGValue(b, time.Unix(math.MaxInt64/1_000_000+1, 0)); !errors.Is(err, gobi.ErrTimeOutOfRange) {
		t.Errorf("past µs range: err = %v, want ErrTimeOutOfRange", err)
	}
}
