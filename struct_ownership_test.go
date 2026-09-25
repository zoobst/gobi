package gobi

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"unsafe"

	"github.com/apache/arrow-go/v18/arrow/array"
)

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
