package pgio

import (
	"bytes"
	"testing"
)

// TestEWKBRoundTrip checks that encodeEWKB followed by decodeEWKB
// produces the input WKB and preserves the SRID. This is the core
// invariant WriteTable + ReadTable rely on.
func TestEWKBRoundTrip(t *testing.T) {
	// Minimal WKB for POINT(1, 2), little-endian.
	//   byte 0     : 1 (LE)
	//   bytes 1-4  : 1 (Point type)
	//   bytes 5-12 : X = 1.0 (float64 LE)
	//   bytes 13-20: Y = 2.0 (float64 LE)
	wkb := []byte{
		0x01,
		0x01, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0xF0, 0x3F,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x40,
	}
	const srid = int32(4326)
	ewkb := encodeEWKB(wkb, srid)
	if len(ewkb) != len(wkb)+4 {
		t.Fatalf("encoded len = %d, want %d (adds 4-byte SRID header)", len(ewkb), len(wkb)+4)
	}
	// SRID flag must be set in the type field.
	if ewkb[4]&0x20 == 0 {
		t.Errorf("SRID flag not set in encoded type byte: %#x", ewkb[4])
	}
	roundtrip, gotSRID, err := decodeEWKB(ewkb)
	if err != nil {
		t.Fatal(err)
	}
	if gotSRID != srid {
		t.Errorf("srid = %d, want %d", gotSRID, srid)
	}
	if !bytes.Equal(roundtrip, wkb) {
		t.Errorf("wkb roundtrip mismatch:\n got %x\nwant %x", roundtrip, wkb)
	}
}

// TestDecodeEWKB_PlainWKB — passing a WKB blob that lacks the SRID
// flag should come back unchanged with srid=0.
func TestDecodeEWKB_PlainWKB(t *testing.T) {
	wkb := []byte{
		0x01,
		0x01, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0xF0, 0x3F,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x40,
	}
	got, srid, err := decodeEWKB(wkb)
	if err != nil {
		t.Fatal(err)
	}
	if srid != 0 {
		t.Errorf("srid = %d, want 0 for plain WKB", srid)
	}
	if !bytes.Equal(got, wkb) {
		t.Errorf("plain WKB should pass through unchanged")
	}
}

// TestRenumberPlaceholders — pgx requires $N placeholders. The
// translator emits `?`; ScanTable rewrites them at pushdown time.
// This test locks in the offset behavior so multiple pushdown
// passes don't clobber each other.
func TestRenumberPlaceholders(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		offset int
		want   string
	}{
		{"single", `("id" > ?)`, 0, `("id" > $1)`},
		{"multiple", `("id" > ? AND "x" < ?)`, 0, `("id" > $1 AND "x" < $2)`},
		{"offset", `("id" > ?)`, 3, `("id" > $4)`}, // first placeholder is $4
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := renumberPlaceholders(tc.in, tc.offset)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestQuoteIdent_DoublesEmbeddedQuotes matches the ANSI SQL
// identifier-quoting behavior gpkgio also uses. Small but
// safety-critical: bad quoting is a SQL-injection surface.
func TestQuoteIdent_DoublesEmbeddedQuotes(t *testing.T) {
	if got := quoteIdent(`weird"col`); got != `"weird""col"` {
		t.Errorf("got %q, want %q", got, `"weird""col"`)
	}
	if got := quoteIdent("plain"); got != `"plain"` {
		t.Errorf("got %q, want %q", got, `"plain"`)
	}
}
