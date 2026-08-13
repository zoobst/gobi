package athenaio

import (
	"testing"

	"github.com/zoobst/gobi"
)

// TestReadOptsFromSpec_Nil covers the zero-spec case: neither Columns
// nor Predicate set, so the helper returns nil — preserves the
// pre-existing "opts == nil ⇒ parquetio default read" contract for
// callers who don't opt into pushdown.
func TestReadOptsFromSpec_Nil(t *testing.T) {
	if got := readOptsFromSpec(nil, gobi.Expr{}); got != nil {
		t.Errorf("readOptsFromSpec(nil, zeroExpr) = %+v, want nil", got)
	}
	if got := readOptsFromSpec([]string{}, gobi.Expr{}); got != nil {
		t.Errorf("readOptsFromSpec([], zeroExpr) = %+v, want nil", got)
	}
}

// TestReadOptsFromSpec_Columns exercises the projection-only case:
// Columns set, Predicate zero. Verifies the helper produces a
// ReadOptions with Columns populated and no Predicate.
func TestReadOptsFromSpec_Columns(t *testing.T) {
	cols := []string{"eid", "dt"}
	opts := readOptsFromSpec(cols, gobi.Expr{})
	if opts == nil {
		t.Fatal("readOptsFromSpec with columns returned nil")
	}
	if len(opts.Columns) != 2 || opts.Columns[0] != "eid" || opts.Columns[1] != "dt" {
		t.Errorf("Columns = %v, want [eid dt]", opts.Columns)
	}
	if opts.Predicate.Node() != nil {
		t.Errorf("Predicate = %v, want zero", opts.Predicate)
	}
}

// TestReadOptsFromSpec_Predicate exercises the predicate-only case
// (row-group pruning without column projection). Confirms the
// predicate survives round-trip via the helper.
func TestReadOptsFromSpec_Predicate(t *testing.T) {
	pred := gobi.Col("dt").Ge(gobi.Lit(int64(0)))
	opts := readOptsFromSpec(nil, pred)
	if opts == nil {
		t.Fatal("readOptsFromSpec with predicate returned nil")
	}
	if len(opts.Columns) != 0 {
		t.Errorf("Columns = %v, want empty", opts.Columns)
	}
	if opts.Predicate.Node() == nil {
		t.Error("Predicate.Node() = nil after round-trip")
	}
}

// TestReadOptsFromSpec_Both exercises the combined case — both
// Columns and Predicate set. Both must reach the ReadOptions verbatim
// so parquetio row-group pruning + projection stack together on the
// per-bucket ReadReader call.
func TestReadOptsFromSpec_Both(t *testing.T) {
	cols := []string{"h3_res8"}
	pred := gobi.Col("dt").Ge(gobi.Lit(int64(0)))
	opts := readOptsFromSpec(cols, pred)
	if opts == nil {
		t.Fatal("readOptsFromSpec with both returned nil")
	}
	if len(opts.Columns) != 1 || opts.Columns[0] != "h3_res8" {
		t.Errorf("Columns = %v, want [h3_res8]", opts.Columns)
	}
	if opts.Predicate.Node() == nil {
		t.Error("Predicate.Node() = nil after round-trip")
	}
}

// TestSpecs_HaveColumnsAndPredicate is a static-shape guard: if
// someone renames or removes the Columns / Predicate fields on the
// three exposed specs (RawCTASSpec, UnloadSpec, OpenOptions), this
// test refuses to compile. Cheap way to prevent silently dropping
// the pushdown surface without noticing.
func TestSpecs_HaveColumnsAndPredicate(t *testing.T) {
	var raw RawCTASSpec
	raw.Columns = []string{"a"}
	raw.Predicate = gobi.Col("a").Ge(gobi.Lit(int64(0)))

	var un UnloadSpec
	un.Columns = []string{"a"}
	un.Predicate = gobi.Col("a").Ge(gobi.Lit(int64(0)))

	var op OpenOptions
	op.Columns = []string{"a"}
	op.Predicate = gobi.Col("a").Ge(gobi.Lit(int64(0)))

	_ = raw
	_ = un
	_ = op
}
