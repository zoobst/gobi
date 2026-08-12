package gobi

import (
	"math"
	"testing"

	"github.com/apache/arrow-go/v18/arrow/array"
)

// TestHilbertSortWithCovering_MatchesTwoPassForm proves the fused
// single-pass path is behaviorally equivalent to the two-step
// SortByHilbert → WithBboxCoveringColumns form. Same row order,
// same column set, same bbox values per row.
//
// Regression protection: if the fused path drifts from the
// canonical two-step semantics (e.g. subtle sort tie-breaking
// change, off-by-one on the permutation, wrong null-mask
// alignment), this catches it.
func TestHilbertSortWithCovering_MatchesTwoPassForm(t *testing.T) {
	// 100-polygon shuffled grid — enough rows to exercise the sort,
	// small enough to compare row-by-row.
	f := gridFrame(t, 0, 200, 0, 200, 10)
	defer f.Release()

	// Two-step reference.
	sortedRef, err := f.SortByHilbert("geometry")
	if err != nil {
		t.Fatalf("SortByHilbert: %v", err)
	}
	defer sortedRef.Release()
	augRef, metaRef, err := WithBboxCoveringColumns(sortedRef)
	if err != nil {
		t.Fatalf("WithBboxCoveringColumns: %v", err)
	}
	defer augRef.Release()

	// Fused single-pass.
	augFused, metaFused, err := HilbertSortWithCovering(f, "geometry")
	if err != nil {
		t.Fatalf("HilbertSortWithCovering: %v", err)
	}
	defer augFused.Release()

	// Column sets must match.
	refNames := augRef.ColumnNames()
	fusedNames := augFused.ColumnNames()
	if len(refNames) != len(fusedNames) {
		t.Fatalf("column count: fused=%d ref=%d", len(fusedNames), len(refNames))
	}
	for i, name := range refNames {
		if fusedNames[i] != name {
			t.Errorf("column %d: fused=%q ref=%q", i, fusedNames[i], name)
		}
	}

	// Row count must match.
	refRows, _ := augRef.Shape()
	fusedRows, _ := augFused.Shape()
	if refRows != fusedRows {
		t.Fatalf("row count: fused=%d ref=%d", fusedRows, refRows)
	}

	// Row-by-row bbox check on the covering columns.
	for _, colName := range []string{
		"geometry_bbox_xmin", "geometry_bbox_ymin",
		"geometry_bbox_xmax", "geometry_bbox_ymax",
	} {
		refVals := float64Column(t, augRef, colName)
		fusedVals := float64Column(t, augFused, colName)
		if len(refVals) != len(fusedVals) {
			t.Fatalf("%s length: fused=%d ref=%d", colName, len(fusedVals), len(refVals))
		}
		for i := range refVals {
			// NaN != NaN semantics — treat two NaNs as equal.
			if math.IsNaN(refVals[i]) && math.IsNaN(fusedVals[i]) {
				continue
			}
			if refVals[i] != fusedVals[i] {
				t.Errorf("%s[%d]: fused=%v ref=%v", colName, i, fusedVals[i], refVals[i])
			}
		}
	}

	// Geo metadata must declare the same covering column paths.
	if metaRef.PrimaryColumn != metaFused.PrimaryColumn {
		t.Errorf("primary column: fused=%q ref=%q",
			metaFused.PrimaryColumn, metaRef.PrimaryColumn)
	}
	refCov := metaRef.Columns["geometry"].Covering
	fusedCov := metaFused.Columns["geometry"].Covering
	if refCov == nil || fusedCov == nil {
		t.Fatalf("covering nil: fused=%v ref=%v", fusedCov, refCov)
	}
	if !sliceEqual(refCov.Bbox.Xmin, fusedCov.Bbox.Xmin) ||
		!sliceEqual(refCov.Bbox.Ymin, fusedCov.Bbox.Ymin) ||
		!sliceEqual(refCov.Bbox.Xmax, fusedCov.Bbox.Xmax) ||
		!sliceEqual(refCov.Bbox.Ymax, fusedCov.Bbox.Ymax) {
		t.Errorf("covering paths differ: fused=%+v ref=%+v", fusedCov.Bbox, refCov.Bbox)
	}
}

// float64Column extracts a Float64 column's values across all
// chunks as a flat []float64. Used by the equivalence test for
// row-by-row bbox comparison.
func float64Column(t *testing.T, f *Frame, colName string) []float64 {
	t.Helper()
	col, err := f.Column(colName)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]float64, 0, f.NumRows())
	for _, chunk := range col.Column().Data().Chunks() {
		fa := chunk.(*array.Float64)
		for i := range fa.Len() {
			if fa.IsNull(i) {
				out = append(out, math.NaN())
				continue
			}
			out = append(out, fa.Value(i))
		}
	}
	return out
}

func sliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestHilbertSortWithCovering_EmptyFrame degenerates to the
// standard WithBboxCoveringColumns behavior (no rows, empty
// bboxes column). Regression protection for the early-return
// branch.
func TestHilbertSortWithCovering_EmptyFrame(t *testing.T) {
	f := gridFrame(t, 0, 0, 0, 0, 0) // 0×0 → empty frame
	defer f.Release()
	if f.NumRows() != 0 {
		t.Fatalf("fixture broken: expected 0 rows, got %d", f.NumRows())
	}
	out, meta, err := HilbertSortWithCovering(f, "geometry")
	if err != nil {
		t.Fatalf("HilbertSortWithCovering: %v", err)
	}
	defer out.Release()
	if meta == nil || meta.PrimaryColumn != "geometry" {
		t.Errorf("empty frame should still emit geo metadata; got %+v", meta)
	}
}
