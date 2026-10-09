package gobi

import (
	"errors"
	"fmt"
	"math"
	"reflect"
	"sync"
	"testing"
	"time"
	"unsafe"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// simpleRow exercises the common non-tagged types.
type simpleRow struct {
	ID       int64
	Name     string
	Value    float64
	Active   bool
	SmallInt int32
}

func TestFromStructs_Primitives(t *testing.T) {
	rows := []simpleRow{
		{ID: 1, Name: "a", Value: 1.5, Active: true, SmallInt: 10},
		{ID: 2, Name: "b", Value: 2.5, Active: false, SmallInt: 20},
	}
	f, err := FromStructs(rows)
	if err != nil {
		t.Fatal(err)
	}
	if r, c := f.Shape(); r != 2 || c != 5 {
		t.Fatalf("shape = (%d, %d), want (2, 5)", r, c)
	}
	names := f.ColumnNames()
	want := []string{"ID", "Name", "Value", "Active", "SmallInt"}
	for i, n := range want {
		if names[i] != n {
			t.Errorf("col %d = %q, want %q", i, names[i], n)
		}
	}
	// Sample values.
	idS, _ := f.Column("ID")
	idArr := idS.Column().Data().Chunks()[0].(*array.Int64)
	if idArr.Value(0) != 1 || idArr.Value(1) != 2 {
		t.Errorf("id values wrong")
	}
	activeS, _ := f.Column("Active")
	activeArr := activeS.Column().Data().Chunks()[0].(*array.Boolean)
	if !activeArr.Value(0) || activeArr.Value(1) {
		t.Errorf("active values wrong")
	}
}

// taggedRow exercises csv:"name" rename, geom:"true", and time:"layout".
type taggedRow struct {
	ID       int64     `csv:"id"`
	Location string    `csv:"loc" geom:"true"`
	TS       time.Time `csv:"ts"`
}

func TestFromStructs_TagsAndRoundTrip(t *testing.T) {
	t0 := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	rows := []taggedRow{
		{ID: 1, Location: "POINT(0 0)", TS: t0},
		{ID: 2, Location: "POINT(1 1)", TS: t0.Add(time.Hour)},
	}
	f, err := FromStructs(rows)
	if err != nil {
		t.Fatalf("FromStructs: %v", err)
	}
	// Renamed columns:
	if names := f.ColumnNames(); names[0] != "id" || names[1] != "loc" || names[2] != "ts" {
		t.Fatalf("names = %v, want [id loc ts]", names)
	}
	// loc must be tagged as geometry.
	locS, _ := f.Column("loc")
	if !locS.IsGeometry() {
		t.Errorf("loc column lost geometry tag")
	}
	// Round-trip.
	back, err := ToStructs[taggedRow](f)
	if err != nil {
		t.Fatalf("ToStructs: %v", err)
	}
	if len(back) != 2 {
		t.Fatalf("back len = %d, want 2", len(back))
	}
	if back[0].ID != 1 || back[1].ID != 2 {
		t.Errorf("id round-trip wrong: %v", back)
	}
	// Location is emitted as WKT on the way back — verify it parses
	// back to a Point at (0, 0).
	if !strContains(back[0].Location, "POINT") || !strContains(back[0].Location, "0") {
		t.Errorf("loc round-trip lost data: %q", back[0].Location)
	}
	// Time round-trip: arrow Timestamp is UTC nanoseconds; compare
	// UnixNano to sidestep monotonic clock differences.
	if back[0].TS.UnixNano() != t0.UnixNano() {
		t.Errorf("ts round-trip: got %v want %v", back[0].TS, t0)
	}
}

// nullableRow tests pointer-typed fields → nullable columns.
type nullableRow struct {
	ID   int64
	Name *string
	Cost *float64
}

func TestFromStructs_NullablePointers(t *testing.T) {
	name := "hello"
	rows := []nullableRow{
		{ID: 1, Name: &name, Cost: nil}, // Cost null
		{ID: 2, Name: nil, Cost: nil},   // both null
	}
	f, err := FromStructs(rows)
	if err != nil {
		t.Fatal(err)
	}
	nameS, _ := f.Column("Name")
	nameArr := nameS.Column().Data().Chunks()[0].(*array.String)
	if nameArr.IsNull(0) {
		t.Error("row 0 Name should be non-null")
	}
	if !nameArr.IsNull(1) {
		t.Error("row 1 Name should be null")
	}
	costS, _ := f.Column("Cost")
	costArr := costS.Column().Data().Chunks()[0].(*array.Float64)
	if !costArr.IsNull(0) || !costArr.IsNull(1) {
		t.Error("Cost should be null on both rows")
	}
	// Round-trip.
	back, err := ToStructs[nullableRow](f)
	if err != nil {
		t.Fatal(err)
	}
	if back[0].Name == nil || *back[0].Name != "hello" {
		t.Errorf("Name pointer round-trip lost data")
	}
	if back[1].Name != nil {
		t.Errorf("Name row 1 should still be nil")
	}
	if back[0].Cost != nil || back[1].Cost != nil {
		t.Errorf("Cost round-trip: expected nil, got %v %v", back[0].Cost, back[1].Cost)
	}
}

// stringTimeRow tests the string-field-with-time-tag path.
type stringTimeRow struct {
	Date string `csv:"date" time:"2006-01-02"`
}

func TestFromStructs_StringTimeTag(t *testing.T) {
	rows := []stringTimeRow{
		{Date: "2026-07-22"},
		{Date: "2026-07-23"},
	}
	f, err := FromStructs(rows)
	if err != nil {
		t.Fatal(err)
	}
	dateS, _ := f.Column("date")
	if dateS.DataType().ID() != arrow.TIMESTAMP {
		t.Fatalf("date column type = %s, want Timestamp", dateS.DataType())
	}
	back, err := ToStructs[stringTimeRow](f)
	if err != nil {
		t.Fatal(err)
	}
	// Round-trip preserves the date string via the same layout.
	if back[0].Date != "2026-07-22" || back[1].Date != "2026-07-23" {
		t.Errorf("date round-trip: %v", back)
	}
}

// listRow exercises slice fields → List columns. Covers value-typed
// elements ([]string), pointer-typed nullable elements ([]*int64),
// nil slice (null list), and empty non-nil slice (zero-length list).
type listRow struct {
	ID   int64
	Tags []string
	Nums []*int64
}

func TestFromStructs_SliceFields(t *testing.T) {
	n1, n2 := int64(10), int64(20)
	rows := []listRow{
		{ID: 1, Tags: []string{"a", "b"}, Nums: []*int64{&n1, nil, &n2}},
		{ID: 2, Tags: []string{}, Nums: nil}, // empty non-nil vs nil
	}
	f, err := FromStructs(rows)
	if err != nil {
		t.Fatalf("FromStructs: %v", err)
	}
	if r, c := f.Shape(); r != 2 || c != 3 {
		t.Fatalf("shape = (%d, %d), want (2, 3)", r, c)
	}
	tagsS, _ := f.Column("Tags")
	if tagsS.DataType().ID() != arrow.LIST {
		t.Fatalf("Tags type = %s, want LIST", tagsS.DataType())
	}
	numsS, _ := f.Column("Nums")
	if numsS.DataType().ID() != arrow.LIST {
		t.Fatalf("Nums type = %s, want LIST", numsS.DataType())
	}
	// Row 0: Tags = ["a", "b"], row 1: [] (non-null, len 0).
	tagsArr := tagsS.Column().Data().Chunks()[0].(*array.List)
	if tagsArr.IsNull(0) || tagsArr.IsNull(1) {
		t.Fatalf("Tags rows should be non-null (empty ≠ null)")
	}
	s0, e0 := tagsArr.ValueOffsets(0)
	if e0-s0 != 2 {
		t.Errorf("row 0 Tags len = %d, want 2", e0-s0)
	}
	s1, e1 := tagsArr.ValueOffsets(1)
	if e1-s1 != 0 {
		t.Errorf("row 1 Tags len = %d, want 0", e1-s1)
	}
	// Row 1 Nums is nil → null list.
	numsArr := numsS.Column().Data().Chunks()[0].(*array.List)
	if numsArr.IsNull(0) {
		t.Errorf("row 0 Nums should be non-null")
	}
	if !numsArr.IsNull(1) {
		t.Errorf("row 1 Nums should be null (input was nil slice)")
	}
	// Row 0 Nums has a null element at index 1.
	numsInner := numsArr.ListValues().(*array.Int64)
	ns, ne := numsArr.ValueOffsets(0)
	if ne-ns != 3 {
		t.Fatalf("row 0 Nums len = %d, want 3", ne-ns)
	}
	if numsInner.IsNull(int(ns)) || !numsInner.IsNull(int(ns)+1) || numsInner.IsNull(int(ns)+2) {
		t.Errorf("row 0 Nums null pattern wrong: [%v %v %v]",
			numsInner.IsNull(int(ns)), numsInner.IsNull(int(ns)+1), numsInner.IsNull(int(ns)+2))
	}
	if numsInner.Value(int(ns)) != 10 || numsInner.Value(int(ns)+2) != 20 {
		t.Errorf("row 0 Nums values wrong")
	}

	// Round-trip.
	back, err := ToStructs[listRow](f)
	if err != nil {
		t.Fatalf("ToStructs: %v", err)
	}
	if len(back) != 2 {
		t.Fatalf("back len = %d, want 2", len(back))
	}
	if len(back[0].Tags) != 2 || back[0].Tags[0] != "a" || back[0].Tags[1] != "b" {
		t.Errorf("row 0 Tags round-trip: %v", back[0].Tags)
	}
	if back[1].Tags == nil || len(back[1].Tags) != 0 {
		t.Errorf("row 1 Tags should be empty non-nil, got %v (nil=%v)", back[1].Tags, back[1].Tags == nil)
	}
	if len(back[0].Nums) != 3 {
		t.Fatalf("row 0 Nums len = %d, want 3", len(back[0].Nums))
	}
	if back[0].Nums[0] == nil || *back[0].Nums[0] != 10 {
		t.Errorf("row 0 Nums[0] wrong")
	}
	if back[0].Nums[1] != nil {
		t.Errorf("row 0 Nums[1] should be nil (was null in list)")
	}
	if back[0].Nums[2] == nil || *back[0].Nums[2] != 20 {
		t.Errorf("row 0 Nums[2] wrong")
	}
	if back[1].Nums != nil {
		t.Errorf("row 1 Nums should be nil (was null list)")
	}
}

// nestedSliceRow triggers the nested-slice error path.
type nestedSliceRow struct {
	Grid [][]int
}

func TestFromStructs_NestedSliceRejected(t *testing.T) {
	_, err := FromStructs([]nestedSliceRow{{Grid: [][]int{{1, 2}, {3}}}})
	if err == nil {
		t.Fatal("expected error on nested slice")
	}
}

// ptrToSliceRow triggers the *[]T rejection path.
type ptrToSliceRow struct {
	Tags *[]string
}

func TestFromStructs_PtrToSliceRejected(t *testing.T) {
	_, err := FromStructs([]ptrToSliceRow{{Tags: nil}})
	if err == nil {
		t.Fatal("expected error on *[]T field")
	}
}

// unsupportedRow triggers an error path — chan is not supported.
type unsupportedRow struct {
	Ch chan int
}

func TestFromStructs_UnsupportedType(t *testing.T) {
	_, err := FromStructs([]unsupportedRow{{}})
	if err == nil {
		t.Fatal("expected error on chan field")
	}
}

// strContains is a case-sensitive substring check; kept local so
// the test file doesn't pull in "strings" for one call. Named to
// avoid clashing with `contains` in setops_test.go (same package).
func strContains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func nullableOf(t *testing.T, f *Frame) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, fld := range f.Schema().Fields() {
		out[fld.Name] = fld.Nullable
	}
	return out
}

// TestStructRequiredFields_Nullability — non-pointer fields become
// REQUIRED; pointers stay nullable; tags override per field.
func TestStructRequiredFields_Nullability(t *testing.T) {
	type row struct {
		A int64
		B *int64
		C string `gobi:"C,optional"`
		L []int32
	}
	f, err := FromStructs([]row{{}}, StructRequiredFields())
	if err != nil {
		t.Fatal(err)
	}
	defer f.Release()
	want := map[string]bool{"A": false, "B": true, "C": true, "L": false}
	if got := nullableOf(t, f); !mapsEqual(got, want) {
		t.Errorf("nullable = %v, want %v", got, want)
	}
	// Required nil slice is an empty list, not null.
	l, _ := f.Column("L")
	if la := l.Column().Data().Chunk(0).(*array.List); la.IsNull(0) || la.Len() != 1 {
		t.Error("required nil slice should be an empty non-null list")
	}

	// `required` tag alone, without the option.
	type tagged struct {
		ID   int64 `gobi:"ID,required"`
		Name string
	}
	g, err := FromStructs([]tagged{{}})
	if err != nil {
		t.Fatal(err)
	}
	defer g.Release()
	if got := nullableOf(t, g); got["ID"] || !got["Name"] {
		t.Errorf("tag-only required: nullable = %v", got)
	}
}

func mapsEqual(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestStructRequiredFields_Errors(t *testing.T) {
	type ptrReq struct {
		P *int64 `gobi:"P,required"`
	}
	type both struct {
		X int64 `gobi:"X,required,optional"`
	}
	type geomReq struct {
		G string `gobi:"G" geom:"true"`
	}
	if _, err := FromStructs([]ptrReq{{}}); !errors.Is(err, ErrUnsupportedStructField) {
		t.Errorf("required pointer: err = %v", err)
	}
	if _, err := FromStructs([]both{{}}); !errors.Is(err, ErrUnsupportedStructField) {
		t.Errorf("required+optional: err = %v", err)
	}
	if _, err := FromStructs([]geomReq{{}}, StructRequiredFields()); !errors.Is(err, ErrUnsupportedStructField) {
		t.Errorf("empty required geometry: err = %v", err)
	}
}

// TestStructZeroTime — zero time.Time is null by default, the zero
// instant with StructZeroTimeAsValue or a required field, and an error
// on a nanosecond column that can't hold it.
func TestStructZeroTime(t *testing.T) {
	type us struct {
		T time.Time `gobi:"T,timestamp(microsecond)"`
	}
	type ns struct {
		T time.Time
	}
	isNull := func(f *Frame) bool {
		c, _ := f.Column("T")
		return c.Column().Data().Chunk(0).IsNull(0)
	}
	zeroInstant := func(f *Frame) bool {
		back, err := ToStructs[us](f)
		return err == nil && back[0].T.Equal(time.Time{})
	}

	def, err := FromStructs([]us{{}})
	if err != nil {
		t.Fatal(err)
	}
	defer def.Release()
	if !isNull(def) {
		t.Error("default: zero time should be null")
	}
	val, err := FromStructs([]us{{}}, StructZeroTimeAsValue())
	if err != nil {
		t.Fatal(err)
	}
	defer val.Release()
	if isNull(val) || !zeroInstant(val) {
		t.Error("StructZeroTimeAsValue: want the zero instant")
	}
	req, err := FromStructs([]us{{}}, StructRequiredFields())
	if err != nil {
		t.Fatal(err)
	}
	defer req.Release()
	if isNull(req) || !zeroInstant(req) {
		t.Error("required: want the zero instant")
	}
	if _, err := FromStructs([]ns{{}}, StructZeroTimeAsValue()); !errors.Is(err, ErrStructFieldOverflow) {
		t.Errorf("zero time into Timestamp[ns]: err = %v, want ErrStructFieldOverflow", err)
	}
}

// TestFromStructs_NanosecondRange — times outside 1677–2262 used to
// overflow UnixNano silently; now they error on ns columns and work on
// coarser ones.
func TestFromStructs_NanosecondRange(t *testing.T) {
	far := time.Date(2500, 1, 1, 0, 0, 0, 0, time.UTC)
	type ns struct{ T time.Time }
	type nsList struct{ L []time.Time }
	type us struct {
		T time.Time `gobi:"T,timestamp(microsecond)"`
	}
	if _, err := FromStructs([]ns{{far}}); !errors.Is(err, ErrStructFieldOverflow) {
		t.Errorf("2500 into ns: err = %v", err)
	}
	if _, err := FromStructs([]nsList{{[]time.Time{far}}}); !errors.Is(err, ErrStructFieldOverflow) {
		t.Errorf("2500 list element into ns: err = %v", err)
	}
	f, err := FromStructs([]us{{far}})
	if err != nil {
		t.Fatalf("2500 into us: %v", err)
	}
	f.Release()
	edge := time.Date(2262, 4, 11, 0, 0, 0, 0, time.UTC)
	g, err := FromStructs([]ns{{edge}})
	if err != nil {
		t.Fatalf("in-range edge: %v", err)
	}
	g.Release()
}

// TestStructAllocator — FromStructs builds with the given allocator,
// and the Frame's Release returns everything.
func TestStructAllocator(t *testing.T) {
	type row struct {
		A int64
		S string
	}
	checked := memory.NewCheckedAllocator(memory.NewGoAllocator())
	f, err := FromStructs([]row{{1, "x"}, {2, "y"}}, StructAllocator(checked))
	if err != nil {
		t.Fatal(err)
	}
	if checked.CurrentAlloc() == 0 {
		t.Fatal("StructAllocator not used")
	}
	f.Release()
	checked.AssertSize(t, 0)
}

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

// TestStructs_AllWidthsRoundTrip — every integer width FromStructs
// writes, plus []byte, must read back through ToStructs. Before, the
// 8- and 16-bit widths and plain []byte failed with "unsupported type".
func TestStructs_AllWidthsRoundTrip(t *testing.T) {
	type row struct {
		I8   int8
		I16  int16
		I32  int32
		I64  int64
		U8   uint8
		U16  uint16
		U32  uint32
		U64  uint64
		P16  *int16
		PU8  *uint8
		Raw  []byte
		L8   []int8
		LU16 []uint16
	}
	p16, pu8 := int16(-7), uint8(9)
	rows := []row{
		{
			I8: math.MinInt8, I16: math.MaxInt16, I32: math.MinInt32, I64: math.MaxInt64,
			U8: math.MaxUint8, U16: math.MaxUint16, U32: math.MaxUint32, U64: math.MaxUint64,
			P16: &p16, PU8: &pu8, Raw: []byte{0, 1, 2},
			L8: []int8{-1, 2}, LU16: []uint16{3, math.MaxUint16},
		},
		{}, // zero values; nil pointers and slices
	}
	f, err := FromStructs(rows)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Release()
	back, err := ToStructs[row](f)
	if err != nil {
		t.Fatalf("ToStructs: %v", err)
	}
	g, w := back[0], rows[0]
	if g.I8 != w.I8 || g.I16 != w.I16 || g.I32 != w.I32 || g.I64 != w.I64 ||
		g.U8 != w.U8 || g.U16 != w.U16 || g.U32 != w.U32 || g.U64 != w.U64 {
		t.Errorf("scalars: got %+v, want %+v", g, w)
	}
	if g.P16 == nil || *g.P16 != p16 || g.PU8 == nil || *g.PU8 != pu8 {
		t.Errorf("pointers: got %v %v", g.P16, g.PU8)
	}
	if string(g.Raw) != string(w.Raw) {
		t.Errorf("Raw = %v, want %v", g.Raw, w.Raw)
	}
	if len(g.L8) != 2 || g.L8[0] != -1 || g.L8[1] != 2 || len(g.LU16) != 2 || g.LU16[1] != math.MaxUint16 {
		t.Errorf("lists: got %v %v", g.L8, g.LU16)
	}
	if z := back[1]; z.P16 != nil || z.PU8 != nil || z.I16 != 0 || z.U8 != 0 {
		t.Errorf("zero row: got %+v", z)
	}
}

// TestToStructs_NarrowingOverflowErrors — a column value that doesn't
// fit the field errors instead of wrapping (1<<30 into int16 used to
// come back as 0). Values that fit still read into narrower fields.
func TestToStructs_NarrowingOverflowErrors(t *testing.T) {
	type wide struct {
		I int64
		U uint64
		F float64
		L []int64
	}
	f, err := FromStructs([]wide{{I: 1 << 30, U: 1 << 40, F: 1e300, L: []int64{1, 1 << 20}}})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Release()

	type narrowI struct{ I int16 }
	type narrowU struct{ U uint32 }
	type narrowF struct{ F float32 }
	type narrowL struct{ L []int8 }
	checks := map[string]error{}
	_, checks["int64→int16"] = ToStructs[narrowI](f)
	_, checks["uint64→uint32"] = ToStructs[narrowU](f)
	_, checks["float64→float32"] = ToStructs[narrowF](f)
	_, checks["[]int64→[]int8"] = ToStructs[narrowL](f)
	for name, err := range checks {
		if !errors.Is(err, ErrStructFieldOverflow) {
			t.Errorf("%s: err = %v, want ErrStructFieldOverflow", name, err)
		}
	}

	// In-range values into a narrower field are fine.
	small, err := FromStructs([]wide{{I: -300, U: 70_000, F: 1.5, L: []int64{-5}}})
	if err != nil {
		t.Fatal(err)
	}
	defer small.Release()
	type fits struct {
		I int16
		U uint32
		F float32
		L []int8
	}
	got, err := ToStructs[fits](small)
	if err != nil {
		t.Fatalf("in-range narrowing: %v", err)
	}
	if got[0].I != -300 || got[0].U != 70_000 || got[0].F != 1.5 || len(got[0].L) != 1 || got[0].L[0] != -5 {
		t.Errorf("in-range narrowing: got %+v", got[0])
	}
}

// TestToStructs_ListElementTypeMismatchErrors — a list whose element
// type doesn't match the slice field errors rather than panicking in
// reflect.
func TestToStructs_ListElementTypeMismatchErrors(t *testing.T) {
	lb := array.NewListBuilder(memory.DefaultAllocator, arrow.PrimitiveTypes.Uint8)
	lb.Append(true)
	lb.ValueBuilder().(*array.Uint8Builder).Append(3)
	list := lb.NewArray()
	lb.Release()
	f := frameFromChunks(t, []string{"xs"}, [][]arrow.Array{{list}})
	defer f.Release()
	type row struct {
		XS []string `gobi:"xs"`
	}
	if _, err := ToStructs[row](f); err == nil {
		t.Error("uint8 list into []string: want error, got nil")
	}
}

// TestToStructsInto_Reuse — dst's backing array is reused when it fits,
// stale contents never leak into fields a batch leaves null or absent,
// and a too-small dst gets a fresh slice.
func TestToStructsInto_Reuse(t *testing.T) {
	type row struct {
		A int64
		P *int64
		S string
	}
	type narrow struct{ A int64 }
	full, err := FromStructs([]row{{1, ptr(int64(10)), "x"}, {2, ptr(int64(20)), "y"}, {3, nil, "z"}})
	if err != nil {
		t.Fatal(err)
	}
	defer full.Release()
	onlyA, err := FromStructs([]narrow{{7}, {8}})
	if err != nil {
		t.Fatal(err)
	}
	defer onlyA.Release()

	dst := make([]row, 0, 4)
	got, err := ToStructsInto(full, dst)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || &got[0] != &dst[:1][0] {
		t.Fatalf("len=%d, reused=%v; want 3 rows in dst's backing array", len(got), &got[0] == &dst[:1][0])
	}
	if got[2].P != nil || *got[0].P != 10 || got[1].S != "y" {
		t.Errorf("first batch = %+v", got)
	}

	// Second batch has no P / S columns: they must come back zero, not
	// the previous batch's values.
	got2, err := ToStructsInto(onlyA, got)
	if err != nil {
		t.Fatal(err)
	}
	if len(got2) != 2 || got2[0].A != 7 || got2[0].P != nil || got2[0].S != "" || got2[1].S != "" {
		t.Errorf("second batch leaked stale fields: %+v", got2)
	}

	small := make([]row, 0, 1)
	got3, err := ToStructsInto(full, small)
	if err != nil {
		t.Fatal(err)
	}
	if len(got3) != 3 || cap(small) != 1 {
		t.Errorf("too-small dst: len=%d", len(got3))
	}

	// nil dst behaves like ToStructs.
	got4, err := ToStructsInto[row](full, nil)
	if err != nil || len(got4) != 3 {
		t.Errorf("nil dst: %v, %v", got4, err)
	}
}

// BenchmarkToStructsInto — the reused slice removes the per-batch []T
// allocation.
func BenchmarkToStructsInto(b *testing.B) {
	type row struct {
		A, B int64
		C    float64
	}
	rows := make([]row, 4096)
	f, err := FromStructs(rows)
	if err != nil {
		b.Fatal(err)
	}
	defer f.Release()
	b.Run("ToStructs", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := ToStructs[row](f); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("ToStructsInto", func(b *testing.B) {
		b.ReportAllocs()
		var dst []row
		for b.Loop() {
			if dst, err = ToStructsInto(f, dst); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// TestStructCoerceNumbers_Exact — cross-kind conversions succeed when
// the value survives exactly, including list elements.
func TestStructCoerceNumbers_Exact(t *testing.T) {
	type src struct {
		F  float64
		F2 float32
		I  int64
		U  uint32
		L  []float64
	}
	f, err := FromStructs([]src{{F: 3, F2: -2, I: 1 << 50, U: 7, L: []float64{1, 2}}})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Release()
	type dst struct {
		F  int32   // float64 3.0 → 3
		F2 int8    // float32 -2 → -2
		I  float64 // 2^50 fits float64's mantissa
		U  int16   // uint32 7 → 7
		L  []int16 // float64 elements → int16
	}
	got, err := ToStructs[dst](f, StructCoerceNumbers())
	if err != nil {
		t.Fatalf("ToStructs: %v", err)
	}
	g := got[0]
	if g.F != 3 || g.F2 != -2 || g.I != 1<<50 || g.U != 7 || len(g.L) != 2 || g.L[1] != 2 {
		t.Errorf("got %+v", g)
	}

	// Without the option, cross-kind stays an error.
	if _, err := ToStructs[dst](f); err == nil {
		t.Error("cross-kind without StructCoerceNumbers: want error")
	}
}

// TestStructCoerceNumbers_Rejects — lossy or out-of-range values fail
// with the matching sentinel.
func TestStructCoerceNumbers_Rejects(t *testing.T) {
	cases := []struct {
		name string
		run  func() error
		want error
	}{
		{"non-integral float → int", func() error {
			return coerceInto[struct{ V int64 }](struct{ V float64 }{3.5})
		}, ErrStructFieldInexact},
		{"NaN → int", func() error {
			return coerceInto[struct{ V int64 }](struct{ V float64 }{math.NaN()})
		}, ErrStructFieldInexact},
		{"huge float → int64", func() error {
			return coerceInto[struct{ V int64 }](struct{ V float64 }{1e19})
		}, ErrStructFieldOverflow},
		{"float → int8 overflow", func() error {
			return coerceInto[struct{ V int8 }](struct{ V float64 }{300})
		}, ErrStructFieldOverflow},
		{"negative → uint", func() error {
			return coerceInto[struct{ V uint64 }](struct{ V int64 }{-1})
		}, ErrStructFieldOverflow},
		{"uint64 max → int64", func() error {
			return coerceInto[struct{ V int64 }](struct{ V uint64 }{math.MaxUint64})
		}, ErrStructFieldOverflow},
		{"2^53+1 → float64", func() error {
			return coerceInto[struct{ V float64 }](struct{ V int64 }{1<<53 + 1})
		}, ErrStructFieldInexact},
		{"2^24+1 → float32", func() error {
			return coerceInto[struct{ V float32 }](struct{ V int32 }{1<<24 + 1})
		}, ErrStructFieldInexact},
	}
	for _, c := range cases {
		if err := c.run(); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", c.name, err, c.want)
		}
	}
}

// coerceInto round-trips one source row into D under
// StructCoerceNumbers and returns the error.
func coerceInto[D any, S any](row S) error {
	f, err := FromStructs([]S{row})
	if err != nil {
		return err
	}
	defer f.Release()
	_, err = ToStructs[D](f, StructCoerceNumbers())
	return err
}

// TestStructRequireColumns — a field with no column fails instead of
// zero-filling; present columns and default behavior are unchanged.
func TestStructRequireColumns(t *testing.T) {
	type src struct{ A int64 }
	f, err := FromStructs([]src{{1}})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Release()
	type dst struct {
		A int64
		B string
	}
	if _, err := ToStructs[dst](f, StructRequireColumns()); !errors.Is(err, ErrColumnNotFound) {
		t.Errorf("missing column B: err = %v, want ErrColumnNotFound", err)
	}
	got, err := ToStructs[dst](f)
	if err != nil || got[0].A != 1 || got[0].B != "" {
		t.Errorf("default zero-fill: %+v, %v", got, err)
	}
	if _, err := ToStructs[src](f, StructRequireColumns()); err != nil {
		t.Errorf("all columns present: %v", err)
	}
}

// inBuf reports whether p points into buf's backing memory.
func inBuf(p unsafe.Pointer, buf []byte) bool {
	if len(buf) == 0 || p == nil {
		return false
	}
	lo := uintptr(unsafe.Pointer(&buf[0]))
	return uintptr(p) >= lo && uintptr(p) < lo+uintptr(len(buf))
}

type ownRow struct {
	Name  string
	Tags  []string
	Raw   []byte
	PName *string
}

func ownFixture(t *testing.T) (*Frame, []byte, []byte, []byte) {
	t.Helper()
	f, err := FromStructs([]ownRow{
		{Name: "alpha", Tags: []string{"x", "y"}, Raw: []byte{1, 2, 3}, PName: ptr("beta")},
		{Name: "gamma", Tags: []string{"z"}, Raw: []byte{4}, PName: ptr("delta")},
	})
	if err != nil {
		t.Fatal(err)
	}
	name, _ := f.Column("Name")
	tags, _ := f.Column("Tags")
	raw, _ := f.Column("Raw")
	nameBuf := name.Column().Data().Chunk(0).(*array.String).ValueBytes()
	tagBuf := tags.Column().Data().Chunk(0).(*array.List).ListValues().(*array.String).ValueBytes()
	rawBuf := raw.Column().Data().Chunk(0).(*array.Binary).ValueBytes()
	return f, nameBuf, tagBuf, rawBuf
}

// TestToStructs_DefaultAliasesBuffers documents the zero-copy default.
func TestToStructs_DefaultAliasesBuffers(t *testing.T) {
	f, nameBuf, tagBuf, rawBuf := ownFixture(t)
	defer f.Release()
	rows, err := ToStructs[ownRow](f)
	if err != nil {
		t.Fatal(err)
	}
	if !inBuf(unsafe.Pointer(unsafe.StringData(rows[0].Name)), nameBuf) ||
		!inBuf(unsafe.Pointer(unsafe.StringData(rows[0].Tags[0])), tagBuf) ||
		!inBuf(unsafe.Pointer(&rows[0].Raw[0]), rawBuf) {
		t.Error("default ToStructs no longer zero-copy; update the ownership docs")
	}
}

// TestToStructs_CopyValuesOwnsMemory — nothing points into the
// Frame's buffers, and values are intact.
func TestToStructs_CopyValuesOwnsMemory(t *testing.T) {
	f, nameBuf, tagBuf, rawBuf := ownFixture(t)
	defer f.Release()
	rows, err := ToStructs[ownRow](f, StructCopyValues())
	if err != nil {
		t.Fatal(err)
	}
	for i, r := range rows {
		for _, s := range append([]string{r.Name, *r.PName}, r.Tags...) {
			p := unsafe.Pointer(unsafe.StringData(s))
			if inBuf(p, nameBuf) || inBuf(p, tagBuf) {
				t.Errorf("row %d: %q aliases an Arrow buffer", i, s)
			}
		}
		if inBuf(unsafe.Pointer(&r.Raw[0]), rawBuf) {
			t.Errorf("row %d: Raw aliases an Arrow buffer", i)
		}
	}
	if rows[0].Name != "alpha" || *rows[1].PName != "delta" || rows[0].Tags[1] != "y" || rows[1].Raw[0] != 4 {
		t.Errorf("values wrong: %+v", rows)
	}
}

// TestToStructs_InternSharesValues — interned fields share one copy
// per distinct value, within a call and (with StructInterner) across
// calls, and never alias the Frame.
func TestToStructs_InternSharesValues(t *testing.T) {
	type out struct {
		OS   string   `gobi:"os,intern"`
		POS  *string  `gobi:"os2,intern"`
		Tags []string `gobi:"tags,intern"`
	}
	type inRow struct {
		OS   string   `gobi:"os"`
		OS2  string   `gobi:"os2"`
		Tags []string `gobi:"tags"`
	}
	var src []inRow
	oses := []string{"android", "ios", "linux"}
	for i := range 300 {
		os := oses[i%3]
		src = append(src, inRow{OS: os, OS2: os, Tags: []string{os}})
	}
	f, err := FromStructs(src)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Release()
	osCol, _ := f.Column("os")
	osBuf := osCol.Column().Data().Chunk(0).(*array.String).ValueBytes()

	shared := NewStringInterner(0)
	a, err := ToStructs[out](f, StructInterner(shared))
	if err != nil {
		t.Fatal(err)
	}
	b, err := ToStructs[out](f, StructInterner(shared))
	if err != nil {
		t.Fatal(err)
	}
	if shared.Len() != 3 {
		t.Errorf("interner holds %d values, want 3", shared.Len())
	}
	first := map[string]*byte{}
	for i, rows := range [][]out{a, b} {
		for j, r := range rows {
			for _, s := range []string{r.OS, *r.POS, r.Tags[0]} {
				p := unsafe.StringData(s)
				if inBuf(unsafe.Pointer(p), osBuf) {
					t.Fatalf("call %d row %d: interned %q aliases the Frame", i, j, s)
				}
				if q, ok := first[s]; !ok {
					first[s] = p
				} else if q != p {
					t.Fatalf("call %d row %d: %q not shared (%p vs %p)", i, j, s, p, q)
				}
			}
			if r.OS != oses[j%3] {
				t.Fatalf("row %d OS = %q", j, r.OS)
			}
		}
	}

	// Without StructInterner each call gets its own interner: values
	// are still shared within the call.
	c, err := ToStructs[out](f)
	if err != nil {
		t.Fatal(err)
	}
	if unsafe.StringData(c[0].OS) != unsafe.StringData(c[3].OS) {
		t.Error("per-call interner: rows 0 and 3 don't share")
	}
}

func TestStringInterner_CapAndConcurrency(t *testing.T) {
	in := NewStringInterner(2)
	buf := []byte("aaabbbccc")
	view := func(i int) string { return unsafe.String(&buf[i*3], 3) }
	a1, b1, c1 := in.Intern(view(0)), in.Intern(view(1)), in.Intern(view(2))
	if in.Len() != 2 {
		t.Fatalf("Len = %d, want 2 (cap)", in.Len())
	}
	if c1 != "ccc" || inBuf(unsafe.Pointer(unsafe.StringData(c1)), buf) {
		t.Error("over-cap value must still be an owned copy")
	}
	if in.Intern("ccc") == c1 && unsafe.StringData(in.Intern("ccc")) == unsafe.StringData(c1) {
		t.Error("over-cap value should not be remembered")
	}
	if unsafe.StringData(in.Intern("aaa")) != unsafe.StringData(a1) || in.Intern("bbb") != b1 {
		t.Error("existing entries must keep being shared")
	}

	var wg sync.WaitGroup
	shared := NewStringInterner(0)
	for g := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 1000 {
				shared.Intern(fmt.Sprintf("v%d", (i+g)%50))
			}
		}()
	}
	wg.Wait()
	if shared.Len() != 50 {
		t.Errorf("concurrent Len = %d, want 50", shared.Len())
	}
}

func TestToStructs_InternRejectsNonString(t *testing.T) {
	type bad struct {
		N int64 `gobi:"n,intern"`
	}
	type badGeom struct {
		G string `gobi:"g,intern" geom:"true"`
	}
	f, err := FromStructs([]struct {
		N int64  `gobi:"n"`
		G string `gobi:"g" geom:"true"`
	}{{1, "POINT(0 0)"}})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Release()
	if _, err := ToStructs[bad](f); !errors.Is(err, ErrUnsupportedStructField) {
		t.Errorf("intern on int64: err = %v", err)
	}
	if _, err := ToStructs[badGeom](f); !errors.Is(err, ErrUnsupportedStructField) {
		t.Errorf("intern on geometry: err = %v", err)
	}
}

// nullRow carries a Null[T] field per supported value shape.
type nullRow struct {
	ID    int64
	Count Null[int64]
	Small Null[int8]
	Label Null[string]
	Score Null[float64]
	Flag  Null[bool]
	Blob  Null[[]byte]
	At    Null[time.Time] `gobi:"At,timestamp(microsecond)"`
}

// TestNull_RoundTrip — FromStructs writes null exactly when Valid is
// false, a zero V with Valid=true stays a non-null value, and ToStructs
// reads both back unchanged.
func TestNull_RoundTrip(t *testing.T) {
	at := time.Date(2026, 3, 4, 5, 6, 7, 8000, time.UTC)
	rows := []nullRow{
		{ID: 1,
			Count: Null[int64]{V: 42, Valid: true}, Small: Null[int8]{V: -7, Valid: true},
			Label: Null[string]{V: "x", Valid: true}, Score: Null[float64]{V: 1.5, Valid: true},
			Flag: Null[bool]{V: true, Valid: true}, Blob: Null[[]byte]{V: []byte{1, 2}, Valid: true},
			At: Null[time.Time]{V: at, Valid: true}},
		{ID: 2}, // every Null field invalid
		{ID: 3, // every field valid at its zero value
			Count: Null[int64]{Valid: true}, Small: Null[int8]{Valid: true},
			Label: Null[string]{Valid: true}, Score: Null[float64]{Valid: true},
			Flag: Null[bool]{Valid: true}, Blob: Null[[]byte]{V: []byte{}, Valid: true},
			At: Null[time.Time]{Valid: true}},
	}
	f, err := FromStructs(rows)
	if err != nil {
		t.Fatal(err)
	}
	if dt := f.Schema().Field(2).Type; dt.ID() != arrow.INT8 {
		t.Errorf("Small column type = %s, want int8", dt)
	}
	for _, name := range []string{"Count", "Small", "Label", "Score", "Flag", "Blob", "At"} {
		s, err := f.Column(name)
		if err != nil {
			t.Fatal(err)
		}
		if !s.field.Nullable {
			t.Errorf("%s: column should be nullable", name)
		}
		chunk := s.Column().Data().Chunks()[0]
		if chunk.IsNull(0) || !chunk.IsNull(1) || chunk.IsNull(2) {
			t.Errorf("%s: nulls = [%v %v %v], want [false true false]",
				name, chunk.IsNull(0), chunk.IsNull(1), chunk.IsNull(2))
		}
	}

	back, err := ToStructs[nullRow](f)
	if err != nil {
		t.Fatal(err)
	}
	r0, r1, r2 := back[0], back[1], back[2]
	if r0.Count != (Null[int64]{V: 42, Valid: true}) || r0.Small != (Null[int8]{V: -7, Valid: true}) ||
		r0.Label != (Null[string]{V: "x", Valid: true}) || r0.Score != (Null[float64]{V: 1.5, Valid: true}) ||
		r0.Flag != (Null[bool]{V: true, Valid: true}) {
		t.Errorf("row 0 scalars: got %+v", r0)
	}
	if !r0.Blob.Valid || string(r0.Blob.V) != "\x01\x02" {
		t.Errorf("row 0 Blob: got %+v", r0.Blob)
	}
	if !r0.At.Valid || !r0.At.V.Equal(at) {
		t.Errorf("row 0 At: got %+v, want %v", r0.At, at)
	}
	if r1.Count.Valid || r1.Small.Valid || r1.Label.Valid || r1.Score.Valid ||
		r1.Flag.Valid || r1.Blob.Valid || r1.At.Valid {
		t.Errorf("row 1: every field should be invalid, got %+v", r1)
	}
	if !r2.Count.Valid || r2.Count.V != 0 || !r2.Label.Valid || r2.Label.V != "" ||
		!r2.Flag.Valid || r2.Flag.V || !r2.Blob.Valid || len(r2.Blob.V) != 0 {
		t.Errorf("row 2: zero values should read back valid, got %+v", r2)
	}
	if !r2.At.Valid || !r2.At.V.IsZero() {
		t.Errorf("row 2 At: want valid zero instant, got %+v", r2.At)
	}
}

// TestNull_ZeroTimeNeedsUnit — a valid zero time.Time is the zero
// instant, which doesn't fit the default Timestamp[ns].
func TestNull_ZeroTimeNeedsUnit(t *testing.T) {
	type row struct{ At Null[time.Time] }
	_, err := FromStructs([]row{{At: Null[time.Time]{Valid: true}}})
	if !errors.Is(err, ErrStructFieldOverflow) {
		t.Fatalf("err = %v, want ErrStructFieldOverflow", err)
	}
	// Invalid stays null with any unit.
	if _, err := FromStructs([]row{{}}); err != nil {
		t.Fatalf("invalid zero time: %v", err)
	}
}

// TestNull_ReadIntoReusedSlice — ToStructsInto resets reused rows, so a
// null cell clears a Valid left over from the previous batch.
func TestNull_ReadIntoReusedSlice(t *testing.T) {
	type row struct{ N Null[int64] }
	valid, err := FromStructs([]row{{N: Null[int64]{V: 9, Valid: true}}})
	if err != nil {
		t.Fatal(err)
	}
	null, err := FromStructs([]row{{}})
	if err != nil {
		t.Fatal(err)
	}
	dst, err := ToStructsInto(valid, []row(nil))
	if err != nil {
		t.Fatal(err)
	}
	dst, err = ToStructsInto(null, dst)
	if err != nil {
		t.Fatal(err)
	}
	if dst[0].N.Valid || dst[0].N.V != 0 {
		t.Errorf("reused row should be reset to invalid zero, got %+v", dst[0].N)
	}
}

// TestNull_RequiredFieldsLeavesNullable — StructRequiredFields makes
// plain fields REQUIRED but keeps Null[T] fields nullable.
func TestNull_RequiredFieldsLeavesNullable(t *testing.T) {
	type row struct {
		A int64
		B Null[int64]
	}
	f, err := FromStructs([]row{{A: 1}}, StructRequiredFields())
	if err != nil {
		t.Fatal(err)
	}
	if f.Schema().Field(0).Nullable {
		t.Error("A should be required")
	}
	if !f.Schema().Field(1).Nullable {
		t.Error("B should stay nullable")
	}
}

// TestNull_UnsupportedShapes — ambiguous Null forms are rejected at
// planning time by both directions.
func TestNull_UnsupportedShapes(t *testing.T) {
	type ptrNull struct{ X *Null[int64] }
	type nullPtr struct{ X Null[*int64] }
	type nullSlice struct{ X Null[[]int64] }
	type requiredNull struct {
		X Null[int64] `gobi:"x,required"`
	}
	check := func(name string, err error) {
		t.Helper()
		if !errors.Is(err, ErrUnsupportedStructField) {
			t.Errorf("%s: err = %v, want ErrUnsupportedStructField", name, err)
		}
	}
	_, err := FromStructs([]ptrNull{{}})
	check("*Null[T] write", err)
	_, err = FromStructs([]nullPtr{{}})
	check("Null[*T] write", err)
	_, err = FromStructs([]nullSlice{{}})
	check("Null[[]T] write", err)
	_, err = FromStructs([]requiredNull{{}})
	check("required Null[T] write", err)

	f, err := FromStructs([]struct{ X int64 }{{X: 1}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = ToStructs[ptrNull](f)
	check("*Null[T] read", err)
	_, err = ToStructs[nullPtr](f)
	check("Null[*T] read", err)
}
