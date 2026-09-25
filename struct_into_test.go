package gobi

import (
	"testing"
)

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
