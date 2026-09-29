package geometry

import (
	"errors"
	"sync"
	"testing"
	"unsafe"
)

func TestCRS_HandleSize(t *testing.T) {
	// Point carries a CRS per vertex; keep both small.
	if s := unsafe.Sizeof(CRS{}); s != 4 {
		t.Errorf("sizeof CRS = %d, want 4", s)
	}
	if s := unsafe.Sizeof(Point{}); s != 32 {
		t.Errorf("sizeof Point = %d, want 32", s)
	}
}

func TestCRS_RegistryLookups(t *testing.T) {
	cases := []struct {
		crs       CRS
		name      string
		projected bool
	}{
		{WGS84, "WGS 84", false},
		{PseudoMercator, "WGS 84 / Pseudo-Mercator", true},
		{CRS{EPSG: 32618}, "WGS 84 / UTM zone 18N", true},
		{CRS{EPSG: 32733}, "WGS 84 / UTM zone 33S", true},
	}
	for _, c := range cases {
		if c.crs.Name() != c.name || c.crs.Projected() != c.projected {
			t.Errorf("%s: Name=%q Projected=%v, want %q %v", c.crs, c.crs.Name(), c.crs.Projected(), c.name, c.projected)
		}
		if got, err := LookupCRS(c.crs.EPSG); err != nil || got != c.crs {
			t.Errorf("LookupCRS(%d) = %v, %v", c.crs.EPSG, got, err)
		}
	}
	unknown := CRS{EPSG: 999_001}
	if unknown.Name() != "" || unknown.Projected() {
		t.Errorf("unregistered: Name=%q Projected=%v", unknown.Name(), unknown.Projected())
	}
	if _, err := LookupCRS(999_001); !errors.Is(err, ErrUnknownCRS) {
		t.Errorf("LookupCRS(unregistered) err = %v", err)
	}

	got := RegisterCRS(999_002, "Test Albers", true)
	if got != (CRS{EPSG: 999_002}) || got.Name() != "Test Albers" || !got.Projected() {
		t.Errorf("RegisterCRS: %v %q %v", got, got.Name(), got.Projected())
	}
	if _, err := LookupCRS(999_002); err != nil {
		t.Errorf("LookupCRS after register: %v", err)
	}
	// Re-registering replaces.
	RegisterCRS(999_002, "Test Albers v2", false)
	if got.Name() != "Test Albers v2" || got.Projected() {
		t.Errorf("re-register: %q %v", got.Name(), got.Projected())
	}
}

// TestCRS_RegistryConcurrent — registrations race against lookups
// without data races (run with -race) and without losing entries.
func TestCRS_RegistryConcurrent(t *testing.T) {
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for i := range 200 {
				RegisterCRS(int32(1_000_000+g*1000+i), "c", i%2 == 0)
			}
		}()
		go func() {
			defer wg.Done()
			for range 2000 {
				_ = WGS84.Name()
				_ = PseudoMercator.Projected()
			}
		}()
	}
	wg.Wait()
	for g := range 8 {
		for i := range 200 {
			c := CRS{EPSG: int32(1_000_000 + g*1000 + i)}
			if c.Name() != "c" || c.Projected() != (i%2 == 0) {
				t.Fatalf("lost registration %v", c)
			}
		}
	}
	if !PseudoMercator.Projected() || WGS84.Name() != "WGS 84" {
		t.Error("built-ins disturbed by concurrent registration")
	}
}
