package gobi

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

// Operations that gather rows of a list column, or elements out of one,
// must work for every element type and keep the column's exact type.
// Each case is an element type with three element values e0, e1, e2
// (JSON); the list column's rows are [e0], [e1, e2], null and [].
// Overture's "sources" (List<Struct>) is the case that first failed.

type listCase struct {
	name       string
	elem       arrow.DataType
	nonNull    bool // list elements declared non-nullable
	e0, e1, e2 string
}

var listCases = []listCase{
	{name: "string, non-null elements", elem: arrow.BinaryTypes.String, nonNull: true, e0: `"a"`, e1: `"b"`, e2: `"c"`},
	{name: "float64 with a null element", elem: arrow.PrimitiveTypes.Float64, e0: `1.5`, e1: `null`, e2: `2.5`},
	{name: "struct", elem: arrow.StructOf(
		arrow.Field{Name: "dataset", Type: arrow.BinaryTypes.String, Nullable: true},
		arrow.Field{Name: "confidence", Type: arrow.PrimitiveTypes.Float64, Nullable: true},
	), e0: `{"dataset":"osm","confidence":0.5}`, e1: `{"dataset":"msft","confidence":null}`, e2: `{"dataset":"google","confidence":0.9}`},
	{name: "nested list", elem: arrow.ListOf(arrow.PrimitiveTypes.Int64), e0: `[1]`, e1: `[2,3]`, e2: `null`},
	{name: "date32", elem: arrow.FixedWidthTypes.Date32, e0: `"2024-01-01"`, e1: `"2024-02-29"`, e2: `"1999-12-31"`},
	{name: "decimal128", elem: &arrow.Decimal128Type{Precision: 10, Scale: 2}, e0: `"1.25"`, e1: `"2.50"`, e2: `"-3.75"`},
}

func (c listCase) listType() *arrow.ListType {
	if c.nonNull {
		return arrow.ListOfNonNullable(c.elem)
	}
	return arrow.ListOf(c.elem)
}

// Rows of the list column, as JSON.
func (c listCase) r0() string { return "[" + c.e0 + "]" }
func (c listCase) r1() string { return "[" + c.e1 + "," + c.e2 + "]" }
func (c listCase) rows() []string {
	return []string{c.r0(), c.r1(), "null", "[]"}
}

// fromJSON builds an array of dt from JSON values, split into chunks of
// the given sizes.
func fromJSON(t *testing.T, dt arrow.DataType, split []int, values ...string) *arrow.Chunked {
	t.Helper()
	var chunks []arrow.Array
	at := 0
	for _, n := range split {
		arr, _, err := array.FromJSON(memory.DefaultAllocator, dt,
			strings.NewReader("["+strings.Join(values[at:at+n], ",")+"]"))
		if err != nil {
			t.Fatalf("FromJSON %s: %v", dt, err)
		}
		chunks = append(chunks, arr)
		at += n
	}
	ch := arrow.NewChunked(dt, chunks)
	for _, a := range chunks {
		a.Release()
	}
	return ch
}

// listFrame builds a frame from (name, chunked) pairs; it takes ownership.
func listFrame(t *testing.T, cols ...any) *Frame {
	t.Helper()
	var fields []arrow.Field
	var columns []arrow.Column
	for i := 0; i < len(cols); i += 2 {
		ch := cols[i+1].(*arrow.Chunked)
		f := arrow.Field{Name: cols[i].(string), Type: ch.DataType(), Nullable: true}
		fields = append(fields, f)
		columns = append(columns, *arrow.NewColumn(f, ch))
		ch.Release()
	}
	fr, err := NewFrame(arrow.NewSchema(fields, nil), columns)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fr.Release)
	return fr
}

// ids is an int64 key column with the given chunk sizes.
func ids(t *testing.T, split []int, vals ...int64) *arrow.Chunked {
	t.Helper()
	s := make([]string, len(vals))
	for i, v := range vals {
		s[i] = fmt.Sprint(v)
	}
	return fromJSON(t, arrow.PrimitiveTypes.Int64, split, s...)
}

// jsonRows renders a column's values one JSON string per row.
func jsonRows(t *testing.T, ch []arrow.Array) []string {
	t.Helper()
	var out []string
	for _, a := range ch {
		b, err := a.MarshalJSON()
		if err != nil {
			t.Fatal(err)
		}
		var rows []json.RawMessage
		if err := json.Unmarshal(b, &rows); err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			out = append(out, string(r))
		}
	}
	return out
}

// wantRows normalizes expected JSON values through arrow, so formatting
// matches what jsonRows produces.
func wantRows(t *testing.T, dt arrow.DataType, values ...string) []string {
	t.Helper()
	ch := fromJSON(t, dt, []int{len(values)}, values...)
	defer ch.Release()
	return jsonRows(t, ch.Chunks())
}

func column(t *testing.T, f *Frame, name string) Series {
	t.Helper()
	s, err := f.Column(name)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func checkRows(t *testing.T, what string, got Series, want []string) {
	t.Helper()
	g := jsonRows(t, got.col.Data().Chunks())
	if strings.Join(g, "|") != strings.Join(want, "|") {
		t.Errorf("%s:\n got  %v\n want %v", what, g, want)
	}
}

// checkType: gathering rows must keep the column's exact type, element
// field nullability included (arrow.NewColumn panics on a mismatch).
func checkType(t *testing.T, what string, got Series, want arrow.DataType) {
	t.Helper()
	if !arrow.TypeEqual(got.DataType(), want) {
		t.Errorf("%s: type %s, want %s", what, got.DataType(), want)
	}
}

func TestListColumns_everyElementType(t *testing.T) {
	for _, c := range listCases {
		for _, split := range [][]int{{4}, {2, 2}} {
			t.Run(fmt.Sprintf("%s/%d chunks", c.name, len(split)), func(t *testing.T) {
				lt := c.listType()
				r := c.rows()
				f := listFrame(t,
					"id", ids(t, split, 0, 1, 2, 3),
					"xs", fromJSON(t, lt, split, r...),
					// For Coalesce: fills row 2 (null in xs) with r0.
					"other", fromJSON(t, lt, split, r[1], r[0], r[0], r[1]),
				)

				t.Run("Take", func(t *testing.T) {
					out, err := f.Take([]int{3, 1, 2, 0})
					if err != nil {
						t.Fatal(err)
					}
					defer out.Release()
					checkType(t, "Take", column(t, out, "xs"), lt)
					checkRows(t, "Take", column(t, out, "xs"), wantRows(t, lt, r[3], r[1], r[2], r[0]))
				})

				t.Run("SortBy", func(t *testing.T) {
					out, err := f.SortBy(SortKey{Column: "id", Descending: true})
					if err != nil {
						t.Fatal(err)
					}
					defer out.Release()
					checkType(t, "SortBy", column(t, out, "xs"), lt)
					checkRows(t, "SortBy", column(t, out, "xs"), wantRows(t, lt, r[3], r[2], r[1], r[0]))
				})

				t.Run("Unique", func(t *testing.T) {
					dup := listFrame(t,
						"k", ids(t, split, 5, 5, 6, 6),
						"xs", fromJSON(t, lt, split, r...),
					)
					out, err := dup.Unique("k")
					if err != nil {
						t.Fatal(err)
					}
					defer out.Release()
					checkType(t, "Unique", column(t, out, "xs"), lt)
					checkRows(t, "Unique", column(t, out, "xs"), wantRows(t, lt, r[0], r[2]))
				})

				t.Run("Join left", func(t *testing.T) {
					// Right side: ids 1 and 3 match, 5 doesn't; ids 0 and 2
					// have no match, so their right-side list is null.
					rsplit := []int{3}
					if len(split) > 1 {
						rsplit = []int{2, 1}
					}
					right := listFrame(t,
						"id", ids(t, rsplit, 1, 3, 5),
						"rx", fromJSON(t, lt, rsplit, r[1], r[0], r[1]),
					)
					out, err := f.Join(right, "id", "id", JoinLeft)
					if err != nil {
						t.Fatal(err)
					}
					defer out.Release()
					if out.NumRows() != 4 {
						t.Fatalf("left join kept %d rows, want 4", out.NumRows())
					}
					checkType(t, "Join xs", column(t, out, "xs"), lt)
					checkType(t, "Join rx", column(t, out, "rx"), lt)
					// Rows may come back in any order: compare by id.
					gotIDs := jsonRows(t, column(t, out, "id").col.Data().Chunks())
					xs := jsonRows(t, column(t, out, "xs").col.Data().Chunks())
					rx := jsonRows(t, column(t, out, "rx").col.Data().Chunks())
					wantXS := wantRows(t, lt, r...)
					wantRX := map[string]string{"0": "null", "2": "null"}
					wantRX["1"], wantRX["3"] = wantRows(t, lt, r[1])[0], wantRows(t, lt, r[0])[0]
					seen := map[string]bool{}
					for i, id := range gotIDs {
						seen[id] = true
						var n int
						fmt.Sscan(id, &n)
						if xs[i] != wantXS[n] || rx[i] != wantRX[id] {
							t.Errorf("id %s: xs=%s rx=%s, want xs=%s rx=%s", id, xs[i], rx[i], wantXS[n], wantRX[id])
						}
					}
					if len(seen) != 4 {
						t.Errorf("ids in output: %v, want 0..3", gotIDs)
					}
				})

				t.Run("Explode", func(t *testing.T) {
					out, err := f.Explode("xs")
					if err != nil {
						t.Fatal(err)
					}
					defer out.Release()
					// Null and empty lists give one null element each.
					checkRows(t, "Explode xs", column(t, out, "xs"), wantRows(t, c.elem, c.e0, c.e1, c.e2, "null", "null"))
					checkRows(t, "Explode id", column(t, out, "id"), wantRows(t, arrow.PrimitiveTypes.Int64, "0", "1", "1", "2", "3"))
				})

				// expr evaluates e as a new column; t is the subtest's.
				expr := func(t *testing.T, name string, e Expr) Series {
					t.Helper()
					out, err := f.WithColumnExpr(name, e)
					if err != nil {
						t.Fatalf("%s: %v", name, err)
					}
					t.Cleanup(out.Release)
					return column(t, out, name)
				}
				t.Run("ListGet", func(t *testing.T) {
					checkRows(t, "first", expr(t, "first", Col("xs").ListGet(0)), wantRows(t, c.elem, c.e0, c.e1, "null", "null"))
					checkRows(t, "last", expr(t, "last", Col("xs").ListGet(-1)), wantRows(t, c.elem, c.e0, c.e2, "null", "null"))
				})
				t.Run("ListSlice", func(t *testing.T) {
					checkRows(t, "slice", expr(t, "tail", Col("xs").ListSlice(1, 2)),
						wantRows(t, arrow.ListOf(c.elem), "[]", "["+c.e2+"]", "null", "[]"))
				})
				t.Run("Coalesce", func(t *testing.T) {
					got := expr(t, "filled", Coalesce(Col("xs"), Col("other")))
					checkType(t, "Coalesce", got, lt)
					checkRows(t, "Coalesce", got, wantRows(t, lt, r[0], r[1], r[0], r[3]))
				})
			})
		}
	}
}
