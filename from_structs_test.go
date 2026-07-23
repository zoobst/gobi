package gobi

import (
	"testing"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
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
