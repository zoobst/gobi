package geojsonio

import (
	"bytes"
	json "encoding/json/v2"
	"fmt"
	"math"
	"math/rand/v2"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/zoobst/gobi"
	"github.com/zoobst/gobi/geometry"
)

// The writer Write used before it appended rows directly: every value
// boxed into a map, every geometry decoded, and the lot handed to
// json.Marshal. Kept here as the reference the direct writer is
// checked against.

// referenceFeature builds row's Feature the old way.
func referenceFeature(df *gobi.Frame, geomIdx, row int) ([]byte, error) {
	props := make(map[string]any)
	var g geometry.Geometry
	for i, name := range df.ColumnNames() {
		s, err := df.ColumnAt(i)
		if err != nil {
			return nil, err
		}
		if i == geomIdx {
			wkb, err := readGeomAt(s, row)
			if err != nil {
				return nil, err
			}
			if wkb != nil {
				if g, err = geometry.ParseWKB(wkb); err != nil {
					return nil, err
				}
			}
			continue
		}
		v, err := scalarAt(s, row)
		if err != nil {
			return nil, err
		}
		props[name] = v
	}
	return MarshalFeature(g, props)
}

// scalarAt reads one row from s as a Go scalar suitable for
// json.Marshal. Handles the primitive types Frame columns commonly
// carry; unsupported types come back as a nil.
func scalarAt(s gobi.Series, row int) (any, error) {
	chunks := s.Column().Data().Chunks()
	offset := 0
	for _, chunk := range chunks {
		if row < offset+chunk.Len() {
			local := row - offset
			if chunk.IsNull(local) {
				return nil, nil
			}
			switch a := chunk.(type) {
			case *array.Int64:
				return a.Value(local), nil
			case *array.Int32:
				return int64(a.Value(local)), nil
			case *array.Uint64:
				return a.Value(local), nil
			case *array.Uint32:
				return uint64(a.Value(local)), nil
			case *array.Float64:
				return a.Value(local), nil
			case *array.Float32:
				return float64(a.Value(local)), nil
			case *array.Boolean:
				return a.Value(local), nil
			case *array.String:
				return a.Value(local), nil
			case *array.LargeString:
				return a.Value(local), nil
			case *array.Binary:
				return a.Value(local), nil
			case *array.Timestamp:
				// Emit as RFC 3339 for round-trip-friendly JSON. Unit
				// comes from the field's arrow type.
				ts := a.Value(local)
				unit := a.DataType().(*arrow.TimestampType).Unit
				return timestampToRFC3339(int64(ts), unit), nil
			}
			return nil, fmt.Errorf("unsupported chunk type %T", chunk)
		}
		offset += chunk.Len()
	}
	return nil, fmt.Errorf("row %d out of range", row)
}

// readGeomAt pulls the raw WKB bytes at row from a geometry column.
// Returns nil for a null row so the caller can emit `"geometry":null`.
func readGeomAt(s gobi.Series, row int) ([]byte, error) {
	chunks := s.Column().Data().Chunks()
	offset := 0
	for _, chunk := range chunks {
		if row < offset+chunk.Len() {
			local := row - offset
			if chunk.IsNull(local) {
				return nil, nil
			}
			ba, ok := chunk.(*array.Binary)
			if !ok {
				return nil, fmt.Errorf("geometry column not Binary, got %T", chunk)
			}
			return ba.Value(local), nil
		}
		offset += chunk.Len()
	}
	return nil, fmt.Errorf("row %d out of range", row)
}

// chunked builds a column from several arrays, taking ownership.
func chunked(field arrow.Field, arrs ...arrow.Array) arrow.Column {
	ch := arrow.NewChunked(field.Type, arrs)
	defer ch.Release()
	for _, a := range arrs {
		a.Release()
	}
	return *arrow.NewColumn(field, ch)
}

// TestWrite_MatchesReference writes a frame holding every supported
// column type and geometry shape and checks each Feature against the
// reference writer, as parsed JSON (the two differ in property order).
func TestWrite_MatchesReference(t *testing.T) {
	pool := memory.DefaultAllocator
	pt := func(x, y float64) geometry.Point { return geometry.Point{X: x, Y: y} }
	ptZ := func(x, y, z float64) geometry.Point { return geometry.Point{X: x, Y: y, Z: z, HasZ: true} }
	ring := []geometry.Point{pt(0, 0), pt(4, 0), pt(4, 4), pt(0, 4), pt(0, 0)}
	hole := []geometry.Point{pt(1, 1), pt(1, 2), pt(2, 2), pt(1, 1)}
	line := geometry.LineString{Points: []geometry.Point{pt(-1.5, 2.25), pt(1e-9, 3e22), pt(123456.789, -0.000001)}}
	lineZ := geometry.LineString{Points: []geometry.Point{ptZ(1, 2, 3), ptZ(4, 5, -6.5)}, HasZ: true}
	poly := geometry.Polygon{Rings: [][]geometry.Point{ring, hole}}
	geoms := []geometry.Geometry{
		pt(-74.006, 40.7128),
		ptZ(1, 2, 3),
		line,
		lineZ,
		poly,
		geometry.Polygon{Rings: [][]geometry.Point{{ptZ(0, 0, 1), ptZ(1, 0, 1), ptZ(1, 1, 2), ptZ(0, 0, 1)}}, HasZ: true},
		geometry.MultiPoint{Points: []geometry.Point{pt(1, 2), pt(3, 4)}},
		geometry.MultiLineString{Lines: []geometry.LineString{line, {Points: []geometry.Point{pt(0, 0), pt(1, 1)}}}},
		geometry.MultiPolygon{Polygons: []geometry.Polygon{poly, {Rings: [][]geometry.Point{ring}}}},
		geometry.GeometryCollection{Geometries: []geometry.Geometry{pt(1, 2), line, poly}},
		nil,
		// Empty shapes.
		geometry.LineString{},
		geometry.Polygon{},
		geometry.MultiPoint{},
		geometry.MultiLineString{},
		geometry.MultiPolygon{},
		geometry.GeometryCollection{},
		pt(0, -0.0),
	}
	n := len(geoms)

	geomB := array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary)
	defer geomB.Release()
	for _, g := range geoms {
		if g == nil {
			geomB.AppendNull()
			continue
		}
		geomB.Append(geometry.WKB(g))
	}

	// Every third value of each column is null. Columns are split
	// into chunks at different rows, so the per-column cursors don't
	// move in step.
	valid := func(i int) bool { return i%3 != 2 }
	split := func(b array.Builder, appendAt func(i int), cuts ...int) []arrow.Array {
		defer b.Release()
		var out []arrow.Array
		start := 0
		for _, end := range append(cuts, n) {
			for i := start; i < end; i++ {
				if valid(i) {
					appendAt(i)
				} else {
					b.AppendNull()
				}
			}
			out = append(out, b.NewArray())
			start = end
		}
		return out
	}
	floats := []float64{0, 1, -1.5, 1e-7, 1e21, 123456789.125, math.MaxFloat64, math.SmallestNonzeroFloat64, 0.1, 1e-6, 999999999999999900000}
	strs := []string{"plain", "quote\"d", "back\\slash", "tab\there", "<html>&amp;", "üñíçødé 🙂", "", "line\nbreak", "\u2028 sep", "ctl\x01"}
	tsType := &arrow.TimestampType{Unit: arrow.Millisecond, TimeZone: "UTC"}

	i64 := array.NewInt64Builder(pool)
	i32 := array.NewInt32Builder(pool)
	u64 := array.NewUint64Builder(pool)
	u32 := array.NewUint32Builder(pool)
	f64 := array.NewFloat64Builder(pool)
	f32 := array.NewFloat32Builder(pool)
	boolB := array.NewBooleanBuilder(pool)
	strB := array.NewStringBuilder(pool)
	lstrB := array.NewLargeStringBuilder(pool)
	binB := array.NewBinaryBuilder(pool, arrow.BinaryTypes.Binary)
	tsB := array.NewTimestampBuilder(pool, tsType)
	geomField := gobi.GeometryField("geometry", 4326)
	fields := []arrow.Field{
		{Name: "i64", Type: arrow.PrimitiveTypes.Int64, Nullable: true},
		{Name: "i32", Type: arrow.PrimitiveTypes.Int32, Nullable: true},
		geomField,
		{Name: "u64", Type: arrow.PrimitiveTypes.Uint64, Nullable: true},
		{Name: "u32", Type: arrow.PrimitiveTypes.Uint32, Nullable: true},
		{Name: "f64", Type: arrow.PrimitiveTypes.Float64, Nullable: true},
		{Name: "f32", Type: arrow.PrimitiveTypes.Float32, Nullable: true},
		{Name: "bool", Type: arrow.FixedWidthTypes.Boolean, Nullable: true},
		{Name: "str \"quoted\" name", Type: arrow.BinaryTypes.String, Nullable: true},
		{Name: "lstr", Type: arrow.BinaryTypes.LargeString, Nullable: true},
		{Name: "bin", Type: arrow.BinaryTypes.Binary, Nullable: true},
		{Name: "ts", Type: tsType, Nullable: true},
	}
	cols := []arrow.Column{
		chunked(fields[0], split(i64, func(i int) { i64.Append(math.MinInt64 + int64(i)*1e15) }, 5)...),
		chunked(fields[1], split(i32, func(i int) { i32.Append(int32(i) - 7) })...),
		chunked(fields[2], geomB.NewArray()),
		chunked(fields[3], split(u64, func(i int) { u64.Append(math.MaxUint64 - uint64(i)) }, 1, 2, 9)...),
		chunked(fields[4], split(u32, func(i int) { u32.Append(uint32(i) * 1000) }, 7)...),
		chunked(fields[5], split(f64, func(i int) { f64.Append(floats[i%len(floats)]) }, 4, 11)...),
		// Float32 values that are finite as float32 (MaxFloat64 isn't).
		chunked(fields[6], split(f32, func(i int) { f32.Append(float32(floats[i%5]) / 3) })...),
		chunked(fields[7], split(boolB, func(i int) { boolB.Append(i%2 == 0) }, 3)...),
		chunked(fields[8], split(strB, func(i int) { strB.Append(strs[i%len(strs)]) }, 6)...),
		chunked(fields[9], split(lstrB, func(i int) { lstrB.Append(strs[(i+3)%len(strs)]) })...),
		chunked(fields[10], split(binB, func(i int) { binB.Append([]byte{byte(i), 0xff, 0}) }, 2)...),
		chunked(fields[11], split(tsB, func(i int) {
			tsB.Append(arrow.Timestamp(time.Date(2020+i, 1, 2, 3, 4, 5, 6e6, time.UTC).UnixMilli()))
		}, 8)...),
	}
	df, err := gobi.NewFrame(arrow.NewSchema(fields, nil), cols)
	if err != nil {
		t.Fatal(err)
	}
	defer df.Release()

	var out bytes.Buffer
	if err := Write(df, &out, &WriteOptions{Format: FormatLineDelimited}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) != n {
		t.Fatalf("wrote %d lines, want %d", len(lines), n)
	}
	for row, line := range lines {
		want, err := referenceFeature(df, 2, row)
		if err != nil {
			t.Fatalf("row %d: reference: %v", row, err)
		}
		var gotV, wantV any
		if err := json.Unmarshal([]byte(line), &gotV); err != nil {
			t.Fatalf("row %d: output is not valid JSON: %v\n%s", row, err, line)
		}
		if err := json.Unmarshal(want, &wantV); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(gotV, wantV) {
			t.Errorf("row %d (%T):\n got %s\nwant %s", row, geoms[row], line, want)
		}
	}
	// Properties come out in column order.
	if i, j := strings.Index(lines[0], `"i64"`), strings.Index(lines[0], `"ts"`); i < 0 || j < i {
		t.Errorf("properties not in column order: %s", lines[0])
	}

	// The same rows as a FeatureCollection, compact and indented.
	for _, indent := range []string{"", "  "} {
		out.Reset()
		if err := Write(df, &out, &WriteOptions{Indent: indent}); err != nil {
			t.Fatalf("Write (indent %q): %v", indent, err)
		}
		var fc struct {
			Type     string `json:"type"`
			Features []any  `json:"features"`
		}
		if err := json.Unmarshal(out.Bytes(), &fc); err != nil {
			t.Fatalf("FeatureCollection (indent %q) is not valid JSON: %v", indent, err)
		}
		if fc.Type != "FeatureCollection" || len(fc.Features) != n {
			t.Fatalf("indent %q: type %q with %d features, want %d", indent, fc.Type, len(fc.Features), n)
		}
		for row, f := range fc.Features {
			var want any
			if err := json.Unmarshal([]byte(lines[row]), &want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(f, want) {
				t.Errorf("indent %q, feature %d differs from the line-delimited row", indent, row)
			}
		}
	}
}

// Numbers are written the way json.Marshal writes them, byte for byte.
func TestAppendJSONFloat_MatchesJSON(t *testing.T) {
	r := rand.New(rand.NewPCG(5, 8))
	vals := []float64{0, math.Copysign(0, -1), 1, -1, 1e-6, 9.99e-7, 1e21, 9.99e20, 1e-7, 1e-10, 123e-20, 5e-324, math.MaxFloat64, 0.1, 100, 1e20}
	for range 20000 {
		vals = append(vals, math.Float64frombits(r.Uint64()), (r.Float64()-0.5)*math.Pow(10, float64(r.IntN(60)-30)))
	}
	for _, v := range vals {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			continue
		}
		want, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		got, err := appendJSONFloat(nil, v)
		if err != nil || string(got) != string(want) {
			t.Fatalf("%v: got %s (%v), want %s", v, got, err, want)
		}
	}
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if _, err := appendJSONFloat(nil, v); err == nil {
			t.Errorf("%v: expected an error, as json.Marshal gives", v)
		}
	}
}

// Input the WKB transcoder declines still gets written (or reported)
// through the geometry types, exactly as before.
func TestWrite_GeometryFallback(t *testing.T) {
	be := geometry.WKB(geometry.LineString{Points: []geometry.Point{{X: 1, Y: 2}, {X: 3, Y: 4}}})
	// Rewrite the little-endian LineString as big-endian by hand.
	big := []byte{0, 0, 0, 0, 2, 0, 0, 0, 2}
	for i := 9; i < len(be); i += 8 {
		for j := 7; j >= 0; j-- {
			big = append(big, be[i+j])
		}
	}
	for name, c := range map[string]struct {
		wkb     []byte
		want    string
		wantErr bool
	}{
		"big-endian":     {wkb: big, want: `{"type":"LineString","coordinates":[[1,2],[3,4]]}`},
		"truncated":      {wkb: be[:len(be)-3], wantErr: true},
		"trailing bytes": {wkb: append(append([]byte{}, be...), 0, 0), wantErr: false},
		"garbage":        {wkb: []byte{9, 9, 9}, wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			b := array.NewBinaryBuilder(memory.DefaultAllocator, arrow.BinaryTypes.Binary)
			defer b.Release()
			b.Append(c.wkb)
			df, err := gobi.NewFrameFromSeries(gobi.SeriesFromArray(gobi.GeometryField("geometry", 4326), b.NewArray()))
			if err != nil {
				t.Fatal(err)
			}
			defer df.Release()
			var out bytes.Buffer
			err = Write(df, &out, &WriteOptions{Format: FormatLineDelimited})
			// Whatever the reference does with this input, Write does too.
			ref, refErr := referenceFeature(df, 0, 0)
			if (err != nil) != (refErr != nil) {
				t.Fatalf("Write err = %v, reference err = %v", err, refErr)
			}
			if c.wantErr != (err != nil) {
				t.Fatalf("err = %v, wantErr %v", err, c.wantErr)
			}
			if err != nil {
				return
			}
			got := strings.TrimSuffix(out.String(), "\n")
			if got != string(ref) {
				t.Errorf("got %s, reference %s", got, ref)
			}
			if c.want != "" && got != fmt.Sprintf(`{"type":"Feature","geometry":%s}`, c.want) {
				t.Errorf("got %s, want geometry %s", got, c.want)
			}
		})
	}
}

// An empty geometry is written with its (empty) coordinates, as RFC
// 7946 requires, and reads back as the same empty geometry.
func TestMarshal_EmptyGeometries(t *testing.T) {
	for want, g := range map[string]geometry.Geometry{
		`{"type":"LineString","coordinates":[]}`:        geometry.LineString{},
		`{"type":"Polygon","coordinates":[]}`:           geometry.Polygon{},
		`{"type":"MultiPoint","coordinates":[]}`:        geometry.MultiPoint{},
		`{"type":"MultiLineString","coordinates":[]}`:   geometry.MultiLineString{},
		`{"type":"MultiPolygon","coordinates":[]}`:      geometry.MultiPolygon{},
		`{"type":"GeometryCollection","geometries":[]}`: geometry.GeometryCollection{},
	} {
		got, err := Marshal(g)
		if err != nil {
			t.Fatalf("%T: %v", g, err)
		}
		if string(got) != want {
			t.Errorf("Marshal(%T) = %s, want %s", g, got, want)
		}
		// The WKB transcoder writes the same bytes.
		if fast, ok := appendWKBAsGeoJSON(nil, geometry.WKB(g)); !ok || string(fast) != want {
			t.Errorf("transcoded %T = %s (ok=%v), want %s", g, fast, ok, want)
		}
		back, err := Unmarshal(got)
		if err != nil {
			t.Errorf("Unmarshal(%s): %v", got, err)
			continue
		}
		if reflect.TypeOf(back) != reflect.TypeOf(g) || !geometry.IsEmpty(back) {
			t.Errorf("%s read back as %T %+v", got, back, back)
		}
	}
}
