package gobi

import (
	"reflect"
	"testing"
)

// TestResolveFieldName_TagPriority checks the resolution order:
//
//	format-specific → gobi → csv → field name.
func TestResolveFieldName_TagPriority(t *testing.T) {
	type row struct {
		A string `parquet:"a_parquet" gobi:"a_gobi" csv:"a_csv"`
		B string `gobi:"b_gobi" csv:"b_csv"`
		C string `csv:"c_csv"`
		D string
	}
	sf := func(name string) reflect.StructField {
		f, _ := reflect.TypeFor[row]().FieldByName(name)
		return f
	}

	cases := []struct {
		field  string
		format string
		want   string
	}{
		// format tag wins when set
		{"A", "parquet", "a_parquet"},
		{"A", "csv", "a_csv"},      // format=csv → csv:"a_csv" wins over gobi
		{"A", "geojson", "a_gobi"}, // no geojson tag → gobi
		{"A", "", "a_gobi"},        // no format → gobi
		// gobi tag when no format match
		{"B", "parquet", "b_gobi"},
		{"B", "gobi", "b_gobi"},
		{"B", "", "b_gobi"},
		// csv legacy fallback
		{"C", "parquet", "c_csv"},
		{"C", "", "c_csv"},
		// field name fallback
		{"D", "parquet", "D"},
		{"D", "", "D"},
	}
	for _, c := range cases {
		got, skip := ResolveFieldName(sf(c.field), c.format)
		if skip {
			t.Errorf("field=%s format=%q: got skip=true", c.field, c.format)
			continue
		}
		if got != c.want {
			t.Errorf("field=%s format=%q: got %q, want %q", c.field, c.format, got, c.want)
		}
	}
}

// TestResolveFieldName_Skip: any tag value of "-" in a considered
// namespace makes the field skip.
func TestResolveFieldName_Skip(t *testing.T) {
	type row struct {
		A string `gobi:"-"`
		B string `parquet:"-"`
		C string `csv:"-"`
	}
	tp := reflect.TypeFor[row]()
	sf := func(name string) reflect.StructField {
		f, _ := tp.FieldByName(name)
		return f
	}
	cases := []struct {
		field, format string
		wantSkip      bool
	}{
		{"A", "", true},        // gobi:"-"
		{"A", "parquet", true}, // fallback to gobi:"-"
		{"B", "parquet", true}, // parquet:"-"
		{"B", "csv", false},    // no csv:"-" set, and no gobi tag → falls back to field name
		{"C", "csv", true},     // csv:"-"
		{"C", "parquet", true}, // fallback to csv:"-"
	}
	for _, c := range cases {
		_, skip := ResolveFieldName(sf(c.field), c.format)
		if skip != c.wantSkip {
			t.Errorf("field=%s format=%q: got skip=%v, want %v", c.field, c.format, skip, c.wantSkip)
		}
	}
}

// TestFromStructs_TagFormat validates that FromStructs with
// StructTagFormat("parquet") picks parquet-namespace column names when
// present, falling back through the chain otherwise.
func TestFromStructs_TagFormat(t *testing.T) {
	type Row struct {
		Age     int    `parquet:"age_pq" gobi:"age_gobi" csv:"age_csv"`
		Name    string `gobi:"name_gobi" csv:"name_csv"`
		Note    string `csv:"note_csv"`
		Ignored string `parquet:"-"`
	}
	rows := []Row{
		{Age: 42, Name: "alice", Note: "hi", Ignored: "skipme"},
	}
	f, err := FromStructs(rows, StructTagFormat("parquet"))
	if err != nil {
		t.Fatalf("FromStructs: %v", err)
	}
	names := f.ColumnNames()
	// Expect exactly age_pq, name_gobi, note_csv (Ignored is skipped).
	want := []string{"age_pq", "name_gobi", "note_csv"}
	if len(names) != len(want) {
		t.Fatalf("column count = %d (%v), want %d (%v)", len(names), names, len(want), want)
	}
	for i, n := range want {
		if names[i] != n {
			t.Errorf("col %d = %q, want %q", i, names[i], n)
		}
	}
}

// TestFromStructs_BackwardCompat checks that FromStructs without any
// options preserves the pre-existing behavior (csv:"..." tags still
// work).
func TestFromStructs_BackwardCompat(t *testing.T) {
	type Row struct {
		Age  int    `csv:"age"`
		Name string `csv:"name"`
	}
	rows := []Row{{Age: 1, Name: "a"}}
	f, err := FromStructs(rows)
	if err != nil {
		t.Fatalf("FromStructs: %v", err)
	}
	names := f.ColumnNames()
	if len(names) != 2 || names[0] != "age" || names[1] != "name" {
		t.Errorf("got %v, want [age name]", names)
	}
}
