package jsonio_test

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/zoobst/gobi/jsonio"
)

// BenchmarkRead_NDJSON — 100k rows × 5 mixed-type columns.
func BenchmarkRead_NDJSON(b *testing.B) {
	var buf bytes.Buffer
	for i := range 100_000 {
		fmt.Fprintf(&buf, `{"id": %d, "name": "row-%d", "x": %d.25, "ok": %t, "tag": null}`+"\n", i, i, i, i%2 == 0)
	}
	data := buf.Bytes()
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	for b.Loop() {
		f, err := jsonio.Read(bytes.NewReader(data), nil)
		if err != nil {
			b.Fatal(err)
		}
		f.Release()
	}
}
