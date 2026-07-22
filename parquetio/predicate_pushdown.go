package parquetio

import (
	"github.com/apache/arrow-go/v18/parquet/file"
	"github.com/apache/arrow-go/v18/parquet/metadata"

	"github.com/zoobst/gobi"
)

// rowGroupStats adapts a parquet row-group's footer metadata to
// gobi.Stats, so gobi.CanPossiblyMatch can evaluate a predicate
// against min/max/null-count bounds without importing parquet
// internals into gobi.
//
// The name→ordinal map is built once when the reader is opened; per-
// row-group instances share it via a pointer so predicate-pushdown
// work is O(#predicates × #row-groups) rather than O(#columns ×
// #row-groups).
type rowGroupStats struct {
	rg       *metadata.RowGroupMetaData
	colByName map[string]int
}

func (s *rowGroupStats) TotalRows() int64 { return s.rg.NumRows() }

func (s *rowGroupStats) MinMax(col string) (any, any, bool) {
	idx, ok := s.colByName[col]
	if !ok {
		return nil, nil, false
	}
	cc, err := s.rg.ColumnChunk(idx)
	if err != nil {
		return nil, nil, false
	}
	stats, err := cc.Statistics()
	if err != nil || stats == nil || !stats.HasMinMax() {
		return nil, nil, false
	}
	return decodeMinMax(stats)
}

func (s *rowGroupStats) NullCount(col string) (int64, bool) {
	idx, ok := s.colByName[col]
	if !ok {
		return 0, false
	}
	cc, err := s.rg.ColumnChunk(idx)
	if err != nil {
		return 0, false
	}
	stats, err := cc.Statistics()
	if err != nil || stats == nil || !stats.HasNullCount() {
		return 0, false
	}
	return stats.NullCount(), true
}

// decodeMinMax pulls Go-typed min/max scalars from a TypedStatistics.
// Returns ok=false for types gobi.CanPossiblyMatch can't compare
// (Int96, FixedLenByteArray outside strings, etc.).
func decodeMinMax(stats metadata.TypedStatistics) (any, any, bool) {
	switch s := stats.(type) {
	case *metadata.Int32Statistics:
		return s.Min(), s.Max(), true
	case *metadata.Int64Statistics:
		return s.Min(), s.Max(), true
	case *metadata.Float32Statistics:
		return s.Min(), s.Max(), true
	case *metadata.Float64Statistics:
		return s.Min(), s.Max(), true
	case *metadata.BooleanStatistics:
		return s.Min(), s.Max(), true
	case *metadata.ByteArrayStatistics:
		// ByteArray covers STRING and BINARY; the parquet-arrow layer
		// converts to Go string for STRING-typed columns.
		return string(s.Min()), string(s.Max()), true
	}
	return nil, nil, false
}

// buildColByName maps top-level column names to their parquet
// leaf-column indices. Used by rowGroupStats to look up ColumnChunk
// entries by user-facing name.
//
// Flat schemas only: gobi doesn't emit nested types, so name-to-leaf
// mapping is straightforward. Nested schemas would need a path walk.
func buildColByName(pf *file.Reader) map[string]int {
	sch := pf.MetaData().Schema
	out := make(map[string]int, sch.NumColumns())
	for i := 0; i < sch.NumColumns(); i++ {
		out[sch.Column(i).Name()] = i
	}
	return out
}

// filterRowGroupsByPredicate walks pf's row-groups and returns the
// subset whose min/max stats don't prove the predicate impossible.
// A nil or unusable predicate keeps every row-group (fallback to the
// caller's original selection).
func filterRowGroupsByPredicate(pf *file.Reader, pred gobi.Expr, candidates []int) []int {
	if pred.Node() == nil {
		return candidates
	}
	colByName := buildColByName(pf)
	kept := make([]int, 0, len(candidates))
	for _, rgIdx := range candidates {
		rg := pf.MetaData().RowGroup(rgIdx)
		s := &rowGroupStats{rg: rg, colByName: colByName}
		if gobi.CanPossiblyMatch(pred, s) {
			kept = append(kept, rgIdx)
		}
	}
	return kept
}
