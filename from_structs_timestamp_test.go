package gobi

import (
	"errors"
	"testing"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
)

// TestFromStructs_TimestampTag — parquet-go-style timestamp options
// pick the arrow unit + zone; untagged time.Time stays Timestamp[ns].
func TestFromStructs_TimestampTag(t *testing.T) {
	type row struct {
		Plain  time.Time   `parquet:"plain"`
		Bare   time.Time   `parquet:"bare,timestamp"`
		Micros time.Time   `parquet:"micros,timestamp(microsecond)"`
		Local  *time.Time  `parquet:"local,timestamp(nanosecond:local)"`
		UTC    time.Time   `parquet:"utc,timestamp(millisecond:utc)"`
		Str    string      `parquet:"str,timestamp(microsecond)" time:"2006-01-02T15:04:05.999999999Z07:00"`
		List   []time.Time `parquet:"list,timestamp(microsecond)"`
		Other  time.Time   `parquet:",timestamp(microsecond)"`  // empty name part
		Gobi   time.Time   `gobi:"gobi,timestamp(microsecond)"` // fallback namespace
	}
	ts := time.Date(2024, 3, 15, 9, 30, 0, 123456789, time.UTC)
	rows := []row{{
		Plain: ts, Bare: ts, Micros: ts, Local: &ts, UTC: ts,
		Str: ts.Format(time.RFC3339Nano), List: []time.Time{ts, ts.Add(time.Second)},
		Other: ts, Gobi: ts,
	}}
	f, err := FromStructs(rows, StructTagFormat("parquet"))
	if err != nil {
		t.Fatalf("FromStructs: %v", err)
	}
	defer f.Release()

	wantTypes := map[string]arrow.DataType{
		"plain":  &arrow.TimestampType{Unit: arrow.Nanosecond},
		"bare":   &arrow.TimestampType{Unit: arrow.Millisecond, TimeZone: "UTC"},
		"micros": &arrow.TimestampType{Unit: arrow.Microsecond, TimeZone: "UTC"},
		"local":  &arrow.TimestampType{Unit: arrow.Nanosecond},
		"utc":    &arrow.TimestampType{Unit: arrow.Millisecond, TimeZone: "UTC"},
		"str":    &arrow.TimestampType{Unit: arrow.Microsecond, TimeZone: "UTC"},
		"list":   arrow.ListOf(&arrow.TimestampType{Unit: arrow.Microsecond, TimeZone: "UTC"}),
		"Other":  &arrow.TimestampType{Unit: arrow.Microsecond, TimeZone: "UTC"},
		"gobi":   &arrow.TimestampType{Unit: arrow.Microsecond, TimeZone: "UTC"},
	}
	for name, want := range wantTypes {
		s, err := f.Column(name)
		if err != nil {
			t.Errorf("column %q: %v", name, err)
			continue
		}
		if !arrow.TypeEqual(s.DataType(), want) {
			t.Errorf("%s type = %s, want %s", name, s.DataType(), want)
		}
	}

	// Stored values are in the column's unit, truncated.
	micros, _ := f.Column("micros")
	if got := micros.Column().Data().Chunk(0).(*array.Timestamp).Value(0); int64(got) != ts.UnixMicro() {
		t.Errorf("micros raw = %d, want %d", got, ts.UnixMicro())
	}

	back, err := ToStructs[row](f, StructTagFormat("parquet"))
	if err != nil {
		t.Fatalf("ToStructs: %v", err)
	}
	b := back[0]
	us := ts.Truncate(time.Microsecond)
	ms := ts.Truncate(time.Millisecond)
	checks := []struct {
		name      string
		got, want time.Time
	}{
		{"plain", b.Plain, ts}, {"bare", b.Bare, ms}, {"micros", b.Micros, us},
		{"local", *b.Local, ts}, {"utc", b.UTC, ms}, {"Other", b.Other, us}, {"gobi", b.Gobi, us},
		{"list[0]", b.List[0], us}, {"list[1]", b.List[1], us.Add(time.Second)},
	}
	for _, c := range checks {
		if !c.got.Equal(c.want) {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
	if b.Str != us.Format(time.RFC3339Nano) {
		t.Errorf("str = %q, want %q", b.Str, us.Format(time.RFC3339Nano))
	}
}

func TestFromStructs_TimestampTagErrors(t *testing.T) {
	type badUnit struct {
		T time.Time `gobi:"t,timestamp(second)"`
	}
	type badZone struct {
		T time.Time `gobi:"t,timestamp(microsecond:pst)"`
	}
	type malformed struct {
		T time.Time `gobi:"t,timestamp(microsecond"`
	}
	if _, err := FromStructs([]badUnit{{}}); !errors.Is(err, ErrUnsupportedStructField) {
		t.Errorf("bad unit: err = %v", err)
	}
	if _, err := FromStructs([]badZone{{}}); !errors.Is(err, ErrUnsupportedStructField) {
		t.Errorf("bad zone: err = %v", err)
	}
	if _, err := FromStructs([]malformed{{}}); !errors.Is(err, ErrUnsupportedStructField) {
		t.Errorf("malformed: err = %v", err)
	}
}

// TestFromStructs_TimestampTagPastNanoRange — ms / us units cover
// dates outside int64-nanosecond range (1677-2262).
func TestFromStructs_TimestampTagPastNanoRange(t *testing.T) {
	type row struct {
		T time.Time `gobi:"t,timestamp(microsecond)"`
	}
	far := time.Date(2500, 1, 1, 0, 0, 0, 0, time.UTC)
	f, err := FromStructs([]row{{T: far}})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Release()
	back, err := ToStructs[row](f)
	if err != nil {
		t.Fatal(err)
	}
	if !back[0].T.Equal(far) {
		t.Errorf("T = %v, want %v", back[0].T, far)
	}
}
