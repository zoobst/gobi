package csvio_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/zoobst/gobi/csvio"
)

type event struct {
	Name string    `csv:"name"`
	When time.Time `csv:"when" time:"2006-01-02 15:04:05"`
	Seq  int64     `csv:"seq"`
}

const eventsCSV = `name,when,seq
launch,2026-01-15 09:30:00,1
demo,2026-03-22 14:00:00,2
release,2026-07-04 00:00:00,3
`

func TestRead_TimeColumn_WithExplicitLayout(t *testing.T) {
	df, err := csvio.Read[event](strings.NewReader(eventsCSV), nil)
	if err != nil {
		t.Fatal(err)
	}
	rows, cols := df.Shape()
	if rows != 3 || cols != 3 {
		t.Fatalf("shape got (%d, %d) want (3, 3)", rows, cols)
	}

	when, _ := df.Column("when")
	if !when.IsDateTime() {
		t.Fatal("when column not tagged as datetime")
	}
	got, ok, err := when.TimeAt(1)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("row 1 unexpectedly null")
	}
	want := time.Date(2026, 3, 22, 14, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("row 1 = %v, want %v", got, want)
	}
}

func TestRead_TimeColumn_DefaultLayoutsFallback(t *testing.T) {
	type simpleEvt struct {
		When time.Time `csv:"when"`
	}
	// Feed an RFC3339 timestamp without an explicit layout — should fall
	// back to DefaultTimeLayouts.
	src := "when\n2026-05-01T12:34:56Z\n"
	df, err := csvio.Read[simpleEvt](strings.NewReader(src), nil)
	if err != nil {
		t.Fatal(err)
	}
	when, _ := df.Column("when")
	got, _, _ := when.TimeAt(0)
	want := time.Date(2026, 5, 1, 12, 34, 56, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("default-layout parse = %v, want %v", got, want)
	}
}

func TestRead_TimeParseFailure(t *testing.T) {
	src := "when\nnot-a-date\n"
	type e struct {
		When time.Time `csv:"when"`
	}
	_, err := csvio.Read[e](strings.NewReader(src), nil)
	if !errors.Is(err, csvio.ErrTimeParse) {
		t.Fatalf("want ErrTimeParse, got %v", err)
	}
}

func TestReadFile_EventsFixture(t *testing.T) {
	df, err := csvio.ReadFile[event]("../testdata/events.csv", nil)
	if err != nil {
		t.Fatal(err)
	}
	rows, _ := df.Shape()
	if rows != 5 {
		t.Fatalf("rows = %d, want 5", rows)
	}
	when, _ := df.Column("when")
	// Row 4 (index 4) is "2026-07-20 16:45:00".
	got, _, _ := when.TimeAt(4)
	want := time.Date(2026, 7, 20, 16, 45, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("row 4 = %v, want %v", got, want)
	}
}
