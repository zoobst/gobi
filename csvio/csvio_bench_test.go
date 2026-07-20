package csvio_test

import (
	"strings"
	"testing"

	"github.com/zoobst/gobi/csvio"
)

// makeBenchCSV builds a synthetic CSV with n rows of (name, int, float,
// string) columns. Row content is deterministic and non-trivial so the
// parser has real work to do.
func makeBenchCSV(n int) string {
	var b strings.Builder
	b.Grow(n * 40)
	b.WriteString("name,i,f,note\n")
	for r := range n {
		// Rotate a few string values so the string column has some
		// realistic variety.
		b.WriteString("row-")
		writeInt(&b, r)
		b.WriteByte(',')
		writeInt(&b, r*7)
		b.WriteString(",")
		writeFloat(&b, float64(r)*0.5)
		b.WriteString(",note-")
		writeInt(&b, r%128)
		b.WriteByte('\n')
	}
	return b.String()
}

func writeInt(b *strings.Builder, v int) {
	var buf [20]byte
	i := len(buf)
	if v == 0 {
		b.WriteByte('0')
		return
	}
	neg := v < 0
	if neg {
		v = -v
	}
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	b.Write(buf[i:])
}

func writeFloat(b *strings.Builder, v float64) {
	// Cheap fixed-precision formatter — good enough for benchmark fodder.
	whole := int(v)
	frac := int((v - float64(whole)) * 100)
	if frac < 0 {
		frac = -frac
	}
	writeInt(b, whole)
	b.WriteByte('.')
	if frac < 10 {
		b.WriteByte('0')
	}
	writeInt(b, frac)
}

type benchRow struct {
	Name string  `csv:"name"`
	I    int64   `csv:"i"`
	F    float64 `csv:"f"`
	Note string  `csv:"note"`
}

var sinkFrame any

func BenchmarkRead_100k(b *testing.B) {
	src := makeBenchCSV(100_000)
	b.ResetTimer()
	b.ReportAllocs()
	for b.Loop() {
		df, err := csvio.Read[benchRow](strings.NewReader(src), nil)
		if err != nil {
			b.Fatal(err)
		}
		sinkFrame = df
	}
}

func BenchmarkRead_1M(b *testing.B) {
	src := makeBenchCSV(1_000_000)
	b.ResetTimer()
	b.ReportAllocs()
	for b.Loop() {
		df, err := csvio.Read[benchRow](strings.NewReader(src), nil)
		if err != nil {
			b.Fatal(err)
		}
		sinkFrame = df
	}
}
