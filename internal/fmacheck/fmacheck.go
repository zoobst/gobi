//go:build !js && !wasip1

// Package fmacheck is a test helper that fails when the compiler fuses
// a multiply and an add in a package that must give the same
// floating-point results on every platform.
//
// Go may compile x*y + z to one FMA instruction, which rounds once
// instead of twice. It does on arm64, ppc64, s390x, riscv64 and amd64
// at GOAMD64=v3, and not on wasm or default amd64, so the same source
// gives different last bits in a browser and on an Apple Silicon
// server. An explicit conversion, float64(x*y) + z, forbids the
// fusion (the language spec guarantees it), and the packages that use
// this helper write every such product that way.
//
// Nothing in the language stops the next edit from leaving one out,
// so the check asks the compiler: it builds the package for arm64
// with the assembly listing on and looks for fused instructions.
// Cross-compiling needs no arm64 machine, so it runs on any host.
package fmacheck

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// fused matches an arm64 fused multiply-add or multiply-subtract, of
// either precision, in a compiler assembly listing, and captures the
// source position it was compiled from.
var fused = regexp.MustCompile(`\(([^()]+\.go:\d+)\)\t(FN?M(?:ADD|SUB)[DS])\t`)

// Check fails t if compiling the package in the current directory
// for arm64 produces a fused multiply-add from that package's own
// source. Call it from a test in the package to be checked.
//
// It is skipped with -short (it recompiles the package) and when the
// go tool isn't available.
func Check(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("recompiles the package; skipped with -short")
	}
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go tool not found")
	}
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(goTool, "build", "-gcflags=-S", "-o", os.DevNull, ".")
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH=arm64", "CGO_ENABLED=0", "GOFLAGS=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go build for arm64: %v\n%s", err, tail(out))
	}
	seen := map[string]bool{}
	var sites []string
	for _, m := range fused.FindAllStringSubmatch(string(out), -1) {
		pos := m[1]
		// Only this package's files: a fused instruction inlined from
		// another package is that package's to fix.
		file := pos[:strings.LastIndex(pos, ":")]
		if filepath.Dir(file) != dir {
			continue
		}
		site := filepath.Base(pos)
		if !seen[site] {
			seen[site] = true
			sites = append(sites, site)
		}
	}
	if len(sites) == 0 {
		return
	}
	sort.Strings(sites)
	t.Errorf("the compiler fuses a multiply into an add or subtract at %d places, so results would differ between platforms:\n  %s\n"+
		"Round the product first: write float64(x*y) + z, not x*y + z. "+
		"If the line shown has no product, it adds a variable that was assigned one earlier: wrap that assignment.",
		len(sites), strings.Join(sites, "\n  "))
}

func tail(b []byte) string {
	if len(b) > 2000 {
		b = b[len(b)-2000:]
	}
	return string(b)
}
